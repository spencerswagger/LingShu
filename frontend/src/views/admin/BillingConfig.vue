<script setup lang="ts">
// 系统配置页（管理端）：按「计费 / 路由 / 网关」三个 tab 组织，**每个 tab 独立保存**——
// 保存时只提交本 tab 的字段，后端对没传的键保持现值（「不传 = 不改」）。
// 未保存修改必须拦得住三条离开路径：切 tab、跳路由、刷新/关页面（spencer 明确要求「不许静默丢弃」）。
// 成本页的多渠道对比 / 渠道轨迹开关已下沉为**本机 localStorage 偏好**，本页不再有该项。
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { onBeforeRouteLeave } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { QuestionFilled } from '@element-plus/icons-vue'
import {
  getSystemConfig,
  putSystemConfig,
  exportChannelProtocols,
  importChannelProtocols,
  type BillingConfigData,
  type CostSample,
  type SessionCostConfig,
  type SessionExpirationData,
  type SystemConfigPayload,
  type ChannelProtocolPack,
} from '@/api/admin'
import ModelPricing from '@/components/ModelPricing.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

// ===== tab =====
type TabKey = 'billing' | 'routing' | 'gateway' | 'ops'
const TABS: { key: TabKey; label: string }[] = [
  { key: 'billing', label: '计费' },
  { key: 'routing', label: '路由' },
  { key: 'gateway', label: '网关' },
  // 「运维」没有 sys_configs 字段，放的是非配置类能力（渠道协议备份/恢复），因此没有保存按钮
  { key: 'ops', label: '运维' },
]
const activeTab = ref<TabKey>('billing')
const tabLabel = (k: TabKey) => TABS.find((t) => t.key === k)?.label || k

const loading = ref(false)
const saving = ref(false)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

// ===== 计费 tab =====
const r = ref<number>(1000)
const cnyRate = ref<number>(6.8) // USD/CNY 汇率（仅用于 models.dev 美元参考价折算）
const creditValue = ref<string>('') // 积分价值 V=R/1e6，只读
const billingBase = ref<BillingConfigData | null>(null)
// 路由成本比较样值（routing.cost_sample）：决定新会话落点的可比用量，不参与实际计费。
const costSample = ref<CostSample>({ input: 10000, cache_read: 9000, output: 1000 })
const DEFAULT_TIME_CONFIG = { Timezone: 'Asia/Shanghai', Default: 1, Periodic: [], Overrides: [] }

// 全局时段/分档默认值抽屉
const fallbackOpen = ref(false)
const fallbackSaving = ref(false)
const fallbackErr = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const fallbackModel = ref<Record<string, any>>({ TimeConfig: null, ContextTiers: null })

// ===== 路由 tab（会话成本路由）=====
// 后端默认：enabled=true / mid_switch=true / shadow_enabled=false（种子与产品默认同源）
const sessionCost = ref<SessionCostConfig>({
  enabled: true,
  mid_switch: true,
  shadow_enabled: false,
  remaining_a: 0.7,
  remaining_b: 0.3,
  remaining_min: 0,
  remaining_max: 100,
  min_remaining: 0,
  cooldown_sec: 0,
  min_improve: 0,
  fallback_note: '',
})
const advancedOpen = ref<string[]>([]) // 「高级参数」折叠区，默认收起

// ===== 网关 tab（会话过期 + 上游响应头白名单）=====
const sessionExpiration = ref<SessionExpirationData>({ default_ttl_minutes: 5, context_tiers: [], turn_tiers: [] })
const whitelist = ref<string[]>([])
const headerSuggestions = [
  'anthropic-version',
  'anthropic-beta',
  'OpenAI-Organization',
  'OpenAI-Project',
  'OpenAI-Beta',
  'traceparent',
  'tracestate',
  'x-request-id',
  'x-session-id',
  'x-client-request-id',
  'x-opencode-session',
]

