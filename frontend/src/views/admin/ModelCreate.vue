<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { createExternalModel } from '@/api/admin'
import ModelPricing from '@/components/ModelPricing.vue'
import ModelPriceCatalog from '@/components/ModelPriceCatalog.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

const router = useRouter()
const saving = ref(false)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

const form = reactive({
  ExternalName: '',
  Description: '',
  Enabled: true,
})
// 定价对象（售价 + 可选模型级时段/分档）
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

async function onSubmit() {
  if (!form.ExternalName.trim()) return ElMessage.warning('请填写对外模型名称')

  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    await createExternalModel({
      ExternalName: form.ExternalName.trim(),
      Description: form.Description.trim(),
      Enabled: form.Enabled,
      SaleRates: pricing.SaleRates,
      TimeConfig: pricing.TimeConfig,
      ContextTiers: pricing.ContextTiers,
    })
    ElMessage.success('对外模型已创建')
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
    <div v-loading="saving" class="card">
      <el-form label-width="110px" class="mc-grid">
        <el-form-item label="对外名称" required>
          <el-input v-model="form.ExternalName" placeholder="如 gpt-4o，作为开发者调用时的模型名" />
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
.mc-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 0 28px;
}
.mc-grid .span-2 {
  grid-column: 1 / -1;
}
@media (max-width: 900px) {
  .mc-grid {
    grid-template-columns: 1fr;
  }
}
.form-actions {
  margin-top: 16px;
  border-top: 1px solid #f0f0f0;
  padding-top: 16px;
}
</style>