package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// 迁移集成测试（可选执行）：验证 0011 的三段归一化与 0013 的 probe 口径修正。
//
// 运行方式（本地 dev PG）：
//
//	cd backend
//	LLMGATEWAY_TEST_PG_DSN='postgres://user:pass@host:port/db' \
//	  go test ./internal/db -run TestMigrate_ThreeSegments -v
//
// 未设置 LLMGATEWAY_TEST_PG_DSN 时该测试 t.Skip，不阻塞普通 go test ./...。
// 测试会在临时库内先应用 0001~0010 建表（直接读 migrations 目录文件，不用 Migrate()，
// 以便精确控制只应用 <0011 的历史迁移），再插入样例数据、执行 0011/0013 并断言结果；
// 结束时无条件删除临时库。
func TestMigrate_ThreeSegments(t *testing.T) {
	tempDB := openTempDB(t)

	migDir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
	applyMigrationsBelow(t, tempDB, migDir, "0011")

	// ---------- 1) 插入 billing_records 样例并执行 0011 ----------
	// 建基础外键依赖行：user -> channel -> channel_key。
	exec(t, tempDB, `INSERT INTO users(id, username, password_hash, role) VALUES (1, 't', 'x', 'ADMIN')`)
	exec(t, tempDB, `INSERT INTO channels(id, name, base_url) VALUES (1, 'c', 'http://x')`)
	exec(t, tempDB, `INSERT INTO channel_keys(id, channel_id, name, credential_enc) VALUES (1, 1, 'k', 'enc')`)

	type sample struct {
		id                            int64
		tokens                        string
		wantInput, wantCache, wantOut int64
	}
	samples := []sample{
		// 输入含缓存读：input 是「含」口径（prompt 含 cached），cache_write 并入 input，reasoning 并入 output。
		{1, `{"input":100,"cache_read":30,"output":50,"cache_write":10,"reasoning":5}`, 80, 30, 55},
		// cache_read > input：来自「输入不含缓存读」的上游，input 不再二次相减，cache_write 直接并入。
		{2, `{"input":20,"cache_read":30,"output":40,"cache_write":0,"reasoning":0}`, 20, 30, 40},
		// 带 cache_write、无 reasoning：正常并入 input。
		{3, `{"input":10,"cache_read":0,"output":5,"cache_write":7}`, 17, 0, 5},
		// 非数字键值（"1.0"/"abc"）：正则守卫按 0 处理，绝不抛错中断迁移。
		{4, `{"input":"1.0","cache_read":5,"output":"abc","cache_write":3,"reasoning":2}`, 3, 5, 2},
	}
	for i, s := range samples {
		exec(t, tempDB,
			`INSERT INTO billing_records(id, billing_id, user_id, pricing_mode, external_model_name, internal_model_id, r_value, tokens)
			 VALUES ($1, $2, 1, 'sale', 'm', 'im', 1, $3::jsonb)`,
			s.id, fmt.Sprintf("b-%d", i), s.tokens)
	}

	runMigration(t, tempDB, migDir, migrationFile(t, migDir, "0011_unify"))

	for _, s := range samples {
		var raw []byte
		if err := tempDB.QueryRow(`SELECT tokens FROM billing_records WHERE id=$1`, s.id).Scan(&raw); err != nil {
			t.Fatalf("读取 id=%d tokens 失败: %v", s.id, err)
		}
		var m map[string]int64
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("id=%d tokens 非数字对象: %s (%v)", s.id, raw, err)
		}
		if len(m) != 3 {
			t.Fatalf("id=%d 归一化后应只剩 3 个键, got %d: %s", s.id, len(m), raw)
		}
		if _, ok := m["reasoning"]; ok {
			t.Fatalf("id=%d 归一化后不应含 reasoning: %s", s.id, raw)
		}
		if _, ok := m["cache_write"]; ok {
			t.Fatalf("id=%d 归一化后不应含 cache_write: %s", s.id, raw)
		}
		if m["input"] != s.wantInput || m["cache_read"] != s.wantCache || m["output"] != s.wantOut {
			t.Fatalf("id=%d 归一化: got input=%d cache_read=%d output=%d, want %d/%d/%d (raw=%s)",
				s.id, m["input"], m["cache_read"], m["output"], s.wantInput, s.wantCache, s.wantOut, raw)
		}
	}

	// ---------- 2) 插入旧口径 probe_logs 行并执行 0013 ----------
	exec(t, tempDB,
		`INSERT INTO probe_logs(id, channel_key_id, target, ok, input_tokens, output_tokens, cached_tokens, total_tokens)
		 VALUES (1, 1, 'http://x', true, 100, 50, 30, 150)`)
	runMigration(t, tempDB, migDir, migrationFile(t, migDir, "0013_probe"))

	var inTok, outTok, cachedTok, totalTok int
	if err := tempDB.QueryRow(
		`SELECT input_tokens, output_tokens, cached_tokens, total_tokens FROM probe_logs WHERE id=1`).
		Scan(&inTok, &outTok, &cachedTok, &totalTok); err != nil {
		t.Fatalf("读取 probe_logs 失败: %v", err)
	}
	if inTok != 70 || cachedTok != 30 || outTok != 50 || totalTok != 150 {
		t.Fatalf("probe_logs 归一化: got input=%d cached=%d output=%d total=%d, want 70/30/50/150",
			inTok, cachedTok, outTok, totalTok)
	}
}

