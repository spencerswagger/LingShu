<script setup lang="ts">
// 统计页（admin 全局 / dev 本人共用），版式按 spencer 定稿 demo（stats-demo-pc / stats-demo）落地：
//   ① 首屏 3 个大数字（总 Token / 总消费积分 / 吞吐量，各带窗口标签）
//   ② 一条小指标行（总请求数 / 成功率 / 平均耗时 / 请求中会话 / 请求中用户）
//   ③ 上下两张图：24h 用量趋势（今天/昨天/前天/大前天四线、按小时、缺数据断线）+ 模型广场
//   ④ 底部实时会话区：我的会话 + 在线用户（30s 定时刷新框架）
//
// 本轮只做前端布局与组件结构，不接后端新接口：
//   - 小时粒度趋势、按模型吞吐/首字/全程、实时会话/在线用户 均无接口 → 统一用「--」或骨架占位，
//     绝不写死假数据冒充真实值；
//   - 统计口径与接口契约不变（仍只调 /admin|/dev/stats/dashboard）。
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import type { EChartsCoreOption } from 'echarts/core'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '@/stores/auth'
import { getStatsDashboard, type StatsDashboard, type StatsTopItem } from '@/api/stats'
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
// 聚合粒度仍由后端按天返回；粒度联动（小时）待接口。
type Preset = '24h' | '7' | '30' | '90' | 'custom'
const preset = ref<Preset>('24h')
const customRange = ref<[string, string] | null>(null)

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

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const { from, to } = resolveRange()
    const res = await getStatsDashboard(auth.role, {
      From: toRFC3339CN(from),
      To: toRFC3339CN(to),
    })
    data.value = res.Data
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
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

// ===== 实时会话（30s 定时刷新框架）=====
// 数据层待后端接口：本轮 liveSessions / liveUsers 恒为空集合，模板统一走骨架占位分支。
// 定时器只在 onMounted 建一次、onUnmounted 清理；回调里不读写 preset / customRange / data，
// 保证筛选条件变化（或定时触发）都不会重置用户选的范围。
interface LiveRow {
  id: string
  name: string
  ctx: string
  round: string
  tpm: string
  spent: string
  spark: string
  ready: boolean
}

interface LiveSessionDTO {
  ID: string
  Name: string
  ContextUsed: number
  ContextLimit: number
  RoundTokens: number
  TPM: number
  SpentCredits: number
  Spark: number[]
}

interface LiveUserDTO {
  ID: string
  Name: string
  TPM: number
  Tokens: number
  Spark: number[]
}

const liveSessions = ref<LiveSessionDTO[]>([])
const liveUsers = ref<LiveUserDTO[]>([])
const liveRefreshing = ref(false)
const LIVE_REFRESH_MS = 30000

async function loadRealtime() {
  // TODO(后端实时接口就绪后)：取「请求中会话 / 在线用户」并写入 liveSessions / liveUsers。
  // 刻意不触碰 preset / customRange / data。
  liveRefreshing.value = true
  try {
    // 接口未就绪：保留空集合 → 模板显示骨架占位。
  } finally {
    liveRefreshing.value = false
  }
}

let liveTimer: number | undefined

onMounted(() => {
  load()
  loadAnnouncements()
  liveTimer = window.setInterval(loadRealtime, LIVE_REFRESH_MS)
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
const fmtDelta = (n: number) => `${n >= 0 ? '▲' : '▼'} ${Math.abs(Number(n || 0) * 100).toFixed(1)}%`

// ===== 图表主题 =====
// 与定稿 demo 同源：主靛蓝 + 四代日线渐变。
const C = {
  primary: '#4f46e5',
  primarySoft: '#eef0ff',
  violet: '#7c3aed',
  cyan: '#06b6d4',
  text: '#101828',
  sub: '#667085',
  grid: '#eef0f6',
  ok: '#059669',
  okSoft: '#ecfdf5',
  info: '#0284c7',
  infoSoft: '#f0f9ff',
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

// ===== ① 首屏大数字（每个都带窗口标签；三处吞吐量窗口刻意区分）=====
// 窗口口径（定稿文案）：
//   首屏吞吐量      = 近 1 分钟
//   模型广场吞吐列  = 近 10 分钟
//   会话/在线用户吞吐 = 近 60 分钟
const TPM_WINDOW_KPI = '近 1 分钟'
const TPM_WINDOW_MODEL = '近 10 分钟'
const TPM_WINDOW_LIVE = '近 60 分钟'

// 吞吐量（RPM/TPM）现有口径：分子 BusyRequests、分母 SumDurationMS 均只取「成功且带耗时」的请求。
// 有数据即显示；样本少时只在副标灰字标注，不做硬门槛。
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
      winTip: '窗口 = 当前统计范围（含缓存读单列，输出含推理）',
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
      value: throughput.value.ready ? fmtCompact(k?.TPM ?? 0) : '--',
      unit: ' TPM',
      sub: throughput.value.ready
        ? `请求 ${(k?.RPM ?? 0).toFixed(1)} RPM${throughput.value.lowSample ? ' · 样本较少' : ''}`
        : '实时窗口接口待接入',
      winLabel: TPM_WINDOW_KPI,
      winTip:
        '窗口口径：近 1 分钟。当前展示统计区间内「成功且有耗时请求」的忙时折算值（≈60÷平均耗时），近 1 分钟实时窗口待接口',
    },
  ]
})

