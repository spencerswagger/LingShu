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

// 账单列表项（后端 billingListItem / devBillingListItem：ExternalModel 为模型字段名）
export interface BillingItem {
  BillingID: string
  UserID: string
  ExternalModel: string
  AutoModelName?: string // 请求的 Auto 名（非 Auto 请求为空）
  PricingMode: string // sale | cost
  CreditsConsumed: number
  Status: string // completed | failed
  CallTime: string
  // 网关请求链路 ID：报给平台即可定位这一次请求（列表按 request_id 归并后取主行的 id）
  RequestID?: string
  // 该请求共产生的账单条数（>1 表示发生过候选/上游重试；列表已归并成一行）
  Attempts?: number
}

// 本人账单列表（支持时间区间 From/To，RFC3339；RequestID 按请求链路 ID 定位，Group=0 展开逐条尝试）
export function listDevBillings(
  params: {
    Page?: number
    Size?: number
    From?: string
    To?: string
    RequestID?: string
    // Group=0：退回逐条展示（展开一次请求的多条候选/重试记录用）；默认按请求归并
    Group?: number
  } = {},
) {
  return http.get<PageResult<BillingItem>, ApiRes<PageResult<BillingItem>>>('/dev/billings', {
    params: {
      Page: params.Page ?? 1,
      Size: params.Size ?? 20,
      From: params.From,
      To: params.To,
      RequestID: params.RequestID,
      Group: params.Group,
    },
  })
}

// 账单详情（三步拆解；admin 额外返回 InternalModelID/ChannelName），可能附带调用日志 CallLog
export function getDevBilling(billingId: string) {
  return http.get<BillingDetail, ApiRes<BillingDetail>>(`/dev/billings/${billingId}`)
}

// 调用日志视图（后端 views.CallLogView）：账单/会话详情附带，admin 与 dev 共用同一契约
export interface CallLogView {
  BillingID: string
  RequestID: string
  SessionID: string
  Model: string
  AutoModelName?: string // 请求的 Auto 名（非 Auto 请求为空）
  Status: string // 'completed' | 'failed'
  RespKind: string // 'non_stream' | 'stream' | 'error' | 'none'
  ErrorMessage: string
  DurationMs?: number
  FirstTokenMs?: number
  CallTime: string
  ReqMessages: unknown // 请求增量 JSON（可能 null）
  // 入站请求参数快照（可能 null）：{endpoint,inbound_protocol,upstream_protocol,upstream_url,converted,
  // token_id,model,auto_model_name,stream,message_count,tools:{count,names},params,summarized,dropped,over_budget}
  // 用于定位「这次到底发了什么参数」；不含 messages（见 ReqMessages），也不含任何凭据。
  // 开发者端后端会抹掉 upstream_url（内部部署映射），前端按「未返回」处理，不要为此要求放开。
  ReqParams?: unknown
  RespBody: string // 非流式完整响应体 / 流式 assistant 增量 JSON / 错误
  Decision: unknown // 决策轨迹 JSON（可能 null）：{attempts:[{order,channel_id,channel_key_id,internal_model_id,reason}], time_coeff, ctx_coeff, pre_consumed, result}
  // 上游原始 usage（可能 null）：{Protocol: 'openai-chat'|'openai-responses'|'anthropic-messages', Usage: {...上游原始键值...}}
  // 用于诊断各渠道 usage 口径（billing_records.tokens 只有归一化后的三段）。
  UpstreamUsage?: unknown
  PricingMode: string
}

// 单条调用日志（dev 详情同样填充 CallLog 字段）
export function getDevCallLog(billingId: string) {
  return http.get<CallLogView, ApiRes<CallLogView>>(`/dev/billings/${billingId}`)
}

export interface BillingDetail {
  BillingID: string
  CallTime: string
  Model: string
  AutoModelName?: string // 请求的 Auto 名（非 Auto 请求为空）
  PricingMode: string
  CreditsConsumed: number
  Status: string
  Steps: Step[]
  RouteDiff?: RouteDiff | null
  InternalModelID?: string
  ChannelName?: string
  CallLog?: CallLogView | null
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
