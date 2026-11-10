// verify-0019-equivalence：0019 的两条回填路径等价性校验（可复现，一条命令跑完）。
//
// 做什么：
//  1. 建两个一次性临时库，写入**完全相同**的造数（默认 55,000 行 channel_models，含软删协议、
//     软删渠道、基址/协议双空、漂移成本、time_config='{}'、脏种子 externalModelID、内联渠道等）；
//  2. A 路（超阈值）：迁移 0019 只做 DDL（数据段被阈值守卫跳过）→ 跑 scripts/backfill-0019 的三阶段；
//  3. B 路（强制内联）：把内联阈值临时调高 → 迁移 0019 自己跑完数据段；
//  4. 比对 channels / channel_models / channel_base_url_archive 三个内容指纹 + edited 行数，
//     全等则退出码 0，否则 1。
//
// 用法（只需一个可建库的 DSN，脚本自己建临时库、跑完删库）：
//
//	cd backend
//	go run ./scripts/verify-0019-equivalence -dsn "$PG_TEST_DSN"
//	go run ./scripts/verify-0019-equivalence -dsn "$PG_TEST_DSN" -rows 55000 -keep   # 保留临时库便于排查
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/db"
)

func main() {
	var (
		dsn        = flag.String("dsn", "", "管理连接 DSN（需能 CREATE DATABASE；为空则读 PG_TEST_DSN / DATABASE_URL）")
		rows       = flag.Int("rows", 55000, "channel_models 行数")
		migrations = flag.String("migrations", "../backend/internal/db/migrations", "迁移目录（默认按仓库布局解析）")
		keep       = flag.Bool("keep", false, "保留临时库（默认跑完删除）")
	)
	flag.Parse()

	target := *dsn
	if target == "" {
		target = os.Getenv("PG_TEST_DSN")
	}
	if target == "" {
		target = os.Getenv("DATABASE_URL")
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "缺少 DSN：用 -dsn 指定，或设置 PG_TEST_DSN / DATABASE_URL")
		os.Exit(2)
	}
	dir := resolveMigrationDir(*migrations)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	fmt.Printf("迁移目录: %s\n行数: %d\n\n", dir, *rows)

	a, err := runPath(ctx, target, dir, *rows, pathThreshold, *keep)
	if err != nil {
		log.Fatalf("A 路（超阈值 + 独立回填）失败: %v", err)
	}
	b, err := runPath(ctx, target, dir, *rows, pathInline, *keep)
	if err != nil {
		log.Fatalf("B 路（强制内联）失败: %v", err)
	}

	fmt.Printf("\n%-28s %s\n", "指标", "结果")
	fmt.Println(strings.Repeat("-", 80))
	ok := true
	for _, k := range fingerprintKeys() {
		same := a.fp[k] == b.fp[k]
		ok = ok && same
		mark := "一致"
		if !same {
			mark = "不一致"
		}
		fmt.Printf("%-28s A=%s B=%s %s\n", k, a.fp[k], b.fp[k], mark)
	}
	same := a.editedCount == b.editedCount
	ok = ok && same
	mark := "一致"
	if !same {
		mark = "不一致"
	}
	fmt.Printf("%-28s A=%d B=%d %s\n", "edited 行数", a.editedCount, b.editedCount, mark)
	fmt.Printf("%-28s A=%d B=%d\n", "channel_models 行数", a.modelRows, b.modelRows)
	fmt.Printf("%-28s A=%s B=%s\n", "耗时", a.elapsed, b.elapsed)

	if !ok {
		fmt.Println("\n结论：两条路径结果不一致（退出码 1）")
		os.Exit(1)
	}
	fmt.Println("\n结论：两条路径结果完全一致（退出码 0）")
}

type pathMode int

const (
	pathThreshold pathMode = iota // 迁移只做 DDL，数据段交给独立回填
	pathInline                    // 阈值临时调高，迁移自己跑完数据段
)

type pathResult struct {
	fp          map[string]string
	editedCount int64
	modelRows   int64
	elapsed     time.Duration
}

func fingerprintKeys() []string {
	return []string{"channels", "channel_models", "archive"}
}

