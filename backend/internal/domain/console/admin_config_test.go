package console

// admin_config_test.go 系统配置读写的后端契约测试：
//
//  1. GET **只返回计费块**（路由/会话过期/上游白名单已随转发链迁出，不再返回）；
//  2. PUT **只写请求体里出现的键**（配置页按 tab 分别保存的前提）；
//  3. 请求体里出现已摘除的旧块（Routing / SessionExpiration / UpstreamResponseHeaderWhitelist）时忽略，
//     不写入任何键（避免老前端把死配置写回库）；
//  4. 已删除的成本页开关不再出现在 GET/PUT 里。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
)

// systemConfigGet 调 GET /configs/system 并把 Data 解成按块取原文的映射。
func systemConfigGet(t *testing.T, admin *Admin) map[string]json.RawMessage {
	t.Helper()
	rec := httptest.NewRecorder()
	admin.HandleGetSystemConfig(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/configs/system", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data map[string]json.RawMessage `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return body.Data
}

// putSystemConfig 调 PUT /configs/system，返回 (状态码, Affected)；非 200 时 Affected=-1。
func putSystemConfig(t *testing.T, admin *Admin, body string) (int, float64) {
	t.Helper()
	rec := httptest.NewRecorder()
	admin.HandlePutSystemConfig(rec, httptest.NewRequest(http.MethodPut, "/api/v1/admin/configs/system", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		return rec.Code, -1
	}
	var out struct {
		Data struct {
			Affected float64 `json:"Affected"`
		} `json:"Data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.Data.Affected
}

// TestGetSystemConfig_BillingOnly 只返回计费块；路由/会话过期/白名单键不再出现。
func TestGetSystemConfig_BillingOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil)

	expectConfigRow(mock, "billing.r", []byte(`10000`))
	expectConfigRow(mock, "billing.cny_rate", []byte(`6.8`))
	expectConfigRow(mock, "billing.context_tiers", []byte(`[]`))
	expectConfigRow(mock, "billing.time_config", []byte(`{"Timezone":"Asia/Shanghai","Default":1}`))
	expectConfigRow(mock, "routing.cost_sample", []byte(`{"input":10000,"cache_read":9000,"output":1000}`))

	data := systemConfigGet(t, admin)
	if _, ok := data["Billing"]; !ok {
		t.Fatalf("响应缺少 Billing 块: %v", data)
	}
	for _, gone := range []string{"Routing", "SessionExpiration", "UpstreamResponseHeaderWhitelist"} {
		if _, ok := data[gone]; ok {
			t.Fatalf("已摘除的配置块 %s 不应再返回", gone)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("不应读取已摘除配置键: %v", err)
	}
}

// TestPutSystemConfig_OnlyProvidedKeys 只写请求体里出现的键（配置页每 tab 独立保存的前提）。
func TestPutSystemConfig_OnlyProvidedKeys(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), testUserIDFrom)

	// 只提交「计费」tab 的汇率一个字段：只允许一条 Exec。
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO sys_configs(key, value, updated_by)`)).
		WithArgs("billing.cny_rate", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	code, affected := putSystemConfig(t, admin, `{"Billing":{"CnyRate":7.1}}`)
	if code != http.StatusOK || affected != 1 {
		t.Fatalf("只应写入 1 个键: code=%d affected=%v", code, affected)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未传的键不得写入: %v", err)
	}
}

// TestPutSystemConfig_EmptyBody 空请求体 = 什么都不改（不是把所有键清零）。
func TestPutSystemConfig_EmptyBody(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), testUserIDFrom)

	code, affected := putSystemConfig(t, admin, `{}`)
	if code != http.StatusOK || affected != 0 {
		t.Fatalf("空请求体不应写库: code=%d affected=%v", code, affected)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("空请求体不应产生写库: %v", err)
	}
}

// TestPutSystemConfig_LegacyBlocksIgnored 老前端仍提交 Routing / SessionExpiration / 白名单时忽略（零写入）。
func TestPutSystemConfig_LegacyBlocksIgnored(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), testUserIDFrom)

	body := `{"Routing":{"SessionCost":{"enabled":true}},` +
		`"SessionExpiration":{"default_ttl_minutes":5,"context_tiers":[],"turn_tiers":[]},` +
		`"UpstreamResponseHeaderWhitelist":["traceparent"]}`
	code, affected := putSystemConfig(t, admin, body)
	if code != http.StatusOK || affected != 0 {
		t.Fatalf("已摘除的配置块应被忽略: code=%d affected=%v", code, affected)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("已摘除的配置块不得写库: %v", err)
	}
}

// TestPutSystemConfig_AffectedCountsConfigRows 断言 Affected 的口径 = 实际写入的配置行数（非字段数）。
func TestPutSystemConfig_AffectedCountsConfigRows(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int
		want     float64
		// expectKeys 为期望写入的配置键（顺序即调用顺序）；nil 表示不应有任何写入。
		expectKeys []string
	}{
		{
			name:       "同块两字段算两行",
			body:       `{"Billing":{"R":10000,"CnyRate":7.1}}`,
			wantCode:   http.StatusOK,
			want:       2,
			expectKeys: []string{"billing.r", "billing.cny_rate"},
		},
		{
			name:       "全五键算五行",
			body:       `{"Billing":{"R":10000,"CnyRate":7.1,"ContextTiers":[],"TimeConfig":{},"CostSample":{"input":1}}}`,
			wantCode:   http.StatusOK,
			want:       5,
			expectKeys: []string{"billing.r", "billing.cny_rate", "billing.context_tiers", "billing.time_config", "routing.cost_sample"},
		},
		{
			name:     "空请求体算零行",
			body:     `{}`,
			wantCode: http.StatusOK,
			want:     0,
		},
		{
			name:     "校验失败零行零写入",
			body:     `{"Billing":{"R":0}}`,
			wantCode: http.StatusBadRequest,
			want:     0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			admin := NewAdmin(db, billing.NewSqlStore(db), testUserIDFrom)

			for _, key := range c.expectKeys {
				mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO sys_configs(key, value, updated_by)`)).
					WithArgs(key, sqlmock.AnyArg(), sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(0, 1))
			}

			code, affected := putSystemConfig(t, admin, c.body)
			if code != c.wantCode {
				t.Fatalf("status=%d want=%d", code, c.wantCode)
			}
			if code == http.StatusOK && affected != c.want {
				t.Fatalf("Affected=%v want=%v（口径=写入的配置行数）", affected, c.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("写入键与期望不符（或多写了）: %v", err)
			}
		})
	}
}
