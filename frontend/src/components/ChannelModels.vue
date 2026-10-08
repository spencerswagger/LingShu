<script setup lang="ts">
// 渠道内部模型子表：承载该渠道支持哪些内部模型（InternalModelID + 成本定价 + 绑定对外模型）。
// 编辑态（readonly=false）支持：新增/编辑（抽屉内 ModelPricing 成本模式）+ 拉取上游模型。
// 只读态（readonly=true）：仅供详情页展示。
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus, RefreshRight, Edit, Delete } from '@element-plus/icons-vue'
import {
  listChannelModels,
  createChannelModel,
  updateChannelModel,
  deleteChannelModel,
  pullChannelModels,
  channelModelState,
  listExternalModels,
  priceCatalog,
  getBillingConfig,
  runSync,
  rateLabels,
  type ChannelModel,
  type ExternalModel,
  type PullModel,
  type CatalogEntry,
} from '@/api/admin'
import ModelPricing from '@/components/ModelPricing.vue'
import StatusTag from '@/components/StatusTag.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'
import { fmtDateTime } from '@/utils/format'

const props = defineProps<{
  channelId: string
  readonly?: boolean
}>()

const loading = ref(false)
const list = ref<ChannelModel[]>([])
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

// 下拉数据：启用的对外模型
const externalModels = ref<ExternalModel[]>([])

// 编辑抽屉
const drawerOpen = ref(false)
const editingId = ref<string | null>(null) // null = 新增；否则为编辑的模型 id
const editingForm = ref<Record<string, any>>({ InternalModelID: '', ExternalModelID: null })
const editingPricing = ref<Record<string, any>>({
  CostRates: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0 },
  TimeConfig: null,
  ContextTiers: null,
})
const saving = ref(false)
const runtimeExpanded = ref<string[]>([])

// 拉取上游模型后「批量配置」的待创建行：每个模型独立绑定对外模型与成本
interface PendingRow {
  InternalModelID: string
  ExternalModelID: string | null
  pricing: Record<string, any>
}
const pendingRows = ref<PendingRow[]>([])
const activeRowIndex = ref(0)
const activeRow = ref<PendingRow | null>(null)

// 拉取上游模型
const pullLoading = ref(false)
const pullDialogOpen = ref(false)
const pulled = ref<PullModel[]>([])
const pullChecked = ref<PullModel[]>([])

// 已添加的内部模型 ID 集合：拉取弹窗中这些行不可勾选
const addedIds = computed(() => new Set(list.value.map((m) => m.InternalModelID)))

function isAdded(id: string): boolean {
  return addedIds.value.has(id)
}

async function loadModules(dp: string) {
  const [cmRes, emRes] = await Promise.all([
    listChannelModels(dp),
    listExternalModels({ enabled: true }),
  ])
  list.value = cmRes.data || []
  externalModels.value = emRes.data || []
}

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    await loadModules(props.channelId)
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)

function fmtCost(m: ChannelModel, k: string): string {
  const v = (m.CostRates || {})[k]
  return v == null ? '-' : String(Math.round(v * 1e6) / 1e6)
}

function costDetail(m: ChannelModel): string {
  const r = m.CostRates || {}
  const parts = ['input', 'output', 'cache_read', 'cache_write', 'reasoning']
    .map((k) => `${rateLabels[k]} ${r[k] == null ? '-' : Math.round(r[k] * 1e6) / 1e6}`)
  return `${parts.join('，')}（单位：人民币元/百万 token）`
}

function openCreate() {
  editingId.value = null
  editingForm.value = {
    InternalModelID: '',
    ExternalModelID: null,
    CostRates: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0 },
    TimeConfig: null,
    ContextTiers: null,
  }
  editingPricing.value = {
    CostRates: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0 },
    TimeConfig: null,
    ContextTiers: null,
  }
  drawerOpen.value = true
}

// 手动设置内部模型状态（正常/排空/禁用）
async function onToggleState(m: ChannelModel, action: 'normal' | 'drain' | 'disable') {
  try {
    await channelModelState(props.channelId, m.ID, action)
    ElMessage.success('模型状态已更新')
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '状态操作失败')
  }
}

