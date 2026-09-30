import { http, type ApiRes } from './http'

// ===== 开发端·令牌 =====

// 令牌列表项
export interface DevToken {
  id: number
  token_display: string
  display_name: string
  tag_id: number | null
  expires_at: string | null
  last_used_at: string | null
  status: string // ACTIVE | DISABLED
  created_at: string
}

// 分页结果
export interface PageResult<T> {
  list: T[]
  total: number
  page: number
  size: number
}

// 创建/轮换后返回的明文令牌（仅展示一次）
export interface CreatedToken {
  plain: string
  display: string
}

// 本人令牌列表
export function listDevTokens(page = 1, size = 20, status = '') {
  return http.get<PageResult<DevToken>, ApiRes<PageResult<DevToken>>>('/dev/tokens', {
    params: { page, size, status },
  })
}

// 创建本人令牌
export function createDevToken(payload: {
  display_name: string
  tag_id: number | null
  expires_at: string | null
}) {
  return http.post<CreatedToken, ApiRes<CreatedToken>>('/dev/tokens', payload)
}

// 切换令牌状态（启用/禁用）
export function toggleDevToken(id: number) {
  return http.post<DevToken, ApiRes<DevToken>>(`/dev/tokens/${id}/toggle`)
}

// 轮换令牌（作废旧令牌换发新令牌，返回新明文）
export function rotateDevToken(id: number) {
  return http.post<CreatedToken, ApiRes<CreatedToken>>(`/dev/tokens/${id}/rotate`)
}

// 查看本人令牌密钥（可随时查看/复制）
export function getDevTokenSecret(id: number) {
  return http.get<CreatedToken, ApiRes<CreatedToken>>(`/dev/tokens/${id}/secret`)
}

// 启用中的语义标签（令牌创建时可选；enabled=true 过滤）
// 后端 tag 领域结构无 json tag → 序列化为 PascalCase 字段
export function listDevTags() {
  return http.get<DevTag[], ApiRes<DevTag[]>>('/dev/tags', {
    params: { enabled: true },
  })
}

export interface DevTag {
  ID: number
  Name: string
  Description: string
  Enabled: boolean
}

// ===== 开发端·钱包/用量/账单/公告 =====

// 钱包余额
export function getDevWallet() {
  return http.get<{ balance: number }, ApiRes<{ balance: number }>>('/dev/wallet')
}

// 钱包流水
export function listDevFlows(page = 1, size = 20) {
  return http.get<PageResult<FlowItem>, ApiRes<PageResult<FlowItem>>>('/dev/wallet/flows', {
    params: { page, size },
  })
}

export interface FlowItem {
  id: number
  type: string // recharge | consume | adjust
  amount: number
  balance?: number // 该笔流水后的钱包余额
  session_name?: string // 关联会话名称（消费流水经账单联表）
  ref_billing_id?: string
  remark?: string
  created_at: string
}

// 按日用量
export function getDevUsage(days = 30) {
  return http.get<{ list: UsageDay[]; days: number }, ApiRes<{ list: UsageDay[]; days: number }>>(
    '/dev/usage',
    { params: { days } },
  )
}

export interface UsageDay {
  date: string // 2006-01-02
  credits: number
  calls: number
}

// 账单列表项
export interface BillingItem {
  billing_id: string
  user_id: number
  model: string
  pricing_mode: string // sale | cost
  credits_consumed: number
  status: string // completed | failed
  call_time: string
}

// 本人账单列表
export function listDevBillings(page = 1, size = 20) {
  return http.get<PageResult<BillingItem>, ApiRes<PageResult<BillingItem>>>('/dev/billings', {
    params: { page, size },
  })
}

// 账单详情（三步拆解）
export function getDevBilling(billingId: string) {
  return http.get<BillingDetail, ApiRes<BillingDetail>>(`/dev/billings/${billingId}`)
}

export interface BillingDetail {
  billing_id: string
  call_time: string
  model: string
  pricing_mode: string
  credits_consumed: number
  status: string
  steps: Step[]
  route_diff?: RouteDiff | null
}

export interface Step {
  title: string
  lines: Array<{ label: string; amount: number }>
  subtotal: number
}

export interface RouteDiff {
  label: string
  value: string
}

// 有效公告
export function listDevAnnouncements() {
  return http.get<{ list: DevAnnouncement[] }, ApiRes<{ list: DevAnnouncement[] }>>(
    '/dev/announcements',
  )
}

export interface DevAnnouncement {
  id: number
  title: string
  content: string
  level: string // info | warning | danger
  publish_at?: string
  expire_at?: string
  enabled: boolean
  created_at: string
}