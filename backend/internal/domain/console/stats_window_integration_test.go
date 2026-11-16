package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/db"
	"github.com/team/llmgateway/internal/domain/billing"
)

// 统计页窗口口径 / 模型广场实体归属的集成测试（可选执行，**只在自己的临时库里跑**）。
//
// 运行方式（不要在共享 test/staging 库上跑）：
//
//	cd backend
//	LLMGATEWAY_TEST_PG_DSN='postgres://user:pass@host:port/postgres?sslmode=disable' \
//	  go test ./internal/domain/console -run TestStats_WindowAndPlaza -v
//
// 未设置该变量时整条测试 t.Skip；设置了就 CREATE DATABASE llmgateway-stats-<纳秒>，
// 跑完 pg_terminate_backend + DROP DATABASE，不留痕。要求角色有 CREATEDB。

// openStatsTempDB 建临时库 + 跑全部迁移，测试结束自动删库。
func openStatsTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过统计窗口集成测试")
	}
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.Ping(); err != nil {
		t.Fatalf("管理连接 ping 失败（DSN 是否可达?）: %v", err)
	}
	name := fmt.Sprintf("llmgateway-stats-%d", time.Now().UnixNano())
	if !regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(name) {
		t.Fatalf("临时库名非法: %s", name)
	}
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("创建临时库 %s 失败: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, name)
		if _, derr := admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`); derr != nil {
			t.Logf("删除临时库 %s 失败: %v", name, derr)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("解析 DSN 失败: %v", err)
	}
	u.Path = "/" + name
	temp, err := db.Open(u.String())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = temp.Close() })
	if _, err := db.Migrate(context.Background(), temp); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	return temp
}

// seedStatsWindowFixture 造一份覆盖「跨自然日边界 / 改名 / 无法解析」的账单样本。
//
// 关键时间点（北京时间）：
//
//	窗口（滚动 24h）：2026-10-24T12:00 ~ 2026-10-25T12:00
//	日界窗口        ：2026-10-24T00:00 ~ 2026-10-25T23:59:59
//
// 账单：
//
//	1  10-24 23:59  gpt-4o         gpt-4o-int   100 token   ← 跨日边界前一条，落入滚动窗口
//	2  10-25 00:01  gpt-4o         gpt-4o-int   200 token   ← 跨日边界后一条，落入滚动窗口
//	3  10-24 11:00  gpt-4o         gpt-4o-int   400 token   ← 早于滚动窗口，只在日界窗口里
//	4  10-25 12:30  gpt-4o         gpt-4o-int   800 token   ← 晚于滚动窗口，只在日界窗口里
//	5  10-25 09:00  gpt-4o-legacy  gpt-4o-int    50 token   ← 改名前的旧文本（0024 后无从解析）→ 未匹配
//	6  10-25 10:00  ghost-model    ghost-int     70 token   ← 目录里没有这个名字 → 未匹配
func seedStatsWindowFixture(t *testing.T, d *sql.DB) {
	t.Helper()
	exec := func(sqlText string, args ...any) {
		t.Helper()
		if _, err := d.Exec(sqlText, args...); err != nil {
			t.Fatalf("执行 SQL 失败: %v\nSQL: %s", err, sqlText)
		}
	}
	exec(`INSERT INTO users(id, username, password_hash, role) VALUES (7,'dev','x','DEVELOPER')`)
	// 渠道族表已随 0024 删除：归属只看账单里的对外模型文本是否命中目录。
	exec(`INSERT INTO external_models(id, external_name) VALUES (31,'gpt-4o')`)
	exec(`INSERT INTO billing_records(id,billing_id,user_id,pricing_mode,external_model_name,internal_model_id,r_value,tokens,credits_consumed,cost_credits,status,duration_ms,call_time) VALUES
	 (1,'w1',7,'sale','gpt-4o','gpt-4o-int',1,'{"input":100,"cache_read":0,"output":0}'::jsonb,1.0,0.5,'completed',1000,'2026-10-24 23:59:00+08'),
	 (2,'w2',7,'sale','gpt-4o','gpt-4o-int',1,'{"input":200,"cache_read":0,"output":0}'::jsonb,2.0,1.0,'completed',2000,'2026-10-25 00:01:00+08'),
	 (3,'w3',7,'sale','gpt-4o','gpt-4o-int',1,'{"input":400,"cache_read":0,"output":0}'::jsonb,4.0,2.0,'completed',4000,'2026-10-24 11:00:00+08'),
	 (4,'w4',7,'sale','gpt-4o','gpt-4o-int',1,'{"input":800,"cache_read":0,"output":0}'::jsonb,8.0,4.0,'completed',8000,'2026-10-25 12:30:00+08'),
	 (5,'w5',7,'sale','gpt-4o-legacy','gpt-4o-int',1,'{"input":50,"cache_read":0,"output":0}'::jsonb,0.5,0.25,'completed',500,'2026-10-25 09:00:00+08'),
	 (6,'w6',7,'sale','ghost-model','ghost-int',1,'{"input":70,"cache_read":0,"output":0}'::jsonb,0.7,0.35,'completed',700,'2026-10-25 10:00:00+08')`)
}

// statsUsageForTest 按给定查询串跑一次 buildStatsUsage（admin 口径、全局范围）。
func statsUsageForTest(t *testing.T, store *billing.SqlStore, query string, now time.Time) statsResp {
	t.Helper()
	q, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("解析查询串失败: %v", err)
	}
	rg, err := parseStatsRange(q, now)
	if err != nil {
		t.Fatalf("parseStatsRange: %v", err)
	}
	opts, err := parseStatsOptions(q, rg.ExactTo.Sub(rg.ExactFrom))
	if err != nil {
		t.Fatalf("parseStatsOptions: %v", err)
	}
	out, err := buildStatsUsage(context.Background(), store, rg, nil, "global", opts)
	if err != nil {
		t.Fatalf("buildStatsUsage: %v", err)
	}
	return out
}

// TestStats_WindowAndPlaza 覆盖 PR #61 Owl P2 的四条校验：
//  1. 滚动 24h 边界（跨自然日 00:00 前后各一条都在窗口内；窗口外的两条都不在）；
//  2. KPI 与小时趋势同窗口（趋势各桶之和 == KPI 总量）；
//  3. 认得出的进目录行、认不出的进「未匹配」行（0024 后归属只看对外模型文本，改名前的旧文本算未匹配）；
//  4. 广场各行汇总与同窗口总量守恒（差值为 0）。
func TestStats_WindowAndPlaza(t *testing.T) {
	d := openStatsTempDB(t)
	seedStatsWindowFixture(t, d)
	store := billing.NewSqlStore(d)

	// 「现在」= 2026-10-25 12:00（北京时间），滚动 24h 窗口 = [10-24 12:00, 10-25 12:00]。
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, cnLocation())
	from := now.Add(-24 * time.Hour)
	query := "From=" + url.QueryEscape(from.Format(time.RFC3339)) +
		"&To=" + url.QueryEscape(now.Format(time.RFC3339)) + "&Granularity=hour"

	out := statsUsageForTest(t, store, query, now)

	// ---- ① 滚动 24h 边界 ----
	// 窗口内 4 条：100 + 200 + 50(旧名) + 70(未匹配) = 420；请求 4 次。
	if out.KPI.TotalTokens != 420 || out.KPI.TotalRequests != 4 {
		t.Fatalf("KPI 窗口口径错: tokens=%d calls=%d，want 420/4（窗口外的 400+800 不得计入）",
			out.KPI.TotalTokens, out.KPI.TotalRequests)
	}
	if out.KPI.WindowMode != "exact" ||
		out.KPI.WindowFrom != from.Format(time.RFC3339) || out.KPI.WindowTo != now.Format(time.RFC3339) {
		t.Fatalf("KPI 窗口元信息错: %+v", out.KPI)
	}
	// 日界口径仍是老样子：6 条全在内，合计 1620。
	if out.Usage.TotalCall != 6 || out.Usage.Total.Total() != 1620 {
		t.Fatalf("Usage 日界口径错: calls=%d tokens=%d，want 6/1620", out.Usage.TotalCall, out.Usage.Total.Total())
	}

	// ---- ② KPI 与趋势同窗口 ----
	var hourlyTokens, hourlyCalls int64
	for _, h := range out.Usage.Hourly {
		hourlyTokens += h.Tokens
		hourlyCalls += h.Calls
	}
	if hourlyTokens != out.KPI.TotalTokens || hourlyCalls != out.KPI.TotalRequests {
		t.Fatalf("趋势与 KPI 不同窗口：hourly=%d/%d KPI=%d/%d",
			hourlyTokens, hourlyCalls, out.KPI.TotalTokens, out.KPI.TotalRequests)
	}

	// ---- ③ 目录归属 + 未匹配兜底 ----
	var gpt, unmatched *billing.DashModelStat
	for i := range out.ModelPlaza.List {
		switch {
		case out.ModelPlaza.List[i].Unmatched:
			unmatched = &out.ModelPlaza.List[i]
		case out.ModelPlaza.List[i].Model == "gpt-4o":
			gpt = &out.ModelPlaza.List[i]
		}
	}
	if gpt == nil {
		t.Fatalf("广场缺少 gpt-4o 行: %+v", out.ModelPlaza.List)
	}
	// 只有名字命中目录的两条归到 gpt-4o：100+200=300，2 次请求。
	// （改名前的 gpt-4o-legacy 在 0024 后没有解析依据 → 落到未匹配行，见下一条断言。）
	if gpt.Tokens != 300 || gpt.Calls != 2 {
		t.Fatalf("gpt-4o tokens=%d calls=%d，want 300/2", gpt.Tokens, gpt.Calls)
	}
	if unmatched == nil {
		t.Fatalf("缺少「未匹配」行: %+v", out.ModelPlaza.List)
	}
	if unmatched.Calls != 2 || unmatched.Tokens != 120 {
		t.Fatalf("未匹配行应含「旧名 50」+「目录里没有的 70」共 120 token / 2 次: %+v", unmatched)
	}
	// 未匹配行不计入目录口径。
	if out.ModelPlaza.Total != 1 || out.ModelPlaza.Active != 1 {
		t.Fatalf("目录口径错: Total=%d Active=%d，want 1/1", out.ModelPlaza.Total, out.ModelPlaza.Active)
	}

	// ---- ④ 广场汇总守恒 ----
	var plazaTokens, plazaCalls, plazaFailed int64
	for _, row := range out.ModelPlaza.List {
		plazaTokens += row.Tokens
		plazaCalls += row.Calls
		plazaFailed += row.Failed
	}
	if plazaTokens != out.KPI.TotalTokens || plazaCalls != out.KPI.TotalRequests || plazaFailed != out.KPI.FailedRequests {
		t.Fatalf("广场与总量不守恒：广场=%d/%d/%d KPI=%d/%d/%d",
			plazaTokens, plazaCalls, plazaFailed, out.KPI.TotalTokens, out.KPI.TotalRequests, out.KPI.FailedRequests)
	}
}

// TestStats_WindowCalendarMode 断言 Window=calendar 时 KPI 退回日界窗口（等价改造前行为），
// 便于需要旧数字的调用方按需切换。
func TestStats_WindowCalendarMode(t *testing.T) {
	d := openStatsTempDB(t)
	seedStatsWindowFixture(t, d)
	store := billing.NewSqlStore(d)

	now := time.Date(2026, 10, 25, 12, 0, 0, 0, cnLocation())
	from := now.Add(-24 * time.Hour)
	query := "From=" + url.QueryEscape(from.Format(time.RFC3339)) +
		"&To=" + url.QueryEscape(now.Format(time.RFC3339)) + "&Window=calendar"

	out := statsUsageForTest(t, store, query, now)
	if out.KPI.WindowMode != "calendar" {
		t.Fatalf("WindowMode=%q，want calendar", out.KPI.WindowMode)
	}
	// 日界窗口把 10-24 11:00 与 10-25 12:30 也算进来：6 条 / 1620 token。
	if out.KPI.TotalRequests != 6 || out.KPI.TotalTokens != 1620 {
		t.Fatalf("calendar 模式 KPI 应回到日界口径: calls=%d tokens=%d", out.KPI.TotalRequests, out.KPI.TotalTokens)
	}
	// 与同一次响应里的日界聚合一致（同一份数据不出现两个数）。
	if out.KPI.TotalRequests != out.Usage.TotalCall || out.KPI.TotalTokens != out.Usage.Total.Total() {
		t.Fatalf("calendar 模式下 KPI 与 Usage 应一致: KPI=%d/%d Usage=%d/%d",
			out.KPI.TotalTokens, out.KPI.TotalRequests, out.Usage.Total.Total(), out.Usage.TotalCall)
	}
}

// TestStats_ResponseJSONShape 守卫响应字段名（前端照此对接）：KPI 窗口三字段与新广场字段必须存在。
func TestStats_ResponseJSONShape(t *testing.T) {
	d := openStatsTempDB(t)
	seedStatsWindowFixture(t, d)
	store := billing.NewSqlStore(d)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, cnLocation())
	out := statsUsageForTest(t, store,
		"From="+url.QueryEscape(now.Add(-24*time.Hour).Format(time.RFC3339))+
			"&To="+url.QueryEscape(now.Format(time.RFC3339)), now)

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"WindowFrom"`, `"WindowTo"`, `"WindowMode"`, `"Unmatched"`} {
		if !regexp.MustCompile(regexp.QuoteMeta(key)).MatchString(string(raw)) {
			t.Fatalf("响应缺少字段 %s", key)
		}
	}
}

