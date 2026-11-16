<script setup lang="ts">
// 订阅账本（P04）——阶段二 A5 月费分摊的**唯一录入入口**。
//
// 口径唯一依据：`docs/designs/2026-11-16-subscription-entry-standard.md`（Spec 2026-11-16 生效版）。
// 页面存在的理由不是「能存一行」，而是让运营一眼看出**这行会不会被摊、摊不出去卡在哪一条**：
//   1. 单位：录入收「元/美元」，落库 `price_amount` 是**积分**（× 1e6 ÷ R，USD 先 × cny_rate）——所以单价旁边必须实时显示折算积分；
//   2. 唯一键 (Provider, Account)：重复录入一律 409，不做静默覆盖；前端先本地查重再弹「改为编辑」；
//   3. 六条件 AND 才摊得到，录入响应只承诺 C1–C4（WillAmortize/BlockReasons），C5/C6 靠本期分摊明细回显；
//   4. 状态是派生的（active/pending/expired/stopped/once/missing_cycle），不落列，页面自己按 §2.3 算。
//
// 本轮范围：S1/S2/S4/S5/S6/S7 均未上线 → 列表空态 + 显式演示数据；表单全部校验与判定照标准实现，
// 提交只给判定结果、不写库（Spec 已定「保存不写库」）。接口就绪后只需把 submit 里的提示换成 createSubscription 调用。
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import {
  formatYmd,
  getSubscriptionFormOptions,
  getSubscriptions,
  priceToCredits,
  staticAmortizeVerdict,
  type BlockReasonCode,
  type PriceCurrency,
  type PricingCycle,
  type Subscription,
  type SubscriptionFormOptions,
  type SubscriptionStatus,
  type SubscriptionView,
} from '@/api/v2'
import { mockFormOptions, mockSubscriptions } from '@/mock/subscriptions'
import { MOCK_ENABLED } from '@/mock'
import { useV2Load } from '@/composables/useV2Load'
import V2Page from '@/components/v2/V2Page.vue'
import type { V2Dep } from '@/components/v2/types'
import { fmtCredits, fmtDate } from '@/utils/format'

const { state, data, error, load, showDemo, exitDemo } = useV2Load<SubscriptionView>(getSubscriptions, mockSubscriptions)

onMounted(() => {
  void load()
  void loadOptions()
})

// 六个接口分页依赖，徽标逐条列出：让 reviewer 一眼看到「哪一条挡着」
const deps = computed<V2Dep[]>(() => [
  { path: 'GET /admin/subscriptions', state: state.value === 'live' ? 'ready' : 'pending' },
  { path: 'POST /admin/subscriptions', state: 'pending' },
  { path: 'GET /admin/subscriptions/{id}/allocations', state: 'pending' },
  { path: 'POST /admin/subscriptions/{id}/roll-cycle', state: 'pending' },
  { path: 'POST /admin/subscriptions/amortize/run', state: 'pending' },
  { path: 'GET /admin/subscriptions/form-options', state: options.value ? 'ready' : 'pending' },
])

// ===== 派生状态 / 原因码文案（标准 §2.3、§5.2，逐条照抄，不给运营看代号） =====
const STATUS_TEXT: Record<SubscriptionStatus, string> = {
  active: '分摊中',
  pending: '待生效',
  expired: '账期已结束',
  stopped: '已停摊',
  once: '一次性（不摊）',
  missing_cycle: '缺账期起点',
}
const STATUS_TAG: Record<SubscriptionStatus, 'success' | 'primary' | 'warning' | 'info' | 'danger'> = {
  active: 'success',
  pending: 'primary',
  expired: 'warning',
  stopped: 'info',
  once: 'info',
  missing_cycle: 'danger',
}
const REASON_TEXT: Record<BlockReasonCode, { text: string; fix: string }> = {
  share_off: { text: '已停摊', fix: '打开「参与分摊」' },
  price_zero: { text: '费用为 0，不参与分摊', fix: '改价' },
  cycle_once: { text: '一次性费用不分摊（设计如此）', fix: '无需处理' },
  cycle_missing: { text: '缺账期起点', fix: '补账期起点' },
  cycle_expired: { text: '账期已结束，未结转', fix: '点「结转到本期」' },
  no_billing_rows: { text: '本期该账号没有账单', fix: '确认是否有流量' },
  no_attribution: { text: '有账单但缺归因（upstream_account 为空）', fix: '找 Core 查归因写入' },
  account_mismatch: { text: '账单账号与订阅账号不一致', fix: '核对账号拼写' },
  ledger_taken: { text: '可摊账单已被账本成本占用', fix: '正常（变动成本优先）' },
  unknown: { text: '未知原因，看接口 Detail', fix: '报 Core' },
}
// skip_reason 中文文案（P06 也用它，放这里会重复；各页各自持有一份小字典，避免为一个字符串建公共模块）
const reasonText = (c: BlockReasonCode) => REASON_TEXT[c]?.text || c

