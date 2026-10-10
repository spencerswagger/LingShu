<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import {
  listExternalModels,
  updateExternalModel,
  listAllChannelModels,
  rateKeys,
  rateLabels,
  type ExternalModel,
  type ChannelModel,
} from '@/api/admin'
import ModelPricing from '@/components/ModelPricing.vue'
import ModelPriceCatalog from '@/components/ModelPriceCatalog.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

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
  SaleRates: { Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0, Reasoning: 0 },
  TimeConfig: null,
  ContextTiers: null,
})

// models.dev 参考价市场（公共组件）
const catalogRef = ref<InstanceType<typeof ModelPriceCatalog> | null>(null)
// 只合并 models.dev 提供的价段，未提供的段保留原值
function onApplyCatalog(partial: Record<string, number>) {
  pricing.SaleRates = { ...pricing.SaleRates, ...partial }
}

// ---- 从内部模型同步定价（售价 / 时段 / 分档） ----
const syncOpen = ref(false)
const syncLoading = ref(false)
const channelModels = ref<ChannelModel[]>([])
const syncOnlyBound = ref(true) // 仅显示本对外模型绑定的内部模型
const syncSourceId = ref<string | null>(null)

// 绑定到当前对外模型的内部模型
const boundItems = computed(() =>
  channelModels.value.filter((i) => String(i.ExternalModelID) === String(id)),
)
// 候选来源：优先本对外模型绑定的内部模型；该模型未绑定时回退全部内部模型
const sourceOptions = computed(() => (syncOnlyBound.value ? boundItems.value : channelModels.value))
const syncSource = computed(() => sourceOptions.value.find((i) => i.ID === syncSourceId.value) || null)

function fmtRate(v: number | undefined): string {
  return v == null ? '-' : String(Math.round(Number(v) * 1e6) / 1e6)
}

async function openSyncFromInternal() {
  syncOpen.value = true
  syncLoading.value = true
  syncSourceId.value = null
  try {
    const res = await listAllChannelModels()
    channelModels.value = res.Data || []
    // 未绑定任何内部模型时：控件置灰并自动切换为「全部内部模型」
    syncOnlyBound.value = boundItems.value.length > 0
  } catch (e: any) {
    ElMessage.error(e?.message || '加载内部模型失败')
  } finally {
    syncLoading.value = false
  }
}

function onSyncFromInternal() {
  const src = sourceOptions.value.find((i) => i.ID === syncSourceId.value)
  if (!src) return ElMessage.warning('请选择要同步的内部模型')
  pricing.SaleRates = { ...(src.CostRates || {}) }
  pricing.TimeConfig = src.TimeConfig ? JSON.parse(JSON.stringify(src.TimeConfig)) : null
  pricing.ContextTiers = src.ContextTiers ? JSON.parse(JSON.stringify(src.ContextTiers)) : null
  syncOpen.value = false
  ElMessage.success('已用内部模型定价覆盖售价、时段与分档')
}

onMounted(async () => {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await listExternalModels()
    const row = (res.Data || []).find((m: ExternalModel) => m.ID === id)
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
    <div v-loading="loading" class="card">
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
              <el-button size="small" @click="catalogRef?.open()">
                从 models.dev 获取参考价
              </el-button>
              <el-button size="small" @click="openSyncFromInternal">
                从内部模型同步定价
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

    <!-- models.dev 参考价市场（公共组件） -->
    <ModelPriceCatalog
      ref="catalogRef"
      :model-name="form.ExternalName"
      label="售价"
      @apply="onApplyCatalog"
    />

    <!-- 从内部模型同步定价：售价 / 时段 / 分档整体覆盖 -->
    <el-dialog v-model="syncOpen" title="从内部模型同步定价" width="720px">
      <div v-loading="syncLoading" class="sync-body">
        <el-alert
          v-if="!syncLoading && !boundItems.length"
          type="info"
          :closable="false"
          show-icon
          title="该对外模型尚未绑定任何内部模型，已切换为全部内部模型"
          style="margin-bottom: 12px"
        />
        <div class="sync-row">
          <el-checkbox v-model="syncOnlyBound" :disabled="!boundItems.length">
            仅显示本对外模型绑定的内部模型
          </el-checkbox>
        </div>
        <div class="sync-row">
          <span class="sync-label">同步来源</span>
          <el-select v-model="syncSourceId" filterable placeholder="选择要同步的内部模型" style="width: 100%">
            <el-option
              v-for="i in sourceOptions"
              :key="i.ID"
              :label="`${i.ChannelName || '渠道#' + i.ChannelID} / ${i.InternalModelID}`"
              :value="i.ID"
            />
          </el-select>
        </div>
        <div v-if="syncSource" class="sync-preview">
          <div class="sp-title">将要覆盖的内容预览</div>
          <div class="sp-rates">
            <span v-for="k in rateKeys" :key="k" class="sp-rate">
              {{ rateLabels[k] }}：{{ fmtRate((syncSource.CostRates || {})[k]) }}
            </span>
          </div>
          <div class="sp-meta">
            时段段数：{{ syncSource.TimeConfig?.Periodic?.length || 0 }} 段 · 分档条数：{{ syncSource.ContextTiers?.length || 0 }} 条
          </div>
        </div>
      </div>
      <template #footer>
        <el-button @click="syncOpen = false">取消</el-button>
        <el-button type="primary" @click="onSyncFromInternal">同步到售价</el-button>
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
/* 从内部模型同步定价 */
.sync-body {
  min-height: 120px;
}
.sync-row {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 12px;
}
.sync-label {
  font-size: 13px;
  color: var(--color-text-secondary);
  white-space: nowrap;
}
.sync-preview {
  border: 1px solid #f0f0f0;
  border-radius: 8px;
  padding: 10px 12px;
  background: #fafbfc;
}
.sp-title {
  font-size: 13px;
  font-weight: 600;
  margin-bottom: 8px;
}
.sp-rates {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 16px;
}
.sp-rate {
  font-size: 12px;
  color: var(--color-text-secondary);
  font-variant-numeric: tabular-nums;
}
.sp-meta {
  font-size: 12px;
  color: var(--color-text-secondary);
  margin-top: 8px;
}
</style>
