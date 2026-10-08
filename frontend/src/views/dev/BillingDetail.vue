<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { getDevBilling, type BillingDetail } from '@/api/dev'
import StatusTag from '@/components/StatusTag.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

const route = useRoute()
const billingId = String(route.params.id)

const loading = ref(false)
const detail = ref<BillingDetail | null>(null)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

onMounted(async () => {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await getDevBilling(billingId)
    detail.value = res.data
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
})

// 金额展示：去掉浮点噪声
function fmt(n: number | undefined) {
  return n == null ? '-' : Number(n.toFixed(5))
}
</script>

<template>
  <div class="page-container">
    <div v-loading="loading" class="card" style="max-width: 860px">
      <el-empty v-if="!loading && !detail" description="账单不存在" />
      <template v-else-if="detail">
        <!-- 账单概况 -->
        <el-descriptions :column="2" border>
          <el-descriptions-item label="时间" :span="2">{{ detail.CallTime }}</el-descriptions-item>
          <el-descriptions-item label="模型">{{ detail.Model }}</el-descriptions-item>
          <el-descriptions-item label="模式">
            <el-tag :type="detail.PricingMode === 'cost' ? 'warning' : 'primary'" size="small" effect="plain">
              {{ detail.PricingMode === 'cost' ? '按成本' : '按售价' }}
            </el-tag>
          </el-descriptions-item>
          <el-descriptions-item label="状态"><StatusTag :value="detail.Status" /></el-descriptions-item>
        </el-descriptions>

        <!-- 三步决算卡 -->
        <div class="steps">
          <el-card v-for="(step, i) in detail.Steps" :key="i" class="step-card" shadow="never">
            <template #header>
              <div class="step-header">
                <span class="step-no">Step {{ i + 1 }}</span>
                <span class="step-title">{{ step.Title }}</span>
              </div>
            </template>
            <div v-for="(line, j) in step.Lines" :key="j" class="step-line">
              <span class="line-label">{{ line.Label }}</span>
              <span class="line-amount">{{ fmt(line.Amount) }}</span>
            </div>
            <div v-if="i === 1 && detail.RouteDiff" class="step-line route-diff">
              <span class="line-label">{{ detail.RouteDiff.Label }}</span>
              <span class="line-amount">{{ detail.RouteDiff.Value }}</span>
            </div>
            <el-divider content-position="right" style="margin: 8px 0">
              <span class="subtotal">
                小计 <b>{{ fmt(step.Subtotal) }}</b>
              </span>
            </el-divider>
          </el-card>
        </div>

        <!-- 最终结果 -->
        <div class="result-strip">
          <span>最终应扣积分（≒）</span>
          <span class="result-value">{{ fmt(detail.CreditsConsumed) }}</span>
        </div>

        <div v-if="detail.PricingMode === 'cost' && detail.RouteDiff" class="cost-note">
          * 成本模式含路由差异调整，实际扣减与最终积分可能存在差值。
        </div>
      </template>
      <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
    </div>
  </div>
</template>

<style scoped>
.mono {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
}
.steps {
  display: grid;
  grid-template-columns: 1fr;
  gap: 12px;
  margin-top: 16px;
}
.step-card {
  border: 1px solid #f0f0f0;
}
.step-header {
  display: flex;
  align-items: center;
  gap: 8px;
}
.step-no {
  color: var(--color-primary);
  font-weight: 700;
  font-size: 13px;
}
.step-title {
  font-weight: 600;
}
.step-line {
  display: flex;
  justify-content: space-between;
  align-items: center;
  padding: 4px 0;
  font-size: 13px;
  color: var(--color-text);
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
.subtotal {
  font-size: 12px;
  color: var(--color-text-secondary);
}
.result-strip {
  margin-top: 16px;
  background: var(--color-primary-light);
  border-radius: 8px;
  padding: 14px 18px;
  display: flex;
  justify-content: space-between;
  align-items: center;
  font-weight: 600;
}
.result-value {
  color: var(--color-primary);
  font-size: 22px;
  font-weight: 700;
}
.cost-note {
  font-size: 12px;
  color: var(--color-text-secondary);
  margin-top: 8px;
}
</style>