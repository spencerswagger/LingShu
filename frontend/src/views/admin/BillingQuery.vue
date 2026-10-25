<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
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

const route = useRoute()
const router = useRouter()

// mode='self' 时复用为开发者自助账单页（API 走 /dev 前缀）
const props = withDefaults(defineProps<{ mode?: 'admin' | 'self' }>(), { mode: 'admin' })
const isSelf = computed(() => props.mode === 'self')
const base = computed(() => (isSelf.value ? '/dev' : '/admin'))

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

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  const params = buildParams({ Page: page.value, Size: size.value })
  const statsParams = buildParams()
  try {
    const [billRes, statsRes] = await Promise.all([
      listBillings(params, base.value),
      getBillingStats(statsParams, base.value),
    ])
    list.value = billRes.Data.List
    total.value = billRes.Data.Total
    stats.value = statsRes.Data
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
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
  () => [route.query.range, route.query.user_id],
  ([r, u]) => {
    const rng = resolveBillingRange(r)
    if (rng) filters.range = rng
    filters.user_id = u ? String(u) : undefined
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
    filters.range
  )
  filters.user_id = undefined
  filters.token_id = undefined
  filters.channel_id = undefined
  filters.channel_key_id = undefined
  filters.session_id = undefined
  filters.model = undefined
  filters.status = undefined
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

// 统计栏：平均 RPM/TPM 采用后端忙时口径（分子只算有耗时的请求，分母是这批请求的耗时之和）。
// 有数据即显示：BusyRequests>0 且耗时合计>0 才出数，否则显示 —；
// 样本少（请求数 < 20 或耗时合计 < 30 秒）时照常显示数值，只在旁边灰字标注「样本较少」。
function busyRateReady(): boolean {
  const s = stats.value
  return (s.BusyRequests || 0) > 0 && (s.DurationTotalMS || 0) > 0
}
const lowSample = computed(() => {
  const s = stats.value
  return (s.BusyRequests || 0) < 20 || (s.DurationTotalMS || 0) < 30000
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
        <span class="stat-label" title="每分钟平均请求数">平均 RPM</span>
        <span class="stat-line">
          <span class="stat-value">{{ avgRPM() }}</span>
          <span v-if="busyRateReady() && lowSample" class="stat-note">样本较少</span>
        </span>
      </div>
      <div class="stat">
        <span class="stat-label" title="每分钟平均 Token 数">平均 TPM</span>
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
      />
      <el-table
        :data="list"
        border
        stripe
        class="table-nowrap clickable-rows"
        row-key="BillingID"
        @row-click="(row: AdminBillingItem) => router.push(isSelf ? `/dev/billings/${row.BillingID}` : `/admin/billings/${row.BillingID}`)"
      >
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

        <!-- 模型 -->
        <el-table-column label="模型 / 内部" min-width="140">
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
              <span class="t-clock">{{ row.InternalModelID || '-' }}</span>
            </div>
          </template>
        </el-table-column>

        <!-- 渠道 / 密钥；self 模式不展示 -->
        <el-table-column v-if="!isSelf" label="渠道 / 密钥" min-width="150">
          <template #header>
            <ColumnFilter
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
              <span class="t-date">{{ row.ChannelName || '-' }}</span>
              <span class="t-clock">{{ keyLabel(row) }}</span>
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
        </el-table-column>

        <!-- 积分 -->
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

      <div class="pager">
        <el-pagination
          layout="total, prev, pager, next"
          :total="total"
          :page-size="size"
          :current-page="page"
          @current-change="(p: number) => { page = p; load() }"
        />
      </div>
      <el-empty v-if="!loading && !list.length" description="暂无账单" />
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
  justify-content: flex-end;
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