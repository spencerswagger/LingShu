package console

// stats_perf_bench_test.go 统计/账单接口的性能对照（默认跳过，需显式给 DSN）。
//
// 为什么要放进仓库而不是一次性脚本：数字必须可复现——同一份数据集、同一台机器、
// 同样的 30 次取样，任何人（Gate/后续维护者）都能重跑出同样的表。
//
// 运行：
//
//	PERF_BENCH_DSN='postgres://.../lingshu_dev' \
//	PERF_BENCH_ITERS=30 \
//	go test ./internal/domain/console/ -run TestPerfStats -v -count=1 -timeout 4h
//
// 数据集构造见 scripts/bench-stats/seed.sql（30M 行、top 用户占 40%）。
//
// 对照口径：
//   - legacy：改造前的 SQL 原文（本人摘要 4 次明细聚合 / 成本统计全表扫 + JSONB 正则），直接压 DB；
//   - new：改造后的 handler 全路径（聚合表 + 缓存；每轮前清缓存 = 冷启动口径）。
//
// 同时做口径等价校验：改造前后「今日 / 昨日 / 本月积分」「三段 token」必须一致
// （否则性能改造就是拿错误数字换速度）。

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/team/llmgateway/internal/domain/billing"
)

// legacyMeSummarySQL 改造前 /me/summary 的明细聚合 SQL（逐行解析 tokens JSONB）。
const legacyMeSummarySQL = `SELECT count(*),
        COALESCE(sum(credits_consumed), 0),
        COALESCE(sum(
            COALESCE(NULLIF(tokens->>'input','')::bigint, 0) +
            COALESCE(NULLIF(tokens->>'cache_read','')::bigint, 0) +
            COALESCE(NULLIF(tokens->>'output','')::bigint, 0)), 0),
        COALESCE(sum(COALESCE(NULLIF(tokens->>'input','')::bigint, 0)), 0),
        COALESCE(sum(COALESCE(NULLIF(tokens->>'cache_read','')::bigint, 0)), 0)
 FROM billing_records
 WHERE user_id = $1 AND status = 'completed' AND call_time >= $2 AND call_time <= $3`

// legacyCostStatsSQL 改造前 /admin/cost/stats 的主查询（全表扫 + 每行三段 JSONB 正则），
// 与 cost.go 的 costStatsQuerySQL 只差 token 表达式与时间窗。
const legacyCostStatsSQL = `
SELECT count(*),
       COALESCE(SUM(b.cost_credits), 0),
       COALESCE(SUM(b.credits_consumed), 0),
       COALESCE(SUM(CASE WHEN (b.tokens->>'input') ~ '^-?[0-9]{1,18}$' THEN (b.tokens->>'input')::bigint ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN (b.tokens->>'cache_read') ~ '^-?[0-9]{1,18}$' THEN (b.tokens->>'cache_read')::bigint ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN (b.tokens->>'output') ~ '^-?[0-9]{1,18}$' THEN (b.tokens->>'output')::bigint ELSE 0 END), 0),
       to_json(array_agg(DISTINCT b.external_model_name))::text
FROM billing_records b
LEFT JOIN channel_keys k ON k.id = b.channel_key_id
LEFT JOIN channels c ON c.id = k.channel_id
WHERE b.status = 'completed'`

// perfSample 一次取样的耗时分位。
type perfSample struct {
	n    int
	p50  time.Duration
	p95  time.Duration
	min  time.Duration
	max  time.Duration
	mean time.Duration
}

// sampleOf 统计 n 次耗时（已排序）的 p50/p95。
func sampleOf(durs []time.Duration) perfSample {
	if len(durs) == 0 {
		return perfSample{}
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	var sum time.Duration
	for _, d := range durs {
		sum += d
	}
	pick := func(p float64) time.Duration {
		idx := int(float64(len(durs))*p+0.5) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(durs) {
			idx = len(durs) - 1
		}
		return durs[idx]
	}
	return perfSample{n: len(durs), p50: pick(0.50), p95: pick(0.95), min: durs[0], max: durs[len(durs)-1], mean: sum / time.Duration(len(durs))}
}

func (s perfSample) String() string {
	ms := func(d time.Duration) string {
		return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 1, 64)
	}
	return fmt.Sprintf("n=%d p50=%sms p95=%sms min=%sms max=%sms", s.n, ms(s.p50), ms(s.p95), ms(s.min), ms(s.max))
}

