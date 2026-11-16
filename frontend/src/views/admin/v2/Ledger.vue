<script setup lang="ts">
// 账本流水（P05）——上游账本条目（变动成本真源）与拉取新鲜度。
//
// 口径：数据层契约 §6.1（ledger_entries 九字段，翻页固定 occurred_at ASC + 幂等键 ledger_id），
// Spec `2026-11-16-stage3-frontend-13-pages.md` P05：回答「成本数据是不是最新的」。
//
// 一条刻意的调整（相对上一版）：**预扣登记不单独占页签**——Spec §1.2 明确「预扣/差额结算」是单笔请求的
// 资金轨迹，落在账单详情（P09）的「资金轨迹」块，全局视图在盈亏页。挂在账本页会让运营以为预扣是账本的一部分。
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { getLedger, getLedgerStatus, type LedgerStatus, type LedgerView } from '@/api/v2'
import { mockLedger, mockLedgerStatus } from '@/mock/ledger'
import { MOCK_ENABLED } from '@/mock'
import { useV2Load } from '@/composables/useV2Load'
import V2Page from '@/components/v2/V2Page.vue'
import StatCards from '@/components/v2/StatCards.vue'
import type { StatItem, V2Dep } from '@/components/v2/types'
import { fmtCompact, fmtDateTime } from '@/utils/format'

const hours = ref(24)
const filters = ref({ provider: '', account: '', model: '', ledgerId: '' })
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

function params() {
  return {
    ...rangeParams(),
    Provider: filters.value.provider || undefined,
    Account: filters.value.account || undefined,
    Model: filters.value.model || undefined,
    // ledger_id 精确查（Spec P05 关键交互）
    LedgerID: filters.value.ledgerId || undefined,
    Limit: 200,
  }
}

const ledger = useV2Load<LedgerView>(() => getLedger(params()), mockLedger)
// 新鲜度卡（L2）单独取：账本可能为空但拉取状态有意义（例如关闭/持续失败）
const status = ref<LedgerStatus | null>(null)

onMounted(() => {
  void ledger.load()
  void loadStatus()
})

async function loadStatus() {
  const res = await getLedgerStatus()
  if (res.state === 'live') status.value = res.data
  // 演示环境：新鲜度卡也用 mock（否则演示时整块是空的，看不出橙色/红色阈值）
  else status.value = MOCK_ENABLED ? mockLedgerStatus() : null
}

function reload() {
  void ledger.load(() => getLedger(params()))
  void loadStatus()
}

const deps = computed<V2Dep[]>(() => [
  { path: 'GET /admin/ledger/entries', state: ledger.state.value === 'live' ? 'ready' : 'pending' },
  { path: 'GET /admin/ledger/status', state: status.value ? 'ready' : 'pending' },
])

// 演示态下也把新鲜度装作有数据（否则演示时看不到橙/红阈值这条核心语义）
const statusView = computed<LedgerStatus | null>(() =>
  status.value || (ledger.state.value === 'demo' ? mockLedgerStatus() : null),
)

// 滞后阈值：>180s 橙「账本不新鲜」，>15min 红「对账不可用」（Spec P05 验收 1）
const lagLevel = computed<'ok' | 'warn' | 'danger'>(() => {
  const lag = statusView.value?.LagSeconds
  if (lag == null) return 'ok'
  if (lag > 900) return 'danger'
  if (lag > 180) return 'warn'
  return 'ok'
})
const lagText = computed(() => {
  const lag = statusView.value?.LagSeconds
  if (lag == null) return '未知'
  if (lag < 60) return `${lag}s`
  if (lag < 3600) return `${Math.round(lag / 60)} 分钟`
  return `${(lag / 3600).toFixed(1)} 小时`
})

const stats = computed<StatItem[]>(() => {
  const d = ledger.data.value
  if (!d) return []
  return [
    { label: '条目数', value: fmtCompact(d.Total), hint: '服务端 count；按 occurred_at 升序，最多 200 条/次' },
    { label: '成本合计（积分）', value: d.CostTotal.toFixed(2), hint: 'Σ cost_credits（已是积分，不做币种换算）' },
    { label: '输入 token', value: fmtCompact(d.TokensIn) },
    { label: '输出 token', value: fmtCompact(d.TokensOut) },
  ]
})

function money(v: number): string {
  return v.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 5 })
}

async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success('已复制账本幂等键')
  } catch {
    ElMessage.warning(`复制失败，请手动选择：${text}`)
  }
}
</script>

