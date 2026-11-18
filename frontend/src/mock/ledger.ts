// 账本流水（P05）演示数据。字段名与 `@/api/v2` 的 LedgerView / LedgerStatus 一致。
import type { LedgerView } from '@/api/v2'

function minute(offset: number): string {
  const base = Date.UTC(2026, 10, 16, 10, 0, 0)
  return new Date(base + offset * 60_000).toISOString()
}

export function mockLedger(): LedgerView {
  const items = Array.from({ length: 8 }, (_, i) => ({
    ID: String(9001 + i),
    LedgerID: `led_${1000 + i}`,
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

// 新鲜度卡（L2）。刻意做成「滞后 210s」→ 页面应显示橙色「账本不新鲜」。
export function mockLedgerStatus() {
  return {
    LastOccurredAt: minute(-3.5),
    LastSuccessAt: minute(-1),
    LagSeconds: 210,
    BlockedWindows: 1,
    ErrorWindows: 0,
    PendingOffset: 4000,
    BudgetExhausted: true,
    LedgerEnabled: true,
  }
}