// ===== ② 小指标行 =====
interface MiniStat {
  label: string
  value: string
  tone?: 'ok' | 'info'
  tip?: string
  pending?: boolean
}

const miniStats = computed<MiniStat[]>(() => {
  const k = data.value?.KPI
  return [
    { label: '总请求数', value: k ? fmtInt(k.TotalRequests) : '--' },
    { label: '成功率', value: k ? fmtPct(k.SuccessRate) : '--', tone: 'ok' },
    { label: '平均耗时', value: k ? fmtMs(k.AvgDurationMS) : '--' },
    {
      label: '请求中会话',
      value: '--',
      tone: 'info',
      pending: true,
      tip: '实时会话接口待接入：本轮仅保留位与骨架占位',
    },
    {
      label: '请求中用户',
      value: '--',
      tone: 'info',
      pending: true,
      tip: '实时在线用户接口待接入：本轮仅保留位与骨架占位',
    },
  ]
})

// ===== ③ 24h 用量趋势（四代日线 / 按小时 / 缺数据断线）=====
const DAY_LEGEND = [
  { name: '今天', color: '#4f46e5', width: 2.8 },
  { name: '昨天', color: '#a5b4fc', width: 2 },
  { name: '前天', color: '#c4b5fd', width: 2 },
  { name: '大前天', color: '#e9d5ff', width: 2 },
]
const hourAxis = Array.from({ length: 25 }, (_, i) => `${String(i).padStart(2, '0')}:00`)
const trendMetric = ref<'tokens' | 'credits' | 'calls'>('tokens')
// 小时粒度接口未就绪：series 全 null，只画坐标轴与图例（占位，不编造曲线）。
const hourlyPending = ref(true)

const trendMetricName = computed(() =>
  trendMetric.value === 'tokens' ? 'Token' : trendMetric.value === 'credits' ? '积分' : '请求数',
)

const trendOption = computed<EChartsCoreOption>(() => ({
  grid: { left: 8, right: 16, top: 28, bottom: 4, containLabel: true },
  legend: { show: false },
  tooltip: { ...tooltipCommon, trigger: 'axis' },
  xAxis: {
    type: 'category',
    boundaryGap: false,
    data: hourAxis,
    ...axisCommon,
    axisLabel: { color: C.sub, fontSize: 11, interval: 5 },
  },
  yAxis: {
    type: 'value',
    name: trendMetricName.value,
    nameTextStyle: { color: C.sub, fontSize: 11, align: 'left' },
    splitLine: { lineStyle: { color: C.grid } },
    axisLabel: { color: C.sub, fontSize: 11, formatter: (v: number) => fmtCompact(v) },
  },
  series: DAY_LEGEND.map((d) => ({
    name: d.name,
    type: 'line',
    smooth: true,
    showSymbol: false,
    // 缺数据断线：接口就绪后把真实点填进来即可，null 处自动断开
    connectNulls: false,
    data: Array<number | null>(hourAxis.length).fill(null),
    lineStyle: { width: d.width, color: d.color },
    itemStyle: { color: d.color },
  })),
}))

const trendNote = computed(() => {
  const base = '按小时聚合待后端接口：本轮只渲染坐标轴与图例，不填充曲线（占位）。'
  if (preset.value === '24h') return base
  return `${base} 趋势图固定近 24h 窗口，暂不随「${rangeText.value}」联动。`
})