// ===== 列表 =====
const rows = computed(() => data.value?.Items || [])

// 顶部固定条：本月有 N 条未摊出、合计 M 积分（0 时隐藏，标准 §8.2）
const unallocated = computed(() => {
  const list = rows.value.filter((r) => (r.CycleUnallocated ?? 0) > 0)
  return { count: list.length, sum: Math.round(list.reduce((s, r) => s + (r.CycleUnallocated || 0), 0) * 1e5) / 1e5 }
})

function creditText(v: number | null): string {
  return v == null ? '未计算' : fmtCredits(v)
}

// ===== 录入表单（标准 §8.1 的字段、控件、默认值、交互逐条落地） =====
const dialog = ref(false)
const formRef = ref<FormInstance>()
const formError = ref('')
const submitting = ref(false)

interface Draft {
  Provider: string
  Account: string
  Price: number
  Currency: PriceCurrency
  Cycle: PricingCycle
  PayDay: number
  ShareCost: boolean
  CycleStart: string
  Note: string
}

function firstOfMonth(): string {
  const d = new Date()
  return formatYmd(new Date(d.getFullYear(), d.getMonth(), 1))
}

function todayStr(): string {
  return formatYmd(new Date())
}

function emptyDraft(): Draft {
  return {
    Provider: '',
    Account: '',
    Price: 0,
    Currency: 'CNY',
    Cycle: 'month',
    PayDay: 1,
    ShareCost: true,
    // 默认本月 1 日（标准 §8.1）：账期起点决定分摊窗口，空着等于不摊
    CycleStart: firstOfMonth(),
    Note: '',
  }
}

const draft = reactive<Draft>(emptyDraft())
// 账号手输开关：候选接口（S7）未就绪时也允许手输，但要显式提示「账号未在近期账单中出现」的风险
const manualAccount = ref(false)
const options = ref<SubscriptionFormOptions | null>(null)

async function loadOptions() {
  const res = await getSubscriptionFormOptions()
  if (res.state === 'live') {
    options.value = res.data
    return
  }
  // 演示环境：候选也用 mock，否则表单里的 provider 下拉是空的，演示不了
  if (MOCK_ENABLED) options.value = mockFormOptions()
}

// 演示态：表单候选也用 mock（否则演示时下拉是空的，R 也未知，折算积分与 provider 校验都演示不了）
watch(state, (v) => {
  if (v === 'demo' && !options.value) options.value = mockFormOptions()
})

const providers = computed(() => options.value?.Providers || [])
const accounts = computed(() => options.value?.Accounts || [])
// 候选接口没上线时允许自由输入（否则整页不可用），并在表单顶部标注——这是唯一一处对「不可自由输入」的让步
const providerFreeInput = computed(() => providers.value.length === 0)
const accountUnverified = computed(
  () => !!draft.Account && accounts.value.length > 0 && !accounts.value.includes(draft.Account),
)

