<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  listBillings,
  getBillingStats,
  listChannels,
  listChannelKeys,
  listExternalModels,
  listAutoModels,
  listSessions,
  listTokens,
  type AdminBillingItem,
  type AdminChannel,
  type AdminSession,
  type AdminToken,
  type BillingStats,
  type ChannelKey,
  type ExternalModel,
} from '@/api/admin'
import ErrorBubble from '@/components/ErrorBubble.vue'
import UserSelect from '@/components/UserSelect.vue'
import ColumnFilter from '@/components/ColumnFilter.vue'
import FilterBar from '@/components/FilterBar.vue'
import { fmtDate, fmtTime } from '@/utils/format'
import { resolveBillingRange } from '@/utils/range'
import { useIsNarrow } from '@/composables/useIsNarrow'
import { ElMessage } from 'element-plus'
import { Search } from '@element-plus/icons-vue'

const route = useRoute()
const router = useRouter()

// mode='self' 时复用为开发者自助账单页（API 走 /dev 前缀）
const props = withDefaults(defineProps<{ mode?: 'admin' | 'self' }>(), { mode: 'admin' })
const isSelf = computed(() => props.mode === 'self')
const base = computed(() => (isSelf.value ? '/dev' : '/admin'))
// 窄屏：表格转卡片式列表（宽表在手机上必然横向撑破）
const isNarrow = useIsNarrow()

