<script setup lang="ts">
// 会话成本路由面板（Step 18/19）：只读三端点 + 临界点图 / 分布 / 事件流。
// 所有在线值默认**只读**并标注来源标签（模型成本 / 真实 token / 当前时刻 / 实际分档）；
// What-if 开关打开后才允许手改，且仅在本地预览（不写回真实配置、不调用任何写接口）。
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Refresh, QuestionFilled } from '@element-plus/icons-vue'
import * as echarts from 'echarts/core'
import type { EChartsCoreOption } from 'echarts/core'
import { MarkLineComponent, MarkPointComponent } from 'echarts/components'
import VChart from '@/components/VChart.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'
import {
  getSessionCostConfig,
  getSessionCostEvents,
  getSessionCostDistribution,
  listExternalModels,
  type ExternalModel,
  type SessionCostConfigData,
  type SessionCostEventView,
  type SessionCostDistributionItem,
} from '@/api/admin'

// 临界点/分位标注所需的 ECharts 组件（VChart 未注册 markLine/markPoint，这里补充注册）。
echarts.use([MarkLineComponent, MarkPointComponent])

// ===== 面板状态 =====
const loading = ref(false)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const config = ref<SessionCostConfigData | null>(null)
const models = ref<ExternalModel[]>([])
const selectedModel = ref('')
const whatIf = ref(false)

const events = ref<SessionCostEventView[]>([])
const evFilter = ref('')
const evLimit = ref(100)

const distribution = ref<SessionCostDistributionItem[]>([])
const distIdx = ref(0)

// 可编辑候选（What-if 覆盖态）；只读时同步为真实值，保证展示与后端一致。
interface EditCandidate {
  ChannelID: string
  InternalModelID: string
  Input: number
  CacheRead: number
  Output: number
  TimeCoeff: number
  CtxCoeff: number
}
const editCands = ref<EditCandidate[]>([])
const editSample = reactive({ Input: 0, CacheRead: 0, Output: 0 })
const editK = ref(0)

// 本轮 token 三段（step）由后端 routing.cost_sample 提供默认样值
const realSample = computed(() => ({
  Input: Number(config.value?.CostSample?.Input) || 0,
  CacheRead: Number(config.value?.CostSample?.CacheRead) || 0,
  Output: Number(config.value?.CostSample?.Output) || 0,
}))

// 真实候选（来自模型成本，只读快照）
const realCands = computed<EditCandidate[]>(() =>
  (config.value?.Candidates || []).map((c) => ({
    ChannelID: c.ChannelID,
    InternalModelID: c.InternalModelID,
    Input: c.Cost.Input,
    CacheRead: c.Cost.CacheRead,
    Output: c.Cost.Output,
    TimeCoeff: c.TimeCoeff,
    CtxCoeff: c.CtxCoeff,
  })),
)

const gates = computed(() => config.value?.Gates || null)
const hasCandidates = computed(() => editCands.value.length > 0)

// 把真实值同步进可编辑态（加载后 / 关闭 What-if 时调用）
function syncEditsFromReal() {
  editCands.value = realCands.value.map((c) => ({ ...c }))
  const s = realSample.value
  editSample.Input = s.Input
  editSample.CacheRead = s.CacheRead
  editSample.Output = s.Output
}

// 经验式剩余轮数 = clamp(RemainingA − RemainingB×k, RemainingMin, RemainingMax)
function remainingTurns(k: number): number {
  const g = gates.value
  if (!g) return 0
  let n = Math.trunc(g.RemainingA - g.RemainingB * k)
  if (n < g.RemainingMin) n = g.RemainingMin
  if (n > g.RemainingMax) n = g.RemainingMax
  return n
}
const editRemaining = computed(() => remainingTurns(editK.value))

