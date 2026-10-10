<script setup lang="ts">
// models.dev 参考价市场（公共组件）：按模型 ID / 供应商搜索参考价，供售价或成本定价取数。
// 组件只负责取数与选择，不直接改父组件数据：底部「应用」时把 >0 的价段换算成倍率，
// 通过 emit('apply', partial) 抛出，由父组件合并到对应的五段费率。
import { computed, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { getBillingConfig, priceCatalog, runSync, type CatalogEntry } from '@/api/admin'
import { fmtDateTime } from '@/utils/format'

const props = defineProps<{
  // 用于打开时预填搜索词 + 计算「官方」供应商前缀（'-' 之前）
  modelName: string
  // '售价' | '成本'，用于底部按钮「应用为{{label}}」文案
  label?: string
}>()
const emit = defineEmits<{ (e: 'apply', rates: Record<string, number>): void }>()

const applyLabel = computed(() => props.label || '售价')

const catOpen = ref(false)
const catQ = ref('')
const catLoading = ref(false)
const catList = ref<CatalogEntry[]>([])
const catUpdatedAt = ref('') // 最近同步时间
const catTotal = ref(0) // 当前搜索命中总数
const catR = ref(10000) // 积分换算底数，用于参考价→积分折算
const catCnyRate = ref(6.8) // USD/CNY 汇率（系统设置中维护）
const catSyncing = ref(false)
const selectedEntry = ref<CatalogEntry | null>(null)

// 推荐条目：与当前模型名完全一致且供应商匹配前缀
const recommended = computed(() => {
  const name = props.modelName
  if (!name) return null
  let prefix = name
  const dash = name.indexOf('-')
  if (dash > 0) prefix = name.slice(0, dash)
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

// 模型名前缀（'-' 之前），用于标记「官方」供应商行（如 deepseek-flash → deepseek）
const modelNamePrefix = computed(() => {
  const name = props.modelName.trim()
  const dash = name.indexOf('-')
  return dash > 0 ? name.slice(0, dash) : name
})

// models.dev 参考价 → 倍率（存储值 = 每百万 token 计费数值，汇率仅在系统设置中配置）
function usdToCnyPerM(usd: number): number {
  return Math.round(usd * catCnyRate.value * 1e6) / 1e6
}
// 倍率 → 积分/M 辅助展示（积分 = 倍率 × 1e6 ÷ R）
function cnyToCredits(rate: number): number {
  return Math.round((rate * 1e6) / catR.value * 1e6) / 1e6
}
function usdToCredits(usd: number): number {
  return cnyToCredits(usdToCnyPerM(usd))
}

// 打开参考价市场：加载 R/汇率并按当前模型名预搜索
async function openCatalog() {
  catOpen.value = true
  catQ.value = props.modelName || ''
  try {
    const res = await getBillingConfig()
    const v = Number(res.Data.R)
    if (v > 0) catR.value = v
    const c = Number(res.Data.CNYRate)
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
    catList.value = res.Data.List || []
    catUpdatedAt.value = res.Data.UpdatedAt || ''
    catTotal.value = res.Data.Total || 0
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
    ElMessage.success(res.Data.Summary)
    await onCatalogSearch()
  } catch (e: any) {
    ElMessage.error(e?.message || '同步 models.dev 失败')
  } finally {
    catSyncing.value = false
  }
}

// 应用所选（或推荐）条目为五段费率：只抛出 models.dev 提供（>0）的价段，
// 无价段不放进 partial，父组件合并时自然保留原值。
function onApplyCatalog() {
  const entry = selectedEntry.value || recommended.value
  if (!entry) return ElMessage.warning('目录为空或未找到参考价')
  const partial: Record<string, number> = {}
  const apply = (k: string, usd: number) => {
    if (usd > 0) partial[k] = usdToCnyPerM(usd)
  }
  apply('Input', entry.InputUSD)
  apply('Output', entry.OutputUSD)
  apply('CacheRead', entry.CacheReadUSD)
  apply('CacheWrite', entry.CacheWriteUSD)
  apply('Reasoning', entry.ReasoningUSD)
  emit('apply', partial)
  catOpen.value = false
  ElMessage.success(`已应用 models.dev 参考价（${entry.Provider}/${entry.ModelID}）`)
}

defineExpose({ open: openCatalog })
</script>

<template>
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
            v-if="props.modelName && row.ModelID === props.modelName"
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
      models.dev 提供各供应商参考价（模型 ID / 输入输出 / 缓存读写 / 推理）。表中数值为折算后的倍率建议值，点击「应用」将其写入{{ applyLabel }}对应的价格段（models.dev 未提供的段保留原值）。
    </p>
    <template #footer>
      <el-button @click="catOpen = false">取消</el-button>
      <el-button type="primary" @click="onApplyCatalog">
        应用为{{ applyLabel }}{{ selectedEntry ? `（${selectedEntry.Provider}/${selectedEntry.ModelID}）` : '' }}
      </el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
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
