package upstream

import (
	"context"
	"errors"
	"testing"
)

// TestLedgerRecord_Validate 断言「最小九字段」这个形状守卫：多一个少一个都不许落库。
func TestLedgerRecord_Validate(t *testing.T) {
	// 九字段是已批准口径（实施计划开放项 ②），锁死数字防止顺手改小。
	if LedgerFieldCount != 9 {
		t.Fatalf("账本最小字段数应为 9，got %d", LedgerFieldCount)
	}
	ok := LedgerRecord{Values: make([]any, LedgerFieldCount)}
	if err := ok.Validate(); err != nil {
		t.Fatalf("九字段应通过校验，got %v", err)
	}
	for _, n := range []int{0, LedgerFieldCount - 1, LedgerFieldCount + 1} {
		rec := LedgerRecord{Values: make([]any, n)}
		if err := rec.Validate(); err == nil {
			t.Fatalf("%d 字段应校验失败", n)
		}
	}
}

// TestUnavailableWriter 断言占位实现：**明确失败**而不是零值成功（账本是钱的口径，静默丢账更糟）。
func TestUnavailableWriter(t *testing.T) {
	w := NewUnavailableWriter()
	rec := LedgerRecord{Values: make([]any, LedgerFieldCount)}

	ref, err := w.Append(context.Background(), rec)
	if !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("占位实现应返回 ErrLedgerUnavailable，got %v", err)
	}
	if ref != "" {
		t.Fatalf("失败时不应返回账本引用，got %q", ref)
	}

	// 形状非法时先报形状错误（优先暴露调用方缺陷，不被「未装配」掩盖）。
	bad := LedgerRecord{Values: make([]any, 3)}
	if _, err := w.Append(context.Background(), bad); err == nil || errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("形状非法应优先报字段数量错误，got %v", err)
	}

	if _, err := w.List(context.Background(), 0, 50); !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("占位 List 应返回 ErrLedgerUnavailable，got %v", err)
	}
}
