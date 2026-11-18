<script setup lang="ts">
// 拓扑：只放 DAG。没有统计卡、没有 tab、没有长篇说明。
// 节点选中在右侧抽屉里看/改；从模型或路由组右侧句柄拖一条线出来 = 新建一个绑定它的对外模型。
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

const { state, data, load } = useV2Load<InternalModelView>(getTopology, mockTopology)
onMounted(() => void load())

const syncing = ref(false)
const syncMsg = ref('')
async function runSync() {
  if (syncing.value) return
  syncing.value = true
  syncMsg.value = ''
  const res = await syncTopology()
  if (res.state === 'live' && res.data) {
    const d = res.data
    syncMsg.value = d.State === 'failed' ? `同步失败：${d.Message || '网关未给原因'}（沿用上一轮快照）` : `拉取 ${d.Fetched} · 新增 ${d.Created} · 失效 ${d.Stale}`
    await load()
  } else {
    syncMsg.value = res.state === 'pending' ? '同步接口未上线' : '同步失败'
  }
  syncing.value = false
}

// ---- 抽屉 ----
type Sel = { kind: string; ref: string; data: Record<string, unknown> }
const sel = ref<Sel | null>(null)
const drawer = ref(false)
const saving = ref(false)
const externals = ref<ExternalModel[]>([])

const form = reactive({
  id: '',
  name: '',
  enabled: true,
  input: 0,
  output: 0,
  bound: '',
  isNew: false,
})

async function ensureExternals() {
  if (externals.value.length) return
  try {
    const r = await listExternalModels({ limit: 500 })
    externals.value = ((r as unknown as { Data?: ExternalModel[] }).Data || (r as unknown as { data?: ExternalModel[] }).data || []) as ExternalModel[]
  } catch {
    externals.value = []
  }
}

async function openNode(s: Sel) {
  sel.value = s
  drawer.value = true
  form.isNew = false
  form.bound = ''
  if (s.kind === 'external') {
    await ensureExternals()
    const m = externals.value.find((x) => x.ID === s.ref)
    form.id = s.ref
    form.name = m?.ExternalName || String(s.data.Name || '')
    form.enabled = m ? m.Enabled : true
    form.input = Number(m?.SaleRates?.Input ?? 0)
    form.output = Number(m?.SaleRates?.Output ?? 0)
    form.bound = (m?.BoundInternalRef as string) || ''
  } else if (s.kind === 'model' || s.kind === 'group') {
    // 从模型/路由组拉线：预填一个待新建的对外模型
    form.isNew = true
    form.id = ''
    form.name = String((s.data as Record<string, unknown>).Name || s.ref) + '-售'
    form.enabled = true
    form.input = 0
    form.output = 0
    form.bound = s.ref
  }
}

// 拖动连线：直接用源模型 ref 打开「新建对外模型」抽屉
async function openConnect(ref: string) {
  const item = data.value?.Items.find((m) => m.Ref === ref)
  await openNode({ kind: item?.Kind === 'group' ? 'group' : 'model', ref, data: { Name: item?.DisplayName || ref } })
}

async function save() {
  if (!form.name.trim()) {
    ElMessage.warning('名称不能为空')
    return
  }
  saving.value = true
  try {
    if (form.isNew) {
      const payload = {
        ExternalName: form.name.trim(),
        Description: '',
        Enabled: form.enabled,
        SaleRates: { Input: form.input, Output: form.output, CacheRead: 0 },
        BoundInternalRef: form.bound || null,
      }
      await createExternalModel(payload as never)
      ElMessage.success('已新建对外模型')
    } else {
      const cur = externals.value.find((x) => x.ID === form.id)
      const payload = {
        ExternalName: form.name.trim(),
        Description: cur?.Description || '',
        Enabled: form.enabled,
        SaleRates: { ...(cur?.SaleRates || {}), Input: form.input, Output: form.output },
        TimeConfig: cur?.TimeConfig ?? null,
        ContextTiers: cur?.ContextTiers ?? null,
        Aliases: cur?.Aliases || [],
        BoundInternalRef: form.bound || null,
      }
      await updateExternalModel(form.id, payload as never)
      ElMessage.success('已保存')
    }
    externals.value = []
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
      <el-button size="small" :loading="syncing" @click="runSync">同步</el-button>
      <el-button size="small" :loading="state === 'loading'" @click="load()">刷新</el-button>
    </div>

    <div v-if="state === 'error'" class="tp-err">数据没取到，先看错误再刷新。</div>
    <div v-else class="tp-canvas">
      <TopologyCanvas v-if="data" :view="data" @select="openNode" @connect="openConnect" />
      <div v-else class="tp-empty">还没有拓扑数据</div>
    </div>

    <el-drawer v-model="drawer" :title="`${selKindLabel}`" size="360px" direction="rtl">
      <template v-if="sel">
        <div v-if="sel.kind === 'external' || sel.kind === 'model' || sel.kind === 'group'">
          <el-form label-position="top" size="small">
            <el-form-item label="对外名称">
              <el-input v-model="form.name" placeholder="对外模型名" />
            </el-form-item>
            <el-form-item v-if="form.bound" label="绑定到">
              <el-input :model-value="form.bound" disabled />
            </el-form-item>
            <el-form-item label="启用">
              <el-switch v-model="form.enabled" />
            </el-form-item>
            <el-form-item label="输入价（积分/百万 token）">
              <el-input-number v-model="form.input" :min="0" :step="0.1" controls-position="right" style="width: 100%" />
            </el-form-item>
            <el-form-item label="输出价（积分/百万 token）">
              <el-input-number v-model="form.output" :min="0" :step="0.1" controls-position="right" style="width: 100%" />
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
</style>
