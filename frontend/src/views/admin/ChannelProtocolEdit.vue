<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { onBeforeRouteLeave, useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus, Delete, Edit, RefreshRight } from '@element-plus/icons-vue'
import {
  createChannelProtocol,
  getChannelProtocol,
  updateChannelProtocol,
  pullChannelProtocolModels,
  listExternalModels,
  createExternalModel,
  rateLabels,
  type ChannelProtocolInput,
  type ChannelProtocolModelOverride,
  type ProtocolOption,
  type ExternalModel,
  type PullModel,
} from '@/api/admin'
import KvpEditor from '@/components/KvpEditor.vue'
import StringListEditor from '@/components/StringListEditor.vue'
import ModelPricing from '@/components/ModelPricing.vue'
import ModelPriceCatalog from '@/components/ModelPriceCatalog.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

const route = useRoute()
const router = useRouter()
const id = route.params.id ? String(route.params.id) : ''
const isEdit = computed(() => !!id)

const loading = ref(false)
const saving = ref(false)
const activeTab = ref('base')
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

// 三个代码固定的基础协议
const baseProtocols = ['openai-chat', 'openai-responses', 'anthropic-messages'] as const
const protocolLabels: Record<string, string> = {
  'openai-chat': 'OpenAI Chat',
  'openai-responses': 'OpenAI Responses',
  'anthropic-messages': 'Anthropic Messages',
}

type ProtoState = { enabled: boolean; path: string; baseUrl: string }
const protocols = reactive<Record<string, ProtoState>>({
  'openai-chat': { enabled: false, path: '', baseUrl: '' },
  'openai-responses': { enabled: false, path: '', baseUrl: '' },
  'anthropic-messages': { enabled: false, path: '', baseUrl: '' },
})
// 首选协议（default）：同一作用域内至多一个，用单选下拉保证互斥
const defaultProtocol = ref<string>('')

const form = reactive({
  name: '',
  base_url: '',
  passthrough_mode: 'none',
  whitelist: [] as string[],
  blacklist: [] as string[],
  set: {} as Record<string, string>,
})

// 来源预设 ID（只读回显）
const presetSourceId = ref('')

// 请求头真实候选：HTTP 通用 / OpenAI / Anthropic 常用头（可自行输入任意头名）。
// 注意：内置强制黑名单（Cookie/Host/Content-Type/Accept-Encoding/X-Forwarded-*
// /anthropic-dangerous-direct-browser-access 等）任何模式下都不会发往上游，故不作候选，避免配了却不生效。
const headerSuggestions = [
  'Accept',
  'Accept-Language',
  'User-Agent',
  'Referer',
  'Origin',
  'Cache-Control',
  'Pragma',
  'OpenAI-Organization',
  'OpenAI-Project',
  'OpenAI-Beta',
  'anthropic-version',
  'anthropic-beta',
]
// 覆盖值可插入的变量
const valueVariables = ['{api_key}', '{session_id}', '{request_id}', '{model}', '{client_header:NAME}']

const passthroughOptions = [
  { label: '不透传', value: 'none' },
  { label: '白名单', value: 'whitelist' },
  { label: '黑名单', value: 'blacklist' },
  { label: '全部透传', value: 'all' },
]

const enabledProtocols = computed(() => baseProtocols.filter((p) => protocols[p].enabled))

// ===== 协议模型清单 =====
// 每行一个内部模型：可绑定对外模型、配置成本定价、收窄可达协议子集、覆盖 BaseURL。
type ModelRow = {
  id: string
  protocols: string[]
  baseUrl: string
  externalModelID: string | null
  pricing: Record<string, any>
}
const modelRows = ref<ModelRow[]>([])

// 下拉数据：启用的对外模型
const externalModels = ref<ExternalModel[]>([])

function emptyPricing(): Record<string, any> {
  return {
    CostRates: { Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0, Reasoning: 0 },
    TimeConfig: null,
    ContextTiers: null,
  }
}

