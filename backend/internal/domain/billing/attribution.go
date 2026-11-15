package billing

import (
	"fmt"
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
func validCostSource(v string) bool {
	if strings.TrimSpace(v) == "" {
		return true
	}
	for _, s := range CostSources() {
		if v == s {
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

// attributionArgs 校验并返回六个归因列的入库参数（顺序与 INSERT 列清单一致）。
//
// 校验放在写入前：非法协议/成本来源会撞迁移里的 CHECK 约束，那时只剩一条 PG 错误码，
// 排障要多跳一次；这里直接给出「哪个字段、什么值」。
func attributionArgs(rec *Record) ([]any, error) {
	if !upstream.ValidAttributionProtocol(rec.UpstreamProtocol) {
		return nil, fmt.Errorf("账单 %s 的 upstream_protocol 非法：%q（取值域 %v）",
			rec.BillingID, rec.UpstreamProtocol, upstream.AttributionProtocols())
	}
	if !validCostSource(rec.CostSource) {
		return nil, fmt.Errorf("账单 %s 的 cost_source 非法：%q（取值域 %v）",
			rec.BillingID, rec.CostSource, CostSources())
	}
	return []any{
		nullableText(rec.RequestID),
		nullableText(rec.UpstreamProvider),
		nullableText(rec.UpstreamAccount),
		nullableText(rec.UpstreamProtocol),
		nullableText(rec.UpstreamModel),
		nullableText(rec.LedgerEntryID),
		normalizeCostSource(rec.CostSource),
	}, nil
}
