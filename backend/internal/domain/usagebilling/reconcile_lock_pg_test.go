package usagebilling

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// openPGForLockTest 打开真 PG（未配 DSN 则跳过）。
//
// 为什么必须用真库：这个 bug 只在连接池的「取锁/解锁落在不同连接」时才出现，
// sqlmock 里每次调用都是同一条假连接，永远看不出来（Owl 就是在真 PG 上抓到的）。
func openPGForLockTest(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过真库锁用例")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	// 连接池开大一点：bug 的前提就是「解锁可能落到另一条连接」。
	db.SetMaxOpenConns(8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("真库不可用，跳过: %v", err)
	}
	return db
}

// advisoryLockCount 数当前库里有几条该键的 advisory lock（跨会话可见）。
func advisoryLockCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	// bigint 形式的 advisory lock 在 pg_locks 里被拆成 classid(高 32 位)/objid(低 32 位)/objsubid=1。
	err := db.QueryRow(`SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND classid = $1 AND objid = $2 AND objsubid = 1`,
		int64(reconcileLockKey>>32), int64(reconcileLockKey&0xffffffff)).Scan(&n)
	if err != nil {
		t.Fatalf("查 pg_locks: %v", err)
	}
	return n
}

// 真 PG：取锁 → 另一副本拿不到 → 释放后锁真的消失（旧实现解锁落到池里另一条连接，
// pg_advisory_unlock 返 false 且锁留在原会话上，后续窗口恒拿不到 → 对账静默停摆）。
func TestAdvisoryLock_RealPG_ReleaseActuallyUnlocks(t *testing.T) {
	db := openPGForLockTest(t)
	defer db.Close()

	// 清掉历史遗留的锁（上一次跑失败的会话已断开，这里只是防止残留）。
	if _, err := db.Exec(`SELECT pg_advisory_unlock_all()`); err != nil {
		t.Fatalf("清场解锁: %v", err)
	}
	if n := advisoryLockCount(t, db); n != 0 {
		t.Fatalf("清场后不应有锁: %d", n)
	}

	// 先验证 bug 前提：在池上直接 unlock 一条并不持有的会话锁 → 返回 false（这就是旧代码的样子）。
	var unlockedElsewhere bool
	if err := db.QueryRow(`SELECT pg_advisory_unlock($1)`, reconcileLockKey).Scan(&unlockedElsewhere); err != nil {
		t.Fatalf("解锁探针: %v", err)
	}
	if unlockedElsewhere {
		t.Fatal("未持锁时 pg_advisory_unlock 应为 false（本用例前提变了）")
	}

	storeA := NewSQLReconcileStore(db)
	storeB := NewSQLReconcileStore(db)

	ctx := context.Background()
	// 关键构造（对应 database/sql 的 FIFO 空闲队列）：先在池里放一条待归还的连接 P，
	// 再取锁（旧实现把取锁连接 Q 放回池里），再点一条连接 R 并归还，得到空闲队列 [P, R]。
	// 旧实现在池上解锁 → 取到 P（≠ 持锁会话 Q）→ pg_advisory_unlock 返 false、
	// 锁留在 Q 上 → 残留 1 条，后续窗口永久拿不到（对账静默停摆）。
	pin1, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("占用连接 P: %v", err)
	}
	release, ok := storeA.TryAdvisoryLock(ctx)
	if !ok {
		t.Fatal("A 应拿到锁")
	}
	// 关键构造：取锁后再点两条连接并归还（模拟「池里恰好有别的空闲连接」这个真实条件）。
	// 旧实现把取锁连接放回池里，解锁时会取到这两条之一 → pg_advisory_unlock 返 false
	// 且锁留在原会话上 → 残留 1 条、后续窗口永久拿不到（对账静默停摆）。
	pin2, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("占用连接 R: %v", err)
	}
	_ = pin1.Close()
	_ = pin2.Close()
	if n := advisoryLockCount(t, db); n != 1 {
		t.Fatalf("持锁期间库里应有 1 条 advisory lock: %d", n)
	}
	// 另一副本（同池不同连接）拿不到。
	if _, ok := storeB.TryAdvisoryLock(context.Background()); ok {
		t.Fatal("A 持锁时 B 不许拿到锁")
	}
	if n := advisoryLockCount(t, db); n != 1 {
		t.Fatalf("B 尝试后锁数量不应变化: %d", n)
	}

	release()
	if n := advisoryLockCount(t, db); n != 0 {
		t.Fatalf("释放后锁必须真的消失（否则下一窗永久拿不到）: %d 条残留", n)
	}
	// 释放后重新可取：连续 5 轮，验证锁不会泄漏在某个连接上。
	for i := 0; i < 5; i++ {
		rel, ok := storeB.TryAdvisoryLock(context.Background())
		if !ok {
			t.Fatalf("第 %d 轮没能重新取锁（锁泄漏）", i+1)
		}
		rel()
		if n := advisoryLockCount(t, db); n != 0 {
			t.Fatalf("第 %d 轮释放后残留 %d 条", i+1, n)
		}
	}
}
