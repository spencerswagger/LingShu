package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
)

// testUserIDFrom 供写接口测试注入的操作者身份（固定用户 1）。
func testUserIDFrom(context.Context) (int64, bool) { return 1, true }

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
		"auto_model_name", "internal_model_id", "channel_key_id", "session_id", "session_name", "call_time", "tokens", "rates",
		"coefficients", "r_value", "raw_total", "credits_consumed", "cost_credits",
		"balance_before", "balance_after", "status", "error_message", "duration_ms",
		"first_token_ms",
	}).AddRow(
		int64(1), "b1", int64(1), "sale", nil, "ext", nil, "int", int64(3), "sess-1", "",
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

// expectConfigRow 登记一次 sys_configs 单键读取的期望返回（val=nil 表示返回 sql.ErrNoRows）。
func expectConfigRow(mock sqlmock.Sqlmock, key string, val []byte) {
	if val == nil {
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key=$1`)).
			WithArgs(key).WillReturnError(sql.ErrNoRows)
		return
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key=$1`)).
		WithArgs(key).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(val))
}

// systemConfigPayload 从 GET /admin/configs/system 响应中提取三组配置原文。
func systemConfigPayload(t *testing.T, rec *httptest.ResponseRecorder) (billing, session, whitelist json.RawMessage) {
	t.Helper()
	var body struct {
		Data map[string]json.RawMessage `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	for _, key := range []string{"Billing", "SessionExpiration", "UpstreamResponseHeaderWhitelist"} {
		if _, ok := body.Data[key]; !ok {
			t.Fatalf("响应缺少 %s 键: %s", key, rec.Body.String())
		}
	}
	return body.Data["Billing"], body.Data["SessionExpiration"], body.Data["UpstreamResponseHeaderWhitelist"]
}

// costSampleFromBilling 从 Billing 原文中解析路由成本比较样值。
func costSampleFromBilling(t *testing.T, billing json.RawMessage) (input, cacheRead, output, reasoning int64) {
	t.Helper()
	var b struct {
		CostSample struct {
			Input     int64 `json:"input"`
			CacheRead int64 `json:"cache_read"`
			Output    int64 `json:"output"`
			Reasoning int64 `json:"reasoning"`
		} `json:"CostSample"`
	}
	if err := json.Unmarshal(billing, &b); err != nil {
		t.Fatalf("unmarshal Billing: %v", err)
	}
	return b.CostSample.Input, b.CostSample.CacheRead, b.CostSample.Output, b.CostSample.Reasoning
}

// sessionExpirationView 会话过期配置的断言视图。
type sessionExpirationView struct {
	DefaultTTLMinutes int `json:"default_ttl_minutes"`
	ContextTiers      []struct {
		MaxTokens  int `json:"max_tokens"`
		TTLMinutes int `json:"ttl_minutes"`
	} `json:"context_tiers"`
	TurnTiers []struct {
		MaxTurns   int `json:"max_turns"`
		TTLMinutes int `json:"ttl_minutes"`
	} `json:"turn_tiers"`
}

// TestHandleGetSystemConfig_ReturnsAllGroups 断言统一读取接口返回三组配置，且计费含成本样值。
func TestHandleGetSystemConfig_ReturnsAllGroups(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, nil)

	expectConfigRow(mock, "billing.r", []byte(`10000`))
	expectConfigRow(mock, "billing.cny_rate", []byte(`6.8`))
	expectConfigRow(mock, "billing.context_tiers", []byte(`[]`))
	expectConfigRow(mock, "billing.time_config", []byte(`{"timezone":"Asia/Shanghai","default_coeff":1.0}`))
	expectConfigRow(mock, "routing.cost_sample",
		[]byte(`{"input":10000,"cache_read":9000,"output":1000,"reasoning":500}`))
	expectConfigRow(mock, "session.expiration",
		[]byte(`{"default_ttl_minutes":7,"context_tiers":[{"max_tokens":1000,"ttl_minutes":3}],"turn_tiers":[{"max_turns":3,"ttl_minutes":9}]}`))
	expectConfigRow(mock, "gateway.upstream_response_header_whitelist", []byte(`["traceparent"]`))

	rec := httptest.NewRecorder()
	admin.HandleGetSystemConfig(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/configs/system", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	billingRaw, sessionRaw, whitelistRaw := systemConfigPayload(t, rec)
	in, cr, out, re := costSampleFromBilling(t, billingRaw)
	if in != 10000 || cr != 9000 || out != 1000 || re != 500 {
		t.Fatalf("CostSample=%d/%d/%d/%d, want 10000/9000/1000/500", in, cr, out, re)
	}
	var se sessionExpirationView
	if err := json.Unmarshal(sessionRaw, &se); err != nil {
		t.Fatalf("unmarshal SessionExpiration: %v", err)
	}
	if se.DefaultTTLMinutes != 7 {
		t.Fatalf("default_ttl_minutes=%d, want 7", se.DefaultTTLMinutes)
	}
	var wl []string
	if err := json.Unmarshal(whitelistRaw, &wl); err != nil {
		t.Fatalf("unmarshal whitelist: %v", err)
	}
	if len(wl) != 1 || wl[0] != "traceparent" {
		t.Fatalf("whitelist=%v, want [traceparent]", wl)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestHandleGetSystemConfig_Fallbacks 断言样值 / 会话配置缺失时回退内置默认，白名单缺失回退空数组。
func TestHandleGetSystemConfig_Fallbacks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, nil)

	expectConfigRow(mock, "billing.r", []byte(`10000`))
	expectConfigRow(mock, "billing.cny_rate", []byte(`6.8`))
	expectConfigRow(mock, "billing.context_tiers", []byte(`[]`))
	expectConfigRow(mock, "billing.time_config", []byte(`{"timezone":"Asia/Shanghai","default_coeff":1.0}`))
	expectConfigRow(mock, "routing.cost_sample", nil)                        // 缺失 → 回退内置默认
	expectConfigRow(mock, "session.expiration", nil)                         // 缺失 → 回退内置默认
	expectConfigRow(mock, "gateway.upstream_response_header_whitelist", nil) // 缺失 → 回退空数组

	rec := httptest.NewRecorder()
	admin.HandleGetSystemConfig(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/configs/system", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	billingRaw, sessionRaw, whitelistRaw := systemConfigPayload(t, rec)
	in, cr, out, re := costSampleFromBilling(t, billingRaw)
	if in != 10000 || cr != 9000 || out != 1000 || re != 500 {
		t.Fatalf("回退样值=%d/%d/%d/%d, want 10000/9000/1000/500", in, cr, out, re)
	}
	var se sessionExpirationView
	if err := json.Unmarshal(sessionRaw, &se); err != nil {
		t.Fatalf("unmarshal SessionExpiration: %v", err)
	}
	if se.DefaultTTLMinutes != 5 || len(se.ContextTiers) == 0 || len(se.TurnTiers) == 0 {
		t.Fatalf("回退会话配置异常: %+v", se)
	}
	if string(whitelistRaw) != "[]" {
		t.Fatalf("回退白名单=%s, want []", string(whitelistRaw))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestHandlePutSystemConfig_RoundTrip 断言统一写入按固定顺序落库七个配置键。
func TestHandlePutSystemConfig_RoundTrip(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, testUserIDFrom)

	// 写入顺序与实现一致。
	for _, key := range []string{
		"billing.r", "billing.cny_rate", "billing.context_tiers", "billing.time_config",
		"routing.cost_sample", "session.expiration", "gateway.upstream_response_header_whitelist",
	} {
		mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO sys_configs(key, value, updated_by)`)).
			WithArgs(key, sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	body := `{"Billing":{"R":10000,"CnyRate":6.8,"ContextTiers":[],` +
		`"TimeConfig":{"Timezone":"Asia/Shanghai","Default":1},` +
		`"CostSample":{"input":12345,"cache_read":6000,"output":2000,"reasoning":300}},` +
		`"SessionExpiration":{"default_ttl_minutes":5,` +
		`"context_tiers":[{"max_tokens":2000,"ttl_minutes":5}],` +
		`"turn_tiers":[{"max_turns":2,"ttl_minutes":5}]},` +
		`"UpstreamResponseHeaderWhitelist":["traceparent"]}`
	rec := httptest.NewRecorder()
	admin.HandlePutSystemConfig(rec, httptest.NewRequest(http.MethodPut, "/api/v1/admin/configs/system", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestHandlePutSystemConfig_Invalid 断言任一字段非法即返回 400 且不落库。
func TestHandlePutSystemConfig_Invalid(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"r 为 0", `{"Billing":{"R":0,"CnyRate":6.8,"ContextTiers":[],"TimeConfig":{},"CostSample":{"input":1}},"SessionExpiration":{"default_ttl_minutes":5,"context_tiers":[],"turn_tiers":[]},"UpstreamResponseHeaderWhitelist":[]}`},
		{"cny_rate 非正数", `{"Billing":{"R":10000,"CnyRate":0,"ContextTiers":[],"TimeConfig":{},"CostSample":{"input":1}},"SessionExpiration":{"default_ttl_minutes":5,"context_tiers":[],"turn_tiers":[]},"UpstreamResponseHeaderWhitelist":[]}`},
		{"cost_sample 为负", `{"Billing":{"R":10000,"CnyRate":6.8,"ContextTiers":[],"TimeConfig":{},"CostSample":{"input":-1}},"SessionExpiration":{"default_ttl_minutes":5,"context_tiers":[],"turn_tiers":[]},"UpstreamResponseHeaderWhitelist":[]}`},
		{"会话 TTL 为 0", `{"Billing":{"R":10000,"CnyRate":6.8,"ContextTiers":[],"TimeConfig":{},"CostSample":{"input":1}},"SessionExpiration":{"default_ttl_minutes":0,"context_tiers":[],"turn_tiers":[]},"UpstreamResponseHeaderWhitelist":[]}`},
		{"会话分档非升序", `{"Billing":{"R":10000,"CnyRate":6.8,"ContextTiers":[],"TimeConfig":{},"CostSample":{"input":1}},"SessionExpiration":{"default_ttl_minutes":5,"context_tiers":[{"max_tokens":8000,"ttl_minutes":5},{"max_tokens":2000,"ttl_minutes":30}],"turn_tiers":[]},"UpstreamResponseHeaderWhitelist":[]}`},
		{"响应头名非法", `{"Billing":{"R":10000,"CnyRate":6.8,"ContextTiers":[],"TimeConfig":{},"CostSample":{"input":1}},"SessionExpiration":{"default_ttl_minutes":5,"context_tiers":[],"turn_tiers":[]},"UpstreamResponseHeaderWhitelist":["bad header"]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			admin := NewAdmin(db, billing.NewSqlStore(db), nil, testUserIDFrom)

			rec := httptest.NewRecorder()
			admin.HandlePutSystemConfig(rec, httptest.NewRequest(http.MethodPut, "/api/v1/admin/configs/system", strings.NewReader(c.body)))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			// 校验先于任何写入：不应产生 Exec。
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("非法配置不应触发写库: %v", err)
			}
		})
	}
}

// TestValidCostSample 覆盖样值校验的纯函数分支。
func TestValidCostSample(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`{"input":10000,"cache_read":9000,"output":1000,"reasoning":500}`, true},
		{`{"input":0}`, true},
		{`{}`, true},
		{`{"input":-1}`, false},
		{`123`, false},
		{`[1,2]`, false},
		{`not-json`, false},
	}
	for _, c := range cases {
		if got := validCostSample([]byte(c.raw)); got != c.want {
			t.Fatalf("validCostSample(%s)=%v, want %v", c.raw, got, c.want)
		}
	}
}
