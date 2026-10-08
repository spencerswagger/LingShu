package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
)

func TestCreditValueFromRaw(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
		want float64
		err  bool
	}{
		{"整数 10000", json.RawMessage(`10000`), 0.01, false},
		{"字符串 100000", json.RawMessage(`"100000"`), 0.1, false},
		{"非法内容", json.RawMessage(`"abc"`), 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := creditValueFromRaw(c.raw)
			if c.err {
				if err == nil {
					t.Fatalf("期望返回错误，实际无错误，got=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("creditValueFromRaw 返回错误：%v", err)
			}
			if got != c.want {
				t.Fatalf("期望 %v，实际 %v", c.want, got)
			}
		})
	}
}

// TestToListItem_KeySession 断言账单列表项携带渠道密钥与会话维度。
func TestToListItem_KeySession(t *testing.T) {
	rec := &billing.Record{
		BillingID:       "bill-20260102-000001",
		UserID:          7,
		ExternalModel:   "gpt-4",
		InternalModelID: "gpt-4-internal",
		ChannelKeyID:    42,
		SessionID:       "sess-abc",
	}
	item := toListItem(rec)
	if item.ChannelKeyID != 42 {
		t.Fatalf("channel_key_id=%d, want 42", item.ChannelKeyID)
	}
	if item.SessionID != "sess-abc" {
		t.Fatalf("session_id=%q, want sess-abc", item.SessionID)
	}
}

