import { defineStore } from 'pinia'
import { getMe, updateMeNickname, logout as logoutApi } from '@/api/auth'

// 认证信息仅存 token + 角色 + 用户名（由后端登录结果决定角色，前端不提供选择）。
// profile 由 /auth/me 拉取（昵称/余额），用于右上角用户浮窗展示，可离线刷新。
export const useAuthStore = defineStore('auth', {
  state: () => ({
    token: localStorage.getItem('token') || '',
    role: localStorage.getItem('role') || '',
    username: localStorage.getItem('username') || '',
    nickname: localStorage.getItem('nickname') || '',
    mustChangePassword: localStorage.getItem('must_change_password') === '1',
    totpEnabled: localStorage.getItem('totp_enabled') === '1',
    balance: 0,
    balanceLoaded: false,
  }),
  actions: {
    setAuth(token: string, role: string, username: string, mustChange = false, totpEnabled = false) {
      this.token = token
      this.role = role
      this.username = username
      this.mustChangePassword = mustChange
      this.totpEnabled = totpEnabled
      localStorage.setItem('token', token)
      localStorage.setItem('role', role)
      localStorage.setItem('username', username)
      localStorage.setItem('must_change_password', mustChange ? '1' : '0')
      localStorage.setItem('totp_enabled', totpEnabled ? '1' : '0')
    },
    setMustChange(v: boolean) {
      this.mustChangePassword = v
      localStorage.setItem('must_change_password', v ? '1' : '0')
    },
    setTotpEnabled(v: boolean) {
      this.totpEnabled = v
      localStorage.setItem('totp_enabled', v ? '1' : '0')
    },
    async fetchProfile() {
      if (!this.token) return
      try {
        const res = await getMe()
        const u = res.data.user
        this.username = u.Username
        this.role = u.Role
        this.nickname = u.Nickname || ''
        this.mustChangePassword = !!u.MustChangePassword
        this.totpEnabled = !!u.TOTPEnabled
        this.balance = Number(res.data.balance ?? 0)
        this.balanceLoaded = true
        localStorage.setItem('role', u.Role)
        localStorage.setItem('username', u.Username)
        localStorage.setItem('must_change_password', this.mustChangePassword ? '1' : '0')
        localStorage.setItem('totp_enabled', this.totpEnabled ? '1' : '0')
        if (u.Nickname) localStorage.setItem('nickname', u.Nickname)
        else localStorage.removeItem('nickname')
      } catch {
        // 未登录 / 网络异常时静默，保持已有展示
      }
    },
    async changeNickname(nickname: string) {
      const res = await updateMeNickname(nickname)
      this.nickname = res.data.Nickname || ''
      if (this.nickname) localStorage.setItem('nickname', this.nickname)
      else localStorage.removeItem('nickname')
    },
    displayName() {
      return this.nickname || this.username || ''
    },
    // 纯本地清态：无副作用、无网络、可重复调用（401 拦截器专用，杜绝递归）。
    clearLocal() {
      this.token = ''
      this.role = ''
      this.username = ''
      this.nickname = ''
      this.balance = 0
      this.balanceLoaded = false
      this.mustChangePassword = false
      this.totpEnabled = false
      localStorage.removeItem('token')
      localStorage.removeItem('role')
      localStorage.removeItem('username')
      localStorage.removeItem('nickname')
      localStorage.removeItem('must_change_password')
      localStorage.removeItem('totp_enabled')
    },
    // 主动登出：先发请求（此刻 token 仍有效），随后同步清空本地态，最后再等服务端收尾。
    // 顺序不可颠倒：若先 clearLocal 再请求，会因缺少 Authorization 头而 401。
    // 先清本地可让调用方 await 几乎立即返回——断网/服务端不可达时不再受 axios 120s 超时拖累。
    async logout() {
      const p = logoutApi().catch(() => {})
      this.clearLocal()
      await p
    },
  },
})