// ===== 前端成本公式（与后端 router 同源）=====
// perTurnCost = (缓存读×缓存读价 + 新输入×输入价 + 输出×输出价) × 时段系数 × 分档系数 / 1e6
function perTurnCost(c: EditCandidate, cacheRead: number, inTok: number, outTok: number): number {
  const raw = cacheRead * c.CacheRead + inTok * c.Input + outTok * c.Output
  return (raw * c.TimeCoeff * c.CtxCoeff) / 1e6
}
// crossCacheRead = (新输入×(cj.In−ci.In) + 输出×(cj.Out−ci.Out)) / (ci.CacheRead−cj.CacheRead)
function crossCacheRead(ci: EditCandidate, cj: EditCandidate, inTok: number, outTok: number): number | null {
  const den = ci.CacheRead - cj.CacheRead
  if (Math.abs(den) <= 1e-12) return null
  return (inTok * (cj.Input - ci.Input) + outTok * (cj.Output - ci.Output)) / den
}

// 交叉临界点（两两渠道曲线在给定新输入/输出下的交点）
interface CrossPoint {
  x: number
  y: number
  pair: string
}
const crossings = computed<CrossPoint[]>(() => {
  const cands = editCands.value
  const s = editSample
  const out: CrossPoint[] = []
  for (let i = 0; i < cands.length; i++) {
    for (let j = i + 1; j < cands.length; j++) {
      const x = crossCacheRead(cands[i], cands[j], s.Input, s.Output)
      if (x == null || !isFinite(x) || x < 0) continue
      out.push({
        x,
        y: perTurnCost(cands[i], x, s.Input, s.Output),
        pair: `${shortName(cands[i])} × ${shortName(cands[j])}`,
      })
    }
  }
  return out
})

// x 轴（缓存读 token）上界：覆盖样值 2 倍、临界点 1.3 倍与一个经验下限
const xMax = computed(() => {
  const s = editSample
  const vals = [s.CacheRead * 2, 20000, ...crossings.value.map((c) => c.x * 1.3)]
  return Math.max(...vals, 1)
})

function shortName(c: EditCandidate): string {
  const s = c.InternalModelID || c.ChannelID
  return s.length > 16 ? s.slice(0, 16) + '…' : s
}
function round6(v: number): number {
  return Math.round(v * 1e6) / 1e6
}
function fmtNum(v: number | null | undefined): string {
  if (v == null || !isFinite(v)) return '-'
  const a = Math.abs(v)
  if (a >= 1000) return v.toLocaleString('zh-CN', { maximumFractionDigits: 2 })
  return String(round6(v))
}
// 渠道迁移展示：优先渠道名；起点（keyID=0）显示「起点」；解析不到名时回落 ID。
function chLabel(keyID: string | undefined, name: string | undefined): string {
  if (name) return name
  if (!keyID || keyID === '0') return '起点'
  return keyID
}

const PALETTE = ['#5a5ce8', '#e6a23c', '#67c23a', '#f56c6c', '#36cfc9', '#b37feb', '#ff85c0', '#909399']

// 生成某组候选渠道的成本曲线序列（x = 缓存读 token，y = 单轮成本积分）
function curveSeries(
  cands: EditCandidate[],
  sample: { Input: number; CacheRead: number; Output: number },
  opts: { prefix: string; dashed: boolean; dim: boolean },
) {
  const steps = 40
  const maxX = xMax.value
  const xs: number[] = []
  for (let i = 0; i <= steps; i++) xs.push((maxX * i) / steps)
  return cands.map((c, idx) => ({
    name: opts.prefix + shortName(c),
    type: 'line' as const,
    showSymbol: false,
    smooth: false,
    lineStyle: {
      width: opts.dim ? 1.5 : 2.2,
      type: opts.dashed ? ('dashed' as const) : ('solid' as const),
      opacity: opts.dim ? 0.55 : 1,
    },
    itemStyle: { color: PALETTE[idx % PALETTE.length] },
    data: xs.map((x) => [round6(x), round6(perTurnCost(c, x, sample.Input, sample.Output))]),
  }))
}

