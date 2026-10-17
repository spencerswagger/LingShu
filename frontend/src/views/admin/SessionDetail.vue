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
</style>
