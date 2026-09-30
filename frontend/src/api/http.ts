import axios from 'axios'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '@/stores/auth'
import router from '@/router'

// 后端统一响应体：{code, message, request_id, data}
export interface ApiBody<T = unknown> {
  code: number
  message: string
  request_id: string
  data: T
}

// API 返回类型：成功拦截器已将响应解包为响应体 ApiBody<T>，
// 故 axios 第二泛型（R）固定为 ApiBody<T>，调用方通过 res.data 取真实载荷。
export type ApiRes<T> = ApiBody<T>

// 归一化后的错误对象（供页面 ErrorBubble 展示 + 复制 requestId）
export interface ApiError {
  message: string
  requestId: string
  code?: number
  status?: number
}

export const http = axios.create({ baseURL: '/api/v1', timeout: 120000 })

// 请求拦截：注入 Bearer token
http.interceptors.request.use((cfg) => {
  const auth = useAuthStore()
  if (auth.token) cfg.headers.Authorization = `Bearer ${auth.token}`
  return cfg
})

// 响应拦截：后端统一返回 {code,message,request_id,data}，成功时直接解出该 body
http.interceptors.response.use(
  (r) => r.data,
  (err) => {
    const body = err.response?.data
    const message = body?.message || '网络异常，请稍后重试'
    const requestId = body?.request_id || ''
    // 40302：需先修改默认密码 → 置位 mustChange 并跳转改密页（放 401 处理之前）
    if (err.response?.status === 403 && body?.code === 40302) {
      useAuthStore().setMustChange(true)
      if (router.currentRoute.value.path !== '/change-password') {
        router.push('/change-password')
      }
    }
    // 401：清除本地登录态并跳转登录页
    if (err.response?.status === 401) {
      useAuthStore().logout()
      if (router.currentRoute.value.path !== '/login') {
        router.push('/login')
      }
    }
    ElMessage.error(message)
    return Promise.reject<ApiError>({
      message,
      requestId,
      code: body?.code,
      status: err.response?.status,
    })
  },
)