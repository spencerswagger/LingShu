<script setup lang="ts">
import { computed } from 'vue'
import type { CallLogView } from '@/api/dev'
import MessageBubbleGroup from '@/components/MessageBubbleGroup.vue'
import { asMessageList, fmtJson, parseRespBody } from '@/utils/messageParts'

// 决策轨迹 JSON 结构（后端 CallLogView.Decision）
interface DecisionAttempt {
  order?: number
  channel_id?: string
  channel_name?: string
  channel_key_id?: string
  channel_key_name?: string
  internal_model_id?: string
  reason?: string
  // 该候选未进入上游即被跳过（禁用/限流/会话满/协议不支持/排空等）
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
  // 成本标量 / 是否有效 / 时段系数（仅排查用，不参与计费）
  cost?: number
  cost_known?: boolean
  route_time_coeff?: number
}
// 流式失败诊断（可能挂在 decision 顶层，也可能挂在失败候选项上）
interface StreamDiag {
  delivered?: boolean
  first_byte_received?: boolean
  elapsed_ms?: number
  upstream_status?: number
}
interface DecisionView extends StreamDiag {
  attempts?: DecisionAttempt[]
  time_coeff?: number
  ctx_coeff?: number
  est_input_tokens?: number
  pre_consumed?: number
  result?: unknown
}
// 上游原始 usage（call_logs.upstream_usage）：{"Protocol":...,"Usage":{...}}
interface UpstreamUsageView {
  Protocol?: string
  Usage?: Record<string, unknown>
}

const props = withDefaults(
  defineProps<{
    log: CallLogView | null | undefined
    // 是否展示渠道/密钥/内部模型等内部标识（dev 端隐藏）
    showInternal?: boolean
    // 面板总标题；置空则不展示标题
    title?: string
  }>(),
  { showInternal: true, title: '请求 / 响应 / 决策轨迹' },
)

// ===== 请求增量：原样展示请求消息（气泡），并保留原始 JSON =====
// 注意：失败请求也必须展示真实请求内容，不能被 ErrorMessage 覆盖（ErrorMessage 属于响应/决策维度）。
const reqMsgs = computed(() => asMessageList(props.log?.ReqMessages))
const reqRaw = computed(() => (props.log?.ReqMessages == null ? '' : fmtJson(props.log.ReqMessages)))

// ===== 响应：消息形状 → 气泡；非消息形状（如上游错误体）→ 原始 JSON =====
const resp = computed(() => parseRespBody(props.log?.RespBody || ''))
const respIsMessage = computed(
  () => !!(resp.value.text || resp.value.reasoning || resp.value.toolCalls.length),
)

// ===== 上游原始 usage（诊断渠道口径：归一化结果见上方计费公式） =====
const upstreamUsage = computed<UpstreamUsageView | null>(() => {
  const v = props.log?.UpstreamUsage
  return v && typeof v === 'object' ? (v as UpstreamUsageView) : null
})
const upstreamUsageJSON = computed(() => fmtJson(upstreamUsage.value?.Usage ?? upstreamUsage.value))

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

// 候选成本标量展示：无效成本（单价缺失/全 0）显式标注「未知」。
function fmtCost(row: DecisionAttempt): string {
  if (!row.cost_known) return '未知'
  const c = row.cost ?? 0
  return Number.isInteger(c) ? c.toLocaleString() : c.toFixed(4)
}

