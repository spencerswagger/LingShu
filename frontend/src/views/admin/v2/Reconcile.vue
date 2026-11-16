<script setup lang="ts">
// 对账窗口：让「漏账」看得见。
//
// 判读口径（不是本页发明，来自数据层契约 §7）：
//   - 计数列 null = 还没算过（对账作业没写），**不是 0 笔**；
//   - ok = 已拉完且偏差在阈值内；alarm = 偏差超 2%（upstream_tokens ≥ 10000 才判）；
//   - blocked = 单轮 4000 条预算用尽、待补拉（看 ScrollOffset 进度）；
//   - error = 本窗没拉完（拉取/落库失败）；skipped = 没有对账结论（账本关闭/不可用/仍 blocked）；
//   - 只比 token 与笔数，**不比金额**（账本币种与积分不等价，比金额必然误报）。
import { computed, onMounted, ref } from 'vue'
import { getReconcileWindows, type ReconcileView, type ReconcileWindow } from '@/api/v2'
import { mockReconcile } from '@/api/v2.mock'
import { useV2Load } from '@/composables/useV2Load'
import V2Page from '@/components/v2/V2Page.vue'
import StatCards from '@/components/v2/StatCards.vue'
import StatusPill from '@/components/v2/StatusPill.vue'
import type { StatItem, V2Dep } from '@/components/v2/types'
import { fmtCompact, fmtDateTime } from '@/utils/format'

const RANGES = [
  { label: '最近 1 小时', minutes: 60 },
  { label: '最近 6 小时', minutes: 360 },
  { label: '最近 24 小时', minutes: 1440 },
]

const minutes = ref(60)
const limit = ref(500)

function rangeParams() {
  const to = new Date()
  const from = new Date(to.getTime() - minutes.value * 60_000)
  return { From: from.toISOString(), To: to.toISOString(), Limit: limit.value }
}

const { state, data, error, load, showDemo, exitDemo } = useV2Load<ReconcileView>(
  () => getReconcileWindows(rangeParams()),
  mockReconcile,
)

function reload() {
  void load(() => getReconcileWindows(rangeParams()))
}

onMounted(() => void load())

const deps = computed<V2Dep[]>(() => [
  {
    path: 'GET /admin/ops/reconcile-windows',
    state: state.value === 'live' || state.value === 'demo' ? 'ready' : 'pending',
  },
])

const items = computed<ReconcileWindow[]>(() => data.value?.Items || [])

const stats = computed<StatItem[]>(() => {
  const rows = items.value
  const alarm = rows.filter((r) => r.Status === 'alarm').length
  const unfinished = rows.filter((r) => r.Status === 'error' || r.Status === 'blocked').length
  const skipped = rows.filter((r) => r.Status === 'skipped').length
  const noCount = rows.filter((r) => r.LocalCount == null || r.UpstreamCount == null).length
  const devs = rows.map((r) => Math.abs(r.DeviationRate ?? 0))
  const maxDev = devs.length ? Math.max(...devs) : 0
  return [
    { label: '窗口数', value: fmtCompact(rows.length), hint: `请求上限 ${limit.value}` },
    { label: '偏差超阈（alarm）', value: fmtCompact(alarm), tone: alarm > 0 ? 'danger' : 'ok', hint: '连续 3 窗 alarm → 按回退判据处理' },
    { label: '没拉完（error / blocked）', value: fmtCompact(unfinished), tone: unfinished > 0 ? 'warn' : 'default', hint: 'blocked 看「补拉进度」是否在推进' },
    { label: '无结论（skipped）', value: fmtCompact(skipped), hint: '账本关闭/不可用/仍 blocked → 不当作「对上了」' },
    { label: '计数列为空', value: fmtCompact(noCount), tone: noCount > 0 ? 'warn' : 'default', hint: '空 = 对账作业还没写这四列，不是 0 笔' },
    { label: '最大偏差率', value: maxDev ? `${(maxDev * 100).toFixed(2)}%` : '—', tone: maxDev > 0.02 ? 'danger' : 'default', hint: '|本窗本地 token − 上游 token| ÷ 上游 token' },
  ]
})

function devText(v: number | null): string {
  return v == null ? '未算' : `${(v * 100).toFixed(2)}%`
}

function countText(v: number | null): string {
  // null 与 0 是两种结论，必须分开显示
  return v == null ? '未算' : fmtCompact(v)
}

function devClass(v: number | null): string {
  if (v == null) return 'dev-none'
  const a = Math.abs(v)
  if (a > 0.1) return 'dev-bad'
  if (a > 0.02) return 'dev-warn'
  return ''
}
</script>

