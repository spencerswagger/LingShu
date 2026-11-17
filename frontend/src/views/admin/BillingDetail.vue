<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { getBilling } from '@/api/admin'
import type { BillingDetail } from '@/api/dev'
import StatusTag from '@/components/StatusTag.vue'
import StatusPill from '@/components/v2/StatusPill.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'
import CallLogPanel from '@/components/CallLogPanel.vue'
import { useIsNarrow } from '@/composables/useIsNarrow'
import { fmtDateTime } from '@/utils/format'

const route = useRoute()
const billingId = String(route.params.id)

// mode='self' 时复用为开发者自助账单详情页（API 走 /dev 前缀，隐藏 admin 字段）
const props = withDefaults(defineProps<{ mode?: 'admin' | 'self' }>(), { mode: 'admin' })
const isSelf = computed(() => props.mode === 'self')
// 窄屏：描述列表回落单列（原来固定双列会把长 ID / 模型名挤成竖排）
const isNarrow = useIsNarrow()

const loading = ref(false)
const detail = ref<BillingDetail | null>(null)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

// 响应类型展示映射（来自调用日志）
const respKindLabels: Record<string, string> = {
  non_stream: '非流式',
  stream: '流式',
  error: '错误',
  none: '无响应',
}
const respKindTypes: Record<string, 'primary' | 'success' | 'warning' | 'info' | 'danger'> = {
  non_stream: 'primary',
  stream: 'success',
  error: 'danger',
  none: 'info',
}
function respKindLabel(kind: string) {
  return respKindLabels[kind] || kind || '-'
}
function respKindType(kind: string) {
  return respKindTypes[kind] || 'info'
}

// 资金轨迹恒等式（Spec P09 验收 1）：预扣 − 退款 + 差额结算 = 实扣积分。
// 等式成立与否都显示结果——不成立时说明这单的钱对不上，比藏起来重要。
const money = computed(() => detail.value?.Hold)
const identity = computed(() => {
  const h = money.value
  if (!h) return null
  const consumed = detail.value?.CreditsConsumed ?? 0
  const refund = h.RefundedAmount ?? 0
  const settled = h.SettledAmount ?? 0
  const lhs = Math.round((h.Amount - refund + settled) * 1e5) / 1e5
  const diff = Math.round((lhs - consumed) * 1e5) / 1e5
  return { lhs, rhs: consumed, diff, ok: Math.abs(diff) <= 1e-4, refund, settled }
})
const holdStatusText: Record<string, string> = {
  held: '预扣中',
  settled: '已结算',
  refunded: '已退预扣',
  expired: '已过期',
}

// 请求序号守卫：同一路由内快速切换（如详情→详情）或离开页面时，旧响应不得覆盖新数据。
const alive = ref(true)
let loadSeq = 0

