// 盈亏（P07）演示数据。字段名与 `@/api/v2` 的 PnlView 一致。
//
// 两处刻意的「陷阱」：
//   1. 第三组成本为 0 且 Uncosted>0 → 页面必须提示「毛利虚高」，不许当真实毛利；
//   2. 收入 ≤0 时 MarginPct 为 null → 页面显示「—」，不许显示 0%。
import type { PnlView } from '@/api/v2'

function day(offset: number): string {
  const base = Date.UTC(2026, 10, 16, 8, 0, 0)
  return new Date(base + offset * 86400_000).toISOString()
}

export function mockPnl(groupBy: 'provider' | 'account' | 'model' = 'provider'): PnlView {
  const keys =
    groupBy === 'provider'
      ? ['acme', 'borealis', 'cirrus']
      : groupBy === 'account'
        ? ['acme/main', 'borealis/team-2', 'cirrus/(unknown)']
        : ['gpt-x', 'claude-y', '(unknown)']
  const groups = keys.map((k, i) => {
    const revenue = [1200, 800, 0][i]
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
      AmortizedCost: i === 0 ? 200 : i === 1 ? 200 : 0,
      Uncosted: [0, 0, 900][i],
    }
  })
  return {
    From: day(-1),
    To: day(0),
    GroupBy: groupBy,
    Requests: groups.reduce((s, g) => s + g.Requests, 0),
    Tokens: groups.reduce((s, g) => s + g.Tokens, 0),
    Revenue: 2000,
    Cost: 1200,
    Margin: 800,
    MarginPct: 0.4,
    Uncosted: 900,
    Groups: groups,
    Truncated: false,
  }
}
