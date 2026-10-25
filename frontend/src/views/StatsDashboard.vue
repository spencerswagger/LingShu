<script setup lang="ts">
// 统计页（admin 全局 / dev 本人共用），版式按定稿 demo，数据接后端真实接口：
//   ① 首屏 3 个大数字（总 Token / 总消费积分 / 吞吐量）+ 5 项小指标
//   ② 24h 四日对比趋势（今天/昨天/前天/大前天，按 HourOfDay 对齐，缺桶断线）
//   ③ 模型广场（后端已排序 + 截断，含零用量模型供折叠）
//   ④ 实时区（/stats/live，30s 刷新）：我的会话 / 在线用户
//
// 三个吞吐口径刻意区分（见 docs/designs/2026-10-25-stats-dashboard-backend.md §4）：
//   首屏      = KPI.TPM            全窗口忙时口径（≈60÷同批请求平均耗时）
//   模型广场  = List[].TPM         按模型忙时口径
//   实时区    = Sessions/Users.TPM 近 60 分钟墙钟平均（含排队空转，天然更低）
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import type { EChartsCoreOption } from 'echarts/core'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '@/stores/auth'
import {
  getStatsDashboard,
  getStatsLive,
  type StatsDashboard,
  type StatsHour,
  type StatsLive,
  type StatsTopItem,
  type StatsModelStat,
  type StatsModelPlaza,
} from '@/api/stats'
import { listAnnouncements, type AdminAnnouncement } from '@/api/admin'
import { listDevAnnouncements } from '@/api/dev'
import { toRFC3339CN } from '@/utils/format'
import VChart from '@/components/VChart.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

const auth = useAuthStore()
const isAdmin = computed(() => auth.role === 'ADMIN')

const loading = ref(false)
const data = ref<StatsDashboard | null>(null)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const announcements = ref<AdminAnnouncement[]>([])

// ===== 时间范围：近 24h / 7 / 30 / 90 天 / 自定义 =====
// 粒度映射：近 24h → hour；其余 → day（后端 Granularity 参数，缺省 auto 时也是这个规律）。
type Preset = '24h' | '7' | '30' | '90' | 'custom'
const preset = ref<Preset>('24h')
const customRange = ref<[string, string] | null>(null)

const granularity = computed<'hour' | 'day'>(() => (preset.value === '24h' ? 'hour' : 'day'))
// 服务端实际生效粒度（回显用，可能与请求值不同的兜底：auto 判定）
const effectiveGranularity = computed(() => data.value?.Usage?.Granularity || granularity.value)

const rangeText = computed(() => {
  if (preset.value === 'custom' && customRange.value) {
    return `${customRange.value[0]} ~ ${customRange.value[1]}`
  }
  if (preset.value === '24h') return '近 24 小时'
  return `近 ${preset.value} 天`
})

// 生成 RFC3339（固定 +08:00，Asia/Shanghai），保证后端按东八区日期切分。
function parseYMD(s: string): Date {
  const [y, m, d] = s.split('-').map(Number)
  return new Date(y, (m || 1) - 1, d || 1)
}

function resolveRange(): { from: Date; to: Date } {
  if (preset.value === 'custom' && customRange.value) {
    const from = parseYMD(customRange.value[0])
    const to = parseYMD(customRange.value[1])
    to.setHours(23, 59, 59)
    return { from, to }
  }
  if (preset.value === '24h') {
    const to = new Date()
    const from = new Date(to.getTime() - 24 * 3600 * 1000)
    return { from, to }
  }
  const now = new Date()
  const end = new Date(now.getFullYear(), now.getMonth(), now.getDate(), 23, 59, 59)
  const days = Number(preset.value) || 30
  const from = new Date(end)
  from.setDate(from.getDate() - (days - 1))
  from.setHours(0, 0, 0, 0)
  return { from, to: end }
}

// ===== 模型广场排序（服务端排序，切排序会重取 dashboard）=====
type ModelSort = 'tokens' | 'tpm' | 'failed' | 'latency'
const modelSort = ref<ModelSort>('tokens')
const MODEL_SORT_OPTIONS: { key: ModelSort; label: string; tip: string }[] = [
  { key: 'tokens', label: 'Token', tip: '按窗口内 Token 消耗倒序' },
  { key: 'tpm', label: '吞吐', tip: '按模型忙时口径 TPM 倒序' },
  { key: 'failed', label: '失败', tip: '按窗口内失败请求数倒序' },
  { key: 'latency', label: '延迟', tip: '按全程平均耗时（AvgMS）倒序' },
]
const MODEL_LIMIT = 50
const COMPARE_DAYS = 4

// silent：定时刷新用，不显示整页 loading
async function load(opts: { silent?: boolean } = {}) {
  if (!opts.silent) loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const { from, to } = resolveRange()
    const res = await getStatsDashboard(auth.role, {
      From: toRFC3339CN(from),
      To: toRFC3339CN(to),
      Granularity: granularity.value,
      CompareDays: COMPARE_DAYS,
      ModelSort: modelSort.value,
      ModelLimit: MODEL_LIMIT,
    })
    data.value = res.Data
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    if (!opts.silent) loading.value = false
  }
}

async function loadAnnouncements() {
  try {
    const res = isAdmin.value ? await listAnnouncements() : await listDevAnnouncements()
    announcements.value = (res.Data.List || []).filter((a) => a.Enabled).slice(0, 4)
  } catch {
    announcements.value = []
  }
}

// ===== 实时区（/stats/live，30s 刷新）=====
const live = ref<StatsLive | null>(null)
const liveLoading = ref(false)
const liveErr = ref('')
const liveTab = ref<'sessions' | 'users'>('sessions')
const LIVE_WINDOW_MINUTES = 60
const LIVE_SPARK_POINTS = 12
const LIVE_LIMIT = 20
const LIVE_REFRESH_MS = 30000

async function loadLive() {
  liveLoading.value = true
  try {
    const res = await getStatsLive(auth.role, {
      Window: LIVE_WINDOW_MINUTES,
      SparkPoints: LIVE_SPARK_POINTS,
      Limit: LIVE_LIMIT,
    })
    live.value = res.Data
    liveErr.value = ''
  } catch (e: any) {
    // 实时区失败不打断页面，也不清空上一次的数据（只标记失败）。
    liveErr.value = e?.message || '实时数据获取失败'
  } finally {
    liveLoading.value = false
  }
}