const costCurveOption = computed<EChartsCoreOption>(() => {
  const sample = { Input: editSample.Input, CacheRead: editSample.CacheRead, Output: editSample.Output }
  const series: Record<string, unknown>[] = []
  if (whatIf.value) {
    // 假设值 vs 真实值：真实曲线（虚线灰）+ 假设曲线（实线彩）
    series.push(...curveSeries(realCands.value, sample, { prefix: '真实·', dashed: true, dim: true }))
    series.push(...curveSeries(editCands.value, sample, { prefix: '假设·', dashed: false, dim: false }))
  } else {
    series.push(...curveSeries(editCands.value, sample, { prefix: '', dashed: false, dim: false }))
  }
  // 交叉临界点标注（不连线，仅显示 pin 标记）
  series.push({
    name: '临界点',
    type: 'line',
    symbol: 'pin',
    symbolSize: 34,
    showSymbol: true,
    lineStyle: { width: 0, opacity: 0 },
    itemStyle: { color: '#e6a23c' },
    label: {
      show: true,
      position: 'top',
      fontSize: 10,
      color: '#8a5a00',
      formatter: (p: any) => (p?.data?.pair ? p.data.pair : '临界'),
    },
    data: crossings.value.map((c) => ({ value: [round6(c.x), round6(c.y)], pair: c.pair })),
    z: 10,
  })
  return {
    tooltip: {
      trigger: 'axis',
      valueFormatter: (v: unknown) => (typeof v === 'number' ? String(round6(v)) : String(v)),
    },
    legend: { type: 'scroll', top: 0 },
    grid: { left: 56, right: 24, top: 44, bottom: 40 },
    xAxis: {
      type: 'value',
      name: '缓存读 token',
      nameLocation: 'middle',
      nameGap: 26,
    },
    yAxis: { type: 'value', name: '单轮成本（积分）' },
    series,
  }
})

// ===== 临界缓存读分布（直方 + P10/中位/P90）=====
const distItem = computed<SessionCostDistributionItem | null>(
  () => distribution.value[distIdx.value] || null,
)
const distOption = computed<EChartsCoreOption>(() => {
  const item = distItem.value
  const bars = item ? item.Histogram.map((b) => [round6(b.UpperBound), b.Count]) : []
  const markData = item
    ? [
        { xAxis: round6(item.P10), name: 'P10', lineStyle: { color: '#67c23a' } },
        { xAxis: round6(item.Median), name: '中位', lineStyle: { color: '#e6a23c' } },
        { xAxis: round6(item.P90), name: 'P90', lineStyle: { color: '#f56c6c' } },
      ]
    : []
  return {
    tooltip: { trigger: 'axis' },
    grid: { left: 56, right: 24, top: 24, bottom: 40 },
    xAxis: { type: 'value', name: '临界缓存读 token', nameLocation: 'middle', nameGap: 26 },
    yAxis: { type: 'value', name: '计数' },
    series: [
      {
        type: 'bar',
        name: '分布',
        data: bars,
        barMaxWidth: 40,
        itemStyle: { color: '#5a5ce8' },
        markLine: {
          silent: true,
          symbol: 'none',
          label: { show: true, formatter: '{b}', fontSize: 11 },
          data: markData,
        },
      },
    ],
  }
})

// ===== 数据加载 =====
async function loadConfig() {
  const res = await getSessionCostConfig(selectedModel.value)
  config.value = res.Data
  syncEditsFromReal()
}
async function loadEvents() {
  const res = await getSessionCostEvents({
    SessionID: evFilter.value || undefined,
    Limit: evLimit.value || undefined,
  })
  events.value = res.Data.List || []
}
async function loadDistribution() {
  const res = await getSessionCostDistribution()
  distribution.value = res.Data.Sessions || []
  if (distIdx.value >= distribution.value.length) distIdx.value = 0
}
async function loadAll() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    await Promise.all([loadConfig(), loadEvents(), loadDistribution()])
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}

async function onModelChange() {
  try {
    await loadConfig()
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  }
}

async function onRefresh() {
  await loadAll()
  if (!errInfo.value.message) ElMessage.success('已刷新')
}

// What-if 开关：打开/关闭都回到真实值起点（仅本地预览，绝不写回）
function onWhatIfToggle(v: boolean) {
  syncEditsFromReal()
  if (v) ElMessage.info('已进入 What-if 覆盖：仅本地预览，不写回真实配置')
}

onMounted(async () => {
  try {
    const res = await listExternalModels({ Enabled: true })
    models.value = (res.Data || []).filter((m) => m.Enabled)
    if (models.value.length && !selectedModel.value) selectedModel.value = models.value[0].ExternalName
  } catch {
    /* 无可用模型时仍展示面板其余部分 */
  }
  await loadAll()
})
</script>