async function loadExternalModels() {
  try {
    const res = await listExternalModels({ Enabled: true })
    externalModels.value = res.Data || []
  } catch {
    /* 忽略：绑定下拉为空时仍可编辑其它字段 */
  }
}

async function load() {
  if (!isEdit.value) return
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await getChannelProtocol(id)
    const v = res.Data
    form.name = v.Name
    form.base_url = v.BaseURL || ''
    presetSourceId.value = v.PresetSourceID || ''
    for (const p of baseProtocols) {
      const opt = v.Protocols?.[p]
      protocols[p].enabled = !!opt
      protocols[p].path = opt?.path || ''
      protocols[p].baseUrl = opt?.baseUrl || ''
      if (opt?.default) defaultProtocol.value = p
    }
    form.passthrough_mode = v.RequestHeaders?.passthroughMode || 'none'
    form.whitelist = v.RequestHeaders?.whitelist || []
    form.blacklist = v.RequestHeaders?.blacklist || []
    form.set = v.RequestHeaders?.set || {}
    modelRows.value = Object.entries(v.Models || {}).map(([mid, ov]) => ({
      id: mid,
      protocols: Object.keys(ov?.protocols || {}),
      baseUrl: ov?.baseUrl || '',
      externalModelID: ov?.externalModelID ?? null,
      pricing: {
        CostRates: { Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0, Reasoning: 0, ...(ov?.costRates || {}) },
        TimeConfig: ov?.timeConfig ?? null,
        ContextTiers: ov?.contextTiers ?? null,
      },
    }))
    savedSnapshot.value = snapshot()
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}

// ===== 未保存修改守卫 =====
// 以「提交口径」序列化当前表单快照，与最近一次加载/保存后的快照比对判断是否有未保存修改。
const savedSnapshot = ref('')
function snapshot(): string {
  return JSON.stringify({
    name: form.name,
    base_url: form.base_url,
    defaultProtocol: defaultProtocol.value,
    protocols: baseProtocols.map((p) => ({ name: p, ...protocols[p] })),
    passthrough_mode: form.passthrough_mode,
    whitelist: form.whitelist,
    blacklist: form.blacklist,
    set: form.set,
    models: modelRows.value,
  })
}
const dirty = computed(() => isEdit.value && snapshot() !== savedSnapshot.value)

function onBeforeUnload(e: BeforeUnloadEvent) {
  if (!dirty.value) return
  e.preventDefault()
  e.returnValue = ''
}

onBeforeRouteLeave(async () => {
  if (!dirty.value) return true
  try {
    await ElMessageBox.confirm('当前有未保存的修改，离开将丢失，确定离开吗？', '未保存的修改', {
      type: 'warning',
      confirmButtonText: '放弃修改并离开',
      cancelButtonText: '留在本页',
    })
    return true
  } catch {
    return false
  }
})

onMounted(() => {
  // 返回「渠道」合并页时保持选中「渠道协议」视图
  sessionStorage.setItem('admin-channels-tab', 'protocols')
  window.addEventListener('beforeunload', onBeforeUnload)
  loadExternalModels()
  load()
})
onBeforeUnmount(() => window.removeEventListener('beforeunload', onBeforeUnload))

// ===== 模型行 增/改/删 =====
const modelDrawerOpen = ref(false)
const editingIndex = ref<number | null>(null) // null = 新增；否则为编辑的行号
const editingForm = ref<{ id: string; protocols: string[]; baseUrl: string; externalModelID: string | null }>({
  id: '',
  protocols: [],
  baseUrl: '',
  externalModelID: null,
})
const editingPricing = ref<Record<string, any>>(emptyPricing())
const editingExternalPrev = ref<string | null>(null)

