import { defineStore } from 'pinia'
import { getMe, getMeSummary, updateMeNickname, logout as logoutApi, type MeResult, type MeSummary } from '@/api/auth'

// 认证信息仅存 token + 角色 + 用户名（由后端登录结果决定角色，前端不提供选择）。
//
// 统一取数（#74 收尾）：本人资料 /auth/me 与本人摘要 /auth/me/summary 共用同一套缓存内核——
//   ① 5 分钟 TTL（与后端 statscache 的 300s 对齐），命中缓存不发请求，切页/反复展开面板零请求；
//   ② 在途去重：同一用户身份并发调用共享同一 Promise，只发一次网络；
//   ③ 身份绑定：在途请求记下发起时的 token，落地前校验 token 未变——登出 / 换账号 / 改密换
//      token 后，旧响应一律丢弃，不会串到新用户；
//   ④ 失效统一入口：invalidateMe()（只作废缓存）/ reloadMe()（作废 + 立即重取）。
//      各页面不要再自己发 /auth/me 或 /auth/me/summary，一律走本 store。
const ME_TTL_MS = 5 * 60 * 1000

interface CacheSlot<T> {
  data: T | null
  loadedAt: number
  inFlight: { token: string; promise: Promise<T | null> } | null
}

function createSlot<T>(): CacheSlot<T> {
  return { data: null, loadedAt: 0, inFlight: null }
}

// 模块级单例：整站共享同一份缓存，不随组件卸载 / 路由切换丢失。
const profileSlot = createSlot<MeResult>()
const summarySlot = createSlot<MeSummary>()

function dropSlot<T>(slot: CacheSlot<T>) {
  slot.data = null
  slot.loadedAt = 0
  slot.inFlight = null
}

