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
//	LLMGATEWAY_TEST_PG_DSN='postgres://dev:123456@10.126.126.1:55432/postgres?sslmode=disable' \
//	  go test ./internal/db -run TestMigrate_ThreeSegments -v
//
// 未设置 LLMGATEWAY_TEST_PG_DSN 时该测试 t.Skip，不阻塞普通 go test ./...。
// 测试会在临时库内先应用 0001~0010 建表（直接读 migrations 目录文件，不用 Migrate()，
// 以便精确控制只应用 <0011 的历史迁移），再插入样例数据、执行 0011/0013 并断言结果；
// 结束时无条件删除临时库。
func TestMigrate_ThreeSegments(t *testing.T) {
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过迁移集成测试")
	}

	adminDB, err := Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败: %v", err)
	}
	defer adminDB.Close()
	if err := adminDB.Ping(); err != nil {
		t.Fatalf("管理连接 ping 失败（DSN 是否可达?）: %v", err)
	}

	name := fmt.Sprintf("llmgateway-test-%d", time.Now().UnixNano())
	if _, err := adminDB.Exec(`CREATE DATABASE ` + quoteIdent(name)); err != nil {
		t.Fatalf("创建临时库 %s 失败: %v", name, err)
	}
	// 清理必须在建库后立即注册：后续 DSN 构造/连接/建表任一步失败都会走到这里，
	// 避免异常路径遗留临时库。tempDB 可能为 nil，defer 内按需关闭。
	var tempDB *sql.DB
	defer func() {
		if tempDB != nil {
			_ = tempDB.Close()
		}
		_, _ = adminDB.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, name)
		if _, derr := adminDB.Exec(`DROP DATABASE IF EXISTS ` + quoteIdent(name)); derr != nil {
			t.Logf("删除临时库 %s 失败: %v", name, derr)
		} else {
			t.Logf("已删除临时库 %s", name)
		}
	}()
	tempDSN, err := withDatabase(dsn, name)
	if err != nil {
		t.Fatalf("构造临时库 DSN 失败: %v", err)
	}
	db, err := Open(tempDSN)
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	tempDB = db
	if err := tempDB.Ping(); err != nil {
		t.Fatalf("临时库 ping 失败: %v", err)
	}
	t.Logf("已创建临时库 %s", name)

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

	runMigration(t, tempDB, migDir, migrationFile(t, migDir, "0011"))

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
	runMigration(t, tempDB, migDir, migrationFile(t, migDir, "0013"))

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
