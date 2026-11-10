package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
)

// meSummaryData 只取断言需要的字段。
type meSummaryData struct {
	TodayCredits     float64 `json:"TodayCredits"`
	YesterdayCredits float64 `json:"YesterdayCredits"`
	MonthCredits     float64 `json:"MonthCredits"`
	InputTokens      int64   `json:"InputTokens"`
	CacheReadTokens  int64   `json:"CacheReadTokens"`
}

// callMeSummary 直接调用 handler（鉴权用户由调用方事先装好的 userIDFrom 桩决定）。
func callMeSummary(t *testing.T, dev *Dev, userID int64) (meSummaryData, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	dev.HandleMeSummary(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me/summary", nil))
	var body struct {
		Data meSummaryData `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal(uid=%d): %v; body=%s", userID, err, rec.Body.String())
	}
	return body.Data, rec.Code
}

// expectRollup 声明一次「聚合表三档查询」的期望（按用户返回不同金额）。
func expectRollup(mock sqlmock.Sqlmock, userID int64, today, yesterday, month float64, monthInput, monthCacheRead int64) {
	mock.ExpectQuery(`FROM user_daily_stats`).WithArgs(
		userID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
	).WillReturnRows(sqlmock.NewRows([]string{"today", "yesterday", "month", "input", "cache_read"}).
		AddRow(today, yesterday, month, monthInput, monthCacheRead))
}

// expectWindow 声明一次「近 1 分钟吞吐窗口」的期望（秒级口径，每次请求都查）。
func expectWindow(mock sqlmock.Sqlmock, userID int64) {
	mock.ExpectQuery(`COALESCE\(sum\(credits_consumed\), 0\)`).WithArgs(
		userID, nil, nil, "", "", "", "", sqlmock.AnyArg(), sqlmock.AnyArg(),
	).WillReturnRows(sqlmock.NewRows([]string{"count", "credits", "tokens", "duration", "input", "cache_read",
		"busy_requests", "busy_tokens", "busy_duration"}).
		AddRow(int64(3), 0.1, int64(30), int64(0), int64(10), int64(5), int64(3), int64(30), int64(0)))
}

// 反向用例：A 的缓存不能被 B 读到。
// 两个用户在同一进程、同一份缓存里，键都必须带用户维度；且第二轮请求必须命中缓存（不再查聚合表）。
func TestDevSummary_CacheIsolatesUsers(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	// 鉴权用户每次请求前切换（顺序调用，无并发）。
	var currentUserID atomic.Int64
	dev := NewDev(billing.NewSqlStore(db), func(context.Context) (int64, bool) {
		return currentUserID.Load(), true
	})
	dev.now = fixedNow
	summary := func(uid int64) (meSummaryData, int) {
		currentUserID.Store(uid)
		return callMeSummary(t, dev, uid)
	}

	// 第一轮：A(用户 1) 与 B(用户 2) 各自回源一次。
	expectRollup(mock, 1, 11.5, 12.5, 113.5, 1200, 300)
	expectWindow(mock, 1)
	expectRollup(mock, 2, 21.5, 22.5, 223.5, 2200, 700)
	expectWindow(mock, 2)

	a1, code := summary(1)
	if code != http.StatusOK {
		t.Fatalf("A 首次 status=%d", code)
	}
	b1, code := summary(2)
	if code != http.StatusOK {
		t.Fatalf("B 首次 status=%d", code)
	}
	if a1.TodayCredits != 11.5 || a1.MonthCredits != 113.5 || a1.InputTokens != 1200 {
		t.Fatalf("A 首次拿到 %+v，期望自己的 11.5 / 113.5 / 1200", a1)
	}
	if b1.TodayCredits != 21.5 || b1.MonthCredits != 223.5 || b1.InputTokens != 2200 {
		t.Fatalf("B 首次拿到 %+v，期望自己的 21.5 / 223.5 / 2200", b1)
	}

	// 第二轮：只声明窗口查询（聚合表不应再被查 = 命中各自缓存）。
	expectWindow(mock, 1)
	expectWindow(mock, 2)
	a2, _ := summary(1)
	b2, _ := summary(2)
	if a2 != a1 {
		t.Fatalf("A 第二轮 %+v != 第一轮 %+v（跨用户串数据或缓存失效异常）", a2, a1)
	}
	if b2 != b1 {
		t.Fatalf("B 第二轮 %+v != 第一轮 %+v（跨用户串数据或缓存失效异常）", b2, b1)
	}
	if a2 == b2 {
		t.Fatalf("A 与 B 读到同一份摘要 %+v —— 缓存键没有按用户隔离", a2)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v（聚合表被重复查询说明缓存未命中）", err)
	}
}

// 并发 100 个请求同一用户：聚合表只落 1 次 DB（其余请求合并到同一次回源）。
func TestDevSummary_ConcurrentSameUserSingleDBQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	dev := NewDev(billing.NewSqlStore(db), func(context.Context) (int64, bool) { return 7, true })
	dev.now = fixedNow

	expectRollup(mock, 7, 1.0, 2.0, 3.0, 100, 20)
	const n = 100
	for i := 0; i < n; i++ {
		expectWindow(mock, 7) // 秒级窗口不缓存，每请求一次
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, code := callMeSummary(t, dev, 7); code != http.StatusOK {
				t.Errorf("status=%d", code)
			}
		}()
	}
	wg.Wait()

	// 只声明了 1 次聚合查询：若并发里有人没合并而直接回源，sqlmock 会报「调用未被期望」→ handler 500 → 上面的断言失败。
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
	st := dev.summaryCache().Stats()
	if st.Loads != 1 {
		t.Fatalf("聚合表回源 %d 次，期望 1（并发同键必须合并）", st.Loads)
	}
}