// ===== ③ 模型广场 =====
type ModelSort = 'tokens' | 'tpm' | 'first' | 'total'
const modelSort = ref<ModelSort>('tokens')
const modelSortOptions: { key: ModelSort; label: string; ready: boolean; tip: string }[] = [
  { key: 'tokens', label: 'Token', ready: true, tip: '' },
  { key: 'tpm', label: '吞吐', ready: false, tip: '按模型近 10 分钟吞吐待接口' },
  { key: 'first', label: '首字', ready: false, tip: '按模型首字耗时待接口' },
  { key: 'total', label: '全程', ready: false, tip: '按模型全程耗时待接口' },
]

interface ModelRow {
  name: string
  tokens: number
  calls: number
  active: boolean
  tokensText: string
  callsText: string
  tpmText: string
  latencyText: string
  barPct: number
  color: string
}

const modelRows = computed<ModelRow[]>(() => {
  const items = data.value?.Usage.ByModel ?? []
  if (!items.length) return []
  const max = Math.max(...items.map((i) => i.Tokens), 1)
  return items.map((i, idx) => ({
    name: i.Label || i.Key || '(未命名模型)',
    tokens: i.Tokens,
    calls: i.Calls,
    active: i.Calls > 0,
    tokensText: fmtCompact(i.Tokens),
    callsText: `${fmtInt(i.Calls)} / ${fmtInt(i.Failed)}`,
    // 按模型的吞吐 / 首字 / 全程：接口未就绪，占位（不填假值）
    tpmText: '--',
    latencyText: '-- / --',
    barPct: Math.round((i.Tokens / max) * 100),
    color: C.series[idx % C.series.length],
  }))
})

const sortedModels = computed<ModelRow[]>(() => {
  // 排序维度里只有 Token 有真实数据；吞吐 / 首字 / 全程 按钮置灰，
  // 接口就绪后在此按 modelSort 分发真实排序 key（不必改模板）。
  return modelRows.value.slice().sort((a, b) => b.tokens - a.tokens)
})

const activeModels = computed(() => sortedModels.value.filter((m) => m.active))
const idleModels = computed(() => sortedModels.value.filter((m) => !m.active))
const idleExpanded = ref(false)
const visibleModels = computed(() => (idleExpanded.value ? sortedModels.value : activeModels.value))

async function copyModelName(name: string) {
  try {
    await navigator.clipboard.writeText(name)
    ElMessage.success(`已复制模型名：${name}`)
  } catch {
    ElMessage.warning('复制失败，请手动选择模型名')
  }
}

// ===== ④ 实时会话行（骨架占位 → 接口就绪后替换数据源，不动模板）=====
const SPARK_PLACEHOLDER = 'M2,20 L22,18 L42,19 L62,14 L82,16 L90,12'

function sparkPath(vals: number[]): string {
  if (!vals.length) return SPARK_PLACEHOLDER
  const w = 90
  const h = 26
  const max = Math.max(...vals)
  const min = Math.min(...vals)
  const span = max - min || 1
  const step = vals.length > 1 ? w / (vals.length - 1) : w
  return vals
    .map((v, i) => `${(2 + i * step).toFixed(1)},${(h - 3 - ((v - min) / span) * (h - 8)).toFixed(1)}`)
    .join(' L')
    .replace(/^/, 'M')
}

function placeholderRows(n: number): LiveRow[] {
  return Array.from({ length: n }, (_, i) => ({
    id: `ph-${i}`,
    name: '——',
    ctx: '--/--',
    round: '--',
    tpm: '--',
    spent: '--',
    spark: SPARK_PLACEHOLDER,
    ready: false,
  }))
}

const mySessionRows = computed<LiveRow[]>(() => {
  if (!liveSessions.value.length) return placeholderRows(2)
  return liveSessions.value.map((s) => ({
    id: s.ID,
    name: s.Name,
    ctx: `${fmtCompact(s.ContextUsed)}/${fmtCompact(s.ContextLimit)}`,
    round: fmtCompact(s.RoundTokens),
    tpm: `${s.TPM.toFixed(1)} TPM`,
    spent: fmtMoney(s.SpentCredits),
    spark: sparkPath(s.Spark),
    ready: true,
  }))
})

const onlineUserRows = computed<LiveRow[]>(() => {
  if (!liveUsers.value.length) return placeholderRows(2)
  return liveUsers.value.map((u) => ({
    id: u.ID,
    name: u.Name,
    ctx: '',
    round: '',
    tpm: `${u.TPM.toFixed(1)} TPM`,
    spent: fmtCompact(u.Tokens),
    spark: sparkPath(u.Spark),
    ready: true,
  }))
})

