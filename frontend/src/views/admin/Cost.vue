<script setup lang="ts">
// 「成本」只读页（仅管理端），三块能力：
//   A. 成本统计表：按「当前实体」（渠道 × 内部模型 × 对外模型）聚合，row-key = EntityKey；
//   B. 模型定价曲线：新接口 /admin/cost/pricing-curve，后端按真实公式逐 token 出点，前端只渲染
//      （成本线 = 渠道内部模型配置、售价线 = 对外模型配置；分档拐点画竖线、超档断线标红）；
//      B+ 多线对比（受开关 console.cost_multichannel_trace 控制，默认关闭）：同图多条线、逐线显隐；
//   C. 会话成本曲线：逐轮历史实际 cost_credits / credits_consumed（切换会话有请求序号守卫）；
//      C+ 会话渠道轨迹（同一开关）：逐轮渠道 + 连续段 + 真实切换点 + 预测打平点。
// 纯只读：全部走 GET，无任何写路径（开关写入在「计费配置」页）。桌面用表格、窄屏用「数字墙 + 实体行」单列表。
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Refresh } from '@element-plus/icons-vue'
import * as echarts from 'echarts/core'
import type { EChartsCoreOption } from 'echarts/core'
import { MarkAreaComponent, MarkLineComponent, MarkPointComponent } from 'echarts/components'
import VChart from '@/components/VChart.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'
import {
  getCostStats,
  getCostPricingCurve,
  getCostSession,
  getCostSessionTrace,
  getSystemConfig,
  listSessions,
  type AdminSession,
  type CostPricingCurveData,
  type CostPricingCurveParams,
  type CostPricingLine,
  type CostPricingMultiLine,
  type CostStatRow,
  type CostStatTotal,
  type CostTraceBreakEven,
  type CostTraceData,
  type CostTraceSegment,
  type CostTraceSwitch,
  type SessionPoint,
} from '@/api/admin'

// 拐点竖线（markLine）、超档红标（markPoint）、超档区间底色（markArea）按需注册。
echarts.use([MarkLineComponent, MarkPointComponent, MarkAreaComponent])

// ===== 通用格式化 =====
const fmt2 = (v: number) => (Number.isFinite(v) ? v.toFixed(2) : '-')
const fmtPct = (v: number) => (Number.isFinite(v) ? (v * 100).toFixed(2) + '%' : '-')
const fmtInt = (v: number) => (Number.isFinite(v) ? v.toLocaleString('zh-CN') : '-')
const round4 = (v: number) => Math.round(v * 10000) / 10000

// 窄屏（移动版）判定：图表高度、布局切换用。
const NARROW_QUERY = '(max-width: 768px)'
const isNarrow = ref(false)
let mq: MediaQueryList | undefined
function onMqChange(e: MediaQueryListEvent | MediaQueryList) {
  isNarrow.value = e.matches
}

// ===== 全局错误 =====
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
function setErr(e: any) {
  errInfo.value = { message: e?.message || '', requestId: e?.requestId || '' }
}

// ===================================================================
// 0. 增强视图开关（console.cost_multichannel_trace，默认 false）
// ===================================================================
// 一个开关同时管两个**只读**入口：定价曲线多线对比（ChannelModelIDs）与会话渠道轨迹（/trace）。
// 关闭时这两个入口返回 403 —— 本页先读开关决定是否展示新入口；万一读不到（或读取后又被人关掉）
// 而收到 403，走「功能未开启」提示，不弹红色报错、也不留空白。
const featEnabled = ref(false)
const featBlocked = ref('') // 403 时后端文案（开关未开启），用于行内引导
const featLoaded = ref(false)

async function loadFeatureSwitch() {
  try {
    const res = await getSystemConfig()
    featEnabled.value = !!res.Data?.Console?.MultichannelTrace
  } catch {
    // 读不到配置时按关闭处理：页面回到改造前的表现，不影响旧的统计/曲线/会话功能。
    featEnabled.value = false
  } finally {
    featLoaded.value = true
  }
}

// 403 = 能力未启用（不是参数错误）：不再弹红条，改为行内空态 + 引导。
function onFeatureForbidden(e: any): boolean {
  if (e?.status !== 403) return false
  featEnabled.value = false
  featBlocked.value = e?.message || '该功能未开启（console.cost_multichannel_trace）'
  return true
}

// ===================================================================
// A. 成本统计表（按实体归一）
// ===================================================================
const loadingStats = ref(false)
const statsRows = ref<CostStatRow[]>([])
const statsTotal = ref<CostStatTotal | null>(null)

async function loadStats() {
  loadingStats.value = true
  try {
    const res = await getCostStats()
    statsRows.value = res.Data.Rows ?? []
    statsTotal.value = res.Data.Total ?? null
  } catch (e: any) {
    setErr(e)
  } finally {
    loadingStats.value = false
  }
}

// 曾用名：SnapshotNames 去掉当前展示名后剩下的历史文本（长度 >1 才标注）。
function oldNames(row: CostStatRow): string[] {
  const all = row.SnapshotNames ?? []
  return all.filter((n) => n && n !== row.ExternalModelName)
}
function oldNamesTitle(row: CostStatRow): string {
  const list = oldNames(row)
  return list.length ? `曾用名：${list.join(' / ')}` : ''
}

// 表格单元格格式化（保留原始数值用于排序）
const cellCost = (_r: CostStatRow, _c: unknown, v: number) => fmt2(v)
const cellInt = (_r: CostStatRow, _c: unknown, v: number) => fmtInt(v)
const cellHit = (_r: CostStatRow, _c: unknown, v: number) => fmtPct(v)
const cellRatio = (_r: CostStatRow, _c: unknown, v: number) => fmt2(v)

// 汇总行取后端 Total（全量口径，不随排序变化）。
function sumSummary({ columns }: { columns: { property?: string }[] }): string[] {
  const t = statsTotal.value
  return columns.map((col, i) => {
    if (i === 0) return '合计'
    if (!t) return ''
    switch (col.property) {
      case 'CostCredits':
        return fmt2(t.CostCredits)
      case 'SaleCredits':
        return fmt2(t.SaleCredits)
      case 'CacheHitRate':
        return fmtPct(t.CacheHitRate)
      case 'InputOutputRatio':
        return fmt2(t.InputOutputRatio)
      case 'Requests':
        return fmtInt(t.Requests)
      case 'InputTokens':
        return fmtInt(t.InputTokens)
      case 'CacheReadTokens':
        return fmtInt(t.CacheReadTokens)
      case 'OutputTokens':
        return fmtInt(t.OutputTokens)
      default:
        return ''
    }
  })
}

const rowKey = (row: CostStatRow) => row.EntityKey

// ===================================================================
// B. 模型定价曲线（/admin/cost/pricing-curve）
// ===================================================================
const loadingCurve = ref(false)
const curve = ref<CostPricingCurveData | null>(null)
const curveEntity = ref('') // 选中的 EntityKey（模型来自统计表的实体行）
// 采样参数：首次响应回填后端生效值，之后用户改动才带参重取。
const curveParams = reactive({ step: 0, maxTokens: 0, hitRate: 0.9, inOutRatio: 10 })
const paramsInited = ref(false)
// 命中率按百分比展示（0~100），内部仍存 0~1。
const hitRatePct = computed<number>({
  get: () => round2(curveParams.hitRate * 100),
  set: (v: number) => {
    curveParams.hitRate = (Number.isFinite(v) ? v : 0) / 100
  },
})
function round2(v: number) {
  return Math.round(v * 100) / 100
}
const useCoeff = ref(true) // 开 = Credits（含时段×分档系数）；关 = CreditsNoCoeff
const curveMode = ref<'both' | 'cost' | 'sale'>('both')
const timeOptionKey = ref('') // 选中的时段（Name|Start|End）
const timeTouched = ref(false) // 用户是否手动选过时段

const curveEntities = computed<CostStatRow[]>(() => statsRows.value)
const currentEntity = computed(() => curveEntities.value.find((r) => r.EntityKey === curveEntity.value) || null)

const timeOptions = computed(() => curve.value?.TimeOptions ?? [])
function timeOptionLabel(t: { Name: string; Start: string; End: string; Coeff: number }) {
  const span = t.Start && t.End ? `${t.Start}–${t.End}` : ''
  return `${t.Name}${span ? ' ' + span : ''} ×${t.Coeff}`
}
function timeOptionValue(t: { Name: string; Start: string; End: string }) {
  return `${t.Name}|${t.Start}|${t.End}`
}
const currentTimeOption = computed(() =>
  timeOptions.value.find((t) => timeOptionValue(t) === timeOptionKey.value) || null,
)

// 曲线请求序号：参数/模型快速切换时丢弃过期响应。
let curveSeq = 0

// 采样参数里「用户手动改过」的字段：只有改过的才回传，未改的一律交给后端默认值。
// 这样多线模式后端按线数自动下调 MaxTokens 时，不会把缩小后的值再喂回去（越切越小）。
const manual = reactive({ step: false, maxTokens: false, hitRate: false, inOut: false })

