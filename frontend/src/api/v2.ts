// V2 运营台接口层（阶段三前端）。
//
// 口径来源（不在前端另立一套字段）：
//   - 表结构与枚举：`docs/designs/2026-11-15-v2-p1-data-contract.md`（0019–0024）
//   - 盈亏 / 对账两个已定型接口：`backend/internal/domain/console/ops.go`（字段名与语义逐条照抄）
//   - 仓库既有 JSON 约定：Go 结构体无 tag 时按字段名首字母大写序列化（`Key`/`Requests`/…）
//
// 接口就绪度分三档，页面据此决定渲染「真数据 / 空态 + 演示数据开关 / 错误」：
//   ready   —— 后端已有 handler（盈亏、对账），分支合入即可用；
//   pending —— 后端未上线（内部模型、拓扑、账本、订阅、预扣），返回 404/405/501 时按「未就绪」处理。
//
// 未就绪端点一律不参与告警：`v2Get` 把 404/405/501 归一成 state='pending'，
// 不上抛全局红条（走 http 的 silentStatuses），页面自己出空态。
import { http, type ApiError } from './http'

// ===== 通用载入结果 =====
export type V2State = 'live' | 'pending' | 'error'

export interface V2Load<T> {
  state: V2State
  data: T | null
  error?: ApiError
}

// 视为「接口未上线」的状态码：404（未注册）/405（方法未注册）/501（未实现）。
const PENDING_STATUSES = [404, 405, 501, 403]

// pick 在多写法间取值：Go 结构体无 tag 时是 PascalCase，写 tag 时可能是 snake_case。
// 同一字段两种写法都要认，避免「后端改了 tag、前端整页空白」。
function pick<T = unknown>(obj: Record<string, unknown>, ...keys: string[]): T | undefined {
  for (const k of keys) {
    if (obj && Object.prototype.hasOwnProperty.call(obj, k) && obj[k] != null) {
      return obj[k] as T
    }
  }
  return undefined
}

function num(v: unknown): number {
  const n = typeof v === 'string' ? Number(v) : (v as number)
  return typeof n === 'number' && Number.isFinite(n) ? n : 0
}

// 可空数字：null/undefined 必须保留为 null —— 对账窗口里「null = 还没算过」与「0 = 算了是 0 笔」是两种结论。
function numOrNull(v: unknown): number | null {
  if (v === null || v === undefined || v === '') return null
  const n = typeof v === 'string' ? Number(v) : (v as number)
  return typeof n === 'number' && Number.isFinite(n) ? n : null
}

function str(v: unknown): string {
  return v === null || v === undefined ? '' : String(v)
}

function obj(v: unknown): Record<string, unknown> {
  return v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : {}
}

function arr(v: unknown): Record<string, unknown>[] {
  return Array.isArray(v) ? (v as Record<string, unknown>[]) : []
}

/**
 * v2Get：读一个 V2 端点并把响应体归一化成前端模型。
 * - 成功：state='live'，data=normalize(Data)
 * - 404/405/501/403：state='pending'（接口未上线 / 功能开关未开），页面出空态，不报错
 * - 其它：state='error'，页面出 ErrorBubble（带 requestId）
 */
export async function v2Get<T>(
  path: string,
  params: Record<string, unknown> | undefined,
  normalize: (raw: Record<string, unknown>) => T,
): Promise<V2Load<T>> {
  try {
    const res = await http.get<{ Data?: unknown }, { Data?: unknown }>(path, {
      params,
      silentStatuses: PENDING_STATUSES,
    })
    return { state: 'live', data: normalize(obj(res?.Data)) }
  } catch (e) {
    const err = e as ApiError
    const status = err?.status ?? 0
    if (PENDING_STATUSES.includes(status)) {
      return { state: 'pending', data: null, error: err }
    }
    return { state: 'error', data: null, error: err }
  }
}

// ===== 盈亏（GET /admin/ops/pnl，已定型） =====
// 语义：收入 = Σ credits_consumed（售价口径）；成本 = Σ cost_credits（账本实付 + 订阅摊销）；
// 毛利 = 收入 − 成本；MarginPct 在收入 ≤ 0 时为 null（后端刻意不返回 0）。
// Uncosted = 成本还没有口径的请求数，> 0 时毛利偏高，必须显式提示，不许当真实毛利。
export type PnlGroupBy = 'provider' | 'account' | 'model'

export interface PnlRow {
  Key: string
  Requests: number
  Tokens: number
  Revenue: number
  Cost: number
  Margin: number
  MarginPct: number | null
  LedgerCost: number
  AmortizedCost: number
  Uncosted: number
}

