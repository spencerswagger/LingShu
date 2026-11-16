// 对账窗口（P06）演示数据。字段名与 `@/api/v2` 的 ReconcileView / ReconcileWindow 一致。
import type { ReconcileView } from '@/api/v2'

function minute(offset: number): string {
  const base = Date.UTC(2026, 10, 16, 10, 0, 0)
  return new Date(base + offset * 60_000).toISOString()
}

export function mockReconcile(): ReconcileView {
  const items: ReconcileView['Items'] = [
    {
      WindowStart: minute(-5),
      WindowEnd: minute(-4),
      Status: 'ok',
      AlarmStreak: 0,
      LocalCount: 120,
      UpstreamCount: 120,
      LocalTokens: 480_000,
      UpstreamTokens: 483_000,
      DeviationRate: 0.006,
      ScrollOffset: 0,
      CheckedAt: minute(-3),
      SkipReason: '',
      Detail: {},
    },
    {
      WindowStart: minute(-4),
      WindowEnd: minute(-3),
      Status: 'alarm',
      AlarmStreak: 3,
      LocalCount: 118,
      UpstreamCount: 90,
      LocalTokens: 470_000,
      UpstreamTokens: 431_000,
      DeviationRate: 0.083,
      ScrollOffset: 0,
      CheckedAt: minute(-2),
      SkipReason: '',
      Detail: { reconcile: { groups: 3 } },
    },
    {
      WindowStart: minute(-3),
      WindowEnd: minute(-2),
      Status: 'blocked',
      AlarmStreak: 0,
      LocalCount: null,
      UpstreamCount: null,
      LocalTokens: null,
      UpstreamTokens: null,
      DeviationRate: null,
      ScrollOffset: 4000,
      CheckedAt: minute(-2),
      SkipReason: '',
      Detail: { sync: { pages: 20, last_error: '' } },
    },
    {
      WindowStart: minute(-2),
      WindowEnd: minute(-1),
      Status: 'skipped',
      AlarmStreak: 0,
      LocalCount: null,
      UpstreamCount: null,
      LocalTokens: null,
      UpstreamTokens: null,
      DeviationRate: null,
      ScrollOffset: 0,
      CheckedAt: minute(-1),
      SkipReason: 'ledger_unavailable',
      Detail: {},
    },
    {
      WindowStart: minute(-1),
      WindowEnd: minute(0),
      Status: 'error',
      AlarmStreak: 0,
      LocalCount: null,
      UpstreamCount: null,
      LocalTokens: null,
      UpstreamTokens: null,
      DeviationRate: null,
      ScrollOffset: 0,
      CheckedAt: minute(0),
      SkipReason: '',
      Detail: { sync: { last_error: 'upstream 502' } },
    },
  ]
  return { From: minute(-60), To: minute(0), Items: items }
}
