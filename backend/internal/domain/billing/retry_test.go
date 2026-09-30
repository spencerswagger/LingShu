package billing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/team/llmgateway/internal/pkg/dbx"
)

// fakeRetryStore 可切换成功/失败的 Store 桩，用于验证重试队列的重投语义。
type fakeRetryStore struct {
	failInserts bool
	failErr     error
	inserted    []string
}

func (f *fakeRetryStore) GetByBillingID(context.Context, string) (*Record, error) {
	return nil, nil
}

func (f *fakeRetryStore) Insert(_ context.Context, rec *Record) (*Record, error) {
	if f.failInserts {
		err := f.failErr
		if err == nil {
			err = errors.New("db down")
		}
		return nil, err
	}
	f.inserted = append(f.inserted, rec.BillingID)
	return rec, nil
}

func (f *fakeRetryStore) InsertTx(ctx context.Context, _ dbx.Execer, rec *Record) (*Record, error) {
	return f.Insert(ctx, rec)
}

func TestRetryQueue_EnqueueFlushRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retry.jsonl")
	q := NewRetryQueue(path, nil)

	if err := q.Enqueue(&Record{BillingID: "bill-1", UserID: 7, Status: StatusCompleted}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if n := q.Pending(); n != 1 {
		t.Fatalf("pending want 1, got %d", n)
	}

	// DB 仍不可用：重投失败但记录必须保留（不丢账单）。
	store := &fakeRetryStore{failInserts: true}
	ok, pending, err := q.Flush(context.Background(), store)
	if err == nil {
		t.Fatal("仍有待重投时 Flush 应返回错误（触发告警）")
	}
	if ok != 0 || pending != 1 {
		t.Fatalf("ok=%d pending=%d, want 0/1", ok, pending)
	}
	if n := q.Pending(); n != 1 {
		t.Fatalf("失败记录必须保留在队列, pending=%d", n)
	}

	// DB 恢复：重投成功并移除。
	store.failInserts = false
	ok, pending, err = q.Flush(context.Background(), store)
	if err != nil || ok != 1 || pending != 0 {
		t.Fatalf("flush after recovery: ok=%d pending=%d err=%v", ok, pending, err)
	}
	if len(store.inserted) != 1 || store.inserted[0] != "bill-1" {
		t.Fatalf("重投记录有误: %+v", store.inserted)
	}
	if n := q.Pending(); n != 0 {
		t.Fatalf("队列应已清空, pending=%d", n)
	}
}

// 撞唯一索引说明该账单已落库（重投幂等）→ 视为成功并移除，不算失败。
func TestRetryQueue_UniqueViolationTreatedAsDone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retry.jsonl")
	q := NewRetryQueue(path, nil)
	if err := q.Enqueue(&Record{BillingID: "bill-dup", Status: StatusCompleted}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	store := &fakeRetryStore{failInserts: true, failErr: errors.New("duplicate key value violates unique constraint (23505)")}
	ok, pending, err := q.Flush(context.Background(), store)
	if err != nil || ok != 1 || pending != 0 {
		t.Fatalf("唯一冲突应按成功处理: ok=%d pending=%d err=%v", ok, pending, err)
	}
	if n := q.Pending(); n != 0 {
		t.Fatalf("队列应已清空, pending=%d", n)
	}
}

// 无法解析的行原样保留（绝不丢数据），并让 Flush 报错以便告警。
func TestRetryQueue_KeepsUnparsableLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retry.jsonl")
	if err := os.WriteFile(path, []byte("{not-json}\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	q := NewRetryQueue(path, nil)

	_, pending, err := q.Flush(context.Background(), &fakeRetryStore{})
	if err == nil || pending != 1 {
		t.Fatalf("无法解析的行应保留并报错: pending=%d err=%v", pending, err)
	}
	if n := q.Pending(); n != 1 {
		t.Fatalf("坏行不得被丢弃, pending=%d", n)
	}
}

// path 为空 → 返回 nil（关闭兜底），所有方法可安全调用。
func TestRetryQueue_Disabled(t *testing.T) {
	q := NewRetryQueue("", nil)
	if q != nil {
		t.Fatal("空路径应返回 nil 队列")
	}
	if err := q.Enqueue(&Record{BillingID: "x"}); err != nil {
		t.Fatalf("nil 队列 Enqueue 应为 no-op: %v", err)
	}
	if n := q.Pending(); n != 0 {
		t.Fatalf("nil 队列 Pending 应为 0, got %d", n)
	}
	ok, pending, err := q.Flush(context.Background(), &fakeRetryStore{})
	if err != nil || ok != 0 || pending != 0 {
		t.Fatalf("nil 队列 Flush 应为 no-op: ok=%d pending=%d err=%v", ok, pending, err)
	}
}
