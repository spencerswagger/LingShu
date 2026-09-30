import { http, type ApiRes } from './http'

// ===== 统计页（统一仪表盘）聚合接口：admin 全局 / dev 本人，同一响应结构 =====

// 五段 Token 用量
export interface StatsUsageSplit {
  input: number
  output: number
  cache_read: number
  cache_write: number
  reasoning: number
}

// 按天聚合行
export interface StatsDaily {
  date: string
  calls: number
  failed: number
  credits: number
  tokens: number
  duration_ms: number
  success_rate: number // 0~1
}

// 多维度 Top 项
export interface StatsTopItem {
  key: string
  label: string
  sub_label: string
  calls: number
  failed: number
  credits: number
  tokens: number
}

export interface StatsLatencyBucket {
  bucket: string
  count: number
}

// 用量块
export interface StatsUsage {
  from: string
  to: string
  days: number
  total_tokens: StatsUsageSplit
  total_credits: number
  total_requests: number
  failed_requests: number
  success_rate: number
  avg_duration_ms: number
  p50_duration_ms: number
  p90_duration_ms: number
  p95_duration_ms: number
  avg_first_token_ms: number
  avg_rpm: number
  avg_tpm: number
  daily: StatsDaily[]
  by_model: StatsTopItem[]
  by_key: StatsTopItem[]
  by_user: StatsTopItem[]
  by_mode: StatsTopItem[]
  errors: StatsTopItem[]
  latency: StatsLatencyBucket[]
}

// 顶部关键指标
export interface StatsKPI {
  total_tokens: number
  total_credits: number
  total_requests: number
  failed_requests: number
  success_rate: number
  avg_duration_ms: number
  avg_first_token_ms: number
  rpm: number
  tpm: number
  delta_tokens: number
  delta_credits: number
  delta_requests: number
}

export interface StatsCounts {
  total: number
  normal: number
  drain: number
  disabled: number
}

export interface StatsModelCounts {
  external_total: number
  external_enabled: number
  internal_total: number
  internal_normal: number
}

// 运维块（仅管理端返回）
export interface StatsOps {
  channels: StatsCounts
  keys: StatsCounts
  models: StatsModelCounts
}

export interface StatsDashboard {
  scope: string // global | self
  from: string
  to: string
  kpi: StatsKPI
  usage: StatsUsage
  ops?: StatsOps
}

// 按角色选择端点：ADMIN → 全局；其余（DEVELOPER）→ 本人
export function getStatsDashboard(
  role: string,
  params: { from?: string; to?: string } = {},
): Promise<ApiRes<StatsDashboard>> {
  const path = role === 'ADMIN' ? '/admin/stats/dashboard' : '/dev/stats/dashboard'
  return http.get<StatsDashboard, ApiRes<StatsDashboard>>(path, { params })
}