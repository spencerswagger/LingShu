// 上游拓扑（P02）演示数据。字段名与 `@/api/v2` 的 InternalModel / ModelLink 一致。
import type { InternalModel, InternalModelView, ModelLink } from '@/api/v2'

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
  return {
    Items: items,
    Links: links,
    StaleCount: 1,
    UnboundExternal: 2,
    SyncedAt: minute(-1),
    // 同步状态条（T1/T3）未就绪时给演示值，明确标注为演示
    Sync: { LastSuccessAt: minute(-1), LastRunAt: minute(-1), State: 'ok', Message: '', Created: 2, Stale: 1 },
  }
}