export interface PnlView {
  From: string
  To: string
  GroupBy: PnlGroupBy
  Requests: number
  Tokens: number
  Revenue: number
  Cost: number
  Margin: number
  MarginPct: number | null
  Uncosted: number
  Groups: PnlRow[]
  Truncated: boolean
}

export function normalizePnl(raw: Record<string, unknown>): PnlView {
  return {
    From: str(pick(raw, 'From', 'from')),
    To: str(pick(raw, 'To', 'to')),
    GroupBy: (str(pick(raw, 'GroupBy', 'group_by')) || 'provider') as PnlGroupBy,
    Requests: num(pick(raw, 'Requests', 'requests')),
    Tokens: num(pick(raw, 'Tokens', 'tokens')),
    Revenue: num(pick(raw, 'Revenue', 'revenue')),
    Cost: num(pick(raw, 'Cost', 'cost')),
    Margin: num(pick(raw, 'Margin', 'margin')),
    MarginPct: numOrNull(pick(raw, 'MarginPct', 'margin_pct')),
    Uncosted: num(pick(raw, 'Uncosted', 'uncosted')),
    Truncated: Boolean(pick(raw, 'Truncated', 'truncated')),
    Groups: arr(pick(raw, 'Groups', 'groups')).map((g) => ({
      Key: str(pick(g, 'Key', 'key')),
      Requests: num(pick(g, 'Requests', 'requests')),
      Tokens: num(pick(g, 'Tokens', 'tokens')),
      Revenue: num(pick(g, 'Revenue', 'revenue')),
      Cost: num(pick(g, 'Cost', 'cost')),
      Margin: num(pick(g, 'Margin', 'margin')),
      MarginPct: numOrNull(pick(g, 'MarginPct', 'margin_pct')),
      LedgerCost: num(pick(g, 'LedgerCost', 'ledger_cost')),
      AmortizedCost: num(pick(g, 'AmortizedCost', 'amortized_cost')),
      Uncosted: num(pick(g, 'Uncosted', 'uncosted')),
    })),
  }
}

export function getPnl(params: { From?: string; To?: string; GroupBy?: PnlGroupBy }) {
  return v2Get('/admin/ops/pnl', params, normalizePnl)
}

// ===== 对账窗口（GET /admin/ops/reconcile-windows，已定型） =====
// 状态取值域唯一含义：ok=已对账且在阈值内；alarm=偏差超阈；error=没拉完；blocked=预算用尽待补拉；
// skipped=没有对账结论（带 detail.skip_reason）。计数器 null = 还没算过，不许显示成 0。
export type ReconcileStatus = 'ok' | 'alarm' | 'error' | 'blocked' | 'skipped'

export interface ReconcileWindow {
  WindowStart: string
  WindowEnd: string
  Status: ReconcileStatus
  AlarmStreak: number
  LocalCount: number | null
  UpstreamCount: number | null
  LocalTokens: number | null
  UpstreamTokens: number | null
  DeviationRate: number | null
  ScrollOffset: number
  CheckedAt: string
}

export interface ReconcileView {
  From: string
  To: string
  Items: ReconcileWindow[]
}

const RECONCILE_STATUSES: ReconcileStatus[] = ['ok', 'alarm', 'error', 'blocked', 'skipped']

export function normalizeReconcile(raw: Record<string, unknown>): ReconcileView {
  const items = arr(pick(raw, 'Items', 'items')).map((w): ReconcileWindow => {
    const s = str(pick(w, 'Status', 'status')) as ReconcileStatus
    return {
      WindowStart: str(pick(w, 'WindowStart', 'window_start')),
      WindowEnd: str(pick(w, 'WindowEnd', 'window_end')),
      Status: RECONCILE_STATUSES.includes(s) ? s : 'skipped',
      AlarmStreak: num(pick(w, 'AlarmStreak', 'alarm_streak')),
      LocalCount: numOrNull(pick(w, 'LocalCount', 'local_count')),
      UpstreamCount: numOrNull(pick(w, 'UpstreamCount', 'upstream_count')),
      LocalTokens: numOrNull(pick(w, 'LocalTokens', 'local_tokens')),
      UpstreamTokens: numOrNull(pick(w, 'UpstreamTokens', 'upstream_tokens')),
      DeviationRate: numOrNull(pick(w, 'DeviationRate', 'deviation_rate')),
      ScrollOffset: num(pick(w, 'ScrollOffset', 'scroll_offset')),
      CheckedAt: str(pick(w, 'CheckedAt', 'checked_at')),
    }
  })
  return { From: str(pick(raw, 'From', 'from')), To: str(pick(raw, 'To', 'to')), Items: items }
}

export function getReconcileWindows(params: { From?: string; To?: string; Limit?: number }) {
  return v2Get('/admin/ops/reconcile-windows', params, normalizeReconcile)
}