// 实时折算积分：标准 §8.1 要求「右侧实时显示 ≈ N 积分」。
// 缺 R 或缺汇率时返回 null → 显示「—」，绝不用 0 让人以为不花钱。
const credits = computed(() =>
  priceToCredits(draft.Price, draft.Currency, options.value?.CnyRate ?? null, options.value?.R ?? null),
)
const rateMissing = computed(() => draft.Currency === 'USD' && !(options.value?.CnyRate ?? 0))

// 账期起点早于今天 → 灰字提示「已过期，保存后需结转」（不阻断，标准 §3.3）
const cycleStartPast = computed(() => !!draft.CycleStart && draft.CycleStart < todayStr())
// 「一次性」不摊：隐藏账期起点并提示（标准 §8.1）
const isOnce = computed(() => draft.Cycle === 'once')

const rules: FormRules = {
  Provider: [
    {
      validator: (_r, v: string, cb) => {
        const s = (v || '').trim()
        if (!s) return cb(new Error('订阅方不能为空'))
        if (s.length > 128) return cb(new Error('订阅方最多 128 字符'))
        if (/[\r\n]/.test(s)) return cb(new Error('订阅方不能含换行'))
        cb()
      },
      trigger: 'blur',
    },
  ],
  Account: [
    {
      validator: (_r, v: string, cb) => {
        const s = (v || '').trim()
        if (!s) return cb(new Error('账号不能为空'))
        if (s.length > 128) return cb(new Error('账号最多 128 字符'))
        if (/[\r\n]/.test(s)) return cb(new Error('账号不能含换行'))
        cb()
      },
      trigger: 'blur',
    },
  ],
  Price: [
    {
      validator: (_r, v: number, cb) => {
        if (typeof v !== 'number' || !Number.isFinite(v) || v < 0) return cb(new Error('整期费用必须 ≥ 0'))
        if (v > 1e9) return cb(new Error('整期费用不能超过 1e9（元）'))
        // 录入按「元」最多 2 位小数（折算后的积分才是 5 位）
        if (Math.round(v * 100) !== v * 100) return cb(new Error('金额最多 2 位小数（元）'))
        if (v === 0 && draft.ShareCost) return cb(new Error('0 元只能作为停摊占位（先关闭「参与分摊」）'))
        cb()
      },
      trigger: 'blur',
    },
  ],
  PayDay: [
    {
      validator: (_r, v: number, cb) => {
        if (!Number.isInteger(v) || v < 1 || v > 28) return cb(new Error('付款日必须是 1–28（29–31 会跨月错位）'))
        cb()
      },
      trigger: 'blur',
    },
  ],
  CycleStart: [
    {
      validator: (_r, v: string, cb) => {
        // 参与分摊且非一次性 → 必填（标准 §2.1：账期起点「参与分摊必填」）
        if (draft.ShareCost && !isOnce.value && !v) return cb(new Error('参与分摊必须填账期起点，否则不摊'))
        if (v && v < '2000-01-01') return cb(new Error('账期起点不能早于 2000-01-01'))
        cb()
      },
      trigger: 'change',
    },
  ],
  Note: [
    {
      validator: (_r, v: string, cb) => {
        if ((v || '').length > 500) return cb(new Error('备注最多 500 字'))
        cb()
      },
      trigger: 'blur',
    },
  ],
}

function openCreate() {
  Object.assign(draft, emptyDraft())
  manualAccount.value = false
  formError.value = ''
  verdict.value = null
  dialog.value = true
}

// ===== 判定回显（标准 §5.3 / §8.1 的最后一条：保存成功也必须展示 WillAmortize 与 BlockReasons） =====
const verdict = ref<{ will: boolean; reasons: BlockReasonCode[] } | null>(null)

// 本地查重：真实 409 的 ExistingID 也走这一条（接口就绪后按 (Provider, Account) 反查即可），
// 前端先把「重复录入一律 409、不做静默覆盖」这条讲清楚。
function findExisting(provider: string, account: string): Subscription | undefined {
  const p = provider.trim().toLowerCase()
  const a = account.trim().toLowerCase()
  return rows.value.find((s) => s.Provider.trim().toLowerCase() === p && s.Account.trim().toLowerCase() === a)
}

