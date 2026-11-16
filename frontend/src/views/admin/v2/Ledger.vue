<script setup lang="ts">
// 账本流水 + 预扣登记。
//
// 账本（ledger_entries）：上游网关账本的镜像，九个字段一列不多（数据层契约 §6.1）。
//   翻页固定 occurred_at ASC + 幂等键 ledger_id；前端只读，不提供任何「改账」入口。
// 预扣（credit_holds）：阶段二启用，一请求一预扣（billing_id 唯一）。
//   阶段二口径：请求失败或没有 usage → 整笔退预扣、状态 failed/no-usage，不做估算计费；
//   所以这里看到 refunded 是正常结果，不是异常。
import { computed, onMounted, ref } from 'vue'
import {
  getCreditHolds,
  getLedger,
  type HoldStatus,
  type HoldView,
  type LedgerView,
} from '@/api/v2'
import { mockHolds, mockLedger } from '@/api/v2.mock'
import { useV2Load } from '@/composables/useV2Load'
import V2Page from '@/components/v2/V2Page.vue'
import StatCards from '@/components/v2/StatCards.vue'
import StatusPill from '@/components/v2/StatusPill.vue'
import type { StatItem, V2Dep } from '@/components/v2/types'
import { fmtCompact, fmtDateTime } from '@/utils/format'

const tab = ref<'ledger' | 'holds'>('ledger')

// ===== 账本 =====
const hours = ref(24)
const filters = ref({ provider: '', account: '', model: '' })
const dateRange = ref<[string, string] | null>(null)

function rangeParams() {
  if (dateRange.value && dateRange.value.length === 2) {
    return {
      From: new Date(dateRange.value[0]).toISOString(),
      To: new Date(dateRange.value[1]).toISOString(),
    }
  }
  const to = new Date()
  const from = new Date(to.getTime() - hours.value * 3600_000)
  return { From: from.toISOString(), To: to.toISOString() }
}

const ledger = useV2Load<LedgerView>(
  () =>
    getLedger({
      ...rangeParams(),
      Provider: filters.value.provider || undefined,
      Account: filters.value.account || undefined,
      Model: filters.value.model || undefined,
      Limit: 200,
    }),
  mockLedger,
)

function reloadLedger() {
  void ledger.load(() =>
    getLedger({
      ...rangeParams(),
      Provider: filters.value.provider || undefined,
      Account: filters.value.account || undefined,
      Model: filters.value.model || undefined,
      Limit: 200,
    }),
  )
}

// ===== 预扣 =====
const holdStatus = ref<HoldStatus | ''>('')
const holds = useV2Load<HoldView>(
  () => getCreditHolds({ Status: holdStatus.value || undefined, Limit: 200 }),
  mockHolds,
)

function reloadHolds() {
  void holds.load(() => getCreditHolds({ Status: holdStatus.value || undefined, Limit: 200 }))
}

onMounted(() => void ledger.load())

function switchTab(name: string) {
  tab.value = name === 'holds' ? 'holds' : 'ledger'
  if (tab.value === 'holds' && holds.state.value === 'loading' && !holds.data.value) void holds.load()
}

// 页头依赖徽标：两个接口都待 Core 上线（阶段二）
const deps = computed<V2Dep[]>(() => [
  { path: 'GET /admin/ledger', state: ledger.state.value === 'live' || ledger.state.value === 'demo' ? 'ready' : 'pending' },
  { path: 'GET /admin/credit-holds', state: holds.state.value === 'live' || holds.state.value === 'demo' ? 'ready' : 'pending' },
])

const pageState = computed(() => (tab.value === 'holds' ? holds.state.value : ledger.state.value))
const pageError = computed(() => (tab.value === 'holds' ? holds.error.value : ledger.error.value))

const ledgerStats = computed<StatItem[]>(() => {
  const d = ledger.data.value
  if (!d) return []
  return [
    { label: '条目数', value: fmtCompact(d.Total), hint: '按 occurred_at 升序返回，最多 200 条/次' },
    { label: '成本合计（积分）', value: d.CostTotal.toFixed(2), hint: 'Σ cost_credits（账本口径，未换算）' },
    { label: '输入 token', value: fmtCompact(d.TokensIn) },
    { label: '输出 token', value: fmtCompact(d.TokensOut) },
  ]
})

const holdStats = computed<StatItem[]>(() => {
  const d = holds.data.value
  if (!d) return []
  return [
    { label: '预扣中笔数', value: fmtCompact(d.HeldCount), tone: d.HeldCount > 0 ? 'warn' : 'default' },
    { label: '预扣中金额（积分）', value: d.HeldAmount.toFixed(2), hint: '未结算的预付占用，15 分钟未结算自动过期' },
    { label: '登记总数', value: fmtCompact(d.Items.length) },
  ]
})

function money(v: number): string {
  return v.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 5 })
}
</script>

