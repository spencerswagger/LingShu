<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import { login, loginTotp } from '@/api/auth'
import { useAuthStore } from '@/stores/auth'
import ErrorBubble from '@/components/ErrorBubble.vue'

const router = useRouter()
const route = useRoute()
const auth = useAuthStore()

const formRef = ref()
const loading = ref(false)
const errMsg = ref('')
const errReqId = ref('')
const step = ref<'password' | 'totp'>('password')
const preauthToken = ref('')
const totpCode = ref('')
const mustChange = ref(false)

const form = reactive({ username: '', password: '' })

const rules = {
  username: [{ required: true, message: '请输入用户名', trigger: 'blur' }],
  password: [{ required: true, message: '请输入密码', trigger: 'blur' }],
}

async function onLogin() {
  // 重入保护：必须在任何 await 之前同步置位 loading，否则两次点击都能越过表单校验的
  // 等待窗口，对同一 preauth 并发提交——败者 401 会触发 401 拦截器 clearLocal()，
  // 把刚建立的成功会话状态清掉（表现为「输入动态码后仍提示失败」）。
  if (loading.value) return
  loading.value = true
  errMsg.value = ''
  errReqId.value = ''
  try {
    await formRef.value.validate()
  } catch {
    loading.value = false
    return
  }
  try {
    // 登录成功后由后端决角色，前端不提供选择
    const res = await login(form.username, form.password)
    const d = res.Data
    // 已开多因素认证：进入第二步（不发 token）
    if (d.NeedTOTP && d.PreAuthToken) {
      step.value = 'totp'
      preauthToken.value = d.PreAuthToken
      mustChange.value = !!d.MustChangePassword
      return
    }
    const { Token, User } = d
    auth.setAuth(Token!, User!.Role, User!.Username, !!d.MustChangePassword, !!d.TotpEnabled)
    ElMessage.success('登录成功')
    if (d.MustChangePassword) {
      router.push('/change-password')
      return
    }
    router.push(safeRedirect(route.query.redirect) || (User!.Role === 'ADMIN' ? '/admin' : '/dev'))
  } catch (e: any) {
    errMsg.value = e?.message || '登录失败'
    errReqId.value = e?.requestId || ''
  } finally {
    loading.value = false
  }
}

async function onLoginTotp() {
  // 同上：同一 preauth 只允许一次在途请求，避免重复提交导致败者 401 清掉成功会话。
  if (loading.value) return
  loading.value = true
  errMsg.value = ''
  errReqId.value = ''
  try {
    const res = await loginTotp(preauthToken.value, totpCode.value)
    const d = res.Data
    auth.setAuth(d.Token!, d.User!.Role, d.User!.Username, !!d.MustChangePassword, !!d.TotpEnabled)
    ElMessage.success('登录成功')
    if (d.MustChangePassword) {
      router.push('/change-password')
      return
    }
    router.push(safeRedirect(route.query.redirect) || (d.User!.Role === 'ADMIN' ? '/admin' : '/dev'))
  } catch (e: any) {
    errMsg.value = e?.message || '验证失败'
    errReqId.value = e?.requestId || ''
  } finally {
    loading.value = false
  }
}

// safeRedirect 仅接受站内相对路径，杜绝开放重定向。
function safeRedirect(v: unknown): string {
  if (typeof v !== 'string') return ''
  if (!v.startsWith('/') || v.startsWith('//')) return ''
  return v
}
</script>

