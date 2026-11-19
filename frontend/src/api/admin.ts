import { http, type ApiRes } from './http'
import type { PageResult, BillingDetail, FlowItem, CallLogView } from './dev'
// 调用日志视图类型与 dev 端共用（同一后端契约）；admin 详情页/会话记录直接复用
export type { CallLogView } from './dev'

// ===== 管理端 API（字段名与后端 Go 结构体字段名对齐） =====

// ===== 用户 =====
export interface AdminUser {
  ID: string
  Username: string
  Nickname: string
  Role: string
  Status: string // ACTIVE | DISABLED
  PricingMode: string // sale | cost
  Balance: number // 当前积分余额（列表接口填充）
  TotalSpent: number // 累计消费积分（completed 账单求和）
  CreatedAt: string
  UpdatedAt: string
}

// 用户详情：用户 + 钱包余额 + 最近流水。
export interface UserDetailData {
  User: AdminUser
  Wallet: { Balance: number }
  RecentFlows: FlowItem[]
}

// 用户列表（分页）
export function listUsers(params: Record<string, unknown> = {}) {
  return http.get<PageResult<AdminUser>, ApiRes<PageResult<AdminUser>>>('/admin/users', { params })
}
export function getUser(id: string) {
  return http.get<UserDetailData, ApiRes<UserDetailData>>(`/admin/users/${id}`)
}
// 创建用户：必填 Username/Password/Role
export function createUser(payload: {
  Username: string
  Password: string
  Nickname?: string
  Role: string
  PricingMode?: string
}) {
  return http.post<AdminUser, ApiRes<AdminUser>>('/admin/users', payload)
}
// 更新用户：Role/Status/PricingMode/Nickname
export function updateUser(
  id: string,
  payload: { Role?: string; Status?: string; PricingMode?: string; Nickname?: string },
) {
  return http.put<AdminUser, ApiRes<AdminUser>>(`/admin/users/${id}`, payload)
}
// 重置用户密码（管理员输入新密码）
export function resetUserPassword(id: string, password: string) {
  return http.post<{ Reset: boolean }, ApiRes<{ Reset: boolean }>>(
    `/admin/users/${id}/reset-password`,
    { Password: password },
  )
}
// 强制解绑用户 TOTP（管理员操作，无需验证码）
export function resetUserTotp(id: string) {
  return http.post<{ Reset: boolean }, ApiRes<{ Reset: boolean }>>(`/admin/users/${id}/reset-totp`)
}
export function batchDeleteUsers(ids: string[]) {
  return http.post<{ Affected: number }, ApiRes<{ Affected: number }>>('/admin/users/batch-delete', {
    IDs: ids,
  })
}

export function rechargeWallet(id: string, amount: number, remark: string, password: string) {
  return http.post<{ Affected: number }, ApiRes<{ Affected: number }>>(
    `/admin/users/${id}/wallet/recharge`,
    { Amount: amount, Remark: remark, Password: password },
  )
}
export function adjustWallet(id: string, amount: number, remark: string, password: string) {
  return http.post<{ Affected: number }, ApiRes<{ Affected: number }>>(
    `/admin/users/${id}/wallet/adjust`,
    { Amount: amount, Remark: remark, Password: password },
  )
}
// 覆盖余额（直接 set）。
export function setWallet(id: string, balance: number, remark: string, password: string) {
  return http.post<{ Balance: number }, ApiRes<{ Balance: number }>>(
    `/admin/users/${id}/wallet/set`,
    { Balance: balance, Remark: remark, Password: password },
  )
}
export function listUserFlows(id: string, page = 1, size = 20) {
  return http.get<PageResult<FlowItem>, ApiRes<PageResult<FlowItem>>>(
    `/admin/users/${id}/wallet/flows`,
    { params: { Page: page, Size: size } },
  )
}

// ===== 令牌 =====
export interface AdminToken {
  ID: string
  TokenDisplay: string
  UserID: string
  Username: string
  UserNickname?: string
  DisplayName: string
  ExpiresAt: string | null
  LastUsedAt: string | null
  Status: string // ACTIVE | DISABLED
  CreatedAt: string
}
export interface CreatedToken {
  Plain: string
  Display: string
}

