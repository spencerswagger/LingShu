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
  errMsg.value = ''
  errReqId.value = ''
  await formRef.value.validate().catch(() => Promise.reject())
  loading.value = true
  try {
    // 登录成功后由后端决角色，前端不提供选择
    const res = await login(form.username, form.password)
    const d = res.data
    // 已开两步验证：进入第二步（不发 token）
    if (d.need_totp && d.preauth_token) {
      step.value = 'totp'
      preauthToken.value = d.preauth_token
      mustChange.value = !!d.must_change_password
      return
    }
    const { token, user } = d
    auth.setAuth(token!, user!.Role, user!.Username, !!d.must_change_password, !!d.totp_enabled)
    ElMessage.success('登录成功')
    if (d.must_change_password) {
      router.push('/change-password')
      return
    }
    router.push(safeRedirect(route.query.redirect) || (user!.Role === 'ADMIN' ? '/admin' : '/dev'))
  } catch (e: any) {
    errMsg.value = e?.message || '登录失败'
    errReqId.value = e?.requestId || ''
  } finally {
    loading.value = false
  }
}

async function onLoginTotp() {
  errMsg.value = ''
  errReqId.value = ''
  loading.value = true
  try {
    const res = await loginTotp(preauthToken.value, totpCode.value)
    const d = res.data
    auth.setAuth(d.token!, d.user!.Role, d.user!.Username, !!d.must_change_password, !!d.totp_enabled)
    ElMessage.success('登录成功')
    if (d.must_change_password) {
      router.push('/change-password')
      return
    }
    router.push(safeRedirect(route.query.redirect) || (d.user!.Role === 'ADMIN' ? '/admin' : '/dev'))
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
          <span class="brand-name">AI网关</span>
        </div>

        <div class="brand-body">
          <h1 class="brand-title">统一 AI 调用平面</h1>
          <p class="brand-desc">
            渠道接入 · 模型定价 · 令牌鉴权 · 智能路由 · 全链路计费，一处掌控
          </p>
          <ul class="brand-points">
            <li>OpenAI 兼容统一网关入口</li>
            <li>语义标签智能路由与会话亲和</li>
            <li>国密加密与安全合规默认内置</li>
          </ul>
        </div>

        <div class="brand-foot">© 2026 AI Gateway · Internal Console</div>
      </div>
    </aside>

    <!-- 右：登录表单 -->
    <main class="form-side">
      <div class="form-box">
        <div v-if="step === 'password'">
          <div class="form-head">
            <div class="form-title">欢迎回来</div>
            <div class="form-sub">请使用你的账号登录控制台</div>
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
        </div>
        <div v-else>
          <div class="form-head">
            <div class="form-title">两步验证</div>
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
  background: #f4f5fa;
}

/* ===== 品牌区 ===== */
.brand {
  flex: 1.1;
  background:
    radial-gradient(900px 520px at 8% 0%, rgba(99, 102, 241, 0.45) 0%, transparent 60%),
    radial-gradient(700px 480px at 100% 100%, rgba(139, 92, 246, 0.30) 0%, transparent 55%),
    linear-gradient(160deg, #171c3b 0%, #10142b 100%);
  display: flex;
  align-items: center;
  justify-content: center;
  color: #fff;
}

.brand-inner {
  max-width: 460px;
  padding: 48px 32px;
  display: flex;
  flex-direction: column;
  min-height: 78vh;
}

.brand-head {
  display: flex;
  align-items: center;
  gap: 12px;
}

.brand-mark {
  width: 40px;
  height: 40px;
  border-radius: 12px;
  background: var(--brand-gradient);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  box-shadow: 0 6px 18px rgba(99, 102, 241, 0.5);
}

.brand-name {
  font-size: 20px;
  font-weight: 700;
  letter-spacing: 1px;
}

.brand-body {
  flex: 1;
  display: flex;
  flex-direction: column;
  justify-content: center;
  padding: 48px 0;
}

.brand-title {
  font-size: 34px;
  font-weight: 800;
  line-height: 1.25;
  margin: 0 0 16px;
  letter-spacing: 0.5px;
}

.brand-desc {
  font-size: 15px;
  line-height: 1.8;
  color: rgba(255, 255, 255, 0.72);
  margin: 0 0 32px;
}

.brand-points {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  gap: 12px;
}

.brand-points li {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 14px;
  color: rgba(255, 255, 255, 0.85);
}

.brand-points li::before {
  content: '';
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--brand-gradient);
  box-shadow: 0 0 0 3px rgba(99, 102, 241, 0.28);
  flex-shrink: 0;
}

.brand-foot {
  font-size: 12px;
  color: rgba(255, 255, 255, 0.35);
}

/* ===== 表单区 ===== */
.form-side {
  flex: 1;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 32px;
}

.form-box {
  width: 100%;
  max-width: 380px;
}

.form-head {
  margin-bottom: 28px;
}

.form-title {
  font-size: 26px;
  font-weight: 800;
  color: var(--color-text);
  letter-spacing: 0.3px;
}

.form-sub {
  margin-top: 8px;
  font-size: 14px;
  color: var(--color-text-secondary);
}

.login-btn {
  width: 100%;
  border-radius: 10px;
  font-size: 15px;
  letter-spacing: 4px;
}

@media (max-width: 900px) {
  .brand {
    display: none;
  }
}
</style>