onMounted(async () => {
  const my = ++loadSeq
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await getBilling(billingId, isSelf.value ? '/dev' : '/admin')
    if (!alive.value || my !== loadSeq) return
    detail.value = res.Data
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

function fmt(n: number | undefined) {
  return n == null ? '-' : Number(n.toFixed(5))
}
</script>

<template>
  <div class="page-container">
    <div v-loading="loading" class="card me-edit">
      <el-empty v-if="!loading && !detail" :description="errInfo.message ? '账单不存在或已被清理' : '没有数据'">
        <el-button @click="$router.push(isSelf ? '/dev/billings' : '/admin/billings')">返回账单列表</el-button>
      </el-empty>
      <template v-else-if="detail">
        <el-descriptions :column="isNarrow ? 1 : 2" border>
          <el-descriptions-item label="时间" :span="isNarrow ? 1 : 2">{{ detail.CallTime }}</el-descriptions-item>
          <el-descriptions-item label="模型">
            {{ detail.Model }}
            <el-tag v-if="detail.AutoModelName" size="small" effect="plain" type="success">Auto {{ detail.AutoModelName }}</el-tag>
          </el-descriptions-item>
          <!-- admin 额外只读字段；self 模式（dev 响应不含该字段）不展示 -->
          <el-descriptions-item v-if="!isSelf" label="渠道">{{ detail.ChannelName || '-' }}</el-descriptions-item>
          <el-descriptions-item label="模式">
            <el-tag :type="detail.PricingMode === 'cost' ? 'warning' : 'primary'" size="small" effect="plain">
              {{ detail.PricingMode === 'cost' ? '按成本' : '按售价' }}
            </el-tag>
          </el-descriptions-item>
          <el-descriptions-item label="状态"><StatusTag :value="detail.Status" /></el-descriptions-item>          <!-- V2 归因（0020 加列）：后端返回了才展示；历史行 upstream_* 为 null → 显示「未归因」，
               不填 0、不推断。渠道族表在 0024 删除，这一块取代老的「渠道」维度。 -->
          <!-- 归因/成本是管理端字段：开发者端即使后端误返也不渲染（Spec P09 负向断言） -->
          <template v-if="!isSelf && (detail.UpstreamProvider !== undefined || detail.CostSource !== undefined)">
            <el-descriptions-item label="上游供应商">{{ detail.UpstreamProvider || '未归因' }}</el-descriptions-item>
            <el-descriptions-item label="上游账号">{{ detail.UpstreamAccount || '未归因' }}</el-descriptions-item>
            <el-descriptions-item label="上游协议">{{ detail.UpstreamProtocol || '—' }}</el-descriptions-item>
            <el-descriptions-item label="成本来源">
              <!-- 不做 `|| 'none'` 兜底：null（历史未知）与 none（明确无成本口径）是两种结论（G2） -->
              <StatusPill domain="costSource" :value="detail.CostSource ?? ''" />
              <span v-if="!detail.CostSource" class="attr-hint">历史行无成本口径记录，不要与新区间同图比较</span>
            </el-descriptions-item>
            <el-descriptions-item label="账本条目">
              <!-- 匹配上 → 可跳账本页按 ledger_id 精确查；没匹配 → 「待结算」，不是 0 也不是「无成本」 -->
              <router-link v-if="detail.LedgerEntryID" :to="`/admin/ledger?ledger_id=${detail.LedgerEntryID}`" class="mono">
                {{ detail.LedgerEntryID }}
              </router-link>
              <span v-else class="attr-hint">待结算（账本条目还没匹配上）</span>
            </el-descriptions-item>
          </template>

          <el-descriptions-item v-if="detail.CallLog" label="响应类型">
            <el-tag :type="respKindType(detail.CallLog.RespKind)" size="small" effect="plain">
              {{ respKindLabel(detail.CallLog.RespKind) }}
            </el-tag>
          </el-descriptions-item>
        </el-descriptions>

        <!-- 资金轨迹（M8）：预扣 → 结算差额 → 退款 → 钱包流水。未就绪时说明缺什么，不显示 0。 -->
        <div v-if="!isSelf" class="trail card">
          <div class="trail-title">资金轨迹</div>
          <template v-if="detail.Hold">
            <div class="trail-grid">
              <span>预扣金额</span><span>{{ fmt(detail.Hold.Amount) }}</span>
              <span>预扣状态</span>
              <span>
                <StatusPill domain="hold" :value="detail.Hold.Status" />
                <span class="trail-sub">{{ holdStatusText[detail.Hold.Status] || detail.Hold.Status }}</span>
              </span>
              <span>预扣到期</span><span>{{ detail.Hold.ExpiresAt ? fmtDateTime(detail.Hold.ExpiresAt) : '—' }}</span>
              <span>退款</span><span>{{ detail.Hold.RefundedAmount == null ? '—（未退）' : fmt(detail.Hold.RefundedAmount) }}</span>
              <span>结算差额</span>
              <span>{{ detail.Hold.SettledAmount == null ? '—（未结算）' : fmt(detail.Hold.SettledAmount) }}</span>
              <span>实扣积分</span><span>{{ fmt(detail.CreditsConsumed) }}</span>
            </div>
            <div v-if="identity" class="trail-eq" :class="identity.ok ? 'is-ok' : 'is-bad'">
              预扣 {{ fmt(detail.Hold.Amount) }} − 退款 {{ fmt(identity.refund) }} + 差额 {{ fmt(identity.settled) }}
              = {{ fmt(identity.lhs) }} / 实扣 {{ fmt(identity.rhs) }}
              <strong>{{ identity.ok ? '等式成立' : `不成立（差 ${identity.diff}）` }}</strong>
            </div>
            <div v-if="detail.Flows && detail.Flows.length" class="trail-flows">
              <el-table :data="detail.Flows" size="small" border>
                <el-table-column prop="Kind" label="类型" width="90" />
                <el-table-column label="金额" width="120" align="right">
                  <template #default="{ row }">{{ fmt(row.Amount) }}</template>
                </el-table-column>
                <el-table-column label="余额后" width="130" align="right">
                  <template #default="{ row }">{{ row.BalanceAfter == null ? '—' : fmt(row.BalanceAfter) }}</template>
                </el-table-column>
                <el-table-column prop="Note" label="备注" min-width="160" show-overflow-tooltip />
                <el-table-column label="时间" width="150">
                  <template #default="{ row }">{{ row.CreatedAt ? fmtDateTime(row.CreatedAt) : '—' }}</template>
                </el-table-column>
              </el-table>
            </div>
            <p v-else class="trail-sub">钱包流水（credit_flows）未返回：本单只看到预扣与结算，看不到余额变动。</p>
          </template>
          <p v-else class="trail-sub">
            接口未返回预扣信息（M8 待上线）。三种情况必须分开读：<b>未预扣</b>（请求在计费前失败）、
            <b>已退款</b>（失败或无 usage，整笔退回，是正常结果）、<b>成本待结算</b>（账本条目还没匹配上，不是 0）。
          </p>
        </div>

        <!-- 计费计算公式 + 请求参数/请求增量/响应/用量/决策轨迹：统一由调用详情面板承载
             （请求 ID 与一键复制在面板顶部，与下方各区块同一组件，避免两处口径漂移） -->
        <CallLogPanel
          :log="detail.CallLog"
          :show-internal="!isSelf"
          :billings-base="isSelf ? '/dev' : '/admin'"
          title="计费明细"
        >
          <template #prepend>
            <el-collapse-item name="calc" title="计费计算公式">
              <div class="calc-steps">
                <div v-for="(step, i) in detail.Steps" :key="i" class="calc-step">
                  <div class="step-header">
                    <span class="step-no">Step {{ i + 1 }}</span>
                    <span class="step-title">{{ step.Title }}</span>
                  </div>
                  <div v-for="(line, j) in step.Lines" :key="j" class="step-line">
                    <span class="line-label">{{ line.Label }}</span>
                    <span class="line-amount">{{ fmt(line.Amount) }}</span>
                  </div>
                  <div v-if="i === 1 && detail.RouteDiff" class="step-line route-diff">
                    <span class="line-label">{{ detail.RouteDiff.Label }}</span>
                    <span class="line-amount">{{ detail.RouteDiff.Value }}</span>
                  </div>
                  <div class="step-line step-subtotal">
                    <span class="line-label">小计</span>
                    <span class="line-amount"><b>{{ fmt(step.Subtotal) }}</b></span>
                  </div>
                </div>
              </div>
              <div class="result-strip">
                <span>最终应扣积分（≒）</span>
                <span class="result-value">{{ fmt(detail.CreditsConsumed) }}</span>
              </div>
              <div v-if="detail.PricingMode === 'cost' && detail.RouteDiff" class="cost-note">
                * 成本模式含路由差异调整，实际扣减与最终积分可能存在差值。
              </div>
            </el-collapse-item>
          </template>
        </CallLogPanel>
      </template>
      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
    </div>
  </div>
</template>

<style scoped>
.trail {
  margin: 10px 0 4px;
}
.trail-title {
  font-size: 13px;
  font-weight: 600;
  margin-bottom: 8px;
}
.trail-grid {
  display: grid;
  grid-template-columns: 90px 1fr;
  gap: 4px 10px;
  font-size: 13px;
}
.trail-grid > span:nth-child(odd) {
  color: var(--color-text-tertiary);
}
.trail-sub {
  margin-left: 6px;
  font-size: 12px;
  color: var(--color-text-tertiary);
}
.trail-eq {
  margin-top: 8px;
  padding: 6px 10px;
  border-radius: var(--radius-s);
  font-size: 13px;
  border: 1px solid var(--color-border);
}
.trail-eq.is-ok {
  background: #f2fcf5;
  border-color: #b7ebc6;
}
.trail-eq.is-bad {
  background: #fef3f2;
  border-color: #fecdca;
  color: #b42318;
}
.trail-flows {
  margin-top: 8px;
}
.attr-hint {
  margin-left: 8px;
  font-size: 12px;
  color: var(--color-text-tertiary);
}
/* 账本条目 ID 用等宽字体：32 位 ID 复制/比对时字符易混 */
.mono {
  font-family: var(--font-mono, ui-monospace, SFMono-Regular, Menlo, Consolas, monospace);
  font-size: 12px;
  color: var(--color-primary);
  word-break: break-all;
}
.calc-steps {
  display: grid;
  gap: 14px;
}
.calc-step {
  border-left: 2px solid var(--el-color-primary-light-7);
  padding: 2px 0 2px 12px;
}
.step-header {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 4px;
}
.step-no {
  color: var(--brand-primary);
  font-weight: 700;
  font-size: 13px;
}
.step-title {
  font-weight: 600;
}
.step-line {
  display: flex;
  justify-content: space-between;
  padding: 3px 0;
  font-size: 13px;
}
.line-label {
  color: var(--color-text-secondary);
}
.line-amount {
  font-variant-numeric: tabular-nums;
}
.route-diff {
  color: var(--color-warning);
}
.step-subtotal {
  margin-top: 6px;
  padding-top: 6px;
  border-top: 1px dashed var(--color-border);
}
.step-subtotal .line-label {
  color: var(--color-text);
}
.result-strip {
  margin-top: 16px;
  background: var(--brand-primary-soft);
  border-radius: 8px;
  padding: 12px 18px;
  display: flex;
  justify-content: space-between;
  align-items: center;
  font-weight: 600;
}
.result-value {
  color: var(--brand-primary);
  font-size: 22px;
  font-weight: 700;
}
.cost-note {
  font-size: 12px;
  color: var(--color-text-secondary);
  margin-top: 8px;
}
/* 窄屏：单列、正文 ≥14px、长 ID/URL 不横向撑破 */
@media (max-width: 768px) {
  .page-container {
    padding: 12px 12px 20px;
  }
  .card.me-edit {
    padding: 14px 12px;
  }
  .step-line {
    font-size: 14px;
    gap: 12px;
    align-items: baseline;
  }
  .line-label {
    min-width: 0;
    word-break: break-word;
    overflow-wrap: anywhere;
  }
  .line-amount {
    flex-shrink: 0;
  }
  .result-strip {
    padding: 10px 12px;
    font-size: 14px;
  }
  .result-value {
    font-size: 20px;
  }
  .cost-note {
    font-size: 13px;
    line-height: 1.5;
  }
}
</style>