<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { CallLogView } from '@/api/dev'
import MessageBubbleGroup from '@/components/MessageBubbleGroup.vue'
import CallResponseView from '@/components/CallResponseView.vue'
import ReqParamsView from '@/components/ReqParamsView.vue'
import { asMessageList, fmtJson } from '@/utils/messageParts'
import { hasClipping, toReqParamsView, type ReqParamsView as ReqParamsModel } from '@/utils/reqParams'
import { asUpstreamUsage, fmtSeg, parseUsageSegments, segResolved } from '@/utils/usage'
import { useIsNarrow } from '@/composables/useIsNarrow'

// 调用详情唯一视图：账单详情与管理端/开发者端会话详情共用（不新建页面、不复制模板）。
// variant 只是容器形态差异，内容口径完全一致：
//   - collapse：账单详情页用（折叠面板，含计费公式插槽）；
//   - timeline：会话详情页用（消息先按对话时间线铺开，其余信息折叠在下方）。
// 区块顺序：错误信息 → 请求参数快照 → 请求增量 → 响应/上游返回 → 用量 → 决策轨迹。

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
  // 成本标量 / 是否有效 / 时段系数（仅排查用，不参与计费；开发者端后端已抹掉）
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

const props = withDefaults(
  defineProps<{
    log: CallLogView | null | undefined
    // 是否展示渠道/密钥/内部模型等内部标识（开发者端为 false：后端已脱敏，不去猜字段）
    showInternal?: boolean
    // 面板总标题；置空则不展示标题
    title?: string
    // 容器形态：collapse=折叠面板（账单详情）；timeline=对话时间线（会话详情）
    variant?: 'collapse' | 'timeline'
    // 「在账单中定位」跳转前缀：admin 或 dev
    billingsBase?: string
  }>(),
  {
    showInternal: true,
    title: '调用详情',
    variant: 'collapse',
    billingsBase: '/admin',
  },
)

const router = useRouter()
const isNarrow = useIsNarrow()

// 失败请求默认展开「错误信息」，其它保持折叠（详情页首次进入不至于满屏）。
const actives = ref<string[]>([])
watch(
  () => [props.log?.Status, props.log?.ErrorMessage] as const,
  () => {
    actives.value = props.log?.ErrorMessage ? ['error'] : []
  },
  { immediate: true },
)

// ===== 请求增量：原样展示请求消息（气泡），并保留原始 JSON =====
// 后端已修完整性问题：首轮落全量（含 system 与历史上下文），不再只留最后一条 user。
// 失败请求也必须展示真实请求内容，不能被 ErrorMessage 覆盖（ErrorMessage 属于响应/决策维度）。
const reqMsgs = computed(() => asMessageList(props.log?.ReqMessages))
const reqRaw = computed(() => (props.log?.ReqMessages == null ? '' : fmtJson(props.log.ReqMessages)))

// role 分布：让「system / 历史上下文有没有留下来」一眼可验证。
const roleSummary = computed(() => {
  const counts = new Map<string, number>()
  for (const m of reqMsgs.value) {
    const role = (m.role || 'message').toLowerCase()
    counts.set(role, (counts.get(role) || 0) + 1)
  }
  return [...counts.entries()].map(([role, n]) => `${role} ${n}`).join(' · ')
})

// ===== 请求参数快照（call_logs.req_params） =====
const reqParams = computed<ReqParamsModel | null>(() => toReqParamsView(props.log?.ReqParams))
const paramsClipped = computed(() => hasClipping(reqParams.value))
const paramsTitle = computed(() => {
  const p = reqParams.value
  if (!p) return '请求参数快照（未记录）'
  return `请求参数快照（${p.Params.length} 项参数）`
})

// ===== 用量：上游原始 usage 的四段（缺失=未解析，绝不补 0） =====
const upstreamUsage = computed(() => asUpstreamUsage(props.log?.UpstreamUsage))
const usageSegs = computed(() => parseUsageSegments(props.log?.UpstreamUsage))
const usageRows = computed(() => [
  { Key: 'input', Label: '输入 input', Value: usageSegs.value.Input },
  { Key: 'output', Label: '输出 output', Value: usageSegs.value.Output },
  { Key: 'cache_read', Label: '缓存读 cache_read', Value: usageSegs.value.CacheRead },
  { Key: 'cache_creation', Label: '缓存写 cache_creation', Value: usageSegs.value.CacheCreation },
])
const usageJSON = computed(() => fmtJson(upstreamUsage.value?.Usage ?? null))

