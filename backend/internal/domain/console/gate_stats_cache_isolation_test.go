package console

// gate_stats_cache_isolation_test.go —— 跨用户 / 跨区间缓存隔离的反向用例（Gate 独立补的，不属于实现）。
//
// 这类用例的价值不在「跑绿」，而在「把隔离维度去掉后必须变红」：
//   * summaryCacheKey：把 meSummaryCacheKey 改成常量（丢掉用户 ID）→
//     TestGateCache_MeSummaryPerUserIsolated / TestGateCache_MeSummaryConcurrentCrossUser 变红；
//   * costStatsCacheKey：把 key 改成常量（丢掉 From/To）→
//     TestGateCache_CostStatsPerWindowIsolated 变红。
// 2026-11-11 实测三处变异都真红（见 notes/ 验收记录）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
)

// gateSummary 只取断言需要的字段（含缓存命中头）。
type gateSummary struct {
	TodayCredits     float64 `json:"TodayCredits"`
	MonthCredits     float64 `json:"MonthCredits"`
	InputTokens      int64   `json:"InputTokens"`
	CacheReadTokens  int64   `json:"CacheReadTokens"`
	YesterdayCredits float64 `json:"YesterdayCredits"`
}

func gateCallSummary(t *testing.T, dev *Dev) (gateSummary, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	dev.HandleMeSummary(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me/summary", nil))
	var body struct {
		Data gateSummary `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	return body.Data, rec.Header().Get("x-stats-cache")
}

// gateExpectRollup 声明一次聚合表三档查询（带延迟，用于并发场景）。
func gateExpectRollup(mock sqlmock.Sqlmock, userID int64, today, yesterday, month float64, in, cache int64, delay time.Duration) {
	q := mock.ExpectQuery(`FROM user_daily_stats`).WithArgs(userID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg())
	if delay > 0 {
		q = q.WillDelayFor(delay)
	}
	q.WillReturnRows(sqlmock.NewRows([]string{"today", "yesterday", "month", "input", "cache_read"}).
		AddRow(today, yesterday, month, in, cache))
}

func gateExpectWindow(mock sqlmock.Sqlmock, userID int64) {
	mock.ExpectQuery(`COALESCE\(sum\(credits_consumed\), 0\)`).WithArgs(
		userID, nil, nil, "", "", "", "", sqlmock.AnyArg(), sqlmock.AnyArg(),
	).WillReturnRows(sqlmock.NewRows([]string{"count", "credits", "tokens", "duration", "input", "cache_read",
		"busy_requests", "busy_tokens", "busy_duration"}).
		AddRow(int64(1), 0.1, int64(10), int64(0), int64(5), int64(2), int64(1), int64(10), int64(0)))
}

// TestGateCache_MeSummaryPerUserIsolated 顺序场景：A 先查、B 后查，
// 三个断言同时成立才算隔离到位 —— 各拿各的值、缓存里确实有两把键、再查 A 仍命中自己的值。
func TestGateCache_MeSummaryPerUserIsolated(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	var uid atomic.Int64
	dev := NewDev(billing.NewSqlStore(db), func(context.Context) (int64, bool) { return uid.Load(), true })
	dev.now = fixedNow

	gateExpectRollup(mock, 1, 11.5, 12.5, 113.5, 1200, 300, 0)
	gateExpectWindow(mock, 1)
	gateExpectRollup(mock, 2, 21.5, 22.5, 223.5, 2200, 700, 0)
	gateExpectWindow(mock, 2)

	uid.Store(1)
	a1, cacheA1 := gateCallSummary(t, dev)
	uid.Store(2)
	b1, cacheB1 := gateCallSummary(t, dev)

	if cacheA1 != "miss" || cacheB1 != "miss" {
		t.Fatalf("首轮都应是回源(miss)，实际 A=%q B=%q", cacheA1, cacheB1)
	}
	// ① 各拿各的：键少了 user_id 时，B 会直接吃 A 的值 → 这里立刻红。
	if b1.TodayCredits != 21.5 || b1.MonthCredits != 223.5 || b1.InputTokens != 2200 {
		t.Fatalf("B 拿到 %+v，期望自己的 21.5/223.5/2200（跨用户串数据）", b1)
	}
	if a1.TodayCredits != 11.5 || a1.MonthCredits != 113.5 || a1.InputTokens != 1200 {
		t.Fatalf("A 拿到 %+v，期望自己的 11.5/113.5/1200", a1)
	}
	// ② 结构断言：两个用户必须在缓存里占两把键（不含用户维度的键只有 1 把）。
	if keys := dev.summaryCache().Stats().Keys; keys != 2 {
		t.Fatalf("缓存键数=%d，期望 2（键里缺用户维度）", keys)
	}
	// ③ 再查 A：必须命中自己的旧值，且不再打聚合表（mock 已无剩余期望）。
	gateExpectWindow(mock, 1)
	uid.Store(1)
	a2, cacheA2 := gateCallSummary(t, dev)
	if cacheA2 != "hit" || a2 != a1 {
		t.Fatalf("A 第二轮 cache=%q 值=%+v，期望 hit 且等于 %+v", cacheA2, a2, a1)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock: %v（聚合表被重复查询 / 有请求没合并到缓存）", err)
	}
}

// TestGateCache_MeSummaryConcurrentCrossUser 并发场景：A 的回源故意慢 400ms，
// B 在 A 在途时进来 —— B 绝不能挂到 A 的 singleflight 上（那是跨用户串数据的另一条路径）。
func TestGateCache_MeSummaryConcurrentCrossUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	var uid atomic.Int64
	dev := NewDev(billing.NewSqlStore(db), func(context.Context) (int64, bool) { return uid.Load(), true })
	dev.now = fixedNow

	gateExpectRollup(mock, 1, 11.5, 12.5, 113.5, 1200, 300, 400*time.Millisecond)
	gateExpectWindow(mock, 1)
	gateExpectRollup(mock, 2, 21.5, 22.5, 223.5, 2200, 700, 0)
	gateExpectWindow(mock, 2)

	type res struct {
		data gateSummary
		err  error
	}
	var aRes res
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		uid.Store(1)
		d, _ := gateCallSummary(t, dev)
		aRes.data = d
	}()

	time.Sleep(80 * time.Millisecond) // 让 A 的回源先进入在途
	var bRes res
	uid.Store(2)
	start := time.Now()
	bRes.data, _ = gateCallSummary(t, dev)
	bElapsed := time.Since(start)
	wg.Wait()

	if bRes.data.TodayCredits != 21.5 || bRes.data.InputTokens != 2200 {
		t.Fatalf("并发下 B 拿到 %+v，期望自己的 21.5/2200（挂到了 A 的在途回源上）", bRes.data)
	}
	if aRes.data.TodayCredits != 11.5 || aRes.data.InputTokens != 1200 {
		t.Fatalf("并发下 A 拿到 %+v，期望自己的 11.5/1200", aRes.data)
	}
	if bElapsed >= 300*time.Millisecond {
		t.Fatalf("B 用时 %v，等到了 A 的慢回源（两者被合并成一次 DB 查询）", bElapsed)
	}
}

// TestGateCache_CostStatsPerWindowIsolated 区间隔离：不同 From 必须各自缓存，
// 键里少了区间就会把「上一个月」的结果当成「这个月」返回（Range 与金额一起错）。
func TestGateCache_CostStatsPerWindowIsolated(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	adm := NewAdmin(db, billing.NewSqlStore(db), nil, func(context.Context) (int64, bool) { return 1, true })
	adm.now = fixedNow

	row := func(cost float64) *sqlmock.Rows {
		return sqlmock.NewRows([]string{"entity_key", "channel_id", "channel_name", "channel_model_id",
			"external_model_id", "internal_model_id", "external_model_name",
			"cost_credits", "sale_credits", "requests",
			"input_tokens", "cache_read_tokens", "output_tokens", "snapshot_names"}).
			AddRow("cm:1", int64(1), "ch", int64(1), int64(1), "int-m", "ext-m",
				cost, cost*2, int64(10), int64(100), int64(10), int64(20), `["ext-m"]`)
	}
	mock.ExpectQuery(`FROM billing_records b`).WillReturnRows(row(1.5))
	mock.ExpectQuery(`FROM billing_records b`).WillReturnRows(row(9.5))

	get := func(q string) (float64, string, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/stats"+q, nil)
		adm.HandleCostStats(rec, req)
		var body struct {
			Data costStatsResponse `json:"Data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
		}
		if len(body.Data.Rows) != 1 {
			t.Fatalf("行数=%d，期望 1；body=%s", len(body.Data.Rows), rec.Body.String())
		}
		return body.Data.Rows[0].CostCredits, body.Data.Range.From, body.Data.Range.To
	}

	// 默认窗口（近 30 天，基准 2026-09-27 → 2026-08-28..2026-09-27）
	c1, from1, to1 := get("")
	if c1 != 1.5 || from1 != "2026-08-28" || to1 != "2026-09-27" {
		t.Fatalf("默认窗口拿到 cost=%v range=%s..%s", c1, from1, to1)
	}
	// 显式 From=2026-09-01：键不同 → 必须另回源一次，拿回自己的 9.5
	c2, from2, _ := get("?From=2026-09-01")
	if from2 != "2026-09-01" {
		t.Fatalf("显式区间拿到 from=%s，期望 2026-09-01（返回了别的区间缓存）", from2)
	}
	if c2 != 9.5 {
		t.Fatalf("显式区间拿到 cost=%v，期望 9.5（返回了别的区间缓存）", c2)
	}
	// 再打默认窗口：必须命中缓存（mock 只剩 2 次查询，多打一次就会报未被期望）
	c3, from3, _ := get("")
	if c3 != 1.5 || from3 != "2026-08-28" {
		t.Fatalf("默认窗口第二次拿到 cost=%v from=%s，期望 1.5 / 2026-08-28", c3, from3)
	}
	if keys := adm.costStatsCache().Stats().Keys; keys != 2 {
		t.Fatalf("缓存键数=%d，期望 2（键里缺区间维度）", keys)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock: %v", err)
	}
}
