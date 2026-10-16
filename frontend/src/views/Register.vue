<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { genCaptcha, register } from '@/api/auth'
import { useAuthStore } from '@/stores/auth'
import ErrorBubble from '@/components/ErrorBubble.vue'

const router = useRouter()
const auth = useAuthStore()

const formRef = ref()
const loading = ref(false)
const errMsg = ref('')
const errReqId = ref('')

const form = reactive({ username: '', password: '', nickname: '', captchaCode: '' })
// 图形验证码：注册前获取（公开接口），点击图片可刷新。
// ImageDataURI 是后端返回的完整 data URI，直接用作 img src，前端不再拼接前缀。
const captcha = ref<{ CaptchaID: string; ImageDataURI: string }>({ CaptchaID: '', ImageDataURI: '' })
const captchaLoading = ref(false)

// 口令强度校验：≥8 位且至少包含两类字符（小写/大写/数字/符号），与后端规则保持一致
function passwordClassCount(pwd: string): number {
  let lower = false, upper = false, digit = false, symbol = false
  for (const ch of pwd) {
    if (ch >= 'a' && ch <= 'z') lower = true
    else if (ch >= 'A' && ch <= 'Z') upper = true
    else if (ch >= '0' && ch <= '9') digit = true
    else symbol = true
  }
  return [lower, upper, digit, symbol].filter(Boolean).length
}

const rules = {
  username: [{ required: true, message: '请输入用户名', trigger: 'blur' }],
  password: [
    { required: true, message: '请输入密码', trigger: 'blur' },
    { min: 8, message: '密码至少 8 位', trigger: 'blur' },
    {
      validator: (_rule: unknown, value: string, cb: (err?: Error) => void) => {
        if (value && passwordClassCount(value) < 2) {
          cb(new Error('密码需至少包含字母、数字、符号中的两类'))
          return
        }
        cb()
      },
      trigger: 'blur',
    },
  ],
  captchaCode: [{ required: true, message: '请输入验证码', trigger: 'blur' }],
}

// 刷新验证码：captchaLoading 作并发防护，忽略重复点击
async function refreshCaptcha() {
  if (captchaLoading.value) return
  captchaLoading.value = true
  try {
    const res = await genCaptcha()
    captcha.value = res.Data
  } catch (e: any) {
    errMsg.value = e?.message || '验证码获取失败'
    errReqId.value = e?.requestId || ''
  } finally {
    captchaLoading.value = false
  }
}

async function onSubmit() {
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
    const res = await register({
      Username: form.username,
      Password: form.password,
      Nickname: form.nickname,
      CaptchaID: captcha.value.CaptchaID,
      CaptchaCode: form.captchaCode,
    })
    const d = res.Data
    // 注册成功即签发会话，直接进入控制台
    auth.setAuth(d.Token!, d.User!.Role, d.User!.Username, false, false)
    ElMessage.success('注册成功')
    router.push('/dev')
  } catch (e: any) {
    errMsg.value = e?.message || '注册失败'
    errReqId.value = e?.requestId || ''
    refreshCaptcha()
  } finally {
    loading.value = false
  }
}

onMounted(refreshCaptcha)
</script>

<template>
  <div class="register-page">
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

    <!-- 右：注册表单 -->
    <main class="form-side">
      <div class="form-box">
        <div class="form-head">
          <div class="form-title">创建账号</div>
          <div class="form-sub">注册成功后即可进入控制台</div>
        </div>
        <el-form ref="formRef" :model="form" :rules="rules" size="large" @keyup.enter="onSubmit">
          <el-form-item prop="username">
            <el-input v-model="form.username" placeholder="用户名" clearable />
          </el-form-item>
          <el-form-item prop="password">
            <el-input v-model="form.password" type="password" placeholder="密码" show-password />
          </el-form-item>
          <div class="field-hint">密码需至少 8 位，且包含字母、数字等至少两类字符</div>
          <el-form-item prop="nickname" style="margin-top: 16px">
            <el-input v-model="form.nickname" placeholder="昵称（选填）" clearable />
          </el-form-item>
          <el-form-item prop="captchaCode">
            <div class="captcha-row">
              <el-input v-model="form.captchaCode" placeholder="验证码" clearable @keyup.enter="onSubmit" />
              <img
                v-if="captcha.ImageDataURI"
                class="captcha-img"
                :src="captcha.ImageDataURI"
                alt="验证码"
                title="点击刷新"
                :class="{ 'is-loading': captchaLoading }"
                @click="refreshCaptcha"
              />
              <span v-else class="captcha-placeholder" @click="refreshCaptcha">点击获取</span>
            </div>
          </el-form-item>
          <el-form-item>
            <el-button type="primary" class="login-btn" :loading="loading" @click="onSubmit">
              注 册
            </el-button>
          </el-form-item>
        </el-form>
        <div class="form-sub back-login">
          <el-link type="info" @click="router.push('/login')">返回登录</el-link>
        </div>
        <ErrorBubble v-if="errMsg || errReqId" :message="errMsg" :request-id="errReqId" />
      </div>
    </main>
  </div>
</template>

<style scoped>
.register-page {
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

.field-hint {
  margin-top: -8px;
  margin-bottom: 8px;
  font-size: 12px;
  line-height: 1.6;
  color: var(--color-text-secondary);
}

.captcha-row {
  display: flex;
  align-items: center;
  gap: 12px;
  width: 100%;
}

.captcha-img {
  width: 110px;
  height: 40px;
  border-radius: 8px;
  border: 1px solid var(--color-border, #dcdfe6);
  object-fit: cover;
  cursor: pointer;
  flex-shrink: 0;
  opacity: 1;
  transition: opacity 0.2s;
}

.captcha-img.is-loading {
  opacity: 0.5;
  cursor: not-allowed;
}

.captcha-placeholder {
  width: 110px;
  height: 40px;
  border-radius: 8px;
  border: 1px dashed var(--color-border, #dcdfe6);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-size: 12px;
  color: var(--color-text-secondary);
  cursor: pointer;
  flex-shrink: 0;
}

.back-login {
  margin-top: 16px;
  text-align: center;
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
