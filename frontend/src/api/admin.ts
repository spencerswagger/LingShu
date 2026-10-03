import { http, type ApiRes } from './http'
import type { PageResult, BillingDetail, FlowItem } from './dev'

// ===== 管理端 API（字段名与后端 handler 返回结构对齐） =====

// ===== 用户 =====
// 后端 identity.User 结构体无 json tag → 序列化为 PascalCase 字段。
export interface AdminUser {
  ID: number
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
  user: AdminUser
  wallet: { balance: number }
  recent_flows: FlowItem[]
}

// 用户列表（分页）
export function listUsers(params: Record<string, unknown> = {}) {
  return http.get<PageResult<AdminUser>, ApiRes<PageResult<AdminUser>>>('/admin/users', { params })
}
export function getUser(id: number) {
  return http.get<UserDetailData, ApiRes<UserDetailData>>(`/admin/users/${id}`)
}
// 创建用户：必填 username/password/role
export function createUser(payload: {
  username: string
  password: string
  nickname?: string
  role: string
  pricing_mode?: string
}) {
  return http.post<AdminUser, ApiRes<AdminUser>>('/admin/users', payload)
}
// 更新用户：role/status/pricing_mode/nickname
export function updateUser(
  id: number,
  payload: { role?: string; status?: string; pricing_mode?: string; nickname?: string },
) {
  return http.put<AdminUser, ApiRes<AdminUser>>(`/admin/users/${id}`, payload)
}
// 重置用户密码（管理员输入新密码）
export function resetUserPassword(id: number, password: string) {
  return http.post<{ reset: boolean }, ApiRes<{ reset: boolean }>>(
    `/admin/users/${id}/reset-password`,
    { password },
  )
}
// 强制解绑用户 TOTP（管理员操作，无需验证码）
export function resetUserTotp(id: number) {
  return http.post<{ reset: boolean }, ApiRes<{ reset: boolean }>>(`/admin/users/${id}/reset-totp`)
}
export function batchDeleteUsers(ids: number[]) {
  return http.post<{ affected: number }, ApiRes<{ affected: number }>>('/admin/users/batch-delete', {
    ids,
  })
}

// 用户钱包
export function getUserWallet(id: number) {
  return http.get<{ balance: number }, ApiRes<{ balance: number }>>(`/admin/users/${id}/wallet`)
}
export function rechargeWallet(id: number, amount: number, remark: string) {
  return http.post<{ affected: number }, ApiRes<{ affected: number }>>(
    `/admin/users/${id}/wallet/recharge`,
    { amount, remark },
  )
}
export function adjustWallet(id: number, amount: number, remark: string) {
  return http.post<{ affected: number }, ApiRes<{ affected: number }>>(
    `/admin/users/${id}/wallet/adjust`,
    { amount, remark },
  )
}
// 覆盖余额（直接 set）。
export function setWallet(id: number, balance: number, remark: string) {
  return http.post<{ balance: number }, ApiRes<{ balance: number }>>(
    `/admin/users/${id}/wallet/set`,
    { balance, remark },
  )
}
export function listUserFlows(id: number, page = 1, size = 20) {
  return http.get<PageResult<FlowItem>, ApiRes<PageResult<FlowItem>>>(
    `/admin/users/${id}/wallet/flows`,
    { params: { page, size } },
  )
}

// ===== 令牌 =====
// 后端令牌走专用 snake_case 响应。
export interface AdminToken {
  id: number
  token_display: string
  user_id: number
  username: string
  user_nickname?: string
  display_name: string
  tag_id: number | null
  expires_at: string | null
  last_used_at: string | null
  status: string // ACTIVE | DISABLED
  created_at: string
}
export interface CreatedToken {
  plain: string
  display: string
}

export function listTokens(params: Record<string, unknown> = {}) {
  return http.get<PageResult<AdminToken>, ApiRes<PageResult<AdminToken>>>('/admin/tokens', {
    params,
  })
}
export function createToken(payload: {
  user_id: number
  display_name: string
  tag_id?: number | null
  expires_at?: string | null
}) {
  return http.post<CreatedToken, ApiRes<CreatedToken>>('/admin/tokens', payload)
}
// 查看令牌密钥（SM4 加密落库，可随时查看/复制）
export function getTokenSecret(id: number) {
  return http.get<CreatedToken, ApiRes<CreatedToken>>(`/admin/tokens/${id}/secret`)
}
export function batchDeleteTokens(ids: number[]) {
  return http.post<{ affected: number }, ApiRes<{ affected: number }>>('/admin/tokens/batch-delete', {
    ids,
  })
}

