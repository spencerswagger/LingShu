<script setup lang="ts">
// 订阅管理（账号财务扩展，一对一）。
//
// 现状：Spec 的「录入标准」尚未定稿，本页先按数据层契约 §2.4 的列做**字段壳 + 前端校验 + 错误提示位**，
// 保存按钮暂不写库（Spec 定稿后只需替换 submit 的实现，不改表单）。
//
// 为什么必须先有这一页：月费不录入 → 订阅摊销恒 0 → 毛利虚高且**没有告警**（契约 §1.3-C8），
// 盈亏页的成本侧会一直缺一块。所以录入入口是阶段二尾部的前置件。
//
// 校验口径（全部来自契约，不自己加业务规则）：
//   provider_ref + account_ref 必填，(provider_ref, account_ref) 唯一；
//   price_amount ≥ 0，NUMERIC(20,5) 精度（最多 5 位小数）；
//   price_currency ∈ {CNY, USD}；billing_cycle ∈ {month, year, once}；
//   pay_day ∈ [1, 28]（29–31 会跨月错位）；
//   share_cost = 是否参与成本分摊；cycle_start 为空 = 未开始摊销。
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { getSubscriptions, type BillingCycle, type PriceCurrency, type Subscription, type SubscriptionView } from '@/api/v2'
import { mockSubscriptions } from '@/api/v2.mock'
import { useV2Load } from '@/composables/useV2Load'
import V2Page from '@/components/v2/V2Page.vue'
import StatCards from '@/components/v2/StatCards.vue'
import type { StatItem, V2Dep } from '@/components/v2/types'
import { fmtDate, fmtDateTime } from '@/utils/format'

const { state, data, error, load, showDemo, exitDemo } = useV2Load<SubscriptionView>(getSubscriptions, mockSubscriptions)

onMounted(() => void load())

const deps = computed<V2Dep[]>(() => [
  { path: 'GET /admin/subscriptions', state: state.value === 'live' || state.value === 'demo' ? 'ready' : 'pending' },
  { path: 'POST /admin/subscriptions', state: 'pending' },
])

const stats = computed<StatItem[]>(() => {
  const d = data.value
  if (!d) return []
  return [
    { label: '订阅账号', value: String(d.Items.length), hint: '一行一个（供应商, 账号）' },
    {
      label: '月费合计（CNY·月付）',
      value: d.MonthlyTotal.toFixed(2),
      hint: '只累加 CNY 月付；USD/年付不换算（换算只在成本侧一次）',
    },
    {
      label: '未开始摊销',
      value: String(d.UnstartedCount),
      tone: d.UnstartedCount > 0 ? 'warn' : 'ok',
      hint: 'cycle_start 为空 = 不参与摊销，该账号成本会缺一块',
    },
  ]
})

// ===== 录入表单 =====
const dialog = ref(false)
const formRef = ref<FormInstance>()
const formError = ref('')

interface Draft {
  ProviderRef: string
  AccountRef: string
  PriceAmount: number
  PriceCurrency: PriceCurrency
  BillingCycle: BillingCycle
  PayDay: number
  ShareCost: boolean
  CycleStart: string
  Note: string
}

function emptyDraft(): Draft {
  return {
    ProviderRef: '',
    AccountRef: '',
    PriceAmount: 0,
    PriceCurrency: 'CNY',
    BillingCycle: 'month',
    PayDay: 1,
    ShareCost: true,
    CycleStart: '',
    Note: '',
  }
}

const draft = reactive<Draft>(emptyDraft())

// 唯一键本地校验：先在已加载的行里查重，避免提交后才被后端 uk 打回
function duplicate(a: Draft): Subscription | undefined {
  return (data.value?.Items || []).find(
    (s) =>
      s.ProviderRef.trim().toLowerCase() === a.ProviderRef.trim().toLowerCase() &&
      s.AccountRef.trim().toLowerCase() === a.AccountRef.trim().toLowerCase(),
  )
}

const rules: FormRules = {
  ProviderRef: [{ required: true, message: '必须填供应商（网关 provider 名）', trigger: 'blur' }],
  AccountRef: [{ required: true, message: '必须填账号（网关账号 id 或 name）', trigger: 'blur' }],
  PriceAmount: [
    {
      validator: (_r, v, cb) => {
        if (typeof v !== 'number' || !Number.isFinite(v) || v < 0) return cb(new Error('账期费用必须 ≥ 0'))
        if (Math.round(v * 1e5) !== v * 1e5) return cb(new Error('最多 5 位小数（NUMERIC(20,5)）'))
        cb()
      },
      trigger: 'blur',
    },
  ],
  PayDay: [
    {
      validator: (_r, v, cb) => {
        if (!Number.isInteger(v) || v < 1 || v > 28) return cb(new Error('账期日必须在 1–28（29–31 会跨月错位）'))
        cb()
      },
      trigger: 'blur',
    },
  ],
}

function openCreate() {
  Object.assign(draft, emptyDraft())
  formError.value = ''
  dialog.value = true
}

async function submit() {
  formError.value = ''
  const ok = await formRef.value?.validate().catch(() => false)
  if (!ok) return
  const dup = duplicate(draft)
  if (dup) {
    formError.value = `已存在同一 (供应商, 账号) 的订阅：#${dup.ID}。一对一由唯一键保证，不能重复录入。`
    return
  }
  // 录入标准（必填/默认值/是否允许修改 provider_ref）待 Spec 定稿：此处只做「校验通过」的确认，不写库。
  ElMessage.warning('录入接口与标准待 Spec 定稿，本次未提交')
}
</script>