// ===== 多线对比（受开关控制）=====
// multiMode       是否打开多线对比（关闭时页面行为与改造前完全一致）
// multiSelected   选中的渠道内部模型 ID（顺序即图例顺序，最多 10 个）
// multiHidden     逐线显隐：被隐藏的 ChannelModelID
const multiMode = ref(false)
const multiSelected = ref<number[]>([])
const multiHidden = ref<number[]>([])
const MULTI_LIMIT = 10

// 可选实体：成本统计里的实体行（同一 ChannelModelID 只出现一次；没有渠道内部模型的实体不可选）
const multiOptions = computed<CostStatRow[]>(() => {
  const seen = new Set<number>()
  const out: CostStatRow[] = []
  for (const r of statsRows.value) {
    if (!r.ChannelModelID || seen.has(r.ChannelModelID)) continue
    seen.add(r.ChannelModelID)
    out.push(r)
  }
  return out
})

function onMultiModeChange() {
  multiHidden.value = []
  loadCurve()
}
function onMultiSelectChange() {
  multiHidden.value = multiHidden.value.filter((id) => multiSelected.value.includes(id))
  loadCurve()
}
function toggleMultiLine(id: number) {
  multiHidden.value = multiHidden.value.includes(id)
    ? multiHidden.value.filter((v) => v !== id)
    : [...multiHidden.value, id]
}

async function loadCurve() {
  const seq = ++curveSeq
  loadingCurve.value = true
  try {
    const row = currentEntity.value
    const p: CostPricingCurveParams = {}
    // 多线模式：只传 ChannelModelIDs（顶层 Cost/Sale 会是「未指定」的不可用线，多线渲染不读它）；
    // 没开多线、或一个都没选时，走改造前的单线参数。
    const multi = featEnabled.value && multiMode.value && multiSelected.value.length > 0
    if (multi) {
      p.ChannelModelIDs = multiSelected.value.slice(0, MULTI_LIMIT).join(',')
    } else {
      if (row?.ChannelModelID) p.ChannelModelID = row.ChannelModelID
      if (row?.ExternalModelID) p.ExternalModelID = row.ExternalModelID
      else if (row?.ExternalModelName) p.ExternalModelName = row.ExternalModelName
    }
    if (paramsInited.value) {
      if (manual.step && curveParams.step > 0) p.Step = curveParams.step
      if (manual.maxTokens && curveParams.maxTokens > 0) p.MaxTokens = curveParams.maxTokens
      if (manual.hitRate) p.HitRate = curveParams.hitRate
      if (manual.inOut) p.InOutRatio = curveParams.inOutRatio
    }
    // 时段：用所选时段的起点作为 At，让后端按该时段算 timeCoeff。
    if (timeTouched.value && currentTimeOption.value?.Start) p.At = currentTimeOption.value.Start

    const res = await getCostPricingCurve(p, { silentStatuses: [403] })
    if (seq !== curveSeq) return
    curve.value = res.Data
    if (!paramsInited.value) {
      curveParams.step = res.Data.Step
      curveParams.maxTokens = res.Data.MaxTokens
      curveParams.hitRate = res.Data.HitRate
      curveParams.inOutRatio = res.Data.InOutRatio
      paramsInited.value = true
    } else if (!manual.maxTokens) {
      // 上限以响应里的 MaxTokens 为准（多线时后端按线数自动下调），不硬编码。
      curveParams.maxTokens = res.Data.MaxTokens
    }
    // 时段下拉默认高亮：按后端实际使用的 At 落在哪个时段。
    if (!timeTouched.value) {
      const hhmm = atHHMM(res.Data.At)
      const hit = timeOptions.value.find((t) => inTimeRange(hhmm, t.Start, t.End))
      timeOptionKey.value = hit ? timeOptionValue(hit) : (timeOptions.value[0] ? timeOptionValue(timeOptions.value[0]) : '')
    }
  } catch (e: any) {
    if (seq !== curveSeq) return
    // 403 = 开关未开：行内提示，不弹红条（多线视图同步退回单线）。
    if (onFeatureForbidden(e)) {
      // 开关已关闭：退回单线视图重取一次（不带新参数，403 不会再出现）。
      await loadCurve()
      return
    }
    setErr(e)
  } finally {
    if (seq === curveSeq) loadingCurve.value = false
  }
}

// 从后端回显的 RFC3339 里取「北京时间 HH:MM」。
// 直接切字符串而不用 Date.getHours()：浏览器的本地时区未必是 +08:00，转换会错位。
function atHHMM(iso: string): string {
  const m = /\d{2}:\d{2}(?=:\d{2}|[+Z])/.exec(iso || '')
  if (m) return m[0]
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}
function inTimeRange(hhmm: string, start: string, end: string): boolean {
  if (!hhmm) return false
  const toMin = (s: string) => {
    const [h, m] = (s || '').split(':').map(Number)
    return (h || 0) * 60 + (m || 0)
  }
  const cur = toMin(hhmm)
  return cur >= toMin(start) && cur < (toMin(end) || 24 * 60)
}

function onTimeChange() {
  timeTouched.value = true
  loadCurve()
}
// 记下「用户手动改过哪个采样参数」：只有改过的才回传后端（见 manual 注释）。
function onParamChange(field?: 'step' | 'maxTokens' | 'hitRate' | 'inOut') {
  if (field) manual[field] = true
  loadCurve()
}

// 取数序列：同一批 Points，只按「系数开关」切字段（不自己乘）。
function pointValue(p: { Credits: number; CreditsNoCoeff: number }): number {
  return useCoeff.value ? p.Credits : p.CreditsNoCoeff
}

interface CurveSeries {
  key: 'cost' | 'sale'
  name: string
  color: string
  dashed: boolean
  line: CostPricingLine
}

const availableLines = computed<CurveSeries[]>(() => {
  const c = curve.value
  if (!c) return []
  const out: CurveSeries[] = []
  if (c.Cost?.Available) out.push({ key: 'cost', name: '成本', color: '#5a5ce8', dashed: false, line: c.Cost })
  if (c.Sale?.Available) out.push({ key: 'sale', name: '售价', color: '#e6a23c', dashed: true, line: c.Sale })
  return out
})

const shownLines = computed<CurveSeries[]>(() =>
  availableLines.value.filter((l) =>
    curveMode.value === 'both' ? true : curveMode.value === 'cost' ? l.key === 'cost' : l.key === 'sale',
  ),
)

// 不可用线的原因（Available=false 时按 Reason 明确提示，不画空线、不把 0 当价格）
const unavailableHints = computed(() => {
  const c = curve.value
  if (!c) return []
  const out: { name: string; reason: string }[] = []
  if (!c.Cost?.Available) out.push({ name: '成本线', reason: c.Cost?.Reason || '不可用' })
  if (!c.Sale?.Available) out.push({ name: '售价线', reason: c.Sale?.Reason || '不可用' })
  return out
})

const curveOption = computed<EChartsCoreOption>(() => {
  const lines = shownLines.value
  const series: Record<string, unknown>[] = []
  let markLineDone = false
  let markAreaDone = false

  for (const s of lines) {
    const points = s.line.Points ?? []
    // 超档点：真实计费会失败 → 断线（y = null），并在下方补红标
    const data = points.map((p) => [p.Tokens, p.OverTier ? null : pointValue(p)])
    const item: Record<string, unknown> = {
      name: s.name,
      type: 'line',
      smooth: false,
      showSymbol: false,
      connectNulls: false, // 超档 / 缺数据断线，不补 0
      lineStyle: { width: s.key === 'cost' ? 2.6 : 2, type: s.dashed ? 'dashed' : 'solid', color: s.color },
      itemStyle: { color: s.color },
      data,
    }
    // 拐点竖线只挂一条序列，避免重复绘制
    if (!markLineDone && (s.line.TierBreaks?.length ?? 0) > 0) {
      markLineDone = true
      item.markLine = {
        silent: true,
        symbol: 'none',
        lineStyle: { color: '#f79009', type: 'dashed', width: 1.2 },
        label: { formatter: (p: any) => `×${p?.data?.toCoeff ?? ''}`, color: '#f79009', fontSize: 10 },
        data: (s.line.TierBreaks ?? []).map((b) => ({
          xAxis: b.Tokens,
          toCoeff: b.ToCoeff,
        })),
      }
    }
    // 超档区间底色
    if (!markAreaDone && (s.line.OverTier?.length ?? 0) > 0) {
      markAreaDone = true
      item.markArea = {
        silent: true,
        itemStyle: { color: 'rgba(240,68,56,0.08)' },
        data: (s.line.OverTier ?? []).map((r) => [{ xAxis: r.FromTokens }, { xAxis: r.ToTokens }]),
      }
    }
    series.push(item)
  }

  // 超档红点（第一条可用线的超档区间起点）
  const overPts: [number, number][] = []
  for (const s of lines) {
    for (const r of s.line.OverTier ?? []) overPts.push([r.FromTokens, 0])
  }
  if (lines.length) {
    series.push({
      name: '超档（计费失败）',
      type: 'scatter',
      symbolSize: 8,
      itemStyle: { color: '#f04438' },
      data: overPts,
      z: 10,
    })
  }

  return {
    tooltip: {
      trigger: 'axis',
      valueFormatter: (v: unknown) => (typeof v === 'number' ? String(round4(v)) : String(v)),
    },
    legend: { show: false }, // 图例用页面下方 HTML 版（移动版也能读）
    grid: { left: 12, right: 20, top: 28, bottom: 44, containLabel: true },
    xAxis: {
      type: 'value',
      name: '上下文 token',
      nameLocation: 'middle',
      nameGap: 26,
      axisLabel: { formatter: (v: number) => (v >= 1000 ? `${Math.round(v / 1000)}K` : String(v)) },
      splitLine: { lineStyle: { color: '#eef0f6' } },
    },
    yAxis: {
      type: 'value',
      name: '积分',
      axisLabel: { formatter: (v: number) => String(round4(v)) },
      splitLine: { lineStyle: { color: '#eef0f6' } },
    },
    series,
  }
})