export function listTokens(params: Record<string, unknown> = {}) {
  return http.get<PageResult<AdminToken>, ApiRes<PageResult<AdminToken>>>('/admin/tokens', {
    params,
  })
}
export function createToken(payload: {
  UserID: string
  DisplayName: string
  ExpiresAt?: string | null
}) {
  return http.post<CreatedToken, ApiRes<CreatedToken>>('/admin/tokens', payload)
}
// 查看令牌密钥（SM4 加密落库，可随时查看/复制）。敏感操作：需当前口令二次验证。
export function getTokenSecret(id: string, password: string) {
  return http.get<CreatedToken, ApiRes<CreatedToken>>(`/admin/tokens/${id}/secret`, {
    headers: { 'X-Current-Password': password },
  })
}
export function batchDeleteTokens(ids: string[]) {
  return http.post<{ Affected: number }, ApiRes<{ Affected: number }>>('/admin/tokens/batch-delete', {
    IDs: ids,
  })
}

// ===== 三段计价费率（售价/成本对称的公共定价 schema）=====
// 与后端 billing.Rates 对齐，键必须齐全且非负。
export const rateKeys = ['Input', 'Output', 'CacheRead'] as const
export const rateLabels: Record<string, string> = {
  Input: '输入',
  Output: '输出',
  CacheRead: '缓存读',
}
// 售价/成本三段倍率的 tooltip 解释（倍率 = 每百万 token 计费数值，照抄官方定价录入）
export const rateHints: Record<string, string> = {
  Input: '输入倍率（每百万 token）：纯输入，不含缓存读；上游报的缓存写 token 计入此段。',
  Output: '输出倍率（每百万 token）：生成 token，含推理。',
  CacheRead: '缓存读倍率（每百万 token）：命中提示词缓存读取的输入，按低价计费。',
}

// ===== 对外模型（external_models·售价层；字段名 = Go 字段名）=====
export interface ExternalModel {
  ID: string
  ExternalName: string
  Description: string
  Enabled: boolean
  SaleRates: Record<string, number>
  TimeConfig: TimeCoeffConfig | null
  ContextTiers: ContextTier[] | null
  // 别名：仅用于模型名解析（请求命中任一别名即路由到本模型），不出现在 /v1/models
  Aliases?: string[]
  // V2 绑定：绑定真源（external_models.bound_internal_ref，0019 加列）。
  // undefined = 后端还没返回该列（V2 未上线）；null = 已上线但未绑定（绑定能力未上线，当前不拦截）。
  BoundInternalRef?: string | null
  CreatedAt: string
  UpdatedAt: string
}
export interface ExternalModelInput {
  ExternalName: string
  Description?: string
  Enabled?: boolean
  SaleRates: Record<string, number>
  TimeConfig?: TimeCoeffConfig | null
  ContextTiers?: ContextTier[] | null
  Aliases?: string[]
}
// 列表返回裸数组（非 {list}）
export function listExternalModels(params: Record<string, unknown> = {}, base = '/admin') {
  return http.get<ExternalModel[], ApiRes<ExternalModel[]>>(`${base}/models`, { params })
}
export function createExternalModel(payload: ExternalModelInput) {
  return http.post<ExternalModel, ApiRes<ExternalModel>>('/admin/models', payload)
}
export function updateExternalModel(id: string, payload: ExternalModelInput) {
  return http.put<ExternalModel, ApiRes<ExternalModel>>(`/admin/models/${id}`, payload)
}
export function batchDeleteExternalModels(ids: string[]) {
  return http.post<{ Deleted: number }, ApiRes<{ Deleted: number }>>('/admin/models/batch-delete', {
    IDs: ids,
  })
}

// 用 models.dev 参考价批量刷新全部对外模型售价；返回更新/跳过清单
export interface SyncPricesResult {
  Updated: string[]
  Skipped: string[]
}
export function syncModelsPrices() {
  return http.post<SyncPricesResult, ApiRes<SyncPricesResult>>('/admin/models/sync-prices')
}
// models.dev 价格目录条目（供应商维度）；价格均为参考原始价 / 百万 token
export interface CatalogEntry {
  Provider: string
  ModelID: string
  InputUSD: number
  OutputUSD: number
  CacheReadUSD: number
}
export function priceCatalog(q: string) {
  return http.get<{ List: CatalogEntry[]; Total: number; UpdatedAt: string }, ApiRes<{ List: CatalogEntry[]; Total: number; UpdatedAt: string }>>(
    '/admin/models/price-catalog',
    { params: { Q: q || undefined } },
  )
}