function openCreateModel() {
  editingIndex.value = null
  editingExternalPrev.value = null
  editingForm.value = { id: '', protocols: [], baseUrl: '', externalModelID: null }
  editingPricing.value = emptyPricing()
  modelDrawerOpen.value = true
}
function openEditModel(row: ModelRow, index: number) {
  editingIndex.value = index
  editingExternalPrev.value = row.externalModelID
  editingForm.value = {
    id: row.id,
    protocols: [...row.protocols],
    baseUrl: row.baseUrl,
    externalModelID: row.externalModelID,
  }
  editingPricing.value = {
    CostRates: { ...(row.pricing.CostRates || {}) },
    TimeConfig: row.pricing.TimeConfig ? JSON.parse(JSON.stringify(row.pricing.TimeConfig)) : null,
    ContextTiers: row.pricing.ContextTiers ? JSON.parse(JSON.stringify(row.pricing.ContextTiers)) : null,
  }
  modelDrawerOpen.value = true
}
function saveModel() {
  const mid = editingForm.value.id.trim()
  if (!mid) return ElMessage.warning('请填写内部模型 ID')
  const dup = modelRows.value.some((r, i) => i !== editingIndex.value && r.id.trim() === mid)
  if (dup) return ElMessage.warning('该内部模型 ID 已存在')
  const row: ModelRow = {
    id: mid,
    protocols: [...editingForm.value.protocols],
    baseUrl: editingForm.value.baseUrl.trim(),
    externalModelID: editingForm.value.externalModelID,
    pricing: {
      CostRates: { ...(editingPricing.value.CostRates || {}) },
      TimeConfig: editingPricing.value.TimeConfig ?? null,
      ContextTiers: editingPricing.value.ContextTiers ?? null,
    },
  }
  if (editingIndex.value == null) modelRows.value.push(row)
  else modelRows.value[editingIndex.value] = row
  modelDrawerOpen.value = false
}
function removeModelRow(index: number) {
  modelRows.value.splice(index, 1)
}

// 快速新建对外模型：绑定下拉选到「＋ 新建对外模型」时弹出
const createExternalOpen = ref(false)
const createExternalName = ref('')
const createExternalEnabled = ref(true)
const createExternalUseCost = ref(true)
const createExternalSaving = ref(false)

function openQuickCreateExternal() {
  // 对外名称默认用当前内部模型 ID 预填，减少手工输入
  createExternalName.value = editingForm.value.id.trim()
  createExternalEnabled.value = true
  createExternalUseCost.value = true
  createExternalOpen.value = true
}
function onExternalChange(val: string | null) {
  if (val === '__create__') {
    editingForm.value.externalModelID = editingExternalPrev.value
    openQuickCreateExternal()
    return
  }
  editingExternalPrev.value = val
}
async function confirmCreateExternal() {
  const name = createExternalName.value.trim()
  if (!name) return ElMessage.warning('请填写对外名称')
  const useCost = createExternalUseCost.value
  const costRates = useCost ? editingPricing.value.CostRates : null
  const saleRates = { Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0, Reasoning: 0, ...(costRates || {}) }
  // 初始化售价时同步时段与分档（与内部模型成本侧一致），避免仅同步五段费率而丢失时段/分档。
  const timeConfig = useCost ? editingPricing.value.TimeConfig ?? null : null
  const contextTiers = useCost ? editingPricing.value.ContextTiers ?? null : null
  createExternalSaving.value = true
  try {
    const res = await createExternalModel({
      ExternalName: name,
      Description: '',
      Enabled: createExternalEnabled.value,
      SaleRates: saleRates,
      TimeConfig: timeConfig,
      ContextTiers: contextTiers,
    })
    const created = res.Data
    externalModels.value.push(created)
    editingForm.value.externalModelID = created.ID
    editingExternalPrev.value = created.ID
    ElMessage.success('对外模型已创建')
    createExternalOpen.value = false
  } catch (e: any) {
    ElMessage.error(e?.message || '创建对外模型失败')
  } finally {
    createExternalSaving.value = false
  }
}

// 从 models 端点拉取（临时 ApiKey，不落库）
const pullDialogOpen = ref(false)
const pullApiKey = ref('')
const pullLoading = ref(false)
const pulled = ref<PullModel[]>([])
const pullChecked = ref<PullModel[]>([])

