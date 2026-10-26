<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { getBilling } from '@/api/admin'
import type { BillingDetail } from '@/api/dev'
import StatusTag from '@/components/StatusTag.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'
import CallLogPanel from '@/components/CallLogPanel.vue'
import { useIsNarrow } from '@/composables/useIsNarrow'

const route = useRoute()
const billingId = String(route.params.id)

// mode='self' 时复用为开发者自助账单详情页（API 走 /dev 前缀，隐藏 admin 字段）
const props = withDefaults(defineProps<{ mode?: 'admin' | 'self' }>(), { mode: 'admin' })
const isSelf = computed(() => props.mode === 'self')
// 窄屏：描述列表回落单列（原来固定双列会把长 ID / 模型名挤成竖排）
const isNarrow = useIsNarrow()

const loading = ref(false)
const detail = ref<BillingDetail | null>(null)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

// 响应类型展示映射（来自调用日志）
const respKindLabels: Record<string, string> = {
  non_stream: '非流式',
  stream: '流式',
  error: '错误',
  none: '无响应',
}
const respKindTypes: Record<string, 'primary' | 'success' | 'warning' | 'info' | 'danger'> = {
  non_stream: 'primary',
  stream: 'success',
  error: 'danger',
  none: 'info',
}
function respKindLabel(kind: string) {
  return respKindLabels[kind] || kind || '-'
}
function respKindType(kind: string) {
  return respKindTypes[kind] || 'info'
}

// 请求序号守卫：同一路由内快速切换（如详情→详情）或离开页面时，旧响应不得覆盖新数据。
const alive = ref(true)
let loadSeq = 0

onMounted(async () => {
  const my = ++loadSeq
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await getBilling(billingId, isSelf.value ? '/dev' : '/admin')
    if (!alive.value || my !== loadSeq) return
    detail.value = res.Data
  } catch (e: any) {
    if (!alive.value || my !== loadSeq) return
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    if (alive.value && my === loadSeq) loading.value = false
  }
})

onBeforeUnmount(() => {
  alive.value = false
  loadSeq++
})

function fmt(n: number | undefined) {
  return n == null ? '-' : Number(n.toFixed(5))
}
</script>

