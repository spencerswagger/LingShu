<script setup lang="ts">
// 拓扑（V3 只读）。唯一可写：对外模型「售价」、对内模型「成本价」。
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { getTopology, getGatewayView, syncTopology, type InternalModelView, type GatewayView, type GwGroup } from '@/api/v2'
import {
  listExternalModels, updateExternalModel, createExternalModel, updateInternalCostRates,
  listBillings, rebillUnpriced, type ExternalModel, type AdminBillingItem, type RebillResult,
} from '@/api/admin'
import type { ApiError } from '@/api/http'
import { mockTopology } from '@/mock/topology'
import { useV2Load } from '@/composables/useV2Load'
import { fmtDateTime } from '@/utils/format'
import TopologyCanvas from '@/components/v2/topology/TopologyCanvas.vue'
import ModelPricing from '@/components/ModelPricing.vue'
import ModelPriceCatalog from '@/components/ModelPriceCatalog.vue'

const { state, data, load } = useV2Load<InternalModelView>(getTopology, mockTopology)
const gw = ref<GatewayView | null>(null)
async function loadGateway() {
  try {
    const r = await getGatewayView()
    if (r.state === 'live' && r.data) gw.value = r.data
  } catch { gw.value = null }
}
onMounted(async () => { await Promise.all([load(), loadGateway()]); await ensureExternals() })

const syncing = ref(false)
const catalogRef = ref<InstanceType<typeof ModelPriceCatalog> | null>(null)
const syncMsg = ref('')
async function runSync() {
  if (syncing.value) return
  syncing.value = true; syncMsg.value = ''
  const res = await syncTopology()
  if (res.state === 'live' && res.data) {
    const d = res.data
    syncMsg.value = d.State === 'failed' ? `同步失败：${d.Message || '网关未给原因'}（沿用上一轮快照）` : `拉取 ${d.Fetched} · 新增 ${d.Created} · 失效 ${d.Stale}`
    await Promise.all([load(), loadGateway()]); await ensureExternals()
  } else syncMsg.value = res.state === 'pending' ? '同步接口未上线' : '同步失败'
  syncing.value = false
}

const externals = ref<ExternalModel[]>([])
async function ensureExternals(force = false) {
  if (externals.value.length && !force) return
  try {
    const r = await listExternalModels({ limit: 500 })
    const raw = (r as unknown as { Data?: ExternalModel[]; data?: ExternalModel[] })
    externals.value = raw.Data || raw.data || []
  } catch { externals.value = [] }
}
// 组 ref → 售价（外部模型 BoundInternalRef == 组 ref；null = 未定价）
const groupPrice = computed(() => {
  const m: Record<string, { In: number; Out: number } | null> = {}
  for (const g of gw.value?.groups || []) {
    const e = externals.value.find((x) => (x.BoundInternalRef || '') === g.ref)
    m[g.ref] = e ? { In: Number(e.SaleRates?.Input ?? 0), Out: Number(e.SaleRates?.Output ?? 0) } : null
  }
  return m
})

type Sel = { kind: string; ref: string; data: Record<string, unknown> }
const sel = ref<Sel | null>(null)
const drawer = ref(false)
const saving = ref(false)
const pricing = reactive<Record<string, unknown>>({ SaleRates: { Input: 0, Output: 0, CacheRead: 0 }, TimeConfig: null, ContextTiers: null })
const costRates = reactive<Record<string, number>>({ input: 0, output: 0, cache_read: 0 })
const costName = ref('')