// TestPerfStatsDashboard 本人摘要 / 成本统计的改造前后 p50·p95 对照 + 口径等价校验。
func TestPerfStatsDashboard(t *testing.T) {
	dsn := os.Getenv("PERF_BENCH_DSN")
	if dsn == "" {
		t.Skip("未设置 PERF_BENCH_DSN（见文件头注释），跳过性能对照")
	}
	iters := 30
	if v := os.Getenv("PERF_BENCH_ITERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			iters = n
		}
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// 被测用户：数据集里热度最高的那个（seed.sql 约定 900000000）。
	userID := int64(900000000)
	now := time.Now()
	today, y, m, win := summaryRanges(now, cnLocation())

	// ---- 口径等价校验（先证明改造后数字没变，再谈快慢）----
	legacy := func(from, to time.Time) (credits float64, input, cacheRead int64) {
		var requests, tokens int64
		err := db.QueryRowContext(ctx, legacyMeSummarySQL, userID, from, to).
			Scan(&requests, &credits, &tokens, &input, &cacheRead)
		if err != nil {
			t.Fatalf("legacy summary: %v", err)
		}
		return credits, input, cacheRead
	}
	legacyToday, _, _ := legacy(today.From, today.To)
	legacyYest, _, _ := legacy(y.From, y.To)
	legacyMonth, legacyInput, legacyCache := legacy(m.From, m.To)

	store := billing.NewSqlStore(db)
	rollupMonth, err := store.UserDayTotals(ctx, userID, today.From, y.From, m.From)
	if err != nil {
		t.Fatalf("rollup totals: %v", err)
	}
	near := func(a, b float64) bool { d := a - b; return d < 0.0005 && d > -0.0005 }
	if !near(rollupMonth.TodayCredits, legacyToday) || !near(rollupMonth.YesterdayCredits, legacyYest) ||
		!near(rollupMonth.MonthCredits, legacyMonth) ||
		rollupMonth.MonthInput != legacyInput || rollupMonth.MonthCacheRead != legacyCache {
		t.Fatalf("聚合口径与明细不一致：rollup=%+v legacy=today %.5f yest %.5f month %.5f input %d cache %d",
			rollupMonth, legacyToday, legacyYest, legacyMonth, legacyInput, legacyCache)
	}
	t.Logf("口径校验通过：今日 %.5f / 昨日 %.5f / 本月 %.5f / 本月 input %d cache %d",
		rollupMonth.TodayCredits, rollupMonth.YesterdayCredits, rollupMonth.MonthCredits,
		rollupMonth.MonthInput, rollupMonth.MonthCacheRead)

	// ---- 1) /api/v1/auth/me/summary ----
	dev := NewDev(store, func(context.Context) (int64, bool) { return userID, true })
	dev.now = func() time.Time { return now }

	legacyMe := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		start := time.Now()
		legacy(today.From, today.To)
		legacy(y.From, y.To)
		legacy(m.From, m.To)
		// 吞吐窗口（近 1 分钟）改造前后都查明细，算进基线。
		if _, err := store.Summarize(ctx, billing.RecordFilter{UserID: &userID, From: &win.From, To: &win.To}); err != nil {
			t.Fatalf("legacy window: %v", err)
		}
		legacyMe = append(legacyMe, time.Since(start))
	}

	coldMe := make([]time.Duration, 0, iters)
	warmMe := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		dev.summaryCache().Purge() // 冷启动口径：每轮都回源
		start := time.Now()
		if code := callMeSummaryNoT(t, dev); code != 200 {
			t.Fatalf("new summary status=%d", code)
		}
		coldMe = append(coldMe, time.Since(start))

		start = time.Now()
		if code := callMeSummaryNoT(t, dev); code != 200 {
			t.Fatalf("new summary(cached) status=%d", code)
		}
		warmMe = append(warmMe, time.Since(start))
	}

	// ---- 2) /api/v1/admin/cost/stats ----
	admin := NewAdmin(db, store, nil, func(context.Context) (int64, bool) { return userID, true })
	admin.now = func() time.Time { return now }

	legacyCost := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		start := time.Now()
		rows, err := db.QueryContext(ctx, legacyCostStatsSQL)
		if err != nil {
			t.Fatalf("legacy cost stats: %v", err)
		}
		for rows.Next() {
			var (
				requests           int64
				cost, sale         float64
				in, cacheRead, out int64
				snapshots          string
			)
			if err := rows.Scan(&requests, &cost, &sale, &in, &cacheRead, &out, &snapshots); err != nil {
				rows.Close()
				t.Fatalf("legacy cost scan: %v", err)
			}
		}
		rows.Close()
		legacyCost = append(legacyCost, time.Since(start))
	}

	coldCost := make([]time.Duration, 0, iters)
	warmCost := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		admin.costStatsCache().Purge()
		start := time.Now()
		if code := callCostStatsNoT(t, admin); code != 200 {
			t.Fatalf("new cost stats status=%d", code)
		}
		coldCost = append(coldCost, time.Since(start))

		start = time.Now()
		if code := callCostStatsNoT(t, admin); code != 200 {
			t.Fatalf("new cost stats(cached) status=%d", code)
		}
		warmCost = append(warmCost, time.Since(start))
	}

	fmt.Printf("\n=== 统计接口改造前后对照（iters=%d）===\n", iters)
	fmt.Printf("/me/summary          legacy : %s\n", sampleOf(legacyMe))
	fmt.Printf("/me/summary          new(冷) : %s\n", sampleOf(coldMe))
	fmt.Printf("/me/summary          new(热) : %s\n", sampleOf(warmMe))
	fmt.Printf("/admin/cost/stats    legacy : %s\n", sampleOf(legacyCost))
	fmt.Printf("/admin/cost/stats    new(冷) : %s\n", sampleOf(coldCost))
	fmt.Printf("/admin/cost/stats    new(热) : %s\n", sampleOf(warmCost))
}

// callMeSummaryNoT / callCostStatsNoT 直接跑 handler（不打网络栈，只测 handler+DB 路径）。
func callMeSummaryNoT(t *testing.T, dev *Dev) int {
	t.Helper()
	rec := httptest.NewRecorder()
	dev.HandleMeSummary(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me/summary", nil))
	return rec.Code
}

func callCostStatsNoT(t *testing.T, admin *Admin) int {
	t.Helper()
	rec := httptest.NewRecorder()
	admin.HandleCostStats(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/stats", nil))
	return rec.Code
}