// TestStats_SoftDeletedModelGoesUnmatched 断言「解析到的目录模型后来被软删」时不会静默丢量：
// 该模型的窗口用量必须落到显式的「未匹配」行，且广场汇总与 KPI 总量守恒（差值 0）。
//
// Gate 复现过的问题：agg 只兜 ext_id=0，而软删模型在目录 JOIN 里被 deleted_at IS NULL 滤掉，
// 于是这批用量既不在目录行、也不进未匹配行（KPI 510/6 vs 广场 450/5）。
func TestStats_SoftDeletedModelGoesUnmatched(t *testing.T) {
	d := openStatsTempDB(t)
	exec := func(sqlText string) {
		t.Helper()
		if _, err := d.Exec(sqlText); err != nil {
			t.Fatalf("执行 SQL 失败: %v\nSQL: %s", err, sqlText)
		}
	}
	exec(`INSERT INTO users(id, username, password_hash, role) VALUES (7,'dev','x','DEVELOPER')`)
	// 701 稍后软删；702 保持在册。（渠道族表已随 0024 删除，归属只看对外模型文本。）
	exec(`INSERT INTO external_models(id, external_name) VALUES (701,'legacy-model'),(702,'gpt-4o')`)
	exec(`INSERT INTO billing_records(id,billing_id,user_id,pricing_mode,external_model_name,internal_model_id,r_value,tokens,credits_consumed,cost_credits,status,duration_ms,call_time) VALUES
	 (1,'s1',7,'sale','gpt-4o','gpt-4o-int',1,'{"input":400,"cache_read":0,"output":0}'::jsonb,4.0,2.0,'completed',1000,'2026-10-25 09:00:00+08'),
	 (2,'s2',7,'sale','gpt-4o','gpt-4o-int',1,'{"input":50,"cache_read":0,"output":0}'::jsonb,0.5,0.25,'completed',500,'2026-10-25 09:30:00+08'),
	 (3,'s3',7,'sale','legacy-model','legacy-int',1,'{"input":60,"cache_read":0,"output":0}'::jsonb,0.6,0.3,'completed',600,'2026-10-25 10:00:00+08')`)
	// 软删 701：历史账单仍在，但目录里它不算在册 → 用量必须落到「未匹配」行（不许静默丢）。
	exec(`UPDATE external_models SET deleted_at = now() WHERE id = 701`)

	store := billing.NewSqlStore(d)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, cnLocation())
	from := now.Add(-24 * time.Hour)
	out := statsUsageForTest(t, store, "From="+url.QueryEscape(from.Format(time.RFC3339))+
		"&To="+url.QueryEscape(now.Format(time.RFC3339)), now)

	// 窗口内 3 条：400 + 50 + 60 = 510 token / 3 次。
	if out.KPI.TotalTokens != 510 || out.KPI.TotalRequests != 3 {
		t.Fatalf("KPI 应统计全部 3 条: tokens=%d calls=%d", out.KPI.TotalTokens, out.KPI.TotalRequests)
	}

	var gpt, unmatched *billing.DashModelStat
	for i := range out.ModelPlaza.List {
		switch {
		case out.ModelPlaza.List[i].Unmatched:
			unmatched = &out.ModelPlaza.List[i]
		case out.ModelPlaza.List[i].Model == "gpt-4o":
			gpt = &out.ModelPlaza.List[i]
		}
	}
	if gpt == nil || gpt.Tokens != 450 || gpt.Calls != 2 {
		t.Fatalf("在册模型的用量错: %+v", gpt)
	}
	if unmatched == nil {
		t.Fatalf("软删模型的用量必须进「未匹配」行，实际行=%+v", out.ModelPlaza.List)
	}
	if unmatched.Tokens != 60 || unmatched.Calls != 1 {
		t.Fatalf("未匹配行应只含软删模型那 60 token / 1 次: %+v", unmatched)
	}
	// 软删模型不计入目录口径。
	if out.ModelPlaza.Total != 1 || out.ModelPlaza.Active != 1 {
		t.Fatalf("目录口径错: Total=%d Active=%d（软删模型不算在册）", out.ModelPlaza.Total, out.ModelPlaza.Active)
	}
	// 守恒：广场各行汇总 == KPI 总量，差值必须为 0。
	var tokens, calls, failed int64
	for _, row := range out.ModelPlaza.List {
		tokens += row.Tokens
		calls += row.Calls
		failed += row.Failed
	}
	if tokens != out.KPI.TotalTokens || calls != out.KPI.TotalRequests || failed != out.KPI.FailedRequests {
		t.Fatalf("守恒失败：广场=%d/%d/%d KPI=%d/%d/%d（差值=%d/%d/%d）",
			tokens, calls, failed, out.KPI.TotalTokens, out.KPI.TotalRequests, out.KPI.FailedRequests,
			out.KPI.TotalTokens-tokens, out.KPI.TotalRequests-calls, out.KPI.FailedRequests-failed)
	}
}