// ===== 渠道 =====
// 后端 channel.Channel 无 json tag → 外层 PascalCase；内嵌 JSONB 结构采用小写 json tag。
export interface RateLimitConfig {
  rpm: number
  tpm: number
  burst_multiplier: number
  on_exceed: string // QUEUE | REJECT
  queue_size: number
  queue_timeout_ms: number
  max_concurrent: number
}
export interface HealthProbeConfig {
  interval: string // 正常态探测频率（6 段 cron，含秒）
  drain_interval_seconds: number // 排空态探测间隔（秒）
  timeout_ms: number
  fail_threshold: number
  recovery_threshold: number
  probe_model: string
}
// 可靠性（真实调用滑动窗口评估），独立于健康探测
export interface ReliabilityConfig {
  window_seconds: number
  min_samples: number
  error_rate_pct: number
  rate_429_pct: number
  p99_latency_ms: number
  auth_fail_threshold?: number
}
export interface AdminChannel {
  ID: number
  Name: string
  Protocol: string
  BaseURL: string
  Tags: Record<string, string>
  TagIDs: number[]
  BoundTags: { ID: number; Name: string; KV: Record<string, string> }[]
  Priority: number
  Weight: number
  State: string // NORMAL | DRAIN | DISABLED
  RateLimit: RateLimitConfig
  HealthProbe: HealthProbeConfig
  Reliability: ReliabilityConfig
  SessionTTLMinutes: number
  Enabled: boolean
  KeyStates?: { key_id: number; key_name: string; state: string }[]
  CreatedAt: string
  UpdatedAt: string
}
// 渠道入参（创建/更新共用）
export interface ChannelInput {
  name: string
  protocol: string
  base_url: string
  tags?: Record<string, string>
  tag_ids?: number[]
  priority?: number
  weight?: number
  rate_limit?: RateLimitConfig
  health_probe?: HealthProbeConfig
  reliability?: ReliabilityConfig
  session_ttl_minutes?: number
  enabled?: boolean
}
// 渠道状态流转事件
export interface ChannelEvent {
  ID: number
  ChannelID: number
  FromState: string
  ToState: string
  Reason: string
  CreatedAt: string
}
// 内部模型状态流转事件
export interface ChannelModelEvent {
  ID: number
  ChannelID: number
  ModelID: string
  FromState: string
  ToState: string
  Reason: string
  CreatedAt: string
}
// 探测历史
export interface ProbeLog {
  ID: number
  ChannelID: number
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
export function updateChannel(id: number, payload: ChannelInput) {
  return http.put<AdminChannel, ApiRes<AdminChannel>>(`/admin/channels/${id}`, payload)
}
export function batchDeleteChannels(ids: number[]) {
  return http.post<{ deleted: number }, ApiRes<{ deleted: number }>>('/admin/channels/batch-delete', {
    ids,
  })
}
export function listChannelEvents(id: number) {
  return http.get<ChannelEvent[], ApiRes<ChannelEvent[]>>(`/admin/channels/${id}/events`)
}
// 手动状态流转（渠道与内部模型共用）：action = normal | drain | disable
export function channelState(id: number, action: 'normal' | 'drain' | 'disable', reason = '') {
  return http.post<AdminChannel, ApiRes<AdminChannel>>(`/admin/channels/${id}/state`, {
    action,
    reason,
  })
}
// 内部模型手动状态流转
export function channelModelState(id: number, mid: number, action: 'normal' | 'drain' | 'disable', reason = '') {
  return http.put<unknown, ApiRes<unknown>>(`/admin/channels/${id}/models/${mid}/state`, {
    action,
    reason,
  })
}
export function listModelEvents(id: number) {
  return http.get<ChannelModelEvent[], ApiRes<ChannelModelEvent[]>>(`/admin/channels/${id}/model-events`)
}
export function listProbeLogs(id: number, limit = 50) {
  return http.get<ProbeLog[], ApiRes<ProbeLog[]>>(`/admin/channels/${id}/probe-logs`, { params: { limit } })
}
// cron 表达式后续执行时间预览（前端编辑时校验用）
export function cronPreview(expr: string, limit = 5) {
  return http.get<{ times: string[] }, ApiRes<{ times: string[] }>>('/admin/channels/cron-preview', {
    params: { expr, limit },
  })
}
// 运行时快照（snake_case 字段，含 last_err）
export function getChannelSnapshot() {
  return http.get<RuntimeChannel[], ApiRes<RuntimeChannel[]>>('/admin/channels/snapshot')
}
export interface RuntimeChannel {
  id: number
  name: string
  protocol: string
  base_url: string
  tags: Record<string, string>
  priority: number
  state: string
  last_err?: string
  last_probe?: string
  rate_limit: RateLimitConfig
  health_probe: HealthProbeConfig
  reliability: ReliabilityConfig
  max_concurrent: number
  session_ttl_minutes: number
  enabled: boolean
  created_at: string
  updated_at: string
}

// 渠道密钥（channel_keys·运行时实体）：凭据只出尾号，状态三态独立流转
export interface ChannelKey {
  id: number
  channel_id: number
  name: string
  credential_tail: string
  state: string // NORMAL | DRAIN | DISABLED
  last_err: string
  active_sessions: number
  created_at: string
  updated_at: string
}
export function listChannelKeys(id: number): Promise<ApiRes<ChannelKey[]>> {
  return http.get<ChannelKey[], ApiRes<ChannelKey[]>>(`/admin/channels/${id}/keys`)
}
export function createChannelKey(
  id: number,
  payload: { name: string; credential: string },
): Promise<ApiRes<ChannelKey>> {
  return http.post<ChannelKey, ApiRes<ChannelKey>>(`/admin/channels/${id}/keys`, payload)
}
export function updateChannelKey(
  channelId: number,
  keyId: number,
  payload: { name: string; credential?: string },
): Promise<ApiRes<ChannelKey>> {
  return http.put<ChannelKey, ApiRes<ChannelKey>>(
    `/admin/channels/${channelId}/keys/${keyId}`,
    payload,
  )
}
export function deleteChannelKey(channelId: number, keyId: number): Promise<ApiRes<unknown>> {
  return http.delete<unknown, ApiRes<unknown>>(`/admin/channels/${channelId}/keys/${keyId}`)
}
export function channelKeyState(
  channelId: number,
  keyId: number,
  action: string,
): Promise<ApiRes<ChannelKey>> {
  return http.post<ChannelKey, ApiRes<ChannelKey>>(
    `/admin/channels/${channelId}/keys/${keyId}/state`,
    { action },
  )
}

// ===== 标签 =====
// 后端 tag.Tag 无 json tag → PascalCase。
export interface AdminTag {
  ID: number
  Name: string
  Description: string
  KVPairs: Record<string, string>
  Enabled: boolean
  CreatedBy: number
  CreatedAt: string
}
export interface TagInput {
  name: string
  description?: string
  kv_pairs?: Record<string, string>
  enabled?: boolean
}
export function listTags(params: Record<string, unknown> = {}) {
  return http.get<AdminTag[], ApiRes<AdminTag[]>>('/admin/tags', { params })
}
export function createTag(payload: TagInput) {
  return http.post<AdminTag, ApiRes<AdminTag>>('/admin/tags', payload)
}
export function updateTag(id: number, payload: TagInput) {
  return http.put<AdminTag, ApiRes<AdminTag>>(`/admin/tags/${id}`, payload)
}
export function batchDeleteTags(ids: number[]) {
  return http.post<{ deleted: number }, ApiRes<{ deleted: number }>>('/admin/tags/batch-delete', {
    ids,
  })
}

// ===== 五段计价费率（售价/成本对称的公共定价 schema）=====
// 与后端 billing.Rates / model.RateKeys 对齐，键必须齐全且非负。
export const rateKeys = ['input', 'output', 'cache_read', 'cache_write', 'reasoning'] as const
export const rateLabels: Record<string, string> = {
  input: '输入',
  output: '输出',
  cache_read: '缓存读',
  cache_write: '缓存写',
  reasoning: '推理',
}
// 售价/成本五段倍率的 tooltip 解释（倍率 = 每百万 token 计费数值，照抄官方定价录入）
export const rateHints: Record<string, string> = {
  input: '输入倍率（每百万 token）：与官方定价数值一致，直接填官网价格即可。',
  output: '输出倍率（每百万 token）：模型生成回复内容所计费。',
  cache_read: '缓存读倍率（每百万 token）：命中提示词缓存的输入按此低价计费。',
  cache_write: '缓存写倍率（每百万 token）：首次写入提示词缓存时按此单价计费。',
  reasoning: '推理倍率（每百万 token）：思维链/深度推理 token 单独计费费率。',
}

// ===== 对外模型（external_models·售价层）=====
// 后端 model.Handler viewModel 显式 snake_case。
export interface ExternalModel {
  id: number
  external_name: string
  description: string
  enabled: boolean
  sale_rates: Record<string, number>
  time_config: TimeCoeffConfig | null
  context_tiers: ContextTier[] | null
  created_at: string
  updated_at: string
}
export interface ExternalModelInput {
  external_name: string
  description?: string
  enabled?: boolean
  sale_rates: Record<string, number>
  time_config?: TimeCoeffConfig | null
  context_tiers?: ContextTier[] | null
}
// 列表返回裸数组（非 {list}）
export function listExternalModels(params: Record<string, unknown> = {}) {
  return http.get<ExternalModel[], ApiRes<ExternalModel[]>>('/admin/models', { params })
}
export function createExternalModel(payload: ExternalModelInput) {
  return http.post<ExternalModel, ApiRes<ExternalModel>>('/admin/models', payload)
}
export function updateExternalModel(id: number, payload: ExternalModelInput) {
  return http.put<ExternalModel, ApiRes<ExternalModel>>(`/admin/models/${id}`, payload)
}
export function batchDeleteExternalModels(ids: number[]) {
  return http.post<{ deleted: number }, ApiRes<{ deleted: number }>>('/admin/models/batch-delete', {
    ids,
  })
}

// models.dev 参考价（查看 + 一键应用为售价）
export interface PriceReferenceResult {
  reference: { input: number; output: number }
  updated_at: string
}
export function getPriceReference(id: number) {
  return http.get<PriceReferenceResult, ApiRes<PriceReferenceResult>>(
    `/admin/models/${id}/price-reference`,
  )
}
// 应用 models.dev 参考价到售价 input/output，返回最新对外模型
export function applyPrice(id: number) {
  return http.post<ExternalModel, ApiRes<ExternalModel>>(`/admin/models/${id}/apply-price`)
}
// 用 models.dev 参考价批量刷新全部对外模型售价；返回更新/跳过清单
export interface SyncPricesResult {
  updated: string[]
  skipped: string[]
}
export function syncModelsPrices() {
  return http.post<SyncPricesResult, ApiRes<SyncPricesResult>>('/admin/models/sync-prices')
}
// models.dev 价格目录条目（供应商维度）；价格均为参考原始价 / 百万 token
export interface CatalogEntry {
  provider: string
  model_id: string
  input_usd: number
  output_usd: number
  cache_read_usd: number
  cache_write_usd: number
  reasoning_usd: number
}
export function priceCatalog(q: string) {
  return http.get<{ list: CatalogEntry[]; total: number; updated_at: string }, ApiRes<{ list: CatalogEntry[]; total: number; updated_at: string }>>(
    '/admin/models/price-catalog',
    { params: { q: q || undefined } },
  )
}

// ===== 渠道内部模型（channel_models·成本层）=====
// 后端 channel.Handler viewChannelModel 含 JOIN 出的 external_name。
export interface ChannelModel {
  id: number
  channel_id: number
  internal_model_id: string
  external_model_id: number
  external_name?: string
  cost_rates: Record<string, number>
  time_config: TimeCoeffConfig | null
  context_tiers: ContextTier[] | null
  state: string // NORMAL | DRAIN | DISABLED
  rate_limit: RateLimitConfig
  health_probe: HealthProbeConfig
  reliability: ReliabilityConfig
  enabled: boolean
  created_at: string
  updated_at: string
}
export interface ChannelModelInput {
  internal_model_id: string
  external_model_id: number
  cost_rates: Record<string, number>
  time_config?: TimeCoeffConfig | null
  context_tiers?: ContextTier[] | null
  rate_limit?: RateLimitConfig
  health_probe?: HealthProbeConfig
  reliability?: ReliabilityConfig
  enabled?: boolean
}
export function listChannelModels(channelId: number) {
  return http.get<ChannelModel[], ApiRes<ChannelModel[]>>(`/admin/channels/${channelId}/models`)
}
export function createChannelModel(channelId: number, payload: ChannelModelInput) {
  return http.post<ChannelModel, ApiRes<ChannelModel>>(
    `/admin/channels/${channelId}/models`,
    payload,
  )
}
export function updateChannelModel(channelId: number, mid: number, payload: ChannelModelInput) {
  return http.put<ChannelModel, ApiRes<ChannelModel>>(
    `/admin/channels/${channelId}/models/${mid}`,
    payload,
  )
}
export function deleteChannelModel(channelId: number, mid: number) {
  return http.delete<{ affected: number }, ApiRes<{ affected: number }>>(
    `/admin/channels/${channelId}/models/${mid}`,
  )
}
// 拉取渠道上游 /v1/models 列表（不落库），供管理员选择填入内部模型
export interface PullModel {
  id: string
  object: string
  owned_by: string
}
export function pullChannelModels(channelId: number) {
  return http.post<{ list: PullModel[] }, ApiRes<{ list: PullModel[] }>>(
    `/admin/channels/${channelId}/models/pull`,
  )
}

// ===== 账单 =====
export interface BillingUsage {
  input: number
  output: number
  cache_read: number
  cache_write: number
  reasoning: number
}
export interface AdminBillingItem {
  billing_id: string
  user_id: number
  username?: string
  user_nickname?: string
  token_name?: string
  model: string
  pricing_mode: string
  credits_consumed: number
  status: string
  call_time: string
  internal_model_id: string
  channel_id: number
  channel_name?: string
  channel_key_id?: number
  key_name?: string
  session_id?: string
  session_name?: string
  tokens: BillingUsage
  rates?: Record<string, number>
  coeff_time?: number
  coeff_context?: number
  r_value?: number
  error_message?: string
  duration_ms?: number | null
  first_token_ms?: number | null
}
export function listBillings(params: Record<string, unknown> = {}) {
  return http.get<PageResult<AdminBillingItem>, ApiRes<PageResult<AdminBillingItem>>>(
    '/admin/billings',
    { params },
  )
}
// 账单筛选聚合统计（与列表同 filter）：请求数/总积分/总 token/总耗时（统计栏用）。
export interface BillingStats {
  requests: number
  credits_total: number
  tokens_total: number
  duration_total_ms: number
}
export function getBillingStats(params: Record<string, unknown> = {}) {
  return http.get<BillingStats, ApiRes<BillingStats>>('/admin/billings/stats', { params })
}
export function getBilling(billingId: string) {
  return http.get<BillingDetail, ApiRes<BillingDetail>>(`/admin/billings/${billingId}`)
}

// ===== 会话（内存注册表 + DB 投影；按用户/令牌/密钥维度过滤与踢下线） =====
export interface AdminSession {
  session_id: string
  name: string
  user_name: string
  user_nickname?: string
  token_name: string
  model: string
  channel_key_name: string
  session_raw: string
  closed: boolean
  created_at: string
  last_active: string
  expire_at: string
  expired: boolean
}
export function listSessions(
  params: Record<string, any>,
): Promise<ApiRes<{ list: AdminSession[]; total: number }>> {
  return http.get<
    { list: AdminSession[]; total: number },
    ApiRes<{ list: AdminSession[]; total: number }>
  >('/admin/sessions', { params })
}
export function kickSessions(
  payload: Record<string, any>,
): Promise<ApiRes<{ affected: number }>> {
  return http.post<{ affected: number }, ApiRes<{ affected: number }>>(
    '/admin/sessions/kick',
    payload,
  )
}
export function renameSession(
  sessionId: string,
  name: string,
): Promise<ApiRes<{ updated: boolean }>> {
  return http.put<{ updated: boolean }, ApiRes<{ updated: boolean }>>(
    `/admin/sessions/${sessionId}/name`,
    { name },
  )
}

// ===== 计费配置 =====
export interface ContextTier {
  min: number
  max: number | null
  coeff: number
}
export interface Segment {
  name: string
  start: string // "HH:MM"
  end: string // "HH:MM" 或 "24:00"
  coeff: number
}
export interface DateOverride {
  name: string
  start: string // "YYYY-MM-DD"
  end: string
  segments: Segment[]
}
export interface TimeCoeffConfig {
  timezone: string
  default_coeff: number
  periodic_segments: Segment[]
  date_overrides: DateOverride[]
}
// r 可能为数字或数字字符串
export interface BillingConfigData {
  r: number | string
  cny_rate: number | string
  credit_value: number | string // 积分价值 V=R/1e6（每积分对应人民币元），只读展示
  context_tiers: ContextTier[]
  time_config: TimeCoeffConfig
}
export function getBillingConfig() {
  return http.get<BillingConfigData, ApiRes<BillingConfigData>>('/admin/configs/billing')
}
export function putBillingConfig(payload: {
  r: number | string
  cny_rate?: number | string
  context_tiers: ContextTier[]
  time_config: TimeCoeffConfig
}) {
  return http.put<{ affected: number }, ApiRes<{ affected: number }>>('/admin/configs/billing', payload)
}

// ===== 公告 =====
export interface AdminAnnouncement {
  id: number
  title: string
  content: string
  level: string // info | warning | danger
  publish_at?: string
  expire_at?: string
  enabled: boolean
  created_at: string
}
export interface AnnouncementInput {
  title: string
  content: string
  level: string
  publish_at?: string | null
  expire_at?: string | null
  enabled?: boolean
}
export function listAnnouncements() {
  return http.get<{ list: AdminAnnouncement[] }, ApiRes<{ list: AdminAnnouncement[] }>>(
    '/admin/announcements',
  )
}
export function createAnnouncement(payload: AnnouncementInput) {
  return http.post<AdminAnnouncement, ApiRes<AdminAnnouncement>>('/admin/announcements', payload)
}
export function updateAnnouncement(id: number, payload: AnnouncementInput) {
  return http.put<AdminAnnouncement, ApiRes<AdminAnnouncement>>(`/admin/announcements/${id}`, payload)
}
export function batchDeleteAnnouncements(ids: number[]) {
  return http.post<{ deleted: number }, ApiRes<{ deleted: number }>>(
    '/admin/announcements/batch-delete',
    { ids },
  )
}

// ===== 价格同步：关注列表 + 告警 =====
export interface WatchlistItem {
  id: number
  external_model_id: string
  local_model_name: string
  alert_on_change: boolean
  last_synced_at?: string
  created_at: string
}
export function listWatchlist() {
  return http.get<{ list: WatchlistItem[] }, ApiRes<{ list: WatchlistItem[] }>>('/admin/watchlist')
}
export function upsertWatchlist(payload: {
  external_model_id: string
  local_model_name: string
  alert_on_change: boolean
}) {
  return http.post<WatchlistItem, ApiRes<WatchlistItem>>('/admin/watchlist', payload)
}
export function batchDeleteWatchlist(ids: number[]) {
  return http.post<{ deleted: number }, ApiRes<{ deleted: number }>>(
    '/admin/watchlist/batch-delete',
    { ids },
  )
}
export function runSync() {
  return http.post<{ summary: string }, ApiRes<{ summary: string }>>('/admin/sync/run')
}

export interface SyncChange {
  old: number
  new: number
  change_pct: number
}
export interface SyncAlert {
  id: number
  external_model_id: string
  local_model_name: string
  changes: Record<string, SyncChange>
  status: string // pending | resolved | ignored
  detected_at: string
  resolved_at?: string
}
export function listAlerts(params: Record<string, unknown> = {}) {
  return http.get<{ list: SyncAlert[] }, ApiRes<{ list: SyncAlert[] }>>('/admin/sync/alerts', {
    params,
  })
}
export function resolveAlert(id: number) {
  return http.post<{ affected: number }, ApiRes<{ affected: number }>>(
    `/admin/sync/alerts/${id}/resolve`,
  )
}