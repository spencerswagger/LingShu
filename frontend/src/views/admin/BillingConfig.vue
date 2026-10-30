<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { QuestionFilled } from '@element-plus/icons-vue'
import {
  getSystemConfig,
  putSystemConfig,
  exportChannelProtocols,
  importChannelProtocols,
  type BillingConfigData,
  type CostSample,
  type SessionExpirationData,
  type ChannelProtocolPack,
} from '@/api/admin'
import ModelPricing from '@/components/ModelPricing.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

const loading = ref(false)
const saving = ref(false)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const r = ref<number>(1000)
const cnyRate = ref<number>(6.8) // USD/CNY 汇率（1 USD = cnyRate CNY），默认 6.8
const creditValue = ref<string>('') // 积分价值 V=R/1e6，只读
// 计费配置中由「全局时段/分档默认值」抽屉维护的两块，页面级保存时原样透传
const billingBase = ref<BillingConfigData | null>(null)

// 全局时段/分档默认值
const fallbackOpen = ref(false)
const fallbackSaving = ref(false)
const fallbackErr = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const fallbackModel = ref<Record<string, any>>({ TimeConfig: null, ContextTiers: null })

// 路由成本比较样值（routing.cost_sample）：新会话创建时的可比成本样值用量，不参与实际计费。
const costSample = ref<CostSample>({
  input: 10000,
  cache_read: 9000,
  output: 1000,
})

// 会话过期分档（后端按「上下文长度 + 对话次数」双维度分档，取较大者）
const sessionExpiration = ref<SessionExpirationData>({ default_ttl_minutes: 5, context_tiers: [], turn_tiers: [] })

// 全局上游响应头白名单（sys_configs 键 gateway.upstream_response_header_whitelist）
const whitelist = ref<string[]>([])

// 成本页增强视图开关（sys_configs 键 console.cost_multichannel_trace，默认 false）。
// 只影响成本页两个只读入口（定价曲线多线对比 / 会话渠道轨迹），不参与计费与路由。
// 只有确实读到过配置（consoleLoaded=true）才会回写，避免一次失败的读取把开关写回 false。
const multichannelTrace = ref(false)
const consoleLoaded = ref(false)
function consolePayload(): { Console?: { MultichannelTrace: boolean } } {
  return consoleLoaded.value ? { Console: { MultichannelTrace: multichannelTrace.value } } : {}
}
// 建议透传的响应头候选（与预设包格式约定保持一致）
const headerSuggestions = [
  'anthropic-version',
  'anthropic-beta',
  'OpenAI-Organization',
  'OpenAI-Project',
  'OpenAI-Beta',
  'traceparent',
  'tracestate',
  'x-request-id',
  'x-session-id',
  'x-client-request-id',
  'x-opencode-session',
]

// 渠道协议导入：隐藏文件输入 + 导出/导入操作状态
const fileInput = ref<HTMLInputElement | null>(null)
const exporting = ref(false)
const importing = ref(false)

