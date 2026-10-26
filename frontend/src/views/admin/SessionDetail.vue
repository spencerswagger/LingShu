<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { listSessions, listSessionCalls, type AdminSession, type CallLogView } from '@/api/admin'
import { fmtDateTime } from '@/utils/format'
import ErrorBubble from '@/components/ErrorBubble.vue'
import StatusTag from '@/components/StatusTag.vue'
import CallLogPanel from '@/components/CallLogPanel.vue'
import { useIsNarrow } from '@/composables/useIsNarrow'

// 会话详情：元信息 + 逐次调用的完整详情。
// 调用正文（请求参数快照 / 请求增量 / 响应 / 用量 / 决策轨迹 / 错误信息）不再在这里另写一套，
// 而是复用账单详情同一个 CallLogPanel（variant='timeline'）——同一次调用在两个入口看到的口径必须一致。
const props = withDefaults(defineProps<{ mode?: 'admin' | 'self' }>(), { mode: 'admin' })
const isSelf = computed(() => props.mode === 'self')
const isNarrow = useIsNarrow()

const route = useRoute()
const router = useRouter()
const sessionId = String(route.params.id)

const loading = ref(false)
const meta = ref<AdminSession | null>(null)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const calls = ref<CallLogView[]>([])

// 请求序号守卫：离开路由或重复进入时，旧响应不得写入（会话详情一次并发两个请求）
const alive = ref(true)
let loadSeq = 0

onMounted(async () => {
  const my = ++loadSeq
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const [metaRes, callsRes] = await Promise.all([
      listSessions({ Q: sessionId, Size: 1 }, isSelf.value ? '/dev' : '/admin'),
      listSessionCalls(sessionId, isSelf.value ? '/dev' : '/admin'),
    ])
    if (!alive.value || my !== loadSeq) return
    meta.value = metaRes.Data.List?.[0] || null
    calls.value = callsRes.Data.List || []
    // 会话元信息缺失（已被清理等）：不阻塞调用记录展示
    if (!meta.value) errInfo.value = { message: '会话信息不可用（记录可能已被清理）', requestId: '' }
  } catch (e: any) {
    if (!alive.value || my !== loadSeq) return
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    if (alive.value && my === loadSeq) loading.value = false
  }
})

onBeforeUnmount(() => {
  alive.value = false
  loadSeq++
})

// 渠道/密钥/内部模型：优先 Decision.result，缺省回退 attempts 末条。
// 仅管理端展示（开发者端后端已脱敏，这些字段本就拿不到，不去猜）。
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

function callKey(cl: CallLogView, index: number): string {
  return cl.BillingID || cl.RequestID || `${index}`
}

// 会话内既有成功也有失败时，顶部给一条汇总，避免逐卡翻找。
const callStats = computed(() => {
  const failed = calls.value.filter((c) => c.Status !== 'completed').length
  return { total: calls.value.length, failed }
})
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
        <el-descriptions :column="isNarrow ? 1 : 2" border class="meta-box">
          <el-descriptions-item label="会话名称" :span="isNarrow ? 1 : 2">{{ meta.SessionName || '未命名' }}</el-descriptions-item>
          <el-descriptions-item label="会话 ID" :span="isNarrow ? 1 : 2">
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
          <!-- 渠道-密钥属内部维度：开发者端不展示（后端 DTO 已抹掉） -->
          <el-descriptions-item v-if="!isSelf" label="渠道-密钥">{{ meta.ChannelKeyName || '-' }}</el-descriptions-item>
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

      <div class="calls-title">
        完整记录（{{ callStats.total }} 次调用<template v-if="callStats.failed">，其中失败 {{ callStats.failed }} 次</template>）
      </div>
      <el-empty
        v-if="!loading && !errInfo.message && !calls.length"
        description="该会话暂无调用记录"
      />

      <div v-for="(call, ci) in calls" :key="callKey(call, ci)" class="call-card">
        <div class="call-head">
          <div class="call-meta">
            <span class="t-date mono">{{ fmtDateTime(call.CallTime) }}</span>
            <span class="t-clock">
              {{ call.Model || '-' }}
              <el-tag v-if="call.AutoModelName" size="small" effect="plain" type="success">Auto {{ call.AutoModelName }}</el-tag>
            </span>
          </div>
          <!-- 渠道显示名开发者端可见（后端白名单保留）；密钥/内部模型是管理端维度 -->
          <div class="call-meta call-channel" title="渠道信息">
            <span class="t-clock">
              <template v-if="isSelf">渠道 {{ callChannel(call).channel }}</template>
              <template v-else>
                渠道 {{ callChannel(call).channel }} · 密钥 {{ callChannel(call).key }} · 内部模型
                {{ callChannel(call).model }}
              </template>
            </span>
          </div>
          <div class="call-state">
            <StatusTag :value="call.Status" />
            <el-tag :type="call.PricingMode === 'cost' ? 'warning' : 'primary'" size="small" effect="plain">
              {{ call.PricingMode === 'cost' ? '按成本' : '按售价' }}
            </el-tag>
          </div>
        </div>

        <!-- 与账单详情同一个详情视图：请求参数快照 / 请求增量 / 响应 / 用量 / 决策轨迹 / 错误信息 -->
        <CallLogPanel
          :log="call"
          :show-internal="!isSelf"
          :billings-base="isSelf ? '/dev' : '/admin'"
          variant="timeline"
          title=""
        />
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
  overflow-wrap: anywhere;
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
  flex-wrap: wrap;
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
  overflow-wrap: anywhere;
}
.call-state {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-shrink: 0;
}
/* 窄屏：单列、正文 ≥14px、长串折行 */
@media (max-width: 768px) {
  .page-container {
    padding: 12px 12px 20px;
  }
  .card {
    padding: 14px 12px;
  }
  .call-card {
    padding: 10px;
  }
  .call-head {
    align-items: flex-start;
  }
  .t-date,
  .t-clock {
    font-size: 14px;
  }
  .calls-title {
    font-size: 15px;
  }
}
</style>