// 30s 定时刷新：重取实时区 + 当前筛选条件下的 dashboard（静默，不闪整页 loading）。
// 该函数只「读」preset / customRange，从不写它们，所以刷新不会重置用户选的筛选条件；
// 页面不可见时直接跳过，避免后台标签页白跑一轮聚合。
async function refreshTick() {
  if (document.visibilityState === 'hidden') return
  await loadLive()
  if (!loading.value) await load({ silent: true })
}

let liveTimer: number | undefined

onMounted(() => {
  load()
  loadAnnouncements()
  loadLive()
  liveTimer = window.setInterval(refreshTick, LIVE_REFRESH_MS)
})
onUnmounted(() => {
  if (liveTimer !== undefined) window.clearInterval(liveTimer)
  liveTimer = undefined
})

watch(preset, () => {
  if (preset.value !== 'custom') load()
})
watch(customRange, (v) => {
  if (preset.value === 'custom' && v && v[0] && v[1]) load()
})
watch(modelSort, () => load())

// ===== 格式化 =====
const fmtInt = (n: number) => Number(n || 0).toLocaleString('zh-CN')
const fmtMoney = (n: number) =>
  Number(n || 0).toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 })
const fmtPct = (n: number, digits = 1) => `${(Number(n || 0) * 100).toFixed(digits)}%`
const fmtMs = (n: number) => {
  const v = Number(n || 0)
  return v >= 1000 ? `${(v / 1000).toFixed(2)}s` : `${Math.round(v)}ms`
}
const fmtCompact = (n: number) => {
  const v = Number(n || 0)
  const abs = Math.abs(v)
  if (abs >= 1e9) return `${(v / 1e9).toFixed(2)}B`
  if (abs >= 1e6) return `${(v / 1e6).toFixed(2)}M`
  if (abs >= 1e3) return `${(v / 1e3).toFixed(2)}K`
  if (abs >= 1) return v.toFixed(abs >= 100 ? 0 : 1)
  return v.toFixed(2)
}
const fmtTPM = (n: number) => {
  const v = Number(n || 0)
  return v >= 100 ? Math.round(v).toString() : v.toFixed(1)
}
const fmtDelta = (n: number) => `${n >= 0 ? '▲' : '▼'} ${Math.abs(Number(n || 0) * 100).toFixed(1)}%`
const fmtTime = (iso: string) => {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  return d.toLocaleTimeString('zh-CN', { hour12: false })
}

// ===== 图表主题 =====
const C = {
  primary: '#4f46e5',
  violet: '#7c3aed',
  text: '#101828',
  sub: '#667085',
  grid: '#eef0f6',
  series: ['#4f46e5', '#8b5cf6', '#06b6d4', '#f79009', '#12b76a', '#f04438'],
}
const axisCommon = {
  axisLine: { lineStyle: { color: C.grid } },
  axisTick: { show: false },
  axisLabel: { color: C.sub, fontSize: 11 },
}
const tooltipCommon = {
  backgroundColor: 'rgba(255,255,255,0.96)',
  borderColor: C.grid,
  textStyle: { color: C.text, fontSize: 12 },
  extraCssText: 'box-shadow:0 6px 20px rgba(16,24,40,0.12);border-radius:8px;',
}

// ===== ① 首屏大数字 =====
// 吞吐量样本量判定：BusyRequests / SumDurationMS 是忙时口径的分子分母。
const throughput = computed(() => {
  const k = data.value?.KPI
  return {
    ready: !!k && (k.BusyRequests ?? 0) > 0 && (k.SumDurationMS ?? 0) > 0,
    lowSample: !!k && ((k.BusyRequests ?? 0) < 20 || (k.SumDurationMS ?? 0) < 30000),
  }
})

interface BigStat {
  label: string
  glyph: string
  tone: 'brand' | 'violet' | 'info'
  value: string
  unit?: string
  sub: string
  delta?: number
  winLabel: string
  winTip?: string
}

const bigStats = computed<BigStat[]>(() => {
  const k = data.value?.KPI
  const t = data.value?.Usage.Total
  return [
    {
      label: '总 Token',
      glyph: 'T',
      tone: 'brand',
      value: k ? fmtCompact(k.TotalTokens) : '--',
      sub: `输入 ${fmtCompact(t?.Input ?? 0)} · 输出 ${fmtCompact(t?.Output ?? 0)}`,
      delta: k?.DeltaTokens,
      winLabel: rangeText.value,
      winTip: '窗口 = 当前统计范围；输入为纯输入、缓存读单列、输出含推理',
    },
    {
      label: '总消费积分',
      glyph: '¥',
      tone: 'violet',
      value: k ? fmtMoney(k.TotalCredits) : '--',
      sub: `日均 ${fmtMoney((k?.TotalCredits ?? 0) / Math.max(data.value?.Usage.Days ?? 1, 1))}`,
      delta: k?.DeltaCredits,
      winLabel: rangeText.value,
      winTip: '窗口 = 当前统计范围',
    },
    {
      label: '吞吐量',
      glyph: '⚡',
      tone: 'info',
      value: throughput.value.ready ? fmtTPM(k?.TPM ?? 0) : '--',
      unit: ' TPM',
      sub: throughput.value.ready
        ? `区间 ${rangeText.value} · 请求 ${(k?.RPM ?? 0).toFixed(1)} RPM${throughput.value.lowSample ? ' · 样本较少' : ''}`
        : '区间内没有成功且带耗时的请求',
      winLabel: '忙时口径',
      winTip:
        '窗口 = 当前统计范围；忙时口径 = 成功且带耗时请求的 Token ÷ 同批耗时折算分钟数（≈60÷平均耗时）。与实时区的「近 60 分钟墙钟平均」不是同一口径',
    },
  ]
})

// ===== ② 小指标行 =====
interface MiniStat {
  label: string
  value: string
  tone?: 'ok' | 'info'
  tip?: string
}

const miniStats = computed<MiniStat[]>(() => {
  const k = data.value?.KPI
  const l = live.value
  return [
    { label: '总请求数', value: k ? fmtInt(k.TotalRequests) : '--' },
    { label: '成功率', value: k ? fmtPct(k.SuccessRate) : '--', tone: 'ok' },
    { label: '平均耗时', value: k ? fmtMs(k.AvgDurationMS) : '--' },
    {
      label: '请求中会话',
      value: l ? fmtInt(l.SessionsTotal) : '…',
      tone: 'info',
      tip: '实时区 /stats/live：请求中（Inflight>0）的会话数',
    },
    {
      label: '请求中用户',
      value: l ? fmtInt(l.UsersTotal) : '…',
      tone: 'info',
      tip: '实时区 /stats/live：有请求中会话的用户数',
    },
  ]
})