// ===== 每个 tab 的提交载荷 + 已保存快照（脏判定用）=====
const billingPayload = computed(() => ({
  R: r.value,
  CnyRate: cnyRate.value,
  ContextTiers: billingBase.value?.ContextTiers ?? [],
  TimeConfig: billingBase.value?.TimeConfig ?? DEFAULT_TIME_CONFIG,
  CostSample: costSample.value,
}))
const routingPayload = computed(() => {
  const c = sessionCost.value
  return {
    enabled: c.enabled,
    mid_switch: c.mid_switch,
    shadow_enabled: c.shadow_enabled,
    remaining_a: c.remaining_a,
    remaining_b: c.remaining_b,
    remaining_min: c.remaining_min,
    remaining_max: c.remaining_max,
    min_remaining: c.min_remaining,
    cooldown_sec: c.cooldown_sec,
    min_improve: c.min_improve,
  }
})
const gatewayPayload = computed(() => ({
  SessionExpiration: sessionExpiration.value,
  UpstreamResponseHeaderWhitelist: whitelist.value,
}))

// 有配置字段的三个 tab（运维 tab 不写 sys_configs，不参与脏判定/保存）
type ConfigTab = Exclude<TabKey, 'ops'>
const isConfigTab = (t: TabKey): t is ConfigTab => t !== 'ops'

const snap = ref<Record<ConfigTab, string>>({ billing: '', routing: '', gateway: '' })
const payloadOf = (tab: ConfigTab) =>
  tab === 'billing' ? billingPayload.value : tab === 'routing' ? routingPayload.value : gatewayPayload.value
const isDirty = (tab: TabKey) => (isConfigTab(tab) ? JSON.stringify(payloadOf(tab)) !== snap.value[tab] : false)
const dirtyTabs = computed(() => TABS.filter((t) => isDirty(t.key)).map((t) => t.key))
const anyDirty = computed(() => dirtyTabs.value.length > 0)

// ===== 读取 =====
async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await getSystemConfig()
    const b = res.Data.Billing
    billingBase.value = b
    r.value = Number(b.R)
    const c = Number(b.CNYRate)
    if (c > 0) cnyRate.value = c
    const cv = Number(b.CreditValue)
    if (!isNaN(cv)) creditValue.value = `1 积分 = ¥${cv}（R ÷ 1,000,000）`
    const cs = b.CostSample
    if (cs) {
      costSample.value = {
        input: Number(cs.input) || 0,
        cache_read: Number(cs.cache_read) || 0,
        output: Number(cs.output) || 0,
      }
    }

    const sc = res.Data.Routing?.SessionCost
    if (sc) sessionCost.value = { ...sessionCost.value, ...sc }

    const se = res.Data.SessionExpiration
    sessionExpiration.value = {
      default_ttl_minutes: Number(se?.default_ttl_minutes) || 5,
      context_tiers: se?.context_tiers || [],
      turn_tiers: se?.turn_tiers || [],
    }
    whitelist.value = res.Data.UpstreamResponseHeaderWhitelist || []

    // 快照必须在状态写完后再取，否则首屏就被判成「有改动」
    snap.value = {
      billing: JSON.stringify(billingPayload.value),
      routing: JSON.stringify(routingPayload.value),
      gateway: JSON.stringify(gatewayPayload.value),
    }
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)

// ===== 每个 tab 独立保存（只提交本 tab 的字段）=====
function validateTab(tab: ConfigTab): string | null {
  if (tab === 'billing') {
    if (!r.value || r.value <= 0) return 'R 值必须为正数'
    if (!cnyRate.value || cnyRate.value <= 0) return '汇率必须为正数'
  }
  if (tab === 'routing') {
    const c = sessionCost.value
    if (c.min_improve < 0 || c.min_improve >= 1) return '最小改善阈值需在 [0, 1) 之间'
    if (c.remaining_min > c.remaining_max) return '剩余量归一化下界不能大于上界'
    if (!Number.isInteger(c.min_remaining) || c.min_remaining < 0) return '最小剩余量需为非负整数'
    if (!Number.isInteger(c.cooldown_sec) || c.cooldown_sec < 0) return '冷却秒数需为非负整数'
  }
  if (tab === 'gateway') {
    if (!Number.isInteger(sessionExpiration.value.default_ttl_minutes) || sessionExpiration.value.default_ttl_minutes <= 0) {
      return '兜底 TTL 必须为正整数'
    }
  }
  return null
}

