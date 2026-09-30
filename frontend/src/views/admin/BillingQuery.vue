<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  listBillings,
  getBillingStats,
  listChannels,
  listChannelKeys,
  listExternalModels,
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

const route = useRoute()
const router = useRouter()
const loading = ref(false)
const list = ref<AdminBillingItem[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

const stats = ref<BillingStats>({ requests: 0, credits_total: 0, tokens_total: 0, duration_total_ms: 0 })

// ===== 列头筛选选项数据 =====
const channels = ref<AdminChannel[]>([])
const channelKeys = ref<ChannelKey[]>([])
const models = ref<ExternalModel[]>([])
const tokens = ref<AdminToken[]>([])
const sessions = ref<AdminSession[]>([])

const filters = reactive({
  user_id: (route.query.user_id ? Number(route.query.user_id) : undefined) as number | undefined,
  token_id: undefined as number | undefined,
  channel_id: undefined as number | undefined,
  channel_key_id: undefined as number | undefined,
  session_id: undefined as string | undefined,
  model: undefined as string | undefined,
  status: undefined as string | undefined,
  range: null as [string, string] | null,
})

function buildParams(extra: Record<string, unknown> = {}): Record<string, unknown> {
  const p: Record<string, unknown> = { ...extra }
  for (const [k, v] of Object.entries(filters)) {
    if (v === undefined || v === null || v === '') continue
    if (k === 'channel_id') continue // 后端按 channel_key_id 过滤，渠道仅作级联
    if (k === 'range') {
      if (filters.range) {
        p.from = filters.range[0]
        p.to = filters.range[1]
      }
      continue
    }
    p[k] = v
  }
  return p
}

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  const params = buildParams({ page: page.value, size: size.value })
  const statsParams = buildParams()
  try {
    const [billRes, statsRes] = await Promise.all([listBillings(params), getBillingStats(statsParams)])
    list.value = billRes.data.list
    total.value = billRes.data.total
    stats.value = statsRes.data
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}

// ===== 选项加载 =====
async function loadChannels() {
  if (channels.value.length) return
  try {
    channels.value = (await listChannels()).data || []
  } catch {
    channels.value = []
  }
}
async function loadModels() {
  if (models.value.length) return
  try {
    models.value = (await listExternalModels({})).data || []
  } catch {
    models.value = []
  }
}
async function loadTokensOfUser() {
  tokens.value = []
  if (!filters.user_id) {
    filters.token_id = undefined
    return
  }
  try {
    const res = await listTokens({ user_id: filters.user_id, page: 1, size: 100 })
    tokens.value = res.data.list || []
  } catch {
    tokens.value = []
  }
}
async function searchSessions(q: string) {
  try {
    const res = await listSessions({ q, page: 1, size: 50 })
    sessions.value = res.data.list || []
  } catch {
    sessions.value = []
  }
}
async function onChannelChange(id?: number) {
  filters.channel_key_id = undefined
  channelKeys.value = []
  if (!id) return
  try {
    channelKeys.value = (await listChannelKeys(id)).data || []
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

function reset() {
  filters.user_id = undefined
  filters.token_id = undefined
  filters.channel_id = undefined
  filters.channel_key_id = undefined
  filters.session_id = undefined
  filters.model = undefined
  filters.status = undefined
  filters.range = null
  channelKeys.value = []
  page.value = 1
  load()
}

onMounted(() => {
  loadChannels()
  loadModels()
  searchSessions('')
  loadTokensOfUser()
  load()
})

// ===== 展示格式化 =====
function fmtDate(iso?: string): string {
  if (!iso) return '-'
  const d = new Date(iso)
  if (isNaN(d.getTime())) return iso
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}
function fmtTime(iso?: string): string {
  if (!iso) return '-'
  const d = new Date(iso)
  if (isNaN(d.getTime())) return '-'
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}:${String(d.getSeconds()).padStart(2, '0')}`
}

// token 用量两行展示：第一行 输入/输出，第二行 缓存读/缓存写/推理（第二行全 0 不显示）
function tokenLine1(row: AdminBillingItem): string {
  const t = row.tokens || ({} as AdminBillingItem['tokens'])
  const parts: string[] = []
  if (t.input || t.input === 0) parts.push(`输入 ${t.input}`)
  if (t.output) parts.push(`输出 ${t.output}`)
  return parts.length ? parts.join(' · ') : '-'
}
function tokenLine2(row: AdminBillingItem): string {
  const t = row.tokens || ({} as AdminBillingItem['tokens'])
  const parts: string[] = []
  if (t.cache_read) parts.push(`缓存读 ${t.cache_read}`)
  if (t.cache_write) parts.push(`缓存写 ${t.cache_write}`)
  if (t.reasoning) parts.push(`推理 ${t.reasoning}`)
  return parts.join(' · ')
}

// 渠道-密钥双行：上渠道名，下密钥名（无 key_name 时 #<id> 兜底）
function keyLabel(row: AdminBillingItem): string {
  return row.key_name || (row.channel_key_id ? `#${row.channel_key_id}` : '-')
}

function fmtMs(ms?: number | null): string {
  if (ms == null) return '-'
  return ms >= 1000 ? `${(ms / 1000).toFixed(2)}s` : `${ms}ms`
}

// 生成速率：输出 token / (总耗时 - 首字耗时)（流式）；非流式无首字 → 输出 / 总耗时。
function tokenRate(row: AdminBillingItem): string {
  const dur = row.duration_ms || 0
  const first = row.first_token_ms
  if (dur <= 0) return '-'
  const gen = first == null || first < 0 ? dur : dur - first
  if (gen <= 0) return '-'
  const out = (row.tokens || {}).output || 0
  if (!out) return '-'
  return `${Math.round((out / gen) * 1000)} tok/s`
}

// 统计栏：平均 RPM = 请求数 / 总耗时(分钟)；平均 TPM = 总 token / 总耗时(分钟)
function avgRPM(): string {
  const min = (stats.value.duration_total_ms || 0) / 60000
  if (!stats.value.requests || min <= 0) return '-'
  return (stats.value.requests / min).toFixed(1)
}
function avgTPM(): string {
  const min = (stats.value.duration_total_ms || 0) / 60000
  if (!stats.value.tokens_total || min <= 0) return '-'
  return Math.round(stats.value.tokens_total / min).toLocaleString()
}
const creditsTotal = computed(() => stats.value.credits_total.toLocaleString(undefined, { maximumFractionDigits: 4 }))
const tokensTotal = computed(() => stats.value.tokens_total.toLocaleString())

// 消耗积分公式（与详情页一致的三步式展示）：Token×单价 → 系数调整 → 积分换算
function creditLines(row: AdminBillingItem): string[] {
  const t = row.tokens || ({} as AdminBillingItem['tokens'])
  const rates = row.rates || {}
  const coeffTime = row.coeff_time ?? 1
  const coeffCtx = row.coeff_context ?? 1
  const r = row.r_value ?? 0

  const segs: Array<[number, string, string]> = [
    [t.input || 0, '输入', 'input'],
    [t.output || 0, '输出', 'output'],
    [t.cache_read || 0, '缓存读', 'cache_read'],
    [t.cache_write || 0, '缓存写', 'cache_write'],
    [t.reasoning || 0, '推理', 'reasoning'],
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
  if (!step1.length) return row.credits_consumed != null ? [`积分 ${row.credits_consumed}`] : ['-']
  sub1 = Math.round(sub1 * 1e6) / 1e6
  const sub2 = Math.round(sub1 * coeffTime * coeffCtx * 1e6) / 1e6
  const final = r ? Math.round((sub2 / r) * 1e6) / 1e6 : sub2
  return [
    `Token × 单价${row.pricing_mode === 'cost' ? '（成本）' : ''}`,
    ...step1,
    `  小计 = ${sub1}`,
    `系数调整：时段 ×${coeffTime}，分档 ×${coeffCtx} → ${sub2}`,
    `积分换算：÷ ${r} (R) = ${final} 积分`,
  ]
}

// 会话选项展示：名称优先，缺失用短 session_id。
function sessionLabel(s: AdminSession): string {
  return s.name || s.session_id.slice(0, 16)
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
        <span class="stat-label" title="请求数 ÷ 符合条件的记录总耗时(分钟)">平均 RPM</span>
        <span class="stat-value">{{ avgRPM() }}</span>
      </div>
      <div class="stat">
        <span class="stat-label" title="总 Token ÷ 符合条件的记录总耗时(分钟)">平均 TPM</span>
        <span class="stat-value">{{ avgTPM() }}</span>
      </div>
      <div class="stat-spacer" />
      <el-button size="small" @click="reset">重置筛选</el-button>
      <el-button type="primary" size="small" :loading="loading" @click="load">刷新</el-button>
    </div>

    <div v-loading="loading" class="card">
      <el-table
        :data="list"
        border
        stripe
        class="table-nowrap clickable-rows"
        row-key="billing_id"
        @row-click="(row: AdminBillingItem) => router.push(`/admin/billings/${row.billing_id}`)"
      >
        <!-- 时间：筛选图标 → 浮窗时间段选择器 -->
        <el-table-column label="时间" min-width="130" align="center">
          <template #header>
            <ColumnFilter label="时间" :width="360" :active="!!filters.range" @clear="() => (filters.range = null)">
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
              <span class="t-date">{{ fmtDate(row.call_time) }}</span>
              <span class="t-clock">{{ fmtTime(row.call_time) }}</span>
            </div>
          </template>
        </el-table-column>

        <!-- 用户 / 密钥：筛选图标 → 浮窗（用户搜索 + 该用户令牌） -->
        <el-table-column label="用户 / 密钥" min-width="150">
          <template #header>
            <ColumnFilter
              label="用户 / 密钥"
              :active="!!filters.user_id || !!filters.token_id"
              @clear="clearUserKey"
            >
              <div class="f-stack">
                <UserSelect v-model="filters.user_id" placeholder="按用户名搜索" clearable />
                <el-select
                  v-model="filters.token_id"
                  placeholder="密钥"
                  filterable
                  clearable
                  size="small"
                  style="width: 100%"
                  :disabled="!filters.user_id"
                >
                  <el-option
                    v-for="tk in tokens"
                    :key="tk.id"
                    :label="tk.display_name || tk.token_display"
                    :value="tk.id"
                  />
                </el-select>
              </div>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.user_nickname || row.username || '-' }}</span>
              <span class="t-clock">{{ row.user_nickname && row.username ? '@' + row.username + ' · ' : '' }}{{ row.token_name || '-' }}</span>
            </div>
          </template>
        </el-table-column>

        <!-- 模型 -->
        <el-table-column label="模型 / 内部" min-width="140">
          <template #header>
            <ColumnFilter label="模型 / 内部" :active="!!filters.model" @clear="() => (filters.model = undefined)">
              <el-select
                v-model="filters.model"
                placeholder="选择模型"
                filterable
                clearable
                size="small"
                style="width: 100%"
              >
                <el-option v-for="m in models" :key="m.id" :label="m.external_name" :value="m.external_name" />
              </el-select>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.model || '-' }}</span>
              <span class="t-clock">{{ row.internal_model_id || '-' }}</span>
            </div>
          </template>
        </el-table-column>

        <!-- 渠道 / 密钥 -->
        <el-table-column label="渠道 / 密钥" min-width="150">
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
                  style="width: 100%"
                  :disabled="!filters.channel_id"
                >
                  <el-option v-for="k in channelKeys" :key="k.id" :label="k.name" :value="k.id" />
                </el-select>
              </div>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.channel_name || '-' }}</span>
              <span class="t-clock">{{ keyLabel(row) }}</span>
            </div>
          </template>
        </el-table-column>

        <!-- 会话（带搜索下拉） -->
        <el-table-column label="会话" min-width="120">
          <template #header>
            <ColumnFilter label="会话" :active="!!filters.session_id" @clear="() => (filters.session_id = undefined)">
              <el-select
                :model-value="filters.session_id ?? undefined"
                placeholder="搜索会话"
                filterable
                remote
                clearable
                size="small"
                style="width: 100%"
                :remote-method="(q: string) => searchSessions(q)"
                @update:model-value="(v: any) => (filters.session_id = v ?? undefined)"
              >
                <el-option v-for="s in sessions" :key="s.session_id" :label="sessionLabel(s)" :value="s.session_id" />
              </el-select>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <span class="tok">{{ row.session_name || '-' }}</span>
          </template>
        </el-table-column>

        <!-- Token（双行：输入/输出 + 缓存/推理） -->
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
                <span class="t-date">首字 {{ fmtMs(row.first_token_ms) }}</span>
                <span class="t-clock">总 {{ fmtMs(row.duration_ms) }}</span>
              </div>
            </el-tooltip>
          </template>
        </el-table-column>

        <!-- 状态 -->
        <el-table-column label="状态" min-width="80">
          <template #header>
            <ColumnFilter label="状态" :active="!!filters.status" @clear="() => (filters.status = undefined)">
              <el-select v-model="filters.status" placeholder="账单状态" clearable size="small" style="width: 100%">
                <el-option label="成功" value="completed" />
                <el-option label="失败" value="failed" />
              </el-select>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <span :class="['st', row.status === 'completed' ? 'st-ok' : 'st-fail']">
              {{ row.status === 'completed' ? '成功' : '失败' }}
            </span>
          </template>
        </el-table-column>

        <!-- 积分 -->
        <el-table-column label="积分" min-width="90" align="right">
          <template #default="{ row }">
            <el-tooltip placement="top">
              <template #content>
                <div style="text-align: left">
                  <template v-if="row.status !== 'completed'">
                    <div class="tip-fail">{{ row.error_message || '失败' }}</div>
                  </template>
                  <template v-else>
                    <div v-for="line in creditLines(row)" :key="line">{{ line }}</div>
                  </template>
                </div>
              </template>
              <span class="credit" :class="{ fail: row.status !== 'completed' }">
                {{ row.credits_consumed ?? '-' }}
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
.stat-spacer {
  flex: 1;
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