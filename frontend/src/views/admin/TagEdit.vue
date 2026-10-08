<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { listTags, updateTag, type AdminTag } from '@/api/admin'
import KvpEditor from '@/components/KvpEditor.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

const route = useRoute()
const router = useRouter()
const id = String(route.params.id)

const loading = ref(false)
const saving = ref(false)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

const form = reactive({
  name: '',
  description: '',
  kv_pairs: {} as Record<string, string>,
  enabled: true,
})

onMounted(async () => {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await listTags()
    const row = (res.Data || []).find((t: AdminTag) => t.ID === id)
    if (!row) {
      ElMessage.error('标签不存在')
      router.push('/admin/tags')
      return
    }
    form.name = row.Name
    form.description = row.Description
    form.kv_pairs = { ...(row.KVPairs || {}) }
    form.enabled = row.Enabled
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
})

async function onSubmit() {
  if (!form.name.trim()) return ElMessage.warning('请填写标签名称')

  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    await updateTag(id, {
      Name: form.name.trim(),
      Description: form.description,
      KVPairs: { ...form.kv_pairs },
      Enabled: form.enabled,
    })
    ElMessage.success('标签已更新')
    router.push('/admin/tags')
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="page-container">
    <div v-loading="loading" class="card" style="max-width: 640px">
      <el-form label-width="110px">
        <el-form-item label="名称" required>
          <el-input v-model="form.name" maxlength="64" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="form.description" type="textarea" :rows="2" />
        </el-form-item>
        <el-form-item label="KV 标签">
          <KvpEditor v-model="form.kv_pairs" />
        </el-form-item>
        <el-form-item label="启用"><el-switch v-model="form.enabled" /></el-form-item>
        <el-form-item>
          <el-button type="primary" :loading="saving" @click="onSubmit">保存</el-button>
          <el-button @click="router.push('/admin/tags')">取消</el-button>
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