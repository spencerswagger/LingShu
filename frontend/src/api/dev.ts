import { http, type ApiRes } from './http'

// ===== 开发端·令牌 =====

// 令牌列表项（与后端 tokenResponse 对齐：字段名 = Go 字段名）
export interface DevToken {
  ID: string
  TokenDisplay: string
  DisplayName: string
  TagID: string | null
  ExpiresAt: string | null
  LastUsedAt: string | null
  Status: string // ACTIVE | DISABLED
  CreatedAt: string
}

// 分页结果
export interface PageResult<T> {
  List: T[]
  Total: number
  Page: number
  Size: number
}

// 创建/轮换后返回的明文令牌（仅展示一次）
export interface CreatedToken {
  Plain: string
  Display: string
}

// 本人令牌列表
export function listDevTokens(page = 1, size = 20, status = '') {
  return http.get<PageResult<DevToken>, ApiRes<PageResult<DevToken>>>('/dev/tokens', {
    params: { Page: page, Size: size, Status: status },
  })
}

// 创建本人令牌
export function createDevToken(payload: {
  DisplayName: string
  TagID: string | null
  ExpiresAt: string | null
}) {
  return http.post<CreatedToken, ApiRes<CreatedToken>>('/dev/tokens', payload)
}

// 切换令牌状态（启用/禁用）
export function toggleDevToken(id: string) {
  return http.post<DevToken, ApiRes<DevToken>>(`/dev/tokens/${id}/toggle`)
}

// 轮换令牌（作废旧令牌换发新令牌，返回新明文）
export function rotateDevToken(id: string) {
  return http.post<CreatedToken, ApiRes<CreatedToken>>(`/dev/tokens/${id}/rotate`)
}

// 查看本人令牌密钥（可随时查看/复制）。敏感操作：需当前口令二次验证。
export function getDevTokenSecret(id: string, password: string) {
  return http.get<CreatedToken, ApiRes<CreatedToken>>(`/dev/tokens/${id}/secret`, {
    headers: { 'X-Current-Password': password },
  })
}

// 启用中的语义标签（令牌创建时可选；enabled=true 过滤）
export function listDevTags() {
  return http.get<DevTag[], ApiRes<DevTag[]>>('/dev/tags', {
    params: { Enabled: true },
  })
}

export interface DevTag {
  ID: string
  Name: string
  Description: string
  KVPairs: Record<string, string>
  Enabled: boolean
  CreatedBy: string
  CreatedAt: string
}

// ===== 开发端·钱包/用量/账单/公告 =====

// 钱包余额
export function getDevWallet() {
  return http.get<{ Balance: number }, ApiRes<{ Balance: number }>>('/dev/wallet')
}

// 钱包流水
export function listDevFlows(page = 1, size = 20) {
  return http.get<PageResult<FlowItem>, ApiRes<PageResult<FlowItem>>>('/dev/wallet/flows', {
    params: { Page: page, Size: size },
  })
}

export interface FlowItem {
  ID: string
  Type: string // recharge | consume | adjust | set
  Amount: number
  Balance?: number // 该笔流水后的钱包余额
  SessionName?: string // 关联会话名称（消费流水经账单联表）
  RefBillingID?: string
  Remark?: string
  CreatedAt: string
}

// 按日用量
export function getDevUsage(days = 30) {
  return http.get<{ List: UsageDay[]; Days: number }, ApiRes<{ List: UsageDay[]; Days: number }>>(
    '/dev/usage',
    { params: { Days: days } },
  )
}

export interface UsageDay {
  Date: string // 2006-01-02
  Credits: number
  Calls: number
}

// 账单列表项（后端 billingListItem：ExternalModel 为模型字段名）
export interface BillingItem {
  BillingID: string
  UserID: string
  ExternalModel: string
  PricingMode: string // sale | cost
  CreditsConsumed: number
  Status: string // completed | failed
  CallTime: string
}

// 本人账单列表
export function listDevBillings(page = 1, size = 20) {
  return http.get<PageResult<BillingItem>, ApiRes<PageResult<BillingItem>>>('/dev/billings', {
    params: { Page: page, Size: size },
  })
}

// 账单详情（三步拆解；admin 额外返回 InternalModelID/ChannelName）
export function getDevBilling(billingId: string) {
  return http.get<BillingDetail, ApiRes<BillingDetail>>(`/dev/billings/${billingId}`)
}

export interface BillingDetail {
  BillingID: string
  CallTime: string
  Model: string
  PricingMode: string
  CreditsConsumed: number
  Status: string
  Steps: Step[]
  RouteDiff?: RouteDiff | null
  InternalModelID?: string
  ChannelName?: string
}

export interface Step {
  Title: string
  Lines: Array<{ Label: string; Amount: number }>
  Subtotal: number
}

export interface RouteDiff {
  Label: string
  Value: string
}

// 有效公告（字段名 = Go 字段名）
export function listDevAnnouncements() {
  return http.get<{ List: DevAnnouncement[] }, ApiRes<{ List: DevAnnouncement[] }>>(
    '/dev/announcements',
  )
}

export interface DevAnnouncement {
  ID: string
  Title: string
  Content: string
  Level: string // info | warning | danger
  PublishAt?: string
  ExpireAt?: string
  Enabled: boolean
  CreatedAt: string
}
