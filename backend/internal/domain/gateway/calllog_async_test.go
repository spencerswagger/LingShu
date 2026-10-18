package gateway

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/pkg/asyncwrite"
)

// safeCallLogStore 并发安全的内存调用日志存储（async 执行在 worker 协程，读取须加锁）。
type safeCallLogStore struct {
	mu       sync.Mutex
	inserted []string // billing_id 序列
}

func (f *safeCallLogStore) Insert(_ context.Context, c *CallLog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inserted = append(f.inserted, c.BillingID)
	return nil
}

func (f *safeCallLogStore) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.inserted...)
}

func waitAsyncDepthZero(t *testing.T, w *asyncwrite.Writer) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if w.Stats().Depth == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待异步队列清空超时，Depth=%d", w.Stats().Depth)
}

// TestAsyncCallLogStore_SubmitThenExecuteAndDedup 断言：Insert 提交后由 worker 异步落到内层 store；
// 相同 billing_id 再次提交被幂等去重，内层只落一次。
func TestAsyncCallLogStore_SubmitThenExecuteAndDedup(t *testing.T) {
	ctx := context.Background()
	w := asyncwrite.New(asyncwrite.Config{
		WALPath:    filepath.Join(t.TempDir(), "wal.log"),
		AsyncKinds: map[asyncwrite.Kind]bool{asyncwrite.KindCallLog: true},
	})
	w.Start(ctx)
	defer func() { _ = w.Shutdown(ctx) }()

	inner := &safeCallLogStore{}
	s := NewAsyncCallLogStore(w, inner)

	if err := s.Insert(ctx, &CallLog{BillingID: "b1", RequestID: "r1", Status: "completed"}); err != nil {
		t.Fatalf("Insert 提交失败: %v", err)
	}
	waitAsyncDepthZero(t, w)

	if got := inner.snapshot(); len(got) != 1 || got[0] != "b1" {
		t.Fatalf("提交后应由 worker 异步落库一次，got %v", got)
	}

	// 相同 billing_id 重复提交：幂等去重，仅落一次。
	if err := s.Insert(ctx, &CallLog{BillingID: "b1", RequestID: "r2", Status: "completed"}); err != nil {
		t.Fatalf("重复 Insert 提交失败: %v", err)
	}
	waitAsyncDepthZero(t, w)

	if got := inner.snapshot(); len(got) != 1 {
		t.Fatalf("重复 billing_id 应只落一次，got %v", got)
	}
}

// TestAsyncCallLogStore_FallbackSynchronous writer 为空时同步直调内层（安全兜底）。
func TestAsyncCallLogStore_FallbackSynchronous(t *testing.T) {
	inner := &safeCallLogStore{}
	s := NewAsyncCallLogStore(nil, inner)
	if err := s.Insert(context.Background(), &CallLog{BillingID: "b2"}); err != nil {
		t.Fatalf("同步兜底 Insert 失败: %v", err)
	}
	if got := inner.snapshot(); len(got) != 1 || got[0] != "b2" {
		t.Fatalf("writer 为空时应同步落库，got %v", got)
	}
}
