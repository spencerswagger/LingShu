package console

// admin_config_test.go 系统配置读写（配置页整改）的后端契约测试：
//
//  1. `Routing.SessionCost` 块：读回运行态、缺失/损坏回退三态全关、写入走「不传＝不改」的字段级合并；
//  2. PUT **只写请求体里出现的键**（配置页按 tab 分别保存的前提）；
//  3. 保存**热生效、无需重启**：UpsertConfig → 写钩子 → Provider.Invalidate → 下一次读取拿到新值；
//  4. 已删除的成本页开关不再出现在 GET/PUT 里。

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/router"
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

// sessionCostOf 从 GET 结果里取出 Routing.SessionCost。
func sessionCostOf(t *testing.T, data map[string]json.RawMessage) sessionCostView {
	t.Helper()
	var routing struct {
		SessionCost sessionCostView `json:"SessionCost"`
	}
	if err := json.Unmarshal(data["Routing"], &routing); err != nil {
		t.Fatalf("解析 Routing 失败: %v (%s)", err, data["Routing"])
	}
	return routing.SessionCost
}

// expectSessionCostRow 会话成本路由配置的读取期望。
// 注意 SQL 形式：`sysConfigRaw` 用的是 `key = $1`（带空格），与 billing.GetConfig 的 `key=$1` 不同。
func expectSessionCostRow(mock sqlmock.Sqlmock, val []byte) {
	if val == nil {
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
			WithArgs(router.SessionCostConfigKey).
			WillReturnError(sql.ErrNoRows)
		return
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
		WithArgs(router.SessionCostConfigKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(val))
}

// expectSystemConfigReads 按 GET 的读取顺序登记期望（计费 5 键 → 路由 1 键 → 会话过期 → 白名单）。
func expectSystemConfigReads(mock sqlmock.Sqlmock, sessionCost []byte) {
	expectConfigRow(mock, "billing.r", []byte(`10000`))
	expectConfigRow(mock, "billing.cny_rate", []byte(`6.8`))
	expectConfigRow(mock, "billing.context_tiers", []byte(`[]`))
	expectConfigRow(mock, "billing.time_config", []byte(`{"Timezone":"Asia/Shanghai","Default":1}`))
	expectConfigRow(mock, "routing.cost_sample", []byte(`{"input":10000,"cache_read":9000,"output":1000}`))
	expectSessionCostRow(mock, sessionCost)
	expectConfigRow(mock, "session.expiration", []byte(`{"default_ttl_minutes":5,"context_tiers":[],"turn_tiers":[]}`))
	expectConfigRow(mock, "gateway.upstream_response_header_whitelist", []byte(`[]`))
}

// TestGetSystemConfig_RoutingSessionCostUninitialized 键缺失（配置未初始化）→ **产品默认**
// （enabled=true / mid_switch=true / shadow_enabled=false + 数值默认），且带只读回退说明；
// 同时确认 Console 块已从响应里消失。注意这条**不是**「解析失败」那条（见下一个用例）。
func TestGetSystemConfig_RoutingSessionCostUninitialized(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, nil)
	expectSystemConfigReads(mock, nil) // nil = 键不存在

	data := systemConfigGet(t, admin)
	sc := sessionCostOf(t, data)
	if !sc.Enabled || !sc.MidSwitch || sc.ShadowEnabled {
		t.Fatalf("键缺失=未初始化，应回显产品默认 true/true/false: %+v", sc)
	}
	if sc.RemainingA != 10 || sc.RemainingB != 1 || sc.RemainingMin != 1 || sc.RemainingMax != 20 ||
		sc.MinRemaining != 1 || sc.CooldownSec != 300 || sc.MinImprove != 0.05 {
		t.Fatalf("数值字段应回退产品默认: %+v", sc)
	}
	if sc.FallbackNote == "" || !strings.Contains(sc.FallbackNote, "渠道亲和") {
		t.Fatalf("必须带回退说明（两开关都关=渠道亲和）: %q", sc.FallbackNote)
	}
	if _, ok := data["Console"]; ok {
		t.Fatalf("成本页开关块已删除，不应再出现: %s", data["Console"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("读取失败时不应回写库: %v", err)
	}
}

// TestGetSystemConfig_RoutingSessionCostStored 库里有值 → 面板显示运行态实际取值（不是默认值）。
func TestGetSystemConfig_RoutingSessionCostStored(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, nil)
	expectSystemConfigReads(mock, []byte(`{"enabled":false,"mid_switch":true,"shadow_enabled":true,
		"remaining_a":7,"remaining_b":2,"remaining_min":2,"remaining_max":9,"min_remaining":3,
		"cooldown_sec":60,"min_improve":0.2}`))

	sc := sessionCostOf(t, systemConfigGet(t, admin))
	if sc.Enabled || !sc.MidSwitch || !sc.ShadowEnabled {
		t.Fatalf("开关取值应按库中值回显: %+v", sc)
	}
	if sc.RemainingA != 7 || sc.RemainingB != 2 || sc.RemainingMin != 2 || sc.RemainingMax != 9 ||
		sc.MinRemaining != 3 || sc.CooldownSec != 60 || sc.MinImprove != 0.2 {
		t.Fatalf("数值字段应按库中值回显: %+v", sc)
	}
}

// TestGetSystemConfig_RoutingSessionCostCorrupt 键存在但解析失败 = **配置异常** → 故障安全态
// （三态全关，与运行时回退一致），且不回写库。这条与「键缺失=未初始化」是两种语义，必须分开验。
func TestGetSystemConfig_RoutingSessionCostCorrupt(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, nil)
	// 注意：sql_configs.value 是 jsonb，真实库不会出现非 JSON，这里模拟「结构不符预期」的解析失败。
	expectSystemConfigReads(mock, []byte(`[1,2,3]`))

	sc := sessionCostOf(t, systemConfigGet(t, admin))
	if sc.Enabled || sc.MidSwitch || sc.ShadowEnabled {
		t.Fatalf("解析失败必须回退三态全关: %+v", sc)
	}
	if sc.RemainingA != 10 || sc.CooldownSec != 300 || sc.MinImprove != 0.05 {
		t.Fatalf("解析失败时数值用默认补齐: %+v", sc)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("解析失败不应回写库: %v", err)
	}
}