// ===== 账本流水（GET /admin/ledger，待 Core 上线；九字段口径来自数据层契约 §6.1） =====
export interface LedgerEntry {
  ID: string
  LedgerID: string
  Provider: string
  ProviderAccount: string
  Model: string
  TokensIn: number
  TokensOut: number
  CostCredits: number
  OccurredAt: string
}

export interface LedgerView {
  Items: LedgerEntry[]
  Total: number
  CostTotal: number
  TokensIn: number
  TokensOut: number
}

export function normalizeLedger(raw: Record<string, unknown>): LedgerView {
  const items = arr(pick(raw, 'Items', 'items')).map((e): LedgerEntry => ({
    ID: str(pick(e, 'ID', 'id')),
    LedgerID: str(pick(e, 'LedgerID', 'ledger_id')),
    Provider: str(pick(e, 'Provider', 'provider')),
    ProviderAccount: str(pick(e, 'ProviderAccount', 'provider_account')),
    Model: str(pick(e, 'Model', 'model')),
    TokensIn: num(pick(e, 'TokensIn', 'tokens_in')),
    TokensOut: num(pick(e, 'TokensOut', 'tokens_out')),
    CostCredits: num(pick(e, 'CostCredits', 'cost_credits')),
    OccurredAt: str(pick(e, 'OccurredAt', 'occurred_at')),
  }))
  return {
    Items: items,
    Total: num(pick(raw, 'Total', 'total')) || items.length,
    CostTotal: num(pick(raw, 'CostTotal', 'cost_total')),
    TokensIn: num(pick(raw, 'TokensIn', 'tokens_in')),
    TokensOut: num(pick(raw, 'TokensOut', 'tokens_out')),
  }
}

export function getLedger(params: {
  From?: string
  To?: string
  Provider?: string
  Account?: string
  Model?: string
  Limit?: number
  Offset?: number
}) {
  return v2Get('/admin/ledger', params, normalizeLedger)
}

// ===== 预扣登记（GET /admin/credit-holds，待 Core；阶段二启用） =====
export type HoldStatus = 'held' | 'settled' | 'refunded' | 'expired'

export interface CreditHold {
  ID: string
  BillingID: string
  UserID: string
  TokenID: string
  Amount: number
  Status: HoldStatus
  LedgerEntryID: string
  ExpiresAt: string
  ClosedAt: string
  CreatedAt: string
}

const HOLD_STATUSES: HoldStatus[] = ['held', 'settled', 'refunded', 'expired']

export interface HoldView {
  Items: CreditHold[]
  HeldAmount: number
  HeldCount: number
}

export function normalizeHolds(raw: Record<string, unknown>): HoldView {
  const items = arr(pick(raw, 'Items', 'items')).map((h): CreditHold => {
    const s = str(pick(h, 'Status', 'status')) as HoldStatus
    return {
      ID: str(pick(h, 'ID', 'id')),
      BillingID: str(pick(h, 'BillingID', 'billing_id')),
      UserID: str(pick(h, 'UserID', 'user_id')),
      TokenID: str(pick(h, 'TokenID', 'token_id')),
      Amount: num(pick(h, 'Amount', 'amount')),
      Status: HOLD_STATUSES.includes(s) ? s : 'held',
      LedgerEntryID: str(pick(h, 'LedgerEntryID', 'ledger_entry_id')),
      ExpiresAt: str(pick(h, 'ExpiresAt', 'expires_at')),
      ClosedAt: str(pick(h, 'ClosedAt', 'closed_at')),
      CreatedAt: str(pick(h, 'CreatedAt', 'created_at')),
    }
  })
  const held = items.filter((i) => i.Status === 'held')
  return {
    Items: items,
    HeldCount: held.length,
    HeldAmount: Math.round(held.reduce((s, i) => s + i.Amount, 0) * 1e5) / 1e5,
  }
}

export function getCreditHolds(params: { Status?: HoldStatus; Limit?: number }) {
  return v2Get('/admin/credit-holds', params, normalizeHolds)
}

// ===== 内部模型 / 拓扑（GET /admin/internal-models、GET /admin/model-links，待 Core） =====
// 字段与 0019_v2_topology.sql 一一对应；绑定真源是 external_models.bound_internal_ref，
// model_links 只存画布坐标（不是关系真源），所以 Links 缺失不影响售卖与计费。
export interface InternalModel {
  Ref: string
  ProviderRef: string
  Kind: 'group' | 'direct'
  DisplayName: string
  ContextLen: number
  PosX: number
  PosY: number
  LastSeenAt: string
  Stale: boolean
}

export interface ModelLink {
  InternalRef: string
  ExternalModelID: string
  PosX: number
  PosY: number
}