// ===== 决策轨迹 =====
const decision = computed<DecisionView | null>(() => {
  const d = props.log?.Decision
  if (d && typeof d === 'object') return d as DecisionView
  return null
})
const attempts = computed<DecisionAttempt[]>(() => {
  const a = decision.value?.attempts
  return Array.isArray(a) ? a : []
})
// 进入过上游的候选（skipped=false）才是「真实尝试」，重试次数以此为准。
const triedCount = computed(() => attempts.value.filter((a) => !a.skipped).length)
const skippedCount = computed(() => attempts.value.length - triedCount.value)
// 重试 = 进入上游的次数 - 1（首次不算重试）
const retryCount = computed(() => Math.max(0, triedCount.value - 1))
const upstreamFailCount = computed(
  () => attempts.value.filter((a) => !a.skipped && (a.upstream_status ?? 0) >= 400).length,
)

// 最终选择：优先 decision.result（后端记录的成功落点），缺省回退到最后一个进入上游的候选。
const finalPick = computed(() => {
  const r = decision.value?.result
  if (r && typeof r === 'object') {
    const o = r as Record<string, unknown>
    const parts = [o.channel_name ?? o.channel_id, o.internal_model_id ?? o.model]
      .filter((x) => x != null && x !== '')
      .map(String)
    if (parts.length) return parts.join(' · ')
  }
  for (let i = attempts.value.length - 1; i >= 0; i--) {
    const a = attempts.value[i]
    if (a.skipped) continue
    const parts = [a.channel_name || a.channel_id, a.internal_model_id].filter(Boolean)
    if (parts.length) return parts.join(' · ')
  }
  return ''
})

const showError = computed(() => !!(props.log?.ErrorMessage || props.log?.Status === 'failed'))
const requestID = computed(() => props.log?.RequestID || '')

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

// ===== 请求 ID 操作：一键复制 / 在账单列表里定位 =====
async function copyRequestID() {
  if (!requestID.value) return
  try {
    await navigator.clipboard.writeText(requestID.value)
    ElMessage.success('请求 ID 已复制')
  } catch {
    ElMessage.warning('复制失败，请手动选择复制')
  }
}
function gotoRequest() {
  if (!requestID.value) return
  router.push({
    path: `${props.billingsBase}/billings`,
    query: { request_id: requestID.value },
  })
}
</script>