// TestMigrate_FullChain 验证「全新库」按文件名升序应用全部迁移可跑通，且合并后并存的两个
// 0011_* 迁移（0011_auto_models 与 0011_unify_usage_three_segments）都能落地：
// call_logs 同时具备 auto_model_name 与 upstream_usage 列，sale_rates 默认值已是三段。
func TestMigrate_FullChain(t *testing.T) {
	tempDB := openTempDB(t)
	migDir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")

	names := migrationNames(t, migDir)
	for _, n := range names {
		runMigration(t, tempDB, migDir, n)
	}
	t.Logf("已按序应用 %d 个迁移", len(names))

	// 两个 0011_* 并存是刻意保留：改号会让已执行过 0013 的库把减法迁移重跑一次。
	var dup11 int
	for _, n := range names {
		if strings.HasPrefix(n, "0011") {
			dup11++
		}
	}
	if dup11 < 2 {
		t.Fatalf("期望合并后并存 2 个 0011_* 迁移, got %d", dup11)
	}

	cols := map[string]bool{}
	rows, err := tempDB.Query(`SELECT column_name FROM information_schema.columns WHERE table_name='call_logs'`)
	if err != nil {
		t.Fatalf("查询 call_logs 列失败: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("扫描列名失败: %v", err)
		}
		cols[c] = true
	}
	for _, want := range []string{"auto_model_name", "upstream_usage"} {
		if !cols[want] {
			t.Fatalf("call_logs 缺少列 %s（合并后迁移链未跑通）", want)
		}
	}

	var def string
	if err := tempDB.QueryRow(
		`SELECT column_default FROM information_schema.columns WHERE table_name='external_models' AND column_name='sale_rates'`).Scan(&def); err != nil {
		t.Fatalf("读取 sale_rates 默认值失败: %v", err)
	}
	if strings.Contains(def, "cache_write") || strings.Contains(def, "reasoning") {
		t.Fatalf("sale_rates 默认值仍含废弃键: %s", def)
	}
}

