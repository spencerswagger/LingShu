import { http, type ApiRes } from './http'

// 登录返回：token + 用户信息。
// 已开两步验证时返回 NeedTOTP + PreAuthToken，无 token/user。
export interface LoginResult {
  Token?: string
  User?: {
    ID: string
    Username: string
    Nickname: string
    Role: string
    Status: string
    PricingMode: string
  } | null
  NeedTOTP?: boolean
  PreAuthToken?: string
  MustChangePassword?: boolean
  TotpEnabled?: boolean
}

// 本人资料：user + balance
export interface MeResult {
  user: {
    ID: string
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

// 登录第一步（密码校验；已开 2FA 时返回 NeedTOTP + PreAuthToken）
export function login(username: string, password: string) {
  return http.post<LoginResult, ApiRes<LoginResult>>('/auth/login', { Username: username, Password: password })
}

// 登录第二步（TOTP 动态码 / 恢复码）
export function loginTotp(preauthToken: string, code: string) {
  return http.post<LoginResult, ApiRes<LoginResult>>('/auth/login/totp', { PreAuthToken: preauthToken, Code: code })
}

// 登出：服务端 bump 会话代数，使当前 token 失效
export function logout() {
  return http.post<{ logout: boolean }, ApiRes<{ logout: boolean }>>('/auth/logout')
}

// 本人改密：成功后返回新签发的 token（会话代数 +1）
export function changeOwnPassword(oldPassword: string, newPassword: string) {
  return http.put<{ token: string }, ApiRes<{ token: string }>>('/auth/me/password', {
    OldPassword: oldPassword,
    NewPassword: newPassword,
  })
}

// TOTP 初始化：生成待确认 secret（返回 otpauth URI 与 base32 secret）。需当前口令二次验证。
export function totpSetup(password: string) {
  return http.post<{ otpauth_uri: string; secret: string }, ApiRes<{ otpauth_uri: string; secret: string }>>('/auth/me/totp/setup', { Password: password })
}

// TOTP 确认：校验动态码后启用，返回一次性恢复码。需当前口令二次验证。
export function totpConfirm(password: string, code: string) {
  return http.post<{ recovery_codes: string[] }, ApiRes<{ recovery_codes: string[] }>>('/auth/me/totp/confirm', { Password: password, Code: code })
}

// TOTP 解绑：需当前口令 + 当前动态码（或恢复码）
export function totpDisable(password: string, code: string) {
  return http.delete<{ disabled: boolean }, ApiRes<{ disabled: boolean }>>('/auth/me/totp', { data: { Password: password, Code: code } })
}

// 本人资料（含余额，右上角浮窗用）
export function getMe() {
  return http.get<MeResult, ApiRes<MeResult>>('/auth/me')
}

// 修改本人昵称
export function updateMeNickname(nickname: string) {
  return http.put<{ ID: string; Username: string; Nickname: string }, ApiRes<{ ID: string; Username: string; Nickname: string }>>(
    '/auth/me',
    { Nickname: nickname },
  )
}