// ===== ③ 24h 四日对比趋势 =====
type TrendMetric = 'tokens' | 'credits' | 'calls'
const trendMetric = ref<TrendMetric>('tokens')
const trendMetricName = computed(() =>
  trendMetric.value === 'tokens' ? 'Token' : trendMetric.value === 'credits' ? '积分' : '请求数',
)
const HOUR_SLOTS = Array.from({ length: 24 }, (_, i) => `${String(i).padStart(2, '0')}:00`)
// 颜色按 Offset 递增：今天的线最深，越往前越浅。
const TREND_COLORS = ['#4f46e5', '#a5b4fc', '#c4b5fd', '#e9d5ff', '#94a3b8', '#cbd5e1', '#e2e8f0']

interface TrendSeries {
  label: string
  offset: number
  color: string
  points: StatsHour[]
}

// Compare 为「今天/昨天/前天/大前天」四条按小时序列；后端无数据时回退单条 Hourly。
const trendSeries = computed<TrendSeries[]>(() => {
  const cmp = (data.value?.Usage.Compare ?? []).filter(Boolean)
  if (cmp.length) {
    return cmp.map((s, i) => ({
      label: s.Label || `D-${s.Offset}`,
      offset: s.Offset,
      color: TREND_COLORS[i % TREND_COLORS.length],
      points: s.Points ?? [],
    }))
  }
  const hourly = data.value?.Usage.Hourly ?? []
  if (hourly.length) {
    return [{ label: '今天', offset: 0, color: TREND_COLORS[0], points: hourly }]
  }
  return []
})

function metricValue(p: StatsHour): number {
  if (trendMetric.value === 'credits') return Number(p.Credits)
  if (trendMetric.value === 'calls') return p.Calls
  return p.Tokens
}

const trendOption = computed<EChartsCoreOption>(() => ({
  grid: { left: 8, right: 16, top: 28, bottom: 4, containLabel: true },
  legend: { show: false }, // 图例用卡片头部的 HTML 版（与定稿 demo 一致）
  tooltip: { ...tooltipCommon, trigger: 'axis' },
  xAxis: {
    type: 'category',
    boundaryGap: false,
    data: HOUR_SLOTS,
    ...axisCommon,
    axisLabel: { color: C.sub, fontSize: 11, interval: 3 },
  },
  yAxis: {
    type: 'value',
    name: trendMetricName.value,
    nameTextStyle: { color: C.sub, fontSize: 11, align: 'left' },
    splitLine: { lineStyle: { color: C.grid } },
    axisLabel: { color: C.sub, fontSize: 11, formatter: (v: number) => fmtCompact(v) },
  },
  series: trendSeries.value.map((s, i) => {
    const byHour = new Map(s.points.map((p) => [p.HourOfDay, p]))
    return {
      name: s.label,
      type: 'line',
      smooth: true,
      showSymbol: false,
      // 缺桶断线：后端缺小时不出行，这里对齐不到就留 null（绝不补 0）
      connectNulls: false,
      data: HOUR_SLOTS.map((_, h) => {
        const p = byHour.get(h)
        return p ? metricValue(p) : null
      }),
      lineStyle: { width: i === 0 ? 2.8 : 2, color: s.color },
      itemStyle: { color: s.color },
    }
  }),
}))

const trendNote = computed(() => {
  const parts: string[] = []
  parts.push(`数据粒度：${effectiveGranularity.value === 'hour' ? '按小时' : '按天'}`)
  if (effectiveGranularity.value === 'day') {
    parts.push('四日对比仍是近 4 天 × 小时桶（Compare 接口口径），与筛选粒度解耦')
  }
  parts.push('缺小时断线，不补 0')
  return parts.join(' · ')
})

// 四条序列都没有任何小时桶 = 窗口内确实没有用量 → 走空态，而不是画四条空线。
const hasTrendData = computed(() => trendSeries.value.some((s) => s.points.length > 0))

// ===== ③ 模型广场 =====
interface PlazaRow {
  model: string
  tokensText: string
  tpmText: string
  callsText: string
  failText: string
  latencyText: string
  barPct: number
  color: string
  active: boolean
  enabled: boolean
}

const plaza = computed<StatsModelPlaza | null>(() => data.value?.ModelPlaza ?? null)

const plazaRows = computed<PlazaRow[]>(() => {
  const list: StatsModelStat[] = plaza.value?.List ?? []
  if (!list.length) return []
  const max = Math.max(...list.map((m) => m.Tokens), 1)
  return list.map((m, i) => ({
    model: m.Model || m.Label || '(未命名模型)',
    tokensText: fmtCompact(m.Tokens),
    // BusyMS=0（窗口内没有成功且带耗时的请求）时后端 TPM 为 0，这里显示 —
    tpmText: m.BusyMS > 0 ? `${fmtTPM(m.TPM)} TPM` : '—',
    callsText: `${fmtInt(m.Calls)} / ${fmtInt(m.Failed)}`,
    failText: m.Calls > 0 ? fmtPct(m.FailRate) : '—',
    latencyText:
      m.AvgFirstMS > 0 || m.AvgMS > 0
        ? `${m.AvgFirstMS > 0 ? fmtMs(m.AvgFirstMS) : '—'} / ${m.AvgMS > 0 ? fmtMs(m.AvgMS) : '—'}`
        : '—',
    barPct: Math.max(0, Math.round((m.Tokens / max) * 100)),
    color: C.series[i % C.series.length],
    active: m.Active,
    enabled: m.Enabled,
  }))
})

const activeModels = computed(() => plazaRows.value.filter((m) => m.active))
const idleModels = computed(() => plazaRows.value.filter((m) => !m.active))
const idleExpanded = ref(false)
const visibleModels = computed(() => (idleExpanded.value ? plazaRows.value : activeModels.value))

const plazaTruncatedText = computed(() => {
  const p = plaza.value
  if (!p || !p.Truncated) return ''
  return `仅显示前 ${p.Limit} 个模型（目录共 ${p.Total} 个，窗口内有请求 ${p.Active} 个）`
})

async function copyModelName(name: string) {
  try {
    await navigator.clipboard.writeText(name)
    ElMessage.success(`已复制模型名：${name}`)
  } catch {
    ElMessage.warning('复制失败，请手动选择模型名')
  }
}

