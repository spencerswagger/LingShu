<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { createTag } from '@/api/admin'
import KvpEditor from '@/components/KvpEditor.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

const router = useRouter()
const saving = ref(false)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

const form = reactive({
  name: '',
  description: '',
  kv_pairs: {} as Record<string, string>,
  enabled: true,
})

async function onSubmit() {
  if (!form.name.trim()) return ElMessage.warning('请填写标签名称')

  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    await createTag({
      Name: form.name.trim(),
      Description: form.description,
      KVPairs: { ...form.kv_pairs },
      Enabled: form.enabled,
    })
    ElMessage.success('标签已创建')
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
    <div v-loading="saving" class="card" style="max-width: 640px">
      <el-form label-width="110px">
        <el-form-item label="名称" required>
          <el-input v-model="form.name" placeholder="如 金融 / 长上下文" maxlength="64" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="form.description" type="textarea" :rows="2" placeholder="选填" />
        </el-form-item>
        <el-form-item label="KV 标签">
          <KvpEditor v-model="form.kv_pairs" key-placeholder="键" value-placeholder="值" />
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