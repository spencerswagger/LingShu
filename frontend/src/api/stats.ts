import { http, type ApiRes } from './http'

// ===== 统计页（统一仪表盘）聚合接口：admin 全局 / dev 本人，同一响应结构 =====

// 三段 Token 用量（后端 UsageSplit）
export interface StatsUsageSplit {
  Input: number
  Output: number
  CacheRead: number
}

// 按天聚合行（后端 DashDay）
export interface StatsDaily {
  Date: string // 2006-01-02
  Calls: number
  Failed: number
  Credits: number
  Tokens: number
  DurationMS: number
  SuccessRate: number // 0~1
}

// 多维度 Top 项（后端 DashTopItem：模型 / 密钥 / 用户 / 模式 / 错误共用）
export interface StatsTopItem {
  Key: string
  Label: string
  SubLabel: string
  Calls: number
  Failed: number
  Credits: number
  Tokens: number
}

// 耗时分桶（后端 DashLatencyBucket）
export interface StatsLatencyBucket {
  Bucket: string
  Count: number
}

// 小时桶（后端 DashHour）：只含**有数据**的小时，缺桶不出行 → 前端断线、不要补 0。
export interface StatsHour {
  Hour: string // 桶起点 2006-01-02T15:04（北京时间墙钟）
  Day: string // 所属自然日 2006-01-02
  HourOfDay: number // 0~23，跨日对齐用
  Calls: number
  Failed: number
  Credits: number
  Tokens: number
  DurationMS: number
  SuccessRate: number // 0~1
}

// 同期对比序列（后端 DashSeries）
export interface StatsSeries {
  Day: string
  Label: string // 今天 / 昨天 / 前天 / 大前天；更早为 MM-DD
  Offset: number // 0=今天，1=昨天…
  Points: StatsHour[] | null // 缺桶不出行
}

// 模型广场行（后端 DashModelStat）
export interface StatsModelStat {
  Model: string
  Label: string
  Enabled: boolean
  Calls: number
  Failed: number
  FailRate: number // 0~1
  Tokens: number
  Credits: number
  TPM: number // 按模型忙时口径
  BusyCalls: number
  BusyMS: number
  AvgMS: number
  AvgFirstMS: number
  Active: boolean // Calls>0；false = 空闲模型 → 折叠
  // 后端兜底行：无法归属到任何在册对外模型的用量（改名/别名/无渠道）。
  // Model 固定为 "__unmatched__"，Frontend 应单独渲染，不计入目录数、不受 Limit 截断。
  Unmatched: boolean
}

// 模型广场（后端 ModelPlaza）
export interface StatsModelPlaza {
  Sort: string
  Total: number
  Active: number
  Limit: number
  Truncated: boolean
  List: StatsModelStat[] | null
}

// 用量块（后端 DashboardStats）
export interface StatsUsage {
  From: string
  To: string
  Days: number
  Granularity: string // hour | day（实际生效粒度）
  Hourly: StatsHour[] | null // day 粒度下为 []
  Compare: StatsSeries[] | null // CompareDays=0 时为 []
  Total: StatsUsageSplit
  TotalCred: number
  TotalCall: number
  Failed: number
  Success: number // 0~1
  AvgMS: number
  P50MS: number
  P90MS: number
  P95MS: number
  AvgFirst: number
  AvgRPM: number
  AvgTPM: number
  Daily: StatsDaily[]
  ByModel: StatsTopItem[]
  ByKey: StatsTopItem[]
  ByUser: StatsTopItem[]
  ByMode: StatsTopItem[]
  Errors: StatsTopItem[]
  Latency: StatsLatencyBucket[]
}

// 顶部关键指标（后端 statsKPI）
export interface StatsKPI {
  TotalTokens: number
  TotalCredits: number
  TotalRequests: number
  FailedRequests: number
  SuccessRate: number
  AvgDurationMS: number
  AvgFirstTokenMS: number
  // 首屏 KPI 的实际窗口（与 Usage.Hourly / ModelPlaza 同源）：
  // WindowMode=exact 为请求区间的严格滚动窗口，calendar 为北京时间自然日归一窗口。
  WindowFrom: string // RFC3339
  WindowTo: string // RFC3339
  WindowMode: string // exact | calendar
  // RPM/TPM 为忙时口径（按有耗时请求的处理时长折算，文案仍叫 RPM/TPM）
  RPM: number
  TPM: number
  // SumDurationMS 忙时分母：这批请求的耗时之和（毫秒），供前端做样本量标注
  SumDurationMS: number
  // BusyRequests 忙时分子：有耗时的请求数（与 RPM/TPM 分母同源）
  BusyRequests: number
  DeltaTokens: number
  DeltaCredits: number
  DeltaRequests: number
}

