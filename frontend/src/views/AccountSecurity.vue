<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import QRCode from 'qrcode'
import { totpSetup, totpConfirm, totpDisable } from '@/api/auth'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const enabled = ref(false)
const loading = ref(false)
const setupUri = ref('')
const setupSecret = ref('')
const code = ref('')
const recoveryCodes = ref<string[]>([])
const qrDataUrl = ref('')

onMounted(async () => {
  await auth.fetchProfile()
  enabled.value = auth.totpEnabled
})

async function onSetup() {
  try {
    const { value: pwd } = await ElMessageBox.prompt('请输入当前登录口令以确认开启', '开启两步验证', {
      inputType: 'password',
      inputPlaceholder: '当前口令',
    })
    loading.value = true
    const res = await totpSetup(pwd)
    setupUri.value = res.data.otpauth_uri
    setupSecret.value = res.data.secret
    qrDataUrl.value = await QRCode.toDataURL(res.data.otpauth_uri, { width: 220, margin: 1 })
  } catch (e: any) {
    if (e === 'cancel' || e === 'close') return
    ElMessage.error(e?.message || '初始化失败')
  } finally {
    loading.value = false
  }
}

async function onConfirm() {
  if (!code.value) return
  try {
    const { value: pwd } = await ElMessageBox.prompt('请输入当前登录口令以确认启用', '确认启用', {
      inputType: 'password',
      inputPlaceholder: '当前口令',
    })
    loading.value = true
    const res = await totpConfirm(pwd, code.value)
    recoveryCodes.value = res.data.recovery_codes
    auth.setTotpEnabled(true)
    enabled.value = true
    ElMessage.success('已启用两步验证')
  } catch (e: any) {
    if (e === 'cancel' || e === 'close') return
    ElMessage.error(e?.message || '验证失败')
  } finally {
    loading.value = false
  }
}

async function onDisable() {
  try {
    const { value: pwd } = await ElMessageBox.prompt('请输入当前登录口令以确认关闭', '关闭两步验证', {
      inputType: 'password',
      inputPlaceholder: '当前口令',
    })
    const { value } = await ElMessageBox.prompt('请输入当前动态码（或恢复码）以确认关闭', '关闭两步验证', {
      inputPlaceholder: '动态码',
    })
    await totpDisable(pwd, value)
    auth.setTotpEnabled(false)
    enabled.value = false
    setupUri.value = ''
    recoveryCodes.value = []
    ElMessage.success('已关闭两步验证')
  } catch {
    /* 取消或失败 */
  }
}
</script>

<template>
  <div class="page">
    <el-card class="box">
      <template #header>账号安全 · 两步验证</template>
      <template v-if="!enabled">
        <el-button v-if="!setupUri" type="primary" :loading="loading" @click="onSetup">开启两步验证</el-button>
        <div v-else>
          <img v-if="qrDataUrl" :src="qrDataUrl" alt="TOTP QR" />
          <p>手动添加密钥：<code>{{ setupSecret }}</code></p>
          <el-input v-model="code" placeholder="输入验证器中的 6 位动态码" style="max-width: 240px" />
          <el-button type="primary" :loading="loading" style="margin-left: 8px" @click="onConfirm">确认并启用</el-button>
        </div>
        <el-alert
          v-if="recoveryCodes.length"
          type="warning"
          :closable="false"
          title="请立即保存以下恢复码（仅显示一次）"
          :description="recoveryCodes.join('  ')"
        />
      </template>
      <template v-else>
        <el-alert type="success" :closable="false" title="两步验证已开启" />
        <el-button type="danger" plain style="margin-top: 12px" @click="onDisable">关闭两步验证</el-button>
      </template>
    </el-card>
  </div>
</template>

<style scoped>
.page { padding: 24px; }
.box { max-width: 560px; }
img { border-radius: 8px; margin-bottom: 8px; }
code { background: #f5f5f5; padding: 2px 6px; border-radius: 4px; }
</style>