const addedIds = computed(() => new Set(modelRows.value.map((m) => m.id.trim())))
function isAdded(mid: string): boolean {
  return addedIds.value.has(mid.trim())
}
function openPull() {
  pullApiKey.value = ''
  pulled.value = []
  pullChecked.value = []
  pullDialogOpen.value = true
}
async function doPull() {
  if (!pullApiKey.value.trim()) return ElMessage.warning('请填写临时 ApiKey')
  pullLoading.value = true
  try {
    const res = await pullChannelProtocolModels(id, pullApiKey.value.trim())
    pulled.value = res.Data.List || []
    pullChecked.value = []
  } catch (e: any) {
    ElMessage.error(e?.message || '拉取协议模型失败')
  } finally {
    pullLoading.value = false
  }
}
function onAddPulled() {
  if (!pullChecked.value.length) return
  let added = 0
  for (const p of pullChecked.value) {
    if (isAdded(p.ID)) continue
    modelRows.value.push({ id: p.ID, protocols: [], baseUrl: '', externalModelID: null, pricing: emptyPricing() })
    added++
  }
  ElMessage.success(`已加入 ${added} 个模型（请在清单中绑定对外模型并配置成本）`)
  pullDialogOpen.value = false
}

// models.dev 参考价：合并到当前编辑模型的成本侧
const catalogRef = ref<InstanceType<typeof ModelPriceCatalog> | null>(null)
const currentModelId = computed(() => editingForm.value.id)
function onApplyCostCatalog(partial: Record<string, number>) {
  editingPricing.value.CostRates = { ...editingPricing.value.CostRates, ...partial }
}

// 从对外模型同步价格：用所绑定对外模型的售价 / 时段 / 分档覆盖当前内部模型的成本侧。
function syncCostFromExternal() {
  const extId = editingForm.value.externalModelID
  if (!extId) return ElMessage.warning('请先绑定一个对外模型')
  const em = externalModels.value.find((e) => String(e.ID) === String(extId))
  if (!em) return ElMessage.warning('未找到该对外模型（可能已停用）')
  editingPricing.value.CostRates = { ...(em.SaleRates || {}) }
  editingPricing.value.TimeConfig = em.TimeConfig ? JSON.parse(JSON.stringify(em.TimeConfig)) : null
  editingPricing.value.ContextTiers = em.ContextTiers ? JSON.parse(JSON.stringify(em.ContextTiers)) : null
  ElMessage.success(`已用对外模型「${em.ExternalName}」的定价覆盖成本、时段与分档`)
}

// 展示辅助
function externalName(extId: string | null): string {
  if (!extId) return ''
  const em = externalModels.value.find((e) => String(e.ID) === String(extId))
  return em?.ExternalName || ''
}
function fmtCost(row: ModelRow, k: string): string {
  const v = (row.pricing.CostRates || {})[k]
  return v == null ? '-' : String(Math.round(v * 1e6) / 1e6)
}
function costDetail(row: ModelRow): string {
  const r = row.pricing.CostRates || {}
  const parts = ['Input', 'Output', 'CacheRead', 'CacheWrite', 'Reasoning'].map(
    (k) => `${rateLabels[k]} ${r[k] == null ? '-' : Math.round(r[k] * 1e6) / 1e6}`,
  )
  return `${parts.join('，')}（单位：人民币元/百万 token）`
}
function protocolNames(row: ModelRow): string {
  return row.protocols.map((p) => protocolLabels[p] || p).join('、')
}