function openEdit(m: ChannelModel) {
  editingId.value = m.ID
  editingForm.value = {
    ...m,
    // 对外模型 ID 现为雪花字符串，直接原样回填保证 el-select 精确匹配
    ExternalModelID: m.ExternalModelID,
    // 模型级运行时配置（留空 = 继承渠道级兜底）
    RateLimit: { ...(m.RateLimit || { RPM: 0, TPM: 0, BurstMultiplier: 0, MaxConcurrent: 0 }) },
    HealthProbe: { ...(m.HealthProbe || { Interval: '', DrainIntervalSeconds: 0, TimeoutMS: 0, FailThreshold: 0, RecoveryThreshold: 0, ProbeModel: '' }) },
    Reliability: { ...(m.Reliability || { WindowSeconds: 0, MinSamples: 0, ErrorRatePct: 0, Rate429Pct: 0, P99LatencyMS: 0 }) },
  }
  editingPricing.value = {
    CostRates: { ...m.CostRates },
    TimeConfig: m.TimeConfig,
    ContextTiers: m.ContextTiers,
  }
  drawerOpen.value = true
}

async function saveModel() {
  if (!editingForm.value) return
  if (!editingForm.value.InternalModelID.trim())
    return ElMessage.warning('请填写内部模型 ID')
  if (editingForm.value.ExternalModelID == null)
    return ElMessage.warning('请绑定一个对外模型')

  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  const payload = {
    InternalModelID: editingForm.value.InternalModelID.trim(),
    ExternalModelID: editingForm.value.ExternalModelID,
    CostRates: editingPricing.value.CostRates,
    TimeConfig: editingPricing.value.TimeConfig,
    ContextTiers: editingPricing.value.ContextTiers,
    RateLimit: editingForm.value.RateLimit,
    HealthProbe: editingForm.value.HealthProbe,
    Reliability: editingForm.value.Reliability,
  }
  try {
    // 编辑既有条目
    if (editingId.value != null) {
      await updateChannelModel(props.channelId, editingId.value, payload)
      ElMessage.success('内部模型已更新')
      drawerOpen.value = false
      await load()
      return
    }
    await createChannelModel(props.channelId, payload)
    ElMessage.success('内部模型已添加')
    drawerOpen.value = false
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '保存失败')
  } finally {
    saving.value = false
  }
}

// 批量保存：逐行校验并创建（每个模型各自绑定对外模型与成本）
async function saveBatch() {
  for (const row of pendingRows.value) {
    if (row.ExternalModelID == null) {
      return ElMessage.warning(`「${row.InternalModelID}」请先绑定一个对外模型`)
    }
  }
  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    for (const row of pendingRows.value) {
      await createChannelModel(props.channelId, {
        InternalModelID: row.InternalModelID.trim(),
        ExternalModelID: row.ExternalModelID!,
        CostRates: row.pricing.CostRates,
        TimeConfig: row.pricing.TimeConfig,
        ContextTiers: row.pricing.ContextTiers,
      })
    }
    ElMessage.success(`已批量加入 ${pendingRows.value.length} 个内部模型`)
    pendingRows.value = []
    activeRow.value = null
    activeRowIndex.value = 0
    drawerOpen.value = false
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '批量创建失败')
  } finally {
    saving.value = false
  }
}

async function onDelete(m: ChannelModel) {
  try {
    await ElMessageBox.confirm(
      `确认删除该渠道的内部模型「${m.InternalModelID}」吗？`,
      '删除确认',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await deleteChannelModel(props.channelId, m.ID)
    ElMessage.success('已删除')
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '删除失败')
  }
}

// 拉取上游模型
async function onPull() {
  pullLoading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await pullChannelModels(props.channelId)
    pulled.value = res.data.list || []
    pullChecked.value = []
    pullDialogOpen.value = true
  } catch (e: any) {
    ElMessage.error(e?.message || '拉取渠道模型失败')
  } finally {
    pullLoading.value = false
  }
}

// 勾选结果：关闭拉取弹窗，打开「批量配置」抽屉 —— 每个勾选的模型
// 都作为一行单独绑定对外模型、单独配置成本，统一落库
function onAddPulled() {
  if (!pullChecked.value.length) return
  pendingRows.value = pullChecked.value.map((p) => ({
    InternalModelID: p.ID,
    ExternalModelID: null,
    pricing: {
      CostRates: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0 },
      TimeConfig: null,
      ContextTiers: null,
    },
  }))
  activeRowIndex.value = 0
  activeRow.value = pendingRows.value[0]
  pullDialogOpen.value = false
  // 与弹窗关闭动画错开：同一帧内先关 dialog 再开 drawer 会触发 Element Plus
  // 浮层过渡竞态，导致抽屉 body 渲染中断、无法正常关闭。等关闭动画结束再开抽屉。
  window.setTimeout(() => {
    drawerOpen.value = true
  }, 280)
}

