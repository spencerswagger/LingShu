import { defineStore } from 'pinia'
import { getMe, updateMeNickname } from '@/api/auth'

// 认证信息仅存 token + 角色 + 用户名（由后端登录结果决定角色，前端不提供选择）。
// profile 由 /auth/me 拉取（昵称/余额），用于右上角用户浮窗展示，可离线刷新。
export const useAuthStore = defineStore('auth', {
  state: () => ({
    token: localStorage.getItem('token') || '',
    role: localStorage.getItem('role') || '',
    username: localStorage.getItem('username') || '',
    nickname: localStorage.getItem('nickname') || '',
    balance: 0,
    balanceLoaded: false,
  }),
  actions: {
    setAuth(token: string, role: string, username: string) {
      this.token = token
      this.role = role
      this.username = username
      localStorage.setItem('token', token)
      localStorage.setItem('role', role)
      localStorage.setItem('username', username)
    },
    async fetchProfile() {
      if (!this.token) return
      try {
        const res = await getMe()
        const u = res.data.user
        this.username = u.Username
        this.role = u.Role
        this.nickname = u.Nickname || ''
        this.balance = Number(res.data.balance ?? 0)
        this.balanceLoaded = true
        localStorage.setItem('role', u.Role)
        localStorage.setItem('username', u.Username)
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
    logout() {
      this.token = ''
      this.role = ''
      this.username = ''
      this.nickname = ''
      this.balance = 0
      this.balanceLoaded = false
      localStorage.removeItem('token')
      localStorage.removeItem('role')
      localStorage.removeItem('username')
      localStorage.removeItem('nickname')
    },
  },
})