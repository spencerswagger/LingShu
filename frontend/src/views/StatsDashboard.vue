<script setup lang="ts">
// 统计页（admin 全局 / dev 本人共用），版式按定稿 demo，数据接后端真实接口：
//   ① 首屏 3 个大数字（总 Token / 总消费积分 / 吞吐量）+ 5 项小指标
//   ② 24h 四日对比趋势（今天/昨天/前天/大前天，按 HourOfDay 对齐，缺桶断线）
//   ③ 模型广场（后端按 Token 倒序 + 截断，含零用量模型供折叠；无排序控件、无进度条）
//   ④ 实时区（/stats/live，30s 刷新）：我的会话（仅请求中）/ 范围内活跃用户，均为卡片网格
//
// 三个吞吐口径刻意区分（见 docs/designs/2026-10-25-stats-dashboard-backend.md §4）：
//   首屏      = KPI.TPM            全窗口忙时口径（≈60÷同批请求平均耗时）
//   模型广场  = List[].TPM         按模型忙时口径
//   实时区    = Sessions/Users.TPM 近 60 分钟墙钟平均（含排队空转，天然更低）
// 三处吞吐量与 RPM 一律走 @/utils/format 的 fmtCompact（K/M 缩写，同一实现）。
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
  type StatsLiveExited,
  type StatsModelSort,
  type StatsTopItem,
  type StatsModelStat,
  type StatsModelPlaza,
} from '@/api/stats'
import { listAnnouncements, type AdminAnnouncement } from '@/api/admin'
import { listDevAnnouncements } from '@/api/dev'
import { fmtCompact, toRFC3339CN } from '@/utils/format'
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

// ===== 模型广场（v4 第 3 条：只留数据列 + 复制 + 空闲折叠；排序仍可切，后端白名单 tokens|tpm|failed|latency）=====
const MODEL_LIMIT = 50
const COMPARE_DAYS = 4
// 首屏 KPI / 小时趋势 / 模型广场都按「请求区间的严格滚动窗口」算（后端默认 exact）。
const WINDOW_MODE: 'exact' | 'calendar' = 'exact'

const modelSort = ref<StatsModelSort>('tokens')
const MODEL_SORT_OPTIONS: { value: StatsModelSort; label: string }[] = [
  { value: 'tokens', label: 'Token' },
  { value: 'tpm', label: '吞吐' },
  { value: 'failed', label: '失败' },
  { value: 'latency', label: '耗时' },
]
// 排序切换只重取 dashboard（实时区不受影响），静默刷新不闪整页 loading。
watch(modelSort, () => load({ silent: true }))

// 请求序号守卫：筛选切换、手动刷新、30s 轮询可能在途交叠，
// 只接受「最新一次」请求的响应，过期响应直接丢弃（不写 store、不闪界面）。
let dashSeq = 0
let dashInFlight = false

