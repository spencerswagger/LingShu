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

/**
 * v2Post：写一个 V2 端点（当前只有订阅录入 S2 用到）。
 * 与 v2Get 的区别：400/409/422 是**业务结果**（字段非法 / 重复订阅 / 汇率未配），
 * 必须把 code 与 message 原样交给页面做字段级定位与分支，不能进全局红条后就丢掉。
 */
export async function v2Post<T>(
  path: string,
  body: unknown,
  normalize: (raw: Record<string, unknown>) => T,
): Promise<V2Load<T>> {
  try {
    const res = await http.post<{ Data?: unknown }, { Data?: unknown }>(path, body, {
      silentStatuses: [...PENDING_STATUSES, 400, 409, 422],
    })
    return { state: 'live', data: normalize(obj(res?.Data)) }
  } catch (e) {
    const err = e as ApiError
    const status = err?.status ?? 0
    if (PENDING_STATUSES.includes(status)) return { state: 'pending', data: null, error: err }
    return { state: 'error', data: null, error: err }
  }
}

// v2Put：PUT 语义的写接口（目前只有画布落位 T2）。与 v2Post 同口径：
// 404/405/501/403 归一成 pending，400/409/422 是业务拒绝（state=error，页面展示原因）。
export async function v2Put<T>(
  path: string,
  body: unknown,
  normalize: (raw: Record<string, unknown>) => T,
): Promise<V2Load<T>> {
  try {
    const res = await http.put<{ Data?: unknown }, { Data?: unknown }>(path, body, {
      silentStatuses: [...PENDING_STATUSES, 400, 409, 422],
    })
    return { state: 'live', data: normalize(obj(res?.Data)) }
  } catch (e) {
    const err = e as ApiError
    const status = err?.status ?? 0
    if (PENDING_STATUSES.includes(status)) return { state: 'pending', data: null, error: err }
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
  // Uncosted = 没有成本口径的请求数（成本为空），> 0 时毛利偏高，页面必须显式提示。
  // 契约事实（Core 2026-11-16 确认）：只有这一个口径，**没有**「历史未配置 / 明确无口径」两段拆分——
  // 约束 ck_billing_cost_presence 强制「cost_credits IS NULL ⇒ cost_source='none'」，
  // 「cost_source IS NULL 的行」在当前 schema 下不存在，故后端有意不返回拆分，前端也不得虚构。
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
  // skipped 必须有原因（M4）：只显示状态词等于没说，页面要翻译成中文
  SkipReason: string
  // 排障用的原始 detail（sync.* / reconcile.*），不参与判定
  Detail: Record<string, unknown>
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
      SkipReason: str(pick(w, 'SkipReason', 'skip_reason')) || str(pick(obj(pick(w, 'Detail', 'detail')), 'skip_reason')),
      Detail: obj(pick(w, 'Detail', 'detail')),
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

// 新鲜度卡（L2）：滞后>180s 橙「账本不新鲜」，>15min 红「对账不可用」。
export interface LedgerStatus {
  LastOccurredAt: string
  LastSuccessAt: string
  LagSeconds: number | null
  BlockedWindows: number
  ErrorWindows: number
  PendingOffset: number
  BudgetExhausted: boolean
  LedgerEnabled: boolean
}

export function normalizeLedgerStatus(raw: Record<string, unknown>): LedgerStatus {
  return {
    LastOccurredAt: str(pick(raw, 'LastOccurredAt', 'last_occurred_at')),
    LastSuccessAt: str(pick(raw, 'LastSuccessAt', 'last_success_at')),
    LagSeconds: numOrNull(pick(raw, 'LagSeconds', 'lag_seconds')),
    BlockedWindows: num(pick(raw, 'BlockedWindows', 'blocked_windows')),
    ErrorWindows: num(pick(raw, 'ErrorWindows', 'error_windows')),
    PendingOffset: num(pick(raw, 'PendingOffset', 'pending_offset')),
    BudgetExhausted: Boolean(pick(raw, 'BudgetExhausted', 'budget_exhausted')),
    LedgerEnabled: pick(raw, 'LedgerEnabled', 'ledger_enabled') !== false,
  }
}

export function getLedgerStatus() {
  return v2Get('/admin/ledger/status', undefined, normalizeLedgerStatus)
}

export function getLedger(params: {
  From?: string
  To?: string
  Provider?: string
  Account?: string
  Model?: string
  // ledger_id 精确查（Spec P05 关键交互）
  LedgerID?: string
  Limit?: number
  Offset?: number
}) {
  return v2Get('/admin/ledger/entries', params, normalizeLedger)
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

// ===== 内部模型 / 拓扑（T1 GET /admin/topology、T4 GET /admin/internal-models、T3 sync、T2 links） =====
// 字段名以 `backend/internal/domain/topology/handler.go` 的 view 为准（PR #88 已合入 v2，PascalCase 逐字对齐）：
//   Providers[] {Provider, Models, Stale, Accounts}         ← **没有 ref 字段**：供应商身份就是 Provider 名，
//                                                             和 Items[].ProviderRef 是同一套取值
//   Accounts[]  {Provider, Account, Subscribed, LastSeenAt} ← **没有 ref 字段**：账号身份 = (Provider, Account)
//   Items[]     {Ref, ProviderRef, Kind, …}                 ← Kind=group 时 ProviderRef 恒空（组是跨供应商聚合层）
//   Externals[] {ID, ExternalName, Enabled, BoundInternalRef, InternalModelHit}
// 绑定真源是 external_models.bound_internal_ref；model_links 只存画布坐标（不是关系真源）。
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

// 第 1 层（供应商）：由 internal_models 聚合，`Accounts` 是「该供应商下出现过账号」的计数。
// `Stale` 是**本轮未见的条数**（不是布尔）：别拿它当 true/false 用，节点标灰以 Items[].Stale 为准。
export interface TopologyProvider {
  Provider: string
  Models: number
  Stale: number
  Accounts: number
}

// 第 2 层（上游账号）：V2 没有账号镜像表，这一层是「账单 / 账本 / 订阅三处来源并集」现算的。
// 后端只给这四个字段——没有额度窗口、没有最近调用时间；页面缺就留空，不要自己编。
export interface TopologyAccount {
  Provider: string
  Account: string
  // Subscribed = 该账号已录入订阅（有固定月费，参与分摊）
  Subscribed: boolean
  LastSeenAt: string
}

// 第 4 层（对外模型）：同时承载绑定边。
// `BoundInternalRef` 空串 = 未绑定（后端不带 omitempty 下发，null/空串同义）；
// `InternalModelHit=false` 且绑定了目标 = **绑定悬空**（目标已从网关下线/改名），页面必须标出来。
export interface TopologyExternal {
  ID: string
  ExternalName: string
  Enabled: boolean
  BoundInternalRef: string
  InternalModelHit: boolean
}

// 同步状态（T1 的同步块）：`State` 取 ok/failed/never，`Message` 是失败原因原文。
// 同步失败时页面**保留旧快照**并显示失败原因与上次成功时间，不清空（Spec P02 验收 3）。
export interface TopologySync {
  LastSuccessAt: string
  LastRunAt: string
  State: 'ok' | 'failed' | 'never' | ''
  Message: string
  Created: number
  Stale: number
}

export interface InternalModelView {
  Items: InternalModel[]
  Links: ModelLink[]
  Providers: TopologyProvider[]
  Accounts: TopologyAccount[]
  Externals: TopologyExternal[]
  StaleCount: number
  UnboundExternal: number
  SyncedAt: string
  Sync: TopologySync
}

function normalizeTopologySync(raw: Record<string, unknown>): TopologySync {
  return {
    LastSuccessAt: str(pick(raw, 'LastSuccessAt', 'last_success_at')),
    LastRunAt: str(pick(raw, 'LastRunAt', 'last_run_at')),
    State: normalizeSyncState(pick(raw, 'State', 'state')),
    Message: str(pick(raw, 'Message', 'message')),
    Created: num(pick(raw, 'Created', 'created')),
    Stale: num(pick(raw, 'Stale', 'stale')),
  }
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

  // 供应商层：T1 给了就用 T1 的（Models / Stale / Accounts 三个计数）；没给就按 Items 的 ProviderRef 现算。
  // 供应商身份的取值口径在两条路径上必须一致（`Provider` === `Items[].ProviderRef`），否则卡里会一行模型都挂不上。
  const providersRaw = arr(pick(raw, 'Providers', 'providers'))
  const providers: TopologyProvider[] = providersRaw.length
    ? providersRaw
        .map((p) => ({
          Provider: str(pick(p, 'Provider', 'provider')),
          Models: num(pick(p, 'Models', 'models')),
          Stale: num(pick(p, 'Stale', 'stale')),
          Accounts: num(pick(p, 'Accounts', 'accounts')),
        }))
        .filter((p) => p.Provider !== '')
    : deriveProviders(items)

  // 账号层：(Provider, Account) 就是唯一键，后端不给 ref。缺这个块就空着——账号没有镜像表，编不出来。
  const accounts = arr(pick(raw, 'Accounts', 'accounts'))
    .map((a): TopologyAccount => ({
      Provider: str(pick(a, 'Provider', 'provider')),
      Account: str(pick(a, 'Account', 'account')),
      Subscribed: Boolean(pick(a, 'Subscribed', 'subscribed')),
      LastSeenAt: str(pick(a, 'LastSeenAt', 'last_seen_at')),
    }))
    .filter((a) => a.Account !== '')

  // 对外模型（售卖面）：绑定真源是 bound_internal_ref；空串 = 未绑定。
  // T1 只给「绑定状态」，不给价格档（价格在模型编辑页），所以这里没有 PriceIn/PriceOut——不要凭空补 0。
  const externals = arr(pick(raw, 'Externals', 'externals')).map(
    (e): TopologyExternal => ({
      ID: str(pick(e, 'ID', 'id')),
      ExternalName: str(pick(e, 'ExternalName', 'external_name', 'Name', 'name')),
      Enabled: pick(e, 'Enabled', 'enabled') === undefined ? true : Boolean(pick(e, 'Enabled', 'enabled')),
      BoundInternalRef: str(pick(e, 'BoundInternalRef', 'bound_internal_ref')),
      InternalModelHit: Boolean(pick(e, 'InternalModelHit', 'internal_model_hit')),
    }),
  )

  // 计数以 T1 为准；T1 没给这一段（老响应）时才按 Items/Externals 现算。
  // 用「字段在不在」判断而不是 `||`：后端真给 0 时不能被现算顶掉（0 条未绑定 ≠ 没这项）。
  const staleCountRaw = pick(raw, 'StaleCount', 'stale_count')
  const unboundRaw = pick(raw, 'UnboundExternal', 'unbound_external')

  return {
    Items: items,
    Links: links,
    Providers: providers,
    Accounts: accounts,
    Externals: externals,
    StaleCount: staleCountRaw === undefined ? items.filter((i) => i.Stale).length : num(staleCountRaw),
    UnboundExternal:
      unboundRaw === undefined ? externals.filter((e) => !e.BoundInternalRef).length : num(unboundRaw),
    SyncedAt: str(pick(raw, 'SyncedAt', 'synced_at')),
    Sync: normalizeTopologySync(obj(pick(raw, 'Sync', 'sync'))),
  }
}

// deriveProviders：没有 T1 的 Providers 块时的降级聚合（只到「供应商 → 模型数 / 未见条数」，账号计数无从得知）。
// 路由组**不参与**聚合：网关侧组是跨供应商的聚合层，后端对组恒给 `ProviderRef=''`
// （backend/internal/domain/topology/source.go `parseGroups`），算进来就会凭空多一张空的「(未知)」供应商卡。
function deriveProviders(items: InternalModel[]): TopologyProvider[] {
  const map = new Map<string, TopologyProvider>()
  for (const m of items) {
    if (m.Kind === 'group') continue
    const ref = m.ProviderRef || '(未知)'
    const cur = map.get(ref) || { Provider: ref, Models: 0, Stale: 0, Accounts: 0 }
    cur.Models += 1
    if (m.Stale) cur.Stale += 1
    map.set(ref, cur)
  }
  return [...map.values()].sort((a, b) => b.Models - a.Models)
}

// 页面取的是 T1（四层聚合：providers / internal models / bindings / 同步状态）；
// T4（`/admin/internal-models`）是 P03 的绑定候选接口，形状是同一个 normalizer。
export function getTopology() {
  return v2Get('/admin/topology', undefined, normalizeInternalModels)
}

export function getInternalModels() {
  return v2Get('/admin/internal-models', undefined, normalizeInternalModels)
}

// 画布落位（T2 PUT /admin/topology/links）：**只存坐标**，不建立绑定
// （绑定真源是 external_models.bound_internal_ref，改动属 T5）
export function saveTopologyLinks(items: Array<Pick<ModelLink, 'InternalRef' | 'ExternalModelID' | 'PosX' | 'PosY'>>) {
  return v2Put<{ Saved: number }>('/admin/topology/links', { Items: items }, (raw) => ({
    Saved: num(pick(raw, 'Saved', 'saved')),
  }))
}

// 手动同步（T3）：与后台定时同步共用实现，返回拉取数 / 新增 / 失效 + 业务结果 `State`。
// ⚠️ 失败也回 **200**（业务结果而非客户端错误，见 backend/.../topology/handler.go `HandleSync`）：
// 只按 HTTP 状态判断会把 `State=failed` 渲染成绿字「同步完成」= 假成功。所以 `State` 是必读字段。
export type TopologySyncState = 'ok' | 'failed' | 'never' | ''

export interface TopologySyncResult {
  Fetched: number
  Created: number
  Stale: number
  State: TopologySyncState
  Message: string
}

function normalizeSyncState(v: unknown): TopologySyncState {
  const st = str(v)
  return st === 'ok' || st === 'failed' || st === 'never' ? st : ''
}

export function syncTopology() {
  return v2Post<TopologySyncResult>('/admin/topology/sync', {}, (raw) => ({
    Fetched: num(pick(raw, 'Fetched', 'fetched')),
    Created: num(pick(raw, 'Created', 'created')),
    Stale: num(pick(raw, 'Stale', 'stale')),
    State: normalizeSyncState(pick(raw, 'State', 'state')),
    Message: str(pick(raw, 'Message', 'message')),
  }))
}

// ===== 订阅（P04；接口 S1/S2/S7）=====
// 字段与校验口径的唯一依据：`docs/designs/2026-11-16-subscription-entry-standard.md`（Spec 2026-11-16 生效版）。
// 最容易写错的一条：录入端收「元/美元」，落库 `price_amount` 是**积分**
// （后端 × 1,000,000 ÷ R，USD 先 × cny_rate），所以列表与表单都要同时给「原币」和 `PriceCredits`。
export type PriceCurrency = 'CNY' | 'USD'
export type PricingCycle = 'month' | 'year' | 'once'

// 派生状态（不落列，优先级从上到下，标准 §2.3）
export type SubscriptionStatus = 'stopped' | 'once' | 'missing_cycle' | 'pending' | 'expired' | 'active'

// 「摊 0 笔」原因码（标准 §5.2）——页面逐条翻译成运营能看懂的文案与处置动作
export type BlockReasonCode =
  | 'share_off'
  | 'price_zero'
  | 'cycle_once'
  | 'cycle_missing'
  // 账期起点还没到（今天起账 = active，不是 pending）
  | 'cycle_pending'
  | 'cycle_expired'
  | 'no_billing_rows'
  | 'no_attribution'
  | 'account_mismatch'
  | 'ledger_taken'
  | 'unknown'

export interface SubscriptionWarning {
  Code: string
  Message: string
}

export interface Subscription {
  ID: string
  Provider: string
  Account: string
  // Price = 原币金额（元 / 美元），仅录入与展示用
  Price: number
  Currency: PriceCurrency
  // PriceCredits = 折算后的积分；null = 后端未折算或缺 R（不要用 0 冒充）
  PriceCredits: number | null
  Cycle: PricingCycle
  PayDay: number
  ShareCost: boolean
  CycleStart: string
  Note: string
  Status: SubscriptionStatus
  // WillAmortize 只判 C1–C4（静态条件）；C5/C6 是运行时事实，靠本期分摊明细回显
  WillAmortize: boolean
  BlockReasons: BlockReasonCode[]
  Warnings: SubscriptionWarning[]
  // 本期已摊 / 未摊出（S4 聚合；null = 还没算过）
  CycleAllocated: number | null
  CycleUnallocated: number | null
  CreatedAt: string
  UpdatedAt: string
}

// 录入入参（字段名与标准 §2.1 的「API 字段」一致）
export interface SubscriptionInput {
  Provider: string
  Account: string
  Price: number
  Currency: PriceCurrency
  Cycle: PricingCycle
  PayDay: number
  ShareCost: boolean
  CycleStart: string
  Note: string
}

// 录入页候选（S7）：provider 只能从镜像里选，账号是软候选（可手输但要提示风险）
export interface SubscriptionFormOptions {
  Providers: string[]
  Accounts: string[]
  // 汇率可用性：USD 录入的前置条件（缺 → 422 rate_not_configured）
  CnyRate: number | null
  R: number | null
}

export interface SubscriptionView {
  Items: Subscription[]
  Total: number
}

const CURRENCIES: PriceCurrency[] = ['CNY', 'USD']
const CYCLES: PricingCycle[] = ['month', 'year', 'once']
const STATUSES: SubscriptionStatus[] = ['stopped', 'once', 'missing_cycle', 'pending', 'expired', 'active']
const BLOCK_CODES: BlockReasonCode[] = [
  'share_off',
  'price_zero',
  'cycle_once',
  'cycle_missing',
  'cycle_pending',
  'cycle_expired',
  'no_billing_rows',
  'no_attribution',
  'account_mismatch',
  'ledger_taken',
  'unknown',
]

function codeList(v: unknown): BlockReasonCode[] {
  if (!Array.isArray(v)) return []
  return (v as unknown[]).map((x) => String(x) as BlockReasonCode).filter((x) => BLOCK_CODES.includes(x))
}

function parseDay(v: string): Date | null {
  // 只接受 YYYY-MM-DD（标准 §3.1 的录入格式），其余按「没填」处理
  if (!/^\d{4}-\d{2}-\d{2}$/.test(v)) return null
  const d = new Date(`${v}T00:00:00`)
  return isNaN(d.getTime()) ? null : d
}

function ymd(d: Date): string {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

// cycleEnd：账期窗口右端（不含）。month +1 月、year +1 年、once 视为当天结束。
export function cycleEnd(cycleStart: string, cycle: PricingCycle): Date | null {
  const start = parseDay(cycleStart)
  if (!start) return null
  if (cycle === 'once') return new Date(start.getTime() + 86400_000)
  const d = new Date(start.getTime())
  if (cycle === 'year') d.setFullYear(d.getFullYear() + 1)
  else d.setMonth(d.getMonth() + 1)
  return d
}

// deriveSubscriptionStatus：前端也按同一优先级算一次（列表标签/排序要用）。
// 口径与标准 §2.3 逐条一致，后端返回 Status 时以返回值为准。
export function deriveSubscriptionStatus(
  s: Pick<Subscription, 'ShareCost' | 'Cycle' | 'CycleStart'>,
  today = new Date(),
): SubscriptionStatus {
  if (!s.ShareCost) return 'stopped'
  if (s.Cycle === 'once') return 'once'
  if (!s.CycleStart) return 'missing_cycle'
  const t = new Date(today.getFullYear(), today.getMonth(), today.getDate())
  const start = parseDay(s.CycleStart)
  if (!start) return 'missing_cycle'
  if (start.getTime() > t.getTime()) return 'pending'
  const end = cycleEnd(s.CycleStart, s.Cycle)
  if (!end) return 'missing_cycle'
  if (t.getTime() >= end.getTime()) return 'expired'
  return 'active'
}

// willAmortize：只判 C1–C4（标准 §5.1）。C5/C6 不许在这里承诺。
export function staticAmortizeVerdict(
  s: Pick<Subscription, 'ShareCost' | 'Cycle' | 'CycleStart' | 'Price'>,
  priceCredits: number | null,
  today = new Date(),
): { will: boolean; reasons: BlockReasonCode[] } {
  const reasons: BlockReasonCode[] = []
  if (!s.ShareCost) reasons.push('share_off')
  const credits = priceCredits == null ? s.Price : priceCredits
  if (!(credits > 0)) reasons.push('price_zero')
  if (s.Cycle === 'once') reasons.push('cycle_once')
  if (s.ShareCost && s.Cycle !== 'once') {
    if (!s.CycleStart) reasons.push('cycle_missing')
    else if (deriveSubscriptionStatus(s, today) === 'expired') reasons.push('cycle_expired')
    else if (deriveSubscriptionStatus(s, today) === 'pending') reasons.push('cycle_pending')
  }
  return { will: reasons.length === 0, reasons }
}

export function normalizeSubscriptions(raw: Record<string, unknown>): SubscriptionView {
  const items = arr(pick(raw, 'Items', 'items')).map((s): Subscription => {
    const cur = str(pick(s, 'Currency', 'PriceCurrency', 'price_currency')) as PriceCurrency
    const cyc = str(pick(s, 'Cycle', 'BillingCycle', 'billing_cycle')) as PricingCycle
    const cycle: PricingCycle = CYCLES.includes(cyc) ? cyc : 'month'
    const shareCost = pick(s, 'ShareCost', 'share_cost') !== false
    const cycleStart = str(pick(s, 'CycleStart', 'cycle_start'))
    const price = num(pick(s, 'Price', 'PriceAmount', 'price_amount'))
    const base = { ShareCost: shareCost, Cycle: cycle, CycleStart: cycleStart, Price: price }
    const creditsRaw = pick(s, 'PriceCredits', 'price_credits')
    const priceCredits = creditsRaw === undefined ? null : numOrNull(creditsRaw)
    const statusRaw = str(pick(s, 'Status', 'status')) as SubscriptionStatus
    const derived = staticAmortizeVerdict(base, priceCredits)
    const reasonsRaw = pick(s, 'BlockReasons', 'block_reasons')
    return {
      ID: str(pick(s, 'ID', 'id')),
      Provider: str(pick(s, 'Provider', 'ProviderRef', 'provider_ref')),
      Account: str(pick(s, 'Account', 'AccountRef', 'account_ref')),
      Price: price,
      Currency: CURRENCIES.includes(cur) ? cur : 'CNY',
      PriceCredits: priceCredits,
      Cycle: cycle,
      PayDay: num(pick(s, 'PayDay', 'pay_day')) || 1,
      ShareCost: shareCost,
      CycleStart: cycleStart,
      Note: str(pick(s, 'Note', 'note')),
      Status: STATUSES.includes(statusRaw) ? statusRaw : deriveSubscriptionStatus(base),
      WillAmortize: pick(s, 'WillAmortize', 'will_amortize') === undefined ? derived.will : Boolean(pick(s, 'WillAmortize', 'will_amortize')),
      BlockReasons: reasonsRaw === undefined ? derived.reasons : codeList(reasonsRaw),
      Warnings: arr(pick(s, 'Warnings', 'warnings')).map((w) => ({
        Code: str(pick(w, 'Code', 'code')),
        Message: str(pick(w, 'Message', 'message')),
      })),
      CycleAllocated: numOrNull(pick(s, 'CycleAllocated', 'cycle_allocated')),
      CycleUnallocated: numOrNull(pick(s, 'CycleUnallocated', 'cycle_unallocated')),
      CreatedAt: str(pick(s, 'CreatedAt', 'created_at')),
      UpdatedAt: str(pick(s, 'UpdatedAt', 'updated_at')),
    }
  })
  return { Items: items, Total: num(pick(raw, 'Total', 'total')) || items.length }
}

export function getSubscriptions(params: {
  Provider?: string
  Account?: string
  Status?: SubscriptionStatus
  ShareCost?: boolean
  Page?: number
  Size?: number
} = {}) {
  return v2Get('/admin/subscriptions', params, normalizeSubscriptions)
}

export function getSubscriptionFormOptions() {
  return v2Get('/admin/subscriptions/form-options', undefined, (raw): SubscriptionFormOptions => {
    const providers = Array.isArray(pick(raw, 'Providers', 'providers')) ? (pick(raw, 'Providers', 'providers') as unknown[]) : []
    const accounts = Array.isArray(pick(raw, 'Accounts', 'accounts')) ? (pick(raw, 'Accounts', 'accounts') as unknown[]) : []
    return {
      Providers: providers.map((p) => String(p)),
      Accounts: accounts.map((a) => String(a)),
      CnyRate: numOrNull(pick(raw, 'CnyRate', 'cny_rate')),
      R: numOrNull(pick(raw, 'R', 'r')),
    }
  })
}

// 新建（S2）。本期**不写库**，页面拿它做「接口就绪后即可接通」的接线点；
// 409 的 ExistingID 由页面用 (Provider, Account) 在本地列表里反查——不额外造一遍请求。
export function createSubscription(input: SubscriptionInput) {
  return v2Post('/admin/subscriptions', input, (raw) => normalizeSubscriptions({ Items: [raw] }).Items[0])
}

// 折算积分：录入页实时显示「≈ N 积分」。与后端同源公式（× 1e6 ÷ R，USD 先 × cny_rate）。
// 缺 R / 缺汇率时返回 null（页面显示「—」，不许显示 0 让人以为不花钱）。
export function priceToCredits(price: number, currency: PriceCurrency, cnyRate: number | null, r: number | null): number | null {
  if (!r || r <= 0) return null
  if (!Number.isFinite(price) || price < 0) return null
  let cny = price
  if (currency === 'USD') {
    if (!cnyRate || cnyRate <= 0) return null
    cny = price * cnyRate
  }
  // 后端落 NUMERIC(20,5)：这里按 5 位小数四舍五入，保证页面上「≈ N 积分」和落库值看到的是同一个数量级
  return Math.round(((cny * 1_000_000) / r) * 1e5) / 1e5
}

export { ymd as formatYmd }