// ===== 账单 =====
export interface BillingUsage {
  Input: number
  Output: number
  CacheRead: number
}
export interface AdminBillingItem {
  BillingID: string
  UserID: string
  Username?: string
  UserNickname?: string
  TokenName?: string
  ExternalModel: string
  AutoModelName?: string // 请求的 Auto 名（非 Auto 请求为空；后端 billingListItem.AutoModelName）
  PricingMode: string
  CreditsConsumed: number
  Status: string
  CallTime: string
  InternalModelID: string
  ChannelName?: string
  ChannelKeyID?: string
  KeyName?: string
  SessionID?: string
  SessionName?: string
  Tokens: BillingUsage
  Rates?: Record<string, number>
  CoeffTime?: number
  CoeffContext?: number
  RValue?: number
  ErrorMessage?: string
  DurationMs?: number | null
  FirstTokenMs?: number | null
  // 网关请求链路 ID（列表默认按 request_id 归并成一行，取主行的 id）
  RequestID?: string
  // 该请求共产生的账单条数（>1 = 发生过候选/上游重试；Group=0 可展开子记录）
  Attempts?: number
  // ===== V2 归因（0020 加列；老后端不返回 → undefined）=====
  // 渠道维度在 V2 被 upstream_provider/upstream_account 取代：渠道族表在 0024 删除，
  // 历史行这两列为 null（不猜），前端显示「未归因」而不是留空或填 0。
  UpstreamProvider?: string | null
  UpstreamAccount?: string | null
  UpstreamProtocol?: string | null
  UpstreamModel?: string | null
  // 成本来源：ledger（账本实付）| amortized（订阅摊销）| none（无口径）；历史行为 null
  CostSource?: string | null
}
// 账单列表。语义变更（后端 PR #63 起，已拍板）：列表默认按 request_id 归并成「一次请求一行」，
// 因此返回的 Total 是**请求数**（不是账单行数）；Group=0 退回逐条展示（展开子记录用），
// RequestID 按请求链路 ID 过滤（命中即整组候选一起返回）。
export function listBillings(params: Record<string, unknown> = {}, base = '/admin') {
  return http.get<PageResult<AdminBillingItem>, ApiRes<PageResult<AdminBillingItem>>>(
    `${base}/billings`,
    { params },
  )
}
// 账单筛选聚合统计（与列表同 filter）：请求数/总积分/总 token/总耗时（统计栏用）。
// InputTokens=纯输入（不含缓存读）总量，CacheReadTokens=缓存读命中量（缓存命中率 = 缓存读 ÷ (输入+缓存读)）。
// Busy* 为忙时口径：分子只算成功且带耗时的请求，分母 BusyDurationMS 为同批请求的耗时之和（严格同源）；
// BusyDurationMS 为 0 时 BusyRPM/BusyTPM 返回 0。DurationTotalMS 为全部请求（含失败无耗时）的耗时合计。
export interface BillingStats {
  Requests: number
  CreditsTotal: number
  TokensTotal: number
  DurationTotalMS: number
  InputTokens: number
  CacheReadTokens: number
  BusyRequests: number
  BusyTokens: number
  BusyDurationMS: number
  BusyRPM: number
  BusyTPM: number
}
export function getBillingStats(params: Record<string, unknown> = {}, base = '/admin') {
  return http.get<BillingStats, ApiRes<BillingStats>>(`${base}/billings/stats`, { params })
}
export function getBilling(billingId: string, base = '/admin') {
  return http.get<BillingDetail, ApiRes<BillingDetail>>(`${base}/billings/${billingId}`)
}
// 单条调用日志（账单详情附带 CallLog 的独立端点；返回单对象，不强转 BillingDetail）
export function getCallLog(billingId: string) {
  return http.get<CallLogView, ApiRes<CallLogView>>(`/admin/billings/${billingId}`)
}

