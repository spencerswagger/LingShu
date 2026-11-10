package console

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/router"
)

// assertRange 断言时间区间端点。
func assertRange(t *testing.T, name string, got timeRange, from, to time.Time) {
	t.Helper()
	if !got.From.Equal(from) || !got.To.Equal(to) {
		t.Fatalf("%s range = [%s, %s], want [%s, %s]", name,
			got.From.Format(time.RFC3339), got.To.Format(time.RFC3339),
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
}

// TestSummaryRanges_Shanghai 断言上海口径四区间边界，覆盖月首与跨月昨日。
func TestSummaryRanges_Shanghai(t *testing.T) {
	loc := cnLocation()
	// 2026-03-01 为月首；其前一自然日为 2026-02-28（跨月，2026 非闰年）。
	now := time.Date(2026, 3, 1, 10, 30, 0, 0, loc)

	today, yesterday, month, window := summaryRanges(now, loc)
	assertRange(t, "today", today,
		time.Date(2026, 3, 1, 0, 0, 0, 0, loc), time.Date(2026, 3, 1, 23, 59, 59, 0, loc))
	assertRange(t, "yesterday", yesterday,
		time.Date(2026, 2, 28, 0, 0, 0, 0, loc), time.Date(2026, 2, 28, 23, 59, 59, 0, loc))
	assertRange(t, "month", month,
		time.Date(2026, 3, 1, 0, 0, 0, 0, loc), time.Date(2026, 3, 1, 23, 59, 59, 0, loc))
	assertRange(t, "window", window, now.Add(-time.Minute), now)

	// 另取月中普通日，覆盖 month 起点固定为当月初。
	now2 := time.Date(2026, 3, 15, 8, 0, 0, 0, loc)
	t2, y2, m2, w2 := summaryRanges(now2, loc)
	assertRange(t, "today2", t2,
		time.Date(2026, 3, 15, 0, 0, 0, 0, loc), time.Date(2026, 3, 15, 23, 59, 59, 0, loc))
	assertRange(t, "yesterday2", y2,
		time.Date(2026, 3, 14, 0, 0, 0, 0, loc), time.Date(2026, 3, 14, 23, 59, 59, 0, loc))
	assertRange(t, "month2", m2,
		time.Date(2026, 3, 1, 0, 0, 0, 0, loc), time.Date(2026, 3, 15, 23, 59, 59, 0, loc))
	assertRange(t, "window2", w2, now2.Add(-time.Minute), now2)
}

// stubTokenCounter 令牌计数桩（固定返回 count）。
type stubTokenCounter struct{ count int64 }

func (s stubTokenCounter) CountByUser(context.Context, int64) (int64, error) {
	return s.count, nil
}

// TestHandleMeSummary 断言四段 Summarize 调用顺序与取值、RPM/TPM、令牌数、在线会话数。
func TestHandleMeSummary(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	const summarySQL = `COALESCE(sum(credits_consumed), 0)`
	// 列顺序与 Summarize 的 SELECT 一致：末尾三列是忙时口径（成功且带耗时的请求数、token、耗时之和）。
	rows := func(requests int64, credits float64, tokens, input, cacheRead int64) *sqlmock.Rows {
		return sqlmock.NewRows([]string{"count", "credits", "tokens", "duration", "input", "cache_read",
			"busy_requests", "busy_tokens", "busy_duration"}).
			AddRow(requests, credits, tokens, int64(0), input, cacheRead, requests, tokens, int64(0))
	}
	// 三档消耗一次查询命中日级预聚合（user_daily_stats），行数=天数，与明细表行数无关；
	// 时间参数用 AnyArg（日界逻辑已由 TestSummaryRanges_Shanghai 覆盖）。
	rollup := sqlmock.NewRows([]string{"today", "yesterday", "month", "input", "cache_read"}).
		AddRow(1.5, 2.5, 3.5, int64(120), int64(30))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM user_daily_stats`)).WithArgs(
		int64(1), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
	).WillReturnRows(rollup)
	// 近 1 分钟吞吐窗口仍查明细（秒级口径不做缓存）：不带 status 过滤。
	argsAll := []driver.Value{int64(1), nil, nil, "", "", "", "", sqlmock.AnyArg(), sqlmock.AnyArg()}
	mock.ExpectQuery(regexp.QuoteMeta(summarySQL)).WithArgs(argsAll...).WillReturnRows(rows(50, 0.5, 500, 200, 50))

	dev := NewDev(billing.NewSqlStore(db), func(context.Context) (int64, bool) { return 1, true })
	dev.now = fixedNow
	reg := router.NewSessionRegistry(fixedNow)
	dev.SetSessions(reg)
	dev.SetTokenCounter(stubTokenCounter{count: 5})

	// 本人未关闭未过期（计入）；本人已关闭（不计）；他人未关闭未过期（不计）。
	attachTestSession(t, reg, "own-active", 1, 10, 100, true)
	attachTestSession(t, reg, "own-closed", 1, 11, 100, true)
	if !reg.Close("own-closed") {
		t.Fatalf("close own-closed failed")
	}
	attachTestSession(t, reg, "other-active", 2, 20, 101, true)

	rec := httptest.NewRecorder()
	dev.HandleMeSummary(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me/summary", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code int `json:"Code"`
		Data struct {
			TokenCount       int64   `json:"TokenCount"`
			SessionCount     int64   `json:"SessionCount"`
			TodayCredits     float64 `json:"TodayCredits"`
			YesterdayCredits float64 `json:"YesterdayCredits"`
			MonthCredits     float64 `json:"MonthCredits"`
			RPM              float64 `json:"RPM"`
			TPM              float64 `json:"TPM"`
			InputTokens      int64   `json:"InputTokens"`
			CacheReadTokens  int64   `json:"CacheReadTokens"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d := body.Data
	if d.TokenCount != 5 {
		t.Fatalf("TokenCount=%d, want 5", d.TokenCount)
	}
	if d.SessionCount != 1 {
		t.Fatalf("SessionCount=%d, want 1", d.SessionCount)
	}
	if d.TodayCredits != 1.5 || d.YesterdayCredits != 2.5 || d.MonthCredits != 3.5 {
		t.Fatalf("credits=%v/%v/%v, want 1.5/2.5/3.5", d.TodayCredits, d.YesterdayCredits, d.MonthCredits)
	}
	if d.RPM != 50 { // 50 requests / 1min（短窗口：墙钟窗口速率）
		t.Fatalf("RPM=%v, want 50", d.RPM)
	}
	if d.TPM != 500 { // 500 tokens / 1min
		t.Fatalf("TPM=%v, want 500", d.TPM)
	}
	// 本月缓存口径：取自 month 那一次 Summarize（input=120、cache_read=30）。
	if d.InputTokens != 120 || d.CacheReadTokens != 30 {
		t.Fatalf("cache=%d/%d, want 120/30", d.InputTokens, d.CacheReadTokens)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
