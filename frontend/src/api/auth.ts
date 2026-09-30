import { http, type ApiRes } from './http'

// 登录返回：token + 用户信息（用户字段未带 JSON tag，序列化为 PascalCase）。
// 已开两步验证时返回 need_totp + preauth_token，无 token/user。
export interface LoginResult {
  token?: string
  user?: {
    ID: number
    Username: string
    Nickname: string
    Role: string
    Status: string
    PricingMode: string
  } | null
  need_totp?: boolean
  preauth_token?: string
  must_change_password?: boolean
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
    MustChangePassword: boolean
    TOTPEnabled: boolean
  }
  balance: number
}

// 登录第一步（密码校验；已开 2FA 时返回 need_totp + preauth_token）
export function login(username: string, password: string) {
  return http.post<LoginResult, ApiRes<LoginResult>>('/auth/login', { username, password })
}

// 登录第二步（TOTP 动态码 / 恢复码）
export function loginTotp(preauthToken: string, code: string) {
  return http.post<LoginResult, ApiRes<LoginResult>>('/auth/login/totp', { preauth_token: preauthToken, code })
}

// 登出：服务端 bump 会话代数，使当前 token 失效
export function logout() {
  return http.post<{ logout: boolean }, ApiRes<{ logout: boolean }>>('/auth/logout')
}

// 本人改密：成功后返回新签发的 token（会话代数 +1）
export function changeOwnPassword(oldPassword: string, newPassword: string) {
  return http.put<{ token: string }, ApiRes<{ token: string }>>('/auth/me/password', {
    old_password: oldPassword,
    new_password: newPassword,
  })
}

// TOTP 初始化：生成待确认 secret（返回 otpauth URI 与 base32 secret）
export function totpSetup() {
  return http.post<{ otpauth_uri: string; secret: string }, ApiRes<{ otpauth_uri: string; secret: string }>>('/auth/me/totp/setup')
}

// TOTP 确认：校验动态码后启用，返回一次性恢复码
export function totpConfirm(code: string) {
  return http.post<{ recovery_codes: string[] }, ApiRes<{ recovery_codes: string[] }>>('/auth/me/totp/confirm', { code })
}

// TOTP 解绑：需当前动态码（或恢复码）
export function totpDisable(code: string) {
  return http.delete<{ disabled: boolean }, ApiRes<{ disabled: boolean }>>('/auth/me/totp', { data: { code } })
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