async function openNode(s: Sel) {
  sel.value = s; drawer.value = true
  // 换节点先清补收状态，别把上一组的结果/报错带过来
  unpricedCount.value = 0; rebillDone.value = null; rebillError.value = ''; checkToken++
  if (s.kind === 'group') {
    await ensureExternals()
    const m = externals.value.find((x) => (x.BoundInternalRef || '') === s.ref)
    const rates = (m?.SaleRates || {}) as Record<string, number>
    Object.assign(pricing, {
      SaleRates: { Input: rates.Input ?? 0, Output: rates.Output ?? 0, CacheRead: rates.CacheRead ?? 0 },
      TimeConfig: m?.TimeConfig ?? null,
      ContextTiers: m?.ContextTiers ?? null,
    })
  } else if (s.kind === 'model') {
    const ref = String(s.ref || '')
    costName.value = String(s.data?.Name || ref)
    // 成本价读自后端视图的 CostRates（如未下发则显示 0）
    const cr = (s.data?.CostRates || {}) as Record<string, number>
    costRates.input = Number(cr.input ?? 0); costRates.output = Number(cr.output ?? 0); costRates.cache_read = Number(cr.cache_read ?? 0)
  }
}

async function savePrice() {
  if (!sel.value || sel.value.kind !== 'group') return
  saving.value = true
  try {
    const m = externals.value.find((x) => (x.BoundInternalRef || '') === sel.value!.ref)
    if (m) {
      await updateExternalModel(m.ID, {
      ExternalName: m.ExternalName,
      Description: m.Description || '',
      Enabled: m.Enabled,
      SaleRates: pricing.SaleRates as Record<string, number>,
      TimeConfig: (pricing.TimeConfig as never) ?? null,
      ContextTiers: (pricing.ContextTiers as never) ?? null,
        Aliases: m.Aliases || [],
        BoundInternalRef: sel.value.ref,
        MagpieModel: sel.value.ref,
      } as never)
    } else {
      // 组还没定价：保存即创建一个绑定到该组的外部模型（名称=组名）
      await createExternalModel({
        ExternalName: String(sel.value.data.Name || sel.value.ref),
        Description: '',
        Enabled: true,
        SaleRates: pricing.SaleRates as Record<string, number>,
        TimeConfig: (pricing.TimeConfig as never) ?? null,
        ContextTiers: (pricing.ContextTiers as never) ?? null,
        Aliases: [],
        BoundInternalRef: sel.value.ref,
        MagpieModel: sel.value.ref,
      } as never)
    }
    ElMessage.success('售价已保存')
    await ensureExternals(true)
    // 保存成功 → 检测该组历史 unpriced 账单：有则**留在抽屉里**引导补收，没有才按原行为关掉
    // （spencer 拍板的 B 方案交互顺序：先保存售价，再问要不要补收历史账）
    const n = await checkUnpricedAfterSave()
    if (n === 0) drawer.value = false
  } catch (e) { ElMessage.error('保存失败：' + (e instanceof Error ? e.message : String(e))) } finally { saving.value = false }
}

// ===== unpriced 账单补收 =====
//
// 检测方式（不改后端，全部用现成接口）：GET /admin/billings?Status=unpriced&Model=<组名形态>&Group=0
// 取 Total。两处口径缺口如实接受（补收接口按同一套候选链匹配，前端多检测只是提示数偏宽，
// 实际补收条数以返回的 Rebilled 为准）：
//   1. Model 是 external_model_name/auto_model_name 精确匹配，而补收扫描还要匹配组快照
//      display_name ↔ ref 双向 —— 快照缺失时前端数不到，补收却能算到；
//   2. 已补收的行 status 仍是 unpriced（原行只追加 error_message 标记）→ Size=200 拉一页
//      用行内 ErrorMessage 是否含「已补收:」剔除，超过 200 条不再往下翻（手动触发场景可接受）。
const unpricedCount = ref(0)
const unpricedChecking = ref(false)
let checkToken = 0 // 组切换竞态守卫：慢响应回来时别覆盖新组的检测结果

