<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { createUser } from '@/api/admin'
import ErrorBubble from '@/components/ErrorBubble.vue'

const router = useRouter()
const saving = ref(false)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

const form = reactive({
  username: '',
  password: '',
  nickname: '',
  role: 'DEVELOPER',
  pricing_mode: 'sale',
})

async function onSubmit() {
  if (!form.username.trim()) return ElMessage.warning('请填写用户名')
  if (!form.password) return ElMessage.warning('请填写密码')

  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    await createUser({
      Username: form.username.trim(),
      Password: form.password,
      Nickname: form.nickname.trim(),
      Role: form.role,
      PricingMode: form.pricing_mode,
    })
    ElMessage.success('用户已创建')
    router.push('/admin/users')
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="page-container">
    <div v-loading="saving" class="card" style="max-width: 560px">
      <el-form label-width="100px">
        <el-form-item label="用户名" required>
          <el-input v-model="form.username" placeholder="登录账号" maxlength="64" />
        </el-form-item>
        <el-form-item label="昵称">
          <el-input v-model="form.nickname" placeholder="选填，最多 40 字" maxlength="40" show-word-limit />
        </el-form-item>
        <el-form-item label="密码" required>
          <el-input v-model="form.password" type="password" show-password placeholder="初始登录密码" />
        </el-form-item>
        <el-form-item label="角色" required>
          <el-radio-group v-model="form.role">
            <el-radio-button label="DEVELOPER">开发者</el-radio-button>
            <el-radio-button label="ADMIN">管理员</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="计价模式">
          <!-- 口径 D-1（Spec P10）：V2 没有成本单价真源，`cost` 模式不可选。
               历史 cost 用户在列表页有明确标注；新用户一律按售价创建。 -->
          <el-radio-group v-model="form.pricing_mode">
            <el-radio-button label="sale">按售价</el-radio-button>
            <el-radio-button label="cost" disabled>按成本（已停用）</el-radio-button>
          </el-radio-group>
          <div class="field-hint">
            成本模式已停用：V2 无成本单价真源（口径 D-1），已有 cost 用户的新请求会被拒绝。
          </div>
        </el-form-item>
        <el-form-item>
          <el-button type="primary" :loading="saving" @click="onSubmit">保存</el-button>
          <el-button @click="router.push('/admin/users')">取消</el-button>
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
.field-hint {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.6;
  color: var(--color-text-tertiary);
}
</style>
