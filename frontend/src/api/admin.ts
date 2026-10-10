import { http, type ApiRes } from './http'
import type { PageResult, BillingDetail, FlowItem } from './dev'

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
  TagID: string | null
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
  TagID?: string | null
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

// ===== 渠道 =====
// 渠道级/模型级限流与可靠性为公共 JSONB 结构（字段名 = Go 字段名）。
export interface RateLimitConfig {
  RPM: number // 每分钟请求数；0 表示不限
  TPM: number // 每分钟 token 数；0 表示不限
  BurstMultiplier: number // 瞬时超发系数
  OnExceed: string // QUEUE 或 REJECT
  QueueSize: number
  QueueTimeoutMS: number
  MaxConcurrent: number // 同时进行的请求上限（并发会话数）；0 表示不限
}
export interface HealthProbeConfig {
  Interval: string // 正常态探测频率（6 段 cron，含秒）
  DrainIntervalSeconds: number // 排空态探测间隔（秒）
  TimeoutMS: number
  FailThreshold: number
  RecoveryThreshold: number
  ProbeModel: string
}
// 可靠性（真实调用滑动窗口评估），独立于健康探测
export interface ReliabilityConfig {
  WindowSeconds: number
  MinSamples: number
  ErrorRatePct: number
  Rate429Pct: number
  P99LatencyMS: number
  AuthFailThreshold?: number
}
export interface AdminChannel {
  ID: string
  Name: string
  Protocol: string
  BaseURL: string
  Tags: Record<string, string>
  TagIDs: string[]
  BoundTags: { ID: string; Name: string; KV: Record<string, string> }[]
  Priority: number
  Weight: number
  State: string // NORMAL | DRAIN | DISABLED
  RateLimit: RateLimitConfig
  HealthProbe: HealthProbeConfig
  Reliability: ReliabilityConfig
  SessionTTLMinutes: number
  KeyStates?: { KeyID: string; KeyName: string; State: string }[]
  CreatedAt: string
  UpdatedAt: string
}
// 渠道入参（创建/更新共用）
export interface ChannelInput {
  Name: string
  Protocol: string
  BaseURL: string
  TagIDs?: string[]
  Priority?: number
  Weight?: number
  RateLimit?: RateLimitConfig
  HealthProbe?: HealthProbeConfig
  Reliability?: ReliabilityConfig
  SessionTTLMinutes?: number
}
// 渠道状态流转事件
export interface ChannelEvent {
  ID: string
  ChannelID: string
  FromState: string
  ToState: string
  Reason: string
  CreatedAt: string
}
// 内部模型状态流转事件
export interface ChannelModelEvent {
  ID: string
  ChannelID: string
  ModelID: string
  FromState: string
  ToState: string
  Reason: string
  CreatedAt: string
}
// 探测历史
export interface ProbeLog {
  ID: string
  ChannelKeyID: string
  ModelID: string
  Level: string // key=密钥级探测；model=模型级探测（探针标识的探测模型）
  Target: string
  OK: boolean
  Error: string
  InputTokens: number
  OutputTokens: number
  CachedTokens: number
  TotalTokens: number
  DurationMS: number
  CreatedAt: string
}

