<script setup lang="ts">
// 拓扑：只放 DAG。没有统计卡、没有 tab、没有长篇说明。
// 点节点在右侧抽屉里查看/编辑（对外模型：完整售价、别名、时段系数、上下文阶梯都在这）；
// 从模型或路由组右侧句柄拖一条线出来 = 新建一个绑定它的对外模型。
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { getTopology, getGatewayView, syncTopology, type InternalModelView, type GatewayView } from '@/api/v2'
import {
  listExternalModels,
  updateExternalModel,
  createExternalModel,
  batchDeleteExternalModels,
  syncModelsPrices,
  type ExternalModel,
} from '@/api/admin'
import { mockTopology } from '@/mock/topology'
import { useV2Load } from '@/composables/useV2Load'
import { fmtDateTime } from '@/utils/format'
import TopologyCanvas from '@/components/v2/topology/TopologyCanvas.vue'
import ModelPricing from '@/components/ModelPricing.vue'
import ModelPriceCatalog from '@/components/ModelPriceCatalog.vue'
import StringListEditor from '@/components/StringListEditor.vue'

const { state, data, load } = useV2Load<InternalModelView>(getTopology, mockTopology)
const gw = ref<GatewayView | null>(null)
async function loadGateway() {
  try {
    const r = await getGatewayView()
    if (r.state === 'live' && r.data) gw.value = r.data
  } catch {
    gw.value = null
  }
}
onMounted(async () => {
  await Promise.all([load(), loadGateway()])
  await ensureExternals()
})

const syncing = ref(false)
const catalogRef = ref<InstanceType<typeof ModelPriceCatalog> | null>(null)
const syncMsg = ref('')
async function runSync() {
  if (syncing.value) return
  syncing.value = true
  syncMsg.value = ''
  const res = await syncTopology()
  if (res.state === 'live' && res.data) {
    const d = res.data
    syncMsg.value =
      d.State === 'failed' ? `同步失败：${d.Message || '网关未给原因'}（沿用上一轮快照）` : `拉取 ${d.Fetched} · 新增 ${d.Created} · 失效 ${d.Stale}`
    await Promise.all([load(), loadGateway()])
    await ensureExternals()
  } else {
    syncMsg.value = res.state === 'pending' ? '同步接口未上线' : '同步失败'
  }
  syncing.value = false
}

// ---- 对外模型（价格/别名等全字段） ----
const externals = ref<ExternalModel[]>([])
async function ensureExternals(force = false) {
  if (externals.value.length && !force) return
  try {
    const r = await listExternalModels({ limit: 500 })
    const raw = (r as unknown as { Data?: ExternalModel[]; data?: ExternalModel[] })
    externals.value = raw.Data || raw.data || []
  } catch {
    externals.value = []
  }
}
const priceMap = computed(() => {
  const m: Record<string, { Input?: number; Output?: number }> = {}
  for (const e of externals.value) m[e.ID] = { Input: Number(e.SaleRates?.Input ?? 0), Output: Number(e.SaleRates?.Output ?? 0) }
  return m
})

// ---- 抽屉 ----
type Sel = { kind: string; ref: string; data: Record<string, unknown> }
const sel = ref<Sel | null>(null)
const drawer = ref(false)
const saving = ref(false)

const form = reactive({
  id: '',
  name: '',
  desc: '',
  enabled: true,
  aliases: [] as string[],
  bound: '',
  isNew: false,
})
const pricing = reactive<Record<string, unknown>>({ SaleRates: { Input: 0, Output: 0, CacheRead: 0 }, TimeConfig: null, ContextTiers: null })

async function openNode(s: Sel) {
  sel.value = s
  drawer.value = true
  form.isNew = false
  form.bound = ''
  form.aliases = []
  form.desc = ''
  Object.assign(pricing, { SaleRates: { Input: 0, Output: 0, CacheRead: 0 }, TimeConfig: null, ContextTiers: null })
  if (s.kind === 'external') {
    await ensureExternals()
    const m = externals.value.find((x) => x.ID === s.ref)
    form.id = s.ref
    form.name = m?.ExternalName || String(s.data.Name || '')
    form.desc = m?.Description || ''
    form.enabled = m ? m.Enabled : true
    form.aliases = m?.Aliases ? [...m.Aliases] : []
    form.bound = (m?.BoundInternalRef as string) || ''
    Object.assign(pricing, {
      SaleRates: { Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0, ...(m?.SaleRates || {}) },
      TimeConfig: m?.TimeConfig ?? null,
      ContextTiers: m?.ContextTiers ?? null,
    })
  }
}

// 显式「新建对外模型」（从按钮或拖线触发），可预绑定到某个内部模型/组
async function openCreate(bound: string, name?: string) {
  sel.value = { kind: 'model', ref: bound, data: { Name: name || bound } }
  drawer.value = true
  form.isNew = true
  form.id = ''
  form.name = name || (bound ? bound.split('/').pop() + '-售' : '')
  form.desc = ''
  form.enabled = true
  form.aliases = []
  form.bound = bound
  Object.assign(pricing, { SaleRates: { Input: 0, Output: 0, CacheRead: 0 }, TimeConfig: null, ContextTiers: null })
}

