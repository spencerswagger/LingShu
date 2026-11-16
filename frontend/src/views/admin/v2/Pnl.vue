<script setup lang="ts">
// 盈亏（P&L）：收入 − 成本 = 毛利，按供应商/账号/模型分组。
//
// 口径（后端 console/ops.go 已定型，前端只做展示，不重算）：
//   收入 = Σ credits_consumed（用户售价口径）
//   成本 = Σ cost_credits，按 cost_source 拆「账本实付（变动）+ 订阅摊销（固定）」
//   毛利 = 收入 − 成本；收入 ≤ 0 时毛利率返回 null（后端刻意不返回 0，前端不许显示成 0%）
//   Uncosted = 成本为 NULL 的请求数：> 0 表示这批账单还没有成本口径，毛利偏高 → 必须提示
//   Unconfigured / NoneCost 是它的两段拆分，后端可能不返回 → 页面显式「未测算」，**不许渲染成 0**；
//   两段只解释构成，不参与收入/成本/毛利的任何加总（合计只认后端返回的 Revenue/Cost/Margin）。
import { computed, onMounted, ref } from 'vue'
import { getPnl, type PnlGroupBy, type PnlView } from '@/api/v2'
import { mockPnl } from '@/mock/pnl'
import { useV2Load } from '@/composables/useV2Load'
import V2Page from '@/components/v2/V2Page.vue'
import StatCards from '@/components/v2/StatCards.vue'
import type { StatItem, V2Dep } from '@/components/v2/types'
import VChart from '@/components/VChart.vue'
import { fmtCompact } from '@/utils/format'

const RANGES = [
  { label: '最近 24 小时', hours: 24 },
  { label: '最近 7 天', hours: 24 * 7 },
  { label: '最近 30 天', hours: 24 * 30 },
]

const hours = ref(24)
const groupBy = ref<PnlGroupBy>('provider')

function rangeParams() {
  const to = new Date()
  const from = new Date(to.getTime() - hours.value * 3600_000)
  return { From: from.toISOString(), To: to.toISOString(), GroupBy: groupBy.value }
}

const { state, data, error, load, showDemo, exitDemo } = useV2Load<PnlView>(
  () => getPnl(rangeParams()),
  () => mockPnl(groupBy.value),
)

function reload() {
  void load(() => getPnl(rangeParams()))
}

onMounted(() => void load())

// 金额：积分制 5 位小数，展示最多 5 位、最少 2 位（0.5 显示 0.50，0.12345 原样）
function money(v: number | null): string {
  if (v == null) return '—'
  return v.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 5 })
}
function pct(v: number | null): string {
  return v == null ? '—' : `${(v * 100).toFixed(2)}%`
}
// 缺失值计数：后端不返回该拆分时显式「未测算」，绝不渲染成 0（0 笔 ≠ 未测算）
function count(v: number | null): string {
  return v == null ? '未测算' : fmtCompact(v)
}
// 拆分明细的提示语：任一段缺失就明确说「未测算」，不参与加总（两者只是 Uncosted 的构成说明，不进毛利计算）
function uncostedHint(d: PnlView): string {
  if (d.Uncosted <= 0) return '全部请求都有成本口径'
  const parts: string[] = []
  parts.push(d.Unconfigured == null ? '「历史未配置」未测算' : `「历史未配置」${count(d.Unconfigured)} 笔（cost_source IS NULL）`)
  parts.push(d.NoneCost == null ? '「明确无口径」未测算' : `「明确无口径」${count(d.NoneCost)} 笔（none）`)
  const missing = d.Unconfigured == null || d.NoneCost == null
  return `${parts.join('、')}${missing ? '；缺失项未计入任何合计' : ''} → 毛利虚高`
}

// 依赖徽标：拿到真实数据才标「已定型」，避免接口没上线时给出错误预期
const deps = computed<V2Dep[]>(() => [
  { path: 'GET /admin/ops/pnl', state: state.value === 'live' ? 'ready' : 'pending' },
  // O2 趋势（收入/成本/毛利时间序列）是 P0 但尚未上线：趋势块先出空态，不自造一条前端折线
  { path: 'GET /admin/ops/pnl/trend', state: 'pending' },
])

// 变动成本 / 固定成本：从分组行聚合（后端汇总块只给 Cost 合计，M5 要求两段可核对）
const ledgerCost = computed(() => Math.round((data.value?.Groups || []).reduce((s, g) => s + g.LedgerCost, 0) * 1e5) / 1e5)
const amortizedCost = computed(
  () => Math.round((data.value?.Groups || []).reduce((s, g) => s + g.AmortizedCost, 0) * 1e5) / 1e5,
)