const curveMeta = computed(() => {
  const c = curve.value
  if (!c) return ''
  const parts = [
    `R=${c.R}`,
    `采样 ${c.PointCount} 点（步进 ${fmtInt(c.Step)} / 上限 ${fmtInt(c.MaxTokens)}）`,
    `命中率 ${fmtPct(c.HitRate)} · 输入输出比 ${fmt2(c.InOutRatio)}`,
  ]
  const cost = c.Cost
  if (cost?.Available) {
    parts.push(`成本时段系数 ×${fmt2(cost.TimeCoeff)}（${cost.TimeConfigSource === 'model' ? '模型级' : '全局'}）`)
    parts.push(`分档来源 ${cost.TierSource === 'none' ? '未配分档' : cost.TierSource === 'model' ? '模型级' : '全局'}`)
  }
  return parts.join(' · ')
})

// 模型下拉：优先用统计表实体（ChannelModelID 能直接喂给曲线接口）
function entityLabel(r: CostStatRow) {
  const parts = [r.ChannelName || '(无渠道)', r.InternalModelID, r.ExternalModelName]
  return parts.filter(Boolean).join(' / ')
}
function onEntityChange() {
  paramsInited.value = false // 换模型后重新吃后端给出的默认采样参数
  timeTouched.value = false
  loadCurve()
}

// ===== 多线对比视图（开关打开且选了 ≥1 条时生效）=====
// 调色板：10 色（后端上限也是 10 条线），保证同图可辨识。
const MULTI_PALETTE = [
  '#5a5ce8', '#e6a23c', '#12b76a', '#f04438', '#7a5af8',
  '#0ba5ec', '#ee46bc', '#84cc16', '#f79009', '#667085',
]

interface MultiLineView {
  id: number // ChannelModelID（与请求参数对号）
  label: string // 渠道 / 内部模型 / 对外模型（图例全文）
  short: string // 渠道 · 内部模型（图例紧凑名）
  color: string
  available: boolean
  reason: string
  cost: CostPricingLine | null
  sale: CostPricingLine | null
  overTierFrom: number | null
}

const multiLines = computed<MultiLineView[]>(() => {
  const lines = curve.value?.Lines ?? []
  return [...lines]
    .sort((a, b) => a.LineIndex - b.LineIndex)
    .map((l: CostPricingMultiLine, i: number) => {
      const short = [l.ChannelName || '(无渠道)', l.InternalModelID].filter(Boolean).join(' · ')
      return {
        id: l.ChannelModelID,
        label: [short, l.ExternalModelName].filter(Boolean).join(' / '),
        short,
        color: MULTI_PALETTE[i % MULTI_PALETTE.length],
        // 成本线缺失 = 这条线画不出来（后端口径），售价线另看 Sale.Available
        available: !!l.Available && !!l.Cost?.Available,
        reason: l.Reason || l.Cost?.Reason || '不可用',
        cost: l.Cost?.Available ? l.Cost : null,
        sale: l.Sale?.Available ? l.Sale : null,
        overTierFrom: (l.Cost?.OverTier?.length ?? 0) > 0 ? l.Cost.OverTier[0].FromTokens : null,
      }
    })
})

const multiActive = computed(
  () => featEnabled.value && multiMode.value && multiSelected.value.length > 0 && multiLines.value.length > 0,
)
const multiVisibleLines = computed(() =>
  multiLines.value.filter((l) => l.available && !multiHidden.value.includes(l.id)),
)
// 不可用线（Available=false）：按 Reason 明确标注，不画成 0
const multiUnavailable = computed(() => multiLines.value.filter((l) => !l.available))

function multiSeries(l: MultiLineView, key: 'cost' | 'sale'): Record<string, unknown> | null {
  const line = key === 'cost' ? l.cost : l.sale
  if (!line) return null
  return {
    name: `${l.short} · ${key === 'cost' ? '成本' : '售价'}`,
    type: 'line',
    smooth: false,
    showSymbol: false,
    connectNulls: false, // 超档断线，不补 0
    lineStyle: {
      width: key === 'cost' ? 2.4 : 1.8,
      type: key === 'cost' ? 'solid' : 'dashed',
      color: l.color,
    },
    itemStyle: { color: l.color },
    data: (line.Points ?? []).map((p) => [p.Tokens, p.OverTier ? null : pointValue(p)]),
  }
}

const multiOption = computed<EChartsCoreOption>(() => {
  const series: Record<string, unknown>[] = []
  for (const l of multiVisibleLines.value) {
    if (curveMode.value !== 'sale') {
      const s = multiSeries(l, 'cost')
      if (s) series.push(s)
    }
    if (curveMode.value !== 'cost') {
      const s = multiSeries(l, 'sale')
      if (s) series.push(s)
    }
  }
  // 升档拐点：只挂第一条可见成本线（各家分档可能不同，全画会糊成一片）
  const firstCost = multiVisibleLines.value.find((l) => l.cost)
  const breaks = firstCost?.cost?.TierBreaks ?? []
  if (series.length && curveMode.value !== 'sale' && breaks.length) {
    series[0].markLine = {
      silent: true,
      symbol: 'none',
      lineStyle: { color: '#f79009', type: 'dashed', width: 1.2 },
      label: { formatter: (p: any) => `×${p?.data?.toCoeff ?? ''}`, color: '#f79009', fontSize: 10 },
      data: breaks.map((b) => ({ xAxis: b.Tokens, toCoeff: b.ToCoeff })),
    }
  }
  return {
    tooltip: {
      trigger: 'axis',
      valueFormatter: (v: unknown) => (typeof v === 'number' ? String(round4(v)) : String(v)),
    },
    legend: { show: false },
    grid: { left: 12, right: 20, top: 28, bottom: 44, containLabel: true },
    xAxis: {
      type: 'value',
      name: '上下文 token',
      nameLocation: 'middle',
      nameGap: 26,
      axisLabel: { formatter: (v: number) => (v >= 1000 ? `${Math.round(v / 1000)}K` : String(v)) },
      splitLine: { lineStyle: { color: '#eef0f6' } },
    },
    yAxis: {
      type: 'value',
      name: '积分',
      axisLabel: { formatter: (v: number) => String(round4(v)) },
      splitLine: { lineStyle: { color: '#eef0f6' } },
    },
    series,
  }
})

// 手动上限被后端收敛时的「实际生效」回显（Owl P3-4）：输入框里保留用户填的值，
// 旁边如实显示后端真正用的值（多线按总点数上限自动收敛），避免「填 200000、实际用别的值」看不见。
const maxTokensEffectiveHint = computed(() => {
  const c = curve.value
  if (!c || !manual.maxTokens || !curveParams.maxTokens) return ''
  if (curveParams.maxTokens === c.MaxTokens) return ''
  const n = multiLines.value.length
  return `实际生效 ${fmtInt(c.MaxTokens)} token${n > 1 ? `（${n} 条线按总点数上限自动收敛）` : '（按总点数上限收敛）'}`
})

// 多线口径说明：上限一律显示**响应里的** MaxTokens（线多时后端自动下调，前端不硬编码）
const multiMeta = computed(() => {
  const c = curve.value
  if (!c) return ''
  const n = multiLines.value.length
  const per = multiLines.value.find((l) => l.cost)?.cost?.Points?.length ?? c.PointCount
  const parts = [
    `R=${c.R}`,
    `${n} 条线 · 每条 ${fmtInt(per)} 点`,
    `步进 ${fmtInt(c.Step)} / 上限 ${fmtInt(c.MaxTokens)}${n > 1 ? '（线多时后端自动收敛，以此为准）' : ''}`,
    `命中率 ${fmtPct(c.HitRate)} · 输入输出比 ${fmt2(c.InOutRatio)}`,
  ]
  return parts.join(' · ')
})