// openTempDB 建一次性库并在测试结束（含失败）时删除；未设置 LLMGATEWAY_TEST_PG_DSN 时 skip。
func openTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过迁移集成测试")
	}
	adminDB, err := Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败: %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })
	if err := adminDB.Ping(); err != nil {
		t.Fatalf("管理连接 ping 失败（DSN 是否可达?）: %v", err)
	}

	name := fmt.Sprintf("llmgateway-test-%d", time.Now().UnixNano())
	if _, err := adminDB.Exec(`CREATE DATABASE ` + quoteIdent(name)); err != nil {
		t.Fatalf("创建临时库 %s 失败: %v", name, err)
	}
	// 注册顺序 = 执行逆序：先注册 DROP，再注册连接关闭，保证 DROP 前已断开临时库连接。
	t.Cleanup(func() {
		_, _ = adminDB.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, name)
		if _, derr := adminDB.Exec(`DROP DATABASE IF EXISTS ` + quoteIdent(name)); derr != nil {
			t.Logf("删除临时库 %s 失败: %v", name, derr)
		} else {
			t.Logf("已删除临时库 %s", name)
		}
	})
	tempDSN, err := withDatabase(dsn, name)
	if err != nil {
		t.Fatalf("构造临时库 DSN 失败: %v", err)
	}
	tempDB, err := Open(tempDSN)
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = tempDB.Close() })
	if err := tempDB.Ping(); err != nil {
		t.Fatalf("临时库 ping 失败: %v", err)
	}
	t.Logf("已创建临时库 %s", name)
	return tempDB
}

// applyMigrationsBelow 按文件名升序应用 <limit 的历史迁移（建表用）。
func applyMigrationsBelow(t *testing.T, tempDB *sql.DB, dir, limit string) {
	t.Helper()
	for _, name := range migrationNames(t, dir) {
		if name >= limit {
			continue
		}
		runMigration(t, tempDB, dir, name)
	}
}

// runMigration 读取并执行单个迁移文件（整段 SQL 一次 Exec，与 db.Migrate 行为一致）。
func runMigration(t *testing.T, tempDB *sql.DB, dir, name string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("读取迁移 %s 失败: %v", name, err)
	}
	if _, err := tempDB.Exec(string(body)); err != nil {
		t.Fatalf("执行迁移 %s 失败: %v", name, err)
	}
}

// migrationNames 返回迁移目录下 .sql 文件名（升序）。
func migrationNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取迁移目录 %s 失败: %v", dir, err)
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

// migrationFile 按数字前缀（如 "0011"）返回完整迁移文件名。
func migrationFile(t *testing.T, dir, prefix string) string {
	t.Helper()
	for _, n := range migrationNames(t, dir) {
		if strings.HasPrefix(n, prefix) {
			return n
		}
	}
	t.Fatalf("未找到以 %s 开头的迁移文件", prefix)
	return ""
}

// exec 执行语句，失败即 Fatal。
func exec(t *testing.T, tempDB *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := tempDB.Exec(query, args...); err != nil {
		t.Fatalf("执行 SQL 失败: %v\nSQL: %s", err, query)
	}
}

// withDatabase 用新的库名替换 DSN 中的数据库路径。
func withDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

// quoteIdent 以双引号包裹标识符（转义内部双引号）。
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// mustCallerFile 返回当前测试文件绝对路径。
func mustCallerFile(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(1)
	if !ok {
		t.Fatalf("无法定位测试文件路径")
	}
	return file
}