// ===== 更多维度（旧版用量分析里仍有效的部分，压低到最低层级、默认收起）=====
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
      <el-button size="small" :loading="loading" @click="load">刷新</el-button>
      <span class="live-tag"><span class="dot-live" />每 30s 自动刷新</span>
    </div>

    <div v-loading="loading">
      <!-- ① 首屏：3 个大数字，每个带窗口标签 -->
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
            <span class="mini-value" :data-tone="m.tone" :class="{ pending: m.pending }">{{ m.value }}</span>
          </el-tooltip>
          <span v-else class="mini-value" :data-tone="m.tone">{{ m.value }}</span>
        </div>
      </div>

      <!-- ③ 两张图：24h 用量趋势 + 模型广场 -->
      <div class="chart-stack">
        <div class="card">
          <div class="card-head">
            <h3 class="card-title">用量趋势</h3>
            <span class="win-chip">近 24h</span>
            <div class="trend-legend">
              <span v-for="d in DAY_LEGEND" :key="d.name">
                <i :style="{ background: d.color }" />{{ d.name }}
              </span>
            </div>
            <el-radio-group v-model="trendMetric" size="small" class="head-actions">
              <el-radio-button value="tokens">Token</el-radio-button>
              <el-radio-button value="credits">积分</el-radio-button>
              <el-radio-button value="calls">请求数</el-radio-button>
            </el-radio-group>
          </div>
          <VChart :option="trendOption" height="260px" />
          <p v-if="hourlyPending" class="pending-note">{{ trendNote }}</p>
        </div>

        <div class="card">
          <div class="card-head">
            <h3 class="card-title">模型广场</h3>
            <span class="win-chip">{{ TPM_WINDOW_MODEL }}</span>
            <div class="sort-group">
              <span class="sort-label">排序：</span>
              <template v-for="o in modelSortOptions" :key="o.key">
                <el-tooltip v-if="!o.ready" :content="o.tip" placement="top" :show-after="150">
                  <button type="button" class="sort-chip off" disabled>{{ o.label }}</button>
                </el-tooltip>
                <button
                  v-else
                  type="button"
                  class="sort-chip"
                  :class="{ on: modelSort === o.key }"
                  @click="modelSort = o.key"
                >
                  {{ o.label }}
                </button>
              </template>
              <span class="sort-hint" v-if="modelSortOptions.some((o) => !o.ready)">吞吐/首字/全程待接口</span>
            </div>
          </div>
          <div v-if="visibleModels.length" class="model-grid">
            <div v-for="m in visibleModels" :key="m.name" class="model-card">
              <div class="model-head">
                <span class="model-name">
                  {{ m.name }}
                  <button type="button" class="copy-btn" title="复制模型名" @click="copyModelName(m.name)">⧉</button>
                </span>
                <span class="model-state" :data-active="m.active">{{ m.active ? '活跃' : '空闲' }}</span>
              </div>
              <div class="model-row"><span>Token 消耗</span><b>{{ m.tokensText }}</b></div>
              <div class="model-row">
                <span>吞吐量</span><b class="pending">{{ m.tpmText }}</b>
              </div>
              <div class="model-row"><span>请求 / 失败</span><b>{{ m.callsText }}</b></div>
              <div class="model-row">
                <span>首字 / 全程</span><b class="pending">{{ m.latencyText }}</b>
              </div>
              <div class="model-bar"><i :style="{ width: m.barPct + '%', background: m.color }" /></div>
            </div>
          </div>
          <el-empty v-else description="暂无模型数据" :image-size="60" />
          <div v-if="idleModels.length" class="idle-note">
            另有 {{ idleModels.length }} 个模型当前无请求，
            <button type="button" class="link-btn" @click="idleExpanded = !idleExpanded">
              {{ idleExpanded ? '收起' : '已折叠' }}
            </button>
          </div>
        </div>
      </div>

      <!-- ④ 实时会话区（数据待接口：骨架占位 + 30s 刷新框架） -->
      <section class="live-block">
        <div class="live-head">
          <h3 class="live-title">实时会话</h3>
          <span class="live-tag"><span class="dot-live" />每 30s 自动刷新 · 仅显示请求中</span>
          <span v-if="liveRefreshing" class="live-window">刷新中…</span>
          <span class="live-window">窗口 {{ TPM_WINDOW_LIVE }}</span>
        </div>
        <div class="live-grid">
          <div class="live-card">
            <div class="live-card-head">
              <span class="live-card-title">我的会话</span>
              <span class="live-card-count">
                {{ liveSessions.length ? `${liveSessions.length} 个请求中` : '数据待接入' }}
              </span>
            </div>
            <div v-for="r in mySessionRows" :key="r.id" class="srow">
              <span class="srow-dot" :class="{ pending: !r.ready }" />
              <span class="srow-name" :class="{ pending: !r.ready }">{{ r.name }}</span>
              <span class="srow-stat">
                <span>上下文 <b :class="{ pending: !r.ready }">{{ r.ctx }}</b></span>
                <span>本轮 <b :class="{ pending: !r.ready }">{{ r.round }}</b></span>
                <span>吞吐 <b :class="{ pending: !r.ready }">{{ r.tpm }}</b></span>
                <span>已花 <b :class="{ pending: !r.ready }">{{ r.spent }}</b> 积分</span>
              </span>
              <svg class="spark" viewBox="0 0 96 26" preserveAspectRatio="none">
                <path :d="r.spark" :class="{ 'spark-ph': !r.ready }" />
              </svg>
            </div>
            <p v-if="!liveSessions.length" class="pending-note">
              实时会话接口待接入：本轮只做版式与 30s 刷新框架。
            </p>
          </div>

          <div v-if="isAdmin" class="live-card">
            <div class="live-card-head">
              <span class="live-card-title">在线用户</span>
              <span class="live-card-count">
                {{ liveUsers.length ? `${liveUsers.length} 个请求中` : '数据待接入' }}
              </span>
            </div>
            <div v-for="r in onlineUserRows" :key="r.id" class="srow">
              <span class="uavatar" :class="{ pending: !r.ready }">{{ r.ready ? r.name.slice(0, 2) : '?' }}</span>
              <span class="srow-name" :class="{ pending: !r.ready }">{{ r.name }}</span>
              <span class="srow-stat">
                <span>吞吐 <b :class="{ pending: !r.ready }">{{ r.tpm }}</b></span>
                <span>消耗 <b :class="{ pending: !r.ready }">{{ r.spent }}</b></span>
              </span>
              <svg class="spark" viewBox="0 0 96 26" preserveAspectRatio="none">
                <path :d="r.spark" :class="{ 'spark-ph': !r.ready }" />
              </svg>
            </div>
            <p v-if="!liveUsers.length" class="pending-note">
              在线用户接口待接入：只汇总吞吐/消耗，不展示会话内容。
            </p>
          </div>
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
.win-chip {
  font-size: 10px;
  font-weight: 600;
  padding: 2px 9px;
  border-radius: 999px;
  background: var(--brand-primary-soft);
  color: var(--brand-primary);
  white-space: nowrap;
}
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
.head-actions {
  margin-left: auto;
}

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
.sort-chip.off { cursor: not-allowed; opacity: 0.55; }
.sort-hint {
  font-size: 10px;
  color: var(--color-text-tertiary);
}
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