// putSystemConfig 发 PUT 并返回 (status, Affected)。
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

// TestPutSystemConfig_SessionCostMergesFields 只传 enabled → 只写一个键，且其余字段保持库中现值（不回落默认）。
func TestPutSystemConfig_SessionCostMergesFields(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, testUserIDFrom)

	// 现值（读一次）：b=2 / cooldown=60 / min_improve=0.2
	expectSessionCostRow(mock, []byte(`{"enabled":false,"mid_switch":true,
		"remaining_a":7,"remaining_b":2,"remaining_min":2,"remaining_max":9,"min_remaining":3,
		"cooldown_sec":60,"min_improve":0.2}`))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO sys_configs(key, value, updated_by)`)).
		WithArgs(router.SessionCostConfigKey, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	code, affected := putSystemConfig(t, admin, `{"Routing":{"SessionCost":{"enabled":true}}}`)
	if code != http.StatusOK || affected != 1 {
		t.Fatalf("只应写入 1 个键: code=%d affected=%v", code, affected)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("期望未满足（不应写其它键）: %v", err)
	}
}

// TestPutSystemConfig_SessionCostMergedValue 断言合并结果：只改传了的字段，其余保留现值。
func TestPutSystemConfig_SessionCostMergedValue(t *testing.T) {
	cur := router.SessionCostConfig{
		Enabled: false, MidSwitch: true, ShadowEnabled: false,
		RemainingA: 7, RemainingB: 2, RemainingMin: 2, RemainingMax: 9, MinRemaining: 3,
		CooldownSec: 60, MinImprove: 0.2,
	}
	enabled := true
	merged := mergeSessionCost(cur, sessionCostPatch{Enabled: &enabled})
	if !merged.Enabled || merged.MidSwitch != true || merged.RemainingA != 7 || merged.RemainingB != 2 ||
		merged.CooldownSec != 60 || merged.MinImprove != 0.2 || merged.MinRemaining != 3 {
		t.Fatalf("未传的字段必须保持现值，不能被默认值覆盖: %+v", merged)
	}
	if err := validateSessionCostConfig(merged); err != nil {
		t.Fatalf("合并结果应合法: %v", err)
	}
}

// TestPutSystemConfig_SessionCostInvalid 非法取值 400 且不落库。
func TestPutSystemConfig_SessionCostInvalid(t *testing.T) {
	cases := map[string]string{
		"min_improve 越界":    `{"Routing":{"SessionCost":{"min_improve":1.5}}}`,
		"min_improve 负数":    `{"Routing":{"SessionCost":{"min_improve":-0.1}}}`,
		"remaining_min 为 0": `{"Routing":{"SessionCost":{"remaining_min":0}}}`,
		"remaining_max 太小":  `{"Routing":{"SessionCost":{"remaining_min":5,"remaining_max":3}}}`,
		"cooldown 负数":       `{"Routing":{"SessionCost":{"cooldown_sec":-1}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			admin := NewAdmin(db, billing.NewSqlStore(db), nil, testUserIDFrom)
			// 合并基线读取（现值）
			expectSessionCostRow(mock, nil)

			if code, _ := putSystemConfig(t, admin, body); code != http.StatusBadRequest {
				t.Fatalf("非法取值应 400，got %d", code)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("校验失败不应落库: %v", err)
			}
		})
	}
}

