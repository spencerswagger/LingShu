// 上游拓扑（P02）演示数据。
//
// 形状与真实报文**逐字对齐**（`backend/internal/domain/topology/handler.go` 的 view）：
//   Providers[] {Provider, Models, Stale, Accounts}         ← 没有 ref 字段，身份就是 Provider 名
//   Accounts[]  {Provider, Account, Subscribed, LastSeenAt} ← 没有 ref 字段，身份 = (Provider, Account)
//   Items[]     {Ref, ProviderRef, Kind, …}                 ← Kind=group 的 ProviderRef 恒空
//   Externals[] {ID, ExternalName, Enabled, BoundInternalRef, InternalModelHit}
// 故意照真态给：①含 1 个路由组；②含未绑定的对外模型；③含绑定悬空的对外模型（目标已下线）。
// 只在用户显式点「预览演示数据」时渲染（财务/拓扑页默认绝不显示假数据）。
import type {
  InternalModel,
  InternalModelView,
  ModelLink,
  TopologyAccount,
  TopologyExternal,
  TopologyProvider,
} from '@/api/v2'

function minute(offset: number): string {
  const base = Date.UTC(2026, 10, 16, 10, 0, 0)
  return new Date(base + offset * 60_000).toISOString()
}

export function mockTopology(): InternalModelView {
  // 直通模型 4 个（挂 3 个供应商）+ 路由组 1 个（ProviderRef 恒空，跨供应商聚合层）
  const items: InternalModel[] = [
    { Ref: 'acme/gpt-x', ProviderRef: 'acme', Kind: 'direct', DisplayName: 'gpt-x', ContextLen: 128_000 },
    { Ref: 'acme/gpt-x-mini', ProviderRef: 'acme', Kind: 'direct', DisplayName: 'gpt-x-mini', ContextLen: 32_000 },
    { Ref: 'borealis/claude-y', ProviderRef: 'borealis', Kind: 'direct', DisplayName: 'claude-y', ContextLen: 200_000 },
    { Ref: 'group/team-2', ProviderRef: '', Kind: 'group', DisplayName: 'team-2', ContextLen: 200_000 },
    { Ref: 'cirrus/legacy', ProviderRef: 'cirrus', Kind: 'direct', DisplayName: 'legacy', ContextLen: 8_000 },
  ].map((m, i) => ({
    ...m,
    Kind: m.Kind as InternalModel['Kind'],
    PosX: 120 + (i % 3) * 220,
    PosY: 100 + Math.floor(i / 3) * 160,
    LastSeenAt: minute(-1),
    // 本轮未见：cirrus 那一个（页面标灰 + 计入 StaleCount）
    Stale: m.Ref === 'cirrus/legacy',
  }))

  const links: ModelLink[] = items.slice(0, 3).map((m, i) => ({
    InternalRef: m.Ref,
    ExternalModelID: String(300 + i),
    PosX: m.PosX + 120,
    PosY: m.PosY + 90,
  }))

  // 3 家供应商；Stale 是**条数**（不是布尔）：cirrus 那条本轮未见。
  const providers: TopologyProvider[] = [
    { Provider: 'acme', Models: 2, Stale: 0, Accounts: 2 },
    { Provider: 'borealis', Models: 1, Stale: 0, Accounts: 1 },
    { Provider: 'cirrus', Models: 1, Stale: 1, Accounts: 0 },
  ]

  // 账号：V2 没有账号镜像表，这一层是「账单 / 账本 / 订阅」三处并集现算的，所以只有这四个字段。
  const accounts: TopologyAccount[] = [
    { Provider: 'acme', Account: 'acct-main', Subscribed: true, LastSeenAt: minute(-2) },
    { Provider: 'acme', Account: 'acct-payg', Subscribed: false, LastSeenAt: minute(-8) },
    { Provider: 'borealis', Account: 'acct-team-abt', Subscribed: true, LastSeenAt: minute(-1) },
  ]

  // 对外模型 = 第 4 层 + 绑定边。四种状态都给：绑直通 / 绑组 / 未绑定 / 绑定悬空。
  const externals: TopologyExternal[] = [
    { ID: '300', ExternalName: 'gpt-x', Enabled: true, BoundInternalRef: 'acme/gpt-x', InternalModelHit: true },
    { ID: '301', ExternalName: 'claude-y', Enabled: true, BoundInternalRef: 'borealis/claude-y', InternalModelHit: true },
    { ID: '302', ExternalName: 'team-2', Enabled: true, BoundInternalRef: 'group/team-2', InternalModelHit: true },
    // 未绑定：画布上照常出节点（灰化 + 「不售卖」），别让运营看不见「我建了但没卖」
    { ID: '303', ExternalName: 'legacy-compat', Enabled: false, BoundInternalRef: '', InternalModelHit: false },
    // 绑定悬空：目标已从网关下线/改名（画布上标出来，否则运营以为「绑定还在、只是没画线」）
    { ID: '304', ExternalName: 'deprecated-x', Enabled: true, BoundInternalRef: 'acme/retired', InternalModelHit: false },
  ]

  return {
    Providers: providers,
    Accounts: accounts,
    Items: items,
    Externals: externals,
    Links: links,
    StaleCount: 1,
    // 未绑定 = bound_internal_ref 为空串（悬空绑定不算未绑定）
    UnboundExternal: 1,
    SyncedAt: minute(-1),
    // 同步状态条（T1/T3）：演示值，State 取 ok/failed/never 三态
    Sync: { LastSuccessAt: minute(-1), LastRunAt: minute(-1), State: 'ok', Message: '', Created: 2, Stale: 1 },
  }
}
