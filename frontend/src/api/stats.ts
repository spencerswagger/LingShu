import { http, type ApiRes } from './http'

// ===== 统计页（统一仪表盘）聚合接口：admin 全局 / dev 本人，同一响应结构 =====

// 五段 Token 用量（后端 UsageSplit）
export interface StatsUsageSplit {
  Input: number
  Output: number
  CacheRead: number
  CacheWrite: number
  Reasoning: number
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

// 用量块（后端 DashboardStats）
export interface StatsUsage {
  From: string
  To: string
  Days: number
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
  RPM: number
  TPM: number
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

// 运维块（仅管理端返回）
export interface StatsOps {
  Channels: StatsCounts
  Keys: StatsCounts
  Models: StatsModelCounts
}

export interface StatsDashboard {
  Scope: string // global | self
  From: string
  To: string
  KPI: StatsKPI
  Usage: StatsUsage
  Ops?: StatsOps
}

// 按角色选择端点：ADMIN → 全局；其余（DEVELOPER）→ 本人
export function getStatsDashboard(
  role: string,
  params: { From?: string; To?: string } = {},
): Promise<ApiRes<StatsDashboard>> {
  const path = role === 'ADMIN' ? '/admin/stats/dashboard' : '/dev/stats/dashboard'
  return http.get<StatsDashboard, ApiRes<StatsDashboard>>(path, { params })
}
