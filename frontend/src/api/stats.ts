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
  // Model 固定为 "__unmatched__"。v4 第 3 条已从页面移除该区块，前端不再渲染（字段保留向后兼容）。
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
  // ModelSort 已下线（v4 第 3 条）：后端固定按 Token 倒序，前端不再传排序参数。
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
  // 累计 Token（三段求和）：卡片「累计」用它，不含单位换算
  TotalTokens: number
  // 会话创建时间（RFC3339，注册表未记录时为空串）
  CreatedAt: string
  LastActive: string
  Spark: number[] // 时间正序，可能不足 SparkPoints 个
}

export interface StatsLiveUser {
  ID: string
  Name: string
  Sessions: number // 仍只数 Inflight>0 的会话
  Tokens: number // 该用户全部会话的近 Window 用量
  TPM: number // 墙钟窗口平均
  SpentCredits: number
  // 所选时间范围内的活跃会话数 / 请求数 / Token / 积分（billing_records 同源）
  ActiveSessions: number
  Calls: number
  RangeTokens: number
  RangeCredits: number
  // 逐日 Token 序列（长度 = DailyPoints，无请求日补 0，时间正序）→ 迷你曲线
  Daily: number[]
  Spark: number[] // 时间正序，无请求分钟补 0
}

// 刚结束请求、正在淡出的会话（**纯展示**，不参与会话数/用户数与任何聚合）。
// 语义：这些卡片已经从 Sessions 里出去，前端把它们标记为「离开中」播淡出动画，
// 到 `ExitedAt + FadeWindowSeconds` 就从 DOM 移除（下一轮服务端也不会再返回）。
export interface StatsLiveExited {
  ID: string // 会话指纹
  Name: string // 可能为空 → 前端用 ID 前缀兜底
  Model: string
  ExitedAt: string // 淡出起点（RFC3339）
  Calls: number // 该会话累计请求数（仅卡片收尾展示）
}

export interface StatsLive {
  Scope: string // global | self
  SampledAt: string
  WindowMinutes: number
  SparkPoints: number
  // 在线用户筛选范围（RFC3339）回显：页面时间范围标签读它，不要本地自拼
  RangeFrom: string
  RangeTo: string
  // 每用户日活跃序列覆盖的日期（YYYY-MM-DD，北京时间日界）
  DailyFrom: string
  DailyTo: string
  DailyPoints: number
  // 我的会话总数（未截断）= **仅请求中**（Inflight > 0）；刚结束的会话只在 RecentlyExited 里淡出，
  // 不会抬高这个数字，前端也不要用 LastActive 再过滤一次。
  SessionsTotal: number
  UsersTotal: number // 所选范围内有过请求的用户数（与 Users 同源未截断）
  Truncated: boolean
  UsersTruncated: boolean
  // Sessions 由服务端保证「只有请求中的会话」（不要再用 LastActive 过滤）。
  Sessions: StatsLiveSession[] | null
  Users: StatsLiveUser[] | null
  // 淡出窗口内刚结束请求的会话（上限 20，按最近活跃倒序）。**纯展示字段**：
  // 不计入 SessionsTotal / UsersTotal，不参与任何排序与聚合。
  RecentlyExited?: StatsLiveExited[] | null
  // 淡出窗口秒数（当前 300）：卡片在 ExitedAt + 该值 之后从 DOM 移除。
  FadeWindowSeconds?: number
}

export interface StatsLiveParams {
  // 在线用户筛选范围；缺省 = 后端默认近 24h，跨度上限 366 天
  From?: string // RFC3339
  To?: string // RFC3339
  Window?: number // 1~1440 分钟，默认 60
  SparkPoints?: number // 1~60，默认 12
  Limit?: number // 1~100，默认 20
}

// 实时区与 dashboard 分开取：30s 刷新只重取 live，不触发整页历史聚合。
export function getStatsLive(role: string, params: StatsLiveParams = {}): Promise<ApiRes<StatsLive>> {
  const path = role === 'ADMIN' ? '/admin/stats/live' : '/dev/stats/live'
  return http.get<StatsLive, ApiRes<StatsLive>>(path, { params })
}
