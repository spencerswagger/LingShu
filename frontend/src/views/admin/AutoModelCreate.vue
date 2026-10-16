<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { listExternalModels, createAutoModel, type ExternalModel } from '@/api/admin'
import AutoModelMembers from '@/components/AutoModelMembers.vue'
import UserSelect from '@/components/UserSelect.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'
import { useUnsavedGuard } from '@/composables/useUnsavedGuard'

const router = useRouter()
// mode='self'：开发者端（走 /dev 前缀接口），强制私有，无归属用户选择
const props = withDefaults(defineProps<{ mode?: 'admin' | 'self' }>(), { mode: 'admin' })
const isSelf = computed(() => props.mode === 'self')
const base = computed(() => (isSelf.value ? '/dev' : '/admin'))
const listPath = computed(() => (isSelf.value ? '/dev/auto-models' : '/admin/auto-models'))

const saving = ref(false)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

const form = reactive({
  Name: '',
  Description: '',
  Enabled: true,
  Scope: isSelf.value ? 'private' : 'global',
  OwnerUserID: null as string | null,
})

const members = ref<{ ExternalModelID: string }[]>([{ ExternalModelID: '' }])
const candidates = ref<ExternalModel[]>([])

async function loadCandidates() {
  try {
    const res = await listExternalModels({ Enabled: true })
    candidates.value = res.Data || []
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  }
}

// ===== 未保存修改守卫 =====
function snapshot(): string {
  return JSON.stringify({
    name: form.Name,
    description: form.Description,
    enabled: form.Enabled,
    scope: form.Scope,
    owner: form.OwnerUserID,
    members: members.value,
  })
}
const { markSaved } = useUnsavedGuard(snapshot)
markSaved() // 新建：以空表单为基线

onMounted(loadCandidates)

function validate(): string | null {
  if (!form.Name.trim()) return '请填写 Auto 模型名称'
  if (!isSelf.value && form.Scope === 'private' && !form.OwnerUserID) return '私有 Auto 必须指定归属用户'
  if (!members.value.length) return '请至少添加一个成员'
  const ids = members.value.map((m) => String(m.ExternalModelID || ''))
  if (ids.some((id) => !id)) return '存在未选择的成员'
  if (new Set(ids).size !== ids.length) return '成员不得重复选择同一对外模型'
  return null
}

async function onSubmit() {
  const err = validate()
  if (err) return ElMessage.warning(err)

  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    await createAutoModel(
      {
        Name: form.Name.trim(),
        Scope: isSelf.value ? 'private' : form.Scope,
        OwnerUserID: !isSelf.value && form.Scope === 'private' ? form.OwnerUserID || undefined : undefined,
        Enabled: form.Enabled,
        Description: form.Description.trim(),
        Members: members.value.map((m, i) => ({ ExternalModelID: String(m.ExternalModelID), Position: i })),
      },
      base.value,
    )
    ElMessage.success('Auto 模型已创建')
    markSaved() // 创建成功后跳转，避免触发「未保存」离开确认
    router.push(listPath.value)
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="page-container">
    <div v-loading="saving" class="card me-edit">
      <el-form label-width="110px" class="me-grid">
        <el-form-item label="名称" required>
          <el-input v-model="form.Name" placeholder="如 auto，作为调用时的模型名（不得与对外模型重名）" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="form.Description" placeholder="用途说明（可选）" />
        </el-form-item>

        <el-form-item label="类型" required>
          <el-radio-group v-model="form.Scope" :disabled="isSelf">
            <el-radio-button value="global">全局</el-radio-button>
            <el-radio-button value="private">私有</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item v-if="form.Scope === 'private' && !isSelf" label="归属用户" required>
          <UserSelect v-model="form.OwnerUserID" placeholder="选择归属用户" />
        </el-form-item>

        <el-form-item label="启用"><el-switch v-model="form.Enabled" /></el-form-item>

        <div class="span-2">
          <div class="block-title">成员</div>
          <div class="block-desc">
            从「启用的对外模型」中选择成员，顺序即优先级：请求 Auto 名时按从上到下的顺序选择第一个可用的成员。
          </div>
          <AutoModelMembers v-model="members" :candidates="candidates" />
        </div>

        <el-form-item class="span-2 form-actions">
          <el-button type="primary" :loading="saving" @click="onSubmit">保存</el-button>
          <el-button @click="router.push(listPath)">取消</el-button>
        </el-form-item>
      </el-form>

      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
    </div>
  </div>
</template>

<style scoped>
.block-desc {
  font-size: 12px;
  color: var(--color-text-secondary);
  margin-bottom: 10px;
  line-height: 1.6;
}
.form-actions {
  margin-top: 16px;
  border-top: 1px solid #f0f0f0;
  padding-top: 16px;
}
</style>