function onApplyCatalog(rates: Record<string, number>) {
  const cur = (pricing.SaleRates as Record<string, number>) || {}
  pricing.SaleRates = { ...cur, ...rates }
  ElMessage.success('已套用 models.dev 参考价')
}

async function syncAllPrices() {
  try {
    const r = await syncModelsPrices()
    const d = (r as unknown as { Data?: { Updated?: string[]; Skipped?: string[] } }).Data || {}
    ElMessage.success(`价目同步完成：更新 ${d.Updated?.length || 0}，跳过 ${d.Skipped?.length || 0}`)
    await ensureExternals(true)
  } catch (e) {
    ElMessage.error('同步失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function openConnect(payload: { ref: string }) {
  await openCreate(payload.ref)
}

// 拖线到已有对外模型 = 绑定，不再弹抽屉
async function bindExisting(payload: { ref: string; externalId: string }) {
  await ensureExternals()
  const m = externals.value.find((x) => x.ID === payload.externalId)
  if (!m) return ElMessage.error('找不到目标对外模型')
  try {
    await updateExternalModel(m.ID, {
      ExternalName: m.ExternalName,
      Description: m.Description || '',
      Enabled: m.Enabled,
      SaleRates: m.SaleRates || {},
      TimeConfig: m.TimeConfig ?? null,
      ContextTiers: m.ContextTiers ?? null,
      Aliases: m.Aliases || [],
      BoundInternalRef: payload.ref,
      MagpieModel: payload.ref,
    } as never)
    ElMessage.success(`已绑定到 ${payload.ref}`)
    await ensureExternals(true)
    await load()
  } catch (e) {
    ElMessage.error('绑定失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

// 按网关的组补全对外模型：每个组若还没有对外模型绑定它，就建一个同名的
// （Auto 组加 " (auto)" 后缀），价格 0 可在抽屉里改。
async function syncGroupModels() {
  if (!gw.value) return ElMessage.warning('还没拿到网关的组数据')
  await ensureExternals(true)
  const boundRefs = new Set(externals.value.map((e) => (e.BoundInternalRef || '')).filter(Boolean))
  const todo = gw.value.groups.filter((g) => !boundRefs.has(g.ref))
  if (!todo.length) return ElMessage.info('每个组都已有对外模型')
  let ok = 0
  for (const g of todo) {
    try {
      await createExternalModel({
        ExternalName: g.auto ? `${g.name} (auto)` : g.name,
        Description: '',
        Enabled: true,
        SaleRates: { Input: 0, Output: 0, CacheRead: 0 },
        TimeConfig: null,
        ContextTiers: null,
        Aliases: [],
        BoundInternalRef: g.ref,
        MagpieModel: g.ref,
      } as never)
      ok++
    } catch {
      /* 跳过失败的，最后一起提示 */
    }
  }
  await ensureExternals(true)
  await load()
  ElMessage.success(`已补全 ${ok} / ${todo.length} 个组的对外模型`)
}

async function removeModel() {
  if (!form.id) return
  try {
    await ElMessageBox.confirm(`删除对外模型「${form.name}」？此操作不可撤销。`, '删除确认', { type: 'warning' })
  } catch {
    return
  }
  try {
    await batchDeleteExternalModels([form.id])
    ElMessage.success('已删除')
    await ensureExternals(true)
    await load()
    drawer.value = false
  } catch (e) {
    ElMessage.error('删除失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function save() {
  if (!form.name.trim()) return ElMessage.warning('名称不能为空')
  saving.value = true
  try {
    const base = {
      ExternalName: form.name.trim(),
      Description: form.desc || '',
      Enabled: form.enabled,
      SaleRates: (pricing.SaleRates as Record<string, number>) || {},
      TimeConfig: pricing.TimeConfig ?? null,
      ContextTiers: pricing.ContextTiers ?? null,
      Aliases: form.aliases,
      BoundInternalRef: form.bound || null,
      MagpieModel: form.bound || null,
    }
    if (form.isNew) {
      await createExternalModel(base as never)
      ElMessage.success('已新建对外模型')
    } else {
      await updateExternalModel(form.id, base as never)
      ElMessage.success('已保存')
    }
    await ensureExternals(true)
    await load()
    drawer.value = false
  } catch (e) {
    ElMessage.error('保存失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    saving.value = false
  }
}

const lastSync = computed(() => (data.value?.SyncedAt ? fmtDateTime(data.value.SyncedAt) : '—'))
const selKindLabel = computed(() => {
  const m: Record<string, string> = { account: '账号', provider: '供应商', model: '模型', group: '路由组', external: '对外模型' }
  return m[sel.value?.kind || ''] || '节点'
})
</script>

<template>
  <div class="tp">
    <div class="bar">
      <span class="brand"><i></i>拓扑</span>
      <span class="tip">账号 → 供应商 → 路由组 / AUTO → 对外模型</span>
      <span class="sp" />
      <span class="live"><em>最后同步</em><b>{{ lastSync }}</b></span>
      <span v-if="syncMsg" class="tip">{{ syncMsg }}</span>
      <button @click="syncGroupModels">补全对外模型</button>
      <button @click="syncAllPrices">同步价目</button>
      <button class="pri" @click="openCreate('')">新建模型</button>
      <button @click="runSync">同步</button>
      <button @click="load()">刷新</button>
    </div>

    <div v-if="state === 'error'" class="tp-err">数据没取到，先看错误再刷新。</div>
    <div v-else class="tp-canvas">
      <TopologyCanvas v-if="data" :view="data" :gw="gw" :prices="priceMap" @select="openNode" @connect="openConnect" @bind="bindExisting" />
      <div v-else class="tp-empty">还没有拓扑数据</div>
    </div>

    <el-drawer v-model="drawer" :title="selKindLabel" size="560px" direction="rtl">
      <template v-if="sel">
        <div v-if="form.isNew || sel.kind === 'external'">
          <el-form label-position="top" size="small">
            <el-form-item label="对外名称">
              <el-input v-model="form.name" />
            </el-form-item>
            <el-form-item v-if="form.bound" label="绑定到（内部模型）">
              <el-input v-model="form.bound" disabled />
            </el-form-item>
            <el-form-item label="说明">
              <el-input v-model="form.desc" type="textarea" :rows="2" />
            </el-form-item>
            <el-form-item label="启用">
              <el-switch v-model="form.enabled" />
            </el-form-item>

            <ModelPricing v-model="pricing" show-rates-key="SaleRates" title="售价">
              <template #title-extra>
                <el-button size="small" @click="catalogRef?.open()">从 models.dev 获取参考价</el-button>
              </template>
            </ModelPricing>
            <el-form-item label="别名（命中任一名即路由到本模型）">
              <StringListEditor v-model="form.aliases" placeholder="每行一个别名，如 gpt-5.2" />
            </el-form-item>
          </el-form>
          <div class="drawer-actions">
            <el-button type="primary" :loading="saving" @click="save">
              {{ form.isNew ? '新建对外模型' : '保存' }}
            </el-button>
            <el-button v-if="!form.isNew && sel.kind === 'external'" type="danger" plain @click="removeModel">删除</el-button>
          </div>
        </div>
        <div v-else class="ro">
          <div class="ro-row"><span>名称</span><b>{{ sel.data.Name || sel.data.Account || sel.ref }}</b></div>
          <div v-if="sel.data.Provider" class="ro-row"><span>供应商</span><b>{{ sel.data.Provider }}</b></div>
          <div v-if="sel.kind === 'group'" class="ro-row"><span>成员</span><b>{{ sel.data.Members }}</b></div>
          <div v-if="sel.kind === 'account'" class="ro-row"><span>类型</span><b>{{ sel.data.Plan || '按量' }}</b></div>
          <p class="ro-hint">只读：这一层由网关同步过来。</p>
          <el-button v-if="sel.kind === 'model' || sel.kind === 'group'" type="primary" style="width:100%;margin-top:12px"
            @click="openCreate(sel.ref, String(sel.data.Name || ''))">新建对外模型并绑定它</el-button>
        </div>
      </template>
      <ModelPriceCatalog ref="catalogRef" :model-name="form.name" label="售价" @apply="onApplyCatalog" />
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
.bar button { height: 26px; padding: 0 10px; border-radius: 7px; border: 1px solid #28334a; background: #16203a; color: var(--ink); font: inherit; font-size: 11.5px; cursor: pointer; }
.bar button:hover { background: #1d2a48; }
.bar button.pri { border-color: #3a56b8; background: linear-gradient(180deg, #3f5fd4, #3450bd); }
.meta { font-size: 11px; color: var(--ink-3); }
.sp { flex: 1; }
.tp-canvas { flex: 1; min-height: 0; border: 1px solid var(--line); border-radius: 10px; overflow: hidden; }
.tp-empty, .tp-err { flex: 1; display: flex; align-items: center; justify-content: center; color: var(--ink-3); font-size: 12px; }
.ro-row { display: flex; justify-content: space-between; padding: 7px 0; border-bottom: 1px solid var(--line); font-size: 12.5px; }
.ro-row span { color: var(--ink-3); }
.drawer-actions { display: flex; gap: 8px; }
.drawer-actions .el-button:first-child { flex: 1; }
.ro-hint { color: var(--ink-3); font-size: 11px; margin-top: 12px; }
.sec { font-size: 11px; color: var(--ink-3); margin: 10px 0 6px; letter-spacing: .5px; }
.rates { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; margin-bottom: 12px; }
.rate { display: flex; flex-direction: column; gap: 3px; font-size: 11px; color: var(--ink-3); }
</style>