// ===== ④ 实时区 =====
function sparkPath(vals: number[]): string {
  const n = vals.length
  if (n < 2) return ''
  const w = 90
  const h = 24
  const max = Math.max(...vals)
  const min = Math.min(...vals)
  const span = max - min || 1
  const step = w / (n - 1)
  return vals
    .map((v, i) => `${(i === 0 ? 'M' : 'L')}${(1 + i * step).toFixed(1)},${(h - 2 - ((v - min) / span) * (h - 6)).toFixed(1)}`)
    .join(' ')
}

interface LiveSessionRow {
  id: string
  name: string
  model: string
  inflight: number
  contextText: string
  contextTip: string
  roundText: string
  callsText: string
  tpmText: string
  spentText: string
  spark: string
  lastActive: string
}

const sessionRows = computed<LiveSessionRow[]>(() =>
  (live.value?.Sessions ?? []).map((s) => {
    // ContextLimit 恒 0 = 平台未配置模型上限：只显示已用绝对值，不做除法、不造分母。
    const hasLimit = s.ContextLimit > 0
    return {
      id: s.ID,
      name: s.Name || s.ID.slice(0, 8),
      model: s.Model,
      inflight: s.Inflight,
      contextText: hasLimit ? `${fmtCompact(s.ContextUsed)}/${fmtCompact(s.ContextLimit)}` : fmtCompact(s.ContextUsed),
      contextTip: hasLimit ? '上下文已用 / 上限' : '仅显示上下文字数：平台未配置模型上限',
      roundText: fmtCompact(s.RoundTokens),
      callsText: fmtInt(s.Calls),
      tpmText: `${fmtTPM(s.TPM)} TPM`,
      spentText: fmtMoney(s.SpentCredits),
      spark: sparkPath(s.Spark ?? []),
      lastActive: fmtTime(s.LastActive),
    }
  }),
)

interface LiveUserRow {
  id: string
  name: string
  initial: string
  sessions: number
  tpmText: string
  tokensText: string
  spentText: string
  spark: string
}

const userRows = computed<LiveUserRow[]>(() =>
  (live.value?.Users ?? []).map((u) => ({
    id: u.ID,
    name: u.Name || `用户 ${u.ID}`,
    initial: (u.Name || '?').slice(0, 2),
    sessions: u.Sessions,
    tpmText: `${fmtTPM(u.TPM)} TPM`,
    tokensText: fmtCompact(u.Tokens),
    spentText: fmtMoney(u.SpentCredits),
    spark: sparkPath(u.Spark ?? []),
  })),
)

const liveSampledAt = computed(() => fmtTime(live.value?.SampledAt || ''))
const liveWindowMinutes = computed(() => live.value?.WindowMinutes ?? LIVE_WINDOW_MINUTES)

// ===== 更多维度（旧版用量分析里仍有数据支撑的部分，默认收起）=====
const moreOpen = ref<string[]>([])

const compositionOption = computed<EChartsCoreOption>(() => {
  const t = data.value?.Usage.Total
  // 三段口径：输入为纯输入、缓存读单列、输出含推理，各切片相加 = 总量、不重复。
  const items = [
    { name: '输入', value: t?.Input ?? 0 },
    { name: '缓存读', value: t?.CacheRead ?? 0 },
    { name: '输出', value: t?.Output ?? 0 },
  ].filter((i) => i.value > 0)
  return {
    tooltip: {
      ...tooltipCommon,
      trigger: 'item',
      formatter: (p: any) => `${p.name}<br/>${fmtInt(p.value)} (${p.percent}%)`,
    },
    legend: { bottom: 0, icon: 'circle', itemWidth: 8, textStyle: { color: C.sub, fontSize: 11 } },
    series: [
      {
        type: 'pie',
        radius: ['48%', '72%'],
        center: ['50%', '44%'],
        avoidLabelOverlap: true,
        itemStyle: { borderColor: '#fff', borderWidth: 2 },
        label: { show: false },
        color: C.series,
        data: items.length ? items : [{ name: '暂无数据', value: 0 }],
      },
    ],
  }
})

function hbarOption(
  items: StatsTopItem[],
  valueFn: (i: StatsTopItem) => number,
  color: string,
): EChartsCoreOption {
  const rows = items.slice().reverse()
  return {
    grid: { left: 6, right: 28, top: 8, bottom: 4, containLabel: true },
    tooltip: {
      ...tooltipCommon,
      trigger: 'axis',
      axisPointer: { type: 'shadow' },
      formatter: (ps: any) => {
        const p = ps[0]
        const it = rows[p.dataIndex]
        return `${it.Label}${it.SubLabel ? ' · ' + it.SubLabel : ''}<br/>${fmtInt(valueFn(it))}`
      },
    },
    xAxis: {
      type: 'value',
      splitLine: { lineStyle: { color: C.grid } },
      axisLabel: { color: C.sub, fontSize: 11, formatter: (v: number) => fmtCompact(v) },
    },
    yAxis: {
      type: 'category',
      data: rows.map((i) => (i.SubLabel ? `${i.Label} · ${i.SubLabel}` : i.Label)),
      ...axisCommon,
      axisLabel: { color: C.text, fontSize: 11, width: 120, overflow: 'truncate' },
    },
    series: [
      {
        type: 'bar',
        data: rows.map(valueFn),
        barMaxWidth: 14,
        itemStyle: { color, borderRadius: [0, 4, 4, 0] },
      },
    ],
  }
}
const keyOption = computed<EChartsCoreOption>(() =>
  hbarOption(data.value?.Usage.ByKey ?? [], (i) => i.Tokens, C.violet),
)

const levelType = (l: string) => (['danger', 'warning'].includes(l) ? l : 'primary')
const modeLabel = (m: string) =>
  m === 'sale' ? '售价模式' : m === 'cost' ? '成本模式' : m === '' ? '其他（探测等）' : m
</script>

