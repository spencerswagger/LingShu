<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { getDevWallet, listDevFlows, type FlowItem } from '@/api/dev'
import { fmtDate, fmtTime } from '@/utils/format'
import ErrorBubble from '@/components/ErrorBubble.vue'

const loading = ref(false)
const flowsLoading = ref(false)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

const balance = ref<number | null>(null)
const flows = ref<FlowItem[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)

// 余额状态：与开发端仪表盘保持一致的提示口径
const balanceState = computed(() => {
  const b = balance.value ?? 0
  if (b <= 0) return { cls: 'danger', text: '余额为 0，请联系管理员充值' }
  if (b < 50) return { cls: 'warning', text: '余额过低，请联系管理员充值' }
  return { cls: 'ok', text: '余额充足' }
})

// 流水类型
const flowType = (t: string) =>
  ({
    recharge: { label: '充值', type: 'success' },
    consume: { label: '消费', type: 'warning' },
    adjust: { label: '差额', type: 'info' },
    set: { label: '设置', type: 'primary' },
  }[t] || { label: t, type: 'info' })

// 积分数值（千分位，最多 4 位小数）
function fmtCredits(n?: number): string {
  if (n == null || isNaN(n)) return '-'
  return n.toLocaleString(undefined, { maximumFractionDigits: 4 })
}
// 系统生成备注规范化（兼容历史英文与旧中文 remark）。
const FLOW_REMARK_MAP: Record<string, string> = {
  '实际调用差额': '实际调用差额',
  '预扣费': '预扣费',
  gateway: '实际调用差额',
  '网关调用': '实际调用差额',
  'gateway preconsume': '预扣费',
  '网关预扣费': '预扣费',
  probe: '健康探测',
  'gateway refund settle': '结算退回',
  'gateway refund settle hold': '结算退回',
  'gateway refund rate limited, all candidates failed': '请求失败退回（限流）',
  'gateway refund no available channel': '请求失败退回（无可用渠道）',
}
function flowRemark(row: FlowItem): string {
  if (!row.Remark) return '-'
  return FLOW_REMARK_MAP[row.Remark] ?? row.Remark
}

async function loadWallet() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await getDevWallet()
    balance.value = res.data.balance
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}

async function loadFlows() {
  flowsLoading.value = true
  try {
    const res = await listDevFlows(page.value, size.value)
    flows.value = res.data.list
    total.value = res.data.total
  } catch {
    flows.value = []
    total.value = 0
  } finally {
    flowsLoading.value = false
  }
}

async function reload() {
  await Promise.all([loadWallet(), loadFlows()])
}

onMounted(reload)
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">我的钱包</div>
      <el-button :loading="loading || flowsLoading" @click="reload">刷新</el-button>
    </div>

    <div class="card" v-loading="loading">
      <div class="wallet-head">
        <div class="balance-card">
          <div class="balance-label">当前余额（积分）</div>
          <div class="balance-value" :class="balanceState.cls">{{ balance ?? '-' }}</div>
          <el-tag
            :type="balanceState.cls === 'danger' ? 'danger' : balanceState.cls === 'warning' ? 'warning' : 'success'"
            size="small"
            effect="light"
          >
            {{ balanceState.text }}
          </el-tag>
        </div>
        <div class="wallet-tip">
          <p>钱包用于按调用量扣减积分。余额不足时请<strong>联系管理员充值</strong>（自助充值暂未开放）。</p>
          <p>每笔调用消耗的积分可在「消费账单」中查看逐步计费明细。</p>
        </div>
      </div>
    </div>

    <div class="card" style="margin-top: 16px">
      <div class="block-title">积分流水</div>
      <div v-loading="flowsLoading">
        <el-table :data="flows" border stripe class="table-nowrap" empty-text="暂无流水">
          <el-table-column label="时间" min-width="150">
            <template #default="{ row }">
              <div class="t-time">
                <span class="t-date">{{ fmtDate(row.CreatedAt) }}</span>
                <span class="t-clock">{{ fmtTime(row.CreatedAt) }}</span>
              </div>
            </template>
          </el-table-column>
          <el-table-column label="类型" width="90" align="center">
            <template #default="{ row }">
              <el-tag :type="(flowType(row.Type).type as any)" size="small" effect="light">
                {{ flowType(row.Type).label }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="会话" min-width="140" show-overflow-tooltip>
            <template #default="{ row }">
              <span v-if="row.SessionName">{{ row.SessionName }}</span>
              <span v-else class="t-clock">-</span>
            </template>
          </el-table-column>
          <el-table-column label="备注" min-width="140" show-overflow-tooltip>
            <template #default="{ row }">{{ flowRemark(row) }}</template>
          </el-table-column>
          <el-table-column label="金额" width="140" align="right">
            <template #default="{ row }">
              <div class="t-time amount-cell">
                <span :class="row.Amount >= 0 ? 'pos' : 'neg'">
                  {{ row.Amount >= 0 ? '+' : '' }}{{ fmtCredits(row.Amount) }}
                </span>
                <span class="t-clock">余额 {{ fmtCredits(row.Balance) }}</span>
              </div>
            </template>
          </el-table-column>
        </el-table>
        <div class="pager">
          <el-pagination
            layout="total, prev, pager, next"
            :total="total"
            :page-size="size"
            :current-page="page"
            @current-change="(p: number) => { page = p; loadFlows() }"
          />
        </div>
      </div>
    </div>

    <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
  </div>
</template>

<style scoped>
.t-time {
  display: flex;
  flex-direction: column;
  line-height: 1.35;
}
.t-date {
  font-size: 12px;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
}
.t-clock {
  font-size: 12px;
  color: var(--color-text-tertiary);
  font-variant-numeric: tabular-nums;
}
.amount-cell {
  align-items: flex-end;
}
.wallet-head {
  display: flex;
  align-items: center;
  gap: 28px;
  flex-wrap: wrap;
}
.balance-card {
  background: var(--color-primary-light);
  border-radius: 8px;
  padding: 18px 24px;
  min-width: 220px;
  display: flex;
  flex-direction: column;
  gap: 8px;
  align-items: flex-start;
}
.balance-label {
  font-size: 13px;
  color: var(--color-text-secondary);
}
.balance-value {
  font-size: 32px;
  font-weight: 700;
  line-height: 1.1;
  font-variant-numeric: tabular-nums;
}
.balance-value.danger {
  color: var(--color-danger);
}
.balance-value.warning {
  color: var(--color-warning);
}
.balance-value.ok {
  color: var(--color-primary);
}
.wallet-tip {
  color: var(--color-text-secondary);
  font-size: 13px;
  line-height: 1.9;
}
.wallet-tip p {
  margin: 0;
}
.block-title {
  font-size: 16px;
  font-weight: 700;
  margin-bottom: 12px;
}
.pos {
  color: var(--color-success);
}
.neg {
  color: var(--color-danger);
}
.pager {
  display: flex;
  justify-content: flex-end;
  margin-top: 16px;
}
</style>
