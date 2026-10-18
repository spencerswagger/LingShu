package router

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/pkg/asyncwrite"
)

// orderedPersister 记录底层 store 收到的操作时序，用于断言「同会话变更不乱序」。
type orderedPersister struct {
	mu     sync.Mutex
	events []string
	closed bool
}

func (f *orderedPersister) Upsert(_ context.Context, s *Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "upsert:"+s.SessionID)
	f.closed = false // 与 SQL UPSERT 语义一致：Upsert 复位为活跃
	return nil
}

func (f *orderedPersister) Delete(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "delete:"+sessionID)
	return nil
}

func (f *orderedPersister) DeleteMany(_ context.Context, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.events = append(f.events, "delete:"+id)
	}
	return nil
}

func (f *orderedPersister) SetClosed(_ context.Context, sessionID string, closed bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "closed:"+sessionID+":"+boolStr(closed))
	f.closed = closed
	return nil
}

func (f *orderedPersister) LoadActive(_ context.Context, _ time.Time) ([]*Session, error) {
	return nil, nil
}

func (f *orderedPersister) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// waitAsyncDepthZero 轮询等待异步 writer 的在途任务清空。
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

// TestAsyncPersister_UpsertThenSetClosed 锁定核心契约：同一 session_id 的 Upsert 与 SetClosed
// 经同一 Key 分片串行执行，底层收到顺序为 upsert→closed，最终状态为 closed=true（不乱序复活）。
func TestAsyncPersister_UpsertThenSetClosed(t *testing.T) {
	ctx := context.Background()
	w := asyncwrite.New(asyncwrite.Config{
		WALPath:    filepath.Join(t.TempDir(), "wal.log"),
		AsyncKinds: map[asyncwrite.Kind]bool{asyncwrite.KindSession: true},
	})
	w.Start(ctx)
	defer func() { _ = w.Shutdown(ctx) }()

	inner := &orderedPersister{}
	p := NewAsyncPersister(w, inner)

	sess := &Session{
		SessionID: "s1", UserID: 1, ChannelKeyID: 42, InternalModelID: "int",
		CreatedAt: time.Now(), LastActive: time.Now(), ExpireAt: time.Now().Add(time.Hour),
	}
	if err := p.Upsert(ctx, sess); err != nil {
		t.Fatalf("Upsert 提交失败: %v", err)
	}
	if err := p.SetClosed(ctx, "s1", true); err != nil {
		t.Fatalf("SetClosed 提交失败: %v", err)
	}

	waitAsyncDepthZero(t, w)

	got := inner.snapshot()
	if len(got) != 2 || got[0] != "upsert:s1" || got[1] != "closed:s1:true" {
		t.Fatalf("同会话变更未按序到达底层：%v", got)
	}
	inner.mu.Lock()
	closed := inner.closed
	inner.mu.Unlock()
	if !closed {
		t.Fatalf("最终状态应为 closed=true（不得被乱序 Upsert 复活）")
	}
}

// TestAsyncPersister_DeleteManyPerID 「DeleteMany 逐条以 session_id 为 Key 提交」：
// 每个会话各产生一次独立删除，互不合并。
func TestAsyncPersister_DeleteManyPerID(t *testing.T) {
	ctx := context.Background()
	w := asyncwrite.New(asyncwrite.Config{
		WALPath:    filepath.Join(t.TempDir(), "wal.log"),
		AsyncKinds: map[asyncwrite.Kind]bool{asyncwrite.KindSession: true},
	})
	w.Start(ctx)
	defer func() { _ = w.Shutdown(ctx) }()

	inner := &orderedPersister{}
	p := NewAsyncPersister(w, inner)

	if err := p.DeleteMany(ctx, []string{"a", "b"}); err != nil {
		t.Fatalf("DeleteMany 提交失败: %v", err)
	}
	waitAsyncDepthZero(t, w)

	got := inner.snapshot()
	if len(got) != 2 {
		t.Fatalf("DeleteMany 应逐条提交，期望 2 条，got %v", got)
	}
	seen := map[string]bool{}
	for _, e := range got {
		seen[e] = true
	}
	if !seen["delete:a"] || !seen["delete:b"] {
		t.Fatalf("DeleteMany 未逐条落库：%v", got)
	}
}

// TestAsyncPersister_LoadActivePassthrough LoadActive 保持同步直读，不进入异步队列。
func TestAsyncPersister_LoadActivePassthrough(t *testing.T) {
	p := NewAsyncPersister(nil, &fakePersister{loadRows: []*Session{{SessionID: "loaded"}}})
	rows, err := p.LoadActive(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("LoadActive 失败: %v", err)
	}
	if len(rows) != 1 || rows[0].SessionID != "loaded" {
		t.Fatalf("LoadActive 应同步直读底层，got %v", rows)
	}
}

// TestAsyncPersister_RepeatedUpsertAllPersist 锁定修复后的契约（回归）：
// 同一会话的多次 Upsert（续期）必须每次都落到 DB——不得用「sessionID|op」做进程内去重把
// 首次之后的续期吞掉（否则 last_active/expire_at 永不刷新）。
func TestAsyncPersister_RepeatedUpsertAllPersist(t *testing.T) {
	ctx := context.Background()
	w := asyncwrite.New(asyncwrite.Config{
		WALPath:    filepath.Join(t.TempDir(), "wal.log"),
		AsyncKinds: map[asyncwrite.Kind]bool{asyncwrite.KindSession: true},
	})
	w.Start(ctx)
	defer func() { _ = w.Shutdown(ctx) }()

	inner := &orderedPersister{}
	p := NewAsyncPersister(w, inner)

	const n = 5
	for i := 0; i < n; i++ {
		if err := p.Upsert(ctx, &Session{
			SessionID: "s-renew", ChannelKeyID: 42, InternalModelID: "int",
			LastActive: time.Now(), ExpireAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("第 %d 次 Upsert 提交失败: %v", i+1, err)
		}
	}
	waitAsyncDepthZero(t, w)

	got := inner.snapshot()
	if len(got) != n {
		t.Fatalf("续期应每次落库，期望 %d 次，got %d：%v", n, len(got), got)
	}
	for _, e := range got {
		if e != "upsert:s-renew" {
			t.Fatalf("非预期事件：%v", got)
		}
	}
}
