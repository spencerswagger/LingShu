<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import {
  listExternalModels,
  updateExternalModel,
  type ExternalModel,
} from '@/api/admin'
import ModelPricing from '@/components/ModelPricing.vue'
import ModelPriceCatalog from '@/components/ModelPriceCatalog.vue'
import StringListEditor from '@/components/StringListEditor.vue'
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
  // 别名：请求命中任一别名即解析到本模型（不出现在 /v1/models）
  Aliases: [] as string[],
})
const pricing = reactive<Record<string, any>>({
  SaleRates: { Input: 0, Output: 0, CacheRead: 0 },
  TimeConfig: null,
  ContextTiers: null,
})

// models.dev 参考价市场（公共组件）
const catalogRef = ref<InstanceType<typeof ModelPriceCatalog> | null>(null)
// 只合并 models.dev 提供的价段，未提供的段保留原值
function onApplyCatalog(partial: Record<string, number>) {
  pricing.SaleRates = { ...pricing.SaleRates, ...partial }
}

// ---- 「从内部模型同步定价」能力已下线 ----
// 原实现读 `channel_models.CostRates`（渠道族成本层，0024 已删表、接口 410）。
// V2 的成本口径只有 ledger / amortized / none，不再有「按渠道成本同步售价」这一套；
// 售价维护改为：手填 / 「从 models.dev 获取参考价」两条路。

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
    form.Aliases = row.Aliases ? [...row.Aliases] : []
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
      Aliases: form.Aliases,
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

        <el-form-item label="别名" class="span-2">
          <StringListEditor
            v-model="form.Aliases"
            placeholder="输入别名后回车（仅用于模型名解析，不出现在 /v1/models）"
          />
        </el-form-item>

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