// ---- models.dev 参考价市场（为成本定价取数） ----
const catOpen = ref(false)
const catQ = ref('')
const catLoading = ref(false)
const catList = ref<CatalogEntry[]>([])
const catUpdatedAt = ref('')
const catTotal = ref(0)
const catR = ref(10000)
const catCnyRate = ref(6.8)
const catApplying = ref(false)
const catSyncing = ref(false)
const selectedEntry = ref<CatalogEntry | null>(null)

// 当前配置中的「内部模型 ID」：批量模式下取当前行，否则取单条表单
const currentModelId = computed(() =>
  pendingRows.value.length ? activeRow.value?.InternalModelID || '' : editingForm.value.InternalModelID,
)
// 当前配置的定价对象：批量模式下为当前行 pricing，否则为单条表单 pricing
const currentPricing = computed(() =>
  pendingRows.value.length ? activeRow.value?.pricing || null : editingPricing.value,
)

// 推荐条目：与当前内部模型 ID 完全一致且供应商匹配前缀
const recommended = computed(() => {
  const name = currentModelId.value
  if (!name) return null
  const dash = name.indexOf('-')
  const prefix = dash > 0 ? name.slice(0, dash) : name
  let best: CatalogEntry | null = null
  for (const e of catList.value) {
    if (e.ModelID !== name) continue
    if (e.Provider === prefix) {
      best = e
      break
    }
    if (best == null || e.Provider < best.Provider) best = e
  }
  return best || catList.value[0] || null
})

// 内部模型 ID 前缀（'-' 之前），用于标记「官方」供应商行
const modelNamePrefix = computed(() => {
  const name = currentModelId.value.trim()
  const dash = name.indexOf('-')
  return dash > 0 ? name.slice(0, dash) : name
})

// models.dev 参考价 → 倍率（每百万 token 计费数值；汇率仅在系统设置中配置）
function usdToCnyPerM(usd: number): number {
  return Math.round(usd * catCnyRate.value * 1e6) / 1e6
}
function usdToCredits(usd: number): number {
  const rate = usdToCnyPerM(usd)
  return Math.round((rate * 1e6) / catR.value * 1e6) / 1e6
}

async function onOpenCatalog() {
  catOpen.value = true
  catQ.value = currentModelId.value || ''
  try {
    const res = await getBillingConfig()
    const v = Number(res.data.r)
    if (v > 0) catR.value = v
    const c = Number(res.data.cny_rate)
    if (c > 0) catCnyRate.value = c
  } catch {
    /* 保持默认 */
  }
  await onCatalogSearch()
}

async function onCatalogSearch() {
  catLoading.value = true
  try {
    const res = await priceCatalog(catQ.value)
    catList.value = res.data.list || []
    catUpdatedAt.value = res.data.updated_at || ''
    catTotal.value = res.data.total || 0
    selectedEntry.value = null
  } catch (e: any) {
    ElMessage.error(e?.message || '获取 models.dev 参考价失败')
  } finally {
    catLoading.value = false
  }
}

// 立即触发一次 models.dev 同步，成功后重搜当前关键词
async function onSyncNow() {
  catSyncing.value = true
  try {
    const res = await runSync()
    ElMessage.success(res.data.summary)
    await onCatalogSearch()
  } catch (e: any) {
    ElMessage.error(e?.message || '同步 models.dev 失败')
  } finally {
    catSyncing.value = false
  }
}

// 应用所选（或推荐）条目为当前配置行的成本五段费率
async function onApplyCostCatalog() {
  const entry = selectedEntry.value || recommended.value
  if (!entry) return ElMessage.warning('目录为空或未找到参考价')
  const pricing = currentPricing.value
  if (!pricing) return ElMessage.warning('尚未选中需要配置的模型')
  catApplying.value = true
  try {
    const next: Record<string, number> = { ...pricing.CostRates }
    const apply = (k: string, usd: number) => {
      if (usd > 0) next[k] = usdToCnyPerM(usd)
    }
    apply('input', entry.InputUSD)
    apply('output', entry.OutputUSD)
    apply('cache_read', entry.CacheReadUSD)
    apply('cache_write', entry.CacheWriteUSD)
    apply('reasoning', entry.ReasoningUSD)
    pricing.CostRates = next
    catOpen.value = false
    ElMessage.success(`已应用 models.dev 参考价（${entry.Provider}/${entry.ModelID}）到成本定价`)
  } finally {
    catApplying.value = false
  }
}