// ===================================================================
// C. 会话成本曲线
// ===================================================================
const loadingSessions = ref(false)
const sessions = ref<AdminSession[]>([])
const sessionId = ref('')
const loadingSession = ref(false)
const sessionPoints = ref<SessionPoint[]>([])
const sessionTotals = ref<{ CostCredits: number; SaleCredits: number } | null>(null)
const sessionMeta = ref<{ Total: number; Limit: number; Truncated: boolean } | null>(null)

// 会话请求序号守卫：切换会话立即清空旧数据，并丢弃过期响应（防止旧响应后到覆盖新会话）。
let sessionSeq = 0

async function loadSessions() {
  loadingSessions.value = true
  try {
    const res = await listSessions({ Page: 1, Size: 200 })
    sessions.value = res.Data.List ?? []
  } catch (e: any) {
    setErr(e)
  } finally {
    loadingSessions.value = false
  }
}

// 下拉标签：名字 · 用户 · 模型 · 时间 · N 次调用（长文本由 el-select 截断，title 给全文）
function sessionLabel(s: AdminSession): string {
  const t = (s.LastActive || s.CreatedAt || '').slice(5, 16).replace('T', ' ')
  const name = s.SessionName || s.SessionID.slice(0, 8)
  const user = s.UserNickname || s.UserName || ''
  return [name, user, s.Model, t, `${s.Calls ?? 0} 次调用`].filter(Boolean).join(' · ')
}

// 切换会话：先清空（不给用户看到上一个会话的残留数据），再带序号取新数据。
function onSessionChange() {
  sessionPoints.value = []
  sessionTotals.value = null
  sessionMeta.value = null
  trace.value = null
  loadSession()
}

async function loadSession() {
  const seq = ++sessionSeq
  if (!sessionId.value) return
  loadingSession.value = true
  try {
    const res = await getCostSession(sessionId.value)
    if (seq !== sessionSeq) return // 已切到别的会话，丢弃过期响应
    sessionPoints.value = res.Data.Points ?? []
    sessionTotals.value = res.Data.Totals ?? null
    sessionMeta.value = {
      Total: res.Data.Total ?? 0,
      Limit: res.Data.Limit ?? 0,
      Truncated: !!res.Data.Truncated,
    }
  } catch (e: any) {
    if (seq !== sessionSeq) return
    setErr(e)
  } finally {
    if (seq === sessionSeq) loadingSession.value = false
  }
  // 渠道轨迹跟会话走（开关未开时不发请求，页面与改造前一致）
  if (seq === sessionSeq) await loadTrace()
}

// ===== C+ 会话渠道轨迹（同一开关控制；只读）=====
// 字段分工（后端下发）：Turn 只编成功轮、TurnNo 含失败轮 → x 轴一律用 TurnNo；
// 真实切换点（ActualSwitches, Kind=actual）与预测打平点（PredictedBreakEven, Kind=predicted/Predicted=true）
// 是两种东西，前端用不同符号 + 不同文案 + 不同颜色区分，绝不混为一谈。
const loadingTrace = ref(false)
const trace = ref<CostTraceData | null>(null)
let traceSeq = 0

async function loadTrace() {
  const seq = ++traceSeq
  if (!sessionId.value || !featEnabled.value) {
    trace.value = null
    return
  }
  loadingTrace.value = true
  try {
    const res = await getCostSessionTrace(sessionId.value, {}, { silentStatuses: [403] })
    if (seq !== traceSeq) return
    trace.value = res.Data
  } catch (e: any) {
    if (seq !== traceSeq) return
    trace.value = null
    if (onFeatureForbidden(e)) return
    setErr(e)
  } finally {
    if (seq === traceSeq) loadingTrace.value = false
  }
}

const tracePoints = computed(() => trace.value?.Points ?? [])
const traceSegments = computed(() => trace.value?.Segments ?? [])
const traceSwitches = computed(() => trace.value?.ActualSwitches ?? [])
const tracePredicted = computed<CostTraceBreakEven | null>(() => trace.value?.PredictedBreakEven ?? null)
const traceTruncatedText = computed(() => {
  const t = trace.value
  if (!t?.Truncated) return ''
  return `轨迹仅显示最早 ${fmtInt(t.Limit)} 行，共 ${fmtInt(t.Total)} 行`
})

// 段 → 轮次区间：后端 Segments 由同一批 Points 顺序切出，按点数顺序消费即可（不重算渠道归属）。
// 点数对不上就不猜，返回空数组（此时段列表只显示次数，不显示轮次区间）。
const segmentRanges = computed<{ from: number; to: number }[]>(() => {
  const pts = tracePoints.value
  const segs = traceSegments.value
  if (!pts.length || !segs.length) return []
  const total = segs.reduce((n, s) => n + Math.max(0, s.Points || 0), 0)
  if (total !== pts.length) return []
  const out: { from: number; to: number }[] = []
  let cursor = 0
  for (const s of segs) {
    const count = Math.max(0, s.Points || 0)
    if (count <= 0) return []
    out.push({ from: pts[cursor].TurnNo, to: pts[cursor + count - 1].TurnNo })
    cursor += count
  }
  return out
})

function segRangeText(seg: CostTraceSegment, i: number): string {
  const r = segmentRanges.value[i]
  return r ? `第 ${r.from}–${r.to} 轮` : `${fmtInt(seg.Points)} 次调用`
}

// 预测打平点的落点：后端给的是**上下文 token 数**（打平点），不是轮次。
// 这里只做「定位」——找本会话里上下文首次达到该 token 数的那一轮，不重算打平值；
// 会话没到过那个长度就不落点（避免在图上画出一个假的打平位置）。
const predictedAnchor = computed<{ turnNo: number; ctxTokens: number } | null>(() => {
  const p = tracePredicted.value
  if (!p || !p.Matched || p.Tokens <= 0) return null
  for (const pt of tracePoints.value) {
    const ctx = (pt.InputTokens || 0) + (pt.CacheReadTokens || 0)
    if (ctx >= p.Tokens) return { turnNo: pt.TurnNo, ctxTokens: ctx }
  }
  return null
})
// 会话里最大上下文（用于「本会话还没到打平点」的说明，取的是真实值）
const traceMaxCtx = computed(() =>
  tracePoints.value.reduce((m, p) => Math.max(m, (p.InputTokens || 0) + (p.CacheReadTokens || 0)), 0),
)

// 真实切换点坐标：x = 切换后第一轮的 TurnNo，y = 该轮的真实成本（找不到该轮就不落点）
const switchMarks = computed<{ s: CostTraceSwitch; x: number; y: number }[]>(() => {
  const byTurn = new Map(tracePoints.value.map((p) => [p.TurnNo, p]))
  const out: { s: CostTraceSwitch; x: number; y: number }[] = []
  for (const s of traceSwitches.value) {
    const p = byTurn.get(s.TurnNo)
    if (p) out.push({ s, x: s.TurnNo, y: p.CostCredits })
  }
  return out
})

