package channelprotocol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// errStatsQueryRe 匹配协议级错误统计汇总查询（aggregate by category）。
var errStatsQueryRe = regexp.QuoteMeta(`SELECT category, COALESCE(SUM(count), 0) AS total
		 FROM protocol_error_stats
		 WHERE protocol_id=$1 AND day >= $2 AND day <= $3
		 GROUP BY category
		 ORDER BY category ASC`)

func errStatsRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"category", "total"}).
		AddRow("code:context_length_exceeded", int64(2)).
		AddRow("status:422", int64(3))
}

// Step 11：Store.ErrorStats 按 category 聚合、跨日累加。
func TestErrorStatsStore(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := NewStore(db)

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(errStatsQueryRe).
		WithArgs(int64(9), "2026-09-01", "2026-09-30").
		WillReturnRows(errStatsRows())

	stats, err := store.ErrorStats(context.Background(), 9, from, to)
	if err != nil {
		t.Fatalf("error stats: %v", err)
	}
	if len(stats) != 2 || stats[0].Category != "code:context_length_exceeded" || stats[0].Count != 2 ||
		stats[1].Category != "status:422" || stats[1].Count != 3 {
		t.Fatalf("stats 不符: %+v", stats)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// Step 10：Store.IncrProtocolErrorStat 同步 upsert（count = count + 1）。
func TestErrorStatsIncr(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := NewStore(db)

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO protocol_error_stats(protocol_id, category, day, count)`)).
		WithArgs(int64(9), "status:422", "2026-09-18").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.IncrProtocolErrorStat(context.Background(), 9, "status:422",
		time.Date(2026, 9, 18, 15, 4, 5, 0, time.UTC)); err != nil {
		t.Fatalf("incr error stat: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// Step 11：Handler.HandleErrorStats 端点返回按 category 聚合的计数。
func TestErrorStatsHandler(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	svc := NewService(NewStore(db))
	svc.SetLogger(nil)
	h := NewHandler(svc)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cpCols + ` FROM channel_protocols WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(9)).
		WillReturnRows(cpRow(9, "p", "", "", false, SourceActive))
	mock.ExpectQuery(errStatsQueryRe).
		WithArgs(int64(9), "2026-09-01", "2026-09-30").
		WillReturnRows(errStatsRows())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/channel-protocols/9/error-stats?from=2026-09-01&to=2026-09-30", nil)
	req.SetPathValue("id", "9")
	rec := httptest.NewRecorder()
	h.HandleErrorStats(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code int `json:"Code"`
		Data struct {
			From  string      `json:"From"`
			To    string      `json:"To"`
			Stats []ErrorStat `json:"Stats"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if body.Code != 0 || body.Data.From != "2026-09-01" || body.Data.To != "2026-09-30" ||
		len(body.Data.Stats) != 2 || body.Data.Stats[0].Category != "code:context_length_exceeded" {
		t.Fatalf("response 不符: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
