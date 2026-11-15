package db

// 迁移链真库门禁（F4 / F7 / F10）——临时门禁用例，跑完即删，不进仓库。
//
// 覆盖三件事：
//  1. F4：空库**一次启动**跑完整链，且 0020 在 0024 之前（字典序 = 执行顺序）；
//     并证明迁移目录里不再有 0021。
//  2. F10：存量库里 cost_credits IS NULL 的行在 0020 之后被归一成 cost_source='none'，
//     且跨列 CHECK 真的拦得住「0 成本 + ledger 来源」。
//  3. F7：0024（唯一不可逆迁移）执行前后，15 张保留表行数不许减少。

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// gateDSN 取门禁库 DSN（未设置则跳过：CI 里没库时不许假绿）。
func gateDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过真库门禁")
	}
	return dsn
}

// newGateDB 建一个独立库（名字带前缀，便于失败后手工清理）。
func newGateDB(t *testing.T, name string) (*sql.DB, string) {
	t.Helper()
	dsn := gateDSN(t)
	admin, err := Open(dsn)
	if err != nil {
		t.Fatalf("连管理库失败: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
		t.Fatalf("清理旧库失败: %v", err)
	}
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Skipf("建库失败（账号无 CREATEDB？）: %v", err)
	}
	target, err := Open(replaceDBName(dsn, name))
	if err != nil {
		t.Fatalf("连目标库失败: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })
	return target, name
}

// replaceDBName 把 DSN 里的库名换成 name（只改 path 段，参数原样保留）。
func replaceDBName(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

// migrationFiles 按字典序返回迁移文件名（与 db.Migrate 的排序口径一致）。
func migrationFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("migrations"))
	if err != nil {
		t.Fatalf("读迁移目录失败: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// TestGate_F4_OneStartupFullChain 断言：一次 db.Migrate 跑完，顺序 = 字典序，0020 早于 0024，且无 0021。
func TestGate_F4_OneStartupFullChain(t *testing.T) {
	g, _ := newGateDB(t, "v2gate_f4")
	ctx := context.Background()

	applied, err := Migrate(ctx, g)
	if err != nil {
		t.Fatalf("一次启动跑完整链失败: %v", err)
	}
	files := migrationFiles(t)
	if applied != len(files) {
		t.Fatalf("应一次应用全部 %d 个迁移，got %d", len(files), applied)
	}

	var order []string
	rows, err := g.QueryContext(ctx, `SELECT name FROM applied_migrations ORDER BY applied_at, name`)
	if err != nil {
		t.Fatalf("读应用记录失败: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		order = append(order, n)
	}

	idx := func(prefix string) int {
		for i, n := range order {
			if strings.HasPrefix(n, prefix) {
				return i
			}
		}
		return -1
	}
	if idx("0020_") < 0 || idx("0024_") < 0 {
		t.Fatalf("0020/0024 都必须被执行：%v", order)
	}
	if idx("0020_") > idx("0024_") {
		t.Fatalf("0020 必须在 0024 之前（字典序=执行顺序）：0020=%d 0024=%d",
			idx("0020_"), idx("0024_"))
	}
	for _, n := range order {
		if strings.HasPrefix(n, "0021") {
			t.Fatalf("迁移目录里不该再有 0021（不可逆动作集中在 0024）: %s", n)
		}
	}
	// 0024 必须是**本批（< 0025）**的最后一个：右界之后是后续批（token_scope 顺延 0025），
	// 本批不为它做保证，也不许把 0025 塞进本批列表。
	last := ""
	for _, n := range order {
		if n >= V2MigrationRightBound {
			continue
		}
		if n > last {
			last = n
		}
	}
	if !strings.HasPrefix(last, "0024_") {
		t.Fatalf("0024 必须排最后，实际最后一个 = %s", last)
	}

	// F2：15 张保留表都在、10 张退役表都没了。
	for _, table := range RetainedTables() {
		var reg *string
		if err := g.QueryRowContext(ctx, `SELECT to_regclass('public.`+table+`')::text`).Scan(&reg); err != nil {
			t.Fatalf("查询 %s 失败: %v", table, err)
		}
		if reg == nil {
			t.Fatalf("保留表 %s 不存在", table)
		}
	}
	for _, table := range RetiredTables() {
		var reg *string
		if err := g.QueryRowContext(ctx, `SELECT to_regclass('public.`+table+`')::text`).Scan(&reg); err != nil {
			t.Fatalf("查询 %s 失败: %v", table, err)
		}
		if reg != nil {
			t.Fatalf("退役表 %s 仍存在", table)
		}
	}
	// 0024 是否已删掉账单的渠道密钥列（证明不可逆那批真的执行了）。
	var n int
	if err := g.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name='billing_records' AND column_name='channel_key_id'`).Scan(&n); err != nil {
		t.Fatalf("查列失败: %v", err)
	}
	if n != 0 {
		t.Fatal("0024 应已删除 billing_records.channel_key_id")
	}
}

// TestGate_F10_NotValidSkipsLegacyRowsButEnforcesNewWrites 断言 F10 补丁口径：
// 存量行不被扫描（NOT VALID），但新写入与任何 UPDATE 都被强约束。
func TestGate_F10_NotValidSkipsLegacyRowsButEnforcesNewWrites(t *testing.T) {
	g, _ := newGateDB(t, "v2gate_f10")
	ctx := context.Background()

	// 1) 只应用 0001–0019，模拟「上线前已有 15 万存量行」的库。
	for _, name := range migrationFiles(t) {
		if name >= "0020" {
			break
		}
		body, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		if _, err := g.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("应用 %s 失败: %v", name, err)
		}
	}
	if _, err := g.ExecContext(ctx,
		`INSERT INTO users(id, username, password_hash, role) VALUES (1,'u1','x','DEVELOPER')`); err != nil {
		t.Fatalf("插入用户失败: %v", err)
	}
	// 存量行：成本 NULL（0020 之前写的账单就是这样）。
	if _, err := g.ExecContext(ctx,
		`INSERT INTO billing_records(id, billing_id, user_id, pricing_mode, external_model_name,
		     internal_model_id, r_value, tokens, call_time, cost_credits)
		 VALUES (1,'legacy-1',1,'sale','m','im',1,'{"input":1,"output":1}'::jsonb, now(), NULL)`); err != nil {
		t.Fatalf("插入历史账单失败: %v", err)
	}

	// 2) 跑 0020（NOT VALID：不扫存量，所以这一步不该因为存量行失败）。
	body, err := os.ReadFile(filepath.Join("migrations", "0020_v2_billing_attribution.sql"))
	if err != nil {
		t.Fatalf("读 0020 失败: %v", err)
	}
	if _, err := g.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("应用 0020 失败（NOT VALID 应当跳过存量扫描）: %v", err)
	}

	// 3) 存量行原样保留（不被归一、也没被删）——这就是「不扫存量」的代价与证据。
	var legacy *string
	if err := g.QueryRowContext(ctx, `SELECT cost_source FROM billing_records WHERE id=1`).Scan(&legacy); err != nil {
		t.Fatalf("读存量行失败: %v", err)
	}
	if legacy != nil {
		t.Fatalf("NOT VALID 不该改写存量行，got cost_source=%v", *legacy)
	}

	// 4) 新写入：无成本却声称 ledger → 必须被拒。
	if _, err := g.ExecContext(ctx,
		`INSERT INTO billing_records(id, billing_id, user_id, pricing_mode, external_model_name,
		     internal_model_id, r_value, tokens, call_time, cost_credits, cost_source)
		 VALUES (2,'bad-1',1,'sale','m','im',1,'{"input":1,"output":1}'::jsonb, now(), NULL, 'ledger')`); err == nil {
		t.Fatal("新写入「无成本 + ledger」必须被 CHECK 拒掉")
	}
	if _, err := g.ExecContext(ctx,
		`INSERT INTO billing_records(id, billing_id, user_id, pricing_mode, external_model_name,
		     internal_model_id, r_value, tokens, call_time, cost_credits, cost_source)
		 VALUES (3,'ok-1',1,'sale','m','im',1,'{"input":1,"output":1}'::jsonb, now(), NULL, 'none')`); err != nil {
		t.Fatalf("无成本 + none 应放行: %v", err)
	}

	// 5) UPDATE 同样被强约束：只补 ledger_entry_id 不改成本来源 → 23514。
	if _, err := g.ExecContext(ctx,
		`UPDATE billing_records SET ledger_entry_id='led-1' WHERE id=1`); err == nil {
		t.Fatal("存量行 UPDATE 时也必须满足跨列约束（NOT VALID 只放过未改动的行）")
	}
	// 按 0020 文件里写明的正确写法（同时 COALESCE 成本来源）→ 通过。
	if _, err := g.ExecContext(ctx,
		`UPDATE billing_records SET ledger_entry_id='led-1', cost_source=COALESCE(cost_source,'none') WHERE id=1`); err != nil {
		t.Fatalf("同时给成本来源的回填应通过: %v", err)
	}
}

// TestGate_F10b_RepairMigrationFixesAlreadyAppliedDB 复现 Gate 的基线红：
// 老环境里 0020/0023 已按旧内容应用过（applied_migrations 有记录，改文件不会重放），
// 靠 0023b 把形状推到最终态：'skipped' 可写、计数列可空、跨列 CHECK 生效。
func TestGate_F10b_RepairMigrationFixesAlreadyAppliedDB(t *testing.T) {
	g, _ := newGateDB(t, "v2gate_f10b")
	ctx := context.Background()

	// 1) 应用到 0023（不含 0023b/0024），再把 reconcile_windows 退回**旧形状**。
	for _, name := range migrationFiles(t) {
		if name >= "0023b" {
			break
		}
		body, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		if _, err := g.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("应用 %s 失败: %v", name, err)
		}
	}
	oldShape := []string{
		`DROP TABLE reconcile_windows`,
		`CREATE TABLE reconcile_windows (
		    window_start TIMESTAMPTZ PRIMARY KEY,
		    window_end TIMESTAMPTZ NOT NULL,
		    local_count BIGINT NOT NULL DEFAULT 0,
		    upstream_count BIGINT NOT NULL DEFAULT 0,
		    local_tokens BIGINT NOT NULL DEFAULT 0,
		    upstream_tokens BIGINT NOT NULL DEFAULT 0,
		    status TEXT NOT NULL DEFAULT 'ok' CHECK (status IN ('ok','alarm','error','blocked')),
		    alarm_streak SMALLINT NOT NULL DEFAULT 0,
		    detail JSONB NOT NULL DEFAULT '{}'::jsonb,
		    checked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		    CONSTRAINT ck_reconcile_window_len CHECK (window_end = window_start + interval '60 seconds')
		)`,
		// 老版本的 blocked 行走「写 0 占位」。
		`INSERT INTO reconcile_windows(window_start, window_end, status)
		 VALUES (now()-interval '1 minute', now(), 'blocked')`,
		// 老版本没有 F10 约束。
		`ALTER TABLE billing_records DROP CONSTRAINT IF EXISTS ck_billing_cost_presence`,
	}
	for _, q := range oldShape {
		if _, err := g.ExecContext(ctx, q); err != nil {
			t.Fatalf("退回旧形状失败: %v\nSQL: %s", err, q)
		}
	}
	// 前置确认：旧形状确实写不进 'skipped'（这就是 Gate 看到的那条红）。
	if _, err := g.ExecContext(ctx,
		`INSERT INTO reconcile_windows(window_start, window_end, status)
		 VALUES (now()-interval '2 minute', now()-interval '1 minute', 'skipped')`); err == nil {
		t.Fatal("旧形状本应拒绝 skipped（前置条件不成立，用例失去意义）")
	}

	// 2) 应用修形迁移。
	body, err := os.ReadFile(filepath.Join("migrations", "0023b_v2_reconcile_repair.sql"))
	if err != nil {
		t.Fatalf("读 0023b 失败: %v", err)
	}
	if _, err := g.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("应用 0023b 失败: %v", err)
	}

	// 3) 'skipped' 可写。
	if _, err := g.ExecContext(ctx,
		`INSERT INTO reconcile_windows(window_start, window_end, status)
		 VALUES (now()-interval '3 minute', now()-interval '2 minute', 'skipped')`); err != nil {
		t.Fatalf("修形后 skipped 应可写: %v", err)
	}
	// 4) 计数列可空：不指定计数插入 → NULL。
	var lc *int64
	if err := g.QueryRowContext(ctx,
		`SELECT local_count FROM reconcile_windows WHERE status='skipped'`).Scan(&lc); err != nil {
		t.Fatalf("读计数失败: %v", err)
	}
	if lc != nil {
		t.Fatalf("未算过的窗口计数必须是 NULL（0 会与「无流量」混淆），got %d", *lc)
	}
	// 5) 老的 blocked 占位 0 被归 NULL。
	if err := g.QueryRowContext(ctx,
		`SELECT local_tokens FROM reconcile_windows WHERE status='blocked'`).Scan(&lc); err != nil {
		t.Fatalf("读 blocked 计数失败: %v", err)
	}
	if lc != nil {
		t.Fatalf("blocked 行的 0 占位应被归成 NULL，got %d", *lc)
	}
	// 6) 跨列 CHECK 也补上了。
	if _, err := g.ExecContext(ctx,
		`INSERT INTO users(id, username, password_hash, role) VALUES (1,'u1','x','DEVELOPER')`); err != nil {
		t.Fatalf("插入用户失败: %v", err)
	}
	if _, err := g.ExecContext(ctx,
		`INSERT INTO billing_records(id, billing_id, user_id, pricing_mode, external_model_name,
		     internal_model_id, r_value, tokens, call_time, cost_credits, cost_source)
		 VALUES (1,'bad',1,'sale','m','im',1,'{"input":1}'::jsonb, now(), NULL, 'ledger')`); err == nil {
		t.Fatal("修形后「无成本 + ledger」必须被拒")
	}
}

// TestGate_F7_RetainedRowCountsDoNotDecrease 断言 0024 前后 15 张保留表行数不减少。
func TestGate_F7_RetainedRowCountsDoNotDecrease(t *testing.T) {
	g, _ := newGateDB(t, "v2gate_f7")
	ctx := context.Background()

	// 1) 应用到 0023（0024 之前的全部）。
	files := migrationFiles(t)
	for _, name := range files {
		if name >= "0024" {
			break
		}
		body, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		if _, err := g.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("应用 %s 失败: %v", name, err)
		}
	}

	// 2) 种一批「必须活下来」的数据（用户/钱包/对外模型/账单/账本）。
	seed := []string{
		`INSERT INTO users(id, username, password_hash, role) VALUES (1,'u1','x','DEVELOPER')`,
		`INSERT INTO external_models(id, external_name) VALUES (1,'m1')`,
		`INSERT INTO billing_records(id, billing_id, user_id, pricing_mode, external_model_name,
		    internal_model_id, r_value, tokens, call_time, cost_source)
		 VALUES (1,'b1',1,'sale','m1','im',1,'{"input":1,"output":1}'::jsonb, now(), 'none')`,
		`INSERT INTO ledger_entries(id, ledger_id, provider, provider_account, model, tokens_in, tokens_out, cost_credits, occurred_at)
		 VALUES (1,'l1','p','a','m',1,1,1, now())`,
		`INSERT INTO reconcile_windows(window_start, window_end, status) VALUES (now() - interval '1 minute', now(), 'ok')`,
	}
	for _, q := range seed {
		if _, err := g.ExecContext(ctx, q); err != nil {
			t.Fatalf("种子失败: %v\nSQL: %s", err, q)
		}
	}

	counts := map[string]int64{}
	for _, table := range RetainedTables() {
		var c int64
		if err := g.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&c); err != nil {
			t.Fatalf("计数 %s 失败: %v", table, err)
		}
		counts[table] = c
	}

	// 3) 执行 0024（唯一不可逆迁移）。
	body, err := os.ReadFile(filepath.Join("migrations", "0024_v2_drop_legacy.sql"))
	if err != nil {
		t.Fatalf("读 0024 失败: %v", err)
	}
	if _, err := g.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("应用 0024 失败: %v", err)
	}

	// 4) 行数不许减少（F7 的验收口径：不等比例下降，只要求不减）。
	for _, table := range RetainedTables() {
		var c int64
		if err := g.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&c); err != nil {
			t.Fatalf("复算 %s 失败: %v", table, err)
		}
		if c < counts[table] {
			t.Fatalf("保留表 %s 行数减少：%d → %d", table, counts[table], c)
		}
	}

	// 5) 关键内容还在（账单/账本/对外模型各 1 行）。
	for table, want := range map[string]int64{"billing_records": 1, "ledger_entries": 1, "external_models": 1} {
		var c int64
		if err := g.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&c); err != nil {
			t.Fatalf("计数 %s 失败: %v", table, err)
		}
		if c != want {
			t.Fatalf("%s 行数应为 %d，got %d", table, want, c)
		}
	}
	fmt.Println("F7 保留表行数校验通过：", len(RetainedTables()), "张表")
}
