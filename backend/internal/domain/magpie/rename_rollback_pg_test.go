// rename_rollback_pg_test.go 真库证据链（任务书要求：迁移执行 + 回滚演练记录）：
//
//  1. 全链迁移在临时库真跑两次（第二次幂等：0030 是 ADD COLUMN/INDEX IF NOT EXISTS 形状）；
//  2. 真库 RenameAndActivate（改名三步的本地段）→ key_name=新名、legacy=旧名；
//  3. RollbackToken 回滚演练：代理侧（fake 页面）旧名复原 + 本地行 key_name=旧名、legacy 清空；
//  4. settleOne 的两段反查 SQL 在真库上验证（新名命中 / 旧名经 legacy 兜底命中 / 未知名零命中）。
//
// 未设置 LLMGATEWAY_TEST_PG_DSN 时 t.Skip（CI 与本地门禁均不阻塞）。
package magpie

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/db"
)

// openRenameTempDB 建一次性库并跑全量迁移（含 0030）两遍（幂等证据）。
func openRenameTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过真库回滚演练")
	}
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败：%v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	name := fmt.Sprintf("magpie_rename_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Skipf("无法创建临时库（账号是否没有 CREATEDB 权限？）：%v", err)
	}
	t.Cleanup(func() {
		if a, derr := db.Open(dsn); derr == nil {
			_, _ = a.ExecContext(context.Background(), "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
			_, _ = a.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name)
			_ = a.Close()
		}
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("解析 DSN 失败：%v", err)
	}
	u.Path = "/" + name
	target, err := db.Open(u.String())
	if err != nil {
		t.Fatalf("打开临时库失败：%v", err)
	}
	t.Cleanup(func() { _ = target.Close() })

	if _, err := db.Migrate(context.Background(), target); err != nil {
		t.Fatalf("全链迁移执行失败：%v", err)
	}
	// 幂等重跑：同一库再 Migrate 一次（0030 的 IF NOT EXISTS 形状必须零报错）。
	if _, err := db.Migrate(context.Background(), target); err != nil {
		t.Fatalf("幂等重跑失败：%v", err)
	}
	return target
}

// TestPG_MigrationAndRollbackDrill 真库回滚演练（证据用例，全链迁移含 0030）。
func TestPG_MigrationAndRollbackDrill(t *testing.T) {
	target := openRenameTempDB(t)
	ctx := context.Background()
	store := NewKeyStore(target)
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := target.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed exec：%v（%s）", err, q)
		}
	}
	must(`INSERT INTO users (id, username, password_hash, role) VALUES (12, 'zhangsan-pg', 'x', 'DEVELOPER')
	      ON CONFLICT DO NOTHING`)
	must(`INSERT INTO tokens (id, user_id, token_hash, token_display, display_name, status)
	      VALUES (7, 12, 'hash-pg-7', 'disp-pg-7', '办公室台式机', 'ACTIVE')`)

	cipher, err := encryptForTest("secret-pg")
	if err != nil {
		t.Fatalf("sm4：%v", err)
	}
	if _, err := store.Provision(ctx, 7, "ls-token-7", cipher); err != nil {
		t.Fatalf("provision：%v", err)
	}
	// 改名三步的本地段：新名 + legacy=旧名。
	if _, err := store.RenameAndActivate(ctx, 7, "ls-token-7", "zhangsan-t7", cipher); err != nil {
		t.Fatalf("rename：%v", err)
	}
	got, err := store.ByTokenID(ctx, 7)
	if err != nil || got.KeyName != "zhangsan-t7" || got.LegacyKeyName != "ls-token-7" {
		t.Fatalf("改名后行不符：%+v err=%v", got, err)
	}

	// 代理侧 fake 页面预置新名（真机同形）。
	srv, pageKeys := fakeMagpiePage(t, syncTestPageKey)
	if _, err := NewPageAdmin(srv.URL, syncTestPageKey, nil).EnsureCallerKey(ctx, "zhangsan-t7", "secret-pg"); err != nil {
		t.Fatalf("page ensure：%v", err)
	}
	sync := NewCallerKeySync(NewPageAdmin(srv.URL, syncTestPageKey, nil), store, testCredKey)

	// 回滚演练：旧名复原（值不变）、legacy 清空、代理侧旧名可用。
	done, err := sync.RollbackToken(ctx, 7)
	if err != nil || !done {
		t.Fatalf("回滚演练失败：done=%v err=%v", done, err)
	}
	got, _ = store.ByTokenID(ctx, 7)
	if got.KeyName != "ls-token-7" || got.LegacyKeyName != "" || got.State != KeyStateActive {
		t.Fatalf("回滚后行不符：%+v", got)
	}
	if v, ok := pageKeys.Load("ls-token-7"); !ok || v != "secret-pg" {
		t.Fatalf("代理侧旧名应复原：ok=%v v=%v", ok, v)
	}

	// settleOne 两段反查在真库上的口径（与 v3billing/poller.go 的两条 SQL 逐字同形）。
	var tid, uid int64
	// (a) 新名直接命中。
	if err := target.QueryRowContext(ctx, `SELECT k.token_id, t.user_id FROM magpie_keys k
		JOIN tokens t ON t.id = k.token_id WHERE k.key_name = $1 AND k.state = 'ACTIVE' LIMIT 1`,
		"ls-token-7").Scan(&tid, &uid); err != nil || tid != 7 || uid != 12 {
		t.Fatalf("新名反查不符：tid=%d uid=%d err=%v", tid, uid, err)
	}
	// (b) 改名窗口内旧名经 legacy 兜底命中（把行恢复成「已改名」形态再验）。
	if _, err := store.RenameAndActivate(ctx, 7, "ls-token-7", "zhangsan-t7", cipher); err != nil {
		t.Fatalf("二次改名：%v", err)
	}
	if err := target.QueryRowContext(ctx, `SELECT k.token_id, t.user_id FROM magpie_keys k
		JOIN tokens t ON t.id = k.token_id WHERE k.legacy_key_name = $1 AND k.state = 'ACTIVE' LIMIT 1`,
		"ls-token-7").Scan(&tid, &uid); err != nil || tid != 7 || uid != 12 {
		t.Fatalf("旧名兜底反查不符：tid=%d uid=%d err=%v", tid, uid, err)
	}
	// (c) 未知名两段皆空：零命中（settleOne 侧的报错与 Warn 由单测锁定）。
	var cnt int
	if err := target.QueryRowContext(ctx, `SELECT count(*) FROM magpie_keys k
		WHERE (k.key_name = $1 AND k.state='ACTIVE') OR (k.legacy_key_name = $1 AND k.state='ACTIVE')`,
		"ls-ghost").Scan(&cnt); err != nil || cnt != 0 {
		t.Fatalf("未知名应零命中：cnt=%d err=%v", cnt, err)
	}

	// (d) 评审追加 P1 的缝（真库行为锚）：停用行占名 → NameOccupied 必须判占用。
	// 0019b 的 key_name 是全列 UNIQUE、不排 DISABLED；查询若排除停用行，
	// 新令牌 Provision 会直接撞 23505（Create 回滚删令牌）。
	cipher2, err := encryptForTest("secret-pg2")
	if err != nil {
		t.Fatalf("sm4：%v", err)
	}
	if _, err := target.ExecContext(ctx, `INSERT INTO tokens (id, user_id, token_hash, token_display, display_name, status)
		VALUES (8, 12, 'hash-pg-8', 'disp-pg-8', '机', 'ACTIVE')`); err != nil {
		t.Fatalf("seed token 8：%v", err)
	}
	if _, err := store.Provision(ctx, 8, "zhangsan-pg", cipher2); err != nil {
		t.Fatalf("provision 8：%v", err)
	}
	if err := store.Disable(ctx, 8); err != nil {
		t.Fatalf("disable 8：%v", err)
	}
	occupied, err := store.NameOccupied(ctx, "zhangsan-pg", 999)
	if err != nil || !occupied {
		t.Fatalf("停用行占名必须判占用（否则 Provision 撞 23505）：occupied=%v err=%v", occupied, err)
	}
}