<template>
  <div class="login-page">
    <div class="aura" aria-hidden="true"></div>
    <div class="grid" aria-hidden="true"></div>
    <main class="login-card">
      <div class="brand">
        <img src="/brand/lockup.svg" alt="灵枢" />
      </div>
      <div v-if="step === 'password'">
        <div class="form-head">
          <h1>登录灵枢</h1>
          <p>统一 AI 调用网关控制台</p>
        </div>
        <el-form ref="formRef" :model="form" :rules="rules" size="large" @keyup.enter="onLogin">
          <el-form-item prop="username"><el-input v-model="form.username" placeholder="用户名" clearable /></el-form-item>
          <el-form-item prop="password"><el-input v-model="form.password" type="password" placeholder="密码" show-password /></el-form-item>
          <el-form-item>
            <el-button type="primary" class="login-btn" :loading="loading" @click="onLogin">登 录</el-button>
          </el-form-item>
        </el-form>
        <div class="foot"><el-link type="primary" @click="router.push('/register')">还没有账号？注册</el-link></div>
      </div>
      <div v-else>
        <div class="form-head"><h1>多因素认证</h1><p>输入身份验证器中的动态码（或恢复码）</p></div>
        <el-alert v-if="mustChange" type="warning" :closable="false" show-icon style="margin-bottom: 16px" title="验证通过后需先修改默认密码" />
        <el-input v-model="totpCode" placeholder="6 位动态码" size="large" @keyup.enter="onLoginTotp" />
        <el-button type="primary" class="login-btn" :loading="loading" style="margin-top: 16px" @click="onLoginTotp">验 证</el-button>
        <div class="foot"><el-link type="info" @click="step = 'password'">返回上一步</el-link></div>
      </div>
      <ErrorBubble v-if="errMsg || errReqId" :message="errMsg" :request-id="errReqId" />
    </main>
  </div>
</template>

<style scoped>
.login-page {
  position: relative;
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  overflow: hidden;
  background: #06080f;
}
/* 呼吸渐变光晕 */
.aura {
  position: absolute;
  width: 120vmax;
  height: 120vmax;
  left: 50%;
  top: 50%;
  transform: translate(-50%, -50%);
  background: conic-gradient(from 0deg at 50% 50%,
      #4c6fe8 0%, #7b5cf0 18%, #101a30 32%, #06080f 45%, #06080f 55%, #101a30 68%, #4c6fe8 84%, #4c6fe8 100%);
  filter: blur(90px);
  opacity: .38;
  animation: breathe 14s ease-in-out infinite, spin 46s linear infinite;
}
@keyframes breathe { 0%,100% { opacity: .26; transform: translate(-50%, -50%) scale(1); } 50% { opacity: .5; transform: translate(-50%, -50%) scale(1.08); } }
@keyframes spin { to { transform: translate(-50%, -50%) rotate(360deg) scale(1.04); } }
.grid {
  position: absolute; inset: 0;
  background-image: linear-gradient(#ffffff08 1px, transparent 1px), linear-gradient(90deg, #ffffff08 1px, transparent 1px);
  background-size: 44px 44px;
  mask-image: radial-gradient(70% 60% at 50% 45%, #000 0%, transparent 100%);
}
.login-card {
  position: relative;
  width: 100%;
  max-width: 380px;
  margin: 24px;
  padding: 34px 30px 26px;
  border-radius: 16px;
  border: 1px solid #26324a;
  background: linear-gradient(175deg, rgba(16,23,40,.92), rgba(9,13,21,.94));
  box-shadow: 0 40px 90px -46px #4c6fe8aa, inset 0 1px #ffffff0a;
  backdrop-filter: blur(10px);
}
.brand { display: flex; justify-content: center; margin-bottom: 22px; }
.brand img { width: 132px; height: auto; }
.form-head { text-align: center; margin-bottom: 20px; }
.form-head h1 { font-size: 19px; font-weight: 600; margin: 0 0 6px; color: var(--ink); }
.form-head p { font-size: 12px; color: var(--ink-3); margin: 0; }
.login-btn { width: 100%; height: 40px; font-size: 14px; }
.foot { margin-top: 14px; text-align: center; }
:deep(.el-input__wrapper) { background: #0b101c; box-shadow: 0 0 0 1px #222c40 inset; }
:deep(.el-input__inner) { color: var(--ink); }
</style>