export interface InternalModelView {
  Items: InternalModel[]
  Links: ModelLink[]
  StaleCount: number
  UnboundExternal: number
  SyncedAt: string
}

export function normalizeInternalModels(raw: Record<string, unknown>): InternalModelView {
  const items = arr(pick(raw, 'Items', 'items')).map((m): InternalModel => ({
    Ref: str(pick(m, 'Ref', 'ref')),
    ProviderRef: str(pick(m, 'ProviderRef', 'provider_ref')),
    Kind: str(pick(m, 'Kind', 'kind')) === 'group' ? 'group' : 'direct',
    DisplayName: str(pick(m, 'DisplayName', 'display_name')),
    ContextLen: num(pick(m, 'ContextLen', 'context_len')),
    PosX: num(pick(m, 'PosX', 'pos_x')),
    PosY: num(pick(m, 'PosY', 'pos_y')),
    LastSeenAt: str(pick(m, 'LastSeenAt', 'last_seen_at')),
    Stale: Boolean(pick(m, 'Stale', 'stale')),
  }))
  const links = arr(pick(raw, 'Links', 'links')).map((l): ModelLink => ({
    InternalRef: str(pick(l, 'InternalRef', 'internal_ref')),
    ExternalModelID: str(pick(l, 'ExternalModelID', 'external_model_id')),
    PosX: num(pick(l, 'PosX', 'pos_x')),
    PosY: num(pick(l, 'PosY', 'pos_y')),
  }))
  return {
    Items: items,
    Links: links,
    StaleCount: num(pick(raw, 'StaleCount', 'stale_count')) || items.filter((i) => i.Stale).length,
    UnboundExternal: num(pick(raw, 'UnboundExternal', 'unbound_external')),
    SyncedAt: str(pick(raw, 'SyncedAt', 'synced_at')),
  }
}

export function getInternalModels() {
  return v2Get('/admin/internal-models', undefined, normalizeInternalModels)
}

// ===== 订阅（GET /admin/subscriptions，录入标准待 Spec 定稿；字段来自数据层契约 §2.4） =====
export type PriceCurrency = 'CNY' | 'USD'
export type BillingCycle = 'month' | 'year' | 'once'

export interface Subscription {
  ID: string
  ProviderRef: string
  AccountRef: string
  PriceAmount: number
  PriceCurrency: PriceCurrency
  BillingCycle: BillingCycle
  PayDay: number
  ShareCost: boolean
  CycleStart: string
  Note: string
  CreatedAt: string
  UpdatedAt: string
}

export interface SubscriptionView {
  Items: Subscription[]
  MonthlyTotal: number
  UnstartedCount: number
}

const CURRENCIES: PriceCurrency[] = ['CNY', 'USD']
const CYCLES: BillingCycle[] = ['month', 'year', 'once']

export function normalizeSubscriptions(raw: Record<string, unknown>): SubscriptionView {
  const items = arr(pick(raw, 'Items', 'items')).map((s): Subscription => {
    const cur = str(pick(s, 'PriceCurrency', 'price_currency')) as PriceCurrency
    const cyc = str(pick(s, 'BillingCycle', 'billing_cycle')) as BillingCycle
    return {
      ID: str(pick(s, 'ID', 'id')),
      ProviderRef: str(pick(s, 'ProviderRef', 'provider_ref')),
      AccountRef: str(pick(s, 'AccountRef', 'account_ref')),
      PriceAmount: num(pick(s, 'PriceAmount', 'price_amount')),
      PriceCurrency: CURRENCIES.includes(cur) ? cur : 'CNY',
      BillingCycle: CYCLES.includes(cyc) ? cyc : 'month',
      PayDay: num(pick(s, 'PayDay', 'pay_day')) || 1,
      ShareCost: pick(s, 'ShareCost', 'share_cost') !== false,
      CycleStart: str(pick(s, 'CycleStart', 'cycle_start')),
      Note: str(pick(s, 'Note', 'note')),
      CreatedAt: str(pick(s, 'CreatedAt', 'created_at')),
      UpdatedAt: str(pick(s, 'UpdatedAt', 'updated_at')),
    }
  })
  return {
    Items: items,
    // 月费合计只做「同币种可加」的粗算：CNY 直接累加，USD 不换算（换算只在成本侧一次，前端不引第二套汇率）。
    MonthlyTotal:
      Math.round(
        items.filter((i) => i.PriceCurrency === 'CNY' && i.BillingCycle === 'month').reduce((s, i) => s + i.PriceAmount, 0) * 1e5,
      ) / 1e5,
    UnstartedCount: items.filter((i) => !i.CycleStart).length,
  }
}

export function getSubscriptions() {
  return v2Get('/admin/subscriptions', undefined, normalizeSubscriptions)
}