async function submit() {
  formError.value = ''
  if (rateMissing.value) {
    formError.value = 'USD 汇率未配置，暂不能按美元录入（422 rate_not_configured）。请先到「系统设置 → 计费参数」配 billing.cny_rate。'
    return
  }
  const ok = await formRef.value?.validate().catch(() => false)
  if (!ok) return

  const dup = findExisting(draft.Provider, draft.Account)
  if (dup) {
    // 标准 §8.3：弹窗「已存在（ID …），是否改为编辑？」
    await ElMessageBox.confirm(
      `该账号已有订阅（ID ${dup.ID}：${dup.Provider} / ${dup.Account}）。唯一键 (订阅方, 账号) 不允许重复录入，是否改为编辑这一条？`,
      '重复订阅（409 duplicate_subscription）',
      { confirmButtonText: '去编辑', cancelButtonText: '取消', type: 'warning' },
    ).catch(() => 'cancel')
    return
  }

  const will = staticAmortizeVerdict(draft, credits.value)
  verdict.value = will
  submitting.value = true
  // S2（POST /admin/subscriptions）未上线：本轮只回判定结果，不落库。
  // 接线点：`await createSubscription({ ...draft })` → state='live' 时用返回值覆盖 verdict（后端口径优先）。
  await new Promise((r) => setTimeout(r, 200))
  submitting.value = false
  ElMessage.warning('录入接口（S2）待 Core 上线：本次只做前端校验与分摊判定，未写库')
}

// 处置动作：原因码 → 焦点/开关（标准 §8.1「给处置按钮」）
function fixBy(reason: BlockReasonCode) {
  if (reason === 'share_off') draft.ShareCost = true
  else if (reason === 'cycle_missing' || reason === 'cycle_expired') draft.CycleStart = firstOfMonth()
  else if (reason === 'price_zero') draft.Price = 1
}
</script>