const loading = ref(false)
const list = ref<AdminBillingItem[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

const stats = ref<BillingStats>({
  Requests: 0,
  CreditsTotal: 0,
  TokensTotal: 0,
  DurationTotalMS: 0,
  InputTokens: 0,
  CacheReadTokens: 0,
  BusyRequests: 0,
  BusyTokens: 0,
  BusyDurationMS: 0,
  BusyRPM: 0,
  BusyTPM: 0,
})

// ===== 列头筛选选项数据 =====
const channels = ref<AdminChannel[]>([])
const channelKeys = ref<ChannelKey[]>([])
const models = ref<ExternalModel[]>([])
const autoNames = ref<string[]>([])
const tokens = ref<AdminToken[]>([])
const sessions = ref<AdminSession[]>([])
// 当前所选用户名（用于筛选标签展示）
const userName = ref('')

const filters = reactive({
  user_id: (route.query.user_id ? String(route.query.user_id) : undefined) as string | undefined,
  token_id: undefined as string | undefined,
  channel_id: undefined as string | undefined,
  channel_key_id: undefined as string | undefined,
  session_id: undefined as string | undefined,
  model: undefined as string | undefined,
  status: undefined as string | undefined,
  // 请求链路 ID：调用方把网关错误体里的 request_id 贴进来，直接定位这一次请求
  request_id: (route.query.request_id ? String(route.query.request_id).trim() : undefined) as
    | string
    | undefined,
  range: null as [string, string] | null,
})

// URL 带 range（today|yesterday|month）时据此设置时间区间初值（Asia/Shanghai），首次 load 即生效
filters.range = resolveBillingRange(route.query.range)

// 前端筛选态键 → 后端 query 参数键（PascalCase 契约）
const paramKeyMap: Record<string, string> = {
  user_id: 'UserID',
  token_id: 'TokenID',
  channel_key_id: 'ChannelKeyID',
  session_id: 'SessionID',
  model: 'Model',
  status: 'Status',
  request_id: 'RequestID',
}

function buildParams(extra: Record<string, unknown> = {}): Record<string, unknown> {
  const p: Record<string, unknown> = { ...extra }
  for (const [k, v] of Object.entries(filters)) {
    if (v === undefined || v === null || v === '') continue
    if (k === 'channel_id') continue // 后端按 channel_key_id 过滤，渠道仅作级联
    if (k === 'range') {
      if (filters.range) {
        p.From = filters.range[0]
        p.To = filters.range[1]
      }
      continue
    }
    p[paramKeyMap[k] ?? k] = v
  }
  return p
}

// ===== request_id 搜索入口 =====
// 调用方把网关错误体（或响应头 x-request-id）里的 ID 贴进来即可定位这一次请求。
// 输入框单独持有 draft，避免边打字边发请求（回车/点「定位」才生效）。
const requestIdInput = ref(filters.request_id || '')

function applyRequestId() {
  const v = requestIdInput.value.trim()
  if (v === (filters.request_id || '')) {
    // 值没变（例如重复回车）：手动重载一次，保证用户有「已执行」的反馈
    if (v) {
      page.value = 1
      load()
    }
    return
  }
  filters.request_id = v || undefined
}
function clearRequestId() {
  requestIdInput.value = ''
  filters.request_id = undefined
}

// ===== 归并主行的子记录（Group=0：一次请求的多条候选/重试账单） =====
// 列表默认按 request_id 归并成一条主行，Attempts>1 说明发生过候选切换或上游重试；
// 展开时才按 RequestID + Group=0 拉子记录，避免列表默认查询变重。
const childrenMap = ref<Record<string, AdminBillingItem[]>>({})
const childrenLoading = ref<Record<string, boolean>>({})

async function loadChildren(row: AdminBillingItem) {
  if (!row.RequestID) return
  if (childrenMap.value[row.BillingID]) return
  childrenLoading.value = { ...childrenLoading.value, [row.BillingID]: true }
  try {
    const res = await listBillings({ RequestID: row.RequestID, Group: 0, Page: 1, Size: 50 }, base.value)
    if (!alive.value) return
    childrenMap.value = { ...childrenMap.value, [row.BillingID]: res.Data.List || [] }
  } catch (e: any) {
    if (!alive.value) return
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    if (alive.value) {
      childrenLoading.value = { ...childrenLoading.value, [row.BillingID]: false }
    }
  }
}

function onExpand(row: AdminBillingItem, expanded: AdminBillingItem[]) {
  // el-table 展开/收起都会回调：只在含本行（展开）时拉数据
  if (expanded.some((r) => r.BillingID === row.BillingID)) loadChildren(row)
}

// 手机端：卡片内联展开（不用 el-table 的 expand 列）
const openCards = ref<Record<string, boolean>>({})
function toggleCard(row: AdminBillingItem) {
  const key = row.BillingID
  const next = !openCards.value[key]
  openCards.value = { ...openCards.value, [key]: next }
  if (next) loadChildren(row)
}

function childrenOf(row: AdminBillingItem): AdminBillingItem[] {
  return childrenMap.value[row.BillingID] || []
}
function childLoading(row: AdminBillingItem): boolean {
  return !!childrenLoading.value[row.BillingID]
}
function canExpand(row: AdminBillingItem): boolean {
  return !!row.RequestID && (row.Attempts || 1) > 1
}

// 请求 ID 展示：完整 ID 32 位左右，列表里截断显示，悬浮看全量，点击复制。
function shortReqId(id?: string): string {
  if (!id) return '-'
  return id.length > 14 ? `${id.slice(0, 8)}…${id.slice(-4)}` : id
}
async function copyReqId(id?: string) {
  if (!id) return
  try {
    await navigator.clipboard.writeText(id)
    ElMessage.success('请求 ID 已复制')
  } catch {
    ElMessage.warning('复制失败，请手动选择复制')
  }
}
function openDetail(row: AdminBillingItem) {
  router.push(isSelf.value ? `/dev/billings/${row.BillingID}` : `/admin/billings/${row.BillingID}`)
}

// ===== 请求序号守卫（Owl 建议项）=====
// 连点翻页 / 快速切筛选条件时，先发的请求可能后返回，把新结果覆盖成旧的。
// 这里给每次 load 发一个自增序号，只有「当前最新序号」的响应才允许写入状态；
// 组件卸载或路由离开时序号自增一次，把在途请求整体作废（避免离开后仍写旧数据）。
// 选序号而不是 AbortController：load 里是 Promise.all 两个请求，序号守卫一处判定即可
// 覆盖两个响应，也不用为每个请求维护自己的 controller 生命周期。
let loadSeq = 0
const alive = ref(true)

async function load() {
  const my = ++loadSeq
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  const params = buildParams({ Page: page.value, Size: size.value })
  const statsParams = buildParams()
  try {
    const [billRes, statsRes] = await Promise.all([
      listBillings(params, base.value),
      getBillingStats(statsParams, base.value),
    ])
    // 过期响应直接丢弃（不写列表、不写统计、不动 loading）
    if (!alive.value || my !== loadSeq) return
    list.value = billRes.Data.List
    total.value = billRes.Data.Total
    stats.value = statsRes.Data
  } catch (e: any) {
    if (!alive.value || my !== loadSeq) return
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    // 只有最新一次请求结束时才清 loading，否则旧请求返回会把新请求的 loading 关掉
    if (alive.value && my === loadSeq) loading.value = false
  }
}

// ===== 选项加载 =====
async function loadChannels() {
  // self 模式仅服务端只读接口（无 /dev/channels），直接跳过，避免 403 噪音。
  if (isSelf.value) return
  if (channels.value.length) return
  try {
    channels.value = (await listChannels()).Data || []
  } catch {
    channels.value = []
  }
}
async function loadModels() {
  if (models.value.length) return
  try {
    models.value = (await listExternalModels({}, base.value)).Data || []
  } catch {
    models.value = []
  }
}
// Auto 名一并作为模型筛选项：Auto 请求的账单 external_model_name 记命中成员名，
// 但用户按 Auto 名也应能搜到（后端 Model 过滤同时匹配 auto_model_name）。
async function loadAutoNames() {
  if (autoNames.value.length) return
  try {
    const res = await listAutoModels({}, base.value)
    autoNames.value = (res.Data || []).map((a) => a.Name)
  } catch {
    autoNames.value = []
  }
}
async function loadTokensOfUser() {
  // self 模式无 admin 令牌接口，跳过，避免 403 噪音。
  if (isSelf.value) return
  tokens.value = []
  if (!filters.user_id) {
    filters.token_id = undefined
    return
  }
  try {
    const res = await listTokens({ UserID: filters.user_id, Page: 1, Size: 100 })
    tokens.value = res.Data.List || []
  } catch {
    tokens.value = []
  }
}
async function searchSessions(q: string) {
  try {
    const res = await listSessions({ Q: q, Page: 1, Size: 50 }, base.value)
    sessions.value = res.Data.List || []
  } catch {
    sessions.value = []
  }
}
async function onChannelChange(id?: string) {
  filters.channel_key_id = undefined
  channelKeys.value = []
  if (!id) return
  try {
    channelKeys.value = (await listChannelKeys(id)).Data || []
  } catch {
    channelKeys.value = []
  }
}

// 列头浮窗「清除」：还原对应筛选条件并保持级联下拉一致。
function clearUserKey() {
  filters.user_id = undefined
  filters.token_id = undefined
}
function clearChannelKey() {
  filters.channel_id = undefined
  filters.channel_key_id = undefined
  channelKeys.value = []
}

// ===== 生效筛选标签 =====
function channelLabel(id?: string) {
  if (!id) return ''
  return channels.value.find((c) => c.ID === id)?.Name || id
}
function channelKeyLabel(id?: string) {
  if (!id) return ''
  return channelKeys.value.find((k) => k.ID === id)?.Name || id
}
function tokenTagLabel(id?: string) {
  if (!id) return ''
  const tk = tokens.value.find((x) => x.ID === id)
  return tk ? tk.DisplayName || tk.TokenDisplay : id
}
function sessionTagLabel(id?: string) {
  if (!id) return ''
  const s = sessions.value.find((x) => x.SessionID === id)
  return s ? sessionLabel(s) : id
}

const activeTags = computed(() => {
  const out: { key: string; label: string }[] = []
  if (filters.range) {
    out.push({
      key: 'range',
      label: `时间: ${fmtDate(filters.range[0])} ${fmtTime(filters.range[0])} ~ ${fmtDate(filters.range[1])} ${fmtTime(filters.range[1])}`,
    })
  }
  if (filters.user_id) out.push({ key: 'user_id', label: `用户: ${userName.value || filters.user_id}` })
  if (filters.token_id) out.push({ key: 'token_id', label: `密钥: ${tokenTagLabel(filters.token_id)}` })
  if (filters.channel_key_id) {
    const parts = [channelLabel(filters.channel_id), channelKeyLabel(filters.channel_key_id)].filter(Boolean).join(' · ')
    out.push({ key: 'channel_key_id', label: `渠道密钥: ${parts || filters.channel_key_id}` })
  } else if (filters.channel_id) {
    out.push({ key: 'channel_id', label: `渠道: ${channelLabel(filters.channel_id)}` })
  }
  if (filters.session_id) out.push({ key: 'session_id', label: `会话: ${sessionTagLabel(filters.session_id)}` })
  if (filters.model) out.push({ key: 'model', label: `模型: ${filters.model}` })
  if (filters.status) out.push({ key: 'status', label: `状态: ${filters.status === 'completed' ? '成功' : '失败'}` })
  if (filters.request_id) out.push({ key: 'request_id', label: `请求 ID: ${filters.request_id}` })
  return out
})

// 关闭单个筛选标签（被 watch 监听的字段变化后自动重载）
function clearTag(key: string) {
  switch (key) {
    case 'range':
      filters.range = null
      break
    case 'user_id':
      filters.user_id = undefined
      break
    case 'token_id':
      filters.token_id = undefined
      break
    case 'channel_id':
      filters.channel_id = undefined
      break
    case 'channel_key_id':
      filters.channel_id = undefined
      filters.channel_key_id = undefined
      channelKeys.value = []
      break
    case 'session_id':
      filters.session_id = undefined
      break
    case 'model':
      filters.model = undefined
      break
    case 'status':
      filters.status = undefined
      break
    case 'request_id':
      filters.request_id = undefined
      requestIdInput.value = ''
      break
  }
}

// ===== 筛选变化自动刷新 =====
watch(
  () => ({
    user_id: filters.user_id,
    token_id: filters.token_id,
    channel_key_id: filters.channel_key_id,
    session_id: filters.session_id,
    model: filters.model,
    status: filters.status,
    request_id: filters.request_id,
    range: filters.range,
  }),
  () => {
    page.value = 1
    load()
  },
)
watch(() => filters.user_id, loadTokensOfUser)

// 同一页面内再次从用户面板跳转（URL query 变化）时同步筛选；上面的 filters watch 会自动刷新
watch(
  () => [route.query.range, route.query.user_id, route.query.request_id],
  ([r, u, rid]) => {
    const rng = resolveBillingRange(r)
    if (rng) filters.range = rng
    filters.user_id = u ? String(u) : undefined
    // 从会话详情/调用详情点「在账单中定位」跳过来时，直接把 request_id 填进搜索框
    const nextRid = rid ? String(rid).trim() : ''
    if (nextRid !== (filters.request_id || '')) {
      filters.request_id = nextRid || undefined
      requestIdInput.value = nextRid
    }
  },
)

function reset() {
  // 若存在被 watch 监听的字段，置空后由 watch 触发重载；否则手动重载，避免重复请求
  const wasActive = !!(
    filters.user_id ||
    filters.token_id ||
    filters.channel_key_id ||
    filters.session_id ||
    filters.model ||
    filters.status ||
    filters.request_id ||
    filters.range
  )
  filters.user_id = undefined
  filters.token_id = undefined
  filters.channel_id = undefined
  filters.channel_key_id = undefined
  filters.session_id = undefined
  filters.model = undefined
  filters.status = undefined
  filters.request_id = undefined
  requestIdInput.value = ''
  filters.range = null
  channelKeys.value = []
  if (!wasActive) {
    page.value = 1
    load()
  }
}

onMounted(() => {
  loadChannels()
  loadModels()
  loadAutoNames()
  searchSessions('')
  loadTokensOfUser()
  load()
})

// 卸载/离开路由：作废在途请求（序号自增后，旧响应一律被判过期）
onBeforeUnmount(() => {
  alive.value = false
  loadSeq++
})

// ===== 展示格式化 =====
// token 用量两行展示：第一行 输入/输出，第二行 缓存读（第二行全 0 不显示）
function tokenLine1(row: AdminBillingItem): string {
  const t = row.Tokens || ({} as AdminBillingItem['Tokens'])
  const parts: string[] = []
  if (t.Input || t.Input === 0) parts.push(`输入 ${t.Input}`)
  if (t.Output) parts.push(`输出 ${t.Output}`)
  return parts.length ? parts.join(' · ') : '-'
}
function tokenLine2(row: AdminBillingItem): string {
  const t = row.Tokens || ({} as AdminBillingItem['Tokens'])
  const parts: string[] = []
  if (t.CacheRead) parts.push(`缓存读 ${t.CacheRead}`)
  return parts.join(' · ')
}

// 渠道-密钥双行：上渠道名，下密钥名（无 key_name 时不展示兜底 ID）
function keyLabel(row: AdminBillingItem): string {
  return row.KeyName || '-'
}

function fmtMs(ms?: number | null): string {
  if (ms == null) return '-'
  return ms >= 1000 ? `${(ms / 1000).toFixed(2)}s` : `${ms}ms`
}

// 渠道显示名：管理端缺失用 '-'；开发者端该字段在白名单内，缺失时明确「不展示」，
// 不要留空让人以为「这次请求没走渠道」（也提示别为补字段去动别的接口）。
function rowChannelLabel(row: AdminBillingItem): string {
  if (row.ChannelName) return row.ChannelName
  return isSelf.value ? '不展示' : '-'
}

// 生成速率：输出 token / (总耗时 - 首字耗时)（流式）；非流式无首字 → 输出 / 总耗时。
function tokenRate(row: AdminBillingItem): string {
  const dur = row.DurationMs || 0
  const first = row.FirstTokenMs
  if (dur <= 0) return '-'
  const gen = first == null || first < 0 ? dur : dur - first
  if (gen <= 0) return '-'
  const out = (row.Tokens || {}).Output || 0
  if (!out) return '-'
  return `${Math.round((out / gen) * 1000)} tok/s`
}

// 统计栏：平均 RPM/TPM 采用后端忙时口径（分子只算成功且带耗时的请求，分母 BusyDurationMS 与之同源）。
// 有数据即显示：BusyRequests>0 且忙时耗时>0 才出数，否则显示 —；
// 样本少（成功请求数 < 20 或忙时耗时 < 30 秒）时照常显示数值，只在旁边灰字标注「样本较少」。
// 说明：这是「忙时处理速率」，与渠道限流用的 RPM/TPM 不是同一个口径，tooltip 里点明。
const BUSY_RATE_TIP = '按成功请求处理耗时折算（≈60÷平均耗时），与渠道限流 '

function busyRateTip(kind: 'RPM' | 'TPM'): string {
  return `${BUSY_RATE_TIP}${kind} 不同口径`
}
function busyRateReady(): boolean {
  const s = stats.value
  return (s.BusyRequests || 0) > 0 && (s.BusyDurationMS || 0) > 0
}
const lowSample = computed(() => {
  const s = stats.value
  return (s.BusyRequests || 0) < 20 || (s.BusyDurationMS || 0) < 30000
})
function avgRPM(): string {
  if (!busyRateReady()) return '—'
  return (stats.value.BusyRPM || 0).toFixed(1)
}
function avgTPM(): string {
  if (!busyRateReady()) return '—'
  return Math.round(stats.value.BusyTPM || 0).toLocaleString()
}
const creditsTotal = computed(() => stats.value.CreditsTotal.toLocaleString(undefined, { maximumFractionDigits: 4 }))
const tokensTotal = computed(() => stats.value.TokensTotal.toLocaleString())
// 缓存命中率 = 缓存读 ÷ (输入 + 缓存读)
const cacheHitRate = computed(() => {
  const s = stats.value
  const denom = s.InputTokens + s.CacheReadTokens
  if (!denom) return '-'
  return `${((s.CacheReadTokens / denom) * 100).toFixed(1)}%`
})

// 消耗积分公式（与详情页一致的三步式展示）：Token×单价 → 系数调整 → 积分换算
function creditLines(row: AdminBillingItem): string[] {
  const t = row.Tokens || ({} as AdminBillingItem['Tokens'])
  const rates = row.Rates || {}
  const coeffTime = row.CoeffTime ?? 1
  const coeffCtx = row.CoeffContext ?? 1
  const r = row.RValue ?? 0

  // 三段直接相加：输入为纯输入（不含缓存读）、缓存读单列、输出含推理，
  // 与后端 sumRates 口径一致，不做 (Input - CacheRead) 减法。
  const segs: Array<[number, string, string]> = [
    [t.Input || 0, '输入', 'Input'],
    [t.Output || 0, '输出', 'Output'],
    [t.CacheRead || 0, '缓存读', 'CacheRead'],
  ]
  const step1: string[] = []
  let sub1 = 0
  for (const [n, label, k] of segs) {
    if (!n) continue
    const rate = rates[k] || 0
    const amt = Math.round(n * rate * 1e6) / 1e6
    sub1 += amt
    step1.push(`  ${label} ${n} × ${rate} = ${amt}`)
  }
  if (!step1.length) return row.CreditsConsumed != null ? [`积分 ${row.CreditsConsumed}`] : ['-']
  sub1 = Math.round(sub1 * 1e6) / 1e6
  const sub2 = Math.round(sub1 * coeffTime * coeffCtx * 1e6) / 1e6
  const final = r ? Math.round((sub2 / r) * 1e6) / 1e6 : sub2
  return [
    `Token × 单价${row.PricingMode === 'cost' ? '（成本）' : ''}`,
    ...step1,
    `  小计 = ${sub1}`,
    `系数调整：时段 ×${coeffTime}，分档 ×${coeffCtx} → ${sub2}`,
    `积分换算：÷ ${r} (R) = ${final} 积分`,
  ]
}

// 会话选项展示：名称优先，缺失用「未命名」。
function sessionLabel(s: AdminSession): string {
  return s.SessionName || '未命名'
}
</script>

<template>
  <div class="page-container">
    <!-- 顶部统计栏：筛选条件聚合 + 刷新/重置 -->
    <div class="card stats-bar">
      <div class="stat">
        <span class="stat-label">总积分</span>
        <span class="stat-value">{{ creditsTotal }}</span>
      </div>
      <div class="stat">
        <span class="stat-label">总 Token</span>
        <span class="stat-value">{{ tokensTotal }}</span>
      </div>
      <div class="stat">
        <span class="stat-label" title="缓存读 ÷ (输入+缓存读)">缓存命中率</span>
        <span class="stat-value">{{ cacheHitRate }}</span>
      </div>
      <div class="stat">
        <span class="stat-label" :title="`每分钟平均请求数；${busyRateTip('RPM')}`">平均 RPM</span>
        <span class="stat-line">
          <span class="stat-value">{{ avgRPM() }}</span>
          <span v-if="busyRateReady() && lowSample" class="stat-note">样本较少</span>
        </span>
      </div>
      <div class="stat">
        <span class="stat-label" :title="`每分钟平均 Token 数；${busyRateTip('TPM')}`">平均 TPM</span>
        <span class="stat-line">
          <span class="stat-value">{{ avgTPM() }}</span>
          <span v-if="busyRateReady() && lowSample" class="stat-note">样本较少</span>
        </span>
      </div>
    </div>

    <div v-loading="loading" class="card">
      <FilterBar
        :tags="activeTags"
        :loading="loading"
        @clear="clearTag"
        @reset="reset"
        @refresh="load"
      >
        <!-- request_id 搜索入口：贴上网关错误体里的 request_id 就能定位这一次请求 -->
        <template #actions>
          <div class="req-search">
            <el-input
              v-model="requestIdInput"
              size="small"
              clearable
              placeholder="粘贴 request_id 定位请求"
              @keyup.enter="applyRequestId"
              @clear="clearRequestId"
            >
              <template #prefix>
                <el-icon><Search /></el-icon>
              </template>
            </el-input>
            <el-button size="small" type="primary" plain @click="applyRequestId">定位</el-button>
          </div>
        </template>
      </FilterBar>

      <!-- 桌面：表格（一次请求一行；Attempts>1 可展开子记录） -->
      <el-table
        v-if="!isNarrow"
        :data="list"
        border
        stripe
        class="table-nowrap clickable-rows"
        row-key="BillingID"
        @expand-change="onExpand"
        @row-click="(row: AdminBillingItem) => openDetail(row)"
      >
        <!-- 展开列：按 RequestID + Group=0 懒加载这一次请求的全部候选/重试账单 -->
        <el-table-column type="expand" width="36">
          <template #default="{ row }">
            <div class="sub-wrap">
              <div v-if="childLoading(row)" class="sub-loading">正在加载该请求的尝试记录…</div>
              <div v-else-if="!childrenOf(row).length" class="sub-loading">
                {{ canExpand(row) ? '该请求暂无子记录' : '该请求未记录 request_id，无法展开子记录' }}
              </div>
              <table v-else class="sub-table">
                <thead>
                  <tr>
                    <th>时间</th>
                    <th>状态</th>
                    <th>模型 / 内部</th>
                    <th>渠道</th>
                    <th class="r">Token</th>
                    <th class="r">积分</th>
                    <th>失败原因</th>
                  </tr>
                </thead>
                <tbody>
                  <tr
                    v-for="c in childrenOf(row)"
                    :key="c.BillingID"
                    class="sub-row"
                    @click.stop="openDetail(c)"
                  >
                    <td>{{ fmtTime(c.CallTime) }}</td>
                    <td>
                      <span :class="['st', c.Status === 'completed' ? 'st-ok' : 'st-fail']">
                        {{ c.Status === 'completed' ? '成功' : '失败' }}
                      </span>
                    </td>
                    <td>
                      {{ c.ExternalModel || '-' }}
                      <span v-if="!isSelf" class="sub-dim">{{ c.InternalModelID || '-' }}</span>
                    </td>
                    <td>
                      {{ rowChannelLabel(c) }}
                      <span v-if="!isSelf" class="sub-dim">{{ c.KeyName || '-' }}</span>
                    </td>
                    <td class="r">{{ tokenLine1(c) }}</td>
                    <td class="r">{{ c.CreditsConsumed ?? '-' }}</td>
                    <td class="sub-reason">{{ c.ErrorMessage || '-' }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
          </template>
        </el-table-column>

        <!-- 时间：筛选图标 → 浮窗时间段选择器 -->
        <el-table-column label="时间" min-width="130" align="center">
          <template #header>
            <ColumnFilter
              label="时间"
              :width="360"
              :active="!!filters.range"

              @clear="() => (filters.range = null)"
            >
              <el-date-picker
                v-model="filters.range"
                type="datetimerange"
                value-format="YYYY-MM-DDTHH:mm:ssZ"
                start-placeholder="开始时间"
                end-placeholder="结束时间"
                size="small"
                style="width: 100%"
              />
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ fmtDate(row.CallTime) }}</span>
              <span class="t-clock">{{ fmtTime(row.CallTime) }}</span>
            </div>
          </template>
        </el-table-column>

        <!-- 请求 ID：一次请求的唯一标识，点一下复制（调用方拿到它就能定位）；归并行显示尝试次数 -->
        <el-table-column label="请求 ID" min-width="130">
          <template #default="{ row }">
            <div class="req-cell">
              <span
                class="mono req-id"
                :title="row.RequestID ? `${row.RequestID}（点击复制）` : '该账单未记录 request_id'"
                @click.stop="copyReqId(row.RequestID)"
              >
                {{ row.RequestID ? shortReqId(row.RequestID) : '-' }}
              </span>
              <el-tag v-if="(row.Attempts || 1) > 1" size="small" type="warning" effect="plain">
                {{ row.Attempts }} 次尝试
              </el-tag>
            </div>
          </template>
        </el-table-column>

        <!-- 用户 / 密钥：筛选图标 → 浮窗（用户搜索 + 该用户令牌）；self 模式不展示 -->
        <el-table-column v-if="!isSelf" label="用户 / 密钥" min-width="150">
          <template #header>
            <ColumnFilter
              label="用户 / 密钥"
              :active="!!filters.user_id || !!filters.token_id"

              @clear="clearUserKey"
            >
              <div class="f-stack">
                <UserSelect
                  v-model="filters.user_id"
                  placeholder="按用户名搜索"
                  clearable
                  @select="(u) => (userName = u ? u.Nickname || u.Username : '')"
                />
                <el-select
                  v-model="filters.token_id"
                  placeholder="密钥"
                  filterable
                  clearable
                  size="small"
                  :disabled="!filters.user_id"
                >
                  <el-option
                    v-for="tk in tokens"
                    :key="tk.ID"
                    :label="tk.DisplayName || tk.TokenDisplay"
                    :value="tk.ID"
                  />
                </el-select>
              </div>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.UserNickname || row.Username || '-' }}</span>
              <span class="t-clock">{{ row.UserNickname && row.Username ? '@' + row.Username + ' · ' : '' }}{{ row.TokenName || '-' }}</span>
            </div>
          </template>
        </el-table-column>

        <!-- 模型 / 内部：内部模型 ID 仅管理端可见（开发者端 DTO 不返回，也不猜） -->
        <el-table-column :label="isSelf ? '模型' : '模型 / 内部'" min-width="140">
          <template #header>
            <ColumnFilter
              label="模型 / 内部"
              :active="!!filters.model"

              @clear="() => (filters.model = undefined)"
            >
              <el-select
                v-model="filters.model"
                placeholder="选择模型"
                filterable
                clearable
                size="small"
              >
                <el-option v-for="m in models" :key="m.ID" :label="m.ExternalName" :value="m.ExternalName" />
                <el-option
                  v-for="n in autoNames"
                  :key="'auto-' + n"
                  :label="n + '（Auto）'"
                  :value="n"
                />
              </el-select>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">
                {{ row.ExternalModel || '-' }}
                <el-tag v-if="row.AutoModelName" size="small" effect="plain" type="success">Auto {{ row.AutoModelName }}</el-tag>
              </span>
              <span v-if="!isSelf" class="t-clock">{{ row.InternalModelID || '-' }}</span>
            </div>
          </template>
        </el-table-column>

        <!-- 渠道 / 密钥：渠道显示名开发者端也可见（白名单），密钥维度仅管理端 -->
        <el-table-column :label="isSelf ? '渠道' : '渠道 / 密钥'" min-width="150">
          <template #header>
            <!-- 开发者端没有渠道/密钥列表接口（也无该维度权限），表头只留纯文本 -->
            <span v-if="isSelf">渠道</span>
            <ColumnFilter
              v-else
              label="渠道 / 密钥"
              :active="!!filters.channel_id || !!filters.channel_key_id"

              @clear="clearChannelKey"
            >
              <div class="f-stack">
                <el-select
                  v-model="filters.channel_id"
                  placeholder="渠道"
                  filterable
                  clearable
                  size="small"
                  @change="onChannelChange"
                >
                  <el-option v-for="c in channels" :key="c.ID" :label="c.Name" :value="c.ID" />
                </el-select>
                <el-select
                  v-model="filters.channel_key_id"
                  placeholder="密钥"
                  filterable
                  clearable
                  size="small"
                  :disabled="!filters.channel_id"
                >
                  <el-option v-for="k in channelKeys" :key="k.ID" :label="k.Name" :value="k.ID" />
                </el-select>
              </div>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ rowChannelLabel(row) }}</span>
              <span v-if="!isSelf" class="t-clock">{{ keyLabel(row) }}</span>
            </div>
          </template>
        </el-table-column>

        <!-- 会话（带搜索下拉） -->
        <el-table-column label="会话" min-width="120">
          <template #header>
            <ColumnFilter
              label="会话"
              :active="!!filters.session_id"

              @clear="() => (filters.session_id = undefined)"
            >
              <el-select
                :model-value="filters.session_id ?? undefined"
                placeholder="搜索会话"
                filterable
                remote
                clearable
                size="small"
                :remote-method="(q: string) => searchSessions(q)"
                @update:model-value="(v: any) => (filters.session_id = v ?? undefined)"
              >
                <el-option v-for="s in sessions" :key="s.SessionID" :label="sessionLabel(s)" :value="s.SessionID" />
              </el-select>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <span class="tok">{{ row.SessionName || '-' }}</span>
          </template>
        </el-table-column>

        <!-- Token（双行：输入/输出 + 缓存读） -->
        <el-table-column label="Token" min-width="150" align="right">
          <template #default="{ row }">
            <div class="t-time right">
              <span class="t-date">{{ tokenLine1(row) }}</span>
              <span v-if="tokenLine2(row)" class="t-clock">{{ tokenLine2(row) }}</span>
            </div>
          </template>
        </el-table-column>

        <!-- 耗时 -->
        <el-table-column label="耗时" min-width="105" align="right">
          <template #default="{ row }">
            <el-tooltip placement="top" :disabled="tokenRate(row) === '-'">
              <template #content>
                <div>速率：{{ tokenRate(row) }}</div>
              </template>
              <div class="t-time right">
                <span class="t-date">首字 {{ fmtMs(row.FirstTokenMs) }}</span>
                <span class="t-clock">总 {{ fmtMs(row.DurationMs) }}</span>
              </div>
            </el-tooltip>
          </template>
        </el-table-column>

        <!-- 状态 -->
        <el-table-column label="状态" min-width="80">
          <template #header>
            <ColumnFilter
              label="状态"
              :active="!!filters.status"

              @clear="() => (filters.status = undefined)"
            >
              <el-select v-model="filters.status" placeholder="账单状态" clearable size="small">
                <el-option label="成功" value="completed" />
                <el-option label="失败" value="failed" />
              </el-select>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <span :class="['st', row.Status === 'completed' ? 'st-ok' : 'st-fail']">
              {{ row.Status === 'completed' ? '成功' : '失败' }}
            </span>
          </template>
        </el-table-column>        <!-- 积分 -->
        <el-table-column label="积分" min-width="90" align="right">
          <template #default="{ row }">
            <el-tooltip placement="top">
              <template #content>
                <div style="text-align: left">
                  <template v-if="row.Status !== 'completed'">
                    <div class="tip-fail">{{ row.ErrorMessage || '失败' }}</div>
                  </template>
                  <template v-else>
                    <div v-for="line in creditLines(row)" :key="line">{{ line }}</div>
                  </template>
                </div>
              </template>
              <span class="credit" :class="{ fail: row.Status !== 'completed' }">
                {{ row.CreditsConsumed ?? '-' }}
              </span>
            </el-tooltip>
          </template>
        </el-table-column>
      </el-table>

      <!-- 窄屏：卡片式列表（宽表在手机上必然横向撑破，这里换成单列卡片） -->
      <div v-else class="bill-cards">
        <div v-for="row in list" :key="row.BillingID" class="bill-card" @click="openDetail(row)">
          <div class="bc-head">
            <span class="bc-time">{{ fmtDate(row.CallTime) }} {{ fmtTime(row.CallTime) }}</span>
            <span :class="['st', row.Status === 'completed' ? 'st-ok' : 'st-fail']">
              {{ row.Status === 'completed' ? '成功' : '失败' }}
            </span>
          </div>
          <div class="bc-line">
            <span class="bc-model">{{ row.ExternalModel || '-' }}</span>
            <el-tag v-if="row.AutoModelName" size="small" effect="plain" type="success">
              Auto {{ row.AutoModelName }}
            </el-tag>
          </div>
          <div class="bc-sub">
            {{ rowChannelLabel(row) }}<template v-if="!isSelf && row.KeyName"> · {{ row.KeyName }}</template>
          </div>
          <div class="bc-sub">{{ tokenLine1(row) }}</div>
          <div class="bc-foot">
            <span class="credit" :class="{ fail: row.Status !== 'completed' }">
              {{ row.CreditsConsumed ?? '-' }} 积分
            </span>
            <span class="bc-reqid mono" @click.stop="copyReqId(row.RequestID)">
              {{ row.RequestID ? shortReqId(row.RequestID) : '-' }}
            </span>
          </div>
          <div v-if="(row.Attempts || 1) > 1" class="bc-attempts" @click.stop="toggleCard(row)">
            <el-tag size="small" type="warning" effect="plain">{{ row.Attempts }} 次尝试</el-tag>
            <span class="bc-toggle">{{ openCards[row.BillingID] ? '收起' : '展开子记录' }}</span>
          </div>
          <div v-if="openCards[row.BillingID]" class="bc-children" @click.stop>
            <div v-if="childLoading(row)" class="sub-loading">正在加载该请求的尝试记录…</div>
            <div v-else-if="!childrenOf(row).length" class="sub-loading">该请求暂无子记录</div>
            <div v-for="c in childrenOf(row)" :key="c.BillingID" class="bc-child" @click="openDetail(c)">
              <span>{{ fmtTime(c.CallTime) }}</span>
              <span :class="['st', c.Status === 'completed' ? 'st-ok' : 'st-fail']">
                {{ c.Status === 'completed' ? '成功' : '失败' }}
              </span>
              <span class="bc-child-credit">{{ c.CreditsConsumed ?? '-' }}</span>
            </div>
          </div>
        </div>
        <el-empty v-if="!loading && !list.length" description="暂无账单" :image-size="60" />
      </div>

      <div class="pager">
        <!-- Total 语义已改为「请求数」（后端按 request_id 归并后计数），文案必须跟 -->
        <span class="pager-total" title="按一次请求归并后的条数；展开主行可看该请求的多条尝试记录">
          共 {{ total }} 个请求
        </span>
        <el-pagination
          layout="prev, pager, next"
          :total="total"
          :page-size="size"
          :current-page="page"
          :small="isNarrow"
          @current-change="(p: number) => { page = p; load() }"
        />
      </div>
      <el-empty v-if="!loading && !list.length && !isNarrow" description="暂无账单" />
      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
    </div>
  </div>
