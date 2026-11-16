// V2 运营台演示数据（**仅用于接口未就绪时的页面预览，绝不可当成真数据**）。
//
// 为什么要有它：阶段三前端要先把页面结构、口径提示、空态/异常态做完，而后端接口分批上线。
// 规定：
//   1. 只有用户显式点「预览演示数据」才渲染，默认永远是空态（财务页不允许默认看到假数字）；
//   2. 每个页面在顶部挂「演示数据」横幅，任何截图/汇报都必须带上这个标记；
//   3. 数值刻意做得不像真实运营数据（整数、明显规律），避免被误读。
// 这里的所有字段名与 `@/api/v2` 的类型一致，接口上线后删本文件即可，页面不用改。
import type {
  CreditHold,
  InternalModel,
  LedgerView,
  ModelLink,
  PnlView,
  ReconcileView,
  Subscription,
} from './v2'

function day(offset: number): string {
  const base = Date.UTC(2026, 10, 16, 8, 0, 0) // 2026-11-16 16:00 +08:00
  return new Date(base + offset * 86400_000).toISOString()
}

function minute(offset: number): string {
  const base = Date.UTC(2026, 10, 16, 10, 0, 0)
  return new Date(base + offset * 60_000).toISOString()
}

// 盈亏：三家供应商，其中一家的成本还没有口径（Uncosted > 0），用来演示「毛利虚高」提示。
export function mockPnl(groupBy: 'provider' | 'account' | 'model' = 'provider'): PnlView {
  const keys = groupBy === 'provider' ? ['acme', 'borealis', 'cirrus'] : groupBy === 'account' ? ['acme/main', 'borealis/team-2', 'cirrus/(unknown)'] : ['gpt-x', 'claude-y', '(unknown)']
  const groups = keys.map((k, i) => {
    const revenue = [1200, 800, 400][i]
    const cost = [700, 500, 0][i]
    const margin = revenue - cost
    return {
      Key: k,
      Requests: [4000, 2600, 900][i],
      Tokens: [8_000_000, 5_000_000, 1_200_000][i],
      Revenue: revenue,
      Cost: cost,
      Margin: margin,
      MarginPct: revenue > 0 ? Math.round((margin / revenue) * 1e5) / 1e5 : null,
      LedgerCost: i === 0 ? 500 : i === 1 ? 300 : 0,
      AmortizedCost: i === 1 ? 200 : i === 0 ? 200 : 0,
      Uncosted: [0, 0, 900][i],
    }
  })
  return {
    From: day(-1),
    To: day(0),
    GroupBy: groupBy,
    Requests: groups.reduce((s, g) => s + g.Requests, 0),
    Tokens: groups.reduce((s, g) => s + g.Tokens, 0),
    Revenue: 2400,
    Cost: 1200,
    Margin: 1200,
    MarginPct: 0.5,
    Uncosted: 900,
    Groups: groups,
    Truncated: false,
  }
}

// 对账：覆盖全部五种状态，含「计数列 null = 还没算过」与 blocked 待补拉。
export function mockReconcile(): ReconcileView {
  const rows: ReconcileView['Items'] = [
    { WindowStart: minute(-5), WindowEnd: minute(-4), Status: 'ok', AlarmStreak: 0, LocalCount: 120, UpstreamCount: 120, LocalTokens: 480_000, UpstreamTokens: 483_000, DeviationRate: 0.006, ScrollOffset: 0, CheckedAt: minute(-3) },
    { WindowStart: minute(-4), WindowEnd: minute(-3), Status: 'alarm', AlarmStreak: 2, LocalCount: 118, UpstreamCount: 90, LocalTokens: 470_000, UpstreamTokens: 431_000, DeviationRate: 0.083, ScrollOffset: 0, CheckedAt: minute(-2) },
    { WindowStart: minute(-3), WindowEnd: minute(-2), Status: 'blocked', AlarmStreak: 0, LocalCount: null, UpstreamCount: null, LocalTokens: null, UpstreamTokens: null, DeviationRate: null, ScrollOffset: 4000, CheckedAt: minute(-2) },
    { WindowStart: minute(-2), WindowEnd: minute(-1), Status: 'skipped', AlarmStreak: 0, LocalCount: null, UpstreamCount: null, LocalTokens: null, UpstreamTokens: null, DeviationRate: null, ScrollOffset: 0, CheckedAt: minute(-1) },
    { WindowStart: minute(-1), WindowEnd: minute(0), Status: 'error', AlarmStreak: 0, LocalCount: null, UpstreamCount: null, LocalTokens: null, UpstreamTokens: null, DeviationRate: null, ScrollOffset: 0, CheckedAt: minute(0) },
  ]
  return { From: minute(-60), To: minute(0), Items: rows }
}

