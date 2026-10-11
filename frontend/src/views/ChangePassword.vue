<script setup lang="ts">
import { useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import ChangePasswordForm from '@/components/account/ChangePasswordForm.vue'

const router = useRouter()
const auth = useAuthStore()

function onSuccess() {
  router.push(auth.role === 'ADMIN' ? '/admin' : '/dev')
}
</script>

<template>
  <div class="page">
    <el-card class="box">
      <template #header>修改密码</template>
      <ChangePasswordForm @success="onSuccess" />
    </el-card>
  </div>
</template>

<style scoped>
.page { padding: 24px; }
.box { max-width: 560px; }
</style>