// 缓存内核：TTL 命中直接返回缓存；否则在途去重后发一次请求。
// isCurrentToken 用于落地前校验身份（登出 / 换账号后旧响应不写入）。
function loadSlot<T>(opts: {
  slot: CacheSlot<T>
  token: string
  force?: boolean
  fetcher: () => Promise<T>
  isCurrentToken: (token: string) => boolean
}): Promise<T | null> {
  const { slot, token, force, fetcher, isCurrentToken } = opts
  if (!token) return Promise.resolve(null)
  if (!force && slot.data !== null && Date.now() - slot.loadedAt < ME_TTL_MS) {
    return Promise.resolve(slot.data)
  }
  // 在途：同一用户身份共享同一 Promise；不同 token 视为不同请求，互不串
  if (slot.inFlight && slot.inFlight.token === token) return slot.inFlight.promise
  const promise = (async () => {
    try {
      const data = await fetcher()
      if (isCurrentToken(token)) {
        slot.data = data
        slot.loadedAt = Date.now()
        return data
      }
      return slot.data
    } catch {
      // 失败不覆盖已有缓存（保留旧数据，避免面板闪空）
      return slot.data
    } finally {
      if (slot.inFlight?.token === token) slot.inFlight = null
    }
  })()
  slot.inFlight = { token, promise }
  return promise
}

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
    // 本人资料缓存落地时间（0 = 无缓存），便于调试与埋点；真实数据在模块级 profileSlot。
    profileLoadedAt: 0,
    // 本人摘要（/me/summary）：同一份数据 5 分钟内不重复请求。
    // 仅右上角用户面板展开 / 统计页需要时按需拉取，切页不重取。
    summary: null as MeSummary | null,
    summaryLoadedAt: 0,
    // 手动刷新在途标记（刷新按钮转圈用）
    meRefreshing: false,
  }),
  actions: {
    setAuth(token: string, role: string, username: string, mustChange = false, totpEnabled = false) {
      this.token = token
      this.role = role
      this.username = username
      this.mustChangePassword = mustChange
      this.totpEnabled = totpEnabled
      // 登录 / 改密换 token：上一身份的缓存与在途请求整体作废（下一次读取自动重取）
      this.invalidateMe()
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
    // 作废本人资料 / 摘要缓存（不立刻发请求）：下一次读取自动重取。
    // 保留当前展示值，避免失效瞬间面板闪 '--'；需要立刻拿到新值用 reloadMe()。
    invalidateMe() {
      dropSlot(profileSlot)
      dropSlot(summarySlot)
      this.profileLoadedAt = 0
      this.summaryLoadedAt = 0
    },
    // 作废并立即重取（登录后 / 令牌变更 / 余额变动 / 手动刷新统一走这里）
    async reloadMe(): Promise<void> {
      if (!this.token) return
      this.invalidateMe()
      this.meRefreshing = true
      try {
        await Promise.all([this.fetchProfile({ force: true }), this.fetchSummary({ force: true })])
      } finally {
        this.meRefreshing = false
      }
    },
    // 本人资料（昵称 / 角色 / 余额）：默认走 5 分钟 TTL，命中缓存不发请求。
    // force=true 仅用于明确的用户动作或业务变更后（reloadMe / refreshProfile）。
    async fetchProfile(opts: { force?: boolean } = {}): Promise<MeResult | null> {
      const token = this.token
      if (!token) return null
      const data = await loadSlot({
        slot: profileSlot,
        token,
        force: !!opts.force,
        fetcher: () => getMe().then((r) => r.Data),
        isCurrentToken: (t) => this.token === t,
      })
      if (data && this.token === token) this.applyProfile(data)
      return data
    },
    // 强制刷新本人资料（改密换 token 后、TOTP 开关后、面板刷新按钮）
    refreshProfile(): Promise<MeResult | null> {
      return this.fetchProfile({ force: true })
    },
    // 本人摘要：带 5 分钟前端缓存 + 在途去重。
    // 面板展开等「被动展示」调用默认走 TTL 缓存——无缓存或缓存超 5 分钟才拉网络，
    // 5 分钟内不重复请求。想强制刷新走明确的用户动作（refreshSummary），
    // 不要在展开里默认绕过 TTL。失败时不覆盖已有缓存。
    async fetchSummary(opts: { force?: boolean } = {}): Promise<MeSummary | null> {
      const token = this.token
      if (!token) return null
      const data = await loadSlot({
        slot: summarySlot,
        token,
        force: !!opts.force,
        fetcher: () => getMeSummary().then((r) => r.Data),
        isCurrentToken: (t) => this.token === t,
      })
      if (data && this.token === token) {
        this.summary = data
        this.summaryLoadedAt = summarySlot.loadedAt
      }
      return data
    },
    // 强制刷新摘要（用户点「刷新」/ 明确的刷新动作）
    refreshSummary(): Promise<MeSummary | null> {
      return this.fetchSummary({ force: true })
    },
    // 私有：把 /auth/me 结果落到 state + localStorage（缓存命中时也会重放一次，保持同源）
    applyProfile(res: MeResult) {
      const u = res.User
      this.userId = String(u.ID || '')
      this.username = u.Username
      this.role = u.Role
      this.nickname = u.Nickname || ''
      this.mustChangePassword = !!u.MustChangePassword
      this.totpEnabled = !!u.TOTPEnabled
      this.balance = Number(res.Balance ?? 0)
      this.balanceLoaded = true
      this.profileLoadedAt = profileSlot.loadedAt
      localStorage.setItem('user_id', this.userId)
      localStorage.setItem('role', u.Role)
      localStorage.setItem('username', u.Username)
      localStorage.setItem('must_change_password', this.mustChangePassword ? '1' : '0')
      localStorage.setItem('totp_enabled', this.totpEnabled ? '1' : '0')
      if (u.Nickname) localStorage.setItem('nickname', u.Nickname)
      else localStorage.removeItem('nickname')
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
      // 登出：作废在途请求 + 清缓存，避免跨用户串数据
      dropSlot(profileSlot)
      dropSlot(summarySlot)
      this.profileLoadedAt = 0
      this.summary = null
      this.summaryLoadedAt = 0
      this.meRefreshing = false
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