// groupNameForms 派生该组可能的全部名字形态（对齐后端 groupRefForms 的保守近似）。
// groupNames 拿不到时退回 ref 与去前缀组 id 两档（ref/id 与 auto slug 归一仍是主命中档）。
function groupNameForms(g: GwGroup): string[] {
  const out: string[] = []
  const add = (v: string) => { if (v && !out.includes(v)) out.push(v) }
  add(g.ref)
  add(g.ref.replace(/^group\//, ''))
  const m = /^auto\/(.+)$/.exec(g.ref)
  if (m) {
    add('group/auto-' + m[1])
    // magpie slug 近似：小写，[a-z0-9.-] 之外的字符替换为 '-'（与后端 groupSlug 同规则）
    add('group/auto-' + m[1].toLowerCase().replace(/[^a-z0-9.-]/g, '-'))
    add(m[1])
  }
  add(g.name)
  add(g.id)
  return out
}

async function countUnpricedFor(refStr: string): Promise<number> {
  const g = gw.value?.groups.find((x) => x.ref === refStr)
  const forms = g ? groupNameForms(g) : [refStr, refStr.replace(/^group\//, '')]
  // 同一行会在多个名字形态下重复出现（ref / 组 id / display_name 指向同一批账单），按 BillingID 去重
  const seen = new Set<string>()
  let n = 0
  for (const name of forms) {
    const res = await listBillings({ Status: 'unpriced', Model: name, Page: 1, Size: 200, Group: 0 }, '/admin')
    const rows: AdminBillingItem[] = res.Data?.List || []
    for (const r of rows) {
      if (seen.has(r.BillingID)) continue
      seen.add(r.BillingID)
      // Total 含已补收行（补收不改 status，只在 ErrorMessage 追加标记），必须逐行剔除
      if ((r.ErrorMessage || '').includes('已补收:')) continue
      n++
    }
    if (rows.length >= 200) break // 该形态已超一页，手动触发场景不再深翻
  }
  return n
}

async function checkUnpricedAfterSave(): Promise<number> {
  const refStr = sel.value?.ref || ''
  if (!refStr) return 0
  unpricedCount.value = 0
  rebillDone.value = null // 新检测周期作废上一轮结果/报错（同一抽屉里连续保存的场景）
  rebillError.value = ''
  unpricedChecking.value = true
  const token = ++checkToken
  try {
    const n = await countUnpricedFor(refStr)
    if (token === checkToken && sel.value?.ref === refStr) unpricedCount.value = n
    return unpricedCount.value
  } catch { unpricedCount.value = 0; return 0 } finally {
    if (token === checkToken) unpricedChecking.value = false
  }
}

const rebilling = ref(false)
// 补收成功后的展示数据（条数/金额/新单号，全部来自后端返回）
const rebillDone = ref<RebillResult | null>(null)
// 后端拒绝原因（Message 中文文案 + Reason 机器键，两者都展示）
const rebillError = ref('')

const REBILL_REASON_TEXT: Record<string, string> = {
  rebill_group_unpriced: '该组当前没有可用的售价口径，请先绑定售价后再补收',
  // 行级真键——取值以 rebill_handler.go 的常量为准（Owl P1：rebill.go 里 RebillRowFailure
  // 的注释键是错的，前端曾照抄导致映射全死；键值只认 handler 实码）。
  // 触发点见 rebill.go：row_unresolvable=解析不到售价/缺 user_id(357/362)、
  // no_wallet=用户无钱包(341)、broken_row=扫描层毒行(187)、record_error=计费落库(423)、
  // already_rebilled=幂等撞号 23505(421)。
  rebill_row_unresolvable: '按当前配置仍解析不到该行售价或用户归属，未补收',
  rebill_no_wallet: '该行用户没有积分钱包，无法扣款（开通钱包后可重放补齐）',
  rebill_broken_row: '该行原始数据损坏，无法计价，未补收',
  rebill_record_error: '计费或落库出错，未补收',
  rebill_already_rebilled: '该行此前已被补收过，本次不重复补收',
  // 兜底：后端未来新增键时展示原文 Message，不吞掉
}

async function runRebill() {
  if (!sel.value || rebilling.value) return
  const refStr = sel.value.ref
  const n = unpricedCount.value
  try {
    await ElMessageBox.confirm(
      // Owl P3：N 是前端预估，弹窗必须写明「预计」，实际条数与金额以后端返回为准（防授权歧义）
      `将为「${String(sel.value.data.Name || refStr)}」按当前售价预计补收 ${n} 条，实际条数与金额以后端返回为准；补收将从对应用户余额扣款，原有 unpriced 账单保持不变。是否继续？`,
      '按当前售价补收',
      { confirmButtonText: '确认补收', cancelButtonText: '取消', type: 'warning' },
    )
  } catch { return } // 用户取消
  rebilling.value = true
  rebillDone.value = null
  rebillError.value = ''
  try {
    const res = await rebillUnpriced(refStr)
    rebillDone.value = res.Data
    unpricedCount.value = 0
    ElMessage.success(`补收完成：${res.Data?.Rebilled ?? 0} 条`)
  } catch (e) {
    const err = e as ApiError
    const byReason = err.reason ? REBILL_REASON_TEXT[err.reason] : ''
    rebillError.value = byReason ? `${byReason}（${err.message}）` : (err.message || '补收失败')
  } finally { rebilling.value = false }
}

// models.dev 参考价套用到售价表单（只覆盖目录给出的价段，其余保留原值）
function onApplyCatalog(rates: Record<string, number>) {
  const cur = (pricing.SaleRates as Record<string, number>) || {}
  pricing.SaleRates = { ...cur, ...rates }
  ElMessage.success('已套用 models.dev 参考价')
}

async function saveCost() {
  if (!sel.value) return
  saving.value = true
  try {
    await updateInternalCostRates(sel.value.ref, { input: costRates.input, output: costRates.output, cache_read: costRates.cache_read })
    ElMessage.success('成本价已保存')
    drawer.value = false
  } catch (e) { ElMessage.error('保存失败：' + (e instanceof Error ? e.message : String(e))) } finally { saving.value = false }
}

const lastSync = computed(() => (data.value?.SyncedAt ? fmtDateTime(data.value.SyncedAt) : '—'))
// 抽屉宽度：窄屏占满（560px 固定宽会横向溢出 390px 视口），桌面固定 560px
const drawerSize = typeof window !== 'undefined' && window.innerWidth < 640 ? '100%' : '560px'
const selKindLabel = computed(() => {
  const m: Record<string, string> = { account: '账号', provider: '供应商', model: '对内模型', group: '路由组（售价）' }
  return m[sel.value?.kind || ''] || '节点'
})
</script>

<template>
  <div class="tp">
    <div class="bar">
      <span class="brand"><i></i>拓扑</span>
      <span class="tip">账号 → 供应商 → 路由组 / AUTO → 对外模型（只读）</span>
      <span class="sp" />
      <span class="live"><em>最后同步</em><b>{{ lastSync }}</b></span>
      <span v-if="syncMsg" class="tip">{{ syncMsg }}</span>
      <button @click="runSync">同步</button>
      <button @click="load()">刷新</button>
    </div>

    <div v-if="state === 'error'" class="tp-err">数据没取到，先看错误再刷新。</div>
    <div v-else class="tp-canvas">
      <TopologyCanvas v-if="data && gw" :view="data" :gw="gw" :group-price="groupPrice" @select="openNode" />
      <div v-else class="tp-empty">还没有拓扑数据</div>
    </div>

    <el-drawer v-model="drawer" :title="selKindLabel" :size="drawerSize" direction="rtl">
      <template v-if="sel">
        <div v-if="sel.kind === 'group'">
          <div class="ro-row"><span>路由组</span><b>{{ sel.data.Name }}</b></div>
          <ModelPricing v-model="pricing" show-rates-key="SaleRates" title="售价">
            <template #title-extra>
              <el-button size="small" @click="catalogRef?.open()">从 models.dev 获取参考价</el-button>
            </template>
          </ModelPricing>
          <el-button type="primary" :loading="saving" style="width:100%" @click="savePrice">保存售价</el-button>

          <!-- unpriced 补收（保存成功且检测到 N 条才出现；条数来自账单列表统计，金额/条数结果以后端返回为准） -->
          <div v-if="unpricedChecking" class="rebill">
            <span class="rebill-hint">正在检测历史未定价账单…</span>
          </div>
          <div v-else-if="rebillDone" class="rebill">
            <div class="rebill-title">✓ 补收完成</div>
            <div class="rebill-row"><span>补收条数</span><b>{{ rebillDone.Rebilled }}</b></div>
            <div class="rebill-row"><span>金额合计</span><b>{{ rebillDone.TotalCredits }} 积分</b></div>
            <div v-if="rebillDone.AlreadyMarked > 0" class="rebill-row"><span>此前已补收（跳过）</span><b>{{ rebillDone.AlreadyMarked }} 条</b></div>
            <div class="rebill-ids">
              <span>新账单</span>
              <code v-for="id in rebillDone.NewBillingIDs" :key="id">{{ id }}</code>
              <span v-if="!rebillDone.NewBillingIDs?.length" class="rebill-none">无（可能全部已被补收过）</span>
            </div>
            <!-- 部分失败（毒行不堵整批）：失败行与成功条数并列展示，防止运营误判全部成功 -->
            <div v-if="rebillDone.Failed?.length" class="rebill-failed">
              <div class="rebill-failed-title">{{ rebillDone.Failed.length }} 条未能补收</div>
              <div v-for="f in rebillDone.Failed" :key="f.BillingID" class="rebill-fail-row">
                <span class="rebill-fail-id">{{ f.BillingID }}</span>
                <span class="rebill-fail-txt">{{ REBILL_REASON_TEXT[f.Reason] || f.Message }}</span>
              </div>
              <div v-for="f in rebillDone.Failed.filter((x) => REBILL_REASON_TEXT[x.Reason])" :key="'m' + f.BillingID" class="rebill-fail-msg">后端说明：{{ f.Message }}</div>
            </div>
          </div>
          <div v-else-if="rebillError" class="rebill">
            <div class="rebill-title rebill-fail">补收失败</div>
            <p class="rebill-note">{{ rebillError }}</p>
          </div>
          <div v-else-if="unpricedCount > 0" class="rebill">
            <div class="rebill-title">检测到 {{ unpricedCount }} 条未定价账单</div>
            <p class="rebill-note">按当前售价为这批历史账单补开新账单，将从对应用户余额扣款；原有账单保持不变。</p>
            <el-button type="warning" :loading="rebilling" style="width:100%" @click="runRebill">
              检测到 {{ unpricedCount }} 条未定价账单，按当前售价补收？
            </el-button>
          </div>
        </div>
        <div v-else-if="sel.kind === 'model' || sel.kind === 'group'">
          <div class="ro-row"><span>对内模型</span><b>{{ costName }}</b></div>
          <div class="sec">成本价（成本口径，用于算毛利）</div>
          <div class="rates">
            <label class="rate"><span>input</span><el-input-number v-model="costRates.input" :min="0" :step="0.01" /></label>
            <label class="rate"><span>output</span><el-input-number v-model="costRates.output" :min="0" :step="0.01" /></label>
            <label class="rate"><span>cache_read</span><el-input-number v-model="costRates.cache_read" :min="0" :step="0.01" /></label>
          </div>
          <el-button type="primary" :loading="saving" style="width:100%" @click="saveCost">保存成本价</el-button>
        </div>
        <div v-else class="ro">
          <div class="ro-row"><span>名称</span><b>{{ sel.data.Name || sel.data.Account || sel.ref }}</b></div>
          <p class="ro-hint">只读：这一层由网关同步。</p>
        </div>
      </template>
      <ModelPriceCatalog ref="catalogRef" :model-name="String(sel?.data?.Name || sel?.ref || '')" label="售价" @apply="onApplyCatalog" />
    </el-drawer>
  </div>
</template>

<style scoped>
.tp { display: flex; flex-direction: column; height: calc(100vh - 92px); }
.bar { display: flex; align-items: center; gap: 10px; padding: 9px 14px; border: 1px solid var(--line); border-radius: 10px; margin-bottom: 10px; font-size: 12px; color: var(--ink-3); flex-wrap: wrap; min-width: 0; }
.bar .tip { white-space: normal; }
.brand { display: flex; align-items: center; gap: 8px; color: var(--ink); font-weight: 600; font-size: 13px; white-space: nowrap; }
.brand i { width: 20px; height: 20px; border-radius: 6px; border: 1px solid var(--line-2); position: relative; background: linear-gradient(150deg, #18213a, transparent); }
.brand i:after { content: ''; position: absolute; left: 50%; top: 50%; width: 3px; height: 3px; margin: -1.5px; border-radius: 50%; background: var(--acc-2); box-shadow: 0 0 6px #a78bfaa6; }
.tip { font-size: 11.5px; padding: 3px 9px; border-radius: 11px; border: 1px solid var(--line); white-space: nowrap; }
.live { display: flex; align-items: baseline; gap: 6px; }
.live em { font-style: normal; font-size: 10px; letter-spacing: .6px; color: #4d5566; }
.live b { font-family: ui-monospace, Menlo, monospace; font-size: 11.5px; color: #d3dcf0; }
.sp { flex: 1; }
.bar button { height: 26px; padding: 0 10px; border-radius: 7px; border: 1px solid #28334a; background: #16203a; color: var(--ink); font: inherit; font-size: 11.5px; cursor: pointer; }
.tp-canvas { flex: 1; min-height: 0; border: 1px solid var(--line); border-radius: 10px; overflow: hidden; }
.tp-empty, .tp-err { flex: 1; display: flex; align-items: center; justify-content: center; color: var(--ink-3); font-size: 12px; }
.ro-row { display: flex; justify-content: space-between; padding: 7px 0; border-bottom: 1px solid var(--line); font-size: 12.5px; }
.ro-row span { color: var(--ink-3); }
.ro-hint { color: var(--ink-3); font-size: 11px; margin-top: 12px; }
.sec { font-size: 11px; color: var(--ink-3); margin: 10px 0 6px; letter-spacing: .5px; }
.rates { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; margin-bottom: 12px; }
.rate { display: flex; flex-direction: column; gap: 3px; font-size: 11px; color: var(--ink-3); }
.rebill { margin-top: 12px; padding: 10px 12px; border: 1px solid var(--line); border-radius: 9px; background: rgba(255, 176, 32, .05); }
.rebill-title { font-size: 12.5px; font-weight: 600; color: #f3b04a; margin-bottom: 4px; }
.rebill-title.rebill-fail { color: #f56c6c; }
.rebill-hint { font-size: 11.5px; color: var(--ink-3); }
.rebill-note { font-size: 11.5px; color: var(--ink-3); margin: 4px 0 10px; line-height: 1.6; }
.rebill-row { display: flex; justify-content: space-between; font-size: 12px; padding: 3px 0; }
.rebill-row span { color: var(--ink-3); }
.rebill-row b { font-family: ui-monospace, Menlo, monospace; color: var(--ink); }
.rebill-ids { display: flex; flex-wrap: wrap; gap: 4px; margin-top: 6px; font-size: 10.5px; color: var(--ink-3); align-items: center; }
.rebill-ids code { font-family: ui-monospace, Menlo, monospace; padding: 1px 6px; border: 1px solid var(--line); border-radius: 5px; color: var(--ink); word-break: break-all; }
.rebill-none { color: var(--ink-3); }
/* 部分失败明细：红色调沿用 Element Plus danger，与失败块 .rebill-fail 同源 */
.rebill-failed { margin-top: 8px; padding-top: 8px; border-top: 1px dashed var(--line); }
.rebill-failed-title { font-size: 12px; font-weight: 600; color: #f56c6c; margin-bottom: 4px; }
.rebill-fail-row { display: flex; gap: 8px; align-items: baseline; font-size: 11.5px; padding: 2px 0; min-width: 0; }
.rebill-fail-id { font-family: ui-monospace, Menlo, monospace; color: var(--ink); flex-shrink: 0; word-break: break-all; }
.rebill-fail-txt { color: #f56c6c; line-height: 1.5; }
.rebill-fail-msg { font-size: 11px; color: var(--ink-3); line-height: 1.5; word-break: break-all; }
</style>
