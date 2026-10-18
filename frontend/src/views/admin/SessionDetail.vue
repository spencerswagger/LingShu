<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { listSessions, listSessionCalls, type AdminSession, type CallLogView } from '@/api/admin'
import { fmtDateTime } from '@/utils/format'
import ErrorBubble from '@/components/ErrorBubble.vue'
import StatusTag from '@/components/StatusTag.vue'
import MessageBubbleGroup from '@/components/MessageBubbleGroup.vue'
import {
  asMessageList,
  parseRespBody,
  contentOfBody,
  type ChatMessage,
  type RespInfo,
} from '@/utils/messageParts'

const props = withDefaults(defineProps<{ mode?: 'admin' | 'self' }>(), { mode: 'admin' })
const isSelf = computed(() => props.mode === 'self')

const route = useRoute()
const router = useRouter()
const sessionId = String(route.params.id)

const loading = ref(false)
const meta = ref<AdminSession | null>(null)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const calls = ref<CallLogView[]>([])

onMounted(async () => {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const [metaRes, callsRes] = await Promise.all([
      listSessions({ Q: sessionId, Size: 1 }, isSelf.value ? '/dev' : '/admin'),
      listSessionCalls(sessionId, isSelf.value ? '/dev' : '/admin'),
    ])
    meta.value = metaRes.Data.List?.[0] || null
    calls.value = callsRes.Data.List || []
    // 会话元信息缺失（已被清理等）：不阻塞调用记录展示
    if (!meta.value) errInfo.value = { message: '会话信息不可用（记录可能已被清理）', requestId: '' }
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
})

// ===== 调用记录 =====
interface TranscriptItem {
  call: CallLogView
  msgs: ChatMessage[]
  resp: RespInfo
  channel: { channel: string; key: string; model: string }
}

// 决策轨迹项（后端 CallLogView.Decision.attempts[]，字段可能随版本扩展，全部可选）。
interface TrailAttempt {
  order?: number
  channel_id?: string
  channel_name?: string
  channel_key_id?: string
  channel_key_name?: string
  internal_model_id?: string
  reason?: string
  // 该候选未进入上游即被排除（排空/禁用/限流/会话满/协议不支持等）
  skipped?: boolean
  // 排除原因的运维动作提示（后端 key 为 hint；兼容 exclusionHint / exclusion_hint 别名）
  hint?: string
  exclusionHint?: string
  exclusion_hint?: string
  // 流式失败诊断（已送达/未送达、是否收到首字节、阶段耗时）
  delivered?: boolean
  first_byte_received?: boolean
  elapsed_ms?: number
  upstream_status?: number
}
// 流式失败诊断（可能挂在 decision 顶层，也可能挂在失败候选项上）
interface TrailDiag {
  delivered?: boolean
  first_byte_received?: boolean
  elapsed_ms?: number
  upstream_status?: number
}

function decisionObj(cl: CallLogView): Record<string, any> | null {
  const d = cl.Decision
  return d && typeof d === 'object' ? (d as Record<string, any>) : null
}

// 决策轨迹尝试项：无 attempts（拒绝/旧数据）时返回空数组，未知字段不影响渲染。
function callAttempts(cl: CallLogView): TrailAttempt[] {
  const d = decisionObj(cl)
  return d && Array.isArray(d.attempts) ? (d.attempts as TrailAttempt[]) : []
}

// 流式失败诊断：优先取 decision 顶层，缺省回退到失败候选项（仅取已存在字段，未知字段容错）。
function callStreamDiag(cl: CallLogView): TrailDiag {
  const d = decisionObj(cl)
  if (!d) return {}
  const atts = Array.isArray(d.attempts) ? (d.attempts as TrailAttempt[]) : []
  const src: TrailDiag = {}
  for (const row of atts) {
    if (row.delivered != null) src.delivered = row.delivered
    if (row.first_byte_received != null) src.first_byte_received = row.first_byte_received
    if (row.elapsed_ms != null) src.elapsed_ms = row.elapsed_ms
    if (row.upstream_status != null) src.upstream_status = row.upstream_status
  }
  const diag: TrailDiag = {}
  if (d.delivered != null) diag.delivered = !!d.delivered
  else if (src.delivered != null) diag.delivered = src.delivered
  if (d.first_byte_received != null) diag.first_byte_received = !!d.first_byte_received
  else if (src.first_byte_received != null) diag.first_byte_received = src.first_byte_received
  if (d.elapsed_ms != null) diag.elapsed_ms = Number(d.elapsed_ms)
  else if (src.elapsed_ms != null) diag.elapsed_ms = src.elapsed_ms
  if (d.upstream_status != null) diag.upstream_status = Number(d.upstream_status)
  else if (src.upstream_status != null) diag.upstream_status = src.upstream_status
  return diag
}

