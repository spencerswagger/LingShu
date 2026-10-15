<script setup lang="ts">
import { computed, nextTick, onMounted, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Edit } from '@element-plus/icons-vue'
import { listSessions, kickSessions, renameSession, type AdminSession } from '@/api/admin'
import { fmtDate, fmtDateTime, fmtTime } from '@/utils/format'
import ErrorBubble from '@/components/ErrorBubble.vue'
import UserSelect from '@/components/UserSelect.vue'
import ColumnFilter from '@/components/ColumnFilter.vue'
import FilterBar from '@/components/FilterBar.vue'

const props = withDefaults(defineProps<{ mode?: 'admin' | 'self' }>(), { mode: 'admin' })
const isSelf = computed(() => props.mode === 'self')
const router = useRouter()

const loading = ref(false)
const list = ref<AdminSession[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const selected = ref<AdminSession[]>([])
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const editingId = ref('')
const editingName = ref('')
const editingInputs = ref<Record<string, any>>({})
// 当前所选用户名（用于筛选标签展示）
const userName = ref('')

const filters = reactive({
  q: '',
  user_id: undefined as string | undefined,
  token_id: undefined as string | undefined,
  channel_key_id: undefined as string | undefined,
})

// 生效中的筛选条件标签（用于统一工具栏展示与逐个移除）
const activeTags = computed(() => {
  const out: { key: string; label: string }[] = []
  if (filters.q.trim()) out.push({ key: 'q', label: `会话 ID: ${filters.q.trim()}` })
  if (filters.user_id) out.push({ key: 'user_id', label: `用户: ${userName.value || filters.user_id}` })
  if (filters.token_id && filters.token_id.trim())
    out.push({ key: 'token_id', label: `密钥: ${filters.token_id.trim()}` })
  if (filters.channel_key_id && filters.channel_key_id.trim())
    out.push({ key: 'channel_key_id', label: `渠道密钥: ${filters.channel_key_id.trim()}` })
  return out
})

// 被 watch 监听、变化即回到第一页并重载的筛选字段
const WATCHED_KEYS = ['user_id', 'token_id', 'channel_key_id'] as const

// 关闭单个筛选标签：被监听字段由 watch 触发重载，会话 ID 手动触发
function clearTag(key: string) {
  if (key === 'q') {
    filters.q = ''
    onSearch()
    return
  }
  if (key === 'user_id') filters.user_id = undefined
  if (key === 'token_id') filters.token_id = undefined
  if (key === 'channel_key_id') filters.channel_key_id = undefined
}

// 重置全部筛选：若存在被监听字段则由 watch 触发重载，否则手动重载，避免重复请求
function resetFilters() {
  const watchedActive = WATCHED_KEYS.some((k) => !!filters[k])
  filters.q = ''
  filters.user_id = undefined
  filters.token_id = undefined
  filters.channel_key_id = undefined
  if (!watchedActive) {
    page.value = 1
    load()
  }
}

function setInputRef(el: any, id: string) {
  if (el) editingInputs.value[id] = el
}

function startRename(row: AdminSession) {
  editingId.value = row.SessionID
  editingName.value = row.SessionName || ''
  nextTick(() => {
    editingInputs.value[row.SessionID]?.focus()
  })
}

async function saveRename(row: AdminSession) {
  const id = editingId.value
  if (!id) return
  editingId.value = ''
  const name = editingName.value.trim()
  if (!name) {
    load()
    return
  }
  if (name === row.SessionName) return
  try {
    await renameSession(id, name)
    row.SessionName = name
    ElMessage.success('已重命名')
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
    load()
  }
}

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  const params: Record<string, any> = { Page: page.value, Size: size.value }
  if (filters.q.trim()) params.Q = filters.q.trim()
  if (filters.user_id) params.UserID = filters.user_id
  if (filters.token_id && filters.token_id.trim()) params.TokenID = filters.token_id.trim()
  if (filters.channel_key_id && filters.channel_key_id.trim()) params.ChannelKeyID = filters.channel_key_id.trim()
  try {
    const res = await listSessions(params, isSelf.value ? '/dev' : '/admin')
    list.value = res.Data.List || []
    total.value = res.Data.Total || 0
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
// 用户/令牌/密钥筛选变化自动刷新；会话 ID 由输入框 change 触发
watch(
  () => `${filters.user_id}|${filters.token_id}|${filters.channel_key_id}`,
  () => {
    page.value = 1
    load()
  },
)
function onSearch() {
  page.value = 1
  load()
}
onMounted(load)

async function close(ids: string[], label: string) {
  if (isSelf.value) return
  try {
    await ElMessageBox.confirm(
      `确认关闭选中的 ${label} 会话吗？关闭后该会话不再被请求命中（并发槽释放），记录保留在列表中。`,
      '关闭确认',
      { type: 'warning', confirmButtonText: '关闭', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    const res = await kickSessions({ SessionIDs: ids })
    ElMessage.success(`已关闭 ${res.Data.Affected} 个会话`)
    selected.value = []
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '关闭失败')
  }
}

function onCloseOne(row: AdminSession) {
  close([row.SessionID], '该')
}
function onCloseBatch() {
  if (!selected.value.length) return
  close(selected.value.map((r) => r.SessionID), `${selected.value.length} 个`)
}

function goDetail(row: AdminSession) {
  router.push({
    name: isSelf.value ? 'dev-session-detail' : 'admin-session-detail',
    params: { id: row.SessionID },
  })
}

// ===== 实时状态 =====
type DotState = 'requesting' | 'idle' | 'aging' | 'stale'

// 会话状态圆点：绿=正在请求（有进行中请求）；蓝=无请求（存活空闲）；
// 黄=已消耗 TTL 的 25%；灰=已消耗 TTL 的 50%（含已过期/已关闭）。
function dotState(row: AdminSession): { key: DotState; label: string } {
  if (row.Closed) return { key: 'stale', label: '已关闭' }
  if (row.Inflight > 0) return { key: 'requesting', label: '正在请求' }
  const last = new Date(row.LastActive).getTime()
  const exp = new Date(row.ExpireAt).getTime()
  const now = Date.now()
  const span = exp - last
  const ratio = span > 0 ? (now - last) / span : 1
  if (row.Expired || now >= exp) return { key: 'stale', label: '已过期' }
  if (ratio >= 0.5) return { key: 'stale', label: '已消耗 TTL 50%' }
  if (ratio >= 0.25) return { key: 'aging', label: '已消耗 TTL 25%' }
  return { key: 'idle', label: '无请求' }
}

// 状态为进行中（未关闭且未过期）才显示关闭操作。
function canClose(row: AdminSession): boolean {
  return !row.Closed && !row.Expired
}

// 数值容错：后端字段缺失（如版本不一致）时按 0 处理，避免渲染报错。
const num = (v: unknown) => Number(v ?? 0)
const intVal = (v: unknown) => Math.round(num(v))
const fmtInt = (n: unknown) => num(n).toLocaleString('zh-CN')
const fmtMoney = (n: unknown) =>
  num(n).toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 })
const compact = (n: unknown) => {
  const v = num(n)
  const abs = Math.abs(v)
  if (abs >= 1e9) return `${(v / 1e9).toFixed(2)}B`
  if (abs >= 1e6) return `${(v / 1e6).toFixed(2)}M`
  if (abs >= 1e3) return `${(v / 1e3).toFixed(2)}K`
  if (abs >= 1) return v.toFixed(abs >= 100 ? 0 : 1)
  return v.toFixed(2)
}
const pct = (v: unknown) => `${Math.round(num(v) * 100)}%`
</script>

<template>
  <div class="page-container">
    <div v-loading="loading" class="card">
      <FilterBar
        :tags="activeTags"
        :loading="loading"
        @clear="clearTag"
        @reset="resetFilters"
        @refresh="load"
      >
        <template #actions>
          <el-button v-if="!isSelf && selected.length" type="danger" size="small" @click="onCloseBatch">
            批量关闭
          </el-button>
        </template>
      </FilterBar>
      <el-table
        :data="list"
        border
        stripe
        class="table-nowrap"
        row-key="SessionID"
        @selection-change="(rows: AdminSession[]) => (selected = rows)"
      >
        <el-table-column v-if="!isSelf" type="selection" width="44" />
        <el-table-column label="会话名称 / 模型" min-width="200">
          <template #header>
            <ColumnFilter
              label="会话 ID"
              :active="!!filters.q"
              @clear="() => { filters.q = ''; onSearch() }"
            >
              <el-input
                v-model="filters.q"
                placeholder="会话 ID 前缀"
                clearable
                size="small"
                @change="onSearch"
                @keyup.enter="(e: KeyboardEvent) => (e.target as HTMLInputElement)?.blur()"
              />
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <el-input
                v-if="editingId === row.SessionID"
                v-model="editingName"
                :ref="(el: any) => setInputRef(el, row.SessionID)"
                size="small"
                maxlength="100"
                placeholder="输入会话名称"
                @click.stop
                @keyup.enter="saveRename(row)"
                @blur="saveRename(row)"
              />
              <span v-else-if="!isSelf" class="cell-name" @click="startRename(row)">
                <span class="name-text">{{ row.SessionName || '未命名' }}</span>
                <el-icon class="name-edit"><Edit /></el-icon>
              </span>
              <span v-else class="cell-name-plain">
                <span class="name-text">{{ row.SessionName || '未命名' }}</span>
              </span>
              <span class="t-clock">{{ row.Model || '-' }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column v-if="!isSelf" label="用户 / 令牌" min-width="150">
          <template #header>
            <ColumnFilter
              label="用户"
              :active="!!filters.user_id"
              @clear="() => (filters.user_id = undefined)"
            >
              <UserSelect
                v-model="filters.user_id"
                placeholder="按用户名搜索"
                @select="(u) => (userName = u ? u.Nickname || u.Username : '')"
              />
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.UserNickname || row.UserName || '-' }}</span>
              <span class="t-clock">{{ row.UserNickname && row.UserName ? '@' + row.UserName + ' · ' : '' }}{{ row.TokenName || '-' }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column v-if="!isSelf" label="渠道 / 密钥" min-width="150" show-overflow-tooltip>
          <template #header>
            <ColumnFilter
              label="渠道-密钥"
              :active="!!filters.channel_key_id"
              @clear="() => (filters.channel_key_id = undefined)"
            >
              <el-input
                v-model="filters.channel_key_id"
                placeholder="渠道密钥 ID"
                clearable
                size="small"
                @change="onSearch"
              />
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.ChannelName || '-' }}</span>
              <span class="t-clock">{{ row.ChannelKeyName || '-' }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="过期时间" min-width="110">
          <template #default="{ row }">
            <el-tooltip placement="top" :show-after="120">
              <template #content>
                <div>创建：{{ fmtDateTime(row.CreatedAt) }}</div>
                <div>最近活跃：{{ fmtDateTime(row.LastActive) }}</div>
              </template>
              <div class="t-time">
                <span class="t-date">{{ fmtDate(row.ExpireAt) }}</span>
                <span class="t-clock">{{ fmtTime(row.ExpireAt) }}</span>
              </div>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="实时状态" min-width="150">
          <template #default="{ row }">
            <el-popover placement="left" trigger="hover" :width="288" :show-after="120">
              <template #reference>
                <div class="rs-cell">
                  <span class="dot-glow" :data-state="dotState(row).key" />
                  <div class="rs-text">
                    <span class="rs-main">{{ compact(row.ContextLen) }} · {{ intVal(row.Credits) }}积分</span>
                    <span class="rs-sub">{{ compact(row.Tokens) }} · {{ pct(row.CacheHitRate) }}</span>
                  </div>
                </div>
              </template>
              <div class="rs-pop">
                <div class="rs-pop-title">
                  <span class="dot-glow" :data-state="dotState(row).key" />
                  <span>{{ dotState(row).label }}</span>
                </div>
                <div class="rs-pop-grid">
                  <div class="rs-pop-item"><span>总调用次数</span><b>{{ fmtInt(row.Calls) }}</b></div>
                  <div class="rs-pop-item"><span>总消耗 Token</span><b>{{ compact(row.Tokens) }}</b></div>
                  <div class="rs-pop-item"><span>总消耗积分</span><b>{{ fmtMoney(row.Credits) }}</b></div>
                  <div class="rs-pop-item"><span>上下文长度</span><b>{{ compact(row.ContextLen) }}</b></div>
                  <div class="rs-pop-item"><span>RPM</span><b>{{ num(row.RPM).toFixed(2) }}</b></div>
                  <div class="rs-pop-item"><span>TPM</span><b>{{ compact(num(row.TPM)) }}</b></div>
                  <div class="rs-pop-item"><span>用户消息次数</span><b>{{ fmtInt(row.UserMsgs) }}</b></div>
                  <div class="rs-pop-item"><span>工具调用次数</span><b>{{ fmtInt(row.ToolCalls) }}</b></div>
                  <div class="rs-pop-item rs-pop-wide"><span>缓存命中率</span><b>{{ pct(row.CacheHitRate) }}</b></div>
                </div>
              </div>
            </el-popover>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="108" align="right" fixed="right">
          <template #default="{ row }">
            <el-button
              link
              type="primary"
              size="small"
              @click="goDetail(row)"
            >
              详情
            </el-button>
            <el-button v-if="!isSelf && canClose(row)" link type="danger" size="small" @click="onCloseOne(row)">关闭</el-button>
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
      <el-empty v-if="!loading && !list.length" description="暂无会话" />
      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
    </div>
  </div>
</template>

<style scoped>
.mono {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
}
.pager {
  display: flex;
  justify-content: flex-end;
  margin-top: 16px;
}
.t-time {
  display: flex;
  flex-direction: column;
  line-height: 1.35;
}
.t-date {
  font-size: 12px;
  color: var(--color-text);
  line-height: 1.3;
  font-variant-numeric: tabular-nums;
}
.t-clock {
  font-size: 12px;
  color: var(--color-text-tertiary);
  font-variant-numeric: tabular-nums;
}
.cell-name {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  cursor: pointer;
  max-width: 100%;
  color: var(--color-primary, #409eff);
}
.cell-name-plain {
  display: inline-flex;
  align-items: center;
  max-width: 100%;
}
.name-text {
  display: inline-block;
  max-width: 200px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.name-edit {
  font-size: 13px;
  color: var(--color-text-secondary);
}

/* 实时状态单元格：光效圆点 + 概要（明细见悬停浮层） */
.rs-cell {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.rs-text {
  display: flex;
  flex-direction: column;
  line-height: 1.35;
  min-width: 0;
}
.rs-main {
  font-size: 12px;
  color: var(--color-text);
}
.rs-sub {
  font-size: 12px;
  color: var(--color-text-tertiary);
  font-variant-numeric: tabular-nums;
}

/* 光效圆点：绿=正在请求（脉冲光晕）· 蓝=无请求 · 黄=已消耗 TTL 25% · 灰=已消耗 TTL 50%/已过期/已关闭 */
.dot-glow {
  flex-shrink: 0;
  width: 9px;
  height: 9px;
  border-radius: 50%;
  background: #98a2b3;
}
.dot-glow[data-state='requesting'] {
  background: #12b76a;
  animation: dot-pulse 1.4s ease-out infinite;
}
.dot-glow[data-state='idle'] {
  background: #0ea5e9;
  box-shadow: 0 0 6px rgba(14, 165, 233, 0.55);
}
.dot-glow[data-state='aging'] {
  background: #f79009;
  box-shadow: 0 0 6px rgba(247, 144, 9, 0.5);
}
.dot-glow[data-state='stale'] {
  background: #98a2b3;
}
@keyframes dot-pulse {
  0% { box-shadow: 0 0 0 0 rgba(18, 183, 106, 0.6); }
  70% { box-shadow: 0 0 0 7px rgba(18, 183, 106, 0); }
  100% { box-shadow: 0 0 0 0 rgba(18, 183, 106, 0); }
}

/* 实时状态悬停明细 */
.rs-pop-title {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 10px;
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text);
}
.rs-pop-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 8px 14px;
}
.rs-pop-item {
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: 12px;
  color: var(--color-text-secondary);
}
.rs-pop-item b {
  font-size: 13px;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
}
.rs-pop-wide {
  grid-column: 1 / -1;
}
</style>