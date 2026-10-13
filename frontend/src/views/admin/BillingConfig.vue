<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { QuestionFilled } from '@element-plus/icons-vue'
import { getBillingConfig, putBillingConfig, type CostSample, getSessionConfig, putSessionConfig, type SessionExpirationData } from '@/api/admin'
import ModelPricing from '@/components/ModelPricing.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

const loading = ref(false)
const saving = ref(false)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const r = ref<number>(1000)
const cnyRate = ref<number>(6.8) // USD/CNY 汇率（1 USD = cnyRate CNY），默认 6.8
const creditValue = ref<string>('') // 积分价值 V=R/1e6，只读

// 全局时段/分档默认值
const fallbackOpen = ref(false)
const fallbackSaving = ref(false)
const fallbackErr = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const fallbackModel = ref<Record<string, any>>({ TimeConfig: null, ContextTiers: null })

// 路由成本比较样值（routing.cost_sample）：新会话创建时的可比成本样值用量，不参与实际计费。
const costSample = ref<CostSample>({
  input: 10000,
  cache_read: 9000,
  cache_write: 0,
  output: 1000,
  reasoning: 500,
})

// 会话过期分档（后端按「上下文长度 + 对话次数」双维度分档，取较大者）
const sessionExpiration = ref<SessionExpirationData>({ default_ttl_minutes: 5, context_tiers: [], turn_tiers: [] })
const sessionSaving = ref(false)

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const [res, sessRes] = await Promise.all([getBillingConfig(), getSessionConfig()])
    r.value = typeof res.Data.R === 'string' ? Number(res.Data.R) : Number(res.Data.R)
    const c = Number(res.Data.CNYRate)
    if (c > 0) cnyRate.value = c
    const cv = Number(res.Data.CreditValue)
    if (!isNaN(cv)) creditValue.value = `1 积分 = ¥${cv}（R ÷ 1,000,000）`
    const cs = res.Data.CostSample
    if (cs) {
      costSample.value = {
        input: Number(cs.input) || 0,
        cache_read: Number(cs.cache_read) || 0,
        cache_write: Number(cs.cache_write) || 0,
        output: Number(cs.output) || 0,
        reasoning: Number(cs.reasoning) || 0,
      }
    }
    const se = sessRes.Data.SessionExpiration
    sessionExpiration.value = {
      default_ttl_minutes: Number(se?.default_ttl_minutes) || 5,
      context_tiers: se?.context_tiers || [],
      turn_tiers: se?.turn_tiers || [],
    }
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)

async function onSave() {
  if (!r.value || r.value <= 0) return ElMessage.warning('R 值必须为正数')
  if (!cnyRate.value || cnyRate.value <= 0) return ElMessage.warning('汇率必须为正数')

  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    // PUT 三键必传：仅回传 R，时段/分档全局默认保留当前数据库值不动
    const cur = await getBillingConfig()
    await putBillingConfig({
      R: r.value,
      CnyRate: cnyRate.value,
      ContextTiers: cur.Data.ContextTiers || [],
      TimeConfig: cur.Data.TimeConfig || {
        Timezone: 'Asia/Shanghai',
        Default: 1,
        Periodic: [],
        Overrides: [],
      },
      CostSample: costSample.value,
    })
    ElMessage.success('计费配置已更新')
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    saving.value = false
  }
}

// 保存会话过期分档配置（独立于计费配置，互不影响）
async function saveSessionConfig() {
  if (!Number.isInteger(sessionExpiration.value.default_ttl_minutes) || sessionExpiration.value.default_ttl_minutes <= 0) {
    return ElMessage.warning('兜底 TTL 必须为正整数')
  }

  sessionSaving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    await putSessionConfig({ SessionExpiration: sessionExpiration.value })
    ElMessage.success('会话过期分档已保存')
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    sessionSaving.value = false
  }
}

// 上下文分档 增/删行
function addContextTier() {
  sessionExpiration.value.context_tiers.push({ max_tokens: 2000, ttl_minutes: 5 })
}
function removeContextTier(index: number) {
  sessionExpiration.value.context_tiers.splice(index, 1)
}
// 对话次数分档 增/删行
function addTurnTier() {
  sessionExpiration.value.turn_tiers.push({ max_turns: 2, ttl_minutes: 5 })
}
function removeTurnTier(index: number) {
  sessionExpiration.value.turn_tiers.splice(index, 1)
}

// 打开全局时段/分档默认值抽屉：读取当前 billing 配置
async function openFallback() {
  fallbackOpen.value = true
  fallbackErr.value = { message: '', requestId: '' }
  try {
    const res = await getBillingConfig()
    const tc = res.Data.TimeConfig as any
    const tiers = res.Data.ContextTiers || []
    fallbackModel.value = {
      TimeConfig: tc && tc.Periodic ? tc : null,
      ContextTiers: tiers.length ? tiers : null,
    }
  } catch (e: any) {
    fallbackErr.value = { message: e?.message, requestId: e?.requestId }
  }
}