<template>
  <V2Page
    title="账本流水"
    intro="上游网关账本的镜像（只读，九字段），也是变动成本的唯一真源。本页回答一件事：成本数据是不是最新的。"
    source="数据层契约 §6.1（ledger_entries）· Spec P05 / L1·L2"
    :deps="deps"
    :state="ledger.state.value"
    :error="ledger.error.value"
    pending-hint="账本读取（L1 /admin/ledger/entries）与新鲜度（L2 /admin/ledger/status）待 Core 上线；字段名已按契约冻结。"
    @reload="reload()"
    @demo="ledger.showDemo()"
    @exit-demo="ledger.exitDemo()"
  >
    <template #actions>
      <el-button size="small" @click="$router.push('/admin/reconcile')">看对账窗口</el-button>
    </template>

    <div class="toolbar">
      <el-select v-model="hours" size="small" style="width: 130px" :disabled="!!dateRange" @change="reload">
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
        style="width: 320px"
        @change="reload"
      />
      <el-input v-model="filters.provider" size="small" placeholder="供应商" clearable style="width: 120px" @change="reload" />
      <el-input v-model="filters.account" size="small" placeholder="账号" clearable style="width: 120px" @change="reload" />
      <el-input v-model="filters.model" size="small" placeholder="上游模型" clearable style="width: 130px" @change="reload" />
      <el-input
        v-model="filters.ledgerId"
        size="small"
        placeholder="ledger_id 精确查"
        clearable
        style="width: 180px"
        @change="reload"
      />
    </div>

    <!-- 新鲜度卡：账本关闭 / 从未拉到 / 滞后 三态分开说 -->
    <div v-if="statusView" class="fresh" :class="`is-${lagLevel}`">
      <div class="fresh-item">
        <span class="fresh-label">数据滞后</span>
        <strong>{{ lagText }}</strong>
        <el-tag v-if="lagLevel === 'warn'" type="warning" size="small" effect="plain">账本不新鲜</el-tag>
        <el-tag v-else-if="lagLevel === 'danger'" type="danger" size="small" effect="plain">对账不可用</el-tag>
      </div>
      <div class="fresh-item">
        <span class="fresh-label">最近成功拉取</span>
        <strong>{{ statusView?.LastSuccessAt ? fmtDateTime(statusView.LastSuccessAt) : '从未' }}</strong>
      </div>
      <div class="fresh-item">
        <span class="fresh-label">最新入账时间</span>
        <strong>{{ statusView?.LastOccurredAt ? fmtDateTime(statusView.LastOccurredAt) : '—' }}</strong>
      </div>
      <div class="fresh-item">
        <span class="fresh-label">blocked / error 窗口</span>
        <strong>{{ statusView?.BlockedWindows }} / {{ statusView?.ErrorWindows }}</strong>
        <router-link
          v-if="(statusView?.BlockedWindows ?? 0) + (statusView?.ErrorWindows ?? 0) > 0"
          to="/admin/reconcile"
          class="fresh-link"
        >去看对账</router-link>
      </div>
      <div class="fresh-item">
        <span class="fresh-label">待补拉 offset</span>
        <strong>{{ fmtCompact(statusView?.PendingOffset ?? 0) }}</strong>
        <el-tag v-if="statusView?.BudgetExhausted" type="warning" size="small" effect="plain">本轮预算用尽</el-tag>
      </div>
    </div>

    <el-alert
      v-else-if="ledger.state.value !== 'live' && ledger.state.value !== 'demo'"
      type="info"
      :closable="false"
      show-icon
      title="新鲜度状态未就绪"
      description="L2（/admin/ledger/status）还没上线：无法判断账本是否新鲜，也就无法判断对账是否可用。"
    />

    <template v-if="ledger.data.value">
      <StatCards :items="stats" :columns="4" />
      <div class="card">
        <div class="table-wrap">
          <el-table :data="ledger.data.value.Items" size="small" border stripe empty-text="区间内没有账本条目">
            <el-table-column label="入账时间" width="150">
              <template #default="{ row }">{{ fmtDateTime(row.OccurredAt) }}</template>
            </el-table-column>
            <el-table-column prop="Provider" label="供应商" width="115" show-overflow-tooltip />
            <el-table-column prop="ProviderAccount" label="账号" width="115" show-overflow-tooltip />
            <el-table-column prop="Model" label="上游模型" min-width="140" show-overflow-tooltip />
            <el-table-column label="输入 token" width="105" align="right">
              <template #default="{ row }">{{ fmtCompact(row.TokensIn) }}</template>
            </el-table-column>
            <el-table-column label="输出 token" width="105" align="right">
              <template #default="{ row }">{{ fmtCompact(row.TokensOut) }}</template>
            </el-table-column>
            <el-table-column label="成本（积分）" width="115" align="right">
              <template #default="{ row }">{{ money(row.CostCredits) }}</template>
            </el-table-column>
            <el-table-column label="账本幂等键" width="190">
              <template #default="{ row }">
                <code class="ledger-id">{{ row.LedgerID }}</code>
                <el-button size="small" text type="primary" @click="copy(row.LedgerID)">复制</el-button>
              </template>
            </el-table-column>
          </el-table>
        </div>
        <p class="foot">
          幂等键重复的条目落库时被吸收（ON CONFLICT DO NOTHING），同一 ledger_id 只会出现一次；金额列已是积分，不做币种换算。
        </p>
      </div>
    </template>

    <!-- 空态三选一：账本关闭 / 从未拉到 / 区间无数据（Spec P05 空态要求） -->
    <el-empty
      v-else-if="ledger.state.value === 'live'"
      :description="
        statusView && !statusView.LedgerEnabled
          ? '账本拉取已关闭（upstream.degrade.ledger_enabled=false）：成本只剩订阅摊销。去「系统设置 → 上游（V2）」开启。'
          : statusView && !statusView.LastSuccessAt
            ? '账本为空：从未成功拉取，成本只剩订阅摊销。'
            : '区间内没有账本条目'
      "
    />
  </V2Page>
</template>

<style scoped>
.toolbar {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 10px;
}
.fresh {
  display: flex;
  flex-wrap: wrap;
  gap: 18px;
  padding: 10px 12px;
  border-radius: var(--radius-s);
  border: 1px solid var(--color-border);
  background: #fafbfd;
  font-size: 13px;
}
.fresh.is-warn {
  background: #fff8e6;
  border-color: #ffe0a3;
}
.fresh.is-danger {
  background: #fef3f2;
  border-color: #fecdca;
}
.fresh-item {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.fresh-label {
  color: var(--color-text-tertiary);
}
.fresh-link {
  font-size: 12px;
  color: var(--color-primary);
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
.ledger-id {
  font-size: 12px;
  color: var(--color-text-secondary);
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