// silent：定时刷新用，不显示整页 loading
async function load(opts: { silent?: boolean } = {}) {
  const seq = ++dashSeq
  dashInFlight = true
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
      Window: WINDOW_MODE,
    })
    if (seq !== dashSeq) return // 已有更新的请求发出，丢弃本次过期响应
    data.value = res.Data
  } catch (e: any) {
    if (seq !== dashSeq) return
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    if (seq === dashSeq) {
      dashInFlight = false
      loading.value = false
    }
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

// 实时区同样加序号守卫：轮询 / 手动刷新交叠时只认最新响应。
let liveSeq = 0
let liveInFlight = false
// 单飞 + 单份待取：「前一发未回不发下一个」，但期间来的请求只记一次「还欠一轮」，
// 目前在飞的那发回来后立刻补取，既不并发也不丢最后一次请求（例如刚切了时间范围）。
let livePending = false

async function loadLive() {
  if (liveInFlight) {
    livePending = true
    return
  }
  const seq = ++liveSeq
  liveInFlight = true
  liveLoading.value = true
  try {
    // 在线用户按页面右上角时间范围筛选：必须带同一份 From/To，
    // 否则后端退回默认近 24h，卡片与「统计范围」标签就对不上了。
    const { from, to } = resolveRange()
    const res = await getStatsLive(auth.role, {
      From: toRFC3339CN(from),
      To: toRFC3339CN(to),
      Window: LIVE_WINDOW_MINUTES,
      SparkPoints: LIVE_SPARK_POINTS,
      Limit: LIVE_LIMIT,
    })
    if (seq !== liveSeq) return // 过期响应直接丢弃
    live.value = res.Data
    // 淡出项与主列表同一批数据一起同步：该进来的进、已回到请求中的撤掉、过期的删掉
    syncFading(res.Data?.RecentlyExited, res.Data?.FadeWindowSeconds)
    liveErr.value = ''
  } catch (e: any) {
    if (seq !== liveSeq) return
    // 实时区失败不打断页面，也不清空上一次的数据（只标记失败）。
    liveErr.value = e?.message || '实时数据获取失败'
  } finally {
    if (seq === liveSeq) {
      liveInFlight = false
      liveLoading.value = false
      if (livePending) {
        livePending = false
        void loadLive()
      }
    }
  }
}

// ===== 淡出（离开中）卡片 =====
// 服务端口径：Sessions 只含**请求中**的会话；刚结束的会话进 RecentlyExited，只做淡出展示。
// 前端做法：把淡出项与主列表**合并成同一个 keyed 列表**渲染（key 都是会话 ID），
// 所以「从请求中消失 + 同时出现在 RecentlyExited」是同一个 DOM 节点就地变暗，
// 不会出现「原地消失 + 别处新冒出一个」。淡出项到 ExitedAt + FadeWindowSeconds 就从 DOM 移除，
// 不靠本地状态长期保留（服务端下一轮也不再返回）。
const FADE_OUT_MS = 400 // 收尾淡出时长：到点前这 400ms 把不透明度降到底，随后移除节点
const FADE_WINDOW_DEFAULT = 300

interface FadingCard {
  id: string
  name: string
  model: string
  exitedAtMs: number // ExitedAt 解析值（解析失败退化为收到响应的时刻）
  deadline: number // exitedAtMs + FadeWindowSeconds（到点移除）
  leaving: boolean // 进入最后 400ms：opacity → 0
  leftText: string // 「4:12」剩余时间
}

const fadingCards = ref<FadingCard[]>([])
// 淡出窗口秒数：读后端下发的 FadeWindowSeconds，缺省 300
const fadeWindowSeconds = computed(() => {
  const v = Number(live.value?.FadeWindowSeconds)
  return Number.isFinite(v) && v > 0 ? v : FADE_WINDOW_DEFAULT
})

// 淡出卡片显示「最后一次实时那一版」的数据（快照），这样它变暗时看起来还是同一张卡，
// 而不是统计值突然变成一堆「—」。快照只用于渲染，不参与任何计数。
const lastLiveRows = new Map<string, LiveSessionRow>()

function parseIsoMs(iso: string | undefined, fallback: number): number {
  const t = iso ? Date.parse(iso) : NaN
  return Number.isFinite(t) ? t : fallback
}
function fmtRemain(ms: number): string {
  const s = Math.max(0, Math.ceil(ms / 1000))
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
}

// 每秒推进一次：更新剩余秒数、进入收尾淡出、到点移除。
function tickFading(now = Date.now()) {
  if (!fadingCards.value.length) return
  const next: FadingCard[] = []
  for (const c of fadingCards.value) {
    const remain = c.deadline - now
    if (remain <= 0) continue // 到点：直接移除（服务端也不再返回）
    const leaving = remain <= FADE_OUT_MS
    const leftText = fmtRemain(remain)
    if (leaving === c.leaving && leftText === c.leftText) {
      next.push(c) // 无变化就不换对象，少一次渲染
      continue
    }
    next.push({ ...c, leaving, leftText })
  }
  fadingCards.value = next
}

// 每轮实时响应后同步淡出集合（幂等：同一份数据重复同步不会抖动）。
function syncFading(list: StatsLiveExited[] | null | undefined, windowSeconds: number | undefined) {
  const now = Date.now()
  const winSec = Number(windowSeconds)
  const winMs = (Number.isFinite(winSec) && winSec > 0 ? winSec : FADE_WINDOW_DEFAULT) * 1000
  const liveIds = new Set((live.value?.Sessions ?? []).map((s) => s.ID))
  const incoming = new Map<string, StatsLiveExited>()
  for (const e of list ?? []) if (e?.ID) incoming.set(e.ID, e)

  const next: FadingCard[] = []
  for (const card of fadingCards.value) {
    if (liveIds.has(card.id)) continue // 又回到请求中：撤掉淡出态，交给主列表（同一 key 复用节点）
    if (now >= card.deadline) continue // 已过淡出窗口
    const inc = incoming.get(card.id)
    if (!inc) {
      next.push(card) // 服务端这轮没返回：仍以本地 deadline 为准，到点移除，不长期保留
      continue
    }
    const exitedAtMs = parseIsoMs(inc.ExitedAt, card.exitedAtMs)
    next.push({
      ...card,
      name: inc.Name || card.name,
      model: inc.Model || card.model,
      exitedAtMs,
      deadline: exitedAtMs + winMs, // 以服务端给的 ExitedAt 为准滚动
    })
  }
  const known = new Set(next.map((c) => c.id))
  for (const e of incoming.values()) {
    if (liveIds.has(e.ID) || known.has(e.ID)) continue
    const exitedAtMs = parseIsoMs(e.ExitedAt, now)
    next.push({
      id: e.ID,
      name: e.Name || e.ID.slice(0, 8),
      model: e.Model || '',
      exitedAtMs,
      deadline: exitedAtMs + winMs,
      leaving: false,
      leftText: '',
    })
  }
  // 顺序：按 ExitedAt 倒序（最近离开的在前），与主列表顺序互不影响，也不参与排序口径
  next.sort((a, b) => b.exitedAtMs - a.exitedAtMs)
  fadingCards.value = next
  // 快照只留「当前请求中 + 正在淡出」的会话，避免长期开着页面时 Map 无限增长
  const keep = new Set<string>([...liveIds, ...next.map((c) => c.id)])
  for (const id of [...lastLiveRows.keys()]) if (!keep.has(id)) lastLiveRows.delete(id)
  tickFading(now)
}

// 30s 定时刷新：重取实时区 + 当前筛选条件下的 dashboard（静默，不闪整页 loading）。
// 该函数只「读」preset / customRange，从不写它们，所以刷新不会重置用户选的筛选条件；
// 页面不可见时直接跳过，避免后台标签页白跑一轮聚合。
// 已有在途请求时本轮跳过（轮询与手动刷新不叠加），交叠时的响应由序号守卫裁决。
async function refreshTick() {
  if (document.visibilityState === 'hidden') return
  if (liveInFlight || dashInFlight || loading.value) return
  await loadLive()
  await load({ silent: true })
}

let liveTimer: number | undefined
// 淡出倒计时用独立 1s 心跳：与 30s 轮询解耦，轮询不会重置淡出进度（不闪）。
let fadeTimer: number | undefined

onMounted(() => {
  load()
  loadAnnouncements()
  loadLive()
  liveTimer = window.setInterval(refreshTick, LIVE_REFRESH_MS)
  fadeTimer = window.setInterval(() => tickFading(), 1000)
})
onUnmounted(() => {
  if (liveTimer !== undefined) window.clearInterval(liveTimer)
  liveTimer = undefined
  if (fadeTimer !== undefined) window.clearInterval(fadeTimer)
  fadeTimer = undefined
})

watch(preset, () => {
  if (preset.value !== 'custom') {
    load()
    loadLive() // 在线用户范围跟页面时间范围走，筛选变了要重取
  }
})
watch(customRange, (v) => {
  if (preset.value === 'custom' && v && v[0] && v[1]) {
    load()
    loadLive()
  }
})

// ===== 格式化 =====
const fmtInt = (n: number) => Number(n || 0).toLocaleString('zh-CN')
const fmtMoney = (n: number) =>
  Number(n || 0).toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 })
