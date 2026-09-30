package router

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakePersister 内存态会话持久化 fake（实现 sessionPersister，记录每次调用）。
type fakePersister struct {
	upserts    []*Session
	deletes    []string
	deleteMany [][]string
	closed     []closedRecord
	loadRows   []*Session
	upsertErr  error
	deleteErr  error
	deleteAll  error
	closedErr  error
	loadErr    error
}

type closedRecord struct {
	id     string
	closed bool
}

func (f *fakePersister) Upsert(_ context.Context, s *Session) error {
	f.upserts = append(f.upserts, s)
	return f.upsertErr
}

func (f *fakePersister) Delete(_ context.Context, sessionID string) error {
	f.deletes = append(f.deletes, sessionID)
	return f.deleteErr
}

func (f *fakePersister) DeleteMany(_ context.Context, ids []string) error {
	f.deleteMany = append(f.deleteMany, append([]string(nil), ids...))
	return f.deleteAll
}

func (f *fakePersister) SetClosed(_ context.Context, sessionID string, closed bool) error {
	f.closed = append(f.closed, closedRecord{id: sessionID, closed: closed})
	return f.closedErr
}

func (f *fakePersister) LoadActive(_ context.Context, _ time.Time) ([]*Session, error) {
	return f.loadRows, f.loadErr
}

// TestRegistry_CloseKeepsRecordReleasesSlotAndMissesRoute 「关闭」语义：
// 记录保留（Lookup/ListAll 可见）、释放并发槽（CountActive 减）、不再被路由命中（Hit=false）、
// 持久化 closed，且同标识新请求可重新激活（Attach 重建）。
func TestRegistry_CloseKeepsRecordReleasesSlotAndMissesRoute(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	f := &fakePersister{}
	r.SetStore(f)

	mustAttach(t, r, "s1", 1, 2, 42, now.Add(time.Hour))
	mustAttach(t, r, "s2", 1, 2, 42, now.Add(time.Hour))
	if r.CountActive(42) != 2 {
		t.Fatalf("attach 后计数 want 2, got %d", r.CountActive(42))
	}
	if !r.Hit("s1") {
		t.Fatalf("s1 未关闭应可命中")
	}

	if !r.Close("s1") {
		t.Fatalf("close s1 should return true")
	}
	if r.Close("s1") {
		t.Fatalf("重复关闭应返回 false")
	}
	if r.Hit("s1") {
		t.Fatalf("已关闭会话不得再被路由命中")
	}
	if r.CountActive(42) != 1 {
		t.Fatalf("关闭后并发计数 want 1, got %d", r.CountActive(42))
	}
	if _, ok := r.Lookup("s1"); !ok {
		t.Fatalf("关闭后的记录必须保留在注册表（列表可见）")
	}
	if len(f.closed) != 1 || f.closed[0].id != "s1" || !f.closed[0].closed {
		t.Fatalf("persist closed want [s1:true], got %+v", f.closed)
	}

	// 同标识新请求重新激活：旧记录被替换为新的活跃会话。
	if ok, created := r.Attach(&Session{
		SessionID: "s1", UserID: 1, ChannelKeyID: 42, InternalModelID: "int", ExpireAt: now.Add(2 * time.Hour),
	}, 10); !ok || !created {
		t.Fatalf("re-activate s1 want ok+created, got ok=%v created=%v", ok, created)
	}
	if !r.Hit("s1") {
		t.Fatalf("重新激活后应可命中")
	}
	if r.CountActive(42) != 2 {
		t.Fatalf("重新激活后计数 want 2, got %d", r.CountActive(42))
	}
}

