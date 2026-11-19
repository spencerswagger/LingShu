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
    <!-- 左：品牌区 -->
    <aside class="brand">
      <div class="brand-inner">
        <div class="brand-head">
          <span class="brand-mark">
            <svg viewBox="0 0 24 24" width="22" height="22" aria-hidden="true">
              <path
                d="M12 2 4 6.5v11L12 22l8-4.5v-11L12 2Zm0 2.6 5.6 3.15v6.5L12 17.4l-5.6-3.15v-6.5L12 4.6Zm-2.2 5.05L12 7.65l2.2 2v2.7L12 14.35l-2.2-2v-2.7Z"
                fill="#fff"
              />
            </svg>
          </span>
        </div>

        <div class="brand-body">
          <img class="brand-lockup" src="/brand/lockup.svg" alt="灵枢" />
          <h1 class="brand-title">统一 AI 调用网关</h1>
          <p class="brand-desc">面向团队的模型接入、计费与路由控制台</p>
          <ul class="brand-points">
            <li>四协议透传 · 上游与本地一致</li>
            <li>预扣与结算分离 · 账本对账</li>
            <li>按内部模型 / 路由组编排售卖</li>
          </ul>
        </div>

        <div class="brand-foot">内部系统</div>
      </div>
    </aside>

    <!-- 右：登录表单 -->
    <main class="form-side">
      <div class="form-box">
        <div v-if="step === 'password'">
          <div class="form-head">
            <div class="form-title">欢迎回来</div>
            <div class="form-sub">登录控制台</div>
          </div>
          <el-form ref="formRef" :model="form" :rules="rules" size="large" @keyup.enter="onLogin">
            <el-form-item prop="username">
              <el-input v-model="form.username" placeholder="用户名" clearable />
            </el-form-item>
            <el-form-item prop="password">
              <el-input v-model="form.password" type="password" placeholder="密码" show-password />
            </el-form-item>
            <el-form-item>
              <el-button type="primary" class="login-btn" :loading="loading" @click="onLogin">
                登 录
              </el-button>
            </el-form-item>
          </el-form>
          <div class="form-sub" style="margin-top: 16px; text-align: center">
            <el-link type="primary" @click="router.push('/register')">还没有账号？注册</el-link>
          </div>
        </div>
        <div v-else>
          <div class="form-head">
            <div class="form-title">多因素认证</div>
            <div class="form-sub">请输入身份验证器中的动态码（或恢复码）</div>
          </div>
          <el-alert v-if="mustChange" type="warning" :closable="false" show-icon style="margin-bottom: 16px"
            title="验证通过后需先修改默认密码" />
          <el-input v-model="totpCode" placeholder="6 位动态码" size="large" @keyup.enter="onLoginTotp" />
          <el-button type="primary" class="login-btn" :loading="loading" style="margin-top: 16px" @click="onLoginTotp">
            验 证
          </el-button>
          <div class="form-sub" style="margin-top: 12px; text-align: center">
            <el-link type="info" @click="step = 'password'">返回上一步</el-link>
          </div>
        </div>
        <ErrorBubble v-if="errMsg || errReqId" :message="errMsg" :request-id="errReqId" />
      </div>
    </main>
  </div>
</template>

<style scoped>
.login-page {
  display: flex;
  min-height: 100vh;
  background:
    radial-gradient(1100px 640px at 12% -10%, #16213c 0%, transparent 58%),
    radial-gradient(900px 520px at 100% 110%, #101a30 0%, transparent 55%),
    #070a14;
  color: var(--ink);
}
.brand {
  flex: 1.15;
  display: flex;
  align-items: center;
  justify-content: center;
  border-right: 1px solid var(--line);
}
.brand-inner {
  max-width: 460px;
  padding: 56px 40px;
  display: flex;
  flex-direction: column;
  gap: 28px;
  min-height: 70vh;
}
.brand-mark svg { display: block; }
.brand-lockup { width: 150px; height: auto; display: block; margin-bottom: 26px; }
.brand-title { font-size: 26px; font-weight: 600; letter-spacing: -.01em; margin: 0 0 8px; }
.brand-desc { font-size: 13.5px; color: var(--ink-2); margin: 0 0 24px; line-height: 1.7; }
.brand-points { list-style: none; padding: 0; margin: 0; display: flex; flex-direction: column; gap: 10px; }
.brand-points li { font-size: 12.5px; color: var(--ink-2); padding-left: 18px; position: relative; }
.brand-points li::before { content: ''; position: absolute; left: 0; top: 7px; width: 6px; height: 6px; border-radius: 50%; background: var(--acc-2); }
.brand-foot { font-size: 11px; color: var(--ink-4); }
.form-side { flex: .85; display: flex; align-items: center; justify-content: center; padding: 40px; }
.form-box {
  width: 100%;
  max-width: 380px;
  background: linear-gradient(175deg, #101728, #0b0f18);
  border: 1px solid var(--line);
  border-radius: 14px;
  padding: 32px 28px;
  box-shadow: 0 30px 80px -40px rgba(0,0,0,.9);
}
.form-head { margin-bottom: 22px; }
.form-title { font-size: 18px; font-weight: 600; }
.form-sub { font-size: 12px; color: var(--ink-3); margin-top: 6px; }
.login-btn { width: 100%; height: 40px; font-size: 14px; }
:deep(.el-input__wrapper) { background: #0b101c; box-shadow: 0 0 0 1px var(--line-2) inset; }
:deep(.el-input__inner) { color: var(--ink); }
@media (max-width: 900px) {
  .login-page { flex-direction: column; }
  .brand { border-right: 0; border-bottom: 1px solid var(--line); }
  .brand-inner { min-height: auto; padding: 36px 24px; }
  .form-side { padding: 28px 18px 48px; }
}
</style>