// TestMigrate_0019b_PassthroughCredentials 验证 #81 拆包后的最小对象集（0019b）：
//  1. **纯新增**：正文不含 DROP / RENAME / DELETE / UPDATE / TRUNCATE / ALTER COLUMN；
//  2. 全链应用后：magpie_keys 在、external_models.magpie_model 在、唯一索引在 —— 且
//     **104 号之后的 0024 跑完它们仍然在**（这是 #81 走方案 A 的核心断言：
//     这两样是被代码使用的对象，不是「旧分支遗留、期望 0 行」）；
//  3. reconcile_windows 由 0023 持有：形状必须是 0023 的（PK=window_start + 四计数列），
//     不是 0019b 建的（本文件不许出现这张表）；
//  4. billing_records 三列**不由本文件处理**（正文不出现 magpie_key_name / estimated / upstream_usage）。
//     口径（2026-11-16 裁定）：本期**保留列、禁止写入** —— 历史库上这三列可能存在，那**不是缺陷**，
//     「有没有写入方」由 TestBilling_LegacyColumnsHaveNoWriter 守；这里只断言 0019b 自己不去动它们；
//  5. 可重复执行，重跑不报错、不改变结果。
func TestMigrate_0019b_PassthroughCredentials(t *testing.T) {
	tempDB := openTempDB(t)
	migDir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
	name := migrationFile(t, migDir, "0019b")

	// 1) 源码级守卫：去掉注释后不得出现破坏性关键字。
	body, err := os.ReadFile(filepath.Join(migDir, name))
	if err != nil {
		t.Fatalf("读取迁移 %s 失败: %v", name, err)
	}
	var code strings.Builder
	for _, line := range strings.Split(string(body), "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	upper := strings.ToUpper(code.String())
	for _, forbidden := range []string{"DROP ", "RENAME", "DELETE ", "UPDATE ", "TRUNCATE", "ALTER COLUMN"} {
		if strings.Contains(upper, forbidden) {
			t.Fatalf("0019b 迁移禁止出现破坏性语句 %q（本期口径：纯 ADD、零 DML）", forbidden)
		}
	}
	if strings.Contains(upper, "RECONCILE_WINDOWS") {
		t.Fatalf("0019b 不许出现 reconcile_windows：同名对象只允许 0023 持有（两份 CREATE 只有第一份生效）")
	}

	// 2) 全链应用（含 0024 删退役体系）。
	for _, n := range migrationNames(t, migDir) {
		runMigration(t, tempDB, migDir, n)
	}

	assertColumn := func(table, column string, want int) {
		t.Helper()
		var got int
		if err := tempDB.QueryRow(
			`SELECT count(*) FROM information_schema.columns WHERE table_name=$1 AND column_name=$2`,
			table, column).Scan(&got); err != nil {
			t.Fatalf("查询列 %s.%s 失败: %v", table, column, err)
		}
		if got != want {
			t.Fatalf("%s.%s 存在性 = %d，期望 %d", table, column, got, want)
		}
	}
	assertTable := func(table string) {
		t.Helper()
		var got int
		if err := tempDB.QueryRow(
			`SELECT count(*) FROM information_schema.tables WHERE table_name=$1`, table).Scan(&got); err != nil {
			t.Fatalf("查询表 %s 失败: %v", table, err)
		}
		if got != 1 {
			t.Fatalf("缺少表 %s（0019b 未落地，或 0024 把它当遗留对象删了）", table)
		}
	}

	assertTable("magpie_keys")
	assertColumn("external_models", "magpie_model", 1)

	// 3) reconcile_windows 归 0023：形状必须是 0023 的那一份。
	assertTable("reconcile_windows")
	assertColumn("reconcile_windows", "local_count", 1)
	assertColumn("reconcile_windows", "scroll_offset", 1)
	var pk int
	if err := tempDB.QueryRow(`
		SELECT count(*) FROM pg_constraint c
		 JOIN pg_class t ON t.oid = c.conrelid
		WHERE t.relname = 'reconcile_windows' AND c.contype = 'p'
		  AND pg_get_constraintdef(c.oid) LIKE '%(window_start)%'`).Scan(&pk); err != nil {
		t.Fatalf("查询主键失败: %v", err)
	}
	if pk != 1 {
		t.Fatalf("reconcile_windows 主键不是 (window_start)：0019_v2_facade 的旧形状又回来了？")
	}

	// 4) 账单三列：0019b 正文不许出现它们。
	//    口径（2026-11-16 裁定）：本期保留列、禁止写入 —— 库上「列存在」不再是缺陷（旧分支可能已建，
	//    阶段二按新形处理），所以这里不再断言「全链跑完必须缺列」，只断言本文件不去动 billing_records。
	// 只看**非注释**正文：文件头部的注释会解释「这三列本期不用」，那是说明而不是改动。
	var executable []string
	for _, line := range strings.Split(string(body), "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "--") {
			continue
		}
		executable = append(executable, line)
	}
	stmtBody := strings.Join(executable, "\n")
	for _, col := range []string{"magpie_key_name", "estimated", "upstream_usage"} {
		if strings.Contains(stmtBody, col) {
			t.Fatalf("0019b 必须对 billing_records 零改动，可执行正文却出现列名 %s", col)
		}
	}

	var idx int
	if err := tempDB.QueryRow(
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'uq_external_models_magpie_model'`).Scan(&idx); err != nil {
		t.Fatalf("查询索引失败: %v", err)
	}
	if idx != 1 {
		t.Fatalf("0019b 的唯一索引 uq_external_models_magpie_model 缺失, got %d", idx)
	}

	// 5) 幂等：重跑同一迁移不报错（全部 IF NOT EXISTS）。
	runMigration(t, tempDB, migDir, name)
}

// TestMigrate_0025_BillingUpstreamError 断言 0025 的三件事：
//  1. 状态值域放开 `upstream_error`，且旧的 0001 内联 CHECK 已被替换（不是两份约束并存）；
//  2. 上游状态码/错误体摘要两列存在；
//  3. 迁移可重复执行（DROP IF EXISTS + ADD + ADD COLUMN IF NOT EXISTS）。
func TestMigrate_0025_BillingUpstreamError(t *testing.T) {
	tempDB := openTempDB(t)
	migDir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
	for _, n := range migrationNames(t, migDir) {
		runMigration(t, tempDB, migDir, n)
	}

	var status string
	if err := tempDB.QueryRow(
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='ck_billing_status'`).Scan(&status); err != nil {
		t.Fatalf("缺少 ck_billing_status 约束: %v", err)
	}
	if !strings.Contains(status, "upstream_error") {
		t.Fatalf("ck_billing_status 未放开 upstream_error: %s", status)
	}
	var oldCheck int
	if err := tempDB.QueryRow(
		`SELECT count(*) FROM pg_constraint WHERE conname='billing_records_status_check'`).Scan(&oldCheck); err != nil {
		t.Fatalf("查询旧约束失败: %v", err)
	}
	if oldCheck != 0 {
		t.Fatalf("0001 的 billing_records_status_check 仍存在，会挡住 upstream_error")
	}

	var cols int
	if err := tempDB.QueryRow(
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name='billing_records' AND column_name IN ('upstream_status','upstream_error_summary')`).Scan(&cols); err != nil {
		t.Fatalf("查询列失败: %v", err)
	}
	if cols != 2 {
		t.Fatalf("upstream_status/upstream_error_summary 应各有 1 列，实际 %d", cols)
	}

	if _, err := tempDB.Exec(
		`INSERT INTO users(id, username, password_hash, role) VALUES (1,'u','h','DEVELOPER') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("插入用户失败: %v", err)
	}
	if _, err := tempDB.Exec(
		`INSERT INTO billing_records(id, billing_id, user_id, pricing_mode, external_model_name, internal_model_id,
		                            r_value, status, upstream_status, upstream_error_summary, cost_source)
		 VALUES (101, 'bill-ue-1', 1, 'sale', 'ext', 'int', 10000, 'upstream_error', 429, 'rate_limit: too many requests', 'none')`); err != nil {
		t.Fatalf("upstream_error 应可写入: %v", err)
	}
	if _, err := tempDB.Exec(
		`INSERT INTO billing_records(id, billing_id, user_id, pricing_mode, external_model_name, internal_model_id, r_value, status, cost_source)
		 VALUES (102, 'bill-ue-2', 1, 'sale', 'ext', 'int', 10000, 'bogus', 'none')`); err == nil {
		t.Fatalf("非法状态必须被 CHECK 拒绝")
	}

	// 幂等：同一文件再跑一次不报错、不重复建约束。
	runMigration(t, tempDB, migDir, "0025_v2_billing_upstream_error.sql")
	var rows int
	if err := tempDB.QueryRow(`SELECT count(*) FROM billing_records WHERE status='upstream_error'`).Scan(&rows); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if rows != 1 {
		t.Fatalf("重跑后 upstream_error 行数应仍为 1，实际 %d", rows)
	}
}
