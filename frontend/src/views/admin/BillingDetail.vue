<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { getBilling } from '@/api/admin'
import type { BillingDetail } from '@/api/dev'
import StatusTag from '@/components/StatusTag.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

// admin 详情额外只读字段（内部模型 ID / 渠道名）
interface AdminBillingDetail extends BillingDetail {
  internal_model_id?: string
  channel_name?: string
}

const route = useRoute()
const billingId = String(route.params.id)

const loading = ref(false)
const detail = ref<AdminBillingDetail | null>(null)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

onMounted(async () => {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await getBilling(billingId)
    detail.value = res.data
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
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
        <el-descriptions :column="2" border>
          <el-descriptions-item label="账单 ID" :span="2">
            <span class="mono">{{ detail.billing_id }}</span>
          </el-descriptions-item>
          <el-descriptions-item label="时间">{{ detail.call_time }}</el-descriptions-item>
          <el-descriptions-item label="模型">{{ detail.model }}</el-descriptions-item>
          <!-- admin 额外只读字段 -->
          <el-descriptions-item label="内部模型 ID">{{ detail.internal_model_id || '-' }}</el-descriptions-item>
          <el-descriptions-item label="渠道">{{ detail.channel_name || '-' }}</el-descriptions-item>
          <el-descriptions-item label="模式">
            <el-tag :type="detail.pricing_mode === 'cost' ? 'warning' : 'primary'" size="small" effect="plain">
              {{ detail.pricing_mode === 'cost' ? '按成本' : '按售价' }}
            </el-tag>
          </el-descriptions-item>
          <el-descriptions-item label="状态"><StatusTag :value="detail.status" /></el-descriptions-item>
        </el-descriptions>

        <div class="steps">
          <el-card v-for="(step, i) in detail.steps" :key="i" class="step-card" shadow="never">
            <template #header>
              <div class="step-header">
                <span class="step-no">Step {{ i + 1 }}</span>
                <span class="step-title">{{ step.title }}</span>
              </div>
            </template>
            <div v-for="(line, j) in step.lines" :key="j" class="step-line">
              <span class="line-label">{{ line.label }}</span>
              <span class="line-amount">{{ fmt(line.amount) }}</span>
            </div>
            <div v-if="i === 1 && detail.route_diff" class="step-line route-diff">
              <span class="line-label">{{ detail.route_diff.label }}</span>
              <span class="line-amount">{{ detail.route_diff.value }}</span>
            </div>
            <el-divider content-position="right" style="margin: 8px 0">
              <span class="subtotal">小计 <b>{{ fmt(step.subtotal) }}</b></span>
            </el-divider>
          </el-card>
        </div>

        <div class="result-strip">
          <span>最终应扣积分（≒）</span>
          <span class="result-value">{{ fmt(detail.credits_consumed) }}</span>
        </div>

        <div v-if="detail.pricing_mode === 'cost' && detail.route_diff" class="cost-note">
          * 成本模式含路由差异调整，实际扣减与最终积分可能存在差值。
        </div>
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
.mono {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
}
.steps {
  display: grid;
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
  padding: 4px 0;
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