// 由行数据组装后端 Models（{ [内部模型 ID]: { baseUrl?, protocols?, externalModelID?, costRates?, timeConfig?, contextTiers? } }）
function buildModels(): Record<string, ChannelProtocolModelOverride> {
  const out: Record<string, ChannelProtocolModelOverride> = {}
  for (const r of modelRows.value) {
    const mid = r.id.trim()
    if (!mid) continue
    const ov: ChannelProtocolModelOverride = {}
    if (r.baseUrl.trim()) ov.baseUrl = r.baseUrl.trim()
    if (r.protocols.length) {
      const p: Record<string, ProtocolOption> = {}
      for (const name of r.protocols) p[name] = {}
      ov.protocols = p
    }
    if (r.externalModelID) ov.externalModelID = r.externalModelID
    if (r.pricing.CostRates) ov.costRates = r.pricing.CostRates
    if (r.pricing.TimeConfig) ov.timeConfig = r.pricing.TimeConfig
    if (r.pricing.ContextTiers) ov.contextTiers = r.pricing.ContextTiers
    out[mid] = ov
  }
  return out
}

async function onSubmit() {
  if (!form.name.trim()) return ElMessage.warning('请填写名称')
  if (form.base_url.trim() && !/^https?:\/\//.test(form.base_url.trim()))
    return ElMessage.warning('BaseURL 必须以 http(s):// 开头')
  if (!enabledProtocols.value.length) return ElMessage.warning('请至少启用一个基础协议')

  const protoOut: Record<string, ProtocolOption> = {}
  for (const p of baseProtocols) {
    if (!protocols[p].enabled) continue
    const opt: ProtocolOption = {}
    if (protocols[p].path.trim()) opt.path = protocols[p].path.trim()
    if (protocols[p].baseUrl.trim()) opt.baseUrl = protocols[p].baseUrl.trim()
    if (defaultProtocol.value === p) opt.default = true
    protoOut[p] = opt
  }

  const payload: ChannelProtocolInput = {
    Name: form.name.trim(),
    BaseURL: form.base_url.trim(),
    Protocols: protoOut,
    RequestHeaders: {
      passthroughMode: form.passthrough_mode,
      whitelist: form.whitelist,
      blacklist: form.blacklist,
      set: form.set,
    },
    Models: buildModels(),
  }

  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    if (isEdit.value) {
      await updateChannelProtocol(id, payload)
      ElMessage.success('渠道协议已更新')
      await load()
    } else {
      await createChannelProtocol(payload)
      ElMessage.success('渠道协议已创建')
      router.push('/admin/channels')
    }
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="page-container">
    <el-tabs v-model="activeTab" class="edit-tabs">
      <el-tab-pane label="基本信息" name="base">
        <div v-loading="loading" class="card">
          <el-form label-width="130px" class="form-grid">
            <el-form-item label="名称" required>
              <el-input v-model="form.name" maxlength="64" placeholder="如 OpenCode Zen" />
            </el-form-item>
            <el-form-item label="来源预设 ID">
              <el-input :model-value="presetSourceId || '—'" disabled placeholder="—" />
              <div class="hint">来源预设 ID 由同步流程写入；手动新建时为空</div>
            </el-form-item>
            <el-form-item class="span-2" label="Base URL">
              <el-input v-model="form.base_url" placeholder="https://api.example.com/v1" />
              <div class="hint">渠道协议级默认上游地址；各协议可单独覆盖</div>
            </el-form-item>
          </el-form>

          <div class="section-title">基础协议</div>
          <div class="block-desc">选择该协议模板原生支持的上游基础协议；键存在 = 支持。可逐协议覆盖路径与上游地址。鉴权由协议隐含（openai-* → Authorization Bearer；anthropic-messages → x-api-key），如需其它方式请在「请求头策略 → 覆盖」中显式写入。</div>
          <div v-for="p in baseProtocols" :key="p" class="proto-row">
            <el-checkbox v-model="protocols[p].enabled" class="proto-check">{{ protocolLabels[p] }}</el-checkbox>
            <div class="proto-fields">
              <el-input v-model="protocols[p].path" placeholder="路径，留空用内置默认（如 /chat/completions）" :disabled="!protocols[p].enabled" />
              <el-input v-model="protocols[p].baseUrl" placeholder="该协议专属 BaseURL，留空回退协议级" :disabled="!protocols[p].enabled" />
            </div>
          </div>
          <el-form label-width="130px" class="init-form">
            <el-form-item label="首选协议">
              <el-select v-model="defaultProtocol" clearable placeholder="不指定" style="width: 320px">
                <el-option v-for="p in enabledProtocols" :key="p" :label="protocolLabels[p]" :value="p" />
              </el-select>
              <div class="hint">标记 default，同一作用域内至多一个；用于二期跨协议转换的目标协议</div>
            </el-form-item>
          </el-form>
        </div>
      </el-tab-pane>

      <el-tab-pane label="请求头策略" name="headers">
        <div class="card">
          <div class="block-desc">控制下游请求头如何透传到上游；鉴权由基础协议隐含注入，「覆盖」在其之后应用，可显式覆盖鉴权头。</div>
          <el-form label-width="130px">
            <el-form-item label="透传模式">
              <el-segmented :model-value="form.passthrough_mode" :options="passthroughOptions" @change="(v: string | number | boolean) => (form.passthrough_mode = String(v))" />
            </el-form-item>
            <el-form-item v-if="form.passthrough_mode === 'whitelist'" label="白名单">
              <StringListEditor v-model="form.whitelist" :suggestions="headerSuggestions" placeholder="仅这些下游头透传" />
            </el-form-item>
            <el-form-item v-else-if="form.passthrough_mode === 'blacklist'" label="黑名单">
              <StringListEditor v-model="form.blacklist" :suggestions="headerSuggestions" placeholder="除这些下游头外透传" />
            </el-form-item>
            <el-form-item label="覆盖">
              <KvpEditor
                v-model="form.set"
                key-placeholder="请求头名"
                value-placeholder="值（支持变量）"
                :key-suggestions="headerSuggestions"
                :value-suggestions="valueVariables"
              />
              <div class="hint">键可下拉选常用头，也可直接输入；值支持变量插入。内置强制黑名单（Cookie / Host / Content-Type / Content-Length / Accept-Encoding / X-Forwarded-* / anthropic-dangerous-direct-browser-access 等）任何模式下都不会发往上游，覆盖同样被忽略；鉴权头（Authorization / x-api-key）可被覆盖。</div>
            </el-form-item>
          </el-form>
        </div>
      </el-tab-pane>

      <el-tab-pane label="模型清单" name="models">
        <div class="card">
          <div class="model-head">
            <div class="block-desc">列出该协议模板下的内部模型；可为每个模型绑定对外模型、配置成本定价、收窄可达协议子集并覆盖 BaseURL（留空 = 继承渠道协议级配置）。</div>
            <div class="model-toolbar">
              <el-tooltip v-if="!isEdit" content="保存协议后可用临时 ApiKey 从 models 端点拉取" placement="top">
                <span><el-button :icon="RefreshRight" disabled>从 models 端点拉取</el-button></span>
              </el-tooltip>
              <el-button v-else :icon="RefreshRight" @click="openPull">从 models 端点拉取</el-button>
              <el-button type="primary" :icon="Plus" @click="openCreateModel">添加模型</el-button>
            </div>
          </div>
          <el-table :data="modelRows" border stripe class="model-table table-nowrap">
            <el-table-column label="内部模型 ID" min-width="180" show-overflow-tooltip>
              <template #default="{ row }">{{ row.id || '—' }}</template>
            </el-table-column>
            <el-table-column label="绑定对外模型" min-width="160">
              <template #default="{ row }">
                <span v-if="externalName(row.externalModelID)">{{ externalName(row.externalModelID) }}</span>
                <el-tag v-else type="info" size="small" effect="light">未绑定</el-tag>
              </template>
            </el-table-column>
            <el-table-column label="成本·输入" width="110" align="right">
              <template #default="{ row }">
                <el-tooltip :content="costDetail(row)" placement="top">
                  <span class="rate-cell">{{ fmtCost(row, 'Input') }}</span>
                </el-tooltip>
              </template>
            </el-table-column>
            <el-table-column label="成本·输出" width="110" align="right">
              <template #default="{ row }">
                <el-tooltip :content="costDetail(row)" placement="top">
                  <span class="rate-cell">{{ fmtCost(row, 'Output') }}</span>
                </el-tooltip>
              </template>
            </el-table-column>
            <el-table-column label="可达协议" min-width="180">
              <template #default="{ row }">{{ protocolNames(row) || '继承协议级' }}</template>
            </el-table-column>
            <el-table-column label="BaseURL 覆盖" min-width="180" show-overflow-tooltip>
              <template #default="{ row }">{{ row.baseUrl || '—' }}</template>
            </el-table-column>
            <el-table-column label="操作" width="110" align="right">
              <template #default="{ row, $index }">
                <div class="op-cell">
                  <el-tooltip content="编辑" placement="top">
                    <el-icon class="op-icon" @click="openEditModel(row, $index)"><Edit /></el-icon>
                  </el-tooltip>
                  <el-tooltip content="删除" placement="top">
                    <el-icon class="op-icon danger" @click="removeModelRow($index)"><Delete /></el-icon>
                  </el-tooltip>
                </div>
              </template>
            </el-table-column>
          </el-table>
          <el-empty v-if="!modelRows.length" description="暂无模型" :image-size="48" />
        </div>
      </el-tab-pane>
    </el-tabs>

    <div class="actions">
      <el-button type="primary" :loading="saving" @click="onSubmit">{{ isEdit ? '保存' : '创建' }}</el-button>
      <el-button @click="router.push('/admin/channels')">取消</el-button>
      <el-tag v-if="dirty" type="warning" size="small" effect="light">有未保存的修改</el-tag>
      <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
    </div>

    <!-- 协议模型 编辑抽屉 -->
    <el-drawer
      v-model="modelDrawerOpen"
      :title="editingIndex == null ? '添加协议模型' : '编辑协议模型'"
      :size="'min(880px, calc(100vw - 48px))'"
      destroy-on-close
    >
      <el-form label-width="120px" class="model-form">
        <el-form-item label="内部模型 ID" required>
          <el-input v-model="editingForm.id" placeholder="上游模型名（请求体 model 字段）" />
        </el-form-item>
        <el-form-item label="绑定对外模型">
          <el-select v-model="editingForm.externalModelID" style="width: 100%" placeholder="选择对外的发售模型" @change="onExternalChange">
            <el-option v-for="em in externalModels" :key="em.ID" :label="em.ExternalName" :value="em.ID" />
            <el-option label="＋ 新建对外模型" value="__create__" />
          </el-select>
        </el-form-item>
        <el-form-item label="可达协议">
          <el-select v-model="editingForm.protocols" multiple clearable placeholder="留空 = 继承协议级" style="width: 100%">
            <el-option v-for="p in baseProtocols" :key="p" :label="protocolLabels[p]" :value="p" />
          </el-select>
        </el-form-item>
        <el-form-item label="BaseURL 覆盖">
          <el-input v-model="editingForm.baseUrl" placeholder="留空 = 继承协议级" />
        </el-form-item>
        <div class="span-2">
          <ModelPricing v-model="editingPricing" show-rates-key="CostRates" title="成本">
            <template #title-extra>
              <el-button @click="catalogRef?.open()">从 models.dev 获取参考价</el-button>
              <el-button @click="syncCostFromExternal">从对外模型同步价格</el-button>
            </template>
          </ModelPricing>
        </div>
      </el-form>
      <template #footer>
        <el-button @click="modelDrawerOpen = false">取消</el-button>
        <el-button type="primary" @click="saveModel">保存</el-button>
      </template>
    </el-drawer>

    <!-- 从 models 端点拉取（临时 ApiKey）弹窗 -->
    <el-dialog v-model="pullDialogOpen" title="从 models 端点拉取" width="560px" destroy-on-close>
      <div class="pull-row">
        <el-input v-model="pullApiKey" placeholder="临时 ApiKey（仅本次拉取使用，不落库）" show-password clearable @keyup.enter="doPull" />
        <el-button type="primary" :loading="pullLoading" @click="doPull">拉取</el-button>
      </div>
      <el-table :data="pulled" border max-height="340" @selection-change="(rows: PullModel[]) => (pullChecked = rows)">
        <el-table-column type="selection" width="40" :selectable="(row: PullModel) => !isAdded(row.ID)" />
        <el-table-column label="模型 ID" min-width="200" show-overflow-tooltip>
          <template #default="{ row }">
            <span :class="{ 'muted-id': isAdded(row.ID) }">{{ row.ID }}</span>
            <el-tag v-if="isAdded(row.ID)" size="small" type="info" effect="plain" class="added-tag">已添加</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="归属" prop="OwnedBy" min-width="120" show-overflow-tooltip />
      </el-table>
      <el-empty v-if="!pullLoading && pullApiKey && !pulled.length" description="未拉取到模型，请检查 ApiKey 并点击「拉取」" :image-size="48" />
      <template #footer>
        <el-button @click="pullDialogOpen = false">取消</el-button>
        <el-button type="primary" :disabled="!pullChecked.length" @click="onAddPulled">加入清单（{{ pullChecked.length }}）</el-button>
      </template>
    </el-dialog>

    <!-- 快速新建对外模型 -->
    <el-dialog v-model="createExternalOpen" title="新建对外模型" width="480px" destroy-on-close>
      <el-form label-width="120px">
        <el-form-item label="对外名称" required>
          <el-input v-model="createExternalName" placeholder="如 gpt-4o" maxlength="128" clearable />
        </el-form-item>
        <el-form-item label="启用">
          <el-switch v-model="createExternalEnabled" />
        </el-form-item>
        <el-form-item label="初始化售价">
          <el-checkbox v-model="createExternalUseCost">用当前内部模型成本初始化售价</el-checkbox>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="createExternalOpen = false">取消</el-button>
        <el-button type="primary" :loading="createExternalSaving" @click="confirmCreateExternal">创建</el-button>
      </template>
    </el-dialog>

    <!-- models.dev 参考价市场 -->
    <ModelPriceCatalog ref="catalogRef" :model-name="currentModelId" label="成本" @apply="onApplyCostCatalog" />
  </div>
</template>

<style scoped>
.edit-tabs {
  width: 100%;
}
.edit-tabs :deep(.el-tabs__header) {
  margin-bottom: 18px;
}
.form-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  align-items: start;
  gap: 0 28px;
}
.form-grid .span-2 {
  grid-column: 1 / -1;
}
@media (max-width: 900px) {
  .form-grid {
    grid-template-columns: 1fr;
  }
}
.hint {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.5;
  color: var(--color-text-secondary);
}
.block-desc {
  color: var(--color-text-secondary);
  font-size: 13px;
  margin-bottom: 14px;
}
.section-title {
  font-size: 15px;
  font-weight: 600;
  margin: 18px 0 8px;
  padding-top: 14px;
  border-top: 1px solid var(--color-border);
}
.proto-row {
  display: flex;
  align-items: flex-start;
  gap: 16px;
  padding: 10px 0;
  border-bottom: 1px solid var(--color-border);
}
.proto-check {
  width: 160px;
  flex: none;
  margin-top: 6px;
}
.proto-fields {
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.init-form {
  margin-top: 16px;
}
.model-head {
  display: flex;
  align-items: flex-start;
  gap: 12px;
}
.model-head .block-desc {
  flex: 1;
  margin-bottom: 14px;
}
.model-toolbar {
  display: flex;
  gap: 8px;
  flex: none;
}
.model-table {
  margin-top: 4px;
}
.rate-cell {
  cursor: help;
  font-variant-numeric: tabular-nums;
  color: var(--color-text);
}
.muted-id {
  color: var(--color-text-tertiary);
  text-decoration: line-through;
}
.added-tag {
  margin-left: 8px;
}
.pull-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
}
.model-form {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 0 24px;
}
.model-form .span-2 {
  grid-column: 1 / -1;
}
.actions {
  margin-top: 20px;
  display: flex;
  align-items: center;
  gap: 8px;
}
</style>
