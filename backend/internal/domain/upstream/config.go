// Package upstream 是灵枢的「上游账本链路」域：账本变更流拉取（ledger/feed）、
// 60s 对账窗口（window）、账本行落库与对账计数写入。
//
// V3 起对外转发由 nginx 直连代理侧网关端口，灵枢的逐请求代理链与预扣（preauth）
// 已整体下线；本包不再持有任何「转发运行参数」——曾经的 upstream.base_url /
// upstream.timeouts / upstream.degrade 三键与降级开关门随代理链一并移除。
// 账本链路可用性的唯一判据是环境变量 UPSTREAM_LEDGER_URL（见 LedgerGate）。
//
// 铁律（延续）：
//   - 本包只读上游数据与落库，不参与请求转发；
//   - **任何密钥一律不进 sys_configs、不进代码、不进日志**：上游鉴权凭据只以
//     「装配层从环境变量注入」的方式持有，不落到配置表与错误文案里。
package upstream

import (
	"context"
	"strings"
)

// LedgerGate 是账本链路的可用性闸门。
//
// LedgerActive 报告账本拉取是否可用 —— **只看账本端点是否配置**（env `UPSTREAM_LEDGER_URL`）。
//
// 为什么不看 upstream.base_url / 降级开关（关键修复，2026-11-17）：
//   - V3 起转发已由 nginx 直连代理侧，base_url 不参与任何链路；
//   - 旧同步轮闸门 `SyncActive()` = base_url 已配 && sync 开；base_url 默认空串（seeding 播种 ""）
//     且没有管理端写入口 → 闸门恒 false → 每 60s 静默 return → ledger_entries 永远空 →
//     v3billing.Poller 无行可结算 → 账单不出、对账窗口 skipped；
//   - 可用性判据统一收敛到本类型（同步轮 / 对账 LedgerEnabled 都用它）：
//     端点配了就拉，没配就停（装配层每轮 WARN，不冒充成功）。
//
// 账本端点来自环境变量（非 sys_configs），是静态值，不需要读库 / TTL。
type LedgerGate struct {
	// ledgerURL 是账本链路端点（env `UPSTREAM_LEDGER_URL`），是账本链路**唯一**可用性判据。
	ledgerURL string
}

// NewLedgerGate 创建账本可用性闸门。ledgerURL 为空表示未配置（一切账本轮次跳过）。
func NewLedgerGate(ledgerURL string) *LedgerGate {
	return &LedgerGate{ledgerURL: strings.TrimSpace(ledgerURL)}
}

// LedgerActive 报告账本拉取是否可用（只看端点是否配置）。
func (g *LedgerGate) LedgerActive(_ context.Context) bool {
	ok, _ := g.LedgerAvailability()
	return ok
}

// LedgerAvailability 返回账本链路是否可跑与不可跑的原因（装配层据此决定是否打 WARN）。
//
// ok=false 且 reason 非空 ⇒ 本轮跳过并打 WARN，**绝不冒充成功**（不落 blocked、不写假账单）。
// LedgerActive 与它同源，保证「同步轮闸门」与「对账 LedgerEnabled」判定一致。
func (g *LedgerGate) LedgerAvailability() (ok bool, reason string) {
	if g == nil || strings.TrimSpace(g.ledgerURL) == "" {
		return false, "未配置 UPSTREAM_LEDGER_URL（账本同步/对账停摆，不写假账单）"
	}
	return true, ""
}