const traceOption = computed<EChartsCoreOption>(() => {
  const pts = tracePoints.value
  const costData = pts.map((p) => [p.TurnNo, isFailedPoint(p) ? null : p.CostCredits])
  const saleData = pts.map((p) => [p.TurnNo, isFailedPoint(p) ? null : p.SaleCredits])
  const failedCoords: [number, number][] = pts.filter(isFailedPoint).map((p) => [p.TurnNo, 0])

  // 连续段底色（渠道不同 → 颜色交替；单轮段左右各放半格保证可见）
  const bands = segmentRanges.value.map((r, i) => [
    {
      xAxis: Math.max(0.5, r.from - 0.5),
      name: traceSegments.value[i]?.ChannelName || '(无渠道)',
      itemStyle: { color: i % 2 === 0 ? 'rgba(90,92,232,0.05)' : 'rgba(18,183,106,0.07)' },
    },
    { xAxis: r.to + 0.5 },
  ])

  const switchLines = switchMarks.value.map((m) => ({
    xAxis: m.x,
    label: { formatter: `真实切换 #${m.s.Seq}`, color: '#f04438', fontSize: 10, position: 'insideEndTop' },
  }))

  const series: Record<string, unknown>[] = [
    {
      name: '成本',
      type: 'line',
      showSymbol: true,
      symbolSize: 5,
      connectNulls: false,
      lineStyle: { width: 2.4, color: '#5a5ce8' },
      itemStyle: { color: '#5a5ce8' },
      data: costData,
      markArea: bands.length ? { silent: true, label: { show: true, position: 'insideTop', fontSize: 10, color: '#98a2b3' }, data: bands } : undefined,
      markLine: switchLines.length
        ? { silent: true, symbol: 'none', lineStyle: { color: '#f04438', type: 'dashed', width: 1.2 }, data: switchLines }
        : undefined,
    },
    {
      name: '售价',
      type: 'line',
      showSymbol: true,
      symbolSize: 5,
      connectNulls: false,
      lineStyle: { width: 2, type: 'dashed', color: '#e6a23c' },
      itemStyle: { color: '#e6a23c' },
      data: saleData,
    },
    {
      // 失败轮：红圈空心点（与真实切换的实心 pin 明显不同）
      name: '失败轮（未计费）',
      type: 'scatter',
      symbol: 'emptyCircle',
      symbolSize: 9,
      itemStyle: { color: '#f04438', borderColor: '#f04438', borderWidth: 1.6 },
      data: failedCoords,
      z: 10,
    },
    {
      // 真实切换点：实心 pin + 竖虚线（事实，红色）
      name: '真实切换点',
      type: 'scatter',
      symbol: 'pin',
      symbolSize: 26,
      itemStyle: { color: '#f04438' },
      data: switchMarks.value.map((m) => ({ value: [m.x, m.y], switch: m.s })),
      z: 12,
    },
  ]

  // 预测打平点：紫色菱形 + 明确「预测」标签（只有能定位到轮次时才落点）
  const anchor = predictedAnchor.value
  const anchorPoint = anchor ? pts.find((p) => p.TurnNo === anchor.turnNo) : undefined
  if (anchor && anchorPoint) {
    series.push({
      name: '预测打平点（预测）',
      type: 'scatter',
      symbol: 'diamond',
      symbolSize: 16,
      itemStyle: { color: '#7a5af8', borderColor: '#fff', borderWidth: 2 },
      label: { show: true, position: 'top', formatter: '预测打平点', color: '#7a5af8', fontSize: 10 },
      data: [[anchor.turnNo, anchorPoint.CostCredits]],
      z: 11,
    })
  }

  return {
    tooltip: {
      trigger: 'axis',
      valueFormatter: (v: unknown) => (typeof v === 'number' ? String(round4(v)) : String(v)),
    },
    legend: { show: false },
    grid: { left: 12, right: 24, top: 26, bottom: 44, containLabel: true },
    xAxis: {
      type: 'value',
      name: '轮次（含失败轮）',
      nameLocation: 'middle',
      nameGap: 26,
      min: 1,
      minInterval: 1,
      splitLine: { lineStyle: { color: '#eef0f6' } },
    },
    yAxis: {
      type: 'value',
      name: '积分',
      axisLabel: { formatter: (v: number) => String(round4(v)) },
      splitLine: { lineStyle: { color: '#eef0f6' } },
    },
    series,
  }
})

// 失败轮判定：后端直接给 Failed（= Status=='failed'），两个都给时取或。
function isFailedPoint(p: SessionPoint): boolean {
  return p.Failed === true || (p.Status || '').toLowerCase() === 'failed'
}

// x 轴用 TurnNo（时间线序号，含失败轮）：失败轮占位、后续点不错位。
// Turn 是「成功轮序号」，失败轮为 0，拿它当 x 会挤到最左边，不再使用。
const failedPoints = computed<SessionPoint[]>(() => sessionPoints.value.filter(isFailedPoint))
const failedCount = computed(() => failedPoints.value.length)

// 失败轮红点：叠在图上，y 固定 0（失败轮金额恒 0），不参与正常曲线连接。
const sessionFailedCoords = computed<[number, number][]>(() =>
  failedPoints.value.map((p) => [p.TurnNo, 0]),
)

const sessionTruncatedText = computed(() => {
  const m = sessionMeta.value
  if (!m?.Truncated) return ''
  return `仅显示最早 ${fmtInt(m.Limit)} 行，共 ${fmtInt(m.Total)} 行（可用 Limit 参数最多取 2000 行）`
})

const sessionOption = computed<EChartsCoreOption>(() => {
  const pts = sessionPoints.value
  // 失败轮 y 置 null → 曲线在该轮断开，不使用 0 参与连线（避免把失败点连成「成本骤降」的假曲线）
  const costData = pts.map((p) => [p.TurnNo, isFailedPoint(p) ? null : p.CostCredits])
  const saleData = pts.map((p) => [p.TurnNo, isFailedPoint(p) ? null : p.SaleCredits])
  return {
    tooltip: {
      trigger: 'axis',
      valueFormatter: (v: unknown) => (typeof v === 'number' ? String(round4(v)) : String(v)),
    },
    legend: { show: false },
    grid: { left: 12, right: 20, top: 24, bottom: 44, containLabel: true },
    xAxis: {
      type: 'value',
      name: '调用序号',
      nameLocation: 'middle',
      nameGap: 26,
      min: 1,
      minInterval: 1,
      splitLine: { lineStyle: { color: '#eef0f6' } },
    },
    yAxis: {
      type: 'value',
      name: '积分',
      axisLabel: { formatter: (v: number) => String(round4(v)) },
      splitLine: { lineStyle: { color: '#eef0f6' } },
    },
    series: [
      {
        name: '成本',
        type: 'line',
        showSymbol: true,
        symbolSize: 5,
        connectNulls: false, // 失败轮断开，不连成正常曲线
        lineStyle: { width: 2.4, color: '#5a5ce8' },
        itemStyle: { color: '#5a5ce8' },
        data: costData,
      },
      {
        name: '售价',
        type: 'line',
        showSymbol: true,
        symbolSize: 5,
        connectNulls: false,
        lineStyle: { width: 2, type: 'dashed', color: '#e6a23c' },
        itemStyle: { color: '#e6a23c' },
        data: saleData,
      },
      {
        // 失败轮单独一条红点散点：不带线、不参与连接，仅占位标注
        name: '失败轮（未计费）',
        type: 'scatter',
        symbolSize: 9,
        itemStyle: { color: '#f04438' },
        data: sessionFailedCoords.value,
        z: 10,
      },
    ],
  }
})

// ===================================================================
// 加载编排
// ===================================================================
const loading = computed(
  () => loadingStats.value || loadingCurve.value || loadingSession.value || loadingTrace.value,
)

async function loadAll() {
  errInfo.value = { message: '', requestId: '' }
  featBlocked.value = ''
  // 先读开关：关闭时后面的一切与改造前完全一致（不发新参数、不请求 /trace）
  await loadFeatureSwitch()
  await loadStats()
  if (!curveEntity.value && curveEntities.value.length) {
    const prefer = curveEntities.value.find((r) => r.ChannelModelID > 0) || curveEntities.value[0]
    curveEntity.value = prefer.EntityKey
  }
  await loadSessions()
  if (!sessionId.value && sessions.value.length) sessionId.value = sessions.value[0].SessionID
  await Promise.all([loadCurve(), loadSession()])
}

async function onRefresh() {
  paramsInited.value = false
  await loadAll()
  if (!errInfo.value.message) ElMessage.success('已刷新')
}

onMounted(() => {
  mq = window.matchMedia(NARROW_QUERY)
  onMqChange(mq)
  mq.addEventListener('change', onMqChange)
  loadAll()
})
onUnmounted(() => {
  mq?.removeEventListener('change', onMqChange)
})
</script>