<template>
  <V2Page
    title="订阅账本"
    intro="录入上游账号的固定订阅费，让 A5 月费分摊真正有数据；并让「摊不出去」这件事在页面上可见——不录 = 盈亏页成本永远少一笔固定支出。"
    source="Spec 2026-11-16《订阅录入标准》§2.1/§2.3/§3.1/§5.2/§8 · 数据层契约 §2.4"
    :deps="deps"
    :state="state"
    :error="error"
    pending-hint="S1/S2/S4/S5/S6/S7 六个接口待 Core 上线。页面结构、校验、分摊判定与原因码文案已按录入标准实现；接口就绪后只需把 submit 的提示换成真实调用。"
    @reload="load()"
    @demo="showDemo"
    @exit-demo="exitDemo"
  >
    <template #actions>
      <el-button size="small" type="primary" @click="openCreate">新增订阅</el-button>
    </template>

    <!-- 顶部固定条：本月 N 条未摊出 / 合计 M 积分，为 0 时隐藏（标准 §8.2） -->
    <div v-if="unallocated.count > 0" class="warn-bar">
      <span>
        本月有 <strong>{{ unallocated.count }}</strong> 条订阅未摊出，合计
        <strong>{{ creditText(unallocated.sum) }}</strong> 积分——这些固定成本还没进盈亏页。
      </span>
      <el-button size="small" text type="primary" @click="dialog = false">查看未摊出</el-button>
    </div>

    <template v-if="data">
      <div class="card">
        <div class="table-wrap">
          <el-table :data="rows" size="small" border stripe empty-text="还没有订阅。录入后下一轮（≤5 分钟）开始分摊；不录 = 盈亏页成本永远少一笔固定支出。">
            <el-table-column prop="Provider" label="订阅方" width="120" show-overflow-tooltip />
            <el-table-column prop="Account" label="账号" width="130" show-overflow-tooltip />
            <el-table-column label="整期费用" width="190" align="right">
              <template #default="{ row }">
                <div>{{ row.Price.toFixed(2) }} <span class="sub">{{ row.Currency }}</span></div>
                <div class="sub">≈ {{ row.PriceCredits == null ? '未折算' : fmtCredits(row.PriceCredits) }} 积分</div>
              </template>
            </el-table-column>
            <el-table-column label="周期" width="90">
              <template #default="{ row }">
                {{ row.Cycle === 'month' ? '月付' : row.Cycle === 'year' ? '年付' : '一次性' }}
              </template>
            </el-table-column>
            <el-table-column label="账期" width="150">
              <template #default="{ row }">
                <span v-if="row.CycleStart">{{ fmtDate(row.CycleStart) }} 起</span>
                <span v-else class="sub">未填</span>
                <div class="sub">付款日 {{ row.PayDay }}</div>
              </template>
            </el-table-column>
            <el-table-column label="状态" width="130">
              <template #default="{ row }">
                <el-tag :type="STATUS_TAG[row.Status as SubscriptionStatus]" size="small" effect="plain">
                  {{ STATUS_TEXT[row.Status as SubscriptionStatus] }}
                </el-tag>
                <div v-if="row.Status === 'pending' && row.CycleStart" class="sub">
                  {{ fmtDate(row.CycleStart) }} 起摊
                </div>
              </template>
            </el-table-column>
            <el-table-column label="本期已摊" width="110" align="right">
              <template #default="{ row }">{{ creditText(row.CycleAllocated) }}</template>
            </el-table-column>
            <el-table-column label="未摊出" width="130" align="right">
              <template #default="{ row }">
                <span v-if="(row.CycleUnallocated ?? 0) > 0" class="unalloc">{{ creditText(row.CycleUnallocated) }}</span>
                <span v-else>{{ creditText(row.CycleUnallocated) }}</span>
                <!-- 未摊出必须能点开看到原因（Spec §7 全局口径 11） -->
                <el-tooltip v-if="row.BlockReasons.length" placement="top">
                  <template #content>
                    <div v-for="r in row.BlockReasons" :key="r" class="tip-line">
                      {{ reasonText(r as BlockReasonCode) }} → {{ REASON_TEXT[r as BlockReasonCode].fix }}
                    </div>
                  </template>
                  <span class="reason-count">{{ row.BlockReasons.length }} 条原因</span>
                </el-tooltip>
              </template>
            </el-table-column>
            <el-table-column label="操作" width="230" fixed="right">
              <template #default>
                <!-- S4/S5/S6 未上线：按 Spec §5 规则 5「禁用 + 待接入提示」，不做点了没反应的按钮 -->
                <el-tooltip content="接口 S4（本期分摊明细）待上线" placement="top">
                  <span><el-button size="small" text type="primary" disabled>分摊明细</el-button></span>
                </el-tooltip>
                <el-tooltip content="接口 S5（账期结转）待上线" placement="top">
                  <span><el-button size="small" text disabled>结转到本期</el-button></span>
                </el-tooltip>
              </template>
            </el-table-column>
          </el-table>
        </div>
        <div class="table-foot">
          <span class="sub">共 {{ data.Total }} 条。分摊生效 = 下一轮（≤5 分钟）后账单出现 cost_source=amortized 且 Σcost_credits = 折算积分；录入成功 ≠ 已摊出。</span>
        </div>
      </div>
    </template>

    <el-empty
      v-else-if="state === 'live'"
      description="还没有订阅。不录订阅 = 盈亏页成本永远少一笔固定支出。"
    />

    <!-- 录入表单 -->
    <el-dialog v-model="dialog" title="新增订阅" width="600px" :close-on-click-modal="false">
      <el-alert v-if="formError" class="form-error" type="error" :closable="false" show-icon :title="formError" />
      <el-alert
        v-if="providerFreeInput"
        class="form-tip"
        type="info"
        :closable="false"
        show-icon
        title="候选接口（S7）未接入，订阅方暂允许手输"
        description="标准要求订阅方只能从网关镜像里选；接口就绪后本项改为纯下拉，手输入口会去掉。"
      />

      <el-form
        ref="formRef"
        :model="draft"
        :rules="rules"
        :class="{ 'is-stopped': !draft.ShareCost }"
        label-width="110px"
        size="small"
      >
        <el-form-item label="订阅方" prop="Provider">
          <el-select
            v-model="draft.Provider"
            filterable
            :allow-create="providerFreeInput"
            default-first-option
            :reserve-keyword="false"
            placeholder="网关 provider（不是老库渠道名）"
            style="width: 220px"
          >
            <el-option v-for="p in providers" :key="p" :label="p" :value="p" />
          </el-select>
          <span class="hint">不在网关镜像里 → 400 provider_not_found（去拓扑页同步）</span>
        </el-form-item>

        <el-form-item label="账号" prop="Account">
          <el-select
            v-model="draft.Account"
            filterable
            allow-create
            default-first-option
            :reserve-keyword="false"
            placeholder="网关账号 id / name"
            style="width: 220px"
          >
            <el-option v-for="a in accounts" :key="a" :label="a" :value="a" />
          </el-select>
          <el-switch v-model="manualAccount" class="inline-switch" size="small" active-text="手动输入" />
        </el-form-item>
        <el-form-item v-if="accountUnverified" label=" " class="risk-row">
          <span class="risk">
            账号未在近 30 天账单/账本中出现（account_unverified）：允许保存，但拼错会导致永远摊 0 笔。
          </span>
        </el-form-item>

        <el-form-item label="整期费用" prop="Price">
          <el-input-number v-model="draft.Price" :min="0" :max="1e9" :step="50" :precision="2" style="width: 160px" />
          <el-select v-model="draft.Currency" style="width: 92px; margin-left: 8px">
            <el-option label="CNY" value="CNY" />
            <el-option label="USD" value="USD" />
          </el-select>
          <span class="credits">≈ {{ credits == null ? '—（缺 R 或汇率）' : fmtCredits(credits) }} 积分</span>
        </el-form-item>
        <el-form-item v-if="rateMissing" label=" " class="risk-row">
          <span class="risk">USD 汇率（billing.cny_rate）未配置：422 rate_not_configured，去「系统设置 → 计费参数」配。</span>
        </el-form-item>

        <el-form-item label="计费周期">
          <el-radio-group v-model="draft.Cycle">
            <el-radio label="month">月</el-radio>
            <el-radio label="year">年</el-radio>
            <el-radio label="once">一次性</el-radio>
          </el-radio-group>
          <span v-if="isOnce" class="hint">一次性费用不分摊（状态 once），不进「未摊出」告警</span>
        </el-form-item>

        <el-form-item v-if="!isOnce" label="账期起点" prop="CycleStart">
          <el-date-picker
            v-model="draft.CycleStart"
            type="date"
            value-format="YYYY-MM-DD"
            placeholder="账期窗口起点"
            style="width: 190px"
          />
          <span v-if="cycleStartPast" class="hint warn-text">已过期：保存后状态为「账期已结束」，需点「结转到本期」才会摊</span>
          <span v-else class="hint">分摊窗口 = [起点, 起点 + 1 期)</span>
        </el-form-item>

        <el-form-item label="付款日" prop="PayDay">
          <el-input-number v-model="draft.PayDay" :min="1" :max="28" style="width: 130px" />
          <span class="hint">1–28（29–31 会跨月错位）；仅提醒用，不参与计算</span>
        </el-form-item>

        <el-form-item label="参与分摊">
          <el-switch v-model="draft.ShareCost" />
          <span class="hint">
            {{ draft.ShareCost ? '满足 6 个条件后按月费行级分摊' : '停摊后不再新增分摊，已摊金额不回滚' }}
          </span>
        </el-form-item>

        <el-form-item label="备注" prop="Note">
          <el-input v-model="draft.Note" type="textarea" :rows="2" maxlength="500" show-word-limit placeholder="哪个套餐 / 谁批的" />
        </el-form-item>
      </el-form>

      <!-- 判定回显：WillAmortize / BlockReasons / Warnings -->
      <div v-if="verdict" class="verdict" :class="verdict.will ? 'is-ok' : 'is-block'">
        <div class="verdict-head">
          <strong>{{ verdict.will ? '会参与分摊（C1–C4 已满足）' : '不会参与分摊' }}</strong>
          <el-tag :type="verdict.will ? 'success' : 'warning'" size="small" effect="plain">
            WillAmortize = {{ verdict.will }}
          </el-tag>
        </div>
        <ul v-if="verdict.reasons.length" class="verdict-list">
          <li v-for="r in verdict.reasons" :key="r">
            <code>{{ r }}</code> {{ reasonText(r) }}
            <el-button size="small" text type="primary" @click="fixBy(r)">{{ REASON_TEXT[r].fix }}</el-button>
          </li>
        </ul>
        <p class="verdict-note">
          {{
            verdict.will
              ? '下一轮分摊 ≤5 分钟；该账号当期若无账单会进「未摊出」告警（C5/C6 属运行时事实，不在此承诺）。'
              : '以上是静态条件（C1–C4）；改完再提交即可。'
          }}
        </p>
        <p v-if="accountUnverified" class="verdict-note risk">
          Warnings：account_unverified —— 账号未在近 30 天账单/账本中出现，确认拼写。
        </p>
      </div>

      <template #footer>
        <span class="footer-hint">本轮只做前端校验与分摊判定，不写库（S2 待 Core）</span>
        <el-button size="small" @click="dialog = false">取消</el-button>
        <el-button size="small" type="primary" :loading="submitting" @click="submit">校验并通过</el-button>
      </template>
    </el-dialog>
  </V2Page>