export function mockLedger(): LedgerView {
  const items = Array.from({ length: 8 }, (_, i) => ({
    ID: String(9001 + i),
    LedgerID: `led_${(1000 + i).toString()}`,
    Provider: ['acme', 'borealis'][i % 2],
    ProviderAccount: ['main', 'team-2'][i % 2],
    Model: ['gpt-x', 'claude-y'][i % 2],
    TokensIn: 1200 + i * 10,
    TokensOut: 800 + i * 5,
    CostCredits: 0.5 + i * 0.1,
    OccurredAt: minute(-i),
  }))
  return {
    Items: items,
    Total: items.length,
    CostTotal: Math.round(items.reduce((s, i) => s + i.CostCredits, 0) * 1e5) / 1e5,
    TokensIn: items.reduce((s, i) => s + i.TokensIn, 0),
    TokensOut: items.reduce((s, i) => s + i.TokensOut, 0),
  }
}

export function mockHolds(): { Items: CreditHold[]; HeldAmount: number; HeldCount: number } {
  const items: CreditHold[] = [
    { ID: '7001', BillingID: 'b_9001', UserID: '12', TokenID: '88', Amount: 2.5, Status: 'held', LedgerEntryID: '', ExpiresAt: minute(15), ClosedAt: '', CreatedAt: minute(-2) },
    { ID: '7002', BillingID: 'b_9002', UserID: '13', TokenID: '89', Amount: 1.25, Status: 'settled', LedgerEntryID: 'led_1000', ExpiresAt: minute(10), ClosedAt: minute(-1), CreatedAt: minute(-5) },
    { ID: '7003', BillingID: 'b_9003', UserID: '14', TokenID: '90', Amount: 3, Status: 'refunded', LedgerEntryID: '', ExpiresAt: minute(8), ClosedAt: minute(-2), CreatedAt: minute(-6) },
    { ID: '7004', BillingID: 'b_9004', UserID: '15', TokenID: '91', Amount: 0.75, Status: 'expired', LedgerEntryID: '', ExpiresAt: minute(-20), ClosedAt: minute(-19), CreatedAt: minute(-40) },
  ]
  const held = items.filter((i) => i.Status === 'held')
  return { Items: items, HeldCount: held.length, HeldAmount: held.reduce((s, i) => s + i.Amount, 0) }
}

export function mockInternalModels(): {
  Items: InternalModel[]
  Links: ModelLink[]
  StaleCount: number
  UnboundExternal: number
  SyncedAt: string
} {
  const refs = ['acme/gpt-x', 'acme/gpt-x-mini', 'borealis/claude-y', 'borealis/team-2/opus', 'cirrus/legacy']
  const items: InternalModel[] = refs.map((ref, i) => ({
    Ref: ref,
    ProviderRef: ref.split('/')[0],
    Kind: i === 1 ? 'group' : 'direct',
    DisplayName: ref.split('/').slice(1).join('/'),
    ContextLen: [128_000, 32_000, 200_000, 200_000, 8_000][i],
    PosX: 120 + (i % 3) * 220,
    PosY: 100 + Math.floor(i / 3) * 160,
    LastSeenAt: minute(-1),
    Stale: i === 4,
  }))
  const links: ModelLink[] = items.slice(0, 3).map((m, i) => ({
    InternalRef: m.Ref,
    ExternalModelID: String(300 + i),
    PosX: m.PosX + 120,
    PosY: m.PosY + 90,
  }))
  return { Items: items, Links: links, StaleCount: 1, UnboundExternal: 2, SyncedAt: minute(-1) }
}

export function mockSubscriptions(): { Items: Subscription[]; MonthlyTotal: number; UnstartedCount: number } {
  const items: Subscription[] = [
    { ID: '501', ProviderRef: 'acme', AccountRef: 'main', PriceAmount: 300, PriceCurrency: 'CNY', BillingCycle: 'month', PayDay: 5, ShareCost: true, CycleStart: '2026-11-01', Note: '主力账号', CreatedAt: day(-30), UpdatedAt: day(-2) },
    { ID: '502', ProviderRef: 'borealis', AccountRef: 'team-2', PriceAmount: 120, PriceCurrency: 'USD', BillingCycle: 'year', PayDay: 20, ShareCost: false, CycleStart: '', Note: '年付，不参与分摊', CreatedAt: day(-20), UpdatedAt: day(-2) },
  ]
  return {
    Items: items,
    MonthlyTotal: 300,
    UnstartedCount: items.filter((i) => !i.CycleStart).length,
  }
}
