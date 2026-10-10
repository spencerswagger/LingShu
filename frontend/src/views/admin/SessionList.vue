<script setup lang="ts">
import { computed, nextTick, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Edit } from '@element-plus/icons-vue'
import {
  listSessions,
  kickSessions,
  renameSession,
  listSessionCalls,
  type AdminSession,
  type CallLogView,
} from '@/api/admin'
import { fmtDate, fmtTime, fmtDateTime } from '@/utils/format'
import ErrorBubble from '@/components/ErrorBubble.vue'
import UserSelect from '@/components/UserSelect.vue'
import ColumnFilter from '@/components/ColumnFilter.vue'
import StatusTag from '@/components/StatusTag.vue'

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

const filters = reactive({
  q: '',
  user_id: undefined as string | undefined,
  token_id: undefined as string | undefined,
  channel_key_id: undefined as string | undefined,
  status: undefined as string | undefined, // active | expired | closed
})

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
  if (filters.status === 'active') params.Expired = 'active'
  if (filters.status === 'expired') params.Expired = 'expired'
  if (filters.status === 'closed') params.Closed = '1'
  try {
    const res = await listSessions(params)
    list.value = res.Data.List || []
    total.value = res.Data.Total || 0
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
// 用户/令牌/密钥/状态筛选变化自动刷新；会话 ID 由输入框 change 触发
watch(
  () => `${filters.user_id}|${filters.token_id}|${filters.channel_key_id}|${filters.status}`,
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

// ===== 会话完整记录 =====
interface TranscriptMsg {
  role?: string
  content?: unknown
}
interface RespInfo {
  text: string
  toolCalls: unknown[]
}
interface TranscriptItem {
  call: CallLogView
  msgs: TranscriptMsg[]
  resp: RespInfo
  channel: { channel: string; key: string; model: string }
}

const callsDlg = ref(false)
const callsLoading = ref(false)
const callsRow = ref<AdminSession | null>(null)
const callsErr = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const calls = ref<CallLogView[]>([])

async function openCalls(row: AdminSession) {
  callsRow.value = row
  calls.value = []
  callsErr.value = { message: '', requestId: '' }
  callsDlg.value = true
  callsLoading.value = true
  try {
    const res = await listSessionCalls(row.SessionID)
    calls.value = res.Data.List || []
  } catch (e: any) {
    callsErr.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    callsLoading.value = false
  }
}

function msgList(v: unknown): TranscriptMsg[] {
  if (!Array.isArray(v)) return []
  return v.map((m) => (m && typeof m === 'object' ? (m as TranscriptMsg) : { content: m }))
}
function msgSide(role?: string) {
  return role === 'user' ? 'right' : 'left'
}

function fmtJson(v: unknown) {
  if (v == null) return ''
  try {
    return JSON.stringify(v, null, 2)
  } catch {
    return String(v)
  }
}

// 解析 RespBody（非流式消息对象 / 流式 assistant 增量数组 / 纯文本）：提取 content 文本与 tool_calls
function respInfo(cl: CallLogView): RespInfo {
  if (!cl.RespBody) return { text: '', toolCalls: [] }
  const out: RespInfo = { text: '', toolCalls: [] }
  const walk = (p: unknown): void => {
    if (p == null) return
    if (typeof p === 'string') {
      if (p) out.text += p
      return
    }
    if (Array.isArray(p)) {
      for (const x of p) walk(x)
      return
    }
    if (typeof p === 'object') {
      const o = p as Record<string, any>
      if (typeof o.content === 'string' && o.content) out.text += o.content
      if (Array.isArray(o.content)) {
        for (const c of o.content) {
          if (typeof c === 'string' && c) out.text += c
          else if (c && typeof c.text === 'string' && c.text) out.text += c.text
        }
      }
      if (Array.isArray(o.tool_calls)) {
        out.toolCalls.push(...o.tool_calls.filter((t: any) => t && (t.function || t.id)))
      }
      // 包裹层（OpenAI choices / message / delta）
      for (const k of ['choices', 'message', 'delta']) {
        const inner = o[k]
        if (Array.isArray(inner)) for (const x of inner) walk(x)
        else if (inner && typeof inner === 'object') walk(inner)
      }
    }
  }
  try {
    walk(JSON.parse(cl.RespBody))
  } catch {
    return { text: cl.RespBody, toolCalls: [] }
  }
  return out
}

// 渠道/密钥/内部模型：优先 Decision.result，缺省回退 attempts 末条
function callChannel(cl: CallLogView): { channel: string; key: string; model: string } {
  const dash = { channel: '-', key: '-', model: '-' }
  const d = cl.Decision as Record<string, any> | null | undefined
  if (!d || typeof d !== 'object') return dash
  const pick = (o: unknown) => {
    const src = o && typeof o === 'object' ? (o as Record<string, any>) : {}
    return {
      channel: String(src.channel_id || src.channel || src.ChannelName || src.channel_name || ''),
      key: String(src.channel_key_id || src.key_id || src.channelKeyID || src.KeyName || ''),
      model: String(src.internal_model_id || src.internalModelID || src.model || src.InternalModelID || ''),
    }
  }
  const fromResult = pick(d.result)
  if (fromResult.channel || fromResult.key || fromResult.model) {
    return { channel: fromResult.channel || '-', key: fromResult.key || '-', model: fromResult.model || '-' }
  }
  const attempts = Array.isArray(d.attempts) ? d.attempts : []
  for (let i = attempts.length - 1; i >= 0; i--) {
    const p = pick(attempts[i])
    if (p.channel || p.key || p.model) {
      return { channel: p.channel || '-', key: p.key || '-', model: p.model || '-' }
    }
  }
  return dash
}

const transcript = computed<TranscriptItem[]>(() =>
  calls.value.map((c) => ({ call: c, msgs: msgList(c.ReqMessages), resp: respInfo(c), channel: callChannel(c) })),
)
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">会话</div>
      <span class="page-count">共 {{ total }} 个会话</span>
      <div class="page-header-spacer" />
      <el-button v-if="selected.length" type="danger" @click="onCloseBatch">批量关闭</el-button>
    </div>

    <div v-loading="loading" class="card">
      <el-table
        :data="list"
        border
        stripe
        class="table-nowrap"
        row-key="SessionID"
        @selection-change="(rows: AdminSession[]) => (selected = rows)"
      >
        <el-table-column type="selection" width="44" />
        <el-table-column label="会话名称 / 模型" min-width="200">
          <template #header>
            <ColumnFilter label="会话 ID" :active="!!filters.q" @clear="() => { filters.q = ''; onSearch() }">
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
              <span v-else class="cell-name" @click="startRename(row)">
                <span class="name-text">{{ row.SessionName || '未命名' }}</span>
                <el-icon class="name-edit"><Edit /></el-icon>
              </span>
              <span class="t-clock">{{ row.Model || '-' }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="用户 / 令牌" min-width="150">
          <template #header>
            <ColumnFilter label="用户" :active="!!filters.user_id" @clear="() => (filters.user_id = undefined)">
              <UserSelect v-model="filters.user_id" placeholder="按用户名搜索" />
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.UserNickname || row.UserName || '-' }}</span>
              <span class="t-clock">{{ row.UserNickname && row.UserName ? '@' + row.UserName + ' · ' : '' }}{{ row.TokenName || '-' }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="渠道-密钥" min-width="120" show-overflow-tooltip>
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
            <span class="cell">{{ row.ChannelKeyName || '-' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="创建 / 最近活跃" min-width="170">
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ fmtDate(row.CreatedAt) }} {{ fmtTime(row.CreatedAt) }}</span>
              <span class="t-clock">{{ fmtDate(row.LastActive) }} {{ fmtTime(row.LastActive) }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="过期时间" min-width="110">
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ fmtDate(row.ExpireAt) }}</span>
              <span class="t-clock">{{ fmtTime(row.ExpireAt) }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="状态" min-width="86" align="center">
          <template #header>
            <ColumnFilter label="状态" :active="!!filters.status" @clear="() => (filters.status = undefined)">
              <el-select v-model="filters.status" placeholder="会话状态" clearable size="small" style="width: 100%">
                <el-option label="进行中" value="active" />
                <el-option label="已过期" value="expired" />
                <el-option label="已关闭" value="closed" />
              </el-select>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <el-tag v-if="row.Closed" type="info" size="small" effect="light">已关闭</el-tag>
            <el-tag v-else-if="row.Expired" type="danger" size="small" effect="light">已过期</el-tag>
            <el-tag v-else type="success" size="small" effect="light">进行中</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="150" align="right" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" size="small" @click="openCalls(row)">完整记录</el-button>
            <el-button link type="danger" size="small" @click="onCloseOne(row)">关闭</el-button>
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

    <!-- 会话完整记录（调用日志）弹窗 -->
    <el-dialog v-model="callsDlg" title="会话完整记录" width="860px" top="6vh" :close-on-click-modal="false">
      <div v-if="callsRow" class="calls-head">
        <span class="calls-name">{{ callsRow.SessionName || '未命名' }}</span>
        <span class="calls-id mono">会话 ID：{{ callsRow.SessionID }}</span>
      </div>
      <div v-loading="callsLoading" class="calls-body">
        <el-empty
          v-if="!callsLoading && !callsErr.message && !calls.length"
          description="该会话暂无调用记录"
        />
        <ErrorBubble
          v-if="callsErr.message || callsErr.requestId"
          :message="callsErr.message"
          :request-id="callsErr.requestId"
        />
        <div v-for="item in transcript" :key="item.call.BillingID || item.call.RequestID || item.call.CallTime" class="call-card">
          <div class="call-head">
            <div class="call-meta">
              <span class="t-date mono">{{ fmtDateTime(item.call.CallTime) }}</span>
              <span class="t-clock">{{ item.call.Model || '-' }}</span>
            </div>
            <div class="call-meta call-channel" title="渠道信息">
              <span class="t-clock">渠道 {{ item.channel.channel }} · 密钥 {{ item.channel.key }} · 内部模型 {{ item.channel.model }}</span>
            </div>
            <div class="call-state">
              <StatusTag :value="item.call.Status" />
              <el-tag :type="item.call.PricingMode === 'cost' ? 'warning' : 'primary'" size="small" effect="plain">
                {{ item.call.PricingMode === 'cost' ? '按成本' : '按售价' }}
              </el-tag>
            </div>
          </div>
          <div v-if="item.msgs.length || item.call.RespBody" class="msgs">
            <div
              v-for="(m, mi) in item.msgs"
              :key="mi"
              class="msg-row"
              :class="msgSide(m.role) === 'right' ? 'is-user' : 'is-other'"
            >
              <div class="msg-bubble">
                <div class="msg-role">{{ m.role || 'message' }}</div>
                <div v-if="typeof m.content === 'string'" class="msg-text">{{ m.content }}</div>
                <pre v-else-if="m.content != null" class="mono msg-json">{{ fmtJson(m.content) }}</pre>
              </div>
            </div>
            <div class="msg-row is-other">
              <div class="msg-bubble">
                <div class="msg-role">{{ item.call.RespKind === 'error' ? '错误' : 'assistant 响应' }}</div>
                <div v-if="item.call.RespKind === 'error' && item.call.ErrorMessage" class="resp-error">
                  {{ item.call.ErrorMessage }}
                </div>
                <template v-else>
                  <div v-if="item.resp.text" class="msg-text">{{ item.resp.text }}</div>
                  <pre v-else class="mono msg-json">{{ item.call.RespBody }}</pre>
                </template>
                <div v-if="item.resp.toolCalls.length" class="tool-calls">
                  <el-collapse>
                    <el-collapse-item
                      v-for="(tc, ti) in item.resp.toolCalls"
                      :key="ti"
                      :name="'tc-' + ti"
                      :title="'tool_calls[' + ti + ']'"
                    >
                      <pre class="mono msg-json">{{ fmtJson(tc) }}</pre>
                    </el-collapse-item>
                  </el-collapse>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
      <template #footer>
        <el-button @click="callsDlg = false">关闭</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.cell {
  font-size: 12px;
  color: var(--color-text-secondary);
}
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
/* 会话完整记录弹窗 */
.calls-head {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 12px;
}
.calls-name {
  font-weight: 600;
  font-size: 14px;
}
.calls-id {
  font-size: 12px;
  color: var(--color-text-secondary);
  word-break: break-all;
}
.calls-body {
  max-height: 60vh;
  overflow-y: auto;
}
.call-card {
  border: 1px solid var(--color-border);
  border-radius: 8px;
  padding: 12px 14px;
  margin-bottom: 12px;
}
.call-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 10px;
}
.call-meta {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.call-channel .t-clock {
  word-break: break-all;
}
.call-state {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-shrink: 0;
}
.msgs {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.msg-row {
  display: flex;
}
.msg-row.is-user {
  justify-content: flex-end;
}
.msg-row.is-other {
  justify-content: flex-start;
}
.msg-bubble {
  max-width: 88%;
  padding: 8px 12px;
  border-radius: 10px;
  background: var(--color-fill, #f6f7fa);
  overflow-wrap: break-word;
}
.msg-row.is-user .msg-bubble {
  background: var(--el-color-primary-light-9, #eef0fe);
}
.msg-role {
  font-size: 11px;
  color: var(--color-text-tertiary);
  margin-bottom: 4px;
}
.msg-text {
  font-size: 13px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
}
.msg-json {
  margin: 0;
  padding: 8px 10px;
  border-radius: 6px;
  background: #101828;
  color: #d0d5dd;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 240px;
  overflow: auto;
}
.resp-error {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-danger);
}
.tool-calls {
  margin-top: 8px;
}
</style>