<template>
  <div class="page-container cost-page">
    <div class="toolbar">
      <span class="tb-hint">全部历史口径（仅积分，不做汇率折算）；本页纯只读。</span>
      <div class="toolbar-spacer" />
      <el-button :icon="Refresh" :loading="loading" @click="onRefresh">刷新</el-button>
    </div>

    <div class="panel-stack">
      <!-- 开关未开启（含「读到开关是开的、请求时已被关掉」的 403）：行内引导，不弹红条、不留空白 -->
      <div v-if="featBlocked" class="feat-notice">
        <el-tag size="small" type="info" effect="plain">增强视图未开启</el-tag>
        <span class="feat-msg">{{ featBlocked }}</span>
        <span class="feat-guide">
          可在「计费配置 → 系统配置 → 成本页增强视图」打开后重试；关闭状态下本页其余功能不受影响。
        </span>
      </div>

      <!-- A. 成本统计（按当前实体聚合） -->
      <div class="card">
        <div class="block-title">
          成本统计
          <el-tag size="small" effect="plain">按当前实体聚合</el-tag>
          <el-tag size="small" effect="plain" type="info">曾用名标注</el-tag>
        </div>
        <div class="block-desc">
          按「渠道 × 内部模型 × 对外模型」<b>当前实体</b>聚合（仅 completed 消耗）：改名/换绑自动归一到当前实体，
          历史文本快照以「曾用名」标注；健康探测记账已从统计中移除（口径已确认，售价数字下降属预期修正）。
          表尾为全量合计（不随排序变化）。
        </div>

        <!-- 桌面：表格 -->
        <div class="desktop-only">
          <el-table
            v-loading="loadingStats"
            :data="statsRows"
            :row-key="rowKey"
            border
            stripe
            show-summary
            :summary-method="sumSummary"
            class="table-nowrap"
          >
            <el-table-column prop="ChannelName" label="渠道" min-width="140" sortable />
            <el-table-column prop="InternalModelID" label="内部模型" min-width="160" sortable show-overflow-tooltip />
            <el-table-column prop="ExternalModelName" label="对外模型" min-width="170" sortable show-overflow-tooltip>
              <template #default="{ row }">
                <span>{{ row.ExternalModelName }}</span>
                <el-tooltip v-if="oldNames(row).length" :content="oldNamesTitle(row)" placement="top" :show-after="150">
                  <span class="oldname">曾用名 {{ oldNames(row).length }}</span>
                </el-tooltip>
              </template>
            </el-table-column>
            <el-table-column prop="CostCredits" label="成本(积分)" min-width="118" align="right" sortable :formatter="cellCost" />
            <el-table-column prop="SaleCredits" label="售价(积分)" min-width="118" align="right" sortable :formatter="cellCost" />
            <el-table-column prop="CacheHitRate" label="缓存命中率" min-width="120" align="right" sortable :formatter="cellHit" />
            <el-table-column prop="InputOutputRatio" label="输入输出比" min-width="110" align="right" sortable :formatter="cellRatio" />
            <el-table-column prop="Requests" label="请求数" min-width="96" align="right" sortable :formatter="cellInt" />
            <el-table-column prop="InputTokens" label="输入token" min-width="110" align="right" sortable :formatter="cellInt" />
            <el-table-column prop="CacheReadTokens" label="缓存读token" min-width="126" align="right" sortable :formatter="cellInt" />
            <el-table-column prop="OutputTokens" label="输出token" min-width="110" align="right" sortable :formatter="cellInt" />
          </el-table>
          <el-empty v-if="!loadingStats && !statsRows.length" description="暂无成本数据" />
        </div>

        <!-- 窄屏：数字墙 + 实体行（单列、不横向滚动） -->
        <div class="mobile-only">
          <div class="sum-wall">
            <div class="sum-item">
              <b>{{ statsTotal ? fmt2(statsTotal.CostCredits) : '-' }}</b>
              <span>总成本</span>
            </div>
            <div class="sum-item sale">
              <b>{{ statsTotal ? fmt2(statsTotal.SaleCredits) : '-' }}</b>
              <span>总售价</span>
            </div>
            <div class="sum-item">
              <b>{{ statsTotal ? fmtInt(statsTotal.Requests) : '-' }}</b>
              <span>请求</span>
            </div>
          </div>
          <div v-for="r in statsRows" :key="r.EntityKey" class="entity-row">
            <div class="entity-name">
              <div class="n1">{{ r.ExternalModelName || r.InternalModelID || '(未知模型)' }}</div>
              <div class="n2">
                {{ r.ChannelName || '(无渠道)' }} · {{ r.InternalModelID }}
                <span v-if="oldNames(r).length" class="oldname">曾用名 {{ oldNames(r).length }}</span>
              </div>
            </div>
            <div class="entity-nums">
              <div class="n-price">{{ fmt2(r.SaleCredits) }}</div>
              <div class="n-sub">售价 · 成本 {{ fmt2(r.CostCredits) }}</div>
              <div class="n-sub">请求 {{ fmtInt(r.Requests) }} · 命中 {{ fmtPct(r.CacheHitRate) }}</div>
            </div>
          </div>
          <el-empty v-if="!loadingStats && !statsRows.length" description="暂无成本数据" :image-size="60" />
        </div>
      </div>

      <!-- B. 模型定价曲线 -->
      <div class="card">
        <div class="block-title">
          模型定价曲线
          <el-tag size="small" effect="plain">含分档 × 分时段</el-tag>
        </div>
        <div class="block-desc">
          每轮成本 =（输入×输入价 + 缓存读×缓存读价 + 输出×输出价）× 时段系数 × 分档系数 ÷ R；
          成本线取渠道内部模型配置、售价线取对外模型配置。跨档画拐点竖线，超出分档上限的区段断线并标红
          （真实请求会失败，不再画更贵的线）。
        </div>

        <div class="curve-toolbar">
          <span class="p-label">内部模型</span>
          <el-select
            v-model="curveEntity"
            filterable
            placeholder="选择模型（来自成本统计实体）"
            class="curve-select"
            @change="onEntityChange"
          >
            <el-option v-for="r in curveEntities" :key="r.EntityKey" :label="entityLabel(r)" :value="r.EntityKey">
              <span class="opt-main">{{ entityLabel(r) }}</span>
              <span class="opt-sub">{{ fmt2(r.CostCredits) }} / {{ fmt2(r.SaleCredits) }}</span>
            </el-option>
          </el-select>

          <span class="p-label">步进</span>
          <el-input-number
            v-model="curveParams.step"
            :min="1"
            :step="500"
            :controls="!isNarrow"
            controls-position="right"
            class="curve-num"
            @change="onParamChange('step')"
          />
          <span class="p-label">上限 token</span>
          <el-input-number
            v-model="curveParams.maxTokens"
            :min="0"
            :step="10000"
            :controls="!isNarrow"
            controls-position="right"
            class="curve-num"
            @change="onParamChange('maxTokens')"
          />
          <span v-if="maxTokensEffectiveHint" class="hint" style="margin: 0 0 0 6px">
            {{ maxTokensEffectiveHint }}
          </span>
          <span class="p-label">缓存命中率 %</span>
          <el-input-number
            v-model="hitRatePct"
            :min="0"
            :max="100"
            :step="5"
            :controls="!isNarrow"
            controls-position="right"
            class="curve-num"
            @change="onParamChange('hitRate')"
          />
          <span class="p-label">输入输出比</span>
          <el-input-number
            v-model="curveParams.inOutRatio"
            :min="0"
            :step="1"
            :controls="!isNarrow"
            controls-position="right"
            class="curve-num"
            @change="onParamChange('inOut')"
          />
          <span class="p-label">时段</span>
          <el-select v-model="timeOptionKey" placeholder="时段" class="curve-time" @change="onTimeChange">
            <el-option v-for="t in timeOptions" :key="timeOptionValue(t)" :label="timeOptionLabel(t)" :value="timeOptionValue(t)" />
          </el-select>
          <span class="switch-wrap">
            <el-switch v-model="useCoeff" size="small" />
            <span class="p-label">{{ useCoeff ? '含系数' : '不含系数' }}</span>
          </span>
          <el-radio-group v-model="curveMode" class="curve-mode">
            <el-radio-button value="both">两条</el-radio-button>
            <el-radio-button value="cost">成本</el-radio-button>
            <el-radio-button value="sale">售价</el-radio-button>
          </el-radio-group>
        </div>

        <!-- 多线对比（开关 console.cost_multichannel_trace 打开时才出现；不选 = 与改造前完全一致） -->
        <div v-if="featEnabled" class="multi-bar">
          <span class="switch-wrap">
            <el-switch v-model="multiMode" size="small" @change="onMultiModeChange" />
            <span class="p-label">多渠道对比</span>
          </span>
          <template v-if="multiMode">
            <el-select
              v-model="multiSelected"
              multiple
              filterable
              collapse-tags
              collapse-tags-tooltip
              :multiple-limit="MULTI_LIMIT"
              placeholder="选择 1~10 个「渠道 × 内部模型」同图对比"
              class="multi-select"
              @change="onMultiSelectChange"
            >
              <el-option v-for="r in multiOptions" :key="r.ChannelModelID" :label="entityLabel(r)" :value="r.ChannelModelID">
                <span class="opt-main">{{ entityLabel(r) }}</span>
                <span class="opt-sub">成本 {{ fmt2(r.CostCredits) }}</span>
              </el-option>
            </el-select>
            <span class="p-label muted">最多 {{ MULTI_LIMIT }} 条</span>
          </template>
          <span v-else class="p-label muted">
            打开后可用同一条横轴对比多个渠道的成本/售价曲线（只读）
          </span>
        </div>

        <div v-loading="loadingCurve">
          <VChart
            v-if="multiActive ? multiVisibleLines.length : shownLines.length"
            :option="multiActive ? multiOption : curveOption"
            :height="isNarrow ? '240px' : '360px'"
          />
          <div v-else class="empty-hint">
            <template v-if="featEnabled && multiMode && !multiSelected.length">
              已打开多渠道对比：请在上方选择 1~10 个「渠道 × 内部模型」（当前仍按单线展示）。
            </template>
            <template v-else-if="multiActive && multiUnavailable.length">
              所选渠道模型都不可用（见下方原因）。
            </template>
            <template v-else>{{ curve ? '所选模型没有可用曲线（见下方原因）。' : '暂无定价曲线数据。' }}</template>
          </div>
          <!-- 单线：不可用原因 -->
          <div v-if="!multiActive && unavailableHints.length" class="unavailable">
            <el-tag v-for="h in unavailableHints" :key="h.name" size="small" type="danger" effect="plain">
              {{ h.name }}不可用：{{ h.reason }}
            </el-tag>
          </div>
          <!-- 多线：逐线不可用原因（按 Reason 说明，不画成 0） -->
          <div v-if="multiActive && multiUnavailable.length" class="unavailable">
            <el-tag v-for="l in multiUnavailable" :key="l.id" size="small" type="danger" effect="plain">
              {{ l.label }} 不可用：{{ l.reason }}
            </el-tag>
          </div>
          <div v-if="multiActive && multiLines.some((l) => l.overTierFrom)" class="hint">
            超档线（这些渠道从对应 token 起真实计费会失败，曲线已断开）：
            <span v-for="l in multiLines.filter((x) => x.overTierFrom)" :key="l.id" class="over-item">
              {{ l.short }} 从 {{ fmtInt(l.overTierFrom || 0) }} tokens
            </span>
          </div>
        </div>

        <!-- 图例：多线模式可逐条点选显隐（不可用线置灰） -->
        <div v-if="multiActive" class="legend multi-legend">
          <el-check-tag
            v-for="l in multiLines"
            :key="l.id"
            :checked="!multiHidden.includes(l.id)"
            :disabled="!l.available"
            class="multi-chip"
            @change="toggleMultiLine(l.id)"
          >
            <i class="line-mark" :style="{ background: l.available ? l.color : '#d0d5dd' }" />
            {{ l.short }}
            <span class="chip-sub">{{ l.label.includes('/') ? l.label.split('/').pop()?.trim() : '' }}</span>
            <el-tooltip v-if="!l.available" :content="l.reason" placement="top" :show-after="150">
              <span class="chip-warn">不可用</span>
            </el-tooltip>
          </el-check-tag>
          <span><i class="dot-mark tier" />升档拐点（首条）</span>
        </div>
        <div v-else class="legend">
          <span v-for="l in shownLines" :key="l.key">
            <i
              class="line-mark"
              :class="{ dashed: l.dashed }"
              :style="l.dashed ? { borderColor: l.color } : { background: l.color }"
            />{{ l.name }}
          </span>
          <span><i class="dot-mark tier" />升档拐点</span>
          <span><i class="dot-mark over" />超档（计费失败）</span>
        </div>
        <div class="hint">{{ multiActive ? multiMeta : curveMeta }}</div>
        <div class="hint">
          时段系数按所选时段整笔计（跨时段不切分）；系数开关只切后端给的两套序列（含 / 不含系数），前端不做乘法。
          <template v-if="featEnabled">
            多线模式下所有线共用一条横轴与一套采样参数，上限以上方响应回报的 MaxTokens 为准。
          </template>
        </div>
      </div>

      <!-- C. 会话成本曲线 -->
      <div class="card">
        <div class="block-title">
          会话成本曲线
          <el-tag size="small" effect="plain" type="info">切换防竞态</el-tag>
        </div>
        <div class="block-desc">
          取该会话逐次调用的历史实际 cost_credits / credits_consumed（含当时时段/分档系数），x 轴 = 调用序号（含失败轮）。
          切换会话时立即清空旧数据并丢弃过期响应。
        </div>
        <div class="curve-toolbar">
          <span class="p-label">会话</span>
          <el-select
            v-model="sessionId"
            filterable
            :loading="loadingSessions"
            placeholder="选择会话"
            class="session-select"
            @change="onSessionChange"
          >
            <el-option v-for="s in sessions" :key="s.SessionID" :label="sessionLabel(s)" :value="s.SessionID">
              <span class="opt-main" :title="sessionLabel(s)">{{ sessionLabel(s) }}</span>
            </el-option>
          </el-select>
          <template v-if="sessionTotals">
            <el-tag size="small" effect="plain" type="info">成本 {{ fmt2(sessionTotals.CostCredits) }}</el-tag>
            <el-tag size="small" effect="plain" type="warning">售价 {{ fmt2(sessionTotals.SaleCredits) }}</el-tag>
            <span class="page-count">{{ sessionPoints.length }} 次调用</span>
            <el-tag v-if="failedCount" size="small" type="danger" effect="plain">
              含 {{ failedCount }} 次失败调用，未计费
            </el-tag>
          </template>
        </div>
        <div v-if="sessionTruncatedText" class="trunc-notice">
          <el-tag size="small" type="warning" effect="plain">已截断</el-tag>
          <span>{{ sessionTruncatedText }}</span>
        </div>
        <div v-loading="loadingSession">
          <VChart
            v-if="sessionPoints.length"
            :option="sessionOption"
            :height="isNarrow ? '220px' : '320px'"
          />
          <div v-else class="empty-hint">
            {{ sessionId ? '该会话暂无逐轮成本数据。' : '请选择一个会话以查看逐轮成本/售价曲线。' }}
          </div>
        </div>
        <div class="legend">
          <span><i class="line-mark" style="background: #5a5ce8" />成本</span>
          <span><i class="line-mark dashed" style="border-color: #e6a23c" />售价</span>
          <span><i class="dot-mark over" />失败轮（未计费）</span>
        </div>
        <div class="hint">
          x 轴用调用序号（含失败轮，1..{{ sessionPoints.length }}）：失败轮打红点、金额按 0 呈现且不参与连线，后续轮次不左移。
          合计只累加成功轮。
        </div>

        <!-- C+ 会话渠道轨迹（同一开关；关闭时整块不出现） -->
        <template v-if="featEnabled && sessionId">
          <el-divider class="trace-divider">
            <span class="trace-divider-text">渠道轨迹</span>
          </el-divider>
          <div v-if="traceTruncatedText" class="trunc-notice">
            <el-tag size="small" type="warning" effect="plain">轨迹已截断</el-tag>
            <span>{{ traceTruncatedText }}</span>
          </div>
          <div v-loading="loadingTrace">
            <VChart
              v-if="tracePoints.length"
              :option="traceOption"
              :height="isNarrow ? '240px' : '340px'"
            />
            <div v-else-if="!loadingTrace" class="empty-hint">该会话暂无渠道轨迹数据（没有账单记录）。</div>
          </div>
          <div v-if="tracePoints.length" class="legend">
            <span><i class="line-mark" style="background: #5a5ce8" />成本</span>
            <span><i class="line-mark dashed" style="border-color: #e6a23c" />售价</span>
            <span><i class="dot-mark failed" />失败轮（未计费）</span>
            <span><i class="dot-mark actual" />真实切换点（账单事实）</span>
            <span><i class="dot-mark predicted" />预测打平点（预测，非事实）</span>
            <span><i class="band-mark" />连续段（同渠道同模型）</span>
          </div>

          <!-- 真实切换点：按发生顺序列出（from → to） -->
          <div v-if="traceSwitches.length" class="trace-block">
            <div class="trace-block-title">真实切换（{{ traceSwitches.length }} 次）</div>
            <div v-for="s in traceSwitches" :key="`sw-${s.Seq}`" class="switch-row">
              <el-tag size="small" type="danger" effect="dark">真实切换 #{{ s.Seq }}</el-tag>
              <span class="switch-main">
                第 {{ s.TurnNo }} 轮：{{ s.FromChannelName || '(无渠道)' }} → <b>{{ s.ToChannelName || '(无渠道)' }}</b>
              </span>
              <span class="switch-sub">{{ s.FromInternalModelID || '—' }} → {{ s.ToInternalModelID || '—' }}</span>
            </div>
          </div>

          <!-- 预测打平点：只有后端给出时展示；null（已真实切换 / 算不出）不占位成 0 -->
          <div v-if="tracePredicted" class="trace-block">
            <div class="trace-block-title">
              预测打平点
              <el-tag size="small" type="warning" effect="plain">预测，非实际切换</el-tag>
            </div>
            <div v-if="tracePredicted.Matched" class="pred-body">
              <div class="pred-main">
                上下文约 <b>{{ fmtInt(tracePredicted.Tokens) }}</b> tokens 起，
                <b>{{ tracePredicted.AlternativeChannelName || `渠道模型 ${tracePredicted.AlternativeChannelModelID}` }}</b>
                的单次成本预计低于 {{ tracePredicted.ChannelName || `渠道模型 ${tracePredicted.ChannelModelID}` }}
                <template v-if="predictedAnchor">（本会话第 {{ predictedAnchor.turnNo }} 轮实际到了 {{ fmtInt(predictedAnchor.ctxTokens) }} tokens）</template>
              </div>
              <div class="pred-sub">
                按同一内部模型在其它渠道的成本曲线推算，采样步进 {{ fmtInt(tracePredicted.Step) }} / 上限
                {{ fmtInt(tracePredicted.MaxTokens) }}；不是账单里发生过的事实。
              </div>
              <div v-if="!predictedAnchor" class="pred-sub">
                本会话最大上下文 {{ fmtInt(traceMaxCtx) }} tokens，尚未达到该打平点，所以时间轴上没有落点。
              </div>
            </div>
            <div v-else class="pred-body">
              <div class="pred-main">无法给出打平点：{{ tracePredicted.Reason || '采样范围内未出现更便宜的替代渠道' }}</div>
              <div class="pred-sub">按这里如实展示原因，不补一个估算数字。</div>
            </div>
          </div>
          <div v-else-if="traceSwitches.length" class="hint">
            本会话已发生真实切换，不再给出预测打平点（后端 PredictedBreakEven 为 null，此处不占位）。
          </div>

          <!-- 连续段：同渠道同模型的区间与花费 -->
          <div v-if="traceSegments.length" class="trace-block">
            <div class="trace-block-title">连续段（{{ traceSegments.length }} 段）</div>
            <div v-for="(s, i) in traceSegments" :key="`seg-${s.Seq}`" class="seg-row">
              <span class="seg-idx">{{ s.Seq }}</span>
              <span class="seg-ch">{{ s.ChannelName || '(无渠道)' }} · {{ s.InternalModelID || '—' }}</span>
              <span class="seg-turns">{{ segRangeText(s, i) }}</span>
              <span class="seg-nums">
                {{ fmtInt(s.Points) }} 次（成功 {{ fmtInt(s.SuccessPoints) }}） · 成本 {{ fmt2(s.CostCredits) }} · 售价
                {{ fmt2(s.SaleCredits) }}
              </span>
            </div>
          </div>
        </template>
      </div>

      <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
    </div>
  </div>