// ===== unpriced 账单补收（B 方案·手动触发；后端 rebill_handler.go 同源） =====
// 补收汇总：条数与金额合计一律用后端返回值，前端不自己算。
export interface RebillResult {
  GroupRef: string
  // 扫描到的本组待补收行数（含已带标记的）
  Scanned: number
  // 本次实际补开的新账单条数
  Rebilled: number
  // 新开账单的积分合计
  TotalCredits: number
  // 新账单号清单（rebill-<原账单id>-v1）
  NewBillingIDs: string[]
  // 已补收过、本次跳过的条数（幂等跳过 ≠ 新补收）
  AlreadyMarked: number
}
// 对指定组执行补收：按当前售价给该组历史 unpriced 账单逐条开新账单并扣款。
// 失败（组未定价/个别行仍解析不到售价）→ 400 + Reason（rebill_group_unpriced / rebill_row_unresolvable），
// Message 是给人看的中文原因，调用方把两者一起展示。
export function rebillUnpriced(ref: string) {
  return http.post<RebillResult, ApiRes<RebillResult>>('/admin/billings/rebill-unpriced', { Ref: ref })
}

// ===== 会话（内存注册表 + DB 投影；按用户/令牌/密钥维度过滤与踢下线） =====
export interface AdminSession {
  SessionID: string
  UserID: string
  UserName: string
  UserNickname?: string
  TokenID: string
  TokenName: string
  Model: string
  ChannelKeyID: string
  ChannelKeyName: string
  ChannelName?: string
  SessionRaw: string
  SessionName: string
  Closed: boolean
  CreatedAt: string
  LastActive: string
  ExpireAt: string
  Expired: boolean
  // 实时状态（后端按 session_id 聚合）
  Inflight: number // 进行中请求数（>0 = 正在请求）
  Calls: number // 总调用次数
  Tokens: number // 总消耗 Token
  Credits: number // 总消耗积分
  ContextLen: number // 总输入 Token（输入+缓存读）
  RPM: number // 近 1 分钟每分钟请求数
  TPM: number // 近 1 分钟每分钟 Token 数
  UserMsgs: number // 用户消息次数
  ToolCalls: number // 工具调用次数
  CacheHitRate: number // 缓存命中率（0~1）
}
export function listSessions(
  params: Record<string, any>,
  base = '/admin',
): Promise<ApiRes<{ List: AdminSession[]; Total: number }>> {
  return http.get<
    { List: AdminSession[]; Total: number },
    ApiRes<{ List: AdminSession[]; Total: number }>
  >(`${base}/sessions`, { params })
}
export function kickSessions(
  payload: Record<string, any>,
): Promise<ApiRes<{ Affected: number }>> {
  return http.post<{ Affected: number }, ApiRes<{ Affected: number }>>(
    '/admin/sessions/kick',
    payload,
  )
}
export function renameSession(
  sessionId: string,
  name: string,
): Promise<ApiRes<{ Updated: boolean }>> {
  return http.put<{ Updated: boolean }, ApiRes<{ Updated: boolean }>>(
    `/admin/sessions/${sessionId}/name`,
    { Name: name },
  )
}
// 会话完整调用记录（按时间序，含请求/响应/决策轨迹）
export function listSessionCalls(
  sessionId: string,
  base = '/admin',
): Promise<ApiRes<{ List: CallLogView[]; Total: number }>> {
  return http.get<
    { List: CallLogView[]; Total: number },
    ApiRes<{ List: CallLogView[]; Total: number }>
  >(`${base}/sessions/${sessionId}/calls`)
}

// ===== 计费配置 =====
export interface ContextTier {
  Min: number
  Max: number | null
  Coeff: number
}
export interface Segment {
  Name: string
  Start: string // "HH:MM"
  End: string // "HH:MM" 或 "24:00"
  Coeff: number
  Weekdays?: number[] // 0=周日…6=周六；空 = 每天生效
}
export interface DateOverride {
  Name: string
  Start: string // "YYYY-MM-DD"
  End: string
  Segments: Segment[]
}
export interface TimeCoeffConfig {
  Timezone: string
  Default: number
  Periodic: Segment[]
  Overrides: DateOverride[]
}
export interface CostSample {
  input: number
  cache_read: number
  output: number
}
export interface BillingConfigData {
  R: number | string
  CNYRate: number | string
  CreditValue: number | string // 积分价值 V=R/1e6（每积分对应人民币元），只读展示
  ContextTiers: ContextTier[]
  TimeConfig: TimeCoeffConfig
  CostSample: CostSample // 路由成本比较样值用量（routing.cost_sample）
}
// ===== 会话过期分档配置（字段名 = 后端存储的 JSON key，snake_case）=====
export interface SessionExpirationContextTier {
  max_tokens: number
  ttl_minutes: number
}
export interface SessionExpirationTurnTier {
  max_turns: number
  ttl_minutes: number
}
export interface SessionExpirationData {
  default_ttl_minutes: number
  context_tiers: SessionExpirationContextTier[]
  turn_tiers: SessionExpirationTurnTier[]
}