// 当前行切换：批量配置表格选中某行后，更新真实的 activeRow 引用
function onSelectRow(index: number) {
  activeRowIndex.value = index
  activeRow.value = pendingRows.value[index] || null
}

// el-table current-change：通过对象引用反查行号
function onRowChange(row: PendingRow | null) {
  if (!row) return
  const idx = pendingRows.value.findIndex((r) => r === row)
  if (idx >= 0) onSelectRow(idx)
}

// 选中行高亮 class
function pendingRowClass({ rowIndex }: { rowIndex: number }) {
  return rowIndex === activeRowIndex.value ? 'active-pricing-row' : ''
}

// 关闭抽屉：清空批量待配置行，恢复正常编辑态
function fallbackClose() {
  if (pendingRows.value.length) {
    pendingRows.value = []
    activeRow.value = null
    activeRowIndex.value = 0
  }
  drawerOpen.value = false
}

// 移除某行待配置模型
function onRemoveRow(index: number) {
  pendingRows.value.splice(index, 1)
  if (!pendingRows.value.length) {
    activeRow.value = null
    activeRowIndex.value = 0
    return
  }
  const next = Math.min(activeRowIndex.value, pendingRows.value.length - 1)
  onSelectRow(next)
}
</script>

<template>
  <div class="cm-block">
    <div class="cm-head">
      <span class="cm-title">模型</span>
      <el-tooltip :content="'该渠道实际可被调度的上游模型。每条绑定一个对外模型并设置成本定价，用于成本侧计费与对外售价比对'" placement="top">
        <span class="cm-tip">?</span>
      </el-tooltip>
      <div class="cm-toolbar" v-if="!readonly">
        <el-button size="small" :icon="RefreshRight" :loading="pullLoading" @click="onPull">从渠道拉取模型</el-button>
        <el-button size="small" type="primary" :icon="Plus" @click="openCreate">新增内部模型</el-button>
      </div>
    </div>

    <div v-loading="loading">
      <el-table :data="list" border stripe class="table-nowrap">
        <el-table-column label="内部模型 ID" prop="InternalModelID" min-width="150" show-overflow-tooltip>
          <template #header>
            <el-tooltip :content="'上游模型名字（模型请求体中的 model 字段）。这个渠道收到该模型的调用时，使用此条的绑定与成本计费'" placement="top">
              <span>内部模型 ID</span>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="绑定对外模型" min-width="150">
          <template #default="{ row }">
            <el-tooltip :content="row.ExternalName ? `开发者调用「${row.ExternalName}」时，可能路由到本渠道并复用该成本` : '未绑定'" placement="top">
              <span>{{ row.ExternalName || '-' }}</span>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="成本·输入" width="110" align="right">
          <template #default="{ row }">
            <el-tooltip :content="costDetail(row)" placement="top">
              <span class="rate-cell">{{ fmtCost(row, 'input') }}</span>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="成本·输出" width="110" align="right">
          <template #default="{ row }">
            <el-tooltip :content="costDetail(row)" placement="top">
              <span class="rate-cell">{{ fmtCost(row, 'output') }}</span>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="成本·缓存读" width="120" align="right">
          <template #default="{ row }">
            <el-tooltip :content="costDetail(row)" placement="top">
              <span class="rate-cell">{{ fmtCost(row, 'cache_read') }}</span>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="100" align="center">
          <template #default="{ row }">
            <el-popover v-if="!readonly" trigger="hover" placement="left" :width="170">
              <template #reference>
                <StatusTag :value="row.State" style="cursor: pointer" />
              </template>
              <div class="pop-actions">
                <el-button v-if="row.State !== 'NORMAL'" size="small" type="success" @click="onToggleState(row, 'normal')">正常</el-button>
                <el-button v-if="row.State !== 'DRAIN'" size="small" type="warning" @click="onToggleState(row, 'drain')">排空</el-button>
                <el-button v-if="row.State !== 'DISABLED'" size="small" type="danger" @click="onToggleState(row, 'disable')">禁用</el-button>
              </div>
            </el-popover>
            <StatusTag v-else :value="row.State" />
          </template>
        </el-table-column>
        <el-table-column v-if="!readonly" label="操作" width="110" align="right">
          <template #default="{ row }">
            <div class="op-cell" @click.stop>
              <el-tooltip content="编辑" placement="top">
                <el-icon class="op-icon" @click="openEdit(row)"><Edit /></el-icon>
              </el-tooltip>
              <el-tooltip content="删除" placement="top">
                <el-icon class="op-icon danger" @click="onDelete(row)"><Delete /></el-icon>
              </el-tooltip>
            </div>
          </template>
        </el-table-column>
      </el-table>
      <el-empty v-if="!loading && !list.length" description="该渠道暂无内部模型" :image-size="48" />
      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
    </div>

    <!-- 新增/编辑/批量配置 内部模型 抽屉 -->
    <el-drawer
      v-model="drawerOpen"
      :title="pendingRows.length ? `批量配置内部模型（${pendingRows.length}）` : editingId == null ? '新增内部模型' : '编辑内部模型'"
      :size="820"
      destroy-on-close
    >
      <template v-if="pendingRows.length">
        <!-- 批量模式：每个勾选的上游模型一行，单独绑定对外模型；点击行配置其成本 -->
        <div class="batch-tip">
          已勾选 {{ pendingRows.length }} 个上游模型。请为每个模型单独绑定对外模型，并点击行选中后在下方面板配置该模型成本，最后统一保存。
        </div>
        <el-table
          :data="pendingRows"
          border
          max-height="280"
          highlight-current-row
          :row-class-name="pendingRowClass"
          @current-change="onRowChange"
        >
          <el-table-column type="index" width="44" align="center" />
          <el-table-column label="内部模型 ID" prop="InternalModelID" min-width="150" show-overflow-tooltip />
          <el-table-column label="绑定对外模型" width="220">
            <template #default="{ row }">
              <el-select v-model="row.ExternalModelID" style="width: 100%" placeholder="选择对外的发售模型">
                <el-option
                  v-for="em in externalModels"
                  :key="em.ID"
                  :label="em.ExternalName"
                  :value="em.ID"
                />
              </el-select>
            </template>
          </el-table-column>
          <el-table-column label="移除" width="64" align="center">
            <template #default="{ $index }">
              <el-button text type="danger" :icon="Delete" @click.stop="onRemoveRow($index)" />
            </template>
          </el-table-column>
        </el-table>
        <div v-if="activeRow" class="batch-pricing">
          <div class="batch-pricing-label">成本定价 — {{ activeRow.InternalModelID }}</div>
          <ModelPricing v-model="activeRow.pricing" show-rates-key="CostRates" title="成本">
            <template #title-extra>
              <el-button size="small" @click="onOpenCatalog">从 models.dev 获取参考价</el-button>
            </template>
          </ModelPricing>
        </div>
      </template>

      <el-form v-else label-width="110px" class="cm-form">
        <el-form-item label="内部模型 ID" required>
          <el-tooltip :content="'上游模型名（请求体 model 字段）。同一渠道内不可重复'" placement="top">
            <el-input v-model="editingForm!.InternalModelID" placeholder="如 deepseek-chat" />
          </el-tooltip>
        </el-form-item>
        <el-form-item label="绑定对外模型" required>
          <el-select v-model="editingForm!.ExternalModelID" style="width: 100%" placeholder="选择对外的发售模型">
            <el-option
              v-for="em in externalModels"
              :key="em.ID"
              :label="em.ExternalName"
              :value="em.ID"
            />
          </el-select>
        </el-form-item>
        <div class="span-2">
          <ModelPricing v-model="editingPricing" show-rates-key="CostRates" title="成本">
            <template #title-extra>
              <el-button size="small" @click="onOpenCatalog">从 models.dev 获取参考价</el-button>
            </template>
          </ModelPricing>
        </div>

        <div class="span-2">
          <el-collapse v-model="runtimeExpanded" class="runtime-collapse">
            <el-collapse-item name="runtime" title="高级：该模型独立运行参数（可选，默认继承渠道配置）">
              <div class="rt-hint">数字 0 或留空 = 继承渠道级配置</div>
              <div class="model-runtime">
                <div class="runtime-col">
                  <div class="runtime-title">限流</div>
                  <div class="rt-item">
                    <div class="rt-label">每分钟请求数</div>
                    <el-input-number v-model="editingForm!.RateLimit.RPM" :min="0" size="small" controls-position="right" class="rt-ctrl" />
                  </div>
                  <div class="rt-item">
                    <div class="rt-label">每分钟 Token 数</div>
                    <el-input-number v-model="editingForm!.RateLimit.TPM" :min="0" size="small" controls-position="right" class="rt-ctrl" />
                  </div>
                  <div class="rt-item">
                    <div class="rt-label">最大并发会话数</div>
                    <el-input-number v-model="editingForm!.RateLimit.MaxConcurrent" :min="0" size="small" controls-position="right" class="rt-ctrl" />
                  </div>
                </div>
                <div class="runtime-col">
                  <div class="runtime-title">健康探测</div>
                  <div class="rt-item">
                    <div class="rt-label">排空间隔(秒)</div>
                    <el-input-number v-model="editingForm!.HealthProbe.DrainIntervalSeconds" :min="0" size="small" controls-position="right" class="rt-ctrl" />
                  </div>
                  <div class="rt-item">
                    <div class="rt-label">连续失败阈值(次)</div>
                    <el-input-number v-model="editingForm!.HealthProbe.FailThreshold" :min="0" size="small" controls-position="right" class="rt-ctrl" />
                  </div>
                  <div class="rt-item">
                    <div class="rt-label">探测模型(可空)</div>
                    <el-input v-model="editingForm!.HealthProbe.ProbeModel" size="small" class="rt-ctrl" placeholder="留空继承渠道级" />
                  </div>
                </div>
                <div class="runtime-col">
                  <div class="runtime-title">可靠性</div>
                  <div class="rt-item">
                    <div class="rt-label">统计窗口(秒)</div>
                    <el-input-number v-model="editingForm!.Reliability.WindowSeconds" :min="0" size="small" controls-position="right" class="rt-ctrl" />
                  </div>
                  <div class="rt-item">
                    <div class="rt-label">错误率阈值(%)</div>
                    <el-input-number v-model="editingForm!.Reliability.ErrorRatePct" :min="0" size="small" controls-position="right" class="rt-ctrl" />
                  </div>
                  <div class="rt-item">
                    <div class="rt-label">P99延迟阈值(毫秒)</div>
                    <el-input-number v-model="editingForm!.Reliability.P99LatencyMS" :min="0" size="small" controls-position="right" class="rt-ctrl" />
                  </div>
                </div>
              </div>
            </el-collapse-item>
          </el-collapse>
        </div>
      </el-form>
      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
      <template #footer>
        <el-button @click="fallbackClose">取消</el-button>
        <el-button type="primary" :loading="saving" @click="pendingRows.length ? saveBatch() : saveModel()">
          {{ pendingRows.length ? `批量创建（${pendingRows.length}）` : '保存' }}
        </el-button>
      </template>
    </el-drawer>

    <!-- 拉取上游模型 弹窗 -->
    <el-dialog v-model="pullDialogOpen" title="从渠道拉取模型" width="520px">
      <p class="pull-tip">
        返回该渠道上游可用的模型列表，勾选后逐行配置绑定对外模型与成本，统一加入本渠道。已添加的模型不可重复加入。
      </p>
      <el-table :data="pulled" border max-height="360" @selection-change="(rows: PullModel[]) => (pullChecked = rows)">
        <el-table-column
          type="selection"
          width="40"
          :selectable="(row: PullModel) => !isAdded(row.ID)"
        />
        <el-table-column label="模型 ID" min-width="160" show-overflow-tooltip>
          <template #default="{ row }">
            <span :class="{ 'muted-id': isAdded(row.ID) }">{{ row.ID }}</span>
            <el-tag v-if="isAdded(row.ID)" size="small" type="info" effect="plain" class="added-tag">已添加</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="类型" prop="Object" width="110" />
        <el-table-column label="归属" prop="OwnedBy" min-width="120" show-overflow-tooltip />
      </el-table>
      <el-empty v-if="!pulled.length" description="上游未返回任何模型" />
      <template #footer>
        <el-button @click="pullDialogOpen = false">取消</el-button>
        <el-button
          type="primary"
          :disabled="!pullChecked.length"
          @click="onAddPulled"
        >
          加入内部模型（{{ pullChecked.length }}）
        </el-button>
      </template>
    </el-dialog>

    <!-- models.dev 参考价市场（成本定价取数） -->
    <el-dialog v-model="catOpen" title="models.dev 参考价市场" width="920px" top="6vh">
      <div class="cat-toolbar">
        <el-input
          v-model="catQ"
          placeholder="搜索模型 ID 或供应商，如 deepseek / gpt-4o"
          clearable
          style="width: 320px"
          @keyup.enter="onCatalogSearch"
          @clear="onCatalogSearch"
        />
        <el-button type="primary" :loading="catLoading" @click="onCatalogSearch">搜索</el-button>
        <el-button :loading="catSyncing" plain @click="onSyncNow">立即同步 models.dev</el-button>
        <span class="cat-hint">同一模型在不同供应商下可能有不同的模型 ID 与价格，请选择与你渠道对应的供应商。</span>
      </div>

      <el-table
        v-loading="catLoading"
        :data="catList"
        border
        max-height="400"
        highlight-current-row
        @current-change="(row: CatalogEntry | null) => (selectedEntry = row)"
      >
        <el-table-column label="供应商" min-width="100" show-overflow-tooltip>
          <template #default="{ row }">
            <span>{{ row.Provider }}</span>
            <el-tag
              v-if="modelNamePrefix && row.Provider.toLowerCase() === modelNamePrefix.toLowerCase()"
              size="small"
              type="primary"
              effect="light"
              class="offi-tag"
            >
              官方
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="模型 ID" min-width="130" show-overflow-tooltip>
          <template #default="{ row }">
            <span>{{ row.ModelID }}</span>
            <el-tag
              v-if="currentModelId && row.ModelID === currentModelId"
              size="small"
              type="success"
              effect="light"
              class="rec-tag"
            >
              推荐
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="输入" width="130" align="right">
          <template #default="{ row }">
            <div class="pcell">
              <span class="cny">{{ usdToCnyPerM(row.InputUSD) }}</span>
              <span class="credit">≈ {{ usdToCredits(row.InputUSD) }} 积分</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="输出" width="130" align="right">
          <template #default="{ row }">
            <div class="pcell">
              <span class="cny">{{ usdToCnyPerM(row.OutputUSD) }}</span>
              <span class="credit">≈ {{ usdToCredits(row.OutputUSD) }} 积分</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="缓存读" width="130" align="right">
          <template #default="{ row }">
            <div v-if="row.CacheReadUSD > 0" class="pcell">
              <span class="cny">{{ usdToCnyPerM(row.CacheReadUSD) }}</span>
              <span class="credit">≈ {{ usdToCredits(row.CacheReadUSD) }} 积分</span>
            </div>
            <span v-else class="zero">—</span>
          </template>
        </el-table-column>
        <el-table-column label="缓存写" width="130" align="right">
          <template #default="{ row }">
            <div v-if="row.CacheWriteUSD > 0" class="pcell">
              <span class="cny">{{ usdToCnyPerM(row.CacheWriteUSD) }}</span>
              <span class="credit">≈ {{ usdToCredits(row.CacheWriteUSD) }} 积分</span>
            </div>
            <span v-else class="zero">—</span>
          </template>
        </el-table-column>
        <el-table-column label="推理" width="130" align="right">
          <template #default="{ row }">
            <div v-if="row.ReasoningUSD > 0" class="pcell">
              <span class="cny">{{ usdToCnyPerM(row.ReasoningUSD) }}</span>
              <span class="credit">≈ {{ usdToCredits(row.ReasoningUSD) }} 积分</span>
            </div>
            <span v-else class="zero">—</span>
          </template>
        </el-table-column>
      </el-table>
      <div class="cat-status">
        <span v-if="catUpdatedAt" class="status-item">数据同步于 {{ fmtDateTime(catUpdatedAt) }}</span>
        <span v-else class="status-item">价格数据尚未同步（服务启动或每 60 分钟拉取一次）</span>
        <span v-if="catTotal" class="status-item">命中 {{ catTotal }} 条</span>
      </div>
      <el-empty v-if="!catLoading && !catList.length" description="未找到匹配的模型价格（同步数据来自后端定时拉取的 models.dev 缓存）" />
      <p class="ref-note">
        models.dev 提供各供应商参考价（模型 ID / 输入输出 / 缓存读写 / 推理）。表中数值为折算后的倍率建议值，点击「应用到成本」将其写入成本对应价格段（models.dev 未提供的段保留原值）。
      </p>
      <template #footer>
        <el-button @click="catOpen = false">取消</el-button>
        <el-button type="primary" :loading="catApplying" @click="onApplyCostCatalog">
          应用到成本{{ selectedEntry ? `（${selectedEntry.Provider}/${selectedEntry.ModelID}）` : '' }}
        </el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.cm-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
}
.cm-title {
  font-size: 15px;
  font-weight: 600;
}
.cm-tip {
  width: 16px;
  height: 16px;
  line-height: 16px;
  text-align: center;
  border-radius: 50%;
  background: #eef2fb;
  color: #4a7bd9;
  font-size: 11px;
  cursor: help;
}
.cm-toolbar {
  margin-left: auto;
  display: flex;
  gap: 8px;
}
.rate-cell {
  cursor: help;
  font-variant-numeric: tabular-nums;
  color: var(--color-text);
}
.pull-tip {
  font-size: 12px;
  color: var(--color-text-secondary);
  margin-bottom: 12px;
}
.muted-id {
  color: var(--color-text-tertiary);
  text-decoration: line-through;
}
.added-tag {
  margin-left: 8px;
}
.batch-tip {
  background: #f6f8ff;
  border: 1px dashed #a5c0f7;
  color: #4a7bd9;
  font-size: 12px;
  padding: 8px 12px;
  border-radius: 6px;
  margin-bottom: 8px;
}
/* 批量配置：选中行高亮 + 成本面板 */
.batch-pricing {
  border: 1px solid #f0f0f0;
  border-radius: 8px;
  padding: 4px 16px 12px;
  margin-top: 8px;
}
.batch-pricing-label {
  font-size: 14px;
  font-weight: 600;
  margin: 10px 0 2px;
}
.pop-actions {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}
:deep(.active-pricing-row) {
  --el-table-tr-bg-color: #f6f8ff;
}
/* 抽屉内两列表单栅格 */
.cm-form {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 0 24px;
}
.cm-form .span-2 {
  grid-column: 1 / -1;
}
.runtime-collapse {
  margin-top: 4px;
  border: 1px solid var(--el-border-color-lighter, #e4e7ed);
  border-radius: 6px;
}
.runtime-collapse :deep(.el-collapse-item__header) {
  padding: 0 12px;
  font-weight: 500;
}
.runtime-collapse :deep(.el-collapse-item__wrap) {
  border-bottom: none;
}
.runtime-collapse :deep(.el-collapse-item__content) {
  padding: 8px 12px 14px;
}
.rt-hint {
  font-size: 12px;
  color: var(--color-text-secondary);
  margin-bottom: 8px;
}
.model-runtime {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 16px;
}
.runtime-col {
  display: flex;
  flex-direction: column;
  gap: 8px;
  min-width: 0;
}
.runtime-title {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text);
}
.rt-item {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.rt-label {
  font-size: 12px;
  color: var(--color-text-secondary);
  line-height: 1.3;
}
.rt-ctrl {
  width: 100%;
}
/* models.dev 参考价市场 */
.cat-toolbar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 12px;
  flex-wrap: wrap;
}
.cat-hint {
  font-size: 12px;
  color: var(--color-text-tertiary);
}
.cat-status {
  display: flex;
  gap: 16px;
  margin-top: 8px;
  font-size: 12px;
  color: var(--color-text-secondary);
}
.status-item {
  white-space: nowrap;
}
.rec-tag {
  margin-left: 8px;
}
.offi-tag {
  margin-left: 8px;
}
.credit {
  color: var(--brand-primary);
  font-size: 12px;
  font-weight: 500;
  white-space: nowrap;
}
.pcell {
  display: flex;
  flex-direction: column;
  align-items: flex-end;
  gap: 2px;
}
.cny {
  color: var(--color-text);
  font-size: 12px;
}
.zero {
  color: var(--color-text-tertiary);
  font-size: 12px;
}
.ref-note {
  font-size: 12px;
  color: var(--color-text-secondary);
  margin-top: 12px;
}
</style>