</template>

<style scoped>
.card {
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-m);
  box-shadow: var(--shadow-card);
  padding: 12px 14px;
}
.table-wrap {
  overflow-x: auto;
}
.table-foot {
  padding-top: 8px;
}
.warn-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 8px 12px;
  border-radius: var(--radius-s);
  background: #fff8e6;
  border: 1px solid #ffe0a3;
  color: #8a5a00;
  font-size: 13px;
}
.sub {
  font-size: 11px;
  color: var(--color-text-tertiary);
}
.unalloc {
  color: #d97706;
  font-weight: 600;
}
.reason-count {
  display: block;
  font-size: 11px;
  color: #d97706;
  cursor: help;
}
.tip-line {
  line-height: 1.6;
}
.hint {
  margin-left: 8px;
  font-size: 12px;
  color: var(--color-text-tertiary);
}
.warn-text {
  color: #d97706;
}
.credits {
  margin-left: 10px;
  font-size: 12px;
  color: var(--color-text-secondary);
}
.inline-switch {
  margin-left: 10px;
}
.risk-row {
  margin-top: -6px;
}
.risk {
  font-size: 12px;
  color: #d97706;
}
.form-error,
.form-tip {
  margin-bottom: 10px;
}
.is-stopped :deep(.el-form-item) {
  opacity: 0.65;
}
.verdict {
  margin-top: 6px;
  padding: 10px 12px;
  border-radius: var(--radius-s);
  border: 1px solid var(--color-border);
  background: #fafbfd;
}
.verdict.is-ok {
  border-color: #b7ebc6;
  background: #f2fcf5;
}
.verdict.is-block {
  border-color: #ffd8a8;
  background: #fffaf2;
}
.verdict-head {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
}
.verdict-list {
  margin: 6px 0 0;
  padding-left: 18px;
  font-size: 13px;
  line-height: 1.9;
}
.verdict-note {
  margin: 6px 0 0;
  font-size: 12px;
  color: var(--color-text-secondary);
}
.verdict-note.risk {
  color: #d97706;
}
.footer-hint {
  float: left;
  font-size: 12px;
  color: var(--color-text-tertiary);
  line-height: 24px;
}

@media (max-width: 640px) {
  .card {
    padding: 10px;
  }
  :deep(.el-table) {
    font-size: 14px;
  }
  .warn-bar {
    flex-direction: column;
    align-items: flex-start;
  }
}
</style>