</template>

<style scoped>
.stats-bar {
  display: flex;
  align-items: center;
  gap: 28px;
  margin-bottom: 12px;
  padding: 10px 16px;
}
.stat {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.stat-label {
  font-size: 12px;
  color: var(--color-text-tertiary);
}
.stat-value {
  font-size: 18px;
  font-weight: 700;
  font-variant-numeric: tabular-nums;
  color: var(--color-text);
}
.stat-line {
  display: flex;
  align-items: baseline;
  gap: 6px;
}
.stat-note {
  font-size: 11px;
  color: var(--color-text-tertiary);
}
.pager {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 12px;
  margin-top: 12px;
}
/* 筛选浮窗内控件堆叠 */
.f-stack {
  display: flex;
  flex-direction: column;
  gap: 8px;
  width: 100%;
}
/* A/B 双行 */
.t-time {
  display: flex;
  flex-direction: column;
  line-height: 1.35;
}
.t-date {
  font-size: 12px;
  color: var(--color-text);
}
.t-clock {
  font-size: 12px;
  color: var(--color-text-tertiary);
  font-variant-numeric: tabular-nums;
}
.tok {
  font-size: 12px;
  color: var(--color-text-secondary);
  font-variant-numeric: tabular-nums;
}
.t-time.right {
  align-items: flex-end;
}
.st {
  font-size: 12px;
}
.st-ok {
  color: var(--color-success, #67c23a);
}
.st-fail {
  color: #f56c6c;
}
.credit {
  font-weight: 600;
  color: var(--brand-primary);
  font-variant-numeric: tabular-nums;
  cursor: help;
}
.credit.fail {
  color: var(--color-text-tertiary);
}
/* ===== request_id 搜索入口 ===== */
.req-search {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-right: 4px;
}
.req-search :deep(.el-input) {
  width: 200px;
}
/* 请求 ID 单元格 */
.req-cell {
  display: flex;
  flex-direction: column;
  gap: 2px;
  align-items: flex-start;
}
.req-id {
  font-size: 12px;
  color: var(--color-text-secondary);
  cursor: pointer;
  border-bottom: 1px dashed var(--color-border-strong);
}
.req-id:hover {
  color: var(--brand-primary);
}
/* ===== 展开的子记录（同一请求的多条候选/重试账单） ===== */
.sub-wrap {
  padding: 6px 12px 10px 44px;
  background: var(--color-primary-light-9, #eef0fe);
}
.sub-loading {
  font-size: 12px;
  color: var(--color-text-tertiary);
  padding: 6px 2px;
}
.sub-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 12px;
}
.sub-table th {
  text-align: left;
  font-weight: 600;
  color: var(--color-text-secondary);
  padding: 4px 8px;
  border-bottom: 1px solid var(--color-border);
  white-space: nowrap;
}
.sub-table td {
  padding: 5px 8px;
  border-bottom: 1px dashed var(--color-border);
  color: var(--color-text);
  vertical-align: top;
}
.sub-table .r {
  text-align: right;
}
.sub-row {
  cursor: pointer;
}
.sub-row:hover td {
  background: #fff;
}
.sub-dim {
  display: block;
  color: var(--color-text-tertiary);
  font-size: 11px;
}
.sub-reason {
  color: var(--color-danger);
  max-width: 260px;
  word-break: break-word;
  overflow-wrap: anywhere;
}
/* ===== 窄屏卡片列表 ===== */
.bill-cards {
  display: grid;
  gap: 10px;
}
.bill-card {
  border: 1px solid var(--color-border);
  border-radius: 10px;
  padding: 10px 12px;
  display: grid;
  gap: 5px;
  min-width: 0;
  cursor: pointer;
}
.bill-card:active {
  background: var(--color-primary-light-9, #eef0fe);
}
.bc-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}
.bc-time {
  font-size: 14px;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
}
.bc-line {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  min-width: 0;
}
.bc-model {
  font-size: 14px;
  font-weight: 600;
  word-break: break-word;
  overflow-wrap: anywhere;
}
.bc-sub {
  font-size: 13px;
  color: var(--color-text-tertiary);
  word-break: break-word;
  overflow-wrap: anywhere;
}
.bc-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  font-size: 14px;
}
.bc-reqid {
  font-size: 12px;
  color: var(--color-text-secondary);
  border-bottom: 1px dashed var(--color-border-strong);
}
.bc-attempts {
  display: flex;
  align-items: center;
  gap: 8px;
}
.bc-toggle {
  font-size: 13px;
  color: var(--brand-primary);
}
.bc-children {
  border-top: 1px dashed var(--color-border);
  padding-top: 6px;
  display: grid;
  gap: 4px;
}
.bc-child {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 13px;
  color: var(--color-text-secondary);
}
.bc-child-credit {
  margin-left: auto;
  font-variant-numeric: tabular-nums;
}
.pager {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 12px;
  margin-top: 12px;
}
.pager-total {
  font-size: 13px;
  color: var(--color-text-secondary);
}
@media (max-width: 768px) {
  .page-container {
    padding: 12px 12px 20px;
  }
  .card {
    padding: 14px 12px;
  }
  .stats-bar {
    gap: 16px;
    flex-wrap: wrap;
    padding: 10px 12px;
  }
  .stat-value {
    font-size: 16px;
  }
  .stat-label {
    font-size: 12px;
  }
  .req-search :deep(.el-input) {
    width: 100%;
  }
  .req-search {
    width: 100%;
  }
  .pager {
    justify-content: space-between;
  }
  .pager-total {
    font-size: 14px;
  }
  /* 移动端正文字号不低于 14px（辅助信息同样抬到 14px，避免手机上读不清） */
  .bc-sub,
  .bc-reqid,
  .bc-child,
  .bc-toggle,
  .sub-loading {
    font-size: 14px;
  }
}
:deep(.el-table__header th) {
  padding: 4px 6px;
}
</style>
<style>
.tip-fail {
  color: #f56c6c;
  white-space: pre-line;
  line-height: 1.5;
}
</style>