// 稳定排除/失败原因 → 中文标签；未知原因原样回退（对后端新增枚举容错）。
const REASON_LABELS: Record<string, string> = {
  no_binding: '未绑定渠道',
  tag_no_route: '标签无路由',
  drained: '排空',
  no_candidate: '无候选',
  session_full: '会话已满',
  rate_limited: '限流',
  unsupported_protocol: '协议不支持',
  credential_unavailable: '凭据不可用',
  disabled: '已禁用',
}
function reasonLabel(reason?: string): string {
  if (!reason) return '-'
  return REASON_LABELS[reason] || reason
}

// 排除原因的运维动作提示：兼容后端 hint 及 exclusionHint/exclusion_hint 别名，未知字段容错。
function attemptHint(row: TrailAttempt): string {
  return row.hint || row.exclusionHint || row.exclusion_hint || ''
}

// skipped 行样式：未进入上游即被排除的候选弱化展示。
function attemptRowClass({ row }: { row: TrailAttempt }): string {
  return row.skipped ? 'skipped-row' : ''
}

function msgSide(role?: string) {
  return role === 'user' ? 'right' : 'left'
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
  calls.value.map((c) => ({
    call: c,
    msgs: asMessageList(c.ReqMessages),
    resp: parseRespBody(c.RespBody),
    channel: callChannel(c),
  })),
)
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">会话详情</div>
      <div class="page-header-spacer" />
      <el-button link @click="router.push(isSelf ? '/dev/sessions' : '/admin/sessions')">返回会话列表</el-button>
    </div>

    <div v-loading="loading" class="card">
      <template v-if="meta">
        <el-descriptions :column="2" border class="meta-box">
          <el-descriptions-item label="会话名称" :span="2">{{ meta.SessionName || '未命名' }}</el-descriptions-item>
          <el-descriptions-item label="会话 ID" :span="2">
            <span class="mono sid">{{ meta.SessionID }}</span>
          </el-descriptions-item>
          <el-descriptions-item label="状态">
            <el-tag v-if="meta.Closed" type="info" size="small" effect="light">已关闭</el-tag>
            <el-tag v-else-if="meta.Expired" type="danger" size="small" effect="light">已过期</el-tag>
            <el-tag v-else type="success" size="small" effect="light">进行中</el-tag>
          </el-descriptions-item>
          <el-descriptions-item label="模型">{{ meta.Model || '-' }}</el-descriptions-item>
          <el-descriptions-item label="用户 / 令牌">
            {{ meta.UserNickname || meta.UserName || '-' }}{{ meta.UserNickname && meta.UserName ? ' @' + meta.UserName : '' }} · {{ meta.TokenName || '-' }}
          </el-descriptions-item>
          <el-descriptions-item label="渠道-密钥">{{ meta.ChannelKeyName || '-' }}</el-descriptions-item>
          <el-descriptions-item label="创建时间">{{ fmtDateTime(meta.CreatedAt) }}</el-descriptions-item>
          <el-descriptions-item label="最近活跃">{{ fmtDateTime(meta.LastActive) }}</el-descriptions-item>
          <el-descriptions-item label="过期时间">{{ fmtDateTime(meta.ExpireAt) }}</el-descriptions-item>
        </el-descriptions>
      </template>
      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />

      <div class="calls-title">完整记录（{{ calls.length }} 次调用）</div>
      <el-empty
        v-if="!loading && !errInfo.message && !calls.length"
        description="该会话暂无调用记录"
      />

      <div v-for="item in transcript" :key="item.call.BillingID || item.call.RequestID || item.call.CallTime" class="call-card">
        <div class="call-head">
          <div class="call-meta">
            <span class="t-date mono">{{ fmtDateTime(item.call.CallTime) }}</span>
            <span class="t-clock">
              {{ item.call.Model || '-' }}
              <el-tag v-if="item.call.AutoModelName" size="small" effect="plain" type="success">Auto {{ item.call.AutoModelName }}</el-tag>
            </span>
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

        <div v-if="item.msgs.length || item.call.RespBody || item.call.ErrorMessage" class="msgs">
          <div
            v-for="(m, mi) in item.msgs"
            :key="mi"
            class="msg-row"
            :class="msgSide(m.role) === 'right' ? 'is-user' : 'is-other'"
          >
            <div class="msg-cluster">
              <MessageBubbleGroup
                :role="m.role || 'message'"
                :content="m.content"
                :tone="m.role === 'user' ? 'user' : 'assistant'"
                empty-text="（无文本内容）"
              />
            </div>
          </div>

          <div class="msg-row is-other">
            <div class="msg-cluster">
              <!-- 失败调用：错误原因 + 上游原始响应（若有），两者都不吞 -->
              <template v-if="item.call.RespKind === 'error'">
                <div v-if="item.call.ErrorMessage" class="resp-error">{{ item.call.ErrorMessage }}</div>
                <MessageBubbleGroup
                  v-if="item.call.RespBody"
                  role="错误响应"
                  :content="contentOfBody(item.call.RespBody)"
                />
              </template>
              <MessageBubbleGroup
                v-else-if="item.resp.text || item.resp.reasoning || item.resp.toolCalls.length"
                role="assistant 响应"
                :content="item.resp.text"
                :reasoning="item.resp.reasoning"
                :tool-calls="item.resp.toolCalls"
              />
              <MessageBubbleGroup
                v-else
                role="assistant 响应"
                :content="contentOfBody(item.call.RespBody || '')"
                empty-text="（无内容）"
              />
            </div>
          </div>
        </div>

        <!-- 决策轨迹：含路由层排除项（排空等）与流式失败诊断，未知字段容错 -->
        <div v-if="callAttempts(item.call).length" class="trail">
          <div class="trail-head">
            <span class="trail-title">决策轨迹</span>
            <el-tag
              v-if="callStreamDiag(item.call).delivered != null"
              size="small"
              :type="callStreamDiag(item.call).delivered ? 'danger' : 'info'"
              effect="plain"
            >
              上游{{ callStreamDiag(item.call).delivered ? '已送达' : '未送达' }}
            </el-tag>
            <el-tag v-if="callStreamDiag(item.call).first_byte_received != null" size="small" effect="plain">
              {{ callStreamDiag(item.call).first_byte_received ? '已收首字节' : '未收首字节' }}
            </el-tag>
            <el-tag v-if="callStreamDiag(item.call).elapsed_ms != null" size="small" effect="plain">
              阶段耗时 {{ callStreamDiag(item.call).elapsed_ms }}ms
            </el-tag>
            <el-tag v-if="callStreamDiag(item.call).upstream_status != null" size="small" effect="plain">
              上游状态 {{ callStreamDiag(item.call).upstream_status }}
            </el-tag>
          </div>
          <el-table
            :data="callAttempts(item.call)"
            border
            size="small"
            class="trail-table"
            :row-class-name="attemptRowClass"
          >
            <el-table-column label="顺序" width="64" align="center">
              <template #default="{ row }">#{{ row.order }}</template>
            </el-table-column>
            <el-table-column label="渠道" min-width="140">
              <template #default="{ row }">
                <span :title="row.channel_id">{{ row.channel_name || row.channel_id || '-' }}</span>
              </template>
            </el-table-column>
            <el-table-column label="内部模型" min-width="140">
              <template #default="{ row }"><span class="mono">{{ row.internal_model_id || '-' }}</span></template>
            </el-table-column>
            <el-table-column label="原因" min-width="220">
              <template #default="{ row }">
                <span>{{ reasonLabel(row.reason) }}{{ row.skipped ? '（跳过）' : '' }}</span>
                <!-- 排除原因的运维动作提示（如排空→查渠道/密钥状态或手动探测） -->
                <el-tag v-if="attemptHint(row)" size="small" type="info" effect="plain" class="hint-tag">
                  {{ attemptHint(row) }}
                </el-tag>
              </template>
            </el-table-column>
          </el-table>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.mono {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
}
.meta-box {
  margin-bottom: 16px;
}
.sid {
  word-break: break-all;
}
.calls-title {
  font-weight: 600;
  font-size: 14px;
  margin: 8px 0 12px;
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
.msg-cluster {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 6px;
  max-width: 92%;
  min-width: 0;
}
.msg-row.is-user .msg-cluster {
  align-items: flex-end;
}
.resp-error {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-danger);
  margin-bottom: 6px;
}
.trail {
  margin-top: 10px;
}
.trail-head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 8px;
}
.trail-title {
  font-weight: 600;
  font-size: 13px;
  color: var(--color-text-secondary);
}
.hint-tag {
  margin-left: 6px;
}
/* skipped 行（未进入上游即被排除）：弱化展示，与真实失败候选区分 */
.trail-table :deep(.skipped-row) {
  color: var(--color-text-tertiary);
}
.trail-table :deep(.skipped-row td.el-table__cell) {
  background: var(--color-primary-light-9);
}
</style>