// sortedJoin 汇总某次批量删除的会话 ID（map 迭代顺序不定，排序后断言）。
func sortedJoin(ids []string) string {
	cp := append([]string(nil), ids...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}

// mustAttach 便捷 Attach：附在密钥 keyID 上并断言成功（会话只携带 ChannelKeyID，无渠道过渡字段）。
func mustAttach(t *testing.T, r *SessionRegistry, id string, userID, tokenID, keyID int64, expireAt time.Time) {
	t.Helper()
	if ok, created := r.Attach(&Session{
		SessionID: id, UserID: userID, TokenID: tokenID,
		ChannelKeyID: keyID, InternalModelID: "int", ExpireAt: expireAt,
	}, 10); !ok || !created {
		t.Fatalf("attach %s failed: ok=%v created=%v", id, ok, created)
	}
}

func TestRegistry_AttachPersistsWithKey(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	f := &fakePersister{}
	r.SetStore(f)

	mustAttach(t, r, "s1", 1, 2, 42, now.Add(time.Hour))

	if len(f.upserts) != 1 {
		t.Fatalf("attach 后应 Upsert 一次，got %d", len(f.upserts))
	}
	up := f.upserts[0]
	if up.SessionID != "s1" || up.ChannelKeyID != 42 || up.UserID != 1 || up.TokenID != 2 {
		t.Fatalf("Upsert 参数异常：%+v", up)
	}
	if up.CreatedAt.IsZero() || !up.LastActive.Equal(now) {
		t.Fatalf("CreatedAt/LastActive 应被初始化：%+v", up)
	}
	if r.CountActive(42) != 1 {
		t.Fatalf("密钥 42 计数应为 1，got %d", r.CountActive(42))
	}
}

func TestRegistry_NoStore_MemoryOnly(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })

	mustAttach(t, r, "s1", 1, 2, 42, now.Add(time.Hour))
	if _, ok := r.Lookup("s1"); !ok {
		t.Fatal("s1 应存在")
	}
	if r.CountActive(42) != 1 {
		t.Fatalf("计数应为 1，got %d", r.CountActive(42))
	}
	if !r.Kill("s1") {
		t.Fatal("kill s1 应成功")
	}
	if r.CountActive(42) != 0 {
		t.Fatalf("kill 后计数应为 0，got %d", r.CountActive(42))
	}
}

func TestRegistry_Attach_KeyMigrationUpserts(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	f := &fakePersister{}
	r.SetStore(f)

	mustAttach(t, r, "s1", 1, 2, 11, now.Add(time.Hour))

	// 同一 sessionID 迁移到新密钥 → 第二次 Upsert，计数从旧密钥迁移。
	if ok, created := r.Attach(&Session{
		SessionID: "s1", UserID: 1, TokenID: 2,
		ChannelKeyID: 22, InternalModelID: "int", ExpireAt: now.Add(time.Hour),
	}, 10); !ok || !created {
		t.Fatalf("迁移 attach 应成功 created=true：ok=%v created=%v", ok, created)
	}

	if len(f.upserts) != 2 {
		t.Fatalf("迁移应 Upsert 两次，got %d", len(f.upserts))
	}
	if f.upserts[1].ChannelKeyID != 22 {
		t.Fatalf("Upsert 应携带新密钥 22，got %d", f.upserts[1].ChannelKeyID)
	}
	if r.CountActive(11) != 0 || r.CountActive(22) != 1 {
		t.Fatalf("迁移后计数异常：key11=%d key22=%d", r.CountActive(11), r.CountActive(22))
	}
	if s, _ := r.Lookup("s1"); s.ChannelKeyID != 22 {
		t.Fatalf("lookup 应返回新密钥，got %d", s.ChannelKeyID)
	}
}

func TestRegistry_Attach_SameKeyRenewNoUpsert(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	f := &fakePersister{}
	r.SetStore(f)

	mustAttach(t, r, "s1", 1, 2, 42, now.Add(time.Hour))
	if ok, created := r.Attach(&Session{
		SessionID: "s1", UserID: 1, TokenID: 2,
		ChannelKeyID: 42, InternalModelID: "int", ExpireAt: now.Add(2 * time.Hour),
	}, 10); !ok || created {
		t.Fatalf("同密钥 attach 应续期 created=false：ok=%v created=%v", ok, created)
	}
	if len(f.upserts) != 1 {
		t.Fatalf("同密钥续期不应额外 Upsert，got %d", len(f.upserts))
	}
	if r.CountActive(42) != 1 {
		t.Fatalf("计数应保持 1，got %d", r.CountActive(42))
	}
}

func TestRegistry_RenewPersists(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	f := &fakePersister{}
	r.SetStore(f)

	mustAttach(t, r, "s1", 1, 2, 42, now.Add(time.Hour))
	r.Renew("s1", 30*time.Minute)

	if len(f.upserts) != 2 {
		t.Fatalf("Renew 后应再 Upsert 一次，got %d", len(f.upserts))
	}
	up := f.upserts[1]
	if !up.LastActive.Equal(now) {
		t.Fatalf("LastActive 应为续期时刻，got %v", up.LastActive)
	}
	if !up.ExpireAt.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("ExpireAt 应滑动为 now+30m，got %v", up.ExpireAt)
	}
	if up.ChannelKeyID != 42 {
		t.Fatalf("Renew Upsert 应保留密钥，got %d", up.ChannelKeyID)
	}
}