<template>
  <V2Page
    title="账本与预扣"
    intro="账本是上游网关流水的镜像（只读，九字段）；预扣是一请求一预扣的登记与终结状态。两者都不做金额换算，换算只在成本侧一次。"
    source="数据层契约 §6（ledger_entries / credit_holds）"
    :deps="deps"
    :state="pageState"
    :error="pageError"
    pending-hint="账本与预扣的读取接口属阶段二交付（A1/A4），未上线前本页只出空态；表结构与字段名已按契约冻结。"
    @reload="tab === 'holds' ? reloadHolds() : reloadLedger()"
    @demo="tab === 'holds' ? holds.showDemo() : ledger.showDemo()"
    @exit-demo="tab === 'holds' ? holds.exitDemo() : ledger.exitDemo()"
  >
    <el-tabs :model-value="tab" @tab-change="switchTab">
      <el-tab-pane label="账本流水" name="ledger">
        <div class="toolbar">
          <el-select v-model="hours" size="small" style="width: 130px" :disabled="!!dateRange" @change="reloadLedger">
            <el-option label="最近 1 小时" :value="1" />
            <el-option label="最近 24 小时" :value="24" />
            <el-option label="最近 7 天" :value="168" />
          </el-select>
          <el-date-picker
            v-model="dateRange"
            type="datetimerange"
            size="small"
            start-placeholder="开始"
            end-placeholder="结束"
            value-format="YYYY-MM-DDTHH:mm:ss"
            style="width: 340px"
            @change="reloadLedger"
          />
          <el-input v-model="filters.provider" size="small" placeholder="供应商" clearable style="width: 130px" @change="reloadLedger" />
          <el-input v-model="filters.account" size="small" placeholder="账号" clearable style="width: 130px" @change="reloadLedger" />
          <el-input v-model="filters.model" size="small" placeholder="上游模型" clearable style="width: 140px" @change="reloadLedger" />
        </div>

        <template v-if="ledger.data.value">
          <StatCards :items="ledgerStats" :columns="4" />
          <div class="card">
            <div class="table-wrap">
              <el-table :data="ledger.data.value.Items" size="small" border stripe empty-text="区间内没有账本条目">
                <el-table-column label="入账时间" width="150">
                  <template #default="{ row }">{{ fmtDateTime(row.OccurredAt) }}</template>
                </el-table-column>
                <el-table-column prop="Provider" label="供应商" width="120" show-overflow-tooltip />
                <el-table-column prop="ProviderAccount" label="账号" width="120" show-overflow-tooltip />
                <el-table-column prop="Model" label="上游模型" min-width="150" show-overflow-tooltip />
                <el-table-column label="输入 token" width="110" align="right">
                  <template #default="{ row }">{{ fmtCompact(row.TokensIn) }}</template>
                </el-table-column>
                <el-table-column label="输出 token" width="110" align="right">
                  <template #default="{ row }">{{ fmtCompact(row.TokensOut) }}</template>
                </el-table-column>
                <el-table-column label="成本（积分）" width="120" align="right">
                  <template #default="{ row }">{{ money(row.CostCredits) }}</template>
                </el-table-column>
                <el-table-column prop="LedgerID" label="账本幂等键" width="140" show-overflow-tooltip />
              </el-table>
            </div>
            <p class="foot">幂等键重复的条目在落库时被吸收（ON CONFLICT DO NOTHING），所以同一 ledger_id 只会出现一次。</p>
          </div>
        </template>
        <el-empty v-else-if="ledger.state.value === 'live'" description="区间内没有账本条目" />
      </el-tab-pane>

      <el-tab-pane label="预扣登记" name="holds">
        <div class="toolbar">
          <el-select v-model="holdStatus" size="small" style="width: 150px" @change="reloadHolds">
            <el-option label="全部状态" value="" />
            <el-option label="预扣中 held" value="held" />
            <el-option label="已结算 settled" value="settled" />
            <el-option label="已退预扣 refunded" value="refunded" />
            <el-option label="已过期 expired" value="expired" />
          </el-select>
        </div>

        <template v-if="holds.data.value">
          <StatCards :items="holdStats" :columns="3" />
          <div class="card">
            <div class="table-wrap">
              <el-table :data="holds.data.value.Items" size="small" border stripe empty-text="没有预扣登记">
                <el-table-column label="创建时间" width="150">
                  <template #default="{ row }">{{ fmtDateTime(row.CreatedAt) }}</template>
                </el-table-column>
                <el-table-column prop="BillingID" label="账单（billing_id）" width="150" show-overflow-tooltip />
                <el-table-column prop="UserID" label="用户" width="90" align="right" />
                <el-table-column prop="TokenID" label="令牌" width="90" align="right" />
                <el-table-column label="状态" width="110">
                  <template #default="{ row }"><StatusPill domain="hold" :value="row.Status" /></template>
                </el-table-column>
                <el-table-column label="金额（积分）" width="120" align="right">
                  <template #default="{ row }">{{ money(row.Amount) }}</template>
                </el-table-column>
                <el-table-column label="到期" width="150">
                  <template #default="{ row }">{{ fmtDateTime(row.ExpiresAt) }}</template>
                </el-table-column>
                <el-table-column label="终结时间" width="150">
                  <template #default="{ row }">{{ row.ClosedAt ? fmtDateTime(row.ClosedAt) : '—' }}</template>
                </el-table-column>
              </el-table>
            </div>
            <p class="foot">无 usage / 请求失败一律整笔退预扣（refunded），不做估算计费；held 超时由扫描器置 expired。</p>
          </div>
        </template>
        <el-empty v-else-if="holds.state.value === 'live'" description="没有预扣登记" />
      </el-tab-pane>
    </el-tabs>
  </V2Page>
</template>

<style scoped>
.toolbar {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 10px;
}
.card {
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-m);
  box-shadow: var(--shadow-card);
  padding: 12px 14px;
}
.table-wrap {
  overflow-x: auto;
}
.foot {
  margin: 8px 0 0;
  font-size: 12px;
  color: var(--color-text-tertiary);
  line-height: 1.6;
}

@media (max-width: 640px) {
  .toolbar :deep(.el-date-editor) {
    width: 100% !important;
  }
  .card {
    padding: 10px;
  }
  :deep(.el-table) {
    font-size: 14px;
  }
}
</style>
