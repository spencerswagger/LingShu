import { defineStore } from 'pinia'
import { getMe, getMeSummary, updateMeNickname, logout as logoutApi, type MeSummary } from '@/api/auth'

// 认证信息仅存 token + 角色 + 用户名（由后端登录结果决定角色，前端不提供选择）。
// profile 由 /auth/me 拉取（昵称/余额），用于右上角用户浮窗展示，可离线刷新。
// 本人摘要在途请求去重：绑定到发起请求时的 token（用户身份指纹），并发调用共享同一 Promise；
// 登出/换账号后旧在途请求即使落地也不会写入新用户（写入前校验 token 未变）。
let summaryInFlight: { token: string; promise: Promise<MeSummary | null> } | null = null

export const useAuthStore = defineStore('auth', {
  state: () => ({
    token: localStorage.getItem('token') || '',
    userId: localStorage.getItem('user_id') || '',
    role: localStorage.getItem('role') || '',
    username: localStorage.getItem('username') || '',
    nickname: localStorage.getItem('nickname') || '',
    mustChangePassword: localStorage.getItem('must_change_password') === '1',
    totpEnabled: localStorage.getItem('totp_enabled') === '1',
    balance: 0,
    balanceLoaded: false,
    // 本人摘要（/me/summary）前端缓存：同一份数据 5 分钟内不重复请求。
    // 仅右上角用户面板展开 / 统计页需要时按需拉取，切页不重取。
    summary: null as MeSummary | null,
    summaryLoadedAt: 0,
  }),
  actions: {
    setAuth(token: string, role: string, username: string, mustChange = false, totpEnabled = false) {
      this.token = token
      this.role = role
      this.username = username
      this.mustChangePassword = mustChange
      this.totpEnabled = totpEnabled
      // 换账号：旧在途摘要请求作废（其落地会因 token 不匹配而被丢弃）
      summaryInFlight = null
      this.summary = null
      this.summaryLoadedAt = 0
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
        const u = res.Data.User
        this.userId = String(u.ID || '')
        this.username = u.Username
        this.role = u.Role
        this.nickname = u.Nickname || ''
        this.mustChangePassword = !!u.MustChangePassword
        this.totpEnabled = !!u.TOTPEnabled
        this.balance = Number(res.Data.Balance ?? 0)
        this.balanceLoaded = true
        localStorage.setItem('user_id', this.userId)
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
    // 本人摘要：带 5 分钟前端缓存 + 在途去重。
    // 面板展开等「被动展示」调用默认走 TTL 缓存——无缓存或缓存超 5 分钟才拉网络，
    // 5 分钟内不重复请求。想强制刷新走明确的用户动作（refreshSummary，点击刷新），
    // 不要在展开里默认绕过 TTL。失败时不覆盖已有缓存。
    async fetchSummary(): Promise<MeSummary | null> {
      const CACHE_MS = 5 * 60 * 1000
      if (!this.token) return null
      if (this.summary && Date.now() - this.summaryLoadedAt < CACHE_MS) {
        return this.summary
      }
      return this.fetchSummaryRemote()
    },
    // 强制刷新（用户点「刷新」）：忽略 TTL 直接拉网络，仍走同一在途去重与身份校验。
    async refreshSummary(): Promise<MeSummary | null> {
      if (!this.token) return null
      return this.fetchSummaryRemote()
    },
    // 私有：真正发请求，带 token 身份校验 + 在途去重。不对外直接调用。
    async fetchSummaryRemote(): Promise<MeSummary | null> {
      if (!this.token) return null
      const token = this.token
      // 在途：同一用户身份共享同一 Promise；不同 token 视为不同请求，互不串
      if (summaryInFlight && summaryInFlight.token === token) return summaryInFlight.promise
      const promise = (async () => {
        try {
          const res = await getMeSummary()
          // 落地时校验身份未变：登出/换账号后旧响应不写入新用户
          if (this.token === token) {
            this.summary = res.Data
            this.summaryLoadedAt = Date.now()
            return this.summary
          }
          return this.summary
        } catch {
          return this.summary
        } finally {
          if (summaryInFlight?.token === token) summaryInFlight = null
        }
      })()
      summaryInFlight = { token, promise }
      return promise
    },
    async changeNickname(nickname: string) {
      const res = await updateMeNickname(nickname)
      this.nickname = res.Data.Nickname || ''
      if (this.nickname) localStorage.setItem('nickname', this.nickname)
      else localStorage.removeItem('nickname')
    },
    displayName() {
      return this.nickname || this.username || ''
    },
    // 纯本地清态：无副作用、无网络、可重复调用（401 拦截器专用，杜绝递归）。
    clearLocal() {
      this.token = ''
      this.userId = ''
      this.role = ''
      this.username = ''
      this.nickname = ''
      this.balance = 0
      this.balanceLoaded = false
      this.mustChangePassword = false
      this.totpEnabled = false
      // 登出：作废在途摘要请求 + 清缓存，避免跨用户串数据
      summaryInFlight = null
      this.summary = null
      this.summaryLoadedAt = 0
      localStorage.removeItem('token')
      localStorage.removeItem('user_id')
      localStorage.removeItem('role')
      localStorage.removeItem('username')
      localStorage.removeItem('nickname')
      localStorage.removeItem('must_change_password')
      localStorage.removeItem('totp_enabled')
    },
    // 主动登出：先把「通知服务端撤销 token」作为尽力而为的旁路请求发出（此刻 token 仍有效），
    // 再同步清空本地态。不 await 请求——用户可见语义是「本地会话结束」，应在本地立即完成，
    // 不能被网络阻塞（否则断网时要点满 axios 120s 超时才跳转）。
    // 顺序不可颠倒：若先 clearLocal 再请求，会因缺少 Authorization 头而 401。
    logout() {
      logoutApi().catch(() => {})
      this.clearLocal()
    },
  },
})