<template>
  <div v-if="log || $slots.prepend" class="call-log" :class="`variant-${variant}`">
    <div v-if="title" class="log-title">{{ title }}</div>

    <!-- 请求 ID：定位一次请求的唯一入口，常显 + 可复制 + 可跳账单列表 -->
    <div v-if="requestID" class="log-head">
      <div class="reqid">
        <span class="reqid-label">请求 ID</span>
        <span class="reqid-value mono">{{ requestID }}</span>
        <el-button link size="small" @click="copyRequestID">复制</el-button>
        <el-button link size="small" @click="gotoRequest">在账单中定位</el-button>
      </div>
      <div v-if="log?.Status === 'failed'" class="reqid-badge">
        <el-tag type="danger" size="small" effect="light">本次请求失败</el-tag>
      </div>
    </div>

    <!-- 会话详情形态：消息先按对话时间线铺开（用户右 / 助手左），其余信息折叠在下方 -->
    <div v-if="variant === 'timeline' && log" class="timeline">
      <div
        v-for="(m, i) in reqMsgs"
        :key="i"
        class="msg-row"
        :class="m.role === 'user' ? 'is-user' : 'is-other'"
      >
        <div class="msg-cluster">
          <MessageBubbleGroup
            :role="m.role || 'message'"
            :content="m.content"
            :tone="m.role === 'user' ? 'user' : 'assistant'"
            empty-text="（该条消息无文本内容）"
          />
        </div>
      </div>
      <div v-if="log.RespBody || log.ErrorMessage || (log.RespKind && log.RespKind !== 'none')" class="msg-row is-other">
        <div class="msg-cluster">
          <CallResponseView
            :body="log.RespBody"
            :resp-kind="log.RespKind"
            :error-message="log.ErrorMessage"
          />
        </div>
      </div>
      <div v-if="!reqMsgs.length && !log.RespBody && !log.ErrorMessage" class="log-empty">
        （本次调用未记录请求/响应正文）
      </div>
    </div>

    <el-collapse v-model="actives">
      <!-- 调用方可在同一折叠组内前置自定义区块（如账单详情页的计费计算公式） -->
      <slot name="prepend" />

      <el-collapse-item v-if="showError" name="error">
        <template #title>
          <span class="t-error">错误信息</span>
        </template>
        <div class="err-body">
          <div class="err-msg">{{ log?.ErrorMessage || '该请求失败，但未记录错误原因' }}</div>
          <div v-if="requestID" class="err-id">
            <span class="err-id-label">请求 ID</span>
            <span class="mono err-id-value">{{ requestID }}</span>
            <el-button link size="small" @click="copyRequestID">复制</el-button>
            <span class="err-id-hint">（把它交给平台即可定位这一次请求）</span>
          </div>
        </div>
      </el-collapse-item>

      <el-collapse-item name="params">
        <template #title>
          <span>{{ paramsTitle }}</span>
          <el-tag v-if="paramsClipped" size="small" type="warning" effect="plain" class="title-tag">
            有裁剪
          </el-tag>
        </template>
        <ReqParamsView :params="reqParams" :show-internal="showInternal" />
      </el-collapse-item>

      <el-collapse-item v-if="variant !== 'timeline'" name="req">
        <template #title>
          <span>请求增量（{{ reqMsgs.length }} 条）</span>
          <span v-if="roleSummary" class="title-sub">{{ roleSummary }}</span>
        </template>
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
            <el-collapse-item name="req-raw" title="原始 JSON（增量消息数组，完整不截断）">
              <pre class="mono log-pre">{{ reqRaw }}</pre>
            </el-collapse-item>
          </el-collapse>
        </template>
        <pre v-else-if="reqRaw" class="mono log-pre">{{ reqRaw }}</pre>
        <div v-else class="log-empty">（未记录 / 继承历史上下文）</div>
      </el-collapse-item>

      <el-collapse-item v-if="variant !== 'timeline'" name="resp" title="响应 / 上游返回">
        <CallResponseView :body="log?.RespBody" :resp-kind="log?.RespKind" :show-error="false" />
      </el-collapse-item>

      <el-collapse-item name="usage">
        <template #title>
          <span>用量</span>
          <el-tag v-if="upstreamUsage?.Protocol" size="small" effect="plain" class="title-tag">
            {{ upstreamUsage.Protocol }}
          </el-tag>
        </template>
        <template v-if="upstreamUsage">
          <div class="usage-grid">
            <div v-for="row in usageRows" :key="row.Key" class="usage-item">
              <span class="u-label">{{ row.Label }}</span>
              <span class="u-val" :class="{ unresolved: !segResolved(row.Value) }">
                {{ fmtSeg(row.Value) }}
              </span>
            </div>
          </div>
          <div class="usage-hint">
            上游原始 usage（未归一化）。缺失的段显示「未解析」而不是 0 —— 上游没报 ≠ 用量为 0；
            计费口径的归一化三段见「计费计算公式」。
          </div>
          <el-collapse class="raw-collapse">
            <el-collapse-item name="usage-raw" title="上游原始 usage（原样 JSON）">
              <pre class="mono log-pre">{{ usageJSON }}</pre>
            </el-collapse-item>
          </el-collapse>
        </template>
        <div v-else class="log-empty">未记录上游 usage（该请求可能未收到上游响应，或渠道未回传 usage）</div>
      </el-collapse-item>

      <el-collapse-item name="decision" title="决策轨迹">
        <template v-if="decision">
          <!-- 路由 / 候选 / 重试 / 最终选择：先给结论，再看候选明细 -->
          <div v-if="attempts.length || finalPick" class="trail-summary">
            <span class="ts-item">候选 <b>{{ attempts.length }}</b> 个</span>
            <span v-if="skippedCount" class="ts-item">路由阶段排除 <b>{{ skippedCount }}</b> 个</span>
            <span class="ts-item">进入上游 <b>{{ triedCount }}</b> 次</span>
            <span v-if="retryCount" class="ts-item is-warn">重试 <b>{{ retryCount }}</b> 次</span>
            <span v-if="upstreamFailCount" class="ts-item is-danger">上游报错 <b>{{ upstreamFailCount }}</b> 次</span>
            <span v-if="finalPick" class="ts-item is-final">最终选择 <b>{{ finalPick }}</b></span>
          </div>
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

          <!-- 桌面：候选表格 -->
          <el-table
            v-if="attempts.length && !isNarrow"
            :data="attempts"
            border
            size="small"
            class="attempt-table"
            :row-class-name="attemptRowClass"
          >
            <el-table-column label="顺序" width="64" align="center">
              <template #default="{ row }">#{{ row.order }}</template>
            </el-table-column>
            <el-table-column label="渠道" min-width="140">
              <template #default="{ row }">
                <!-- 渠道显示名属开发者端白名单（保留），密钥/内部模型/成本才是脱敏项 -->
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

          <!-- 窄屏：候选卡片（避免表格横向撑破） -->
          <div v-else-if="attempts.length" class="attempt-cards">
            <div v-for="(row, i) in attempts" :key="i" class="attempt-card" :class="{ skipped: row.skipped }">
              <div class="ac-head">
                <span class="ac-no">#{{ row.order }}</span>
                <el-tag v-if="row.skipped" size="small" type="info" effect="plain">路由阶段跳过</el-tag>
                <el-tag v-else-if="(row.upstream_status ?? 0) >= 400" size="small" type="danger" effect="plain">
                  上游 {{ row.upstream_status }}
                </el-tag>
                <el-tag v-else size="small" type="success" effect="plain">已进入上游</el-tag>
              </div>
              <div v-if="row.channel_name || row.channel_id" class="ac-line">
                渠道 {{ row.channel_name || row.channel_id }}
                <template v-if="showInternal && (row.channel_key_name || row.channel_key_id)">
                  · 密钥 {{ row.channel_key_name || row.channel_key_id }}
                </template>
              </div>
              <div v-if="showInternal && row.internal_model_id" class="ac-line mono">
                内部模型 {{ row.internal_model_id }}
              </div>
              <div class="ac-line">原因 {{ reasonLabel(row.reason) }}</div>
              <div v-if="attemptHint(row)" class="ac-line ac-hint">{{ attemptHint(row) }}</div>
            </div>
          </div>
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
.log-head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 10px;
}
.reqid {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  min-width: 0;
  background: var(--color-primary-light-9, #eef0fe);
  border-radius: 8px;
  padding: 5px 10px;
}
.reqid-label {
  font-size: 12px;
  color: var(--color-text-secondary);
}
.reqid-value {
  font-size: 12px;
  color: var(--color-text);
  word-break: break-all;
  overflow-wrap: anywhere;
  min-width: 0;
}
.reqid-badge {
  flex-shrink: 0;
}
.t-error {
  color: var(--color-danger);
  font-weight: 600;
}
.err-body {
  display: grid;
  gap: 8px;
}
.err-msg {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-danger);
  word-break: break-word;
  overflow-wrap: anywhere;
  white-space: pre-wrap;
}
.err-id {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  font-size: 12px;
}
.err-id-label {
  color: var(--color-text-tertiary);
}
.err-id-value {
  word-break: break-all;
  overflow-wrap: anywhere;
}
.err-id-hint {
  color: var(--color-text-tertiary);
}
.title-tag {
  margin-left: 8px;
}
.title-sub {
  margin-left: 8px;
  font-size: 12px;
  color: var(--color-text-tertiary);
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
.raw-collapse {
  margin-top: 10px;
}
/* ===== 用量 ===== */
.usage-grid {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 8px;
  margin-bottom: 8px;
}
.usage-item {
  border: 1px solid var(--color-border);
  border-radius: 8px;
  padding: 8px 10px;
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.u-label {
  font-size: 11px;
  color: var(--color-text-tertiary);
}
.u-val {
  font-size: 15px;
  font-weight: 700;
  font-variant-numeric: tabular-nums;
  color: var(--color-text);
  word-break: break-all;
}
.u-val.unresolved {
  font-size: 13px;
  font-weight: 500;
  color: var(--color-text-tertiary);
}
.usage-hint {
  font-size: 12px;
  color: var(--color-text-tertiary);
  line-height: 1.5;
  margin-bottom: 4px;
}
/* ===== 决策轨迹 ===== */
.trail-summary {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 12px;
  margin-bottom: 10px;
  font-size: 12px;
  color: var(--color-text-secondary);
}
.ts-item b {
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
}
.ts-item.is-warn b {
  color: var(--color-warning);
}
.ts-item.is-danger b {
  color: var(--color-danger);
}
.ts-item.is-final b {
  color: var(--brand-primary);
  word-break: break-all;
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
/* 窄屏候选卡片 */
.attempt-cards {
  display: grid;
  gap: 8px;
}
.attempt-card {
  border: 1px solid var(--color-border);
  border-radius: 8px;
  padding: 8px 10px;
  display: grid;
  gap: 3px;
  min-width: 0;
}
.attempt-card.skipped {
  background: var(--color-primary-light-9);
  color: var(--color-text-tertiary);
}
.ac-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 2px;
}
.ac-no {
  font-weight: 700;
  font-size: 13px;
  color: var(--color-text-secondary);
}
.ac-line {
  font-size: 13px;
  word-break: break-word;
  overflow-wrap: anywhere;
}
.ac-hint {
  color: var(--color-text-tertiary);
  font-size: 12px;
}
/* ===== 会话详情：对话时间线 ===== */
.timeline {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-bottom: 10px;
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
  gap: 6px;
  max-width: 92%;
  min-width: 0;
}
.msg-row.is-user .msg-cluster {
  align-items: flex-end;
}
/* ===== 窄屏 ===== */
@media (max-width: 768px) {
  .usage-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .usage-hint,
  .log-empty,
  .err-msg {
    font-size: 14px;
  }
  .u-val {
    font-size: 16px;
  }
  .msg-cluster {
    max-width: 100%;
  }
}
</style>