<template>
  <div class="page-container">
    <!-- 工具栏：统计范围 + 刷新 + 自动刷新提示 -->
    <div class="page-header">
      <span class="page-count">统计范围：{{ rangeText }}</span>
      <el-radio-group v-model="preset" size="small">
        <el-radio-button value="24h">近 24h</el-radio-button>
        <el-radio-button value="7">近 7 天</el-radio-button>
        <el-radio-button value="30">近 30 天</el-radio-button>
        <el-radio-button value="90">近 90 天</el-radio-button>
        <el-radio-button value="custom">自定义</el-radio-button>
      </el-radio-group>
      <el-date-picker
        v-if="preset === 'custom'"
        v-model="customRange"
        type="daterange"
        size="small"
        value-format="YYYY-MM-DD"
        range-separator="至"
        start-placeholder="开始日期"
        end-placeholder="结束日期"
        style="width: 240px"
      />
      <el-button size="small" :loading="loading" @click="load()">刷新</el-button>
      <span class="live-tag"><span class="dot-live" />实时区每 30s 自动刷新</span>
    </div>

    <div v-loading="loading">
      <!-- ① 首屏：3 个大数字，各自带窗口/口径标签 -->
      <div class="big-grid">
        <div v-for="s in bigStats" :key="s.label" class="big-card" :data-tone="s.tone">
          <div class="big-head">
            <span class="big-glyph" :data-tone="s.tone">{{ s.glyph }}</span>
            <span class="big-label">{{ s.label }}</span>
            <el-tooltip v-if="s.winTip" :content="s.winTip" placement="top" :show-after="150">
              <span class="win-tag" :data-tone="s.tone">{{ s.winLabel }}</span>
            </el-tooltip>
            <span v-else class="win-tag" :data-tone="s.tone">{{ s.winLabel }}</span>
          </div>
          <div class="big-value">
            {{ s.value }}<small v-if="s.unit">{{ s.unit }}</small>
          </div>
          <div class="big-foot">
            <span class="big-sub">{{ s.sub }}</span>
            <span v-if="s.delta !== undefined" class="big-delta" :class="s.delta >= 0 ? 'up' : 'down'">
              {{ fmtDelta(s.delta as number) }}
            </span>
          </div>
        </div>
      </div>

      <!-- ② 小指标行 -->
      <div class="mini-grid">
        <div v-for="m in miniStats" :key="m.label" class="mini-card">
          <span class="mini-label">{{ m.label }}</span>
          <el-tooltip v-if="m.tip" :content="m.tip" placement="top" :show-after="150">
            <span class="mini-value" :data-tone="m.tone">{{ m.value }}</span>
          </el-tooltip>
          <span v-else class="mini-value" :data-tone="m.tone">{{ m.value }}</span>
        </div>
      </div>

      <!-- ③ 两张图：24h 四日对比 + 模型广场 -->
      <div class="chart-stack">
        <div class="card">
          <div class="card-head">
            <h3 class="card-title">用量趋势</h3>
            <span class="win-chip">近 24h · 四日对比</span>
            <span class="gran-chip">{{ effectiveGranularity === 'hour' ? '按小时' : '按天' }}</span>
            <div class="trend-legend">
              <span v-for="s in trendSeries" :key="s.label">
                <i :style="{ background: s.color }" />{{ s.label }}
              </span>
              <span v-if="!trendSeries.length" class="legend-empty">暂无数据</span>
            </div>
            <el-radio-group v-model="trendMetric" size="small" class="head-actions">
              <el-radio-button value="tokens">Token</el-radio-button>
              <el-radio-button value="credits">积分</el-radio-button>
              <el-radio-button value="calls">请求数</el-radio-button>
            </el-radio-group>
          </div>
          <VChart v-if="hasTrendData" :option="trendOption" height="260px" />
          <el-empty v-else description="当前范围内没有用量数据" :image-size="70" />
          <p class="pending-note">{{ trendNote }}</p>
        </div>

        <div class="card">
          <div class="card-head">
            <h3 class="card-title">模型广场</h3>
            <el-tooltip
              content="窗口 = 当前统计范围；按模型忙时口径（该模型成功且带耗时请求的 Token ÷ 同批耗时折算分钟数）"
              placement="top"
              :show-after="150"
            >
              <span class="win-chip">按模型忙时</span>
            </el-tooltip>
            <div class="sort-group">
              <span class="sort-label">排序：</span>
              <el-tooltip v-for="o in MODEL_SORT_OPTIONS" :key="o.key" :content="o.tip" placement="top" :show-after="150">
                <button
                  type="button"
                  class="sort-chip"
                  :class="{ on: modelSort === o.key }"
                  @click="modelSort = o.key"
                >
                  {{ o.label }}
                </button>
              </el-tooltip>
            </div>
          </div>

          <template v-if="plazaRows.length">
            <div v-if="visibleModels.length" class="model-grid">
              <div v-for="m in visibleModels" :key="m.model" class="model-card">
                <div class="model-head">
                  <span class="model-name">
                    {{ m.model }}
                    <button type="button" class="copy-btn" title="复制模型名" @click="copyModelName(m.model)">⧉</button>
                  </span>
                  <span class="model-state" :data-active="m.active">
                    {{ m.active ? '活跃' : '空闲' }}{{ m.enabled ? '' : ' · 已停用' }}
                  </span>
                </div>
                <div class="model-row"><span>Token 消耗</span><b>{{ m.tokensText }}</b></div>
                <div class="model-row"><span>吞吐量（忙时）</span><b>{{ m.tpmText }}</b></div>
                <div class="model-row">
                  <span>请求 / 失败</span>
                  <b>{{ m.callsText }}<em class="fail-rate">{{ m.failText }}</em></b>
                </div>
                <div class="model-row"><span>首字 / 全程</span><b>{{ m.latencyText }}</b></div>
                <div class="model-bar"><i :style="{ width: m.barPct + '%', background: m.color }" /></div>
              </div>
            </div>
            <div class="idle-note">
              <span v-if="!visibleModels.length && !idleExpanded">
                当前窗口内没有模型请求，可展开查看模型目录
              </span>
              <span v-if="idleModels.length">
                另有 {{ idleModels.length }} 个模型当前无请求，
                <button type="button" class="link-btn" @click="idleExpanded = !idleExpanded">
                  {{ idleExpanded ? '收起' : '展开查看' }}
                </button>
              </span>
              <span v-if="plazaTruncatedText" class="trunc-note">{{ plazaTruncatedText }}</span>
            </div>
          </template>
          <el-empty v-else description="模型目录为空：还没有对外模型" :image-size="70" />
        </div>
      </div>

      <!-- ④ 实时区：/stats/live，30s 刷新 -->
      <section class="live-block">
        <div class="live-head">
          <h3 class="live-title">实时会话</h3>
          <span class="live-tag"><span class="dot-live" />每 30s 自动刷新 · 仅显示请求中</span>
          <span class="live-window">
            近 {{ liveWindowMinutes }} 分钟墙钟平均<span v-if="liveSampledAt"> · 采样 {{ liveSampledAt }}</span>
            <span v-if="liveLoading" class="live-loading">刷新中…</span>
          </span>
        </div>

        <div class="live-tabs">
          <el-radio-group v-model="liveTab" size="small">
            <el-radio-button value="sessions">
              我的会话{{ live ? ` · ${live.SessionsTotal}` : '' }}
            </el-radio-button>
            <el-radio-button v-if="isAdmin" value="users">
              在线用户{{ live ? ` · ${live.UsersTotal}` : '' }}
            </el-radio-button>
          </el-radio-group>
          <span v-if="liveErr" class="live-err">{{ liveErr }}</span>
        </div>

        <div v-if="liveTab === 'sessions'" class="live-card">
          <template v-if="sessionRows.length">
            <div v-for="s in sessionRows" :key="s.id" class="srow">
              <span class="srow-dot" />
              <span class="srow-name" :title="s.name">{{ s.name }}</span>
              <span class="model-chip">{{ s.model }}</span>
              <span class="srow-stat">
                <el-tooltip :content="s.contextTip" placement="top" :show-after="150">
                  <span>上下文 <b>{{ s.contextText }}</b></span>
                </el-tooltip>
                <span>本轮 <b>{{ s.roundText }}</b></span>
                <span>吞吐 <b>{{ s.tpmText }}</b></span>
                <span>已花 <b>{{ s.spentText }}</b> 积分</span>
                <span>累计 <b>{{ s.callsText }}</b> 次</span>
              </span>
              <svg v-if="s.spark" class="spark" viewBox="0 0 92 24" preserveAspectRatio="none">
                <path :d="s.spark" />
              </svg>
            </div>
            <div v-if="live?.Truncated" class="trunc-note">
              仅显示前 {{ LIVE_LIMIT }} 个会话（共 {{ live?.SessionsTotal }} 个请求中）
            </div>
          </template>
          <el-empty v-else-if="live" description="当前没有请求中的会话" :image-size="60" />
          <el-empty v-else description="实时数据加载中" :image-size="60" />
        </div>

        <div v-else class="live-card">
          <template v-if="userRows.length">
            <div v-for="u in userRows" :key="u.id" class="srow">
              <span class="uavatar">{{ u.initial }}</span>
              <span class="srow-name" :title="u.name">{{ u.name }}</span>
              <span class="srow-stat">
                <span>会话 <b>{{ u.sessions }}</b></span>
                <span>吞吐 <b>{{ u.tpmText }}</b></span>
                <span>消耗 <b>{{ u.tokensText }}</b> Token</span>
                <span>积分 <b>{{ u.spentText }}</b></span>
              </span>
              <svg v-if="u.spark" class="spark" viewBox="0 0 92 24" preserveAspectRatio="none">
                <path :d="u.spark" />
              </svg>
            </div>
            <div v-if="live?.UsersTruncated" class="trunc-note">
              仅显示前 100 个在线用户（共 {{ live?.UsersTotal }} 个）
            </div>
          </template>
          <el-empty v-else-if="live" description="当前没有在线用户" :image-size="60" />
          <el-empty v-else description="实时数据加载中" :image-size="60" />
        </div>
      </section>

      <!-- 更多维度：旧版用量分析里仍有数据支撑的部分，压到最低层级、默认收起 -->
      <el-collapse v-model="moreOpen" class="more-block">
        <el-collapse-item name="more" title="更多维度（Token 构成 / Top 渠道密钥 / 计费模式 / Top 用户）">
          <div class="grid">
            <div class="card">
              <div class="card-title-s">Token 构成</div>
              <VChart :option="compositionOption" height="260px" />
            </div>
            <div class="card">
              <div class="card-title-s">Top 渠道密钥</div>
              <VChart v-if="(data?.Usage.ByKey?.length ?? 0) > 0" :option="keyOption" height="260px" />
              <el-empty v-else description="暂无数据" :image-size="60" />
            </div>
            <div class="card">
              <div class="card-title-s">计费模式分布</div>
              <div class="mode-list">
                <div v-for="m in data?.Usage.ByMode ?? []" :key="m.Key" class="mode-item">
                  <div class="mode-head">
                    <span class="mode-name">{{ modeLabel(m.Key) }}</span>
                    <span class="mode-calls">{{ fmtInt(m.Calls) }} 次</span>
                  </div>
                  <div class="mode-bar">
                    <div
                      class="mode-bar-in"
                      :style="{
                        width:
                          ((m.Calls /
                            Math.max((data?.Usage.ByMode ?? []).reduce((s, x) => s + x.Calls, 0), 1)) *
                            100).toFixed(1) + '%',
                      }"
                    />
                  </div>
                  <div class="mode-foot">
                    <span>{{ fmtCompact(m.Tokens) }} Token</span>
                    <span>{{ fmtMoney(m.Credits) }} 积分</span>
                  </div>
                </div>
                <el-empty v-if="!(data?.Usage.ByMode?.length ?? 0)" description="暂无数据" :image-size="60" />
              </div>
            </div>
            <div v-if="isAdmin" class="card">
              <div class="card-title-s">Top 用户</div>
              <el-table
                :data="(data?.Usage.ByUser ?? []).slice(0, 8)"
                size="small"
                class="table-nowrap"
                empty-text="暂无数据"
              >
                <el-table-column type="index" label="#" width="52" />
                <el-table-column label="用户" min-width="120">
                  <template #default="{ row }">
                    <span class="cell-main">{{ row.Label }}</span>
                    <span v-if="row.SubLabel" class="cell-sub">{{ row.SubLabel }}</span>
                  </template>
                </el-table-column>
                <el-table-column label="请求数" align="right" min-width="90">
                  <template #default="{ row }">{{ fmtInt(row.Calls) }}</template>
                </el-table-column>
                <el-table-column label="失败" align="right" min-width="80">
                  <template #default="{ row }">{{ fmtInt(row.Failed) }}</template>
                </el-table-column>
                <el-table-column label="Token" align="right" min-width="100">
                  <template #default="{ row }">{{ fmtCompact(row.Tokens) }}</template>
                </el-table-column>
                <el-table-column label="积分" align="right" min-width="100">
                  <template #default="{ row }">{{ fmtMoney(row.Credits) }}</template>
                </el-table-column>
              </el-table>
            </div>
          </div>
        </el-collapse-item>
      </el-collapse>

      <!-- 最近公告 -->
      <div v-if="announcements.length" class="announce-card">
        <div class="card-title-s">最近公告</div>
        <div v-for="a in announcements" :key="a.ID" class="announce-item">
          <el-tag :type="(levelType(a.Level) as any)" size="small" effect="light">{{ a.Level }}</el-tag>
          <span class="announce-title">{{ a.Title }}</span>
          <span class="announce-time">{{ a.PublishAt || '立即发布' }}</span>
        </div>
      </div>

      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
    </div>
  </div>