</template>

<style scoped>
.cost-page {
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
.tb-hint {
  color: var(--color-text-tertiary);
  font-size: 12px;
}
.p-label {
  color: var(--color-text-secondary);
  font-size: 13px;
  white-space: nowrap;
}
.curve-toolbar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 12px 14px;
  margin-bottom: 14px;
}
.curve-select {
  min-width: 280px;
  max-width: 460px;
}
.curve-time {
  min-width: 200px;
}
.curve-num {
  width: 132px;
}
.switch-wrap {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.curve-mode {
  margin-left: 4px;
}
.session-select {
  width: 420px;
  max-width: 100%;
}
/* 长会话名不撑破容器（Element Plus 默认只在部分场景截断） */
.session-select :deep(.el-select__selected-item),
.session-select :deep(.el-select__placeholder) {
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.opt-main {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.opt-sub {
  float: right;
  color: var(--color-text-tertiary);
  font-size: 11px;
  margin-left: 12px;
}
.oldname {
  margin-left: 6px;
  font-size: 11px;
  color: var(--color-text-tertiary);
  cursor: help;
}
.unavailable {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 10px;
}
.legend {
  display: flex;
  flex-wrap: wrap;
  gap: 14px;
  margin-top: 10px;
  font-size: 12px;
  color: var(--color-text-secondary);
}
.line-mark {
  display: inline-block;
  width: 14px;
  height: 3px;
  border-radius: 2px;
  margin-right: 5px;
  vertical-align: 2px;
}
.line-mark.dashed {
  height: 0;
  border-top: 2px dashed currentColor;
  background: transparent;
  border-radius: 0;
}
.dot-mark {
  display: inline-block;
  width: 9px;
  height: 9px;
  border-radius: 2px;
  margin-right: 5px;
  vertical-align: 0;
}
.dot-mark.tier {
  background: #f79009;
}
.dot-mark.over {
  background: #f04438;
  border-radius: 50%;
}
/* 失败轮：空心红圈（与真实切换点的实心红 pin 区分） */
.dot-mark.failed {
  background: transparent;
  border: 2px solid #f04438;
  border-radius: 50%;
  width: 10px;
  height: 10px;
}
/* 真实切换点：实心红色 Pin（事实） */
.dot-mark.actual {
  background: #f04438;
  border-radius: 50% 50% 50% 0;
  transform: rotate(-45deg);
}
/* 预测打平点：紫色菱形（预测，非事实） */
.dot-mark.predicted {
  background: #7a5af8;
  transform: rotate(45deg);
}
/* 连续段底色 */
.band-mark {
  display: inline-block;
  width: 14px;
  height: 9px;
  margin-right: 5px;
  border-radius: 2px;
  background: rgba(90, 92, 232, 0.12);
  vertical-align: 0;
}
.multi-bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px 14px;
  margin: -4px 0 14px;
  padding: 10px 12px;
  border: 1px dashed var(--color-border);
  border-radius: 10px;
}
.multi-select {
  min-width: 320px;
  max-width: 560px;
}
.muted {
  color: var(--color-text-tertiary);
}
.multi-legend {
  gap: 8px;
}
.multi-chip {
  font-size: 12px;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.chip-sub {
  margin-left: 4px;
  color: var(--color-text-tertiary);
  font-size: 11px;
}
.chip-warn {
  margin-left: 6px;
  color: #b42318;
  font-size: 11px;
}
.over-item {
  margin-right: 8px;
}
.feat-notice {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px 10px;
  padding: 12px 14px;
  border: 1px solid #d0d5dd;
  background: #f9fafb;
  border-radius: 10px;
  font-size: 13px;
  color: var(--color-text-secondary);
}
.feat-msg {
  color: var(--color-text);
}
.feat-guide {
  color: var(--color-text-tertiary);
  font-size: 12px;
}
.trace-divider {
  margin: 18px 0 12px;
}
.trace-divider-text {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text-secondary);
}
.trace-block {
  margin-top: 12px;
}
.trace-block-title {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text);
  margin-bottom: 8px;
}
.switch-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px 10px;
  padding: 7px 0;
  border-bottom: 1px dashed var(--color-border);
  font-size: 13px;
}
.switch-row:last-of-type {
  border-bottom: none;
}
.switch-main {
  color: var(--color-text);
}
.switch-sub {
  color: var(--color-text-tertiary);
  font-size: 12px;
}
.pred-body {
  border: 1px solid #e4e7ec;
  border-radius: 10px;
  padding: 10px 12px;
  background: #fbfaff;
}
.pred-main {
  font-size: 13px;
  color: var(--color-text);
  line-height: 1.7;
}
.pred-sub {
  margin-top: 4px;
  font-size: 11px;
  color: var(--color-text-tertiary);
  line-height: 1.6;
}
.seg-row {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 4px 10px;
  padding: 7px 0;
  border-bottom: 1px dashed var(--color-border);
  font-size: 13px;
}
.seg-row:last-of-type {
  border-bottom: none;
}
.seg-idx {
  width: 20px;
  color: var(--color-text-tertiary);
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}
.seg-ch {
  font-weight: 600;
  color: var(--color-text);
}
.seg-turns {
  color: var(--color-text-secondary);
  font-size: 12px;
}
.seg-nums {
  color: var(--color-text-tertiary);
  font-size: 12px;
}
.hint {
  margin-top: 6px;
  font-size: 11px;
  line-height: 1.6;
  color: var(--color-text-tertiary);
}
.empty-hint {
  font-size: 13px;
  color: var(--color-text-tertiary);
  padding: 10px 4px;
}
.trunc-notice {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 0 0 10px;
  font-size: 12px;
  color: var(--color-warning);
}