export interface StatsCounts {
  Total: number
  Normal: number
  Drain: number
  Disabled: number
}

export interface StatsModelCounts {
  ExternalTotal: number
  ExternalEnabled: number
  InternalTotal: number
  InternalNormal: number
}

// 渠道无独立状态，仅总数（可用性见 Keys 三态）
export interface StatsChannelCounts {
  Total: number
}

// 运维块（仅管理端返回）
export interface StatsOps {
  Channels: StatsChannelCounts
  Keys: StatsCounts
  Models: StatsModelCounts
}

export interface StatsDashboard {
  Scope: string // global | self
  From: string
  To: string
  KPI: StatsKPI
  Usage: StatsUsage
  ModelPlaza: StatsModelPlaza
  Ops?: StatsOps
}

// 按角色选择端点：ADMIN → 全局；其余（DEVELOPER）→ 本人
export function getStatsDashboard(
  role: string,
  params: StatsDashboardParams = {},
): Promise<ApiRes<StatsDashboard>> {
  const path = role === 'ADMIN' ? '/admin/stats/dashboard' : '/dev/stats/dashboard'
  return http.get<StatsDashboard, ApiRes<StatsDashboard>>(path, { params })
}

// ===== 增量可选参数（不传 = 老行为，后端按 auto 判定粒度）=====
export interface StatsDashboardParams {
  From?: string
  To?: string
  Granularity?: 'auto' | 'hour' | 'day'
  CompareDays?: number // 0~7，四线图传 4
  ModelSort?: 'tokens' | 'tpm' | 'failed' | 'latency'
  ModelLimit?: number // 1~200
  // exact（默认）= 严格按请求区间的滚动窗口；calendar = 北京时间自然日归一。
  // KPI / Hourly / ModelPlaza 跟这个窗口走，Usage.Daily 与其它维度聚合仍是日界口径。
  Window?: 'exact' | 'calendar'
}

// ===== 实时区 /stats/live =====
export interface StatsLiveSession {
  ID: string // 会话指纹（不是客户端原始 session-id）
  Name: string // 可能为空 → 前端用 ID 前缀兜底
  Model: string
  Inflight: number
  ContextUsed: number
  ContextLimit: number // 平台未配置模型上限时恒为 0（=未知）
  RoundTokens: number
  Calls: number
  WindowTokens: number
  TPM: number // 墙钟窗口平均（Token ÷ Window 分钟）
  SpentCredits: number
  LastActive: string
  Spark: number[] // 时间正序，可能不足 SparkPoints 个
}

export interface StatsLiveUser {
  ID: string
  Name: string
  Sessions: number
  Tokens: number
  TPM: number // 墙钟窗口平均
  SpentCredits: number
  Spark: number[] // 时间正序，无请求分钟补 0
}

export interface StatsLive {
  Scope: string // global | self
  SampledAt: string
  WindowMinutes: number
  SparkPoints: number
  SessionsTotal: number
  UsersTotal: number
  Truncated: boolean
  UsersTruncated: boolean
  Sessions: StatsLiveSession[] | null
  Users: StatsLiveUser[] | null
}

export interface StatsLiveParams {
  Window?: number // 1~1440 分钟，默认 60
  SparkPoints?: number // 1~60，默认 12
  Limit?: number // 1~100，默认 20
}

// 实时区与 dashboard 分开取：30s 刷新只重取 live，不触发整页历史聚合。
export function getStatsLive(role: string, params: StatsLiveParams = {}): Promise<ApiRes<StatsLive>> {
  const path = role === 'ADMIN' ? '/admin/stats/live' : '/dev/stats/live'
  return http.get<StatsLive, ApiRes<StatsLive>>(path, { params })
}
