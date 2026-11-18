<script setup lang="ts">
// 拓扑：只放 DAG。没有统计卡、没有 tab、没有长篇说明。
// 点节点在右侧抽屉里查看/编辑（对外模型：完整售价、别名、时段系数、上下文阶梯都在这）；
// 从模型或路由组右侧句柄拖一条线出来 = 新建一个绑定它的对外模型。
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { getTopology, syncTopology, type InternalModelView } from '@/api/v2'
import {
  listExternalModels,
  updateExternalModel,
  createExternalModel,
  type ExternalModel,
} from '@/api/admin'
import { mockTopology } from '@/mock/topology'
import { useV2Load } from '@/composables/useV2Load'
import { fmtDateTime } from '@/utils/format'
import TopologyCanvas from '@/components/v2/topology/TopologyCanvas.vue'
import ModelPricing from '@/components/ModelPricing.vue'
import StringListEditor from '@/components/StringListEditor.vue'

const { state, data, load } = useV2Load<InternalModelView>(getTopology, mockTopology)
onMounted(async () => {
  await load()
  await ensureExternals()
})

const syncing = ref(false)
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
    await load()
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
  } else if (s.kind === 'model' || s.kind === 'group') {
    form.isNew = true
    form.id = ''
    form.name = String((s.data as Record<string, unknown>).Name || s.ref) + '-售'
    form.enabled = true
    form.bound = s.ref
  }
}

async function openConnect(payload: { ref: string }) {
  const item = data.value?.Items.find((m) => m.Ref === payload.ref)
  await openNode({ kind: item?.Kind === 'group' ? 'group' : 'model', ref: payload.ref, data: { Name: item?.DisplayName || payload.ref } })
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
    } as never)
    ElMessage.success(`已绑定到 ${payload.ref}`)
    await ensureExternals(true)
    await load()
  } catch (e) {
    ElMessage.error('绑定失败：' + (e instanceof Error ? e.message : String(e)))
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
    <div class="tp-bar">
      <h2>拓扑</h2>
      <span class="meta">最后同步 {{ lastSync }}</span>
      <span v-if="syncMsg" class="meta">{{ syncMsg }}</span>
      <div class="sp" />
      <el-button size="small" type="primary" @click="openNode({ kind: 'model', ref: '', data: { Name: '' } })">新建模型</el-button>
      <el-button size="small" :loading="syncing" @click="runSync">同步</el-button>
      <el-button size="small" :loading="state === 'loading'" @click="load()">刷新</el-button>
    </div>

    <div v-if="state === 'error'" class="tp-err">数据没取到，先看错误再刷新。</div>
    <div v-else class="tp-canvas">
      <TopologyCanvas v-if="data" :view="data" :prices="priceMap" @select="openNode" @connect="openConnect" @bind="bindExisting" />
      <div v-else class="tp-empty">还没有拓扑数据</div>
    </div>

    <el-drawer v-model="drawer" :title="selKindLabel" size="560px" direction="rtl">
      <template v-if="sel">
        <div v-if="sel.kind === 'external' || sel.kind === 'model' || sel.kind === 'group'">
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

            <ModelPricing v-model="pricing" show-rates-key="SaleRates" title="售价" />
            <el-form-item label="别名（命中任一名即路由到本模型）">
              <StringListEditor v-model="form.aliases" placeholder="每行一个别名，如 gpt-5.2" />
            </el-form-item>
          </el-form>
          <el-button type="primary" :loading="saving" style="width: 100%" @click="save">
            {{ form.isNew ? '新建对外模型' : '保存' }}
          </el-button>
        </div>
        <div v-else class="ro">
          <div class="ro-row"><span>名称</span><b>{{ sel.data.Account || sel.data.Ref || sel.ref }}</b></div>
          <div v-if="sel.data.Provider" class="ro-row"><span>供应商</span><b>{{ sel.data.Provider }}</b></div>
          <div v-if="sel.kind === 'account'" class="ro-row"><span>类型</span><b>{{ sel.data.Subscribed ? '订阅' : '按量' }}</b></div>
          <div v-if="sel.kind === 'provider'" class="ro-row"><span>账号数</span><b>{{ sel.data.Accounts }}</b></div>
          <p class="ro-hint">只读：这一层由网关同步过来。</p>
        </div>
      </template>
    </el-drawer>
  </div>
</template>

<style scoped>
.tp { display: flex; flex-direction: column; height: calc(100vh - 92px); }
.tp-bar { display: flex; align-items: center; gap: 10px; padding: 0 0 10px; }
.tp-bar h2 { font-size: 15px; margin: 0; }
.meta { font-size: 11px; color: var(--ink-3); }
.sp { flex: 1; }
.tp-canvas { flex: 1; min-height: 0; border: 1px solid var(--line); border-radius: 10px; overflow: hidden; }
.tp-empty, .tp-err { flex: 1; display: flex; align-items: center; justify-content: center; color: var(--ink-3); font-size: 12px; }
.ro-row { display: flex; justify-content: space-between; padding: 7px 0; border-bottom: 1px solid var(--line); font-size: 12.5px; }
.ro-row span { color: var(--ink-3); }
.ro-hint { color: var(--ink-3); font-size: 11px; margin-top: 12px; }
.sec { font-size: 11px; color: var(--ink-3); margin: 10px 0 6px; letter-spacing: .5px; }
.rates { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; margin-bottom: 12px; }
.rate { display: flex; flex-direction: column; gap: 3px; font-size: 11px; color: var(--ink-3); }
</style>
