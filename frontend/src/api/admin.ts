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
  P99LatencyMS: number // 单位毫秒（后端契约不变；默认 120000ms=120 秒，前端按秒录入/展示，提交转毫秒）
  AuthFailThreshold?: number
}

// 渠道视图中的逐密钥状态快照（与后端 ChannelKeyState 对齐）
export interface ChannelKeyState {
  KeyID: string
  KeyName: string
  State: string
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
  RateLimit: RateLimitConfig
  HealthProbe: HealthProbeConfig
  Reliability: ReliabilityConfig
  ChannelProtocolID?: number | string | null // 引用的渠道协议 ID；空=使用渠道内联配置
  KeyStates?: ChannelKeyState[]
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
  RateLimit?: RateLimitConfig
  HealthProbe?: HealthProbeConfig
  Reliability?: ReliabilityConfig
  // 引用的渠道协议 ID（数字或 null）；选中后以协议配置为准，内联 protocol/base_url 被忽略
  ChannelProtocolID?: number | string | null
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
// 探测历史（token 为三段口径，与账单一致：InputTokens=纯输入不含缓存读；TotalTokens=三段合计）
export interface ProbeLog {
  ID: string
  ChannelKeyID: string
  ModelID: string
  Level: string // key=密钥级探测；model=模型级探测（探针标识的探测模型）
  Target: string
  OK: boolean
  Error: string
  InputTokens: number // 纯输入（不含缓存读）
  OutputTokens: number // 生成 token（含推理）
  CachedTokens: number // 缓存读命中
  TotalTokens: number // 三段合计 = 输入 + 缓存读 + 输出
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
// 内部模型手动状态流转：action = normal | drain | disable
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

// ===== 渠道协议（channel_protocols·可复用接入模板；字段名 = Go 字段名）=====
// 单协议可选配置：缺省回退到渠道协议级 BaseURL / 基础协议内置默认路径。
export interface ProtocolOption {
  baseUrl?: string
  path?: string
  default?: boolean // 标记首选协议；同一作用域内至多一个
}
// 模型级 gateway 覆盖：baseUrl / 可达协议子集（键存在 = 可达）/ 绑定对外模型 / 成本定价。
// 与渠道内部模型（channel_models）一致地承载外部模型绑定与成本侧计费（camelCase，向后兼容）。
export interface ChannelProtocolModelOverride {
  baseUrl?: string
  protocols?: Record<string, ProtocolOption>
  // 绑定的对外模型 ID（雪花字符串）；空 = 未绑定
  externalModelID?: string | null
  // 成本三段费率（键 = billing.Rates：Input/Output/CacheRead）
  costRates?: Record<string, number>
  // 模型级时段/分档（空 = 回落全局默认）
  timeConfig?: TimeCoeffConfig | null
  contextTiers?: ContextTier[] | null
}
// 请求头策略：passthroughMode 决定下游头透传方式，whitelist/blacklist 为名单，set 为覆盖键值（支持变量）。
export interface ChannelProtocolRequestHeaders {
  passthroughMode: string // none | whitelist | blacklist | all
  whitelist: string[]
  blacklist: string[]
  set: Record<string, string>
}
export interface ChannelProtocolView {
  ID: string
  Name: string
  BaseURL: string
  Protocols: Record<string, ProtocolOption>
  RequestHeaders: ChannelProtocolRequestHeaders
  Models: Record<string, ChannelProtocolModelOverride>
  PresetSourceID: string
  Edited: boolean
  SourceStatus: string // ACTIVE | STALE（来源已失效）
}
// 创建/更新入参（提交即视为「已编辑」）
export interface ChannelProtocolInput {
  Name: string
  BaseURL: string
  Protocols: Record<string, ProtocolOption>
  RequestHeaders: ChannelProtocolRequestHeaders
  Models: Record<string, ChannelProtocolModelOverride>
}
// 同步预览：Pending 为需二次确认才覆盖的条目（含差异文本），Applied 为本次直接覆盖数
export interface ChannelProtocolSyncPending {
  Id: string
  Name: string
  Diff: string
  Edited: boolean
  Stale: boolean
}
export interface ChannelProtocolSyncResult {
  Pending: ChannelProtocolSyncPending[]
  Applied: number
}

// 列表返回裸数组（非 {list}）
export function listChannelProtocols() {
  return http.get<ChannelProtocolView[], ApiRes<ChannelProtocolView[]>>('/admin/channel-protocols')
}
export function createChannelProtocol(payload: ChannelProtocolInput) {
  return http.post<ChannelProtocolView, ApiRes<ChannelProtocolView>>('/admin/channel-protocols', payload)
}
export function getChannelProtocol(id: string) {
  return http.get<ChannelProtocolView, ApiRes<ChannelProtocolView>>(`/admin/channel-protocols/${id}`)
}
export function updateChannelProtocol(id: string, payload: ChannelProtocolInput) {
  return http.put<ChannelProtocolView, ApiRes<ChannelProtocolView>>(`/admin/channel-protocols/${id}`, payload)
}
export function deleteChannelProtocol(id: string) {
  return http.delete<{ Affected: number }, ApiRes<{ Affected: number }>>(`/admin/channel-protocols/${id}`)
}
// 同步预设：confirm=false 返回待确认清单（已编辑条目），confirm=true 执行覆盖
export function syncChannelProtocols(confirm: boolean) {
  return http.post<ChannelProtocolSyncResult, ApiRes<ChannelProtocolSyncResult>>(
    '/admin/channel-protocols/sync',
    { confirm },
  )
}
// 用协议 baseUrl(openai-chat) + 临时 ApiKey 调上游 /models，返回可供选择填入协议模型清单的列表（不落库）
export function pullChannelProtocolModels(id: string, apiKey: string) {
  return http.post<{ List: PullModel[] }, ApiRes<{ List: PullModel[] }>>(
    `/admin/channel-protocols/${id}/models/pull`,
    { ApiKey: apiKey },
  )
}
// 渠道协议预设包：{ "<id>": Provider }（含 gateway 块），导出/导入共用
export interface ChannelProtocolPack {
  [key: string]: unknown
}
// 导入结果统计
export interface ChannelProtocolBackupResult {
  Created: number
  Skipped: number
  Warnings: string[]
}
// 导出全部渠道协议为预设包 JSON
export function exportChannelProtocols() {
  return http.get<ChannelProtocolPack, ApiRes<ChannelProtocolPack>>('/admin/channel-protocols/export')
}
// 从预设包 JSON 导入渠道协议（按名称/来源去重，仅新增不覆盖）
export function importChannelProtocols(pack: ChannelProtocolPack) {
  return http.post<ChannelProtocolBackupResult, ApiRes<ChannelProtocolBackupResult>>(
    '/admin/channel-protocols/import',
    pack,
  )
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
// 密钥状态流转事件（后端 channel.ChannelKeyEvent 无 json tag，故键为 PascalCase、ID 为数字）
export interface ChannelKeyEvent {
  ID: number
  ChannelKeyID: number
  FromState: string
  ToState: string
  Reason: string
  CreatedAt: string
}
// 密钥状态流转事件（只读）
export function listChannelKeyEvents(
  channelId: string,
  keyId: string,
): Promise<ApiRes<ChannelKeyEvent[]>> {
  return http.get<ChannelKeyEvent[], ApiRes<ChannelKeyEvent[]>>(
    `/admin/channels/${channelId}/keys/${keyId}/events`,
  )
}
// 手动探测单个渠道内部模型（经该渠道可用密钥发起，仅回喂模型状态机）
export function probeChannelModel(channelId: string, mid: string): Promise<ApiRes<ProbeOutcome>> {
  return http.post<ProbeOutcome, ApiRes<ProbeOutcome>>(
    `/admin/channels/${channelId}/models/${mid}/probe`,
  )
}

// ===== 服务状态（只读聚合视图；字段名 = 后端 Go JSON 结构逐字对齐）=====
// 状态机窗口指标（后端 channel.MachineMetrics）。窗口指标为最近一次 Feed 时快照，仅供最终一致展示。
export interface MachineMetrics {
  WindowSeconds: number
  Total: number
  Failures: number
  Timeouts: number
  Rate429: number
  SuccessSamples: number
  MinSamples: number
  SampleOK: boolean
  ErrorRatePct: number
  Rate429Pct: number
  P99LatencyMS: number
  ProbeFails: number
  ProbeOKs: number
  AuthFails: number
  DrainingReasons?: string[]
}
// 渠道运行时视图（后端 channel.ChannelView；渠道=配置模板，无自身状态字段）
export interface ChannelRuntimeView {
  ID: string
  Name: string
  Protocol: string
  BaseURL: string
  Tags: Record<string, string>
  BoundTags: { ID: string; Name: string; KV?: Record<string, string> }[]
  Priority: number
  LastErr?: string
  LastProbe?: string | null
  RateLimit: RateLimitConfig
  HealthProbe: HealthProbeConfig
  Reliability: ReliabilityConfig
  MaxSessions: number
  CreatedAt: string
  UpdatedAt: string
}
// 内部模型运行时视图（后端 channel.ModelView）
export interface ModelRuntimeView {
  ID: string
  InternalModelID: string
  ExternalModelID: string
  State: string // NORMAL | DRAIN | DISABLED
  LastErr?: string
  LastProbe?: string | null
  RateLimit: RateLimitConfig
  HealthProbe: HealthProbeConfig
  Reliability: ReliabilityConfig
  MaxSessions: number
}
// 密钥运行时视图（后端 channel.KeyRuntimeView；凭据仅尾号）
export interface KeyRuntimeView {
  KeyID: string
  Name: string
  State: string // NORMAL | DRAIN | DISABLED
  LastErr?: string
  CredentialTail?: string
  LastProbe?: string | null
  DrainReasons?: string[]
}
// 内部模型层明细（后端 channel.ModelDetail）
export interface ModelDetail {
  View: ModelRuntimeView
  Metrics: MachineMetrics
  RPM: number
  TPM: number
  RPMLimit: number
  TPMLimit: number
}
// 密钥层明细（后端 channel.KeyDetail）
export interface KeyDetail {
  View: KeyRuntimeView
  Metrics: MachineMetrics
  RPM: number
  TPM: number
  RPMLimit: number
  TPMLimit: number
  MaxSessions: number // 并发会话上限
  ActiveSessions: number // 存活会话数（实时会话数）
}
// 渠道层明细（后端 channel.ChannelDetail；渠道无独立状态机，State 为内部模型/密钥派生最差值）
export interface ChannelDetail {
  View: ChannelRuntimeView
  State: string // NORMAL | DRAIN | DISABLED（派生：全部内部模型与密钥的最差值）
  RPM: number
  TPM: number
  RPMLimit: number
  TPMLimit: number
  Models: ModelDetail[]
  Keys: KeyDetail[]
}
// 对外模型清单项（供前端展示名称/启用态）
export interface ExternalModelItem {
  ID: string
  ExternalName: string
  Enabled: boolean
}
// 路由候选（顺序即 router.RouteAll 选区顺序，Rank 从 1 递增；不前端重算）
export interface CandidateItem {
  ChannelID: string
  ChannelKeyID: string
  InternalModelID: string
  ModelRowID: string
  ExternalModelID: string
  Rank: number
  Cost: number
  CostKnown: boolean
  TimeCoeff: number
}
// 成本比较样值：与后端 billing.Usage 同源（snake 键，逐字镜像）
export interface SampleUsage {
  input: number
  output: number
  cache_read: number
}
// 聚合视图（后端 console.ServiceStatusView）
export interface ServiceStatusView {
  DataTime: string
  Channels: ChannelDetail[]
  ExternalModels: ExternalModelItem[]
  Candidates: Record<string, CandidateItem[]>
  CandidatesDataTime?: string | null
  SampleUsage: SampleUsage
}
// 服务状态聚合查询：候选（成本/序位）由后端对全部启用对外模型计算，无需查询参数
export function getServiceStatus() {
  return http.get<ServiceStatusView, ApiRes<ServiceStatusView>>('/admin/service-status')
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

// ===== Auto 模型（对外模型之上的有序别名组；字段名 = Go 字段名）=====
// 成员：请求 Auto 名时按 position 升序选择第一个可用的成员对外模型。
export interface AutoModelMember {
  ID: string // auto_model_members 行 ID；新建时后端生成，前端提交传 0/空即可
  ExternalModelID: string // 对外模型 ID（字符串）
  ExternalName: string // 后端 JOIN 填充，提交时不传
  Enabled: boolean // 后端 JOIN 填充（成员对外模型是否启用）
  Position: number // 升序即优先级
}
export interface AutoModel {
  ID: string
  Name: string
  Scope: 'global' | 'private'
  OwnerUserID?: string // private 时非空；global 为空
  Enabled: boolean
  Description: string
  Members: AutoModelMember[]
  CreatedAt: string
  UpdatedAt: string
}
export interface AutoModelInput {
  Name: string
  Scope: string // 'global' | 'private'（dev 端由服务端强制覆盖为 private）
  OwnerUserID?: string // 管理端建 private 时传；global 不传
  Enabled?: boolean
  Description: string
  Members: { ExternalModelID: string; Position: number }[]
}
// 列表返回裸数组（非 {list}）；base 可传 '/dev' 复用开发者端
export function listAutoModels(params: Record<string, unknown> = {}, base = '/admin') {
  return http.get<AutoModel[], ApiRes<AutoModel[]>>(`${base}/auto-models`, { params })
}
export function getAutoModel(id: string, base = '/admin') {
  return http.get<AutoModel, ApiRes<AutoModel>>(`${base}/auto-models/${id}`)
}
export function createAutoModel(payload: AutoModelInput, base = '/admin') {
  return http.post<AutoModel, ApiRes<AutoModel>>(`${base}/auto-models`, payload)
}
export function updateAutoModel(id: string, payload: AutoModelInput, base = '/admin') {
  return http.put<AutoModel, ApiRes<AutoModel>>(`${base}/auto-models/${id}`, payload)
}
export function deleteAutoModel(id: string, base = '/admin') {
  return http.delete<{ Deleted: boolean }, ApiRes<{ Deleted: boolean }>>(`${base}/auto-models/${id}`)
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
export function listBillings(params: Record<string, unknown> = {}, base = '/admin') {
  return http.get<PageResult<AdminBillingItem>, ApiRes<PageResult<AdminBillingItem>>>(
    `${base}/billings`,
    { params },
  )
}
// 账单筛选聚合统计（与列表同 filter）：请求数/总积分/总 token/总耗时（统计栏用）。
// InputTokens=纯输入（不含缓存读）总量，CacheReadTokens=缓存读命中量（缓存命中率 = 缓存读 ÷ (输入+缓存读)）。
export interface BillingStats {
  Requests: number
  CreditsTotal: number
  TokensTotal: number
  DurationTotalMS: number
  InputTokens: number
  CacheReadTokens: number
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
  RPM: number // 近 5 分钟每分钟请求数
  TPM: number // 近 5 分钟每分钟 Token 数
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

// ===== 系统配置（计费 + 会话过期 + 网关白名单，统一入口）=====
export interface SystemConfigData {
  Billing: BillingConfigData
  SessionExpiration: SessionExpirationData
  UpstreamResponseHeaderWhitelist: string[]
}
export function getSystemConfig() {
  return http.get<SystemConfigData, ApiRes<SystemConfigData>>('/admin/configs/system')
}
export function putSystemConfig(payload: {
  Billing: {
    R: number | string
    CnyRate?: number | string
    ContextTiers: ContextTier[]
    TimeConfig: TimeCoeffConfig
    CostSample?: CostSample
  }
  SessionExpiration: SessionExpirationData
  UpstreamResponseHeaderWhitelist: string[]
}) {
  return http.put<{ Affected: number }, ApiRes<{ Affected: number }>>('/admin/configs/system', payload)
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