// 列表返回裸数组（非 {list}）
export function listChannels(params: Record<string, unknown> = {}) {
  return http.get<AdminChannel[], ApiRes<AdminChannel[]>>('/admin/channels', { params })
}
export function createChannel(payload: ChannelInput) {
  return http.post<AdminChannel, ApiRes<AdminChannel>>('/admin/channels', payload)
}
export function updateChannel(id: string, payload: ChannelInput) {
  return http.put<AdminChannel, ApiRes<AdminChannel>>(`/admin/channels/${id}`, payload)
}
export function batchDeleteChannels(ids: string[]) {
  return http.post<{ Deleted: number }, ApiRes<{ Deleted: number }>>('/admin/channels/batch-delete', {
    IDs: ids,
  })
}
export function listChannelEvents(id: string) {
  return http.get<ChannelEvent[], ApiRes<ChannelEvent[]>>(`/admin/channels/${id}/events`)
}
// 手动状态流转（渠道与内部模型共用）：action = normal | drain | disable
export function channelState(id: string, action: 'normal' | 'drain' | 'disable', reason = '') {
  return http.post<AdminChannel, ApiRes<AdminChannel>>(`/admin/channels/${id}/state`, {
    Action: action,
    Reason: reason,
  })
}
// 内部模型手动状态流转
export function channelModelState(id: string, mid: string, action: 'normal' | 'drain' | 'disable', reason = '') {
  return http.put<unknown, ApiRes<unknown>>(`/admin/channels/${id}/models/${mid}/state`, {
    Action: action,
    Reason: reason,
  })
}
export function listModelEvents(id: string) {
  return http.get<ChannelModelEvent[], ApiRes<ChannelModelEvent[]>>(`/admin/channels/${id}/model-events`)
}
export function listProbeLogs(id: string, limit = 50) {
  return http.get<ProbeLog[], ApiRes<ProbeLog[]>>(`/admin/channels/${id}/probe-logs`, { params: { Limit: limit } })
}
// cron 表达式后续执行时间预览（前端编辑时校验用）
export function cronPreview(expr: string, limit = 5) {
  return http.get<{ Times: string[] }, ApiRes<{ Times: string[] }>>('/admin/channels/cron-preview', {
    params: { Expr: expr, Limit: limit },
  })
}

// 渠道密钥（channel_keys·运行时实体）：凭据只出尾号，状态三态独立流转
export interface ChannelKey {
  ID: string
  ChannelID: string
  Name: string
  CredentialTail: string
  State: string // NORMAL | DRAIN | DISABLED
  LastErr: string
  ActiveSessions: number
  CreatedAt: string
  UpdatedAt: string
}
export function listChannelKeys(id: string): Promise<ApiRes<ChannelKey[]>> {
  return http.get<ChannelKey[], ApiRes<ChannelKey[]>>(`/admin/channels/${id}/keys`)
}
export function createChannelKey(
  id: string,
  payload: { Name: string; Credential: string },
): Promise<ApiRes<ChannelKey>> {
  return http.post<ChannelKey, ApiRes<ChannelKey>>(`/admin/channels/${id}/keys`, payload)
}
export function updateChannelKey(
  channelId: string,
  keyId: string,
  payload: { Name: string; Credential?: string },
): Promise<ApiRes<ChannelKey>> {
  return http.put<ChannelKey, ApiRes<ChannelKey>>(
    `/admin/channels/${channelId}/keys/${keyId}`,
    payload,
  )
}
export function deleteChannelKey(channelId: string, keyId: string): Promise<ApiRes<unknown>> {
  return http.delete<unknown, ApiRes<unknown>>(`/admin/channels/${channelId}/keys/${keyId}`)
}
export function channelKeyState(
  channelId: string,
  keyId: string,
  action: string,
): Promise<ApiRes<ChannelKey>> {
  return http.post<ChannelKey, ApiRes<ChannelKey>>(
    `/admin/channels/${channelId}/keys/${keyId}/state`,
    { Action: action },
  )
}

// 手动触发一轮健康探测的结果（密钥/内部模型探测共用）。
export interface ProbeOutcome {
  OK: boolean
  Error?: string
  DurationMS: number
  ModelID?: string
  State: string // 探测后的状态机状态：NORMAL | DRAIN | DISABLED
}
// 手动探测单个渠道密钥（同步执行一轮，结果按定时探测同规则驱动密钥状态机）
export function probeChannelKey(channelId: string, keyId: string): Promise<ApiRes<ProbeOutcome>> {
  return http.post<ProbeOutcome, ApiRes<ProbeOutcome>>(
    `/admin/channels/${channelId}/keys/${keyId}/probe`,
  )
}
// 手动探测单个渠道内部模型（经该渠道可用密钥发起，仅回喂模型状态机）
export function probeChannelModel(channelId: string, mid: string): Promise<ApiRes<ProbeOutcome>> {
  return http.post<ProbeOutcome, ApiRes<ProbeOutcome>>(
    `/admin/channels/${channelId}/models/${mid}/probe`,
  )
}

