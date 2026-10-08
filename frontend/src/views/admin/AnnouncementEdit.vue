<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import {
  listAnnouncements,
  createAnnouncement,
  updateAnnouncement,
  type AdminAnnouncement,
} from '@/api/admin'
import ErrorBubble from '@/components/ErrorBubble.vue'

const route = useRoute()
const router = useRouter()
// :id 存在则为编辑，否则为新建
const editId = route.params.id ? String(route.params.id) : null

const loading = ref(false)
const saving = ref(false)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

const form = reactive({
  title: '',
  content: '',
  level: 'info',
  publish_at: null as string | null,
  expire_at: null as string | null,
  enabled: true,
})

onMounted(async () => {
  if (!editId) return
  loading.value = true
  try {
    const res = await listAnnouncements()
    const row = (res.Data.List || []).find((a: AdminAnnouncement) => a.ID === editId)
    if (!row) {
      ElMessage.error('公告不存在')
      router.push('/admin/announcements')
      return
    }
    form.title = row.Title
    form.content = row.Content
    form.level = row.Level
    form.publish_at = row.PublishAt || null
    form.expire_at = row.ExpireAt || null
    form.enabled = row.Enabled
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
})

async function onSubmit() {
  if (!form.title.trim()) return ElMessage.warning('请填写标题')
  if (!form.content.trim()) return ElMessage.warning('请填写公告内容')

  const payload = {
    Title: form.title.trim(),
    Content: form.content,
    Level: form.level,
    PublishAt: form.publish_at,
    ExpireAt: form.expire_at,
    Enabled: form.enabled,
  }
  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    if (editId) {
      await updateAnnouncement(editId, payload)
    } else {
      await createAnnouncement(payload)
    }
    ElMessage.success(editId ? '公告已更新' : '公告已创建')
    router.push('/admin/announcements')
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="page-container">
    <div v-loading="loading" class="card me-edit">
      <el-form label-width="110px" class="me-grid">
        <el-form-item label="标题" required class="span-2">
          <el-input v-model="form.title" maxlength="128" />
        </el-form-item>
        <el-form-item label="内容" required class="span-2">
          <el-input v-model="form.content" type="textarea" :rows="6" />
        </el-form-item>
        <el-form-item label="级别">
          <el-radio-group v-model="form.level">
            <el-radio-button label="info">Info</el-radio-button>
            <el-radio-button label="warning">Warning</el-radio-button>
            <el-radio-button label="danger">Danger</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="启用"><el-switch v-model="form.enabled" /></el-form-item>
        <el-form-item label="发布时间">
          <el-date-picker
            v-model="form.publish_at"
            type="datetime"
            value-format="YYYY-MM-DDTHH:mm:ssZ"
            placeholder="留空 = 立即发布"
            style="width: 100%"
          />
        </el-form-item>
        <el-form-item label="过期时间">
          <el-date-picker
            v-model="form.expire_at"
            type="datetime"
            value-format="YYYY-MM-DDTHH:mm:ssZ"
            placeholder="留空 = 永不过期"
            style="width: 100%"
          />
        </el-form-item>
        <el-form-item class="span-2">
          <el-button type="primary" :loading="saving" @click="onSubmit">保存</el-button>
          <el-button @click="router.push('/admin/announcements')">取消</el-button>
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