const fmtPct = (n: number, digits = 1) => `${(Number(n || 0) * 100).toFixed(digits)}%`
// 输入输出比：≥100 取整、≥10 一位小数、其余两位（避免 0.35 : 1 被压成 0 : 1）
const fmtRatio = (n: number) => {
  const v = Number(n || 0)
  if (v >= 100) return v.toFixed(0)
  if (v >= 10) return v.toFixed(1)
  return v.toFixed(2)
}
const fmtMs = (n: number) => {
  const v = Number(n || 0)
  return v >= 1000 ? `${(v / 1000).toFixed(2)}s` : `${Math.round(v)}ms`
}
// 吞吐量（TPM/RPM）与 Token 统一走 @/utils/format 的 fmtCompact（K/M 缩写，单一实现）
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
// 首屏窗口标签直接跟后端 KPI 窗口走（不再硬编码「近 24h」）：
//   WindowMode=exact    → 请求区间的严格滚动窗口（如「滚动 24 小时」）
//   WindowMode=calendar → 北京时间自然日归一窗口（如「自然日 7 天」）
// 后端把 KPI / Usage.Hourly / ModelPlaza 都算在这一个窗口内，所以标签与数字同源。
function fmtWindowTs(iso: string): string {
  // 直接切 RFC3339 里的墙钟（后端统一 +08:00），不用 Date 转换以免浏览器时区偏移
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(iso || '')
  return m ? `${m[2]}-${m[3]} ${m[4]}:${m[5]}` : ''
}

const kpiWindow = computed(() => {
  const k = data.value?.KPI
  const mode = k?.WindowMode === 'calendar' ? '自然日' : '滚动'
  const fromMs = Date.parse(k?.WindowFrom || '')
  const toMs = Date.parse(k?.WindowTo || '')
  if (!Number.isFinite(fromMs) || !Number.isFinite(toMs) || toMs <= fromMs) {
    return { label: rangeText.value, tip: `窗口 = ${rangeText.value}（等待后端回传窗口）` }
  }
  const hours = Math.max(1, Math.round((toMs - fromMs) / 3600000))
  const span = hours >= 48 ? `${Math.round(hours / 24)} 天` : `${hours} 小时`
  return {
    label: `${mode} ${span}`,
    tip: `${mode}窗口 ${fmtWindowTs(k!.WindowFrom)} ~ ${fmtWindowTs(k!.WindowTo)}（${span}）；首屏数字 / 小时趋势 / 模型广场同源此窗口`,
  }
})

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
      winLabel: kpiWindow.value.label,
      winTip: kpiWindow.value.tip,
    },
    {
      label: '总消费积分',
      glyph: '¥',
      tone: 'violet',
      value: k ? fmtMoney(k.TotalCredits) : '--',
      sub: `日均 ${fmtMoney((k?.TotalCredits ?? 0) / Math.max(data.value?.Usage.Days ?? 1, 1))}`,
      delta: k?.DeltaCredits,
      winLabel: kpiWindow.value.label,
      winTip: kpiWindow.value.tip,
    },
    {
      label: '吞吐量',
      glyph: '⚡',
      tone: 'info',
      value: throughput.value.ready ? fmtCompact(k?.TPM ?? 0) : '--',
      unit: ' TPM',
      sub: throughput.value.ready
        ? `${kpiWindow.value.label} · 请求 ${fmtCompact(k?.RPM ?? 0)} RPM${throughput.value.lowSample ? ' · 样本较少' : ''}`
        : '窗口内没有成功且带耗时的请求',
      winLabel: '忙时口径',
      winTip: `忙时口径 = 成功且带耗时请求的 Token ÷ 同批耗时折算分钟数（≈60÷平均耗时）。${kpiWindow.value.tip}`,
    },
  ]
})

// ===== ② 小指标行 =====
interface MiniStat {
  label: string
  value: string
  tone?: 'ok' | 'info'
  tip?: string
  // 后端尚未提供的指标：如实标「待后端」，不自己造数
  pending?: boolean
}