<template>
  <V2Page
    title="对账窗口"
    intro="60 秒一窗，只比 token 与笔数（不比金额）。计数列为空 = 还没算过；blocked = 预算用尽待补拉；skipped = 没有结论。"
    source="数据层契约 §7 + backend/internal/domain/console/ops.go（GET /admin/ops/reconcile-windows）"
    :deps="deps"
    :state="state"
    :error="error"
    pending-hint="对账窗口接口在阶段二分支定型；计数四列的写入方也在阶段二补齐，接口上线前本页只出空态。"
    @reload="reload"
    @demo="showDemo"
    @exit-demo="exitDemo"
  >
    <template #actions>
      <el-select v-model="minutes" size="small" style="width: 130px" @change="reload">
        <el-option v-for="r in RANGES" :key="r.minutes" :label="r.label" :value="r.minutes" />
      </el-select>
      <el-select v-model="limit" size="small" style="width: 110px" @change="reload">
        <el-option label="500 窗" :value="500" />
        <el-option label="1000 窗" :value="1000" />
        <el-option label="2000 窗" :value="2000" />
      </el-select>
    </template>

    <template v-if="data">
      <StatCards :items="stats" :columns="3" />

      <el-alert
        type="info"
        :closable="false"
        show-icon
        title="状态取值只有一个含义，不许混读"
        description="ok=已对账且在阈值内；alarm=偏差超阈；error=本窗没拉完；blocked=预算用尽待补拉；skipped=没有结论（账本关闭/不可用/仍 blocked）。「两边都是 0」是 ok，不是 skipped。"
      />

      <div class="card">
        <div class="table-wrap">
          <el-table :data="items" size="small" border stripe empty-text="区间内没有对账窗口">
            <el-table-column label="窗口" min-width="150">
              <template #default="{ row }">
                <div class="cell-strong">{{ fmtDateTime(row.WindowStart) }}</div>
                <div class="sub">→ {{ fmtDateTime(row.WindowEnd) }}</div>
              </template>
            </el-table-column>
            <el-table-column label="状态" width="110">
              <template #default="{ row }"><StatusPill domain="reconcile" :value="row.Status" /></template>
            </el-table-column>
            <el-table-column label="连续告警" width="95" align="right">
              <template #default="{ row }">
                <span :class="row.AlarmStreak >= 3 ? 'neg' : ''">{{ row.AlarmStreak }}</span>
              </template>
            </el-table-column>
            <el-table-column label="笔数（本地 / 上游）" width="170" align="right">
              <template #default="{ row }">
                <span>{{ countText(row.LocalCount) }} / {{ countText(row.UpstreamCount) }}</span>
                <span v-if="row.LocalCount != null && row.UpstreamCount != null && row.LocalCount !== row.UpstreamCount" class="sub">
                  差 {{ row.LocalCount - row.UpstreamCount }}
                </span>
              </template>
            </el-table-column>
            <el-table-column label="Token（本地 / 上游）" width="200" align="right">
              <template #default="{ row }">
                {{ countText(row.LocalTokens) }} / {{ countText(row.UpstreamTokens) }}
              </template>
            </el-table-column>
            <el-table-column label="偏差率" width="110" align="right">
              <template #default="{ row }">
                <span :class="devClass(row.DeviationRate)">{{ devText(row.DeviationRate) }}</span>
              </template>
            </el-table-column>
            <el-table-column label="补拉进度" width="120" align="right">
              <template #default="{ row }">
                <span v-if="row.Status === 'blocked' || row.ScrollOffset > 0">已处理 {{ fmtCompact(row.ScrollOffset) }} 条</span>
                <span v-else class="sub">—</span>
              </template>
            </el-table-column>
            <el-table-column label="最后写入" width="150">
              <template #default="{ row }">{{ fmtDateTime(row.CheckedAt) }}</template>
            </el-table-column>
          </el-table>
        </div>
      </div>
    </template>

    <el-empty v-else-if="state === 'live'" description="区间内没有对账窗口" />
  </V2Page>
</template>

<style scoped>
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
.cell-strong {
  font-weight: 500;
}
.sub {
  display: block;
  font-size: 11px;
  color: var(--color-text-tertiary);
}
.neg {
  color: var(--color-danger);
  font-weight: 600;
}
.dev-warn {
  color: var(--color-warning);
  font-weight: 600;
}
.dev-bad {
  color: var(--color-danger);
  font-weight: 600;
}
.dev-none {
  color: var(--color-text-tertiary);
}

@media (max-width: 640px) {
  .card {
    padding: 10px;
  }
  :deep(.el-table) {
    font-size: 14px;
  }
}
</style>