// ===== 系统配置（四块：计费 / 路由 / 会话过期 / 网关白名单）=====
// 后端语义：**只写请求体里出现的键**——没传的键/整块保持库中现值（「不传 = 不改」）；
// 白名单 `[]` = 清空、不传 = 不改；`SessionExpiration` / 白名单显式 `null` 等同不传。
// 响应 `Affected` = 实际写入的键数（不要按 1 判断成功）。
// 成本页的多渠道对比 / 渠道轨迹开关已改为**本机 localStorage 偏好**，接口不再有 `Console` 块。
export interface SessionCostConfig {
  enabled: boolean // 开启会话成本路由（关掉即回退渠道亲和）
  mid_switch: boolean // 允许会话中途切换渠道
  shadow_enabled: boolean // 影子观测（会产生额外上游调用与成本，按需开启）
  remaining_a: number // 剩余量权重 a
  remaining_b: number // 剩余量权重 b
  remaining_min: number // 剩余量归一化下界
  remaining_max: number // 剩余量归一化上界
  min_remaining: number // 低于该剩余量不再参与评分
  cooldown_sec: number // 两次切换之间的冷却秒数
  min_improve: number // 最小改善阈值 [0,1)
  fallback_note: string // 回退说明（只读，PUT 时忽略）
}
export interface RoutingConfigData {
  SessionCost: SessionCostConfig
}
// V2 上游三键（sys_configs）：base_url / timeouts / degrade.{passthrough,ledger,sync}
// + 订阅池前缀名单。老后端不返回该块 → undefined（页面显示「V2 配置未上线」，不显示 0/false）。
export interface UpstreamConfigData {
  base_url?: string
  timeouts?: { stream_first_byte_ms?: number; stream_idle_ms?: number }
  degrade?: { passthrough_enabled?: boolean; ledger_enabled?: boolean; sync_enabled?: boolean }
  subscription_prefixes?: string[]
}

export interface SystemConfigData {
  Billing: BillingConfigData
  Routing: RoutingConfigData
  SessionExpiration: SessionExpirationData
  UpstreamResponseHeaderWhitelist: string[]
  Upstream?: UpstreamConfigData
}
export function getSystemConfig() {
  return http.get<SystemConfigData, ApiRes<SystemConfigData>>('/admin/configs/system')
}
// 每块都可选：前端按 tab 只提交本 tab 的字段（整块不传 = 不改该块）
export interface SystemConfigPayload {
  Billing?: {
    R?: number | string
    CnyRate?: number | string
    ContextTiers?: ContextTier[]
    TimeConfig?: TimeCoeffConfig
    CostSample?: CostSample
  }
  Routing?: {
    // 会话成本路由：字段级合并，没传的字段保持现值；fallback_note 只读、PUT 时忽略
    SessionCost?: Partial<Omit<SessionCostConfig, 'fallback_note'>>
  }
  SessionExpiration?: SessionExpirationData
  UpstreamResponseHeaderWhitelist?: string[]
}
export function putSystemConfig(payload: SystemConfigPayload) {
  return http.put<{ Affected: number }, ApiRes<{ Affected: number }>>('/admin/configs/system', payload)
}

// ===== 代理侧（magpie）管理面一键跳转 =====
// 后端先校验会话是 ADMIN，再签一张 8 小时的票据（HttpOnly cookie `magpie_gate`），
// 回一个**同源相对路径**。代理侧的页面口令由 nginx 在服务端代填，URL 里不含任何凭据。
export interface MagpieAccessData {
  Url: string
  ExpiresAt: string
}
// 503 = 代理侧还没接入（magpie.base_url 没配）：页面自己出引导文案，不弹全局红条。
export function requestMagpieConsole() {
  return http.post<MagpieAccessData, ApiRes<MagpieAccessData>>('/admin/ops/magpie-access', undefined, {
    silentStatuses: [503],
  })
}