function fmtCoeff(v: number | undefined): string {
  if (v == null) return '-'
  return `×${v}`
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
function attemptHint(row: DecisionAttempt): string {
  return row.hint || row.exclusionHint || row.exclusion_hint || ''
}

// skipped 行样式：未进入上游即被排除的候选弱化展示。
function attemptRowClass({ row }: { row: DecisionAttempt }): string {
  return row.skipped ? 'skipped-row' : ''
}
</script>

<template>
  <div v-if="log || $slots.prepend" class="call-log">
    <div v-if="title" class="log-title">{{ title }}</div>
    <el-collapse>
      <!-- 调用方可在同一折叠组内前置自定义区块（如账单详情页的计费计算公式） -->
      <slot name="prepend" />
      <template v-if="log">
        <el-collapse-item name="req" title="请求增量">
          <template v-if="reqMsgs.length">
            <MessageBubbleGroup
              v-for="(m, i) in reqMsgs"
              :key="i"
              :role="m.role || 'message'"
              :content="m.content"
              :tone="m.role === 'user' ? 'user' : 'assistant'"
              empty-text="（该条消息无文本内容）"
            />
            <el-collapse class="raw-collapse">
              <el-collapse-item name="req-raw" title="原始 JSON（增量消息数组）">
                <pre class="mono log-pre">{{ reqRaw }}</pre>
              </el-collapse-item>
            </el-collapse>
          </template>
          <pre v-else-if="reqRaw" class="mono log-pre">{{ reqRaw }}</pre>
          <div v-else class="log-empty">（未记录 / 继承历史上下文）</div>
        </el-collapse-item>

        <el-collapse-item name="resp" title="响应">
          <!-- 网关侧失败：RespBody 可能为空，错误原因（error_message）独立展示，不依赖响应体 -->
          <div v-if="log.ErrorMessage" class="resp-error">{{ log.ErrorMessage }}</div>
          <template v-if="!log.RespBody">
            <!-- 已有 error_message 时不再重复叙述；仅在既无响应体也无错误原因时提示去看决策轨迹 -->
            <div v-if="!log.ErrorMessage" class="log-empty">
              {{ log.RespKind === 'error' ? '未收到上游响应（原因见「决策轨迹」）' : '—' }}
            </div>
          </template>
          <template v-else-if="respIsMessage">
            <MessageBubbleGroup
              role="assistant"
              :content="resp.text"
              :reasoning="resp.reasoning"
              :tool-calls="resp.toolCalls"
            />
            <el-collapse class="raw-collapse">
              <el-collapse-item name="resp-raw" title="原始响应">
                <pre class="mono log-pre">{{ log.RespBody }}</pre>
              </el-collapse-item>
            </el-collapse>
          </template>
          <!-- 非消息形状（如上游错误体）：原样展示，不吞原文 -->
          <pre v-else class="mono log-pre">{{ log.RespBody }}</pre>
        </el-collapse-item>

        <el-collapse-item v-if="upstreamUsage" name="usage" title="上游原始 usage">
          <div class="usage-head">
            <el-tag v-if="upstreamUsage.Protocol" size="small" effect="plain">
              {{ upstreamUsage.Protocol }}
            </el-tag>
            <span class="usage-hint">上游原始键值（未归一化）；三段归一化结果见上方计费明细</span>
          </div>
          <pre class="mono log-pre">{{ upstreamUsageJSON }}</pre>
        </el-collapse-item>

        <el-collapse-item name="decision" title="决策轨迹">
          <template v-if="decision">
            <div class="decision-tags">
              <el-tag v-if="decision.time_coeff && decision.time_coeff > 0" size="small" effect="plain">
                时段系数 ×{{ decision.time_coeff }}
              </el-tag>
              <el-tag v-if="decision.ctx_coeff && decision.ctx_coeff > 0" size="small" effect="plain">
                上下文分档 ×{{ decision.ctx_coeff }}
              </el-tag>
              <el-tag v-if="decision.pre_consumed != null" size="small" type="warning" effect="plain">
                预扣积分 {{ decision.pre_consumed }}
                <span v-if="decision.est_input_tokens != null"
                  >（预估输入 {{ decision.est_input_tokens.toLocaleString() }} tok）</span
                >
              </el-tag>
              <!-- 流式失败诊断：区分「已送达/未送达」并展示响应阶段线索（仅排查用） -->
              <el-tag
                v-if="decision.delivered != null"
                size="small"
                :type="decision.delivered ? 'danger' : 'info'"
                effect="plain"
              >
                上游{{ decision.delivered ? '已送达' : '未送达' }}
              </el-tag>
              <el-tag v-if="decision.first_byte_received != null" size="small" effect="plain">
                {{ decision.first_byte_received ? '已收首字节' : '未收首字节' }}
              </el-tag>
              <el-tag v-if="decision.elapsed_ms != null" size="small" effect="plain">
                阶段耗时 {{ decision.elapsed_ms }}ms
              </el-tag>
              <el-tag v-if="decision.upstream_status != null" size="small" effect="plain">
                上游状态 {{ decision.upstream_status }}
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
              :row-class-name="attemptRowClass"
            >
              <el-table-column label="顺序" width="64" align="center">
                <template #default="{ row }">#{{ row.order }}</template>
              </el-table-column>
              <el-table-column v-if="showInternal" label="渠道" min-width="140">
                <template #default="{ row }">
                  <span :title="row.channel_id">{{ row.channel_name || row.channel_id || '-' }}</span>
                </template>
              </el-table-column>
              <el-table-column v-if="showInternal" label="渠道密钥" min-width="140">
                <template #default="{ row }">
                  <span :title="row.channel_key_id">{{ row.channel_key_name || row.channel_key_id || '-' }}</span>
                </template>
              </el-table-column>
              <el-table-column v-if="showInternal" label="内部模型" min-width="140">
                <template #default="{ row }"><span class="mono">{{ row.internal_model_id || '-' }}</span></template>
              </el-table-column>
              <el-table-column v-if="showInternal" label="成本" width="120" align="right">
                <template #default="{ row }">
                  <span v-if="row.cost_known" class="mono">{{ fmtCost(row) }}</span>
                  <span v-else class="cost-unknown">未知</span>
                </template>
              </el-table-column>
              <el-table-column v-if="showInternal" label="时段系数" width="100" align="right">
                <template #default="{ row }"><span class="mono">{{ fmtCoeff(row.route_time_coeff) }}</span></template>
              </el-table-column>
              <el-table-column label="原因" min-width="200">
                <template #default="{ row }">
                  <span>{{ reasonLabel(row.reason) }}{{ row.skipped ? '（跳过）' : '' }}</span>
                  <!-- 排除原因的运维动作提示（如排空→查渠道/密钥状态或手动探测） -->
                  <el-tag v-if="attemptHint(row)" size="small" type="info" effect="plain" class="hint-tag">
                    {{ attemptHint(row) }}
                  </el-tag>
                </template>
              </el-table-column>
            </el-table>
          </template>
          <div v-else class="log-empty">（未记录决策轨迹）</div>
        </el-collapse-item>
      </template>
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
.resp-error {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-danger);
  margin-bottom: 6px;
  word-break: break-all;
}
.raw-collapse {
  margin-top: 10px;
}
.usage-head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
  margin-bottom: 8px;
}
.usage-hint {
  font-size: 12px;
  color: var(--color-text-tertiary);
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
.hint-tag {
  margin-left: 6px;
}
/* skipped 行（未进入上游即被排除）：弱化展示，与真实失败候选区分 */
.attempt-table :deep(.skipped-row) {
  color: var(--color-text-tertiary);
}
.attempt-table :deep(.skipped-row td.el-table__cell) {
  background: var(--color-primary-light-9);
}
.cost-unknown {
  color: var(--color-text-tertiary);
}
</style>