async function saveTab(tab: TabKey = activeTab.value) {
  if (!isConfigTab(tab)) return // 运维 tab 没有可保存的配置字段
  const bad = validateTab(tab)
  if (bad) {
    ElMessage.warning(bad)
    return
  }
  if (!isDirty(tab)) {
    ElMessage.info('本页签没有需要保存的修改')
    return
  }
  const payload: SystemConfigPayload =
    tab === 'billing'
      ? { Billing: billingPayload.value }
      : tab === 'routing'
        ? { Routing: { SessionCost: routingPayload.value } }
        : { SessionExpiration: sessionExpiration.value, UpstreamResponseHeaderWhitelist: whitelist.value }

  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await putSystemConfig(payload)
    // Affected = 实际写入的键数；不按 1 判断成功（不同 tab 写入键数不同）
    ElMessage.success(`已保存（本次写入 ${res.Data?.Affected ?? 0} 个键）`)
    await load()
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    saving.value = false
  }
}

// ===== 离开守卫：切 tab =====
// el-tabs 的 before-leave 支持返回 Promise<boolean>，弹确认时先卡住切换。
async function beforeLeaveTab(_next: TabKey | number, prev: TabKey | number): Promise<boolean> {
  const from = prev as TabKey
  if (!isDirty(from)) return true
  try {
    await ElMessageBox.confirm(
      `「${tabLabel(from)}」还有未保存的修改，切换后将丢弃。`,
      '未保存的修改',
      { confirmButtonText: '丢弃并切换', cancelButtonText: '留在本页', type: 'warning' },
    )
    // 「丢弃」必须真的回滚：重新拉一次配置把状态和快照对齐，
    // 否则脏标记会一直挂着、用户也可能在别处误把这批未确认的改动保存出去。
    await load()
    return true
  } catch {
    return false
  }
}

// ===== 离开守卫：跳路由 =====
onBeforeRouteLeave(async () => {
  if (!anyDirty.value) return true
  const names = dirtyTabs.value.map(tabLabel).join('、')
  try {
    await ElMessageBox.confirm(`「${names}」还有未保存的修改，离开后将丢弃。`, '未保存的修改', {
      confirmButtonText: '离开并丢弃',
      cancelButtonText: '留在本页',
      type: 'warning',
    })
    return true
  } catch {
    return false
  }
})

// ===== 离开守卫：刷新 / 关闭页面 =====
function onBeforeUnload(e: BeforeUnloadEvent) {
  if (!anyDirty.value) return
  e.preventDefault()
  e.returnValue = '' // Chrome 需要设置该字段才会弹原生确认
}
onMounted(() => window.addEventListener('beforeunload', onBeforeUnload))
onUnmounted(() => window.removeEventListener('beforeunload', onBeforeUnload))

// ===== 分档增删 =====
function addContextTier() {
  sessionExpiration.value.context_tiers.push({ max_tokens: 2000, ttl_minutes: 5 })
}
function removeContextTier(index: number) {
  sessionExpiration.value.context_tiers.splice(index, 1)
}
function addTurnTier() {
  sessionExpiration.value.turn_tiers.push({ max_turns: 2, ttl_minutes: 5 })
}
function removeTurnTier(index: number) {
  sessionExpiration.value.turn_tiers.splice(index, 1)
}

// ===== 全局时段/分档默认值抽屉（属于计费 tab 的字段）=====
async function openFallback() {
  fallbackOpen.value = true
  fallbackErr.value = { message: '', requestId: '' }
  try {
    const res = await getSystemConfig()
    const tc = res.Data.Billing.TimeConfig as any
    const tiers = res.Data.Billing.ContextTiers || []
    fallbackModel.value = {
      TimeConfig: tc && tc.Periodic ? tc : null,
      ContextTiers: tiers.length ? tiers : null,
    }
  } catch (e: any) {
    fallbackErr.value = { message: e?.message, requestId: e?.requestId }
  }
}

async function saveFallback() {
  fallbackSaving.value = true
  fallbackErr.value = { message: '', requestId: '' }
  try {
    // 抽屉只改计费块里的时段/分档：仍然只提交 Billing 一块，不连带路由/网关
    await putSystemConfig({
      Billing: {
        ContextTiers: fallbackModel.value.ContextTiers || [],
        TimeConfig: fallbackModel.value.TimeConfig || DEFAULT_TIME_CONFIG,
      },
    })
    ElMessage.success('全局时段/分档默认值已保存')
    fallbackOpen.value = false
    await load()
  } catch (e: any) {
    fallbackErr.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    fallbackSaving.value = false
  }
}

// ===== 渠道协议备份与恢复（与配置无关，常驻在 tab 下方）=====
const fileInput = ref<HTMLInputElement | null>(null)
const exporting = ref(false)
const importing = ref(false)

