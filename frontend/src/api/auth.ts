import { http, type ApiRes } from './http'

// 登录返回：token + 用户信息（用户字段未带 JSON tag，序列化为 PascalCase）
export interface LoginResult {
  token: string
  user: {
    ID: number
    Username: string
    Nickname: string
    Role: string
    Status: string
    PricingMode: string
  }
}

// 本人资料：user（PascalCase）+ balance
export interface MeResult {
  user: {
    ID: number
    Username: string
    Nickname: string
    Role: string
    Status: string
    PricingMode: string
  }
  balance: number
}

// 登录（角色由后端认证决定）
export function login(username: string, password: string) {
  return http.post<LoginResult, ApiRes<LoginResult>>('/auth/login', { username, password })
}

// 本人资料（含余额，右上角浮窗用）
export function getMe() {
  return http.get<MeResult, ApiRes<MeResult>>('/auth/me')
}

// 修改本人昵称
export function updateMeNickname(nickname: string) {
  return http.put<{ ID: number; Username: string; Nickname: string }, ApiRes<{ ID: number; Username: string; Nickname: string }>>(
    '/auth/me',
    { nickname },
  )
}