<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import QRCode from 'qrcode'
import { totpSetup, totpConfirm, totpDisable } from '@/api/auth'
import { useAuthStore } from '@/stores/auth'

const props = defineProps<{ modelValue: boolean }>()
const emit = defineEmits<{ (e: 'update:modelValue', v: boolean): void }>()

const visible = computed({
  get: () => props.modelValue,
  set: (v) => emit('update:modelValue', v),
})

const auth = useAuthStore()
const enabled = ref(false)
const loading = ref(false)
const setupUri = ref('')
const setupSecret = ref('')
const code = ref('')
const recoveryCodes = ref<string[]>([])
const qrDataUrl = ref('')

watch(visible, async (v) => {
  if (!v) return
  enabled.value = auth.totpEnabled
  setupUri.value = ''
  setupSecret.value = ''
  code.value = ''
  recoveryCodes.value = []
  qrDataUrl.value = ''
  await auth.fetchProfile()
  enabled.value = auth.totpEnabled
})

async function onSetup() {
  try {
    const { value: pwd } = await ElMessageBox.prompt('请输入当前登录口令以确认开启', '开启多因素认证', {
      inputType: 'password',
      inputPlaceholder: '当前口令',
    })
    loading.value = true
    const res = await totpSetup(pwd)
    setupUri.value = res.Data.OtpAuthURI
    setupSecret.value = res.Data.Secret
    qrDataUrl.value = await QRCode.toDataURL(res.Data.OtpAuthURI, { width: 220, margin: 1 })
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
    recoveryCodes.value = res.Data.RecoveryCodes
    auth.setTotpEnabled(true)
    enabled.value = true
    ElMessage.success('已启用多因素认证')
  } catch (e: any) {
    if (e === 'cancel' || e === 'close') return
    ElMessage.error(e?.message || '验证失败')
  } finally {
    loading.value = false
  }
}

async function onDisable() {
  try {
    const { value: pwd } = await ElMessageBox.prompt('请输入当前登录口令以确认关闭', '关闭多因素认证', {
      inputType: 'password',
      inputPlaceholder: '当前口令',
    })
    const { value } = await ElMessageBox.prompt('请输入当前动态验证码（或恢复码）以确认关闭', '关闭多因素认证', {
      inputPlaceholder: '动态验证码',
    })
    await totpDisable(pwd, value)
    auth.setTotpEnabled(false)
    enabled.value = false
    setupUri.value = ''
    recoveryCodes.value = []
    ElMessage.success('已关闭多因素认证')
  } catch (e: any) {
    if (e === 'cancel' || e === 'close') return
    ElMessage.error(e?.message || '关闭失败')
  }
}
</script>

<template>
  <el-dialog
    v-model="visible"
    title="多因素认证"
    width="520px"
    destroy-on-close
    append-to-body
  >
    <template v-if="!enabled">
      <el-button v-if="!setupUri" type="primary" :loading="loading" @click="onSetup">
        开启多因素认证
      </el-button>
      <div v-else>
        <img v-if="qrDataUrl" :src="qrDataUrl" alt="TOTP QR" />
        <p>手动添加密钥：<code>{{ setupSecret }}</code></p>
        <el-input v-model="code" placeholder="输入验证器中的 6 位动态验证码" style="max-width: 240px" />
        <el-button type="primary" :loading="loading" style="margin-left: 8px" @click="onConfirm">
          确认并启用
        </el-button>
      </div>
    </template>
    <template v-else>
      <el-alert type="success" :closable="false" title="多因素认证已开启" />
      <el-button type="danger" plain style="margin-top: 12px" @click="onDisable">关闭多因素认证</el-button>
    </template>
    <!-- 恢复码置于分支之外：启用成功后 enabled=true 会切到已开启分支，若放在其中会被立即卸载而看不到 -->
    <el-alert
      v-if="recoveryCodes.length"
      type="warning"
      :closable="false"
      title="请立即保存以下恢复码（仅显示一次）"
      :description="recoveryCodes.join('  ')"
      style="margin-top: 12px"
    />
  </el-dialog>
</template>

<style scoped>
img { border-radius: 8px; margin-bottom: 8px; }
code { background: #f5f5f5; padding: 2px 6px; border-radius: 4px; }
</style>