<template>
  <V2Page
    title="订阅管理"
    intro="账号级月费与账期。订阅摊销按窗口从账本现算，本页只维护「账号 → 账期费用」这一条财务事实。"
    source="数据层契约 §2.4（subscriptions）+ §1.3-C8 / §7-8（录入来源与漏录后果）"
    :deps="deps"
    :state="state"
    :error="error"
    pending-hint="录入标准（必填项、默认值、可否改供应商、错误码）待 Spec 定稿；读取接口也未上线，本页先出字段壳与校验。"
    @reload="load()"
    @demo="showDemo"
    @exit-demo="exitDemo"
  >
    <template #actions>
      <el-button size="small" type="primary" @click="openCreate">新增订阅</el-button>
    </template>

    <el-alert
      type="warning"
      :closable="false"
      show-icon
      title="漏录 = 毛利虚高且没有告警"
      description="没有账期费用的账号，摊销成本恒 0。上线前置项要求「订阅类账号每行都有 price_amount 与 billing_cycle」。"
    />

    <template v-if="data">
      <StatCards :items="stats" :columns="3" />

      <div class="card">
        <div class="table-wrap">
          <el-table :data="data.Items" size="small" border stripe empty-text="还没有订阅记录">
            <el-table-column prop="ProviderRef" label="供应商" width="130" show-overflow-tooltip />
            <el-table-column prop="AccountRef" label="账号" width="140" show-overflow-tooltip />
            <el-table-column label="账期费用" width="140" align="right">
              <template #default="{ row }">
                {{ row.PriceAmount.toFixed(2) }} <span class="sub">{{ row.PriceCurrency }}</span>
              </template>
            </el-table-column>
            <el-table-column label="周期" width="90">
              <template #default="{ row }">
                {{ row.BillingCycle === 'month' ? '月付' : row.BillingCycle === 'year' ? '年付' : '一次性' }}
              </template>
            </el-table-column>
            <el-table-column label="账期日" width="90" align="right">
              <template #default="{ row }">{{ row.PayDay }}</template>
            </el-table-column>
            <el-table-column label="参与分摊" width="100">
              <template #default="{ row }">
                <el-tag :type="row.ShareCost ? 'success' : 'info'" size="small" effect="plain">
                  {{ row.ShareCost ? '参与' : '不分摊' }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column label="当前账期起点" width="140">
              <template #default="{ row }">
                <span v-if="row.CycleStart">{{ fmtDate(row.CycleStart) }}</span>
                <el-tag v-else type="warning" size="small" effect="plain">未开始摊销</el-tag>
              </template>
            </el-table-column>
            <el-table-column prop="Note" label="备注" min-width="160" show-overflow-tooltip />
            <el-table-column label="更新时间" width="150">
              <template #default="{ row }">{{ fmtDateTime(row.UpdatedAt) }}</template>
            </el-table-column>
          </el-table>
        </div>
      </div>
    </template>

    <el-empty v-else-if="state === 'live'" description="还没有订阅记录：月费需人工按财务台账录入" />

    <el-dialog v-model="dialog" title="新增订阅" width="560px" :close-on-click-modal="false">
      <el-alert
        v-if="formError"
        class="form-error"
        type="error"
        :closable="false"
        show-icon
        :title="formError"
      />
      <el-form ref="formRef" :model="draft" :rules="rules" label-width="120px" size="small">
        <el-form-item label="供应商" prop="ProviderRef">
          <el-input v-model="draft.ProviderRef" placeholder="网关 provider 名（不是老库渠道名）" />
        </el-form-item>
        <el-form-item label="账号" prop="AccountRef">
          <el-input v-model="draft.AccountRef" placeholder="网关账号 id 或 name" />
        </el-form-item>
        <el-form-item label="账期费用" prop="PriceAmount">
          <el-input-number v-model="draft.PriceAmount" :min="0" :step="100" :precision="2" style="width: 180px" />
          <el-select v-model="draft.PriceCurrency" style="width: 100px; margin-left: 8px">
            <el-option label="CNY" value="CNY" />
            <el-option label="USD" value="USD" />
          </el-select>
        </el-form-item>
        <el-form-item label="周期">
          <el-select v-model="draft.BillingCycle" style="width: 160px">
            <el-option label="月付 month" value="month" />
            <el-option label="年付 year" value="year" />
            <el-option label="一次性 once" value="once" />
          </el-select>
        </el-form-item>
        <el-form-item label="账期日" prop="PayDay">
          <el-input-number v-model="draft.PayDay" :min="1" :max="28" style="width: 140px" />
          <span class="hint">1–28（29–31 会跨月错位）</span>
        </el-form-item>
        <el-form-item label="参与成本分摊">
          <el-switch v-model="draft.ShareCost" />
          <span class="hint">关闭 = 该账号不参与摊销分摊</span>
        </el-form-item>
        <el-form-item label="账期起点">
          <el-date-picker v-model="draft.CycleStart" type="date" value-format="YYYY-MM-DD" placeholder="留空 = 未开始摊销" style="width: 200px" />
        </el-form-item>
        <el-form-item label="备注">
          <el-input v-model="draft.Note" type="textarea" :rows="2" maxlength="200" show-word-limit />
        </el-form-item>
      </el-form>
      <template #footer>
        <span class="footer-hint">录入标准待 Spec 定稿，保存暂不写库</span>
        <el-button size="small" @click="dialog = false">取消</el-button>
        <el-button size="small" type="primary" @click="submit">校验并通过</el-button>
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
.sub {
  font-size: 11px;
  color: var(--color-text-tertiary);
}
.hint {
  margin-left: 8px;
  font-size: 12px;
  color: var(--color-text-tertiary);
}
.form-error {
  margin-bottom: 10px;
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
}
</style>