</template>

<style scoped>
/* ===== 顶部提示 ===== */
.live-tag {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  font-weight: 600;
  color: var(--color-success);
  background: #e8f8f0;
  padding: 5px 12px;
  border-radius: 999px;
}
.dot-live {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--color-success);
  box-shadow: 0 0 0 3px rgba(18, 183, 106, 0.18);
  animation: livePulse 1.6s infinite;
}
@keyframes livePulse {
  50% { opacity: 0.4; }
}

/* ===== ① 首屏大数字 ===== */
.big-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 14px;
  margin-bottom: 14px;
}
.big-card {
  position: relative;
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-m);
  padding: 16px 18px;
  box-shadow: var(--shadow-card);
  overflow: hidden;
  min-width: 0;
}
.big-card::after {
  content: '';
  position: absolute;
  right: -22px;
  top: -22px;
  width: 96px;
  height: 96px;
  border-radius: 50%;
  background: var(--brand-primary-soft);
  opacity: 0.55;
  pointer-events: none;
}
.big-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}
.big-glyph {
  width: 22px;
  height: 22px;
  border-radius: 6px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-size: 11px;
  font-weight: 700;
  background: var(--brand-primary-soft);
  color: var(--brand-primary);
  flex-shrink: 0;
}
.big-glyph[data-tone='violet'] { background: #f5f3ff; color: #7c3aed; }
.big-glyph[data-tone='info'] { background: #f0f9ff; color: #0284c7; }
.big-label {
  font-size: 13px;
  color: var(--color-text-secondary);
  white-space: nowrap;
}
.win-tag {
  margin-left: auto;
  font-size: 10px;
  font-weight: 600;
  padding: 2px 9px;
  border-radius: 999px;
  background: var(--brand-primary-soft);
  color: var(--brand-primary);
  cursor: default;
  white-space: nowrap;
}
.win-tag[data-tone='violet'] { background: #f5f3ff; color: #7c3aed; }
.win-tag[data-tone='info'] { background: #f0f9ff; color: #0284c7; }
.big-value {
  font-size: 32px;
  font-weight: 800;
  line-height: 1.15;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
  font-family: 'SF Mono', Consolas, monospace;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.big-value small {
  font-size: 14px;
  font-weight: 600;
  color: var(--color-text-secondary);
  margin-left: 2px;
}
.big-foot {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 8px;
  font-size: 12px;
  color: var(--color-text-tertiary);
  min-width: 0;
}
.big-sub {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.big-delta {
  flex-shrink: 0;
  font-weight: 600;
}
.big-delta.up { color: var(--color-success); }
.big-delta.down { color: var(--color-danger); }

/* ===== ② 小指标行 ===== */
.mini-grid {
  display: grid;
  grid-template-columns: repeat(5, minmax(0, 1fr));
  gap: 10px;
  margin-bottom: 14px;
}
.mini-card {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: 10px;
  padding: 10px 14px;
  box-shadow: var(--shadow-card);
  min-width: 0;
}
.mini-label {
  font-size: 12px;
  color: var(--color-text-secondary);
  white-space: nowrap;
}
.mini-value {
  font-size: 17px;
  font-weight: 700;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
  font-family: 'SF Mono', Consolas, monospace;
  white-space: nowrap;
}
.mini-value[data-tone='ok'] { color: var(--color-success); }
.mini-value[data-tone='info'] { color: #0284c7; }

/* ===== ③ 图表区 ===== */
.chart-stack {
  display: grid;
  grid-template-columns: 1fr;
  gap: 14px;
  margin-bottom: 14px;
}
.card {
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-m);
  padding: 14px 16px;
  box-shadow: var(--shadow-card);
}
.card-head {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 12px;
  flex-wrap: wrap;
}
.card-title {
  margin: 0;
  font-size: 14px;
  font-weight: 700;
  color: var(--color-text);
}
.card-title-s {
  font-size: 14px;
  font-weight: 600;
  color: var(--color-text);
  margin-bottom: 10px;
}
.win-chip,
.gran-chip {
  font-size: 10px;
  font-weight: 600;
  padding: 2px 9px;
  border-radius: 999px;
  white-space: nowrap;
}
.win-chip { background: var(--brand-primary-soft); color: var(--brand-primary); }
.gran-chip { background: #f2f4f7; color: var(--color-text-secondary); }
.trend-legend {
  display: flex;
  gap: 12px;
  font-size: 11px;
  color: var(--color-text-tertiary);
  flex-wrap: wrap;
}
.trend-legend i {
  display: inline-block;
  width: 10px;
  height: 10px;
  border-radius: 2px;
  margin-right: 4px;
  vertical-align: -1px;
}
.legend-empty { color: var(--color-text-tertiary); }
.head-actions { margin-left: auto; }

/* 模型广场 */
.sort-group {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-left: auto;
  flex-wrap: wrap;
}
.sort-label {
  font-size: 11px;
  color: var(--color-text-tertiary);
}
.sort-chip {
  border: none;
  background: transparent;
  font-size: 11px;
  font-weight: 600;
  color: var(--color-text-tertiary);
  padding: 2px 8px;
  border-radius: 999px;
  cursor: pointer;
  transition: background 0.15s ease, color 0.15s ease;
}
.sort-chip:hover { color: var(--brand-primary); }
.sort-chip.on { background: var(--brand-primary-soft); color: var(--brand-primary); }
.model-grid {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 10px;
}
.model-card {
  background: #fafbfd;
  border: 1px solid var(--color-border);
  border-radius: 10px;
  padding: 12px 14px;
  min-width: 0;
}
.model-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  margin-bottom: 8px;
}
.model-name {
  font-size: 13px;
  font-weight: 700;
  color: var(--color-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.model-state {
  font-size: 10px;
  font-weight: 600;
  padding: 2px 8px;
  border-radius: 999px;
  background: #f0f1f4;
  color: var(--color-text-tertiary);
  flex-shrink: 0;
}
.model-state[data-active='true'] {
  background: #e8f8f0;
  color: var(--color-success);
}
.model-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  font-size: 12px;
  color: var(--color-text-secondary);
  padding: 3px 0;
}
.model-row b {
  color: var(--color-text);
  font-weight: 600;
  font-variant-numeric: tabular-nums;
  font-family: 'SF Mono', Consolas, monospace;
  white-space: nowrap;
}
.fail-rate {
  margin-left: 5px;
  font-style: normal;
  font-weight: 500;
  font-size: 11px;
  color: var(--color-text-tertiary);
}
.model-bar {
  height: 5px;
  border-radius: 3px;
  background: var(--color-border);
  margin-top: 8px;
  overflow: hidden;
}
.model-bar i {
  display: block;
  height: 100%;
  border-radius: 3px;
  transition: width 0.3s ease;
}
.copy-btn {
  border: none;
  margin-left: 5px;
  width: 16px;
  height: 16px;
  border-radius: 4px;
  background: var(--brand-primary-soft);
  color: var(--brand-primary);
  font-size: 10px;
  line-height: 1;
  cursor: pointer;
  vertical-align: 1px;
}
.copy-btn:hover { background: var(--brand-primary); color: #fff; }
.idle-note {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  margin-top: 10px;
  font-size: 11px;
  color: var(--color-text-tertiary);
}
.link-btn {
  border: none;
  background: transparent;
  padding: 0;
  font-size: 11px;
  color: var(--brand-primary);
  cursor: pointer;
  text-decoration: underline;
}
.trunc-note {
  font-size: 11px;
  color: var(--color-text-tertiary);
  margin-top: 8px;
}

/* ===== ④ 实时区 ===== */
.live-block {
  background: linear-gradient(160deg, #eef0ff, #f8fafc);
  border: 1px solid #dde1f5;
  border-radius: var(--radius-m);
  padding: 14px 16px;
  margin-bottom: 14px;
}
.live-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 10px;
  flex-wrap: wrap;
}
.live-title {
  margin: 0;
  font-size: 14px;
  font-weight: 700;
  color: var(--color-text);
}
.live-window {
  margin-left: auto;
  font-size: 12px;
  color: var(--color-text-tertiary);
}
.live-loading { margin-left: 8px; color: var(--brand-primary); }
.live-tabs {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 10px;
  flex-wrap: wrap;
}
.live-err {
  font-size: 11px;
  color: var(--color-danger);
}
.live-card {
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: 10px;
  padding: 12px 14px;
  min-width: 0;
}
.srow {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 9px 0;
  border-bottom: 1px dashed var(--color-border);
  flex-wrap: wrap;
}
.srow:last-of-type { border-bottom: none; }
.srow-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--color-success);
  box-shadow: 0 0 0 3px rgba(18, 183, 106, 0.18);
  animation: livePulse 1.6s infinite;
  flex-shrink: 0;
}
.uavatar {
  width: 26px;
  height: 26px;
  border-radius: 50%;
  background: var(--brand-primary-soft);
  color: var(--brand-primary);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-size: 11px;
  font-weight: 700;
  flex-shrink: 0;
}
.srow-name {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text);
  min-width: 110px;
  max-width: 170px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.model-chip {
  font-size: 10px;
  font-weight: 600;
  padding: 1px 8px;
  border-radius: 999px;
  background: var(--brand-primary-soft);
  color: var(--brand-primary);
  white-space: nowrap;
}
.srow-stat {
  display: flex;
  gap: 14px;
  font-size: 12px;
  color: var(--color-text-secondary);
  flex-wrap: wrap;
}
.srow-stat b {
  color: var(--color-text);
  font-weight: 600;
  font-variant-numeric: tabular-nums;
  font-family: 'SF Mono', Consolas, monospace;
}
.spark {
  width: 92px;
  height: 24px;
  flex-shrink: 0;
  margin-left: auto;
}
.spark path {
  fill: none;
  stroke: var(--brand-primary);
  stroke-width: 1.8;
}
.pending-note {
  margin: 8px 0 0;
  font-size: 11px;
  line-height: 1.5;
  color: var(--color-text-tertiary);
}

/* ===== 更多维度 ===== */
.more-block {
  border: none;
  margin-bottom: 14px;
}
.more-block :deep(.el-collapse-item__header) {
  background: transparent;
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text-secondary);
  border-bottom: 1px solid var(--color-border);
  padding-left: 2px;
}
.more-block :deep(.el-collapse-item__wrap) {
  background: transparent;
  border-bottom: none;
  padding-top: 12px;
}
.grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 14px;
}
.mode-list {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding-top: 4px;
}
.mode-item {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.mode-head {
  display: flex;
  justify-content: space-between;
  font-size: 13px;
}
.mode-name { font-weight: 600; color: var(--color-text); }
.mode-calls { color: var(--color-text-secondary); }
.mode-bar {
  height: 8px;
  border-radius: 6px;
  background: var(--color-border);
  overflow: hidden;
}
.mode-bar-in {
  height: 100%;
  border-radius: 6px;
  background: var(--brand-gradient);
  transition: width 0.4s ease;
}
.mode-foot {
  display: flex;
  justify-content: space-between;
  font-size: 12px;
  color: var(--color-text-secondary);
}
.cell-main { color: var(--color-text); }
.cell-sub {
  margin-left: 8px;
  font-size: 12px;
  color: var(--color-text-tertiary);
}

/* ===== 最近公告 ===== */
.announce-card {
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-m);
  padding: 14px 16px;
  box-shadow: var(--shadow-card);
  margin-bottom: 14px;
}
.announce-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 9px 0;
  border-bottom: 1px solid var(--color-border);
}
.announce-item:last-child { border-bottom: none; }
.announce-title {
  flex: 1;
  font-weight: 500;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.announce-time {
  color: var(--color-text-secondary);
  font-size: 12px;
}

/* ===== 响应式 ===== */
@media (max-width: 1280px) {
  .model-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
}
@media (max-width: 1100px) {
  .mini-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); }
}
@media (max-width: 1000px) {
  .big-grid { grid-template-columns: 1fr; }
  .grid { grid-template-columns: 1fr; }
}
@media (max-width: 760px) {
  .mini-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .model-grid { grid-template-columns: 1fr; }
  .head-actions, .sort-group { margin-left: 0; }
  .live-window { margin-left: 0; }
}
</style>