<template>
  <div class="page-container session-cost-page">
    <div class="toolbar">
      <span>对外模型</span>
      <el-select
        v-model="selectedModel"
        placeholder="选择对外模型以加载候选渠道"
        style="width: 260px"
        filterable
        @change="onModelChange"
      >
        <el-option v-for="m in models" :key="m.ID" :label="m.ExternalName" :value="m.ExternalName" />
      </el-select>
      <el-button :icon="Refresh" :loading="loading" @click="onRefresh">刷新</el-button>
      <div class="toolbar-spacer" />
      <span class="whatif-label">
        What-if 覆盖
        <el-tooltip
          content="打开后仅本地预览：可手改成本价/倍率/本轮 token/轮数，图上高亮「假设值 vs 真实值」。覆盖态不写回真实配置。"
          placement="top"
        >
          <el-icon class="hint-icon"><QuestionFilled /></el-icon>
        </el-tooltip>
      </span>
      <el-switch v-model="whatIf" @change="onWhatIfToggle" />
    </div>

    <el-alert
      v-if="whatIf"
      type="warning"
      show-icon
      :closable="false"
      title="What-if 覆盖态：以下改动仅本地预览，不会写回真实配置（不调用任何写接口）"
      class="whatif-alert"
    />

    <div v-loading="loading" class="panel-stack">
      <!-- 参数面板 -->
      <div class="card">
        <div class="block-title">
          参数面板
          <el-tag size="small" effect="plain" type="info">{{ whatIf ? 'What-if（本地覆盖）' : '在线值（只读）' }}</el-tag>
        </div>
        <div class="block-desc">
          在线值来源：渠道三档成本价<b>来自模型成本</b>、时段倍率<b>按当前时刻</b>、分档倍率<b>按实际分档</b>、本轮 token 三段<b>来自本轮 usage（routing.cost_sample 样值）</b>。
          每项右侧标签为后端返回的来源字段。
        </div>

        <!-- 本轮 token 三段 + 剩余轮数 -->
        <div class="param-grid">
          <div class="param-item">
            <span class="p-label">本轮输入 token</span>
            <el-input-number v-model="editSample.Input" :min="0" :step="1000" :disabled="!whatIf" controls-position="right" />
            <el-tag size="small" effect="plain" type="info">{{ config?.CostSampleSource || '-' }}</el-tag>
          </div>
          <div class="param-item">
            <span class="p-label">本轮缓存读 token</span>
            <el-input-number v-model="editSample.CacheRead" :min="0" :step="1000" :disabled="!whatIf" controls-position="right" />
            <el-tag size="small" effect="plain" type="info">{{ config?.CostSampleSource || '-' }}</el-tag>
          </div>
          <div class="param-item">
            <span class="p-label">本轮输出 token</span>
            <el-input-number v-model="editSample.Output" :min="0" :step="100" :disabled="!whatIf" controls-position="right" />
            <el-tag size="small" effect="plain" type="info">{{ config?.CostSampleSource || '-' }}</el-tag>
          </div>
          <div class="param-item">
            <span class="p-label">当前时刻倍率</span>
            <span class="mark-line mono">{{ fmtNum(config?.NowTimeCoeff) }}</span>
            <el-tag size="small" effect="plain" type="info">{{ config?.NowTimeCoeffSource || '-' }}</el-tag>
          </div>
          <div class="param-item">
            <span class="p-label">k（已进行轮数）</span>
            <el-input-number v-model="editK" :min="0" :step="1" :disabled="!whatIf" controls-position="right" />
            <el-tag size="small" effect="plain" type="info">{{ gates?.Source || '-' }}</el-tag>
          </div>
          <div class="param-item">
            <span class="p-label">剩余轮数（派生）</span>
            <span class="mark-line mono">{{ editRemaining }}</span>
            <el-tag size="small" effect="plain" type="info">clamp(A−B×k) · {{ gates?.Source || '-' }}</el-tag>
          </div>
        </div>

        <div class="gate-row">
          <span class="gate-item">剩余轮数 = clamp(A − B×k) = {{ fmtNum(gates?.RemainingA) }} − {{ fmtNum(gates?.RemainingB) }}×k（固定值，不预测）</span>
          <span class="gate-item">clamp 区间 [{{ gates?.RemainingMin ?? '-' }}, {{ gates?.RemainingMax ?? '-' }}]</span>
          <span class="gate-item">最小剩余轮数门槛 {{ gates?.MinRemaining ?? '-' }}</span>
          <span class="gate-item">冷却 {{ gates?.CooldownSec ?? '-' }}s</span>
          <span class="gate-item">滞回阈值 {{ fmtNum(gates?.MinImprove) }}</span>
          <span class="gate-item">影子观测 {{ config?.SessionCost?.shadow_enabled ? '开启' : '关闭' }}</span>
          <el-tag size="small" effect="plain" type="info">{{ gates?.Source || '-' }}</el-tag>
        </div>

        <!-- 候选渠道成本 / 时段 / 分档 -->
        <div v-if="hasCandidates" class="cand-table">
          <el-table :data="editCands" size="small" border class="table-nowrap">
            <el-table-column label="渠道 / 内部模型" min-width="180">
              <template #default="{ row }">
                <div class="mono">{{ shortName(row) }}</div>
                <div class="sub mono">{{ row.ChannelID }}</div>
              </template>
            </el-table-column>
            <el-table-column label="输入价" min-width="140" align="right">
              <template #default="{ row, $index }">
                <el-input-number v-model="row.Input" :min="0" :step="0.1" :disabled="!whatIf" size="small" controls-position="right" class="cell-num" />
                <el-tag size="small" effect="plain" type="info" class="src-tag">{{ config?.Candidates[$index]?.Cost.Source || '-' }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column label="缓存读价" min-width="140" align="right">
              <template #default="{ row, $index }">
                <el-input-number v-model="row.CacheRead" :min="0" :step="0.1" :disabled="!whatIf" size="small" controls-position="right" class="cell-num" />
                <el-tag size="small" effect="plain" type="info" class="src-tag">{{ config?.Candidates[$index]?.Cost.Source || '-' }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column label="输出价" min-width="140" align="right">
              <template #default="{ row, $index }">
                <el-input-number v-model="row.Output" :min="0" :step="0.1" :disabled="!whatIf" size="small" controls-position="right" class="cell-num" />
                <el-tag size="small" effect="plain" type="info" class="src-tag">{{ config?.Candidates[$index]?.Cost.Source || '-' }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column label="时段倍率" min-width="140" align="right">
              <template #default="{ row, $index }">
                <el-input-number v-model="row.TimeCoeff" :min="0" :step="0.1" :disabled="!whatIf" size="small" controls-position="right" class="cell-num" />
                <el-tag size="small" effect="plain" type="info" class="src-tag">{{ config?.Candidates[$index]?.TimeCoeffSource || '-' }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column label="分档倍率" min-width="140" align="right">
              <template #default="{ row, $index }">
                <el-input-number v-model="row.CtxCoeff" :min="0" :step="0.1" :disabled="!whatIf" size="small" controls-position="right" class="cell-num" />
                <el-tag size="small" effect="plain" type="info" class="src-tag">{{ config?.Candidates[$index]?.CtxCoeffSource || '-' }}</el-tag>
              </template>
            </el-table-column>
          </el-table>
        </div>
        <div v-else class="empty-hint">未选择对外模型或该模型下无候选渠道（请选择模型以加载候选渠道成本视图）。</div>

        <!-- 全局时段/分档（只读，来源=billing.time_config / billing.context_tiers；如需编辑请前往系统页） -->
        <div class="global-block">
          <div class="global-head">
            <span class="global-title">全局时段 / 分档（只读）</span>
            <el-tag size="small" effect="plain" type="info">{{ config?.TimeConfigSource || '-' }}</el-tag>
            <el-tag size="small" effect="plain" type="info">{{ config?.ContextTiersSource || '-' }}</el-tag>
            <router-link class="global-link" to="/admin/system">
              <el-button link type="primary" size="small">前往「系统」维护时段 / 分档</el-button>
            </router-link>
          </div>
          <pre class="raw-pre mono">{{ { TimeConfig: config?.TimeConfig ?? null, ContextTiers: config?.ContextTiers ?? null } }}</pre>
        </div>

        <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
      </div>

      <!-- 临界点图 -->
      <div class="card">
        <div class="block-title">
          临界点图
          <el-tag size="small" effect="plain">随「缓存读 token」变化的多渠道路由成本曲线</el-tag>
          <el-tag v-if="whatIf" size="small" type="warning" effect="plain">假设值（实线） vs 真实值（虚线）</el-tag>
        </div>
        <div class="block-desc">
          y = 单轮成本（积分）= (缓存读×缓存读价 + 新输入×输入价 + 输出×输出价) × 时段系数 × 分档系数 ÷ 1e6；
          曲线两两交叉处即临界缓存读（crossCacheRead），以 pin 标注渠道对。
        </div>
        <VChart v-if="hasCandidates" :option="costCurveOption" height="360px" />
        <div v-else class="empty-hint">暂无候选渠道，无法绘制成本曲线。</div>
        <div v-if="crossings.length" class="cross-list">
          <span class="cross-title">临界点（缓存读 token）：</span>
          <el-tag v-for="(c, i) in crossings" :key="i" size="small" type="warning" effect="plain" class="cross-tag">
            {{ c.pair }} ≈ {{ fmtNum(c.x) }}
          </el-tag>
        </div>
      </div>

      <!-- 临界缓存读分布 -->
      <div class="card">
        <div class="block-title">
          临界缓存读分布
          <el-tag size="small" effect="plain" type="info">{{ distItem?.QuantilesSource || '-' }}</el-tag>
        </div>
        <div class="block-desc">按会话聚合事件流中的临界缓存读（crossCacheRead），显示直方与 P10 / 中位 / P90 分位。</div>
        <div v-if="distribution.length" class="dist-head">
          <span>会话</span>
          <el-select v-model="distIdx" style="width: 320px" filterable>
            <el-option
              v-for="(s, i) in distribution"
              :key="s.SessionID"
              :label="`${s.SessionID}（${s.Count} 条）`"
              :value="i"
            />
          </el-select>
          <template v-if="distItem">
            <el-tag size="small" effect="plain" type="success">P10 {{ fmtNum(distItem.P10) }}</el-tag>
            <el-tag size="small" effect="plain" type="warning">中位 {{ fmtNum(distItem.Median) }}</el-tag>
            <el-tag size="small" effect="plain" type="danger">P90 {{ fmtNum(distItem.P90) }}</el-tag>
            <span class="dist-count">样本 {{ distItem.Count }}</span>
          </template>
        </div>
        <VChart v-if="distItem" :option="distOption" height="300px" />
        <div v-else class="empty-hint">暂无临界缓存读样本（无 CrossCacheReadKnown=true 的事件）。</div>
      </div>

      <!-- 切换事件流 -->
      <div class="card">
        <div class="block-title">
          切换事件流
          <el-tag size="small" effect="plain" type="info">asyncwrite KindSessionCostEvent</el-tag>
        </div>
        <div class="toolbar">
          <span>会话 ID</span>
          <el-input v-model="evFilter" placeholder="按 SessionID 过滤（留空=全部）" style="width: 280px" clearable @keyup.enter="loadEvents" />
          <span>最近条数</span>
          <el-input-number v-model="evLimit" :min="1" :max="1000" :step="50" controls-position="right" />
          <el-button @click="loadEvents">查询</el-button>
          <span class="page-count">共 {{ events.length }} 条</span>
        </div>
        <el-table :data="events" size="small" border class="table-nowrap">
          <el-table-column prop="SessionID" label="会话" min-width="160" show-overflow-tooltip />
          <el-table-column label="轮次" width="72" align="center">
            <template #default="{ row }">#{{ row.Turn }}</template>
          </el-table-column>
          <el-table-column label="类型" width="92" align="center">
            <template #default="{ row }">
              <el-tag size="small" :type="row.Shadow ? 'warning' : 'primary'" effect="plain">
                {{ row.Shadow ? '影子假设' : '实际决策' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="切换" width="80" align="center">
            <template #default="{ row }">
              <el-tag size="small" :type="row.Switch ? 'success' : 'info'" effect="plain">{{ row.Switch ? '切换' : '保持' }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="渠道迁移" min-width="200">
            <template #default="{ row }">
              <span class="mono">{{ chLabel(row.FromChannelKeyID, row.FromChannelName) }} → {{ chLabel(row.ToChannelKeyID, row.ToChannelName) }}</span>
            </template>
          </el-table-column>
          <el-table-column label="本轮成本（旧→新）" min-width="170" align="right">
            <template #default="{ row }">
              <span class="mono">{{ fmtNum(row.TurnCostOld) }} → {{ fmtNum(row.TurnCostNew) }}</span>
            </template>
          </el-table-column>
          <el-table-column label="重灌代价" width="110" align="right">
            <template #default="{ row }"><span class="mono">{{ fmtNum(row.RechargeCost) }}</span></template>
          </el-table-column>
          <el-table-column label="回本轮数" width="110" align="right">
            <template #default="{ row }">
              <span v-if="row.PaybackTurns == null" class="muted">永不回本</span>
              <span v-else class="mono">{{ fmtNum(row.PaybackTurns) }}</span>
            </template>
          </el-table-column>
          <el-table-column label="剩余轮数" width="90" align="right">
            <template #default="{ row }"><span class="mono">{{ row.RemainingTurns }}</span></template>
          </el-table-column>
          <el-table-column label="临界缓存读" min-width="150" align="right">
            <template #default="{ row }">
              <span v-if="row.CrossCacheReadKnown" class="mono">{{ fmtNum(row.CrossCacheRead) }}</span>
              <span v-else class="muted">未知</span>
            </template>
          </el-table-column>
          <el-table-column label="原因" min-width="220" show-overflow-tooltip>
            <template #default="{ row }">{{ row.Reason || '-' }}</template>
          </el-table-column>
        </el-table>
      </div>
    </div>
  </div>
</template>

<style scoped>
.session-cost-page {
  display: flex;
  flex-direction: column;
}
.panel-stack {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.block-desc {
  color: var(--color-text-secondary);
  font-size: 13px;
  margin-bottom: 14px;
  line-height: 1.6;
}
.whatif-label {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 13px;
  color: var(--color-text-secondary);
}
.whatif-alert {
  margin-bottom: 12px;
}
.hint-icon {
  cursor: pointer;
  color: var(--color-text-secondary);
}
.param-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 14px 24px;
  margin-bottom: 14px;
}
@media (max-width: 1100px) {
  .param-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}
.param-item {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.p-label {
  color: var(--color-text-secondary);
  font-size: 13px;
}
.mark-line {
  font-size: 15px;
  font-weight: 600;
  color: var(--brand-primary);
  padding: 2px 0;
}
.gate-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 12px;
  padding: 10px 12px;
  background: var(--brand-primary-soft);
  border-radius: 8px;
  font-size: 12px;
  color: var(--color-text-secondary);
  margin-bottom: 14px;
}
.gate-item {
  white-space: nowrap;
}
.cand-table {
  margin-bottom: 16px;
}
.cell-num {
  width: 100%;
  max-width: 130px;
}
.src-tag {
  margin-top: 4px;
}
.sub {
  color: var(--color-text-secondary);
  font-size: 11px;
}
.global-block {
  border-top: 1px dashed var(--color-border);
  padding-top: 12px;
}
.global-head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 8px;
}
.global-title {
  font-size: 14px;
  font-weight: 600;
}
.global-link {
  text-decoration: none;
}
.raw-pre {
  margin: 0;
  max-height: 220px;
  overflow: auto;
  padding: 10px 12px;
  border-radius: 6px;
  background: #101828;
  color: #d0d5dd;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
}
.empty-hint {
  font-size: 13px;
  color: var(--color-text-tertiary);
  padding: 8px 4px;
}
.cross-list {
  margin-top: 12px;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.cross-title {
  font-size: 13px;
  color: var(--color-text-secondary);
}
.cross-tag {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
}
.dist-head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
  margin-bottom: 10px;
  font-size: 13px;
  color: var(--color-text-secondary);
}
.dist-count {
  font-size: 12px;
}
.mono {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
}
.muted {
  color: var(--color-text-tertiary);
}
</style>