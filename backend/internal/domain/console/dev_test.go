package console

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
)

// newTestDev 构造注入固定 userID 的 Dev（userIDFrom 忽略上下文，直接返回 id）。
func newTestDev(db *sql.DB, userID int64) *Dev {
	return NewDev(billing.NewSqlStore(db), func(ctx context.Context) (int64, bool) { return userID, true })
}

// TestHandleDevListBillings_TimeRange 断言 /api/v1/dev/billings 解析 From/To（RFC3339）
// 并按 where 参数位置透传 RecordFilter 时间参数；缺省时对应参数为 nil。
func TestHandleDevListBillings_TimeRange(t *testing.T) {
	fromStr := "2026-09-01T00:00:00+08:00"
	toStr := "2026-09-01T23:59:59+08:00"
	from, err := time.Parse(time.RFC3339, fromStr)
	if err != nil {
		t.Fatalf("parse from: %v", err)
	}
	to, err := time.Parse(time.RFC3339, toStr)
	if err != nil {
		t.Fatalf("parse to: %v", err)
	}

	cases := []struct {
		name    string
		from    string
		to      string
		fromArg any
		toArg   any
	}{
		{"带 From/To", fromStr, toStr, from, to},
		{"不带 From/To", "", "", nil, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()

			dev := newTestDev(db, 7)

			// recordWhere 参数顺序：$1=user_id $2=channel_key_id $3=token_id
			// $4=session_id $5=model $6=pricing_mode $7=status $8=from $9=to
			countArgs := []driver.Value{int64(7), nil, nil, "", "", "", "", c.fromArg, c.toArg}
			mock.ExpectQuery(`SELECT count\(\*\) FROM billing_records .*call_time >= \$8.*call_time <= \$9.*`).
				WithArgs(countArgs...).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
			listArgs := append(append([]driver.Value{}, countArgs...), 20, 0)
			mock.ExpectQuery(`SELECT .* FROM billing_records .*call_time >= \$8.*call_time <= \$9.*ORDER BY id DESC LIMIT \$10 OFFSET \$11`).
				WithArgs(listArgs...).
				WillReturnRows(sqlmock.NewRows([]string{"id"}))

			qs := url.Values{}
			if c.from != "" {
				qs.Set("From", c.from)
			}
			if c.to != "" {
				qs.Set("To", c.to)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/dev/billings?"+qs.Encode(), nil)
			rec := httptest.NewRecorder()
			dev.HandleListBillings(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				Code int `json:"Code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if body.Code != 0 {
				t.Fatalf("code=%d, want 0", body.Code)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("mock expectations: %v", err)
			}
		})
	}
}

// TestHandleDevListBillings_InvalidTimeRange 断言非法 From/To 格式返回 400（不发查询）。
func TestHandleDevListBillings_InvalidTimeRange(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	dev := newTestDev(db, 7)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dev/billings?From=not-a-time", nil)
	rec := httptest.NewRecorder()
	dev.HandleListBillings(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
