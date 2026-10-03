<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { changeOwnPassword } from '@/api/auth'
import { useAuthStore } from '@/stores/auth'

const router = useRouter()
const auth = useAuthStore()
const formRef = ref()
const loading = ref(false)
const form = reactive({ oldPassword: '', newPassword: '', confirm: '' })

const rules = {
  oldPassword: [{ required: true, message: '请输入当前密码', trigger: 'blur' }],
  newPassword: [
    { required: true, message: '请输入新密码', trigger: 'blur' },
    { min: 8, message: '密码至少 8 个字符', trigger: 'blur' },
  ],
  confirm: [
    { required: true, message: '请再次输入新密码', trigger: 'blur' },
    {
      validator: (_: unknown, v: string, cb: (e?: Error) => void) =>
        v === form.newPassword ? cb() : cb(new Error('两次输入的密码不一致')),
      trigger: 'blur',
    },
  ],
}

async function onSubmit() {
  await formRef.value.validate().catch(() => Promise.reject())
  loading.value = true
  try {
    const res = await changeOwnPassword(form.oldPassword, form.newPassword)
    auth.setAuth(res.data.token, auth.role, auth.username, false, auth.totpEnabled)
    auth.setMustChange(false)
    ElMessage.success('密码修改成功')
    router.push(auth.role === 'ADMIN' ? '/admin' : '/dev')
  } catch (e: any) {
    ElMessage.error(e?.message || '修改失败')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="page">
    <el-card class="box">
      <template #header>修改密码</template>
      <el-form ref="formRef" :model="form" :rules="rules" label-width="100px" style="max-width: 420px">
        <el-form-item label="当前密码" prop="oldPassword">
          <el-input v-model="form.oldPassword" type="password" show-password />
        </el-form-item>
        <el-form-item label="新密码" prop="newPassword">
          <el-input v-model="form.newPassword" type="password" show-password placeholder="至少 8 位，含两类字符" />
        </el-form-item>
        <el-form-item label="确认新密码" prop="confirm">
          <el-input v-model="form.confirm" type="password" show-password />
        </el-form-item>
        <el-form-item>
          <el-button type="primary" :loading="loading" @click="onSubmit">确认修改</el-button>
        </el-form-item>
      </el-form>
    </el-card>
  </div>
</template>

<style scoped>
.page { padding: 24px; }
.box { max-width: 560px; }
</style>