// ===== 标签 =====
export interface AdminTag {
  ID: string
  Name: string
  Description: string
  KVPairs: Record<string, string>
  Enabled: boolean
  CreatedBy: string
  CreatedAt: string
}
export interface TagInput {
  Name: string
  Description?: string
  KVPairs?: Record<string, string>
  Enabled?: boolean
}
export function listTags(params: Record<string, unknown> = {}) {
  return http.get<AdminTag[], ApiRes<AdminTag[]>>('/admin/tags', { params })
}
export function createTag(payload: TagInput) {
  return http.post<AdminTag, ApiRes<AdminTag>>('/admin/tags', payload)
}
export function updateTag(id: string, payload: TagInput) {
  return http.put<AdminTag, ApiRes<AdminTag>>(`/admin/tags/${id}`, payload)
}
export function batchDeleteTags(ids: string[]) {
  return http.post<{ Deleted: number }, ApiRes<{ Deleted: number }>>('/admin/tags/batch-delete', {
    IDs: ids,
  })
}

// ===== 五段计价费率（售价/成本对称的公共定价 schema）=====
// 与后端 billing.Rates / model.RateKeys 对齐，键必须齐全且非负。
export const rateKeys = ['Input', 'Output', 'CacheRead', 'CacheWrite', 'Reasoning'] as const
export const rateLabels: Record<string, string> = {
  Input: '输入',
  Output: '输出',
  CacheRead: '缓存读',
  CacheWrite: '缓存写',
  Reasoning: '推理',
}
// 售价/成本五段倍率的 tooltip 解释（倍率 = 每百万 token 计费数值，照抄官方定价录入）
export const rateHints: Record<string, string> = {
  Input: '输入倍率（每百万 token）：与官方定价数值一致，直接填官网价格即可。',
  Output: '输出倍率（每百万 token）：模型生成回复内容所计费。',
  CacheRead: '缓存读倍率（每百万 token）：命中提示词缓存的输入按此低价计费。',
  CacheWrite: '缓存写倍率（每百万 token）：首次写入提示词缓存时按此单价计费。',
  Reasoning: '推理倍率（每百万 token）：思维链/深度推理 token 单独计费费率。',
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
}
// 列表返回裸数组（非 {list}）
export function listExternalModels(params: Record<string, unknown> = {}) {
  return http.get<ExternalModel[], ApiRes<ExternalModel[]>>('/admin/models', { params })
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
  CacheWriteUSD: number
  ReasoningUSD: number
}
export function priceCatalog(q: string) {
  return http.get<{ List: CatalogEntry[]; Total: number; UpdatedAt: string }, ApiRes<{ List: CatalogEntry[]; Total: number; UpdatedAt: string }>>(
    '/admin/models/price-catalog',
    { params: { Q: q || undefined } },
  )
}