func TestRegistry_SweepExpired_DeleteMany(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	f := &fakePersister{}
	r.SetStore(f)

	mustAttach(t, r, "s1", 1, 2, 42, now.Add(time.Second))
	mustAttach(t, r, "s2", 1, 2, 42, now.Add(time.Hour))

	now = now.Add(2 * time.Second) // s1 过期
	r.SweepNow()

	if len(f.deleteMany) != 1 || sortedJoin(f.deleteMany[0]) != "s1" {
		t.Fatalf("Sweep 应批量删除过期 s1，got %v", f.deleteMany)
	}
	if _, ok := r.Lookup("s1"); ok {
		t.Fatal("s1 应已清除")
	}
	if _, ok := r.Lookup("s2"); !ok {
		t.Fatal("s2 应存活")
	}
	if r.CountActive(42) != 1 {
		t.Fatalf("清扫后计数应为 1，got %d", r.CountActive(42))
	}
}

func TestRegistry_Kill_DeletesAndRemoves(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	f := &fakePersister{}
	r.SetStore(f)

	mustAttach(t, r, "s1", 1, 2, 42, now.Add(time.Hour))

	if !r.Kill("s1") {
		t.Fatal("kill s1 应成功")
	}
	if len(f.deletes) != 1 || f.deletes[0] != "s1" {
		t.Fatalf("Kill 应 Delete s1，got %v", f.deletes)
	}
	if _, ok := r.Lookup("s1"); ok {
		t.Fatal("s1 应已清除")
	}
	if r.CountActive(42) != 0 {
		t.Fatalf("kill 后计数应为 0，got %d", r.CountActive(42))
	}

	if r.Kill("ghost") {
		t.Fatal("kill 不存在的会话应返回 false")
	}
	if len(f.deletes) != 1 {
		t.Fatalf("不存在的 kill 不应产生 Delete，got %v", f.deletes)
	}
}

func TestRegistry_KillByFilter_Dimensions(t *testing.T) {
	scenarios := []struct {
		name         string
		user, tok, k int64
		want         int
		wantIDs      string
	}{
		{name: "by-user", user: 1, want: 2, wantIDs: "sA,sC"},
		{name: "by-token", tok: 10, want: 2, wantIDs: "sA,sB"},
		{name: "by-key", k: 100, want: 2, wantIDs: "sA,sB"},
		{name: "user-and-token", user: 1, tok: 10, want: 1, wantIDs: "sA"},
		{name: "no-filter", want: 0, wantIDs: ""},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			now := time.Now()
			r := NewSessionRegistry(func() time.Time { return now })
			f := &fakePersister{}
			r.SetStore(f)

			mustAttach(t, r, "sA", 1, 10, 100, now.Add(time.Hour))
			mustAttach(t, r, "sB", 2, 10, 100, now.Add(time.Hour))
			mustAttach(t, r, "sC", 1, 20, 200, now.Add(time.Hour))

			got := r.KillByFilter(sc.user, sc.tok, sc.k)
			if got != sc.want {
				t.Fatalf("KillByFilter(%d,%d,%d) 应删除 %d 条，got %d", sc.user, sc.tok, sc.k, sc.want, got)
			}
			if sc.want > 0 {
				if len(f.deleteMany) != 1 || sortedJoin(f.deleteMany[0]) != sc.wantIDs {
					t.Fatalf("批量删除应命中 %s，got %v", sc.wantIDs, f.deleteMany)
				}
			} else if len(f.deleteMany) != 0 {
				t.Fatalf("全零维度不应触发删除，got %v", f.deleteMany)
			}
			if len(r.ListAll()) != 3-sc.want {
				t.Fatalf("内存剩余会话数异常：%d", len(r.ListAll()))
			}
		})
	}
}

func TestRegistry_LoadPersisted_RestoresCounts(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	f := &fakePersister{loadRows: []*Session{
		{SessionID: "s1", UserID: 1, TokenID: 5, Model: "m", SessionRaw: "x",
			ChannelKeyID: 42, InternalModelID: "i", CreatedAt: now, LastActive: now, ExpireAt: now.Add(time.Hour)},
		{SessionID: "s2", UserID: 1, TokenID: 5, Model: "m", SessionRaw: "y",
			ChannelKeyID: 42, InternalModelID: "i", CreatedAt: now, LastActive: now, ExpireAt: now.Add(time.Hour)},
		{SessionID: "s3", UserID: 2, TokenID: 6, Model: "m", SessionRaw: "z",
			ChannelKeyID: 43, InternalModelID: "i", CreatedAt: now, LastActive: now, ExpireAt: now.Add(time.Hour)},
	}}
	r.SetStore(f)

	if err := r.LoadPersisted(context.Background()); err != nil {
		t.Fatalf("load persisted: %v", err)
	}

	if got := r.CountActive(42); got != 2 {
		t.Fatalf("密钥 42 恢复计数应为 2，got %d", got)
	}
	if got := r.CountActive(43); got != 1 {
		t.Fatalf("密钥 43 恢复计数应为 1，got %d", got)
	}
	if _, ok := r.Lookup("s1"); !ok {
		t.Fatal("恢复的 s1 应可查询")
	}
	if got := len(r.ListAll()); got != 3 {
		t.Fatalf("恢复会话总数应为 3，got %d", got)
	}

	// 恢复后的 max_concurrent 语义与持久化前一致：key42 已有 2 → 上限 2 拒绝，上限 3 放行。
	if ok, _ := r.Attach(&Session{
		SessionID: "news", UserID: 1, ChannelKeyID: 42, InternalModelID: "i",
		ExpireAt: now.Add(time.Hour),
	}, 2); ok {
		t.Fatal("恢复计数存在时，超过 max_concurrent=2 不应放行")
	}
	if ok, created := r.Attach(&Session{
		SessionID: "news", UserID: 1, ChannelKeyID: 42, InternalModelID: "i",
		ExpireAt: now.Add(time.Hour),
	}, 3); !ok || !created {
		t.Fatalf("max_concurrent=3 应放行新会话：ok=%v created=%v", ok, created)
	}
	if got := r.CountActive(42); got != 3 {
		t.Fatalf("attach 后密钥 42 计数应为 3，got %d", got)
	}
}