// runPath 在独立临时库里跑完整一条路径，返回四个指纹（三个内容指纹 + edited 行数）。
func runPath(ctx context.Context, adminDSN, migDir string, rows int, mode pathMode, keep bool) (pathResult, error) {
	start := time.Now()
	name := fmt.Sprintf("verify0019_%s_%d", map[pathMode]string{pathThreshold: "a", pathInline: "b"}[mode], time.Now().UnixNano())
	admin, err := db.Open(adminDSN)
	if err != nil {
		return pathResult{}, fmt.Errorf("连接管理库: %w", err)
	}
	defer admin.Close()
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		return pathResult{}, fmt.Errorf("建临时库 %s: %w", name, err)
	}
	if !keep {
		defer func() {
			_, _ = admin.ExecContext(context.Background(),
				`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, name)
			_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+quoteIdent(name))
		}()
	}

	dsn, err := withDatabase(adminDSN, name)
	if err != nil {
		return pathResult{}, err
	}
	conn, err := db.Open(dsn)
	if err != nil {
		return pathResult{}, fmt.Errorf("连接临时库: %w", err)
	}
	defer conn.Close()

	// 1) 应用 < 0019 的历史迁移（与线上同一条链）。
	files, err := migrationFiles(migDir)
	if err != nil {
		return pathResult{}, err
	}
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), "0019") {
			continue
		}
		if err := execFile(ctx, conn, f); err != nil {
			return pathResult{}, fmt.Errorf("应用迁移 %s: %w", filepath.Base(f), err)
		}
	}

	// 2) 造数（两条路径完全一致）。
	if err := seed(ctx, conn, rows); err != nil {
		return pathResult{}, fmt.Errorf("造数: %w", err)
	}

	// 3) 跑 0019：A 路让阈值守卫跳过数据段；B 路把阈值临时调高走内联。
	orig := db.Backfill0019InlineThreshold
	if mode == pathInline {
		db.Backfill0019InlineThreshold = 1 << 62
	}
	var migrationFileAbs string
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), "0019") {
			migrationFileAbs = f
		}
	}
	if migrationFileAbs == "" {
		db.Backfill0019InlineThreshold = orig
		return pathResult{}, fmt.Errorf("迁移目录里没有 0019_*.sql")
	}
	err = execFile(ctx, conn, migrationFileAbs)
	db.Backfill0019InlineThreshold = orig
	if err != nil {
		return pathResult{}, fmt.Errorf("应用 0019: %w", err)
	}

	if mode == pathThreshold {
		if _, err := db.Backfill0019(ctx, conn, db.Backfill0019Options{BatchSize: 5000}); err != nil {
			return pathResult{}, fmt.Errorf("独立回填: %w", err)
		}
	}

	// 4) 取指纹。
	res := pathResult{fp: map[string]string{}, elapsed: time.Since(start)}
	queries := map[string]string{
		"channels": `SELECT md5(COALESCE(string_agg(id::text||'/'||base_url||'/'||protocol, '|' ORDER BY id), '')) FROM channels`,
		"channel_models": `SELECT md5(COALESCE(string_agg(id::text||'/'||internal_model_id||'/'||
			COALESCE(protocols::text,'-')||'/'||edited::text||'/'||COALESCE(cost_rates::text,'-')||'/'||
			external_model_id::text, '|' ORDER BY id), '')) FROM channel_models`,
		"archive": `SELECT md5(COALESCE(string_agg(channel_id::text||'/'||base_url||'/'||protocol, '|' ORDER BY channel_id), '')) FROM channel_base_url_archive`,
	}
	for k, q := range queries {
		var fp sql.NullString
		if err := conn.QueryRowContext(ctx, q).Scan(&fp); err != nil {
			return pathResult{}, fmt.Errorf("取指纹 %s: %w", k, err)
		}
		res.fp[k] = fp.String
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM channel_models WHERE edited`).Scan(&res.editedCount); err != nil {
		return pathResult{}, fmt.Errorf("统计 edited: %w", err)
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM channel_models`).Scan(&res.modelRows); err != nil {
		return pathResult{}, fmt.Errorf("统计行数: %w", err)
	}
	return res, nil
}

// seed 写入与测试同构的造数：五类渠道 + 七类边界模型行 + 批量行，总数恰为 rows。
func seed(ctx context.Context, conn *sql.DB, rows int) error {
	// 边界行 7 条 + 成规模漂移行 1000 条 + 其余批量行（与违反种子无关，用于压扫描路径）。
	const edgeRows, driftRows = 7, 1000
	if rows < edgeRows+driftRows {
		return fmt.Errorf("rows 至少 %d", edgeRows)
	}
	stmts := []string{
		`INSERT INTO external_models(id, external_name) VALUES (2,'ext-a'), (5,'ext-b')`,
		`INSERT INTO channel_protocols(id, name, base_url, protocols, request_headers, models, deleted_at) VALUES
		 (7,'live','https://proto.example.com','{"openai-chat":{"baseUrl":"https://proto-chat.example.com"}}'::jsonb,
		  '{"passthroughMode":"none"}'::jsonb,
		  '{"m-subset":{"externalModelID":"2","protocols":{"anthropic-messages":{}}},
		    "m-drift":{"externalModelID":"2","costRates":{"Input":3,"Output":4,"CacheRead":0.3}},
		    "m-timecfg-empty":{"externalModelID":"2","timeConfig":{"Default":2}},
		    "m-dirty-seed":{"externalModelID":"abc"},
		    "m-eq":{"externalModelID":"2","costRates":{"Input":1,"Output":2,"CacheRead":0.1}},
		    "m-conflict-protocols":{"externalModelID":"2","protocols":{"anthropic-messages":{}}}}'::jsonb, NULL),
		 (8,'gone','https://gone.example.com','{"openai-chat":{}}'::jsonb,'{"passthroughMode":"none"}'::jsonb,'{}'::jsonb, now())`,
		`INSERT INTO channels(id, name, base_url, protocol, channel_protocol_id) VALUES
		 (1,'ref-with-inline','https://legacy-1.example.com','openai-compat',7),
		 (2,'ref-empty','','',7),
		 (3,'refs-soft-deleted','https://kept-3.example.com','openai-compat',8),
		 (4,'soft-deleted','https://legacy-4.example.com','openai-compat',7),
		 (5,'inline','https://inline.example.com','openai-chat',NULL)`,
		`UPDATE channels SET deleted_at = now() WHERE id = 4`,
		`INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates, protocols, time_config) VALUES
		 (1, 1, 'm-subset', 2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb, NULL, NULL),
		 (2, 1, 'm-drift', 2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb, NULL, NULL),
		 (3, 1, 'm-timecfg-empty', 2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb, NULL, '{}'::jsonb),
		 (4, 1, 'm-dirty-seed', 2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb, NULL, NULL),
		 (5, 1, 'm-eq', 2, '{"input":1.0,"output":2.0,"cache_read":0.1}'::jsonb, NULL, NULL),
		 (6, 1, 'm-conflict-protocols', 2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb,
		     '{"openai-responses":{}}'::jsonb, NULL),
		 (7, 5, 'inline-1', 2, '{"input":7,"output":7,"cache_read":7}'::jsonb, NULL, NULL)`,
		// 成规模漂移：种子动态追加 1000 个模型键（成本 1/2/0.1），实例写成 5/6/0.5 → 全部应标 edited。
		fmt.Sprintf(`UPDATE channel_protocols SET models = models || (
			SELECT jsonb_object_agg('drift-' || i, jsonb_build_object(
				'externalModelID','2',
				'costRates', jsonb_build_object('Input',1,'Output',2,'CacheRead',0.1)))
			FROM generate_series(1, %d) AS i)
			WHERE id = 7`, driftRows),
		fmt.Sprintf(`INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates, protocols)
			SELECT 200000 + i, 1, 'drift-' || i, 2, '{"input":5,"output":6,"cache_read":0.5}'::jsonb, NULL
			FROM generate_series(1, %d) AS i`, driftRows),
		fmt.Sprintf(`INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates, protocols)
			SELECT 100000 + i, 1, 'bulk-' || i, 2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb,
			       '{"openai-chat":{}}'::jsonb
			FROM generate_series(1, %d) AS i`, rows-edgeRows-driftRows),
	}
	for i, q := range stmts {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("语句 %d: %w", i+1, err)
		}
	}
	return nil
}

func migrationFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取迁移目录 %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

func execFile(ctx context.Context, conn *sql.DB, path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, db.RenderMigration(string(body)))
	return err
}

// resolveMigrationDir 允许相对当前工作目录解析不到时，按仓库布局回退到 backend/internal/db/migrations。
func resolveMigrationDir(dir string) string {
	candidates := []string{dir, "internal/db/migrations", "../internal/db/migrations", "backend/internal/db/migrations"}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return dir
}

func withDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