// 恒等式自检（Spec P07 验收 1/3）：收入 − 成本 = 毛利；变动 + 固定 = 成本。
// 分币级容差 1e-4（5 位小数四舍五入）。不成立就红字点出来，别让运营拿错的数去汇报。
const EPS = 1e-4
const identity = computed(() => {
  const d = data.value
  if (!d) return null
  const marginOk = Math.abs(d.Revenue - d.Cost - d.Margin) <= EPS
  const splitOk = Math.abs(ledgerCost.value + amortizedCost.value - d.Cost) <= EPS
  return {
    marginOk,
    splitOk,
    ok: marginOk && splitOk,
    marginDelta: Math.round((d.Revenue - d.Cost - d.Margin) * 1e5) / 1e5,
    splitDelta: Math.round((ledgerCost.value + amortizedCost.value - d.Cost) * 1e5) / 1e5,
  }
})

const stats = computed<StatItem[]>(() => {
  const d = data.value
  if (!d) return []
  const uncostedTone = d.Uncosted > 0 ? 'warn' : 'default'
  return [
    { label: '收入（售价口径）', value: money(d.Revenue), hint: 'Σ credits_consumed' },
    {
      label: '成本（账本 + 摊销）',
      value: money(d.Cost),
      hint: '账本实付 + 订阅摊销，不含「无成本口径」的账单',
    },
    {
      label: '毛利',
      value: money(d.Margin),
      tone: d.Margin < 0 ? 'danger' : 'ok',
      hint: '收入 − 成本',
    },
    {
      label: '毛利率',
      value: pct(d.MarginPct),
      hint: d.MarginPct == null ? '收入 ≤ 0，后端不返回毛利率' : '毛利 ÷ 收入',
    },
    {
      label: '变动成本（账本实付）',
      value: money(ledgerCost.value),
      hint: 'cost_source=ledger，随流量变化',
    },
    {
      label: '固定成本（订阅摊销）',
      value: money(amortizedCost.value),
      hint: 'cost_source=amortized，A5 月费行级分摊',
    },
    { label: '请求数', value: fmtCompact(d.Requests) },
    { label: 'Token（三段合计）', value: fmtCompact(d.Tokens) },
    {
      label: '无成本口径请求',
      value: fmtCompact(d.Uncosted),
      tone: uncostedTone,
      hint: uncostedHint(d),
    },
  ]
})

// 柱状图：各分组收入 vs 成本。分组多的只画前 12 组（表里看全量）。
const chartOption = computed(() => {
  const groups = (data.value?.Groups || []).slice(0, 12)
  return {
    tooltip: { trigger: 'axis' },
    legend: { data: ['收入', '成本', '毛利'], right: 8, top: 0 },
    grid: { left: 8, right: 16, top: 32, bottom: 8, containLabel: true },
    xAxis: { type: 'category', data: groups.map((g) => g.Key), axisLabel: { hideOverlap: true } },
    yAxis: { type: 'value' },
    series: [
      { name: '收入', type: 'bar', data: groups.map((g) => g.Revenue), itemStyle: { color: '#5a5ce8' } },
      { name: '成本', type: 'bar', data: groups.map((g) => g.Cost), itemStyle: { color: '#f79009' } },
      { name: '毛利', type: 'bar', data: groups.map((g) => g.Margin), itemStyle: { color: '#12b76a' } },
    ],
  }
})
</script>