/* ===== 移动版：数字墙 + 实体行 ===== */
.desktop-only {
  display: block;
}
.mobile-only {
  display: none;
}
.sum-wall {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 10px;
  margin-bottom: 12px;
}
.sum-item {
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: 10px;
  padding: 10px 12px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.sum-item b {
  font-size: 18px;
  font-weight: 800;
  font-variant-numeric: tabular-nums;
  font-family: 'SF Mono', Consolas, monospace;
  color: var(--color-text);
}
.sum-item span {
  font-size: 12px;
  color: var(--color-text-secondary);
}
.sum-item.sale b {
  color: #b45309;
}
.entity-row {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 10px;
  padding: 10px 0;
  border-bottom: 1px dashed var(--color-border);
}
.entity-row:last-of-type {
  border-bottom: none;
}
.entity-name {
  min-width: 0;
  flex: 1;
}
.entity-name .n1 {
  font-size: 14px;
  font-weight: 700;
  color: var(--color-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.entity-name .n2 {
  font-size: 12px;
  color: var(--color-text-tertiary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.entity-nums {
  text-align: right;
  flex-shrink: 0;
}
.entity-nums .n-price {
  font-size: 15px;
  font-weight: 700;
  font-variant-numeric: tabular-nums;
  font-family: 'SF Mono', Consolas, monospace;
  color: var(--color-text);
}
.entity-nums .n-sub {
  font-size: 11px;
  color: var(--color-text-tertiary);
}

@media (max-width: 768px) {
  .desktop-only {
    display: none;
  }
  .mobile-only {
    display: block;
  }
  .block-desc {
    font-size: 14px;
  }
  .curve-toolbar {
    gap: 8px;
    align-items: stretch;
  }
  .curve-select,
  .curve-time,
  .session-select,
  .curve-num,
  .multi-select {
    width: 100%;
    min-width: 0;
    max-width: none;
  }
  .p-label {
    font-size: 13px;
  }
  /* 说明类长文本在窄屏必须换行，否则会横向溢出 */
  .multi-bar .p-label,
  .multi-bar .muted {
    white-space: normal;
    font-size: 14px;
  }
  .seg-row,
  .switch-row,
  .pred-main {
    font-size: 14px;
  }
  .seg-turns,
  .seg-nums,
  .switch-sub,
  .chip-sub,
  .pred-sub {
    font-size: 13px;
  }
}
</style>