/* ===== ④ 实时会话区 ===== */
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
  margin-bottom: 12px;
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
.live-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 10px;
}
.live-card {
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: 10px;
  padding: 12px 14px;
  min-width: 0;
}
.live-card-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}
.live-card-title {
  font-size: 13px;
  font-weight: 700;
  color: var(--color-text);
}
.live-card-count {
  margin-left: auto;
  font-size: 11px;
  color: var(--color-text-tertiary);
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
.srow-dot.pending {
  background: var(--color-border-strong);
  box-shadow: none;
  animation: none;
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
  max-width: 160px;
  overflow: hidden;
  text-overflow: ellipsis;
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
  width: 90px;
  height: 26px;
  flex-shrink: 0;
  margin-left: auto;
}
.spark path {
  fill: none;
  stroke: var(--brand-primary);
  stroke-width: 1.8;
}
.spark path.spark-ph {
  stroke: var(--color-border-strong);
  stroke-dasharray: 3 3;
}
.pending { color: var(--color-text-tertiary) !important; }

/* 占位说明（一眼可辨：本轮数据待接口，不是真实值） */
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
  .live-grid { grid-template-columns: 1fr; }
  .grid { grid-template-columns: 1fr; }
}
@media (max-width: 760px) {
  .mini-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .model-grid { grid-template-columns: 1fr; }
  .head-actions, .sort-group { margin-left: 0; }
}
</style>