// ===== 公告（字段名 = Go 字段名）=====
export interface AdminAnnouncement {
  ID: string
  Title: string
  Content: string
  Level: string // info | warning | danger
  PublishAt?: string
  ExpireAt?: string
  Enabled: boolean
  CreatedAt: string
}
export interface AnnouncementInput {
  Title: string
  Content: string
  Level: string
  PublishAt?: string | null
  ExpireAt?: string | null
  Enabled?: boolean
}
export function listAnnouncements() {
  return http.get<{ List: AdminAnnouncement[] }, ApiRes<{ List: AdminAnnouncement[] }>>(
    '/admin/announcements',
  )
}
export function createAnnouncement(payload: AnnouncementInput) {
  return http.post<AdminAnnouncement, ApiRes<AdminAnnouncement>>('/admin/announcements', payload)
}
export function updateAnnouncement(id: string, payload: AnnouncementInput) {
  return http.put<AdminAnnouncement, ApiRes<AdminAnnouncement>>(`/admin/announcements/${id}`, payload)
}
export function batchDeleteAnnouncements(ids: string[]) {
  return http.post<{ Deleted: number }, ApiRes<{ Deleted: number }>>(
    '/admin/announcements/batch-delete',
    { IDs: ids },
  )
}

// ===== 价格同步：立即执行（watchlist/告警相关端点当前前端未使用） =====
export function runSync() {
  return http.post<{ Summary: string }, ApiRes<{ Summary: string }>>('/admin/sync/run')
}

// ===== 成本（只读；字段名 = 后端 Go 结构体字段名）=====
// 成本统计行：按「当前实体」（渠道 × 内部模型 × 对外模型）聚合。
// EntityKey 是稳定实体键（`cm:<channel_model_id>` 或 `name:<文本>#<内部模型ID>`），直接用作前端 row-key。
// ExternalModelName 是「展示名」（解析成功取当前对外模型名，否则原文）；SnapshotNames 为历史上出现过的文本快照。
// CacheHitRate 为 0~1 比率（缓存读 ÷ (输入+缓存读)）；InputOutputRatio = 输入 ÷ 输出。
export interface CostStatRow {
  EntityKey: string
  ChannelModelID: number
  ExternalModelID: number
  ChannelID: number
  ChannelName: string
  InternalModelID: string
  ExternalModelName: string
  SnapshotNames: string[]
  CostCredits: number
  SaleCredits: number
  Requests: number
  InputTokens: number
  CacheReadTokens: number
  OutputTokens: number
  CacheHitRate: number
  InputOutputRatio: number
}
// 成本统计汇总行：与 CostStatRow 同口径，但不含渠道/模型维度字段（全量合计）。
export interface CostStatTotal {
  CostCredits: number
  SaleCredits: number
  Requests: number
  InputTokens: number
  CacheReadTokens: number
  OutputTokens: number
  CacheHitRate: number
  InputOutputRatio: number
}
// 成本统计：Rows 为逐行明细，Total 为全量汇总行（口径同 Rows，仅缺维度字段）
export function getCostStats() {
  return http.get<
    { Rows: CostStatRow[]; Total: CostStatTotal },
    ApiRes<{ Rows: CostStatRow[]; Total: CostStatTotal }>
  >('/admin/cost/stats')
}
// 全平台 token 默认值（仅由 token 求和得出，与金额解耦）
export interface CostDefaults {
  PerTurnInputTokens: number
  CacheHitRate: number
  InputOutputRatio: number
}
export function getCostDefaults() {
  return http.get<CostDefaults, ApiRes<CostDefaults>>('/admin/cost/defaults')
}
// 定价曲线：某「渠道 × 内部模型」在轮次 1..Turns 上的每轮成本/售价（积分）
export interface CurveItem {
  ChannelID: number
  ChannelName: string
  InternalModelID: string
  ExternalModelName: string
  Cost: number[]
  Sale: number[]
}
export interface CostCurvesData {
  Turns: number
  Curves: CurveItem[]
}
export function getCostCurves(params: {
  perTurnInput: number
  hitRate: number
  inOutRatio: number
  turns: number
}) {
  return http.get<CostCurvesData, ApiRes<CostCurvesData>>('/admin/cost/curves', { params })
}
// 会话逐轮成本点（取历史实际 cost_credits / credits_consumed，**含失败轮**）
// 两个序号刻意分开：
//   - Turn   成功调用序号（1..N），失败轮为 0 —— 老字段，兼容用；
//   - TurnNo 时间线序号（1..M，含失败轮）—— 前端 x 轴用这个，失败轮占位、轴不断裂。
// 失败轮 Status='failed'、Failed=true、金额恒 0，token 保留实际值。
export interface SessionPoint {
  Turn: number
  TurnNo: number
  Status: string // completed | failed | upstream_error（上游 4xx/5xx，金额 0）
  Failed: boolean
  CallTime: string
  ChannelName: string
  InternalModelID: string
  InputTokens: number
  CacheReadTokens: number
  OutputTokens: number
  CostCredits: number
  SaleCredits: number
}
export interface CostSessionData {
  SessionID: string
  Points: SessionPoint[]
  Totals: { CostCredits: number; SaleCredits: number }
  Total: number // 该会话账单总行数（含失败轮）
  Limit: number // 本次生效上限（缺省 500、最多 2000）
  Truncated: boolean // 是否被 Limit 截断
}
export function getCostSession(id: string) {
  return http.get<CostSessionData, ApiRes<CostSessionData>>(`/admin/cost/session/${id}`)
}

