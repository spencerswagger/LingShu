<script setup lang="ts">
import { computed } from 'vue'
import type { CallLogView } from '@/api/dev'

// 决策轨迹 JSON 结构（后端 CallLogView.Decision）
interface DecisionAttempt {
  order?: number
  channel_id?: string
  channel_key_id?: string
  internal_model_id?: string
  reason?: string
}
interface DecisionView {
  attempts?: DecisionAttempt[]
  time_coeff?: number
  ctx_coeff?: number
  pre_consumed?: number
  result?: unknown
}
interface RespPart {
  role?: string
  text: string
}

const props = withDefaults(
  defineProps<{
    log: CallLogView | null | undefined
    // 是否展示渠道/密钥/内部模型等内部标识（dev 端隐藏）
    showInternal?: boolean
  }>(),
  { showInternal: true },
)

function fmtJson(v: unknown) {
  if (v == null) return ''
  try {
    return JSON.stringify(v, null, 2)
  } catch {
    return String(v)
  }
}

// 响应体可能为：非流式消息对象 / 流式 assistant 增量数组 / 纯文本
function parseRespPart(item: unknown): RespPart | null {
  if (typeof item === 'string') return item ? { text: item } : null
  if (item && typeof item === 'object') {
    const o = item as Record<string, any>
    const role = typeof o.role === 'string' ? o.role : undefined
    const c = o.content
    if (typeof c === 'string') return c ? { role, text: c } : role ? { role, text: '' } : null
    if (Array.isArray(c)) {
      const text = c
        .map((x: any) => (typeof x === 'string' ? x : x && typeof x.text === 'string' ? x.text : ''))
        .join('')
      if (text) return { role, text }
      return role ? { role, text: '' } : null
    }
    return { role, text: fmtJson(o) }
  }
  return null
}

const respParts = computed<RespPart[]>(() => {
  const body = props.log?.RespBody
  if (!body) return []
  let parsed: unknown = null
  try {
    parsed = JSON.parse(body)
  } catch {
    return []
  }
  if (Array.isArray(parsed)) {
    return parsed.map(parseRespPart).filter((p): p is RespPart => p !== null)
  }
  const part = parseRespPart(parsed)
  return part ? [part] : []
})

const decision = computed<DecisionView | null>(() => {
  const d = props.log?.Decision as DecisionView | null | undefined
  if (d && typeof d === 'object') return d
  return null
})

function fmtResult(v: unknown): string {
  if (v == null) return '-'
  if (typeof v === 'string' || typeof v === 'number' || typeof v === 'boolean') return String(v)
  return fmtJson(v)
}
</script>

<template>
  <div v-if="log" class="call-log">
    <div class="log-title">请求 / 响应 / 决策轨迹</div>
    <el-collapse>
      <el-collapse-item name="req" title="请求增量">
        <template v-if="log.RespKind === 'error'">
          <div class="err-msg">{{ log.ErrorMessage || '请求失败，无更多信息' }}</div>
        </template>
        <pre v-else-if="log.ReqMessages != null" class="mono log-pre">{{ fmtJson(log.ReqMessages) }}</pre>
        <div v-else class="log-empty">（未记录/继承历史上下文）</div>
      </el-collapse-item>

      <el-collapse-item name="resp" title="响应">
        <template v-if="!log.RespBody">
          <div class="log-empty">—</div>
        </template>
        <template v-else-if="respParts.length">
          <div v-for="(part, k) in respParts" :key="k" class="resp-block">
            <div v-if="part.role !== undefined" class="resp-role">{{ part.role }}</div>
            <pre class="mono log-pre">{{ part.text }}</pre>
          </div>
        </template>
        <pre v-else class="mono log-pre">{{ log.RespBody }}</pre>
      </el-collapse-item>

      <el-collapse-item name="decision" title="决策轨迹">
        <template v-if="decision">
          <div class="decision-tags">
            <el-tag v-if="decision.time_coeff != null" size="small" effect="plain">
              时段系数 ×{{ decision.time_coeff }}
            </el-tag>
            <el-tag v-if="decision.ctx_coeff != null" size="small" effect="plain">
              上下文分档 ×{{ decision.ctx_coeff }}
            </el-tag>
            <el-tag v-if="decision.pre_consumed != null" size="small" type="warning" effect="plain">
              预扣积分 {{ decision.pre_consumed }}
            </el-tag>
          </div>
          <div v-if="decision.result != null" class="decision-result">
            <span class="d-label">最终结果</span>
            <span class="d-value mono">{{ fmtResult(decision.result) }}</span>
          </div>
          <el-table
            v-if="decision.attempts && decision.attempts.length"
            :data="decision.attempts"
            border
            size="small"
            class="attempt-table"
          >
            <el-table-column label="顺序" width="64" align="center">
              <template #default="{ row }">#{{ row.order }}</template>
            </el-table-column>
            <el-table-column v-if="showInternal" label="渠道 ID" min-width="150">
              <template #default="{ row }"><span class="mono">{{ row.channel_id || '-' }}</span></template>
            </el-table-column>
            <el-table-column v-if="showInternal" label="渠道密钥 ID" min-width="150">
              <template #default="{ row }"><span class="mono">{{ row.channel_key_id || '-' }}</span></template>
            </el-table-column>
            <el-table-column v-if="showInternal" label="内部模型 ID" min-width="150">
              <template #default="{ row }"><span class="mono">{{ row.internal_model_id || '-' }}</span></template>
            </el-table-column>
            <el-table-column label="原因" min-width="200">
              <template #default="{ row }">{{ row.reason || '-' }}</template>
            </el-table-column>
          </el-table>
        </template>
        <div v-else class="log-empty">（未记录决策轨迹）</div>
      </el-collapse-item>
    </el-collapse>
  </div>
</template>

<style scoped>
.mono {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
}
.call-log {
  margin-top: 16px;
}
.log-title {
  font-weight: 600;
  margin-bottom: 10px;
}
.log-pre {
  margin: 0;
  max-height: 360px;
  overflow: auto;
  padding: 10px 12px;
  border-radius: 6px;
  background: #101828;
  color: #d0d5dd;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
}
.log-empty {
  font-size: 13px;
  color: var(--color-text-tertiary);
  padding: 8px 4px;
}
.err-msg {
  font-size: 15px;
  font-weight: 600;
  color: var(--color-danger);
  padding: 8px 4px;
}
.resp-block + .resp-block {
  margin-top: 10px;
}
.resp-role {
  font-size: 12px;
  color: var(--color-text-secondary);
  margin-bottom: 4px;
}
.decision-tags {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 10px;
}
.decision-result {
  display: flex;
  align-items: center;
  gap: 12px;
  background: var(--color-primary-light-9);
  border-radius: 8px;
  padding: 10px 14px;
  margin-bottom: 10px;
}
.d-label {
  font-size: 13px;
  color: var(--color-text-secondary);
  flex-shrink: 0;
}
.d-value {
  font-size: 13px;
  color: var(--color-text);
  word-break: break-all;
  white-space: pre-wrap;
}
.attempt-table {
  margin-top: 4px;
}
</style>