func TestRegistry_LoadPersisted_NoStoreReturnsNil(t *testing.T) {
	r := NewSessionRegistry(func() time.Time { return time.Now() })
	if err := r.LoadPersisted(context.Background()); err != nil {
		t.Fatalf("无 store 时 LoadPersisted 应返回 nil，got %v", err)
	}
}

func TestRegistry_StoreError_SwallowedAndLogged(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	f := &fakePersister{upsertErr: errors.New("boom")}
	var logged string
	r.SetLogger(func(format string, args ...any) { logged = fmt.Sprintf(format, args...) })
	r.SetStore(f)

	mustAttach(t, r, "s1", 1, 2, 42, now.Add(time.Hour))

	if !strings.Contains(logged, "boom") {
		t.Fatalf("Upsert 失败应记日志，got %q", logged)
	}
	if _, ok := r.Lookup("s1"); !ok {
		t.Fatal("DB 失败不应影响内存热路径")
	}
	if r.CountActive(42) != 1 {
		t.Fatalf("DB 失败不应影响计数，got %d", r.CountActive(42))
	}
}

func TestRegistry_CountActiveByChannel(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	// 注入 key→channel 映射（装配层经 manager 提供）。
	r.SetKeyChannelOf(func(keyID int64) int64 {
		byKey := map[int64]int64{11: 1, 12: 1, 21: 2}
		return byKey[keyID]
	})

	mustAttach(t, r, "s1", 1, 1, 11, now.Add(time.Hour))
	mustAttach(t, r, "s2", 1, 1, 12, now.Add(time.Hour))
	mustAttach(t, r, "s3", 1, 1, 21, now.Add(time.Hour))

	if got := r.CountActive(11); got != 1 {
		t.Fatalf("密钥 11 计数应为 1，got %d", got)
	}
	if got := r.CountActiveByChannel(1); got != 2 {
		t.Fatalf("渠道 1 聚合计数应为 2，got %d", got)
	}
	if got := r.CountActiveByChannel(2); got != 1 {
		t.Fatalf("渠道 2 聚合计数应为 1，got %d", got)
	}
}

// D3：KeyID() 回退已收口——ChannelKeyID 为会话唯一维度；渠道聚合只认 key→channel 映射反查结果。
func TestRegistry_CountActiveByChannel_ResolvedByKeyMapping(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	r.SetKeyChannelOf(func(keyID int64) int64 {
		return map[int64]int64{3: 3}[keyID]
	})

	if ok, created := r.Attach(&Session{
		SessionID: "s1", UserID: 1, ChannelKeyID: 3, InternalModelID: "i",
		ExpireAt: now.Add(time.Hour),
	}, 10); !ok || !created {
		t.Fatalf("attach failed：ok=%v created=%v", ok, created)
	}
	if got := r.CountActive(3); got != 1 {
		t.Fatalf("密钥 3 计数应为 1，got %d", got)
	}
	if got := r.CountActiveByChannel(3); got != 1 {
		t.Fatalf("按 key→channel 映射聚合计数应为 1，got %d", got)
	}
}

func TestRegistry_ListAll_SnapshotCopy(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	f := &fakePersister{}
	r.SetStore(f)

	mustAttach(t, r, "s1", 1, 2, 42, now.Add(time.Hour))

	list := r.ListAll()
	if len(list) != 1 {
		t.Fatalf("ListAll 应返回 1 条，got %d", len(list))
	}
	list[0].UserID = 999 // 修改快照不应影响注册表
	if s, _ := r.Lookup("s1"); s.UserID == 999 {
		t.Fatal("ListAll 应返回防御性副本")
	}
}