// TestPG_NewTokenSkipsDisabledOccupiedName 评审追加 P1 的真库行为锚（建令牌路径）：
// 停用行占住基础名时，新令牌的 callerKey 分配必须靠消歧 -2 逃逸——
// 修复前 NameOccupied 排除停用行 → 判「空闲」→ Provision INSERT 直接撞
// key_name 全列 UNIQUE（23505）→ CreateToken 回滚删令牌；修复后检查期即占用。
func TestPG_NewTokenSkipsDisabledOccupiedName(t *testing.T) {
	target := openRenameTempDB(t)
	ctx := context.Background()
	store := NewKeyStore(target)
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := target.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed exec：%v（%s）", err, q)
		}
	}
	must(`INSERT INTO users (id, username, password_hash, role) VALUES (12, 'lisi-pg', 'x', 'DEVELOPER')
	      ON CONFLICT DO NOTHING`)
	must(`INSERT INTO tokens (id, user_id, token_hash, token_display, display_name, status)
	      VALUES (9, 12, 'hash-pg-9', 'disp-pg-9', '旧令牌', 'ACTIVE')`)
	must(`INSERT INTO tokens (id, user_id, token_hash, token_display, display_name, status)
	      VALUES (31, 12, 'hash-pg-31', 'disp-pg-31', '新令牌', 'ACTIVE')`)

	base := KeyNameFor(12, 31, "lisi", "新令牌")
	cipher9, err := encryptForTest("secret-old")
	if err != nil {
		t.Fatalf("sm4：%v", err)
	}
	if _, err := store.Provision(ctx, 9, base, cipher9); err != nil {
		t.Fatalf("provision 9：%v", err)
	}
	if err := store.Disable(ctx, 9); err != nil {
		t.Fatalf("disable 9：%v", err)
	}

	srv, pageKeys := fakeMagpiePage(t, syncTestPageKey)
	sync := NewCallerKeySync(NewPageAdmin(srv.URL, syncTestPageKey, nil), store, testCredKey)
	if err := sync.Ensure(ctx, 12, TokenRef{ID: 31, DisplayName: "新令牌"}, "lisi", "secret-new"); err != nil {
		t.Fatalf("停用占名下新建必须消歧成功（修复前此处 23505 回滚）：%v", err)
	}
	got, err := store.ByTokenID(ctx, 31)
	if err != nil || got == nil || got.KeyName != base+"-2" || got.State != KeyStateActive {
		t.Fatalf("新映射应落 -2 尾号：got=%+v base=%s err=%v", got, base, err)
	}
	if v, ok := pageKeys.Load(base + "-2"); !ok || v != "secret-new" {
		t.Fatalf("代理侧 -2 名不符：ok=%v v=%v", ok, v)
	}
	occ, err := store.NameOccupied(ctx, base, 999)
	if err != nil || !occ {
		t.Fatalf("停用行占名必须仍判占用：occ=%v err=%v", occ, err)
	}
}
