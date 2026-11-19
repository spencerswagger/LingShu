<script setup lang="ts">
// 拓扑（V3 只读）。唯一可写：对外模型「售价」、对内模型「成本价」。
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { getTopology, getGatewayView, syncTopology, type InternalModelView, type GatewayView } from '@/api/v2'
import { listExternalModels, updateExternalModel, createExternalModel, updateInternalCostRates, type ExternalModel } from '@/api/admin'
import { mockTopology } from '@/mock/topology'
import { useV2Load } from '@/composables/useV2Load'
import { fmtDateTime } from '@/utils/format'
import TopologyCanvas from '@/components/v2/topology/TopologyCanvas.vue'
import ModelPricing from '@/components/ModelPricing.vue'

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
    await ensureExternals(true); drawer.value = false
  } catch (e) { ElMessage.error('保存失败：' + (e instanceof Error ? e.message : String(e))) } finally { saving.value = false }
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

    <el-drawer v-model="drawer" :title="selKindLabel" size="560px" direction="rtl">
      <template v-if="sel">
        <div v-if="sel.kind === 'group'">
          <div class="ro-row"><span>路由组</span><b>{{ sel.data.Name }}</b></div>
          <ModelPricing v-model="pricing" show-rates-key="SaleRates" title="售价" />
          <el-button type="primary" :loading="saving" style="width:100%" @click="savePrice">保存售价</el-button>
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
    </el-drawer>
  </div>
</template>

<style scoped>
.tp { display: flex; flex-direction: column; height: calc(100vh - 92px); }
.bar { display: flex; align-items: center; gap: 10px; padding: 9px 14px; border: 1px solid var(--line); border-radius: 10px; margin-bottom: 10px; font-size: 12px; color: var(--ink-3); }
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
</style>