async function saveFallback() {
  fallbackSaving.value = true
  fallbackErr.value = { message: '', requestId: '' }
  try {
    await putBillingConfig({
      R: r.value,
      CnyRate: cnyRate.value,
      ContextTiers: fallbackModel.value.ContextTiers || [],
      TimeConfig: fallbackModel.value.TimeConfig || {
        Timezone: 'Asia/Shanghai',
        Default: 1,
        Periodic: [],
        Overrides: [],
      },
      CostSample: costSample.value,
    })
    ElMessage.success('全局时段/分档默认值已保存')
    fallbackOpen.value = false
  } catch (e: any) {
    fallbackErr.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    fallbackSaving.value = false
  }
}
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">系统</div>
      <el-button type="primary" :loading="saving" @click="onSave">保存</el-button>
    </div>

    <div v-loading="loading" class="card config-card">
      <div class="config-block">
        <div class="block-title">积分换算</div>
        <div class="block-desc">token 计费换算底数与积分价值。R 值决定积分余额量级，由 R 推导积分价值，不可单独修改。</div>

        <div class="form-row">
          <span class="label">R 值（token 缩小分母）</span>
          <el-input-number v-model="r" :min="1" :step="100" />
          <el-tooltip
            :content="'token 缩小分母：决定积分余额量级与积分价值。扣费积分 = Σ(token × 倍率) × 时段/分档系数 ÷ R。R 越大，余额数字越小、单次消耗积分越精细。修改只影响之后产生的调用，历史账单不变。'"
            placement="top"
          >
            <el-icon class="hint-icon"><QuestionFilled /></el-icon>
          </el-tooltip>
        </div>

        <div class="form-row">
          <span class="label">积分价值（只读）</span>
          <span class="static-value">{{ creditValue || '由 R 值推导…' }}</span>
          <el-tooltip
            :content="'积分价值 V = R ÷ 1,000,000（每积分对应人民币元），由 R 推导、不可单独修改。R=10000 时 1 积分 = ¥0.01。'"
            placement="top"
          >
            <el-icon class="hint-icon"><QuestionFilled /></el-icon>
          </el-tooltip>
        </div>
      </div>

      <el-divider />

      <div class="config-block">
        <div class="block-title">汇率换算</div>
        <div class="block-desc">仅用于 models.dev 参考价折算：把外部美元报价按汇率换算成倍率后供参考填充。供应商以人民币报价时无需汇率，直接照抄官网价录入。</div>

        <div class="form-row">
          <span class="label">USD/CNY 汇率</span>
          <el-input-number v-model="cnyRate" :min="0.0001" :precision="4" :step="0.1" />
          <el-tooltip
            :content="'USD/CNY 汇率（1 美元 = cnyRate 元人民币），仅用于 models.dev 美元参考价折算为人民币倍率。默认 6.8，可按实际情况调整。'"
            placement="top"
          >
            <el-icon class="hint-icon"><QuestionFilled /></el-icon>
          </el-tooltip>
        </div>
      </div>

      <el-divider />

      <div class="config-block">
        <div class="block-title">计费默认值</div>
        <div class="block-desc">未配置模型级时段/分档的对外模型与渠道内部模型，将按此全局默认计费。</div>

        <div class="form-row">
          <span class="label">全局时段/分档默认</span>
          <el-button @click="openFallback">维护默认值</el-button>
          <el-tooltip
            :content="'配置时段系数与上下文分档的全局默认值。对外模型与渠道内部模型未单独配置时，按此默认计费。'"
            placement="top"
          >
            <el-icon class="hint-icon"><QuestionFilled /></el-icon>
          </el-tooltip>
        </div>
      </div>

      <el-divider />

      <div class="config-block">
        <div class="block-title">路由成本比较样值</div>
        <div class="block-desc">新会话创建时，以该样值用量代入各候选渠道的成本单价求和，比较「谁更便宜」来决定落点（同优先级内取最低成本）。仅用于路由选择，不参与实际计费。</div>

        <div class="form-row">
          <span class="label">输入 tokens</span>
          <el-input-number v-model="costSample.input" :min="0" :step="1000" />
        </div>
        <div class="form-row">
          <span class="label">缓存读 tokens</span>
          <el-input-number v-model="costSample.cache_read" :min="0" :step="1000" />
        </div>
        <div class="form-row">
          <span class="label">缓存写 tokens</span>
          <el-input-number v-model="costSample.cache_write" :min="0" :step="1000" />
        </div>
        <div class="form-row">
          <span class="label">输出 tokens</span>
          <el-input-number v-model="costSample.output" :min="0" :step="100" />
        </div>
        <div class="form-row">
          <span class="label">推理 tokens</span>
          <el-input-number v-model="costSample.reasoning" :min="0" :step="100" />
          <el-tooltip
            :content="'以该样值代入渠道成本单价求和（输入按「输入-缓存读」计入），作为新会话落点的可比成本；再乘以当前时段系数。'"
            placement="top"
          >
            <el-icon class="hint-icon"><QuestionFilled /></el-icon>
          </el-tooltip>
        </div>
      </div>

      <el-divider />

      <div class="config-block">
        <div class="block-title">会话过期分档</div>
        <div class="block-desc">
          会话过期时长按「上下文长度（prompt token 估算）」与「用户对话次数」两维分别命中档位，取较大者，且不低于兜底值。档位为上界桶：实际值 ≤ 该档上界即命中该档，超过最大上界取最高档；过期时间只延长、不缩短。分档参数为系统级配置，对所有渠道生效。
        </div>

        <div class="form-row">
          <span class="label">兜底 TTL（分钟）</span>
          <el-input-number v-model="sessionExpiration.default_ttl_minutes" :min="1" />
        </div>

        <div class="tier-title">上下文分档</div>
        <el-table :data="sessionExpiration.context_tiers" size="small" border>
          <el-table-column label="最大 tokens" min-width="180">
            <template #default="{ row }">
              <el-input-number v-model="row.max_tokens" :min="1" style="width: 100%" />
            </template>
          </el-table-column>
          <el-table-column label="TTL（分钟）" min-width="180">
            <template #default="{ row }">
              <el-input-number v-model="row.ttl_minutes" :min="1" style="width: 100%" />
            </template>
          </el-table-column>
          <el-table-column label="操作" width="90" align="center">
            <template #default="{ $index }">
              <el-button link type="danger" @click="removeContextTier($index)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
        <el-button class="add-tier-btn" @click="addContextTier">添加档位</el-button>

        <div class="tier-title">对话次数分档</div>
        <el-table :data="sessionExpiration.turn_tiers" size="small" border>
          <el-table-column label="最大对话次数" min-width="180">
            <template #default="{ row }">
              <el-input-number v-model="row.max_turns" :min="1" style="width: 100%" />
            </template>
          </el-table-column>
          <el-table-column label="TTL（分钟）" min-width="180">
            <template #default="{ row }">
              <el-input-number v-model="row.ttl_minutes" :min="1" style="width: 100%" />
            </template>
          </el-table-column>
          <el-table-column label="操作" width="90" align="center">
            <template #default="{ $index }">
              <el-button link type="danger" @click="removeTurnTier($index)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
        <el-button class="add-tier-btn" @click="addTurnTier">添加档位</el-button>

        <div class="form-row">
          <el-button type="primary" :loading="sessionSaving" @click="saveSessionConfig">保存分档配置</el-button>
        </div>
      </div>

      <!-- 全局时段/分档默认值 抽屉 -->
      <el-drawer v-model="fallbackOpen" title="全局时段/分档默认值" :size="720">
        <div class="fallback-tip">
          当前积分换算 R 值：{{ r }}。这里仅维护时段系数与上下文分档，R 值请在「积分换算」区块修改。
        </div>
        <ModelPricing
          v-if="fallbackOpen"
          v-model="fallbackModel"
          show-rates-key=""
          title="全局默认定价"
        />
        <ErrorBubble
          v-if="fallbackErr.message || fallbackErr.requestId"
          :message="fallbackErr.message"
          :request-id="fallbackErr.requestId"
        />
        <template #footer>
          <el-button @click="fallbackOpen = false">取消</el-button>
          <el-button type="primary" :loading="fallbackSaving" @click="saveFallback">保存全局默认</el-button>
        </template>
      </el-drawer>

      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
    </div>
  </div>
</template>

<style scoped>
.config-card {
  padding: 20px 24px;
}
.config-block {
  margin-bottom: 4px;
}
.block-desc {
  color: var(--color-text-secondary);
  font-size: 13px;
  margin-bottom: 14px;
}
.tier-title {
  font-size: 14px;
  font-weight: 600;
  color: var(--color-text);
  margin: 16px 0 8px;
}
.add-tier-btn {
  margin-top: 10px;
}
.form-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
}
.label {
  color: var(--color-text-secondary);
  font-size: 13px;
  min-width: 150px;
}
.static-value {
  color: var(--brand-primary);
  font-size: 13px;
  font-weight: 600;
}
.hint-icon {
  cursor: pointer;
  color: var(--color-text-secondary);
}
.fallback-tip {
  background: #f6f8ff;
  border: 1px dashed #a5c0f7;
  color: #4a7bd9;
  font-size: 12px;
  padding: 8px 12px;
  border-radius: 6px;
  margin-bottom: 12px;
}
</style>