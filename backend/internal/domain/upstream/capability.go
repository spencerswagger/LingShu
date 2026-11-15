package upstream

import (
	"context"
	"fmt"
	"time"
)

// LedgerOrder 是「上游账本接口的排序能力」的三种结论。
//
// 为什么要先证后写（F9 裁定）：翻页正确性完全依赖 `ORDER BY occurred_at ASC, ledger_id ASC`
// 的**稳定**排序。上游若不保证，offset 翻页会同时出现两种坏结果——同一条重复（幂等键能吃掉）
// 和某条永不出现（静默漏账，且 offset 已持久化，永远不会自愈）。
// 所以：证不了支持，就不许清零该窗，也不许自己造一个排序把账「理顺」。
type LedgerOrder int

const (
	// LedgerOrderUnverified 表示还没证过（默认值，最保守）。
	LedgerOrderUnverified LedgerOrder = iota
	// LedgerOrderSupported 表示实测过：相邻两页整体单调不减，且同值按 ledger_id 稳定。
	LedgerOrderSupported
	// LedgerOrderUnsupported 表示实测反例：乱序、跨页回退、或缺 tie-break。
	LedgerOrderUnsupported
)

// String 便于日志与 detail 记录（这三个词也会进 reconcile_windows.detail）。
func (o LedgerOrder) String() string {
	switch o {
	case LedgerOrderSupported:
		return "supported"
	case LedgerOrderUnsupported:
		return "unsupported"
	default:
		return "unverified"
	}
}

// ProbeResult 是一次排序能力的探测结论。
type ProbeResult struct {
	Order LedgerOrder
	// Reason 是人能看懂的依据（进窗 detail；不含任何凭据）。
	Reason string
	// Samples 是实际比较过的记录条数（0 = 一条都没拿到，等于没证过）。
	Samples int
	// CheckedAt 是探测时刻。
	CheckedAt time.Time
}

// ProbeLedgerOrder 用一次真实翻页证明排序能力：取相邻两页，检查
//  1. 页内非递减：`(occurred_at, ledger_id)` 字典序；
//  2. 跨页不回退：第二页首条 >= 第一页末条；
//  3. 有可比样本：至少两条（只有一条等于没证）。
//
// 注意：这是**能力探测**，不是数据校验——它比对的是「上游给的顺序」是否自洽，
// 不判断账对不对（那是 reconcile 的事）。
func ProbeLedgerOrder(ctx context.Context, src PageSource, w Window, pageSize int) ProbeResult {
	now := time.Now
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	probe := func() ProbeResult {
		p1, err := src.FetchPage(ctx, w, 0, pageSize)
		if err != nil {
			return ProbeResult{Order: LedgerOrderUnverified, Reason: "拉第一页失败：" + err.Error(), CheckedAt: now()}
		}
		p2, err := src.FetchPage(ctx, w, len(p1.Entries), pageSize)
		if err != nil {
			return ProbeResult{Order: LedgerOrderUnverified, Reason: "拉第二页失败：" + err.Error(), CheckedAt: now()}
		}
		all := make([]LedgerEntry, 0, len(p1.Entries)+len(p2.Entries))
		all = append(all, p1.Entries...)
		all = append(all, p2.Entries...)
		if len(all) < 2 {
			return ProbeResult{
				Order:     LedgerOrderUnverified,
				Reason:    fmt.Sprintf("两页共 %d 条，样本不足，证不了排序是否稳定", len(all)),
				Samples:   len(all),
				CheckedAt: now(),
			}
		}
		for i := 1; i < len(all); i++ {
			prev, cur := all[i-1], all[i]
			switch {
			case cur.OccurredAt.Before(prev.OccurredAt):
				return ProbeResult{
					Order: LedgerOrderUnsupported,
					Reason: fmt.Sprintf("第 %d 条 occurred_at 回退（%s < %s），上游不是升序",
						i+1, cur.OccurredAt.Format(time.RFC3339Nano), prev.OccurredAt.Format(time.RFC3339Nano)),
					Samples:   len(all),
					CheckedAt: now(),
				}
			case cur.OccurredAt.Equal(prev.OccurredAt) && cur.LedgerID < prev.LedgerID:
				return ProbeResult{
					Order: LedgerOrderUnsupported,
					Reason: fmt.Sprintf("同一 occurred_at 下 ledger_id 未按升序（%q 在 %q 之后）——"+
						"缺 tie-break 时翻页会重复或漏条", cur.LedgerID, prev.LedgerID),
					Samples:   len(all),
					CheckedAt: now(),
				}
			}
		}
		if len(p2.Entries) == 0 && len(p1.Entries) == pageSize {
			return ProbeResult{
				Order:   LedgerOrderUnverified,
				Reason:  "第一页已满但第二页为空，翻页语义不自洽，证不了排序",
				Samples: len(all), CheckedAt: now(),
			}
		}
		return ProbeResult{
			Order:   LedgerOrderSupported,
			Reason:  fmt.Sprintf("实测 %d 条：occurred_at 升序、同值按 ledger_id 升序、跨页不回退", len(all)),
			Samples: len(all), CheckedAt: now(),
		}
	}
	res := probe()
	if res.CheckedAt.IsZero() {
		res.CheckedAt = now()
	}
	return res
}

// ErrLedgerOrderUnproven 表示「上游排序能力未证实/已被证否」，本窗不许清零。
//
// 它不是失败重试：重试也证不出排序，只能等上游给出能力承诺（或修好接口）。
type ErrLedgerOrderUnproven struct {
	Window Window
	Order  LedgerOrder
	Reason string
}

func (e *ErrLedgerOrderUnproven) Error() string {
	return fmt.Sprintf("上游账本排序能力 %s，窗口 %s 记 blocked 且不可清零：%s",
		e.Order, e.Window, e.Reason)
}

// IsLedgerOrderUnproven 报告 err 是否为排序能力缺失导致的 blocked。
func IsLedgerOrderUnproven(err error) bool {
	_, ok := err.(*ErrLedgerOrderUnproven)
	return ok
}