function formatTs(d = new Date()) {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`
}

async function onExport() {
  exporting.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await exportChannelProtocols()
    const blob = new Blob([JSON.stringify(res.Data, null, 2)], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `channel-protocols-${formatTs()}.json`
    a.click()
    URL.revokeObjectURL(url)
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    exporting.value = false
  }
}

function triggerImport() {
  fileInput.value?.click()
}

async function onFileChange(ev: Event) {
  const input = ev.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return
  try {
    const raw = await file.text()
    if (!raw) return
    let parsed: ChannelProtocolPack
    try {
      parsed = JSON.parse(raw)
    } catch {
      ElMessage.error('文件不是合法的 JSON')
      return
    }
    importing.value = true
    await importChannelProtocols(parsed)
    ElMessage.success('渠道协议已导入')
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    importing.value = false
    input.value = '' // 允许再次选同一个文件
  }
}
</script>

<template>
  <div class="page-container system-page">
    <div class="system-body">
      <div v-loading="loading" class="card config-card">
        <el-tabs v-model="activeTab" class="cfg-tabs" :before-leave="beforeLeaveTab">
          <!-- ① 计费 -->
          <el-tab-pane name="billing">
            <template #label>
              <span class="tab-label">计费<i v-if="isDirty('billing')" class="dirty-dot" /></span>
            </template>

            <div class="config-block">
              <div class="block-title">积分换算</div>
              <div class="block-desc">token 计费换算底数与积分价值。R 值决定积分余额量级，积分价值由 R 推导、不可单独修改。</div>

              <div class="form-row">
                <span class="label">R 值（token 缩小分母）</span>
                <el-input-number v-model="r" :min="1" :step="100" />
                <el-tooltip
                  content="token 缩小分母：决定积分余额量级与积分价值。扣费积分 = Σ(token × 倍率) × 时段/分档系数 ÷ R。R 越大，余额数字越小、单次消耗积分越精细。修改只影响之后产生的调用，历史账单不变。"
                  placement="top"
                >
                  <el-icon class="hint-icon"><QuestionFilled /></el-icon>
                </el-tooltip>
              </div>

              <div class="form-row">
                <span class="label">积分价值（只读）</span>
                <span class="static-value">{{ creditValue || '由 R 值推导…' }}</span>
              </div>
            </div>

            <el-divider />

            <div class="config-block">
              <div class="block-title">汇率换算</div>
              <div class="block-desc">仅用于 models.dev 参考价折算：把外部美元报价按汇率换算成倍率后供参考填充。供应商以人民币报价时无需汇率。</div>
              <div class="form-row">
                <span class="label">USD/CNY 汇率</span>
                <el-input-number v-model="cnyRate" :min="0.0001" :precision="4" :step="0.1" />
              </div>
            </div>

            <el-divider />

            <div class="config-block">
              <div class="block-title">计费默认值</div>
              <div class="block-desc">未配置模型级时段/分档的对外模型与渠道内部模型，按此全局默认计费。</div>
              <div class="form-row">
                <span class="label">全局时段/分档默认</span>
                <el-button @click="openFallback">维护默认值</el-button>
                <el-tooltip content="配置时段系数与上下文分档的全局默认值；抽屉保存后本 tab 会自动刷新。" placement="top">
                  <el-icon class="hint-icon"><QuestionFilled /></el-icon>
                </el-tooltip>
              </div>
            </div>

            <el-divider />

            <div class="config-block">
              <div class="block-title">路由成本比较样值</div>
              <div class="block-desc">
                新会话创建时，以该样值用量代入各候选渠道的成本单价求和，比较「谁更便宜」来决定落点（同优先级内取最低成本）。仅用于路由选择，不参与实际计费。
              </div>
              <div class="form-row">
                <span class="label">输入 tokens</span>
                <el-input-number v-model="costSample.input" :min="0" :step="1000" />
              </div>
              <div class="form-row">
                <span class="label">缓存读 tokens</span>
                <el-input-number v-model="costSample.cache_read" :min="0" :step="1000" />
              </div>
              <div class="form-row">
                <span class="label">输出 tokens</span>
                <el-input-number v-model="costSample.output" :min="0" :step="100" />
              </div>
            </div>

            <div class="tab-actions">
              <span v-if="isDirty('billing')" class="dirty-hint">本页签有未保存的修改</span>
              <el-button type="primary" :loading="saving" @click="saveTab('billing')">保存计费配置</el-button>
            </div>
          </el-tab-pane>

          <!-- ② 路由 -->
          <el-tab-pane name="routing">
            <template #label>
              <span class="tab-label">路由<i v-if="isDirty('routing')" class="dirty-dot" /></span>
            </template>

            <div class="config-block">
              <div class="block-title">
                会话成本路由
                <el-tooltip
                  content="按「成本更省」在会话中途换渠道。默认开启；关掉即回退渠道亲和（同一会话钉在同一渠道）。"
                  placement="top"
                >
                  <el-icon class="hint-icon"><QuestionFilled /></el-icon>
                </el-tooltip>
              </div>
              <div class="block-desc">
                默认：启用=开、会话中途切换=开、影子模式=关。下面两个开关直接决定是否启用与会话中途切换，其余参数在「高级参数」里。
              </div>

              <div class="form-row">
                <span class="label">启用会话成本路由</span>
                <el-switch v-model="sessionCost.enabled" active-text="开" inactive-text="关" />
                <span class="hint">关掉后不做成本比较，回退到渠道亲和。</span>
              </div>
              <div class="form-row">
                <span class="label">允许会话中途切换</span>
                <el-switch v-model="sessionCost.mid_switch" active-text="开" inactive-text="关" />
                <span class="hint">关掉后只在会话开始时选一次渠道，中途不再换。</span>
              </div>

              <el-collapse v-model="advancedOpen" class="adv-collapse">
                <el-collapse-item name="adv" title="高级参数">
                  <div class="form-row">
                    <span class="label">影子模式</span>
                    <el-switch v-model="sessionCost.shadow_enabled" active-text="开" inactive-text="关" />
                    <span class="hint warn">会产生额外上游调用与成本，按需开启。</span>
                  </div>
                  <div class="form-row">
                    <span class="label">剩余量权重 a</span>
                    <el-input-number v-model="sessionCost.remaining_a" :min="0" :step="0.1" :precision="2" />
                  </div>
                  <div class="form-row">
                    <span class="label">剩余量权重 b</span>
                    <el-input-number v-model="sessionCost.remaining_b" :min="0" :step="0.1" :precision="2" />
                  </div>
                  <div class="form-row">
                    <span class="label">归一化下界</span>
                    <el-input-number v-model="sessionCost.remaining_min" :min="0" :step="1" />
                  </div>
                  <div class="form-row">
                    <span class="label">归一化上界</span>
                    <el-input-number v-model="sessionCost.remaining_max" :min="0" :step="1" />
                  </div>
                  <div class="form-row">
                    <span class="label">最小剩余量</span>
                    <el-input-number v-model="sessionCost.min_remaining" :min="0" :step="1" />
                    <span class="hint">低于该剩余量不再参与评分。</span>
                  </div>
                  <div class="form-row">
                    <span class="label">切换冷却（秒）</span>
                    <el-input-number v-model="sessionCost.cooldown_sec" :min="0" :step="1" />
                  </div>
                  <div class="form-row">
                    <span class="label">最小改善阈值</span>
                    <el-input-number v-model="sessionCost.min_improve" :min="0" :max="0.999" :step="0.01" :precision="2" />
                    <span class="hint">[0, 1)：候选渠道要比现渠道便宜这个比例才切。</span>
                  </div>
                </el-collapse-item>
              </el-collapse>

              <div v-if="sessionCost.fallback_note" class="fallback-note">
                <span class="note-title">回退说明</span>
                <span class="note-body">{{ sessionCost.fallback_note }}</span>
              </div>
            </div>

            <div class="tab-actions">
              <span v-if="isDirty('routing')" class="dirty-hint">本页签有未保存的修改</span>
              <el-button type="primary" :loading="saving" @click="saveTab('routing')">保存路由配置</el-button>
            </div>
          </el-tab-pane>

          <!-- ③ 网关 -->
          <el-tab-pane name="gateway">
            <template #label>
              <span class="tab-label">网关<i v-if="isDirty('gateway')" class="dirty-dot" /></span>
            </template>

            <div class="config-block">
              <div class="block-title">会话过期分档</div>
              <div class="block-desc">
                过期时长按「上下文长度」与「用户对话次数」两维分别命中档位，取较大者且不低于兜底值。档位为上界桶：实际值 ≤ 上界即命中，超过最大上界取最高档；过期时间只延长、不缩短。
              </div>
              <div class="form-row">
                <span class="label">兜底 TTL（分钟）</span>
                <el-input-number v-model="sessionExpiration.default_ttl_minutes" :min="1" />
              </div>

              <div class="tier-title">上下文分档</div>
              <el-table :data="sessionExpiration.context_tiers" size="small" border>
                <el-table-column label="最大 tokens" min-width="180">
                  <template #default="{ row }">
                    <el-input-number v-model="row.max_tokens" :min="1" controls-position="right" class="tier-num" />
                  </template>
                </el-table-column>
                <el-table-column label="TTL（分钟）" min-width="180">
                  <template #default="{ row }">
                    <el-input-number v-model="row.ttl_minutes" :min="1" controls-position="right" class="tier-num" />
                  </template>
                </el-table-column>
                <el-table-column label="操作" width="90" align="center">
                  <template #default="{ $index }">
                    <el-button link type="danger" @click="removeContextTier($index)">删除</el-button>
                  </template>
                </el-table-column>
              </el-table>
              <el-button class="add-tier-btn" @click="addContextTier">添加档位</el-button>

              <div class="tier-title">对话次数分档</div>
              <el-table :data="sessionExpiration.turn_tiers" size="small" border>
                <el-table-column label="最大对话次数" min-width="180">
                  <template #default="{ row }">
                    <el-input-number v-model="row.max_turns" :min="1" controls-position="right" class="tier-num" />
                  </template>
                </el-table-column>
                <el-table-column label="TTL（分钟）" min-width="180">
                  <template #default="{ row }">
                    <el-input-number v-model="row.ttl_minutes" :min="1" controls-position="right" class="tier-num" />
                  </template>
                </el-table-column>
                <el-table-column label="操作" width="90" align="center">
                  <template #default="{ $index }">
                    <el-button link type="danger" @click="removeTurnTier($index)">删除</el-button>
                  </template>
                </el-table-column>
              </el-table>
              <el-button class="add-tier-btn" @click="addTurnTier">添加档位</el-button>
            </div>

            <el-divider />

            <div class="config-block">
              <div class="block-title">
                全局上游响应头白名单
                <el-tooltip content="上游响应头默认不透传；仅白名单中的头会原样回传给客户端，用于排查链路。" placement="top">
                  <el-icon class="hint-icon"><QuestionFilled /></el-icon>
                </el-tooltip>
              </div>
              <div class="block-desc">留空并保存 = 清空白名单（不透传任何上游响应头）；不保存则保持现值。</div>

              <el-form label-width="130px">
                <el-form-item label="白名单头">
                  <el-select
                    v-model="whitelist"
                    multiple
                    filterable
                    allow-create
                    default-first-option
                    style="width: 100%"
                    placeholder="输入或选择需透传的上游响应头名"
                  >
                    <el-option v-for="h in headerSuggestions" :key="h" :label="h" :value="h" />
                  </el-select>
                  <div class="hint">支持直接输入自定义头名回车添加。</div>
                </el-form-item>
              </el-form>
            </div>

            <div class="tab-actions">
              <span v-if="isDirty('gateway')" class="dirty-hint">本页签有未保存的修改</span>
              <el-button type="primary" :loading="saving" @click="saveTab('gateway')">保存网关配置</el-button>
            </div>
          </el-tab-pane>

          <!-- ④ 运维：非配置类能力（渠道协议备份/恢复），没有 sys_configs 字段、也没有保存按钮 -->
          <el-tab-pane name="ops">
            <template #label>
              <span class="tab-label">运维</span>
            </template>

            <div class="config-block">
              <div class="block-title">备份与恢复</div>
              <div class="block-desc">
                导出全部渠道协议为预设包 JSON，或从该文件导入。导入按名称/来源自动去重，仅新增不覆盖。
                该能力不写系统配置，因此本页签没有「保存」按钮。
              </div>
              <div class="form-row">
                <span class="label">渠道协议</span>
                <el-button type="primary" :loading="exporting" @click="onExport">导出</el-button>
                <el-button :loading="importing" @click="triggerImport">导入</el-button>
                <input ref="fileInput" type="file" accept=".json,application/json" style="display: none" @change="onFileChange" />
              </div>
            </div>
          </el-tab-pane>
        </el-tabs>

        <ErrorBubble
          v-if="errInfo.message || errInfo.requestId"
          :message="errInfo.message"
          :request-id="errInfo.requestId"
        />
      </div>

    </div>

    <!-- 全局时段/分档默认值 抽屉 -->
    <el-drawer v-model="fallbackOpen" title="全局时段/分档默认值" :size="720">
      <div class="fallback-tip">
        当前积分换算 R 值：{{ r }}。这里仅维护时段系数与上下文分档（属「计费」tab 的字段），保存后本页会刷新。
      </div>
      <ModelPricing v-if="fallbackOpen" v-model="fallbackModel" show-rates-key="" title="全局默认定价" />
      <ErrorBubble
        v-if="fallbackErr.message || fallbackErr.requestId"
        :message="fallbackErr.message"
        :request-id="fallbackErr.requestId"
      />
      <template #footer>
        <el-button @click="fallbackOpen = false">取消</el-button>
        <el-button type="primary" :loading="fallbackSaving" @click="saveFallback">保存全局默认</el-button>
      </template>
    </el-drawer>
  </div>
</template>

<style scoped>
.system-page {
  display: flex;
  flex-direction: column;
  min-height: 100%;
}
.system-body {
  flex: 1;
}
.config-card {
  padding: 20px 24px;
}
.backup-card {
  margin-top: 14px;
}
.config-block {
  margin-bottom: 4px;
}
.block-desc {
  color: var(--color-text-secondary);
  font-size: 13px;
  margin-bottom: 14px;
  line-height: 1.6;
}
.tier-title {
  font-size: 14px;
  font-weight: 600;
  color: var(--color-text);
  margin: 16px 0 8px;
}
.add-tier-btn {
  margin-top: 10px;
}
.form-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
}
.label {
  color: var(--color-text-secondary);
  font-size: 13px;
  min-width: 150px;
}
.static-value {
  color: var(--brand-primary);
  font-size: 13px;
  font-weight: 600;
}
.hint-icon {
  cursor: pointer;
  color: var(--color-text-secondary);
}
.hint {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.5;
  color: var(--color-text-secondary);
}
.hint.warn {
  color: #b54708;
}
.tier-num {
  width: 100%;
  max-width: 160px;
}
/* tab 标题：右上角小圆点表示该 tab 有未保存修改 */
.tab-label {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.dirty-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: #f79009;
  display: inline-block;
}
.tab-actions {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 12px;
  margin-top: 18px;
  padding-top: 14px;
  border-top: 1px solid var(--color-border);
}
.dirty-hint {
  color: #b54708;
  font-size: 12px;
}
.adv-collapse {
  margin: 4px 0 6px;
  border-top: none;
}
.fallback-note {
  margin-top: 12px;
  padding: 10px 12px;
  border: 1px dashed var(--color-border);
  border-radius: 8px;
  background: #f9fafb;
  font-size: 12px;
  line-height: 1.6;
  color: var(--color-text-secondary);
}
.note-title {
  font-weight: 600;
  color: var(--color-text);
  margin-right: 8px;
}
.fallback-tip {
  background: #f6f8ff;
  border: 1px dashed #a5c0f7;
  color: #4a7bd9;
  font-size: 12px;
  padding: 8px 12px;
  border-radius: 6px;
  margin-bottom: 12px;
}

/* ≤640px：单列 + 正文 ≥14px（含表格、注记、徽标），标签不再占固定 150px */
@media (max-width: 640px) {
  .config-card {
    padding: 14px 14px;
  }
  .form-row {
    align-items: stretch;
    flex-direction: column;
    gap: 6px;
  }
  .label {
    min-width: 0;
    font-size: 14px;
  }
  .block-desc,
  .hint,
  .hint.warn,
  .static-value,
  .dirty-hint,
  .fallback-note,
  .fallback-tip,
  .tier-title {
    font-size: 14px;
  }
  .tier-num {
    max-width: none;
  }
  :deep(.el-tabs__item) {
    font-size: 14px;
  }
  :deep(.el-collapse-item__header),
  :deep(.el-collapse-item__title) {
    font-size: 14px;
  }
  :deep(.el-table) {
    font-size: 14px;
  }
  :deep(.el-table .cell),
  :deep(.el-table td.el-table__cell),
  :deep(.el-table th.el-table__cell),
  :deep(.el-table__empty-text) {
    font-size: 14px;
  }
  :deep(.el-form-item__label) {
    font-size: 14px;
  }
  :deep(.el-button) {
    font-size: 14px;
  }
  .tab-actions {
    flex-direction: column;
    align-items: stretch;
  }
  .dirty-hint {
    text-align: right;
  }
}
</style>