<template>
  <div class="page-container">
    <div v-loading="loading" class="card me-edit">
      <el-empty v-if="!loading && !detail" description="账单不存在" />
      <template v-else-if="detail">
        <el-descriptions :column="isNarrow ? 1 : 2" border>
          <el-descriptions-item label="时间" :span="isNarrow ? 1 : 2">{{ detail.CallTime }}</el-descriptions-item>
          <el-descriptions-item label="模型">
            {{ detail.Model }}
            <el-tag v-if="detail.AutoModelName" size="small" effect="plain" type="success">Auto {{ detail.AutoModelName }}</el-tag>
          </el-descriptions-item>
          <!-- admin 额外只读字段；self 模式（dev 响应不含该字段）不展示 -->
          <el-descriptions-item v-if="!isSelf" label="渠道">{{ detail.ChannelName || '-' }}</el-descriptions-item>
          <el-descriptions-item label="模式">
            <el-tag :type="detail.PricingMode === 'cost' ? 'warning' : 'primary'" size="small" effect="plain">
              {{ detail.PricingMode === 'cost' ? '按成本' : '按售价' }}
            </el-tag>
          </el-descriptions-item>
          <el-descriptions-item label="状态"><StatusTag :value="detail.Status" /></el-descriptions-item>
          <el-descriptions-item v-if="detail.CallLog" label="响应类型">
            <el-tag :type="respKindType(detail.CallLog.RespKind)" size="small" effect="plain">
              {{ respKindLabel(detail.CallLog.RespKind) }}
            </el-tag>
          </el-descriptions-item>
        </el-descriptions>

        <!-- 计费计算公式 + 请求参数/请求增量/响应/用量/决策轨迹：统一由调用详情面板承载
             （请求 ID 与一键复制在面板顶部，与下方各区块同一组件，避免两处口径漂移） -->
        <CallLogPanel
          :log="detail.CallLog"
          :show-internal="!isSelf"
          :billings-base="isSelf ? '/dev' : '/admin'"
          title="计费明细"
        >
          <template #prepend>
            <el-collapse-item name="calc" title="计费计算公式">
              <div class="calc-steps">
                <div v-for="(step, i) in detail.Steps" :key="i" class="calc-step">
                  <div class="step-header">
                    <span class="step-no">Step {{ i + 1 }}</span>
                    <span class="step-title">{{ step.Title }}</span>
                  </div>
                  <div v-for="(line, j) in step.Lines" :key="j" class="step-line">
                    <span class="line-label">{{ line.Label }}</span>
                    <span class="line-amount">{{ fmt(line.Amount) }}</span>
                  </div>
                  <div v-if="i === 1 && detail.RouteDiff" class="step-line route-diff">
                    <span class="line-label">{{ detail.RouteDiff.Label }}</span>
                    <span class="line-amount">{{ detail.RouteDiff.Value }}</span>
                  </div>
                  <div class="step-line step-subtotal">
                    <span class="line-label">小计</span>
                    <span class="line-amount"><b>{{ fmt(step.Subtotal) }}</b></span>
                  </div>
                </div>
              </div>
              <div class="result-strip">
                <span>最终应扣积分（≒）</span>
                <span class="result-value">{{ fmt(detail.CreditsConsumed) }}</span>
              </div>
              <div v-if="detail.PricingMode === 'cost' && detail.RouteDiff" class="cost-note">
                * 成本模式含路由差异调整，实际扣减与最终积分可能存在差值。
              </div>
            </el-collapse-item>
          </template>
        </CallLogPanel>
      </template>
      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
    </div>
  </div>
</template>

<style scoped>
.calc-steps {
  display: grid;
  gap: 14px;
}
.calc-step {
  border-left: 2px solid var(--el-color-primary-light-7);
  padding: 2px 0 2px 12px;
}
.step-header {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 4px;
}
.step-no {
  color: var(--brand-primary);
  font-weight: 700;
  font-size: 13px;
}
.step-title {
  font-weight: 600;
}
.step-line {
  display: flex;
  justify-content: space-between;
  padding: 3px 0;
  font-size: 13px;
}
.line-label {
  color: var(--color-text-secondary);
}
.line-amount {
  font-variant-numeric: tabular-nums;
}
.route-diff {
  color: var(--color-warning);
}
.step-subtotal {
  margin-top: 6px;
  padding-top: 6px;
  border-top: 1px dashed var(--color-border);
}
.step-subtotal .line-label {
  color: var(--color-text);
}
.result-strip {
  margin-top: 16px;
  background: var(--brand-primary-soft);
  border-radius: 8px;
  padding: 12px 18px;
  display: flex;
  justify-content: space-between;
  align-items: center;
  font-weight: 600;
}
.result-value {
  color: var(--brand-primary);
  font-size: 22px;
  font-weight: 700;
}
.cost-note {
  font-size: 12px;
  color: var(--color-text-secondary);
  margin-top: 8px;
}
/* 窄屏：单列、正文 ≥14px、长 ID/URL 不横向撑破 */
@media (max-width: 768px) {
  .page-container {
    padding: 12px 12px 20px;
  }
  .card.me-edit {
    padding: 14px 12px;
  }
  .step-line {
    font-size: 14px;
    gap: 12px;
    align-items: baseline;
  }
  .line-label {
    min-width: 0;
    word-break: break-word;
    overflow-wrap: anywhere;
  }
  .line-amount {
    flex-shrink: 0;
  }
  .result-strip {
    padding: 10px 12px;
    font-size: 14px;
  }
  .result-value {
    font-size: 20px;
  }
  .cost-note {
    font-size: 13px;
    line-height: 1.5;
  }
}
</style>