// ===== 模型定价曲线（按真实公式逐 token 出点，后端算、前端只渲染）=====
// credits = Σ(段 token × 段费率) × timeCoeff × ctxCoeff ÷ R
// 售价线用外部模型配置、成本线用渠道内部模型配置，两条一起画。
export interface CostPricingRates {
  Input: number
  CacheRead: number
  Output: number
}
export interface CostPricingTier {
  Min: number
  Max: number | null // null = 无上限
  Coeff: number
}
export interface CostPricingPoint {
  Tokens: number // 上下文长度（input + cache_read），横轴
  InputTokens: number
  CacheReadTokens: number
  OutputTokens: number
  CtxCoeff: number
  Credits: number // 含系数（真实计费口径）
  CreditsNoCoeff: number // 不含任何系数
  OverTier: boolean // 落在分档之外：真实计费会失败，前端断线 + 红标
}
export interface CostPricingTierBreak {
  Tokens: number
  FromCoeff: number
  ToCoeff: number
}
export interface CostPricingOverRange {
  FromTokens: number
  ToTokens: number
}
export interface CostPricingLine {
  Available: boolean
  Source: string // external_model | channel_model
  Reason: string // Available=false 时的原因
  Name: string
  ExternalModelID: number
  ChannelModelID: number
  ChannelID: number
  ChannelName: string
  InternalModelID: string
  Rates: CostPricingRates
  TimeZone: string
  TimeCoeff: number
  TimeConfigSource: string // model | global
  TierSource: string // model | global | none
  Tiers: CostPricingTier[]
  Points: CostPricingPoint[]
  TierBreaks: CostPricingTierBreak[]
  OverTier: CostPricingOverRange[]
}
export interface CostTimeOption {
  Name: string
  Start: string
  End: string
  Coeff: number
}
export interface CostPricingCurveData {
  At: string
  TimeZone: string
  R: number
  Step: number
  MaxTokens: number
  PointCount: number
  HitRate: number
  InOutRatio: number
  TimeOptions: CostTimeOption[]
  DefaultTimeCoeff: number
  Sale: CostPricingLine
  Cost: CostPricingLine
  // 多线对比（仅传了 ChannelModelIDs 时出现，否则该键整个不出现）：每项一个 (渠道, 渠道内部模型)。
  // Available/Reason 取**成本线**判定；未传单线参数时顶层 Cost/Sale 是「未指定」的不可用线，
  // 多线模式一律读 Lines，不读顶层。
  Lines?: CostPricingMultiLine[]
}
// 多线对比里的一条线：LineIndex 为去重后的请求顺序下标（图例排序用）；
// ChannelModelID 是请求里给的渠道内部模型 ID（模型不存在也原样回吐，方便前端对号）。
export interface CostPricingMultiLine {
  LineIndex: number
  ChannelModelID: number
  ChannelID: number
  ChannelName: string
  InternalModelID: string
  ExternalModelID: number
  ExternalModelName: string
  Available: boolean
  Reason: string
  PointCount: number
  Cost: CostPricingLine
  Sale: CostPricingLine
}
export interface CostPricingCurveParams {
  ChannelModelID?: number
  ExternalModelID?: number
  ExternalModelName?: string
  // 多渠道多线对比：逗号分隔的渠道内部模型 ID，最多 10 个（超过后端 400，不静默截断）。
  // 不传（或空串）= 单线模式，响应与改造前逐字节一致。
  ChannelModelIDs?: string
  Step?: number
  MaxTokens?: number
  HitRate?: number
  InOutRatio?: number
  At?: string // RFC3339 或 HH:MM
}
// 第二个参数用于「开关未开 → 403 自己渲染」：silentStatuses 让全局拦截器不弹红条。
export function getCostPricingCurve(
  params: CostPricingCurveParams = {},
  opts: { silentStatuses?: number[] } = {},
) {
  return http.get<CostPricingCurveData, ApiRes<CostPricingCurveData>>('/admin/cost/pricing-curve', {
    params,
    silentStatuses: opts.silentStatuses,
  })
}