const miniStats = computed<MiniStat[]>(() => {
  const k = data.value?.KPI
  const t = data.value?.Usage.Total
  // 两条都是纯前端从 Usage.Total 算：口径与首屏窗口同源，不额外取接口。
  const input = Number(t?.Input ?? 0)
  const output = Number(t?.Output ?? 0)
  const cacheRead = Number(t?.CacheRead ?? 0)
  const cacheDenom = input + cacheRead
  const hitRate = cacheDenom > 0 ? cacheRead / cacheDenom : null
  const inOut = output > 0 ? input / output : null
  return [
    { label: '总请求数', value: k ? fmtInt(k.TotalRequests) : '--' },
    { label: '成功率', value: k ? fmtPct(k.SuccessRate) : '--', tone: 'ok' },
    { label: '平均耗时', value: k ? fmtMs(k.AvgDurationMS) : '--' },
    {
      label: '缓存命中率',
      value: hitRate === null ? '—' : fmtPct(hitRate),
      tone: hitRate !== null && hitRate >= 0.5 ? 'ok' : 'info',
      tip: '缓存读 ÷（输入 + 缓存读）。口径 = 所选范围的日累计用量（Usage.Total），与首屏大数字的滚动窗口不同源，两者不要互相换算；没有输入也没有缓存读时显示 —',
    },
    {
      label: '输入输出比',
      value: inOut === null ? '—' : `${fmtRatio(inOut)} : 1`,
      tip: '输入 : 输出。同样来自所选范围的日累计用量（Usage.Total），不是首屏滚动窗口；输出为 0 时显示 —',
    },
    {
      // 与下方实时区同一口径：只数请求中（Inflight>0）的会话，淡出项不算
      label: '请求中会话',
      value: live.value ? fmtInt(live.value.SessionsTotal) : '--',
      tip: '仅统计请求中（Inflight>0）的会话；刚结束的会话会短暂淡出但不计入。管理端=全站，开发端=本人（与下方实时区同源）',
    },
    {
      // UsersTotal 在 #69 之后才是「有请求中会话的用户数」。兼容期（响应里没有 RangeUsers/RangeUsersTotal）
      // 它仍是范围口径，不能拿来顶替请求中用户 → 如实标「待后端合入」。
      label: '请求中用户',
      value: hasRangeUsers.value ? fmtInt(live.value?.UsersTotal ?? 0) : '—',
      pending: !hasRangeUsers.value,
      tip: hasRangeUsers.value
        ? '有请求中会话的用户数（与 Users 行集同源；不含只有范围活跃、当前没有在途请求的用户）'
        : '当前后端响应仍是范围口径：UsersTotal 是「所选范围内有请求的用户数」，不能顶替请求中用户。待 #69 合入后本项自动接上',
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

// ===== ③ 模型广场（v4 第 3 条：只留数据列 + 复制 + 空闲折叠）=====
interface PlazaRow {
  model: string
  tokensText: string
  tpmText: string
  callsText: string
  failText: string
  latencyText: string
  active: boolean
  enabled: boolean
}

const plaza = computed<StatsModelPlaza | null>(() => data.value?.ModelPlaza ?? null)

// 实际生效的排序键：优先读响应回显（后端会把非法值回落成 tokens），没有回显时用本地选择。
const plazaSortLabel = computed(() => {
  const sortKey = ((plaza.value?.Sort as StatsModelSort | undefined) || modelSort.value) as StatsModelSort
  return MODEL_SORT_OPTIONS.find((o) => o.value === sortKey)?.label || 'Token'
})

// 未匹配兜底行（Unmatched=true）：解析不到在册模型的用量。不再当普通模型卡片展示，
// 但**不能丢**——用量信息以小字摘要挂在空闲折叠行里（后端不计入 Total/Active，前端也不参与排序）。
const unmatchedRow = computed<StatsModelStat | null>(
  () => (plaza.value?.List ?? []).find((m) => m.Unmatched) ?? null,
)
const unmatchedText = computed(() => {
  const u = unmatchedRow.value
  if (!u) return ''
  return ['未匹配模型用量：Token ' + fmtCompact(u.Tokens), '请求 ' + fmtInt(u.Calls), '失败 ' + fmtInt(u.Failed)].join(' · ')
})

const plazaRows = computed<PlazaRow[]>(() => {
  // 后端仍会追加一行 Unmatched=true 的兜底行（Model 固定 "__unmatched__"、从不置 Active）：
  // 不当模型卡片渲染（否则展开空闲区会露出明文 __unmatched__），但用量不丢——
  // 由 unmatchedText 在空闲折叠行里单独列一句摘要。
  const list: StatsModelStat[] = (plaza.value?.List ?? []).filter((m) => !m.Unmatched)
  if (!list.length) return []
  return list.map((m) => ({
    model: m.Model || m.Label || '(未命名模型)',
    tokensText: fmtCompact(m.Tokens),
    // BusyMS=0（窗口内没有成功且带耗时的请求）时后端 TPM 为 0，这里显示 —
    tpmText: m.BusyMS > 0 ? `${fmtCompact(m.TPM)} TPM` : '—',
    callsText: `${fmtInt(m.Calls)} / ${fmtInt(m.Failed)}`,
    failText: m.Calls > 0 ? fmtPct(m.FailRate) : '—',
    latencyText:
      m.AvgFirstMS > 0 || m.AvgMS > 0
        ? `${m.AvgFirstMS > 0 ? fmtMs(m.AvgFirstMS) : '—'} / ${m.AvgMS > 0 ? fmtMs(m.AvgMS) : '—'}`
        : '—',
    active: m.Active,
    enabled: m.Enabled,
  }))
})

const activeModels = computed(() => plazaRows.value.filter((m) => m.active))
const idleModels = computed(() => plazaRows.value.filter((m) => !m.active))
const idleExpanded = ref(false)
const visibleModels = computed(() =>
  idleExpanded.value ? plazaRows.value : activeModels.value,
)

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
  running: boolean
  inflight: number
  contextText: string
  contextTip: string
  roundText: string
  totalText: string
  totalTip: string
  tpmText: string
  spentText: string
  spark: string
  lastActive: string
}

const sessionRows = computed<LiveSessionRow[]>(() =>
  (live.value?.Sessions ?? []).map((s) => {
    // ContextLimit 恒 0 = 平台未配置模型上限：只显示已用绝对值，不做除法、不造分母。
    const hasLimit = s.ContextLimit > 0
    // 累计 Token 用后端新字段 TotalTokens（三段求和），前端不自己拼 Calls × 均值。
    const created = s.CreatedAt ? `始自 ${fmtTime(s.CreatedAt)}` : '创建时间未知'
    return {
      id: s.ID,
      name: s.Name || s.ID.slice(0, 8),
      model: s.Model,
      running: s.Inflight > 0,
      inflight: s.Inflight,
      contextText: hasLimit ? `${fmtCompact(s.ContextUsed)}/${fmtCompact(s.ContextLimit)}` : fmtCompact(s.ContextUsed),
      contextTip: hasLimit ? '上下文已用 / 上限' : '仅显示上下文字数：平台未配置模型上限',
      roundText: fmtCompact(s.RoundTokens),
      totalText: `${fmtCompact(s.TotalTokens)} Token`,
      totalTip: `会话累计 Token（输入 + 缓存读 + 输出）· ${created} · 累计 ${fmtInt(s.Calls)} 次请求`,
      tpmText: `${fmtCompact(s.TPM)} TPM`,
      spentText: fmtMoney(s.SpentCredits),
      spark: sparkPath(s.Spark ?? []),
      lastActive: fmtTime(s.LastActive),
    }
  }),
)

// 主列表（请求中）+ 淡出项合并成同一个 keyed 列表：key = 会话 ID。
// 淡出卡片沿用「最后一次实时的那一版」快照，只有徽标/状态变暗，统计值不会突然清空。
interface LiveCard {
  row: LiveSessionRow
  fading: boolean
  leaving: boolean
  leftText: string
}

const liveSessionCards = computed<LiveCard[]>(() => {
  const out: LiveCard[] = sessionRows.value.map((row) => {
    lastLiveRows.set(row.id, row) // 快照：供它随后淡出时沿用
    return { row, fading: false, leaving: false, leftText: '' }
  })
  for (const c of fadingCards.value) {
    const snap = lastLiveRows.get(c.id)
    const row: LiveSessionRow = snap
      ? { ...snap, running: false, inflight: 0 }
      : {
          id: c.id,
          name: c.name,
          model: c.model,
          running: false,
          inflight: 0,
          contextText: '—',
          contextTip: '会话已结束：只保留淡出占位',
          roundText: '—',
          totalText: '',
          totalTip: '',
          tpmText: '',
          spentText: '',
          spark: '',
          lastActive: fmtTime(new Date(c.exitedAtMs).toISOString()),
        }
    out.push({ row, fading: true, leaving: c.leaving, leftText: c.leftText })
  }
  return out
})

interface LiveUserRow {
  id: string
  name: string
  initial: string
  sessions: number
  tpmText: string
  tokensText: string
  tokensTip: string
  spentText: string
  spark: string
}

const liveSampledAt = computed(() => fmtTime(live.value?.SampledAt || ''))
const liveWindowMinutes = computed(() => live.value?.WindowMinutes ?? LIVE_WINDOW_MINUTES)
// 在线用户的实际生效范围读后端回显（RangeFrom / RangeTo），不用本地 preset 自猜
const liveRangeText = computed(() => {
  const l = live.value
  if (!l?.RangeFrom || !l?.RangeTo) return ''
  return `${fmtWindowTs(l.RangeFrom)} ~ ${fmtWindowTs(l.RangeTo)}`
})
// 日序列覆盖区间（DailyFrom + DailyPoints，北京时间日界）：迷你曲线的横轴说明。
const dailyRangeText = computed(() => {
  const l = live.value
  if (!l?.DailyFrom || !l?.DailyTo) return ''
  return `${l.DailyFrom.slice(5)} ~ ${l.DailyTo.slice(5)} 每日`
})

// 兼容期判定：#69 之后才有 RangeUsers / RangeUsersTotal 这两个字段。
// 有 → 用户区走「范围内活跃用户」新口径；没有（#69 未合入）→ 退回旧 Users/UsersTotal（当时是范围口径），
// 标签也回到「在线用户」，绝不把旧字段当新口径展示（不混用两套数）。
const hasRangeUsers = computed(() => Array.isArray(live.value?.RangeUsers))
const usersTabLabel = computed(() => (hasRangeUsers.value ? '范围内活跃用户' : '在线用户'))
const usersTabTotal = computed(() =>
  hasRangeUsers.value ? Number(live.value?.RangeUsersTotal ?? 0) : Number(live.value?.UsersTotal ?? 0),
)
// 两套用户列表共用一个 UsersTruncated（#69：RangeUsers 同样受用户上限约束）
const usersTruncated = computed(() => !!live.value?.UsersTruncated)

const userRows = computed<LiveUserRow[]>(() => {
  const l = live.value
  if (!l) return []
  if (hasRangeUsers.value) {
    // 新口径：RangeSessions/RangeCalls/RangeTokens/RangeCredits 是「所选范围」合计；
    // WindowTokens/SpentCredits 是「近 Window 分钟」——刻意不同名，卡面各就各位、不互相顶替。
    return (l.RangeUsers ?? []).map((u) => ({
      id: u.ID,
      name: u.Name || `用户 ${u.ID}`,
      initial: (u.Name || '?').slice(0, 2),
      sessions: u.RangeSessions,
      tpmText: `${fmtCompact(u.TPM)} TPM`,
      tokensText: fmtCompact(u.RangeTokens),
      tokensTip: `所选范围 Token ${fmtCompact(u.RangeTokens)} · 近 ${liveWindowMinutes.value} 分钟 ${fmtCompact(u.WindowTokens)} · 范围内 ${fmtInt(u.RangeCalls)} 次请求 / ${fmtInt(u.RangeSessions)} 个会话`,
      spentText: fmtMoney(u.RangeCredits),
      spark: sparkPath(u.Daily?.length ? u.Daily : u.Spark ?? []),
    }))
  }
  // 兼容兜底（#69 未合入时的老响应）：当时 Users 就是范围口径，字段名是 ActiveSessions/Calls/RangeTokens/RangeCredits
  return (l.Users ?? []).map((u) => ({
    id: u.ID,
    name: u.Name || `用户 ${u.ID}`,
    initial: (u.Name || '?').slice(0, 2),
    sessions: u.ActiveSessions ?? u.Sessions,
    tpmText: `${fmtCompact(u.TPM)} TPM`,
    tokensText: fmtCompact(u.RangeTokens ?? u.Tokens),
    tokensTip: `所选范围 Token ${fmtCompact(u.RangeTokens ?? u.Tokens)} · 近 ${liveWindowMinutes.value} 分钟 ${fmtCompact(u.Tokens)} · 范围内 ${fmtInt(u.Calls ?? 0)} 次请求`,
    spentText: fmtMoney(u.RangeCredits ?? u.SpentCredits),
    spark: sparkPath(u.Daily?.length ? u.Daily : u.Spark ?? []),
  }))
})

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
            <span class="mini-value" :data-tone="m.tone" :data-pending="m.pending">
              {{ m.value }}
              <em v-if="m.pending" class="mini-pending">待后端</em>
            </span>
          </el-tooltip>
          <span v-else class="mini-value" :data-tone="m.tone">{{ m.value }}</span>
        </div>
      </div>

      <!-- ③ 两张图：四日对比趋势 + 模型广场 -->
      <div class="chart-stack">
        <div class="card">
          <div class="card-head">
            <h3 class="card-title">用量趋势</h3>
            <el-tooltip
              content="四日对比：今天 / 昨天 / 前天 / 大前天 各一条按小时序列（与筛选粒度解耦）"
              placement="top"
              :show-after="150"
            >
              <span class="win-chip">四日对比</span>
            </el-tooltip>
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
          <div class="card-head plaza-head">
            <h3 class="card-title">模型广场</h3>
            <el-tooltip
              :content="`${kpiWindow.tip}；按模型忙时口径（该模型成功且带耗时请求的 Token ÷ 同批耗时折算分钟数）`"
              placement="top"
              :show-after="150"
            >
              <span class="win-chip">{{ kpiWindow.label }} · 按模型忙时</span>
            </el-tooltip>
            <el-radio-group v-model="modelSort" size="small" class="head-actions plaza-sort">
              <el-radio-button v-for="o in MODEL_SORT_OPTIONS" :key="o.value" :value="o.value">
                {{ o.label }}
              </el-radio-button>
            </el-radio-group>
            <span class="gran-chip">实际按{{ plazaSortLabel }}倒序</span>
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
              <span v-if="unmatchedRow" class="unmatched-note" :title="unmatchedText">
                {{ unmatchedText }}（无法归属到在册模型，单独列出、不参与上方排序）
              </span>
            </div>
          </template>
          <el-empty v-else description="模型目录为空：还没有对外模型" :image-size="70" />
        </div>
      </div>

      <!-- ④ 实时区：/stats/live，30s 刷新；会话 / 在线用户均为卡片网格 -->
      <section class="live-block">
        <div class="live-head">
          <h3 class="live-title">实时会话</h3>
          <span class="live-tag"><span class="dot-live" />每 30s 自动刷新 · 只显示请求中的会话</span>
          <span class="live-window">
            吞吐窗口 近 {{ liveWindowMinutes }} 分钟墙钟平均<span v-if="liveSampledAt"> · 采样 {{ liveSampledAt }}</span>
            <span v-if="liveRangeText"> · 在线用户范围 {{ liveRangeText }}</span>
            <span v-if="liveLoading" class="live-loading">刷新中…</span>
          </span>
        </div>

        <div class="live-tabs">
          <el-radio-group v-model="liveTab" size="small">
            <el-radio-button value="sessions">
              我的会话{{ live ? ` · ${live.SessionsTotal}` : '' }}
            </el-radio-button>
            <el-radio-button v-if="isAdmin" value="users">
              {{ usersTabLabel }}{{ live ? ` · ${usersTabTotal}` : '' }}
            </el-radio-button>
          </el-radio-group>
          <span v-if="liveErr" class="live-err">{{ liveErr }}</span>
        </div>

        <div v-if="liveTab === 'sessions'" class="live-card">
          <div v-if="fadingCards.length" class="live-fade-note">
            刚结束请求的会话会在这里淡出（最长 {{ fadeWindowSeconds }} 秒），
            <b>不计入</b>会话数与用户数，也不参与排序。
          </div>
          <template v-if="liveSessionCards.length">
            <div class="xcard-grid">
              <article
                v-for="c in liveSessionCards"
                :key="c.row.id"
                class="xcard"
                :class="{ 'is-fading': c.fading, 'is-leaving': c.leaving }"
                :data-idle="!c.row.running"
                :data-fading="c.fading"
              >
                <header class="xcard-head">
                  <span class="srow-dot" :data-idle="!c.row.running" />
                  <span class="xcard-name" :title="c.row.name">{{ c.row.name }}</span>
                  <span class="model-chip">{{ c.row.model }}</span>
                  <span v-if="c.fading" class="xcard-badge" data-fading="true">
                    离开中 · {{ c.leftText }}
                  </span>
                  <span v-else-if="c.row.inflight > 0" class="xcard-badge">请求中 {{ c.row.inflight }}</span>
                </header>
                <div class="xcard-stats">
                  <el-tooltip :content="c.row.contextTip" placement="top" :show-after="150">
                    <div class="xstat">
                      <i>上下文</i>
                      <b>{{ c.row.contextText }}</b>
                    </div>
                  </el-tooltip>
                  <div class="xstat">
                    <i>本轮</i>
                    <b>{{ c.row.roundText }}</b>
                  </div>
                  <div class="xstat">
                    <i>吞吐</i>
                    <b>{{ c.row.tpmText }}</b>
                  </div>
                  <div class="xstat">
                    <i>已花</i>
                    <b>{{ c.row.spentText }}</b>
                  </div>
                  <el-tooltip :content="c.row.totalTip" placement="top" :show-after="150">
                    <div class="xstat">
                      <i>累计</i>
                      <b>{{ c.row.totalText }}</b>
                    </div>
                  </el-tooltip>
                </div>
                <svg v-if="c.row.spark" class="spark xcard-spark" viewBox="0 0 92 24" preserveAspectRatio="none">
                  <path :d="c.row.spark" />
                </svg>
                <div class="xcard-foot">
                  {{ c.fading ? '请求已结束' : '最近活跃' }} {{ c.row.lastActive }}
                </div>
              </article>
            </div>
            <div v-if="live?.Truncated" class="trunc-note">
              仅显示前 {{ LIVE_LIMIT }} 个请求中的会话（共 {{ live?.SessionsTotal }} 个）
            </div>
          </template>
          <el-empty
            v-else-if="live"
            :description="
              fadingCards.length
                ? '当前没有请求中的会话（刚结束的会话正在淡出）'
                : '当前没有请求中的会话'
            "
            :image-size="60"
          />
          <el-empty v-else description="实时数据加载中" :image-size="60" />
        </div>

        <div v-else class="live-card">
          <template v-if="userRows.length">
            <!-- 日序列区间是整组共享的，只在表头渲染一次，不在每张卡里重复 -->
            <div v-if="dailyRangeText" class="grid-caption">
              迷你曲线：{{ dailyRangeText }}活跃 · 无请求日按 0
            </div>
            <div class="xcard-grid">
              <article v-for="u in userRows" :key="u.id" class="xcard">
                <header class="xcard-head">
                  <span class="uavatar">{{ u.initial }}</span>
                  <span class="xcard-name" :title="u.name">{{ u.name }}</span>
                  <el-tooltip
                    :content="hasRangeUsers ? '所选范围内有过请求的会话数（去重）；此刻不一定在请求中' : '当前请求中的会话数'"
                    placement="top"
                    :show-after="150"
                  >
                    <span class="xcard-badge" :data-idle="u.sessions === 0">会话 {{ u.sessions }}</span>
                  </el-tooltip>
                </header>
                <div class="xcard-stats">
                  <div class="xstat">
                    <i>吞吐</i>
                    <b>{{ u.tpmText }}</b>
                  </div>
                  <el-tooltip :content="u.tokensTip" placement="top" :show-after="150">
                    <div class="xstat">
                      <i>消耗</i>
                      <b>{{ u.tokensText }}</b>
                    </div>
                  </el-tooltip>
                  <el-tooltip
                    :content="hasRangeUsers ? '所选范围内的消耗积分合计' : '近窗口消耗积分'"
                    placement="top"
                    :show-after="150"
                  >
                    <div class="xstat">
                      <i>积分</i>
                      <b>{{ u.spentText }}</b>
                    </div>
                  </el-tooltip>
                </div>
                <svg v-if="u.spark" class="spark xcard-spark" viewBox="0 0 92 24" preserveAspectRatio="none">
                  <path :d="u.spark" />
                </svg>
              </article>
            </div>
            <div v-if="usersTruncated" class="trunc-note">
              仅显示前 100 个{{ usersTabLabel }}（共 {{ usersTabTotal }} 个）
            </div>
          </template>
          <el-empty v-else-if="live" :description="`所选范围内没有${usersTabLabel}`" :image-size="60" />
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
  /* 项数会随口径增删（当前 7 项）：按最小宽度自适应换行，不写死列数 */
  grid-template-columns: repeat(auto-fit, minmax(168px, 1fr));
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
/* 后端未提供的指标：值位显示 — + 「待后端」小标，不自己造数 */
.mini-value[data-pending='true'] {
  color: var(--color-text-tertiary);
  font-weight: 600;
}
.mini-pending {
  margin-left: 6px;
  font-style: normal;
  font-family: inherit;
  font-size: 11px;
  font-weight: 500;
  padding: 1px 6px;
  border-radius: 999px;
  background: #f2f4f7;
  color: var(--color-text-tertiary);
}
.unmatched-note {
  display: inline-block;
  color: var(--color-text-tertiary);
  font-size: 11px;
  line-height: 1.6;
}

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
/* 实时卡片网格：3 列 → 2 列 → 1 列（见文末响应式断点） */
.xcard-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 10px;
  transition: grid-template-columns 0.2s ease;
}
.xcard {
  display: flex;
  flex-direction: column;
  gap: 8px;
  background: #fafbfd;
  border: 1px solid var(--color-border);
  border-radius: 10px;
  padding: 12px 14px;
  min-width: 0;
  transition: border-color 0.18s ease, box-shadow 0.18s ease, background 0.18s ease;
}
.xcard:hover {
  border-color: var(--color-border-strong);
  box-shadow: var(--shadow-card);
}
.xcard[data-idle='true'] { background: #f7f8fa; }
/* 离开中（淡出）卡片：同一节点就地变暗 + 虚线边，不参与计数；到 ExitedAt+FadeWindow 后节点被移除 */
.xcard.is-fading {
  opacity: 0.62;
  background: #f8f9fb;
  border-style: dashed;
  filter: saturate(0.7);
}
.xcard.is-leaving {
  opacity: 0;
  transition: opacity 0.4s ease;
}
.xcard-badge[data-fading='true'] {
  background: #fff4e5;
  color: #b54708;
}
.live-fade-note {
  font-size: 11px;
  line-height: 1.6;
  color: var(--color-text-tertiary);
  margin-bottom: 8px;
}
.live-fade-note b { color: var(--color-text-secondary); }
.xcard-head {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.xcard-name {
  font-size: 13px;
  font-weight: 700;
  color: var(--color-text);
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.xcard-badge {
  margin-left: auto;
  font-size: 10px;
  font-weight: 600;
  padding: 2px 8px;
  border-radius: 999px;
  background: #e8f8f0;
  color: var(--color-success);
  white-space: nowrap;
  flex-shrink: 0;
}
.xcard-badge[data-idle='true'] {
  background: #f0f1f4;
  color: var(--color-text-tertiary);
}
.xcard-stats {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 6px 10px;
}
.xstat {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 6px;
  min-width: 0;
  font-size: 12px;
  color: var(--color-text-secondary);
}
.xstat i {
  font-style: normal;
  font-size: 11px;
  color: var(--color-text-tertiary);
  white-space: nowrap;
}
.xstat b {
  font-size: 12px;
  font-weight: 600;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
  font-family: 'SF Mono', Consolas, monospace;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.xcard-spark { margin-left: 0; }
.xcard-foot {
  font-size: 11px;
  color: var(--color-text-tertiary);
}
/* 整组共享的表头说明（日序列区间等），避免每张卡重复同一行 */
.grid-caption {
  font-size: 11px;
  color: var(--color-text-tertiary);
  margin-bottom: 8px;
}
.srow-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--color-success);
  box-shadow: 0 0 0 3px rgba(18, 183, 106, 0.18);
  animation: livePulse 1.6s infinite;
  flex-shrink: 0;
}
/* 非请求中（已在淡出/已结束）：灰点，不闪 */
.srow-dot[data-idle='true'] {
  background: var(--color-text-tertiary);
  box-shadow: 0 0 0 3px rgba(102, 112, 133, 0.14);
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
.model-chip {
  font-size: 10px;
  font-weight: 600;
  padding: 1px 8px;
  border-radius: 999px;
  background: var(--brand-primary-soft);
  color: var(--brand-primary);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  max-width: 60%;
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
  .xcard-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
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
  .xcard-grid { grid-template-columns: 1fr; }
  .head-actions { margin-left: 0; }
  .live-window { margin-left: 0; }
  .plaza-head { row-gap: 8px; }
}
/* ≤640px：移动端铁律——单列 + 正文 ≥14px（辅助小标 12px 不在此限） */
@media (max-width: 640px) {
  .mini-grid { grid-template-columns: 1fr; }
  .big-label,
  .big-sub,
  .mini-label,
  .mini-value,
  .model-row span,
  .model-row b,
  .xstat i,
  .xstat b,
  .card-title,
  .live-title,
  .xcard-name,
  .xcard-foot,
  .idle-note,
  .pending-note,
  .trend-legend {
    font-size: 14px;
  }
  .big-value { font-size: 30px; }
  .mini-value { font-size: 18px; }
  :deep(.el-radio-button__inner) { font-size: 14px; }
  .plaza-sort { width: 100%; }
  .plaza-sort :deep(.el-radio-button) { flex: 1; }
  .plaza-sort :deep(.el-radio-button__inner) { width: 100%; padding-left: 4px; padding-right: 4px; }
}
</style>
