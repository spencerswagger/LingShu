<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { QuestionFilled } from '@element-plus/icons-vue'
import { getBillingConfig, putBillingConfig } from '@/api/admin'
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
const fallbackModel = ref<Record<string, any>>({ time_config: null, context_tiers: null })

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await getBillingConfig()
    r.value = typeof res.data.r === 'string' ? Number(res.data.r) : Number(res.data.r)
    const c = Number(res.data.cny_rate)
    if (c > 0) cnyRate.value = c
    const cv = Number(res.data.credit_value)
    if (!isNaN(cv)) creditValue.value = `1 积分 = ¥${cv}（R ÷ 1,000,000）`
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
      r: r.value,
      cny_rate: cnyRate.value,
      context_tiers: cur.data.context_tiers || [],
      time_config: cur.data.time_config || {
        timezone: 'Asia/Shanghai',
        default_coeff: 1,
        periodic_segments: [],
        date_overrides: [],
      },
    })
    ElMessage.success('计费配置已更新')
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    saving.value = false
  }
}

// 打开全局时段/分档默认值抽屉：读取当前 billing 配置
async function openFallback() {
  fallbackOpen.value = true
  fallbackErr.value = { message: '', requestId: '' }
  try {
    const res = await getBillingConfig()
    const tc = res.data.time_config as any
    const tiers = res.data.context_tiers || []
    fallbackModel.value = {
      time_config: tc && tc.periodic_segments ? tc : null,
      context_tiers: tiers.length ? tiers : null,
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
      r: r.value,
      cny_rate: cnyRate.value,
      context_tiers: fallbackModel.value.context_tiers || [],
      time_config: fallbackModel.value.time_config || {
        timezone: 'Asia/Shanghai',
        default_coeff: 1,
        periodic_segments: [],
        date_overrides: [],
      },
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

      <!-- 全局时段/分档默认值 抽屉 -->
      <el-drawer v-model="fallbackOpen" title="全局时段/分档默认值" :size="560">
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