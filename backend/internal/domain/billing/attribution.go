package billing

import (
	"context"
	"log/slog"
	"strings"

	"github.com/team/llmgateway/internal/domain/upstream"
)

// V2 归因列口径（billing_records 纯 ADD，不改既有统计口径）。
//
// 七列（列名/类型逐字对齐架构侧 0020，别自己加「更合理」的列）：
//   - request_id       网关请求链路 ID（写入侧由网关传入；空 → NULL）
//   - upstream_provider / upstream_account  上游回程头里的供应商与账号（取不到 → NULL，**不猜**）
//   - upstream_protocol 上游协议，取值域见 upstream.AttributionProtocols()（空 → NULL）
//   - upstream_model    上游真实模型名（空 → NULL）
//   - ledger_entry_id   匹配到的账本条目 id（阶段一无匹配逻辑 → 恒 NULL，字段先留出）
//   - cost_source       成本来源：ledger / amortized / none（空 → none）
//
// 迁移由架构侧 0019–0023 统一落地（ADD COLUMN + CHECK），本包只负责把值写进这七列。
const (
	// CostSourceLedger 表示成本来自上游账本（真实对账口径）。
	CostSourceLedger = "ledger"
	// CostSourceAmortized 表示成本来自订阅摊销（账期均摊）。
	CostSourceAmortized = "amortized"
	// CostSourceNone 表示没有成本口径（免费/未配置/失败记录）。
	CostSourceNone = "none"
)

// CostSources 返回 cost_source 取值域（与迁移 CHECK 同源）。
func CostSources() []string {
	return []string{CostSourceLedger, CostSourceAmortized, CostSourceNone}
}

// validCostSource 判断取值是否合法；空串合法（落库归一为 none）。
// 比对前先去掉首尾空白：`" ledger "` 是同一个枚举，不该被判非法（与协议侧同口径）。
func validCostSource(v string) bool {
	t := strings.TrimSpace(v)
	if t == "" {
		return true
	}
	for _, s := range CostSources() {
		if t == s {
			return true
		}
	}
	return false
}

// normalizeCostSource 把空值归一为 none（列 NOT NULL，不留空串）。
func normalizeCostSource(v string) string {
	if strings.TrimSpace(v) == "" {
		return CostSourceNone
	}
	return v
}

// nullableText 把可选文本转为可入库的 any：空串 → NULL（缺失不猜）。
func nullableText(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

// attributionArgs 归一化并返回七个归因列的入库参数（顺序与 INSERT 列清单一致）。
//
// ⚠️ **这里不返回 error，是刻意的**（2026-11-15 预审 P1 资金事故）：
// 归因列是「解释性字段」，它非法绝不能让账单写不进去。曾经的实现把非法枚举当写入错误往上抛，
// 于是 settleTx 判「真实写入失败」→ 回滚 → 退回非事务路径**先真扣钱** → Insert 再失败 →
// 只剩一条 record lost，钱扣了账没落。现在改为**写库前归一**：非法协议 → NULL（0020 的
// CHECK 允许 NULL）、非法 cost_source → none，并各打一条 WARN 点名字段与原始值。
func attributionArgs(ctx context.Context, rec *Record) []any {
	// 先 trim 再判合法、并且**按 trim 后的值落库**：枚举列有 CHECK 约束，带空白的原文写不进去；
	// 带空白本身不是错误（上游头里常见 " openai-chat "），归一而不是拒收。
	rec.UpstreamProtocol = strings.TrimSpace(rec.UpstreamProtocol)
	rec.CostSource = strings.TrimSpace(rec.CostSource)
	if !upstream.ValidAttributionProtocol(rec.UpstreamProtocol) {
		slog.WarnContext(ctx, "账单归因值非法，按 NULL 落库",
			"billing_id", rec.BillingID, "field", "upstream_protocol",
			"value", rec.UpstreamProtocol, "allowed", upstream.AttributionProtocols())
		rec.UpstreamProtocol = ""
	}
	if !validCostSource(rec.CostSource) {
		slog.WarnContext(ctx, "账单归因值非法，按 none 落库",
			"billing_id", rec.BillingID, "field", "cost_source",
			"value", rec.CostSource, "allowed", CostSources())
		rec.CostSource = ""
	}
	return []any{
		nullableText(rec.RequestID),
		nullableText(rec.UpstreamProvider),
		nullableText(rec.UpstreamAccount),
		nullableText(rec.UpstreamProtocol),
		nullableText(rec.UpstreamModel),
		nullableText(rec.LedgerEntryID),
		normalizeCostSource(rec.CostSource),
	}
}