// TestDecorateList_KeyName 断言 decorateList 反查密钥名/渠道名，并对缺失密钥兜底 #<id>。
func TestDecorateList_KeyName(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	admin := NewAdmin(db, billing.NewSqlStore(db), nil, nil)

	recs := []billing.Record{
		{BillingID: "b1", UserID: 1, ChannelKeyID: 1},
		{BillingID: "b2", UserID: 1, ChannelKeyID: 99},
	}
	list := make([]billingListItem, 0, len(recs))
	for i := range recs {
		list = append(list, toListItem(&recs[i]))
	}

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, username, nickname FROM users WHERE id = ANY('{1}'::bigint[])`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username", "nickname"}).AddRow(int64(1), "u1", "昵称A"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, channel_id, name FROM channel_keys WHERE id = ANY('{1,99}'::bigint[])`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name"}).
			AddRow(int64(1), int64(10), "主密钥"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, name FROM channels WHERE id = ANY('{10}'::bigint[])`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).AddRow(int64(10), "渠道A"))

	admin.decorateList(context.Background(), list, recs)

	if list[0].KeyName != "主密钥" {
		t.Fatalf("key_name=%q, want 主密钥", list[0].KeyName)
	}
	if list[0].ChannelName != "渠道A" {
		t.Fatalf("channel_name=%q, want 渠道A", list[0].ChannelName)
	}
	if list[1].KeyName != "#99" {
		t.Fatalf("兜底 key_name=%q, want #99", list[1].KeyName)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestChannelNameByKeyID 断言由密钥 ID 反查渠道名（join channel_keys）。
func TestChannelNameByKeyID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	admin := NewAdmin(db, billing.NewSqlStore(db), nil, nil)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT c.name FROM channels c JOIN channel_keys k ON k.channel_id = c.id WHERE k.id = $1`)).
		WithArgs(int64(7)).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("渠道B"))

	if got := admin.channelNameByKeyID(context.Background(), 7); got != "渠道B" {
		t.Fatalf("channel_name=%q, want 渠道B", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT c.name FROM channels c JOIN channel_keys k ON k.channel_id = c.id WHERE k.id = $1`)).
		WithArgs(int64(999)).WillReturnError(sqlmock.ErrCancelled)
	if got := admin.channelNameByKeyID(context.Background(), 999); got != "" {
		t.Fatalf("缺失密钥应返回空串, got %q", got)
	}
}

// TestHandleListBillings_FilterParams 断言 /api/v1/admin/billings 解析
// channel_key_id / session_id 查询参数并透传 RecordFilter 过滤 SQL。
func TestHandleListBillings_FilterParams(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	admin := NewAdmin(db, billing.NewSqlStore(db), nil, nil)

	rows := sqlmock.NewRows([]string{
		"id", "billing_id", "user_id", "pricing_mode", "token_id", "external_model_name",
		"internal_model_id", "channel_key_id", "session_id", "session_name", "call_time", "tokens", "rates",
		"coefficients", "r_value", "raw_total", "credits_consumed", "cost_credits",
		"balance_before", "balance_after", "status", "error_message", "duration_ms",
		"first_token_ms",
	}).AddRow(
		int64(1), "b1", int64(1), "sale", nil, "ext", "int", int64(3), "sess-1", "",
		time.Now(), `{}`, `{}`, `{"time":1,"context":1}`, int64(10000),
		nil, nil, nil, nil, nil, "completed", nil, nil, nil,
	)

	mock.ExpectQuery(`SELECT count\(\*\) FROM billing_records .*channel_key_id = \$2.*token_id = \$3.*session_id = \$4.*`).
		WithArgs(nil, int64(3), nil, "sess-1", "", "", "", nil, nil).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))
	mock.ExpectQuery(`SELECT .* FROM billing_records .*channel_key_id = \$2.*token_id = \$3.*session_id = \$4.*ORDER BY id DESC LIMIT \$10 OFFSET \$11`).
		WithArgs(nil, int64(3), nil, "sess-1", "", "", "", nil, nil, 20, 0).
		WillReturnRows(rows)
	// decorateList 反查（空结果：用户名缺失、密钥缺失触发 #<id> 兜底）。
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, username, nickname FROM users WHERE id = ANY('{1}'::bigint[])`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username", "nickname"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, channel_id, name FROM channel_keys WHERE id = ANY('{3}'::bigint[])`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name"}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/billings?ChannelKeyID=3&SessionID=sess-1", nil)
	rec := httptest.NewRecorder()
	admin.HandleListBillings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code int `json:"Code"`
		Data struct {
			Total int64 `json:"Total"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Code != 0 || body.Data.Total != 1 {
		t.Fatalf("code=%d total=%d, want 0/1", body.Code, body.Data.Total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestStatsRangeFromQuery_ShanghaiDayBoundary 断言统计区间日界统一按 Asia/Shanghai 归一，
// 不随请求自带时区偏移截断。
func TestStatsRangeFromQuery_ShanghaiDayBoundary(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}

	// from 带 -05:00 偏移、to 带 Z（UTC）：各自换算为上海日历日后取整天边界。
	q := url.Values{
		"From": {"2026-09-01T08:30:00-05:00"}, // 上海 2026-09-01 21:30
		"To":   {"2026-09-03T20:00:00Z"},      // 上海 2026-09-04 04:00（跨日）
	}
	from, to, err := statsRangeFromQuery(q, time.Now())
	if err != nil {
		t.Fatalf("statsRangeFromQuery: %v", err)
	}
	wantFrom := time.Date(2026, 9, 1, 0, 0, 0, 0, loc)
	wantTo := time.Date(2026, 9, 4, 23, 59, 59, 0, loc)
	if !from.Equal(wantFrom) || !to.Equal(wantTo) {
		t.Fatalf("from=%v to=%v, want %v/%v", from, to, wantFrom, wantTo)
	}
	if from.Location().String() != "Asia/Shanghai" {
		t.Fatalf("from location=%v, want Asia/Shanghai", from.Location())
	}

	// 缺省近 30 天：以 now 所在上海日历日为准。
	now := time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC) // 上海 2026-09-03 23:00
	from2, to2, err := statsRangeFromQuery(url.Values{}, now)
	if err != nil {
		t.Fatalf("statsRangeFromQuery default: %v", err)
	}
	if !from2.Equal(time.Date(2026, 8, 5, 0, 0, 0, 0, loc)) ||
		!to2.Equal(time.Date(2026, 9, 3, 23, 59, 59, 0, loc)) {
		t.Fatalf("default from=%v to=%v", from2, to2)
	}

	// 边界校验：from 晚于 to 报错。
	if _, _, err := statsRangeFromQuery(url.Values{
		"From": {"2026-09-05T00:00:00+08:00"},
		"To":   {"2026-09-01T00:00:00+08:00"},
	}, now); err == nil {
		t.Fatalf("from 晚于 to 应报错")
	}
}
