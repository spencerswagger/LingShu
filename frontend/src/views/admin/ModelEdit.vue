<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import {
  listExternalModels,
  updateExternalModel,
  priceCatalog,
  getBillingConfig,
  runSync,
  type CatalogEntry,
  type ExternalModel,
} from '@/api/admin'
import ModelPricing from '@/components/ModelPricing.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'
import { fmtDateTime } from '@/utils/format'

const route = useRoute()
const router = useRouter()
const id = String(route.params.id)

const loading = ref(false)
const saving = ref(false)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

const form = reactive({
  ExternalName: '',
  Description: '',
  Enabled: true,
})
const pricing = reactive<Record<string, any>>({
  SaleRates: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0 },
  TimeConfig: null,
  ContextTiers: null,
})

// models.dev 参考价市场（多供应商目录）
const catOpen = ref(false)
const catQ = ref('')
const catLoading = ref(false)
const catList = ref<CatalogEntry[]>([])
const catUpdatedAt = ref('') // 最近同步时间
const catTotal = ref(0) // 当前搜索命中总数
const catR = ref(10000) // 积分换算底数，用于参考价→积分折算
const catApplying = ref(false)

// 推荐条目：与当前对外名称完全一致且供应商匹配前缀
const recommended = computed(() => {
  const name = form.ExternalName
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
    if (best == null || e.Provider < best.Provider) {
      best = e
    }
  }
  return best || catList.value[0] || null
})
const selectedEntry = ref<CatalogEntry | null>(null)

// 对外名称前缀（'-' 之前），用于标记「官方」供应商行（如 deepseek-flash → deepseek）
const modelNamePrefix = computed(() => {
  const name = form.ExternalName.trim()
  const dash = name.indexOf('-')
  return dash > 0 ? name.slice(0, dash) : name
})

// USD/CNY 汇率（系统设置中维护）——models.dev 参考价 → 倍率折算用
const catCnyRate = ref(6.8)

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

// 打开 models.dev 参考价市场：加载 R 并按当前模型名预搜索
async function onOpenCatalog() {
  catOpen.value = true
  catQ.value = form.ExternalName
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
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await priceCatalog(catQ.value)
    catList.value = res.data.list || []
    catUpdatedAt.value = res.data.updated_at || ''
    catTotal.value = res.data.total || 0
    selectedEntry.value = null
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    catLoading.value = false
  }
}

// 立即触发一次 models.dev 同步，成功后重搜当前关键词
const catSyncing = ref(false)
async function onSyncNow() {
  catSyncing.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await runSync()
    ElMessage.success(res.data.summary)
    await onCatalogSearch()
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    catSyncing.value = false
  }
}

// 应用所选（或推荐）条目为售价五段费率
async function onApplyCatalog() {
  const entry = selectedEntry.value || recommended.value
  if (!entry) return ElMessage.warning('目录为空或未找到参考价')
  catApplying.value = true
  try {
    const next: Record<string, number> = { ...pricing.SaleRates }
    // 只有 models.dev 提供的价格段才覆盖，无价段保留原值（避免清空用户已填的缓存写/推理费率）
    const apply = (k: string, usd: number) => {
      if (usd > 0) next[k] = usdToCnyPerM(usd)
    }
    apply('input', entry.InputUSD)
    apply('output', entry.OutputUSD)
    apply('cache_read', entry.CacheReadUSD)
    apply('cache_write', entry.CacheWriteUSD)
    apply('reasoning', entry.ReasoningUSD)
    pricing.SaleRates = next
    catOpen.value = false
    ElMessage.success(`已应用 models.dev 参考价（${entry.Provider}/${entry.ModelID}）到售价可用的价格段`)
  } finally {
    catApplying.value = false
  }
}

onMounted(async () => {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await listExternalModels()
    const row = (res.data || []).find((m: ExternalModel) => m.ID === id)
    if (!row) {
      ElMessage.error('对外模型不存在')
      router.push('/admin/models')
      return
    }
    form.ExternalName = row.ExternalName
    form.Description = row.Description
    form.Enabled = row.Enabled
    pricing.SaleRates = { ...(row.SaleRates || {}) }
    pricing.TimeConfig = row.TimeConfig
    pricing.ContextTiers = row.ContextTiers
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
})

async function onSubmit() {
  if (!form.ExternalName.trim()) return ElMessage.warning('请填写对外模型名称')

  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    await updateExternalModel(id, {
      ExternalName: form.ExternalName.trim(),
      Description: form.Description.trim(),
      Enabled: form.Enabled,
      SaleRates: pricing.SaleRates,
      TimeConfig: pricing.TimeConfig,
      ContextTiers: pricing.ContextTiers,
    })
    ElMessage.success('对外模型已更新')
    router.push('/admin/models')
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="page-container">
    <div v-loading="loading" class="card" style="max-width: 1080px">
      <el-form label-width="110px" class="me-grid">
        <el-form-item label="对外名称" required>
          <el-input v-model="form.ExternalName" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="form.Description" placeholder="用途说明（可选）" />
        </el-form-item>
        <el-form-item label="启用"><el-switch v-model="form.Enabled" /></el-form-item>

        <div class="span-2">
          <ModelPricing
            :model-value="pricing"
            show-rates-key="SaleRates"
            title="售价"
            @update:model-value="Object.assign(pricing, $event)"
          >
            <template #title-extra>
              <el-button size="small" @click="onOpenCatalog">
                从 models.dev 获取参考价
              </el-button>
            </template>
          </ModelPricing>
        </div>

        <el-form-item class="span-2 form-actions">
          <el-button type="primary" :loading="saving" @click="onSubmit">保存</el-button>
          <el-button @click="router.push('/admin/models')">取消</el-button>
        </el-form-item>
      </el-form>

      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
    </div>

    <!-- models.dev 参考价市场（供应商维度搜索） -->
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
              v-if="row.ModelID === form.ExternalName"
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
        models.dev 提供各供应商参考价（模型 ID / 输入输出 / 缓存读写 / 推理）。表中数值为折算后的倍率建议值，点击「应用」将其写入售价对应价格段（models.dev 未提供的段保留原值）。
      </p>
      <template #footer>
        <el-button @click="catOpen = false">取消</el-button>
        <el-button type="primary" :loading="catApplying" @click="onApplyCatalog">
          应用为售价{{ selectedEntry ? `（${selectedEntry.Provider}/${selectedEntry.ModelID}）` : '' }}
        </el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
/* 两列表单栅格 */
.me-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 0 28px;
}
.me-grid .span-2 {
  grid-column: 1 / -1;
}
@media (max-width: 900px) {
  .me-grid {
    grid-template-columns: 1fr;
  }
}
.form-actions {
  margin-top: 16px;
  border-top: 1px solid #f0f0f0;
  padding-top: 16px;
}
.ref-note {
  font-size: 12px;
  color: var(--color-text-secondary);
  margin-top: 12px;
}
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
.mono {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
  margin-right: 6px;
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
.rec-tag {
  margin-left: 8px;
}
.offi-tag {
  margin-left: 8px;
}
.zero {
  color: var(--color-text-tertiary);
  font-size: 12px;
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
</style>