// ===== 会话渠道轨迹（只读）=====
// 数据源只有 billing_records：逐点渠道/模型/积分 → 连续段 → 真实切换点；从未真实切换过渠道时
// 额外给一个**预测打平点**（Predicted=true，明确是预测而非事实）。
// 口径：金额直接取账单落库值，不重算；Turn 只编成功轮，TurnNo 含失败轮（x 轴用 TurnNo）。
export interface CostTracePoint {
  Turn: number
  TurnNo: number
  Status: string // completed | failed | upstream_error（上游 4xx/5xx，金额 0）
  Failed: boolean
  CallTime: string
  ChannelID: number
  ChannelName: string
  ChannelModelID: number
  InternalModelID: string
  InputTokens: number
  CacheReadTokens: number
  OutputTokens: number
  CostCredits: number
  SaleCredits: number
  CumulativeCostCredits: number
  ChannelChanged: boolean // 真实切换点标记（本点渠道 ≠ 上一点渠道，第一点恒 false）
}
export interface CostTraceSegment {
  Seq: number
  ChannelID: number
  ChannelName: string
  ChannelModelID: number
  InternalModelID: string
  FromCallTime: string
  ToCallTime: string
  Points: number
  SuccessPoints: number
  CostCredits: number
  SaleCredits: number
  InputTokens: number
  CacheReadTokens: number
  OutputTokens: number
}
// 真实切换点：Kind 恒为 "actual"（枚举字面量由后端下发，前端不硬编码字符串）。
export interface CostTraceSwitch {
  Kind: string
  Seq: number
  TurnNo: number // 切换后第一轮的时间线序号
  AtCallTime: string
  FromChannelID: number
  FromChannelName: string
  FromChannelModelID: number
  FromInternalModelID: string
  ToChannelID: number
  ToChannelName: string
  ToChannelModelID: number
  ToInternalModelID: string
}
// 预测打平点：Kind 恒为 "predicted" 且 Predicted 恒 true。
// Matched=false 表示采样范围内没出现反超（Tokens 为 0，原因见 Reason）——前端如实展示，不自己补数。
export interface CostTraceBreakEven {
  Kind: string
  Predicted: boolean
  ChannelModelID: number
  ChannelID: number
  ChannelName: string
  InternalModelID: string
  AlternativeChannelModelID: number
  AlternativeChannelID: number
  AlternativeChannelName: string
  Tokens: number // 打平点上下文长度（输入 + 缓存读）
  Matched: boolean
  Step: number
  MaxTokens: number
  Reason: string
}
export interface CostTraceData {
  SessionID: string
  Points: CostTracePoint[]
  Segments: CostTraceSegment[]
  ActualSwitches: CostTraceSwitch[]
  // 为 null 的两种情形：① 已真实切换过渠道；② 算不出打平点。前端不占位、不补 0。
  PredictedBreakEven: CostTraceBreakEven | null
  Total: number
  Limit: number
  Truncated: boolean
  SwitchKindActual: string
  SwitchKindPredicted: string
}
export function getCostSessionTrace(
  id: string,
  params: { Limit?: number } = {},
  opts: { silentStatuses?: number[] } = {},
) {
  return http.get<CostTraceData, ApiRes<CostTraceData>>(`/admin/cost/session/${id}/trace`, {
    params,
    silentStatuses: opts.silentStatuses,
  })
}


// V3：对内模型成本价（唯一可写的镜像字段）
export function updateInternalCostRates(ref: string, rates: { input: number; output: number; cache_read: number }) {
  return http.put<unknown, unknown>(`/admin/internal-models/${encodeURIComponent(ref)}/cost-rates`, rates)
}


// 一键开通该令牌的代理访问（用系统配置的 callerKey 绑定；共享 key，按需再换独立 key）
export function autoBindMagpieKey(id: number | string) {
  return http.post<unknown, unknown>(`/admin/tokens/${id}/magpie-key/auto`, {})
}