<template>
  <V2Page
    title="盈亏"
    intro="按供应商 / 账号 / 模型看「收入 − 成本」。成本拆成账本实付（变动）与订阅摊销（固定）两段；无成本口径的请求单独计数，它会让毛利偏高。"
    source="backend/internal/domain/console/ops.go（GET /admin/ops/pnl）+ 数据层契约 §9（cost_source）"
    :deps="deps"
    :state="state"
    :error="error"
    pending-hint="盈亏接口在阶段二 A6 分支定型（形状已冻结），合入 v2 后本页自动取真实数据。"
    @reload="reload"
    @demo="showDemo"
    @exit-demo="exitDemo"
  >
    <template #actions>
      <el-select v-model="hours" size="small" style="width: 140px" @change="reload">
        <el-option v-for="r in RANGES" :key="r.hours" :label="r.label" :value="r.hours" />
      </el-select>
      <el-select v-model="groupBy" size="small" style="width: 120px" @change="reload">
        <el-option label="按供应商" value="provider" />
        <el-option label="按账号" value="account" />
        <el-option label="按模型" value="model" />
      </el-select>
    </template>

    <template v-if="data">
      <StatCards :items="stats" :columns="4" />

      <!-- 恒等式自检：不成立就红字点出来（Spec P07 验收 1/3） -->
      <div v-if="identity" class="identity" :class="identity.ok ? 'is-ok' : 'is-bad'">
        <span>
          收入 − 成本 = 毛利：<strong>{{ identity.marginOk ? '成立' : `不成立（差 ${identity.marginDelta}）` }}</strong>
        </span>
        <span>
          变动 + 固定 = 成本：<strong>{{ identity.splitOk ? '成立' : `不成立（差 ${identity.splitDelta}）` }}</strong>
        </span>
        <span class="sub">分币级容差 1e-4；不成立说明后端聚合有偏差，先报 Core，不要把这张表发出去。</span>
      </div>

      <el-alert
        v-if="data.Uncosted > 0"
        type="warning"
        :closable="false"
        show-icon
        title="毛利偏高：存在没有成本口径的请求"
        :description="`${fmtCompact(data.Uncosted)} 笔请求的 cost_credits 为空（历史未配置 ${count(data.Unconfigured)} / 明确无口径 ${count(data.NoneCost)}）。这些请求只计收入、不计成本，算出来的毛利不能当真实毛利。点右侧按钮按「成本来源=未配置」筛账单。`"
      >
        <template #default>
          <el-button size="small" @click="$router.push('/admin/billings?cost_source=unconfigured')">
            去看这批账单
          </el-button>
        </template>
      </el-alert>
      <el-alert
        v-if="data.Truncated"
        type="info"
        :closable="false"
        show-icon
        title="分组已截断"
        description="分组数超过后端上限（200），当前只返回前 200 组：请缩小时间区间或换分组维度。"
      />

      <div class="card">
        <div class="card-title">收入 / 成本 / 毛利（前 12 个分组）</div>
        <VChart :option="chartOption" height="280px" />
      </div>

      <div class="card">
        <div class="card-title">趋势（按小时 / 按天）</div>
        <!-- O2 未就绪：不拿分组数据折一条假趋势线（Spec §5 规则 4：不许用 mock/假数据通过验收） -->
        <el-empty :image-size="54" description="趋势接口（O2 /admin/ops/pnl/trend）待 Core 上线；接口就绪后此块显示收入/成本/毛利三条线，可切小时/天粒度。" />
      </div>

      <div class="card">
        <div class="card-title">分组明细（{{ data.Groups.length }} 组）</div>
        <div class="table-wrap">
          <el-table :data="data.Groups" size="small" border stripe empty-text="区间内没有账单">
            <el-table-column prop="Key" label="分组" min-width="180" show-overflow-tooltip />
            <el-table-column label="请求数" width="100" align="right">
              <template #default="{ row }">{{ fmtCompact(row.Requests) }}</template>
            </el-table-column>
            <el-table-column label="Token" width="110" align="right">
              <template #default="{ row }">{{ fmtCompact(row.Tokens) }}</template>
            </el-table-column>
            <el-table-column label="收入" width="130" align="right" sortable prop="Revenue">
              <template #default="{ row }">{{ money(row.Revenue) }}</template>
            </el-table-column>
            <el-table-column label="成本" width="190" align="right">
              <template #default="{ row }">
                <span>{{ money(row.Cost) }}</span>
                <span class="sub">账本 {{ money(row.LedgerCost) }} / 摊销 {{ money(row.AmortizedCost) }}</span>
              </template>
            </el-table-column>
            <el-table-column label="毛利" width="130" align="right" sortable prop="Margin">
              <template #default="{ row }">
                <span :class="row.Margin < 0 ? 'neg' : ''">{{ money(row.Margin) }}</span>
              </template>
            </el-table-column>
            <el-table-column label="毛利率" width="110" align="right">
              <template #default="{ row }">{{ pct(row.MarginPct) }}</template>
            </el-table-column>
            <el-table-column label="无成本口径" width="120" align="right">
              <template #default="{ row }">
                <el-tag v-if="row.Uncosted > 0" type="warning" size="small" effect="plain">
                  {{ fmtCompact(row.Uncosted) }}
                </el-tag>
                <span v-else class="sub">0</span>
              </template>
            </el-table-column>
          </el-table>
        </div>
      </div>
    </template>

    <el-empty v-else-if="state === 'live'" description="区间内没有账单" />
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
.card-title {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text);
  margin-bottom: 8px;
}
.table-wrap {
  overflow-x: auto;
}
.sub {
  display: block;
  font-size: 11px;
  color: var(--color-text-tertiary);
}
.identity {
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
  padding: 8px 12px;
  border-radius: var(--radius-s);
  font-size: 13px;
  border: 1px solid var(--color-border);
  background: #fafbfd;
}
.identity.is-ok {
  border-color: #b7ebc6;
  background: #f2fcf5;
}
.identity.is-bad {
  border-color: #fecdca;
  background: #fef3f2;
  color: #b42318;
}
.neg {
  color: var(--color-danger);
  font-weight: 600;
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