// 生成本地时间戳文件名片段：YYYYMMDD-HHmmss
function formatTs(d = new Date()) {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`
}

// 导出全部渠道协议为预设包 JSON 并触发下载
async function onExport() {
  exporting.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await exportChannelProtocols()
    const text = JSON.stringify(res.Data, null, 2)
    const blob = new Blob([text], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `channel-protocols-${formatTs()}.json`
    a.click()
    URL.revokeObjectURL(url)
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    exporting.value = false
  }
}

// 触发隐藏文件选择框
function triggerImport() {
  fileInput.value?.click()
}

// 读取选中文件并导入渠道协议
async function onFileChange(ev: Event) {
  const input = ev.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) {
    return
  }
  try {
    const raw = await file.text()
    if (!raw) {
      return
    }
    let parsed: ChannelProtocolPack
    try {
      parsed = JSON.parse(raw)
    } catch {
      ElMessage.error('文件不是合法 JSON')
      return
    }
    await ElMessageBox.confirm(
      '即将从该文件导入渠道协议（同名/同来源将自动跳过）。是否继续？',
      '导入渠道协议',
      { type: 'warning' },
    )
    importing.value = true
    errInfo.value = { message: '', requestId: '' }
    try {
      const res = await importChannelProtocols(parsed)
      ElMessage.success(`导入完成：新增 ${res.Data.Created} 条，跳过 ${res.Data.Skipped} 条`)
      const warnings = res.Data.Warnings || []
      if (warnings.length) {
        ElMessageBox.alert(warnings.join('\n'), '导入告警', { type: 'warning' })
      }
    } catch (e: any) {
      errInfo.value = { message: e?.message, requestId: e?.requestId }
    } finally {
      importing.value = false
    }
  } catch (e: any) {
    // 用户取消确认弹窗：静默处理
    if (e === 'cancel' || e === 'close') return
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    // 无论成败都重置，支持重复选择同一文件
    input.value = ''
  }
}

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await getSystemConfig()
    const b = res.Data.Billing
    billingBase.value = b
    r.value = Number(b.R)
    const c = Number(b.CNYRate)
    if (c > 0) cnyRate.value = c
    const cv = Number(b.CreditValue)
    if (!isNaN(cv)) creditValue.value = `1 积分 = ¥${cv}（R ÷ 1,000,000）`
    const cs = b.CostSample
    if (cs) {
      costSample.value = {
        input: Number(cs.input) || 0,
        cache_read: Number(cs.cache_read) || 0,
        output: Number(cs.output) || 0,
      }
    }
    const se = res.Data.SessionExpiration
    sessionExpiration.value = {
      default_ttl_minutes: Number(se?.default_ttl_minutes) || 5,
      context_tiers: se?.context_tiers || [],
      turn_tiers: se?.turn_tiers || [],
    }
    whitelist.value = res.Data.UpstreamResponseHeaderWhitelist || []
    // Console 块可选：老后端没有该块时按关闭处理，且不回写（consoleLoaded 保持 false）
    if (res.Data.Console && typeof res.Data.Console.MultichannelTrace === 'boolean') {
      multichannelTrace.value = res.Data.Console.MultichannelTrace
      consoleLoaded.value = true
    }
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)

// 页面级保存：一次提交计费 + 会话过期 + 网关白名单三组配置（后端统一接口）
async function onSave() {
  if (!r.value || r.value <= 0) return ElMessage.warning('R 值必须为正数')
  if (!cnyRate.value || cnyRate.value <= 0) return ElMessage.warning('汇率必须为正数')
  if (!Number.isInteger(sessionExpiration.value.default_ttl_minutes) || sessionExpiration.value.default_ttl_minutes <= 0) {
    return ElMessage.warning('兜底 TTL 必须为正整数')
  }

  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    await putSystemConfig({
      Billing: {
        R: r.value,
        CnyRate: cnyRate.value,
        // 时段/分档全局默认由抽屉单独维护，这里原样透传不回退
        ContextTiers: billingBase.value?.ContextTiers || [],
        TimeConfig: billingBase.value?.TimeConfig || {
          Timezone: 'Asia/Shanghai',
          Default: 1,
          Periodic: [],
          Overrides: [],
        },
        CostSample: costSample.value,
      },
      SessionExpiration: sessionExpiration.value,
      UpstreamResponseHeaderWhitelist: whitelist.value,
      ...consolePayload(),
    })
    ElMessage.success('系统配置已保存')
    await load()
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    saving.value = false
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
    const res = await getSystemConfig()
    const tc = res.Data.Billing.TimeConfig as any
    const tiers = res.Data.Billing.ContextTiers || []
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
    await putSystemConfig({
      Billing: {
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
      },
      SessionExpiration: sessionExpiration.value,
      UpstreamResponseHeaderWhitelist: whitelist.value,
      ...consolePayload(),
    })
    ElMessage.success('全局时段/分档默认值已保存')
    fallbackOpen.value = false
    await load()
  } catch (e: any) {
    fallbackErr.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    fallbackSaving.value = false
  }
}
</script>

<template>
  <div class="page-container system-page">
    <div class="system-body">
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
            <span class="label">输出 tokens</span>
            <el-input-number v-model="costSample.output" :min="0" :step="100" />
            <el-tooltip
              :content="'以该样值代入渠道成本单价求和（三段相加：输入为纯输入、不含缓存读），作为新会话落点的可比成本；再乘以当前时段系数。'"
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
            会话过期时长按「上下文长度（prompt token 估算）」与「用户对话次数」两维分别命中档位，取较大者，且不低于兜底值。档位为上界桶：实际值 ≤ 该档上界即命中该档，超过最大上界取最高档；过期时间只延长、不缩短。分档参数为系统级配置，对所有渠道生效。修改后随页面底部「保存」一并提交。
          </div>

          <div class="form-row">
            <span class="label">兜底 TTL（分钟）</span>
            <el-input-number v-model="sessionExpiration.default_ttl_minutes" :min="1" />
          </div>

          <div class="tier-title">上下文分档</div>
          <el-table :data="sessionExpiration.context_tiers" size="small" border>
            <el-table-column label="最大 tokens" min-width="180">
              <template #default="{ row }">
                <el-input-number v-model="row.max_tokens" :min="1" controls-position="right" class="tier-num" />
              </template>
            </el-table-column>
            <el-table-column label="TTL（分钟）" min-width="180">
              <template #default="{ row }">
                <el-input-number v-model="row.ttl_minutes" :min="1" controls-position="right" class="tier-num" />
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
                <el-input-number v-model="row.max_turns" :min="1" controls-position="right" class="tier-num" />
              </template>
            </el-table-column>
            <el-table-column label="TTL（分钟）" min-width="180">
              <template #default="{ row }">
                <el-input-number v-model="row.ttl_minutes" :min="1" controls-position="right" class="tier-num" />
              </template>
            </el-table-column>
            <el-table-column label="操作" width="90" align="center">
              <template #default="{ $index }">
                <el-button link type="danger" @click="removeTurnTier($index)">删除</el-button>
              </template>
            </el-table-column>
          </el-table>
          <el-button class="add-tier-btn" @click="addTurnTier">添加档位</el-button>
        </div>

        <el-divider />

        <div class="config-block">
          <div class="block-title">
            全局上游响应头白名单
            <el-tooltip
              content="上游响应头默认不透传（维持渠道透明）。仅白名单中的头会被透传给客户端，用于排查链路。"
              placement="top"
            >
              <el-icon class="hint-icon"><QuestionFilled /></el-icon>
            </el-tooltip>
          </div>
          <div class="block-desc">
            上游响应头默认不透传给客户端。在此列出需要透传的响应头名称后，网关才会把对应头原样回传（全局生效，对所有渠道一致）。
          </div>

          <el-form label-width="130px">
            <el-form-item label="白名单头">
              <el-select
                v-model="whitelist"
                multiple
                filterable
                allow-create
                default-first-option
                style="width: 100%"
                placeholder="输入或选择需透传的上游响应头名（留空 = 不透传任何上游响应头）"
              >
                <el-option v-for="h in headerSuggestions" :key="h" :label="h" :value="h" />
              </el-select>
              <div class="hint">支持直接输入自定义头名回车添加；候选项为常见可透传头。</div>
            </el-form-item>
          </el-form>
        </div>

        <el-divider />

        <div class="config-block">
          <div class="block-title">
            成本页增强视图
            <el-tooltip
              content="只控制「成本」页两个只读入口：定价曲线多渠道对比、会话渠道轨迹。关闭时旧接口与页面表现完全不变。"
              placement="top"
            >
              <el-icon class="hint-icon"><QuestionFilled /></el-icon>
            </el-tooltip>
          </div>
          <div class="block-desc">
            打开后，「成本」页的定价曲线可同图对比多个「渠道 × 内部模型」
            （接口参数 ChannelModelIDs），会话成本多一块渠道轨迹（真实切换点 + 预测打平点）。
            该项**只读展示**，不参与计费公式、不参与路由决策；默认关闭。
          </div>
          <div class="form-row">
            <span class="label">多渠道对比 / 渠道轨迹</span>
            <el-switch v-model="multichannelTrace" active-text="开启" inactive-text="关闭" />
            <span class="hint" style="margin-left: 8px">
              键名 console.cost_multichannel_trace；保存本页后生效（未读到配置时不回写，避免误关）。
            </span>
          </div>
        </div>

        <el-divider />

        <div class="config-block">
          <div class="block-title">备份与恢复</div>
          <div class="block-desc">导出全部渠道协议为预设包 JSON，或从该文件导入。导入按名称/来源自动去重，仅新增不覆盖。</div>

          <div class="form-row">
            <span class="label">渠道协议</span>
            <el-button type="primary" :loading="exporting" @click="onExport">导出</el-button>
            <el-button :loading="importing" @click="triggerImport">导入</el-button>
            <input
              ref="fileInput"
              type="file"
              accept=".json,application/json"
              style="display: none"
              @change="onFileChange"
            />
          </div>
        </div>

        <ErrorBubble
          v-if="errInfo.message || errInfo.requestId"
          :message="errInfo.message"
          :request-id="errInfo.requestId"
        />
      </div>
    </div>

    <!-- 底部吸附保存栏：内容超过一屏时无需回到顶部即可保存 -->
    <div class="save-bar">
      <el-button type="primary" :loading="saving" @click="onSave">保存</el-button>
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
  </div>
</template>

<style scoped>
.system-page {
  display: flex;
  flex-direction: column;
  min-height: 100%;
}
.system-body {
  flex: 1;
}
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
.hint {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.5;
  color: var(--color-text-secondary);
}
/* 分档数字输入框：自适应宽度（不撑满单元格），窄容器下收缩 */
.tier-num {
  width: 100%;
  max-width: 160px;
}
.save-bar {
  position: sticky;
  bottom: 0;
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  margin: 16px -24px -28px;
  padding: 12px 24px;
  background: var(--color-surface);
  border-top: 1px solid var(--color-border);
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
