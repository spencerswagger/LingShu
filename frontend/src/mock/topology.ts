// 上游拓扑（P02）演示数据。字段名与 `@/api/v2` 的 InternalModel / ModelLink 一致。
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
  // 四层图的另外三层（T1 一并下发）：供应商 / 上游账号 / 对外模型（含绑定）
  const providers: TopologyProvider[] = [...new Set(items.map((m) => m.ProviderRef))].map((p) => {
    const mine = items.filter((m) => m.ProviderRef === p)
    return {
      Provider: p,
      Models: mine.length,
      Stale: mine.filter((m) => m.Stale).length,
      Accounts: 1,
    }
  })
  const accounts: TopologyAccount[] = providers.map((p, i) => ({
    Provider: p.Provider,
    Account: `acct-${i + 1}`,
    Subscribed: i === 0,
    LastSeenAt: minute(-5 - i),
  }))
  const externals: TopologyExternal[] = [
    { ID: '300', ExternalName: 'gpt-x', Enabled: true, BoundInternalRef: items[0].Ref, InternalModelHit: true },
    { ID: '301', ExternalName: 'claude-y', Enabled: true, BoundInternalRef: items[2].Ref, InternalModelHit: true },
    { ID: '302', ExternalName: 'gpt-x-mini', Enabled: true, BoundInternalRef: items[1].Ref, InternalModelHit: true },
    // 未绑定：四层图里要能看出「这个对外模型还没接内部模型」
    { ID: '303', ExternalName: 'legacy-compat', Enabled: false, BoundInternalRef: '', InternalModelHit: false },
    // 悬空绑定：目标已从网关下线（画布上画红色虚线）
    { ID: '304', ExternalName: 'deprecated-x', Enabled: true, BoundInternalRef: 'acme/retired', InternalModelHit: false },
  ]
  return {
    Providers: providers,
    Accounts: accounts,
    Items: items,
    Externals: externals,
    Links: links,
    StaleCount: 1,
    UnboundExternal: 2,
    SyncedAt: minute(-1),
    // 同步状态条（T1/T3）未就绪时给演示值，明确标注为演示
    Sync: { LastSuccessAt: minute(-1), LastRunAt: minute(-1), State: 'ok', Message: '', Created: 2, Stale: 1 },
  }
}
