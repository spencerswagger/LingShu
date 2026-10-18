package router

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/pkg/asyncwrite"
)

// TestSwitchMigrate_UpdatesChannelAndPersists 中途切换后的会话迁移：
// Migrate 更新 Session.ChannelKeyID/InternalModelID（Lookup 反映新渠道）、迁移并发计数、
// 刷新 LastSwitchAt，并经 sessionPersister.Upsert 落库。
func TestSwitchMigrate_UpdatesChannelAndPersists(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	f := &fakePersister{}
	r.SetStore(f)

	mustAttach(t, r, "s1", 1, 2, 11, now.Add(time.Hour))

	if !r.Migrate("s1", 22, "int2", 10, 0) {
		t.Fatal("迁移应成功")
	}
	s, ok := r.Lookup("s1")
	if !ok {
		t.Fatal("迁移后会话应仍存在")
	}
	if s.ChannelKeyID != 22 || s.InternalModelID != "int2" {
		t.Fatalf("迁移后应更新渠道密钥/内部模型，got key=%d model=%q", s.ChannelKeyID, s.InternalModelID)
	}
	if !s.LastSwitchAt.Equal(now) {
		t.Fatalf("迁移应刷新 LastSwitchAt=now，got %v", s.LastSwitchAt)
	}
	if r.CountActive(11) != 0 || r.CountActive(22) != 1 {
		t.Fatalf("迁移后计数异常：key11=%d key22=%d", r.CountActive(11), r.CountActive(22))
	}
	if len(f.upserts) != 2 {
		t.Fatalf("迁移应 Upsert 一次（连同初次 attach 共 2 次），got %d", len(f.upserts))
	}
	if last := f.upserts[len(f.upserts)-1]; last.ChannelKeyID != 22 || last.InternalModelID != "int2" {
		t.Fatalf("Upsert 应落库新渠道，got key=%d model=%q", last.ChannelKeyID, last.InternalModelID)
	}
}

// TestSwitchMigrate_SlidingTTLExtendsOnly 迁移沿用 Renew 的滑动 TTL 语义：只延长、不缩短。
func TestSwitchMigrate_SlidingTTLExtendsOnly(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	r.SetStore(&fakePersister{})

	mustAttach(t, r, "s1", 1, 2, 11, now.Add(time.Hour))

	// 候选续期时刻早于现有 ExpireAt → 不得缩短。
	if !r.Migrate("s1", 22, "int2", 10, 30*time.Minute) {
		t.Fatal("迁移应成功")
	}
	if s, _ := r.Lookup("s1"); !s.ExpireAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("较短 TTL 不应缩短 ExpireAt，got %v", s.ExpireAt)
	}
	// 更长 TTL → 延长。
	if !r.Migrate("s1", 33, "int3", 10, 3*time.Hour) {
		t.Fatal("迁移应成功")
	}
	if s, _ := r.Lookup("s1"); !s.ExpireAt.Equal(now.Add(3 * time.Hour)) {
		t.Fatalf("更长 TTL 应延长 ExpireAt，got %v", s.ExpireAt)
	}
}

// TestSwitchMigrate_ConcurrencyLimitKeepsOldChannel 目标密钥并发会话超限时原子回退：
// 不切换、保持原渠道（Lookup 仍为旧密钥）、不迁移计数、不产生新的落库。
func TestSwitchMigrate_ConcurrencyLimitKeepsOldChannel(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	f := &fakePersister{}
	r.SetStore(f)

	// 目标密钥 21 已达上限 2。
	mustAttach(t, r, "a", 1, 1, 21, now.Add(time.Hour))
	mustAttach(t, r, "b", 1, 1, 21, now.Add(time.Hour))
	// 待迁移会话在密钥 11。
	mustAttach(t, r, "s1", 1, 1, 11, now.Add(time.Hour))

	upsertsBefore := len(f.upserts)
	if r.Migrate("s1", 21, "int2", 2, 0) {
		t.Fatal("目标密钥超并发上限时应回退（返回 false）")
	}
	s, _ := r.Lookup("s1")
	if s.ChannelKeyID != 11 || s.InternalModelID != "int" {
		t.Fatalf("回退应保持原渠道(key=11,int)，got key=%d model=%q", s.ChannelKeyID, s.InternalModelID)
	}
	if s.LastSwitchAt != (time.Time{}) {
		t.Fatalf("回退不应刷新 LastSwitchAt，got %v", s.LastSwitchAt)
	}
	if r.CountActive(11) != 1 || r.CountActive(21) != 2 {
		t.Fatalf("回退后计数不应变化：key11=%d key21=%d", r.CountActive(11), r.CountActive(21))
	}
	if len(f.upserts) != upsertsBefore {
		t.Fatalf("回退不应产生新的落库，got %d (before=%d)", len(f.upserts), upsertsBefore)
	}
}

// TestSwitchMigrate_SameKeyRefreshesModel 目标密钥与当前相同：仅刷新内部模型并滑动续期，不涉及计数迁移。
func TestSwitchMigrate_SameKeyRefreshesModel(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	r.SetStore(&fakePersister{})

	mustAttach(t, r, "s1", 1, 2, 11, now.Add(time.Hour))

	if !r.Migrate("s1", 11, "int-switched", 10, 0) {
		t.Fatal("同密钥刷新应返回 true")
	}
	s, _ := r.Lookup("s1")
	if s.ChannelKeyID != 11 || s.InternalModelID != "int-switched" {
		t.Fatalf("同密钥应刷新内部模型，got key=%d model=%q", s.ChannelKeyID, s.InternalModelID)
	}
	if r.CountActive(11) != 1 {
		t.Fatalf("同密钥刷新不应改变计数，got %d", r.CountActive(11))
	}
}

// TestSwitchMigrate_AsyncUpsertAndSetClosedOrdered 经既有 asyncwrite AsyncPersister（Key=session_id 保序）：
// 同一会话的迁移 Upsert 先于 SetClosed 到达底层，最终状态为 closed=true（不乱序复活）。
func TestSwitchMigrate_AsyncUpsertAndSetClosedOrdered(t *testing.T) {
	ctx := context.Background()
	w := asyncwrite.New(asyncwrite.Config{
		WALPath:    filepath.Join(t.TempDir(), "wal.log"),
		AsyncKinds: map[asyncwrite.Kind]bool{asyncwrite.KindSession: true},
	})
	w.Start(ctx)
	defer func() { _ = w.Shutdown(ctx) }()

	inner := &orderedPersister{}
	p := NewAsyncPersister(w, inner)

	r := NewSessionRegistry(nil)
	r.SetStore(p)

	// Attach（Upsert）→ Migrate（Upsert）→ Close（SetClosed），全部 Key=sessionID 串行。
	mustAttach(t, r, "s1", 1, 2, 11, time.Now().Add(time.Hour))
	if !r.Migrate("s1", 22, "int2", 10, 0) {
		t.Fatal("迁移应成功")
	}
	if !r.Close("s1") {
		t.Fatal("关闭应成功")
	}

	waitAsyncDepthZero(t, w)

	got := inner.snapshot()
	if len(got) < 2 {
		t.Fatalf("应至少有 Upsert 与 SetClosed 两次落库，got %v", got)
	}
	last2 := got[len(got)-2:]
	if last2[0] != "upsert:s1" || last2[1] != "closed:s1:true" {
		t.Fatalf("同会话变更未保序（应 upsert→closed），got %v", got)
	}
	inner.mu.Lock()
	closed := inner.closed
	inner.mu.Unlock()
	if !closed {
		t.Fatalf("最终状态应为 closed=true（不得被乱序 Upsert 复活）")
	}
}