// TestPutSystemConfig_InvalidSessionCostWritesNothing 校验失败必须**一个键都不写**：
// 即使同一个请求里还带了合法的计费字段，也不能先写一半再 400。
func TestPutSystemConfig_InvalidSessionCostWritesNothing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, testUserIDFrom)
	expectSessionCostRow(mock, nil) // 合并基线读取

	body := `{"Billing":{"R":10000},"Routing":{"SessionCost":{"min_improve":2}}}`
	if code, _ := putSystemConfig(t, admin, body); code != http.StatusBadRequest {
		t.Fatalf("非法 SessionCost 应 400，got %d", code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("校验失败时不得写入任何键（含计费块）: %v", err)
	}
}

// TestPutSystemConfig_OnlyProvidedKeys 只写请求体里出现的键（配置页每 tab 独立保存的前提）。
func TestPutSystemConfig_OnlyProvidedKeys(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, testUserIDFrom)

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
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, testUserIDFrom)

	code, affected := putSystemConfig(t, admin, `{}`)
	if code != http.StatusOK || affected != 0 {
		t.Fatalf("空请求体应 200 且 Affected=0: code=%d affected=%v", code, affected)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("空请求体不应写库: %v", err)
	}
}

// TestPutSystemConfig_WhitelistEmptyArray 显式传空数组 = 清空白名单（与「不传」区分开）。
func TestPutSystemConfig_WhitelistEmptyArray(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, testUserIDFrom)

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO sys_configs(key, value, updated_by)`)).
		WithArgs("gateway.upstream_response_header_whitelist", []byte(`[]`), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if code, affected := putSystemConfig(t, admin, `{"UpstreamResponseHeaderWhitelist":[]}`); code != http.StatusOK || affected != 1 {
		t.Fatalf("显式空数组应写入一次: code=%d affected=%v", code, affected)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("期望未满足: %v", err)
	}
}

// TestPutSystemConfig_WhitelistNullIsNoop 显式传 null 等同「没传」（指针为 nil）：
// 不写库、不把白名单清空——避免「想清空却传错 null」把配置静默改掉。
func TestPutSystemConfig_WhitelistNullIsNoop(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, testUserIDFrom)

	if code, affected := putSystemConfig(t, admin, `{"UpstreamResponseHeaderWhitelist":null}`); code != http.StatusOK || affected != 0 {
		t.Fatalf("null 应视为未传: code=%d affected=%v", code, affected)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("null 不应写库: %v", err)
	}
}

// TestPutSystemConfig_HotReloadWithoutRestart 断言「保存即热生效、无需重启」的完整链路：
// Provider 先读到旧值并缓存 → PUT 写库（触发 SetConfigWriteHook → Invalidate）→ 同一进程再读，
// 必须**重新回源**拿到新值。若写钩子没接上，第二次读取会命中旧缓存、也不会发出第二次查询。
func TestPutSystemConfig_HotReloadWithoutRestart(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	billStore := billing.NewSqlStore(db)
	provider := router.NewSessionCostProvider(billStore)
	billStore.SetConfigWriteHook(provider.Invalidate) // 与 cmd/server/main.go 的装配一致

	// 1) 首次读取：旧值 enabled=false，并被缓存。
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key=$1`)).
		WithArgs(router.SessionCostConfigKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`{"enabled":false,"mid_switch":false}`)))
	before := provider.Config()
	if before.Enabled || before.MidSwitch {
		t.Fatalf("初始应为关闭态: %+v", before)
	}

	admin := NewAdmin(db, billStore, nil, testUserIDFrom)
	// 2) 保存：读现值（合并基线）+ 写库
	expectSessionCostRow(mock, []byte(`{"enabled":false,"mid_switch":false}`))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO sys_configs(key, value, updated_by)`)).
		WithArgs(router.SessionCostConfigKey, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if code, affected := putSystemConfig(t, admin, `{"Routing":{"SessionCost":{"enabled":true,"mid_switch":true}}}`); code != http.StatusOK || affected != 1 {
		t.Fatalf("保存失败: code=%d affected=%v", code, affected)
	}

	// 3) 同一进程再读：必须重新回源（这里给了新值），证明写钩子把缓存失效了。
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key=$1`)).
		WithArgs(router.SessionCostConfigKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`{"enabled":true,"mid_switch":true}`)))
	after := provider.Config()
	if !after.Enabled || !after.MidSwitch {
		t.Fatalf("保存后应立即读到新值（热生效，无需重启）: %+v", after)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("期望未满足（写钩子可能没触发失效）: %v", err)
	}
}

// TestPutSystemConfig_ResponseAffectedCounts 老客户端提交全量字段时，Affected = 实际写入键数。
func TestPutSystemConfig_ResponseAffectedCounts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, testUserIDFrom)

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
		`"CostSample":{"input":1}},"SessionExpiration":{"default_ttl_minutes":5,` +
		`"context_tiers":[],"turn_tiers":[]},"UpstreamResponseHeaderWhitelist":[]}`
	if code, affected := putSystemConfig(t, admin, body); code != http.StatusOK || affected != 7 {
		t.Fatalf("全量提交应写入 7 个键: code=%d affected=%v", code, affected)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("期望未满足: %v", err)
	}
}
