<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { listSessions, listSessionCalls, type AdminSession, type CallLogView } from '@/api/admin'
import { fmtDateTime } from '@/utils/format'
import ErrorBubble from '@/components/ErrorBubble.vue'
import StatusTag from '@/components/StatusTag.vue'
import ChatBubble from '@/components/ChatBubble.vue'
import { contentToBubbles, fmtJson, type MsgBubble } from '@/utils/messageParts'

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
interface TranscriptMsg {
  role?: string
  content?: unknown
}
interface RespInfo {
  text: string
  reasoning: string
  toolCalls: unknown[]
}
interface TranscriptItem {
  call: CallLogView
  msgs: TranscriptMsg[]
  resp: RespInfo
  channel: { channel: string; key: string; model: string }
}

function msgList(v: unknown): TranscriptMsg[] {
  if (!Array.isArray(v)) return []
  return v.map((m) => (m && typeof m === 'object' ? (m as TranscriptMsg) : { content: m }))
}
function msgSide(role?: string) {
  return role === 'user' ? 'right' : 'left'
}

// 消息 content → 多气泡（纯文本/多模态数组/XML 标签块）
function bubblesOf(m: TranscriptMsg): MsgBubble[] {
  return contentToBubbles(m.content)
}

// 解析 RespBody（非流式消息对象 / 流式 assistant 增量数组 / 纯文本）：
// 提取 content 文本、reasoning 推理过程与 tool_calls
function respInfo(cl: CallLogView): RespInfo {
  if (!cl.RespBody) return { text: '', reasoning: '', toolCalls: [] }
  const out: RespInfo = { text: '', reasoning: '', toolCalls: [] }
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
      // 推理过程（推理型模型 delta 携带）
      if (typeof o.reasoning_content === 'string' && o.reasoning_content) out.reasoning += o.reasoning_content
      if (Array.isArray(o.reasoning_content)) {
        for (const c of o.reasoning_content) {
          if (typeof c === 'string' && c) out.reasoning += c
          else if (c && typeof c.text === 'string' && c.text) out.reasoning += c.text
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
    return { text: cl.RespBody, reasoning: '', toolCalls: [] }
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

        <div v-if="item.msgs.length || item.call.RespBody" class="msgs">
          <div
            v-for="(m, mi) in item.msgs"
            :key="mi"
            class="msg-row"
            :class="msgSide(m.role) === 'right' ? 'is-user' : 'is-other'"
          >
            <div class="msg-cluster">
              <div class="msg-role">{{ m.role || 'message' }}</div>
              <template v-if="bubblesOf(m).length">
                <div v-for="(b, bi) in bubblesOf(m)" :key="bi" class="msg-bubble">
                  <ChatBubble :bubble="b" />
                </div>
              </template>
              <div v-else class="msg-empty">（无文本内容）</div>
            </div>
          </div>

          <div class="msg-row is-other">
            <div class="msg-cluster">
              <div class="msg-role">{{ item.call.RespKind === 'error' ? '错误' : 'assistant 响应' }}</div>
              <div v-if="item.call.RespKind === 'error' && item.call.ErrorMessage" class="resp-error">
                {{ item.call.ErrorMessage }}
              </div>
              <template v-else-if="item.resp.text || item.resp.reasoning">
                <div v-if="item.resp.text" class="msg-bubble">
                  <ChatBubble :bubble="{ kind: 'text', text: item.resp.text }" />
                </div>
                <div v-if="item.resp.reasoning" class="msg-bubble">
                  <ChatBubble :bubble="{ kind: 'xml', tag: 'reasoning', text: item.resp.reasoning }" />
                </div>
              </template>
              <template v-else>
                <div class="msg-bubble">
                  <ChatBubble :bubble="{ kind: 'json', text: item.call.RespBody || '—' }" />
                </div>
              </template>
              <div v-if="item.resp.toolCalls.length">
                <div v-for="(tc, ti) in item.resp.toolCalls" :key="ti" class="msg-bubble tool-bubble">
                  <ChatBubble :bubble="{ kind: 'tool', text: fmtJson(tc) }" />
                </div>
              </div>
            </div>
          </div>
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
.msg-bubble {
  max-width: 100%;
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
  margin-bottom: 2px;
}
.msg-empty {
  font-size: 12px;
  color: var(--color-text-tertiary);
}
.tool-bubble {
  width: 100%;
}
.resp-error {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-danger);
}
</style>