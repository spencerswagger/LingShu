package console

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
			// 列表默认按 request_id 归并（Group 缺省开启）：走 CTE grp，参数位与逐条查询一致。
			mock.ExpectQuery(`SELECT count\(\*\) FROM \(\s*SELECT 1 FROM billing_records br LEFT JOIN call_logs cl .*call_time >= \$8.*call_time <= \$9.*GROUP BY \(br.user_id::text \|\| ':' \|\| COALESCE`).
				WithArgs(countArgs...).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
			listArgs := append(append([]driver.Value{}, countArgs...), 20, 0)
			mock.ExpectQuery(`WITH grp AS \(.*call_time >= \$8.*call_time <= \$9.*grp.rid, grp.cnt.*LIMIT \$10 OFFSET \$11`).
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

// TestHandleDevListBillings_ChannelName 是 Owl PR #63 建议 3 的回归：
// 开发者端列表要展示**渠道显示名**（非密钥，属白名单内），且与管理端共用同一份回查实现。
func TestHandleDevListBillings_ChannelName(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	dev := newTestDev(db, 7)
	dev.SetDB(db) // 渠道名回查走 h.db

	rows := sqlmock.NewRows([]string{
		"id", "billing_id", "user_id", "pricing_mode", "token_id", "external_model_name",
		"auto_model_name", "internal_model_id", "channel_key_id", "session_id", "session_name", "call_time",
		"tokens", "rates", "coefficients", "r_value", "raw_total", "credits_consumed", "cost_credits",
		"balance_before", "balance_after", "status", "error_message", "duration_ms", "first_token_ms",
		"rid", "cnt",
	}).AddRow(
		int64(1), "b1", int64(7), "sale", nil, "gpt-4", nil, "gpt-4-int", int64(99), "", "",
		time.Now(), `{}`, `{}`, `{"time":1,"context":1}`, int64(10000),
		nil, float64(0.5), nil, nil, nil, "completed", nil, nil, nil, "req-1", int64(1),
	)

	mock.ExpectQuery(`SELECT count\(\*\) FROM \(\s*SELECT 1 FROM billing_records br LEFT JOIN call_logs cl .*GROUP BY \(br.user_id::text \|\| ':' \|\| COALESCE`).
		WithArgs(int64(7), nil, nil, "", "", "", "", nil, nil).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))
	mock.ExpectQuery(`WITH grp AS \(.*grp.rid, grp.cnt.*LIMIT \$10 OFFSET \$11`).
		WithArgs(int64(7), nil, nil, "", "", "", "", nil, nil, 20, 0).
		WillReturnRows(rows)
	// 渠道显示名回查（channel_keys JOIN channels）：dev 列表据此填 ChannelName。
	mock.ExpectQuery(`SELECT k.id, c.name FROM channel_keys k JOIN channels c ON c.id = k.channel_id`).
		WithArgs().
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).AddRow(int64(99), "azure-东区"))

	rec := httptest.NewRecorder()
	dev.HandleListBillings(rec, httptest.NewRequest(http.MethodGet, "/api/v1/dev/billings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			List []map[string]any `json:"List"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data.List) != 1 {
		t.Fatalf("list len=%d, want 1", len(body.Data.List))
	}
	if got := body.Data.List[0]["ChannelName"]; got != "azure-东区" {
		t.Fatalf("ChannelName=%v, want azure-东区", got)
	}
	// 仍不得出现密钥维度/内部模型 ID（白名单口径不变）。
	raw := rec.Body.String()
	for _, banned := range []string{"ChannelKeyID", "KeyName", "InternalModelID", "gpt-4-int"} {
		if strings.Contains(raw, banned) {
			t.Fatalf("dev 列表泄漏 %q: %s", banned, raw)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