// ===== 渠道内部模型（channel_models·成本层；字段名 = Go 字段名）=====
// 含 JOIN 出的 external_name。
export interface ChannelModel {
  ID: string
  ChannelID: string
  InternalModelID: string
  ExternalModelID: string
  ExternalName?: string
  ChannelName?: string
  CostRates: Record<string, number>
  TimeConfig: TimeCoeffConfig | null
  ContextTiers: ContextTier[] | null
  State: string // NORMAL | DRAIN | DISABLED
  RateLimit: RateLimitConfig
  HealthProbe: HealthProbeConfig
  Reliability: ReliabilityConfig
  CreatedAt: string
  UpdatedAt: string
}
export interface ChannelModelInput {
  InternalModelID: string
  ExternalModelID: string
  CostRates: Record<string, number>
  TimeConfig?: TimeCoeffConfig | null
  ContextTiers?: ContextTier[] | null
  RateLimit?: RateLimitConfig
  HealthProbe?: HealthProbeConfig
  Reliability?: ReliabilityConfig
}
export function listChannelModels(channelId: string) {
  return http.get<ChannelModel[], ApiRes<ChannelModel[]>>(`/admin/channels/${channelId}/models`)
}
// 全部渠道内部模型（跨渠道；用于对外模型定价同步时选择来源）
export function listAllChannelModels() {
  return http.get<ChannelModel[], ApiRes<ChannelModel[]>>('/admin/channel-models')
}
export function createChannelModel(channelId: string, payload: ChannelModelInput) {
  return http.post<ChannelModel, ApiRes<ChannelModel>>(
    `/admin/channels/${channelId}/models`,
    payload,
  )
}
export function updateChannelModel(channelId: string, mid: string, payload: ChannelModelInput) {
  return http.put<ChannelModel, ApiRes<ChannelModel>>(
    `/admin/channels/${channelId}/models/${mid}`,
    payload,
  )
}
export function deleteChannelModel(channelId: string, mid: string) {
  return http.delete<{ Affected: number }, ApiRes<{ Affected: number }>>(
    `/admin/channels/${channelId}/models/${mid}`,
  )
}
// 拉取渠道上游 /v1/models 列表（不落库），供管理员选择填入内部模型
export interface PullModel {
  ID: string
  Object: string
  OwnedBy: string
}
export function pullChannelModels(channelId: string) {
  return http.post<{ List: PullModel[] }, ApiRes<{ List: PullModel[] }>>(
    `/admin/channels/${channelId}/models/pull`,
  )
}

// ===== 账单 =====
export interface BillingUsage {
  Input: number
  Output: number
  CacheRead: number
  CacheWrite: number
  Reasoning: number
}
export interface AdminBillingItem {
  BillingID: string
  UserID: string
  Username?: string
  UserNickname?: string
  TokenName?: string
  ExternalModel: string
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
}
export function listBillings(params: Record<string, unknown> = {}) {
  return http.get<PageResult<AdminBillingItem>, ApiRes<PageResult<AdminBillingItem>>>(
    '/admin/billings',
    { params },
  )
}
// 账单筛选聚合统计（与列表同 filter）：请求数/总积分/总 token/总耗时（统计栏用）。
export interface BillingStats {
  Requests: number
  CreditsTotal: number
  TokensTotal: number
  DurationTotalMS: number
}
export function getBillingStats(params: Record<string, unknown> = {}) {
  return http.get<BillingStats, ApiRes<BillingStats>>('/admin/billings/stats', { params })
}
export function getBilling(billingId: string) {
  return http.get<BillingDetail, ApiRes<BillingDetail>>(`/admin/billings/${billingId}`)
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
  SessionRaw: string
  SessionName: string
  Closed: boolean
  CreatedAt: string
  LastActive: string
  ExpireAt: string
  Expired: boolean
}
export function listSessions(
  params: Record<string, any>,
): Promise<ApiRes<{ List: AdminSession[]; Total: number }>> {
  return http.get<
    { List: AdminSession[]; Total: number },
    ApiRes<{ List: AdminSession[]; Total: number }>
  >('/admin/sessions', { params })
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
export interface BillingConfigData {
  R: number | string
  CNYRate: number | string
  CreditValue: number | string // 积分价值 V=R/1e6（每积分对应人民币元），只读展示
  ContextTiers: ContextTier[]
  TimeConfig: TimeCoeffConfig
}
export function getBillingConfig() {
  return http.get<BillingConfigData, ApiRes<BillingConfigData>>('/admin/configs/billing')
}
export function putBillingConfig(payload: {
  R: number | string
  CnyRate?: number | string
  ContextTiers: ContextTier[]
  TimeConfig: TimeCoeffConfig
}) {
  return http.put<{ Affected: number }, ApiRes<{ Affected: number }>>('/admin/configs/billing', payload)
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
