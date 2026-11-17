package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
)

// fakeRProvider 满足 costRProvider，便于注入受控的积分换算系数 R。
type fakeRProvider struct {
	v   int64
	err error
}

func (f fakeRProvider) R() (int64, error) { return f.v, f.err }

// costAPIResp 内部 API 统一响应壳（仅取本测试需要的字段）。
type costAPIResp struct {
	Code int             `json:"Code"`
	Data json.RawMessage `json:"Data"`
}

func decodeCostResp(t *testing.T, rec *httptest.ResponseRecorder) costAPIResp {
	t.Helper()
	var out costAPIResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal body: %v; body=%s", err, rec.Body.String())
	}
	return out
}

// TestCostStats_NilDB_Empty 未装配 DB 时返回空结构而非报错。
func TestCostStats_NilDB_Empty(t *testing.T) {
	h := NewAdmin(nil, nil, nil)
	rec := httptest.NewRecorder()
	h.HandleCostStats(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var data struct {
		Rows  []costStatsRow `json:"Rows"`
		Total costStatsTotal `json:"Total"`
	}
	if err := json.Unmarshal(decodeCostResp(t, rec).Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if len(data.Rows) != 0 {
		t.Fatalf("Rows len=%d, want 0", len(data.Rows))
	}
	if data.Total.Requests != 0 || data.Total.CostCredits != 0 {
		t.Fatalf("Total=%+v, want zero", data.Total)
	}
}

// TestCostDefaults_NilDB_Empty 未装配 DB 时返回零值默认参数。
func TestCostDefaults_NilDB_Empty(t *testing.T) {
	h := NewAdmin(nil, nil, nil)
	rec := httptest.NewRecorder()
	h.HandleCostDefaults(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/defaults", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out costDefaultsResponse
	if err := json.Unmarshal(decodeCostResp(t, rec).Data, &out); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if out.PerTurnInputTokens != 0 || out.CacheHitRate != 0 || out.InputOutputRatio != 0 {
		t.Fatalf("defaults=%+v, want zero", out)
	}
}

// TestCostCurves_NilDB_TurnsClamp turns 上限/默认钳制（1..200）。
func TestCostCurves_NilDB_TurnsClamp(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"1000", costCurveMaxTurns},
		{"200", 200},
		{"0", costCurveDefaultTurns},
		{"", costCurveDefaultTurns},
	}
	for _, tc := range cases {
		h := NewAdmin(nil, nil, nil)
		url := "/api/v1/admin/cost/curves"
		if tc.raw != "" {
			url += "?turns=" + tc.raw
		}
		rec := httptest.NewRecorder()
		h.HandleCostCurves(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("turns=%q status=%d, want 200", tc.raw, rec.Code)
		}
		var out costCurvesResponse
		if err := json.Unmarshal(decodeCostResp(t, rec).Data, &out); err != nil {
			t.Fatalf("unmarshal data: %v", err)
		}
		if out.Turns != tc.want {
			t.Fatalf("turns=%q got %d, want %d", tc.raw, out.Turns, tc.want)
		}
	}
}

// TestCostCurves_InvalidParams_BadRequest NaN/±Inf 及非数字参数一律 400，杜绝序列化 500。
func TestCostCurves_InvalidParams_BadRequest(t *testing.T) {
	cases := []string{
		"?hitRate=Inf",
		"?hitRate=-Inf",
		"?hitRate=NaN",
		"?perTurnInput=abc",
		"?inOutRatio=1e999", // 溢出为 +Inf
		"?turns=abc",
	}
	for _, qs := range cases {
		h := NewAdmin(nil, nil, nil)
		rec := httptest.NewRecorder()
		h.HandleCostCurves(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/curves"+qs, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("qs=%q status=%d, want 400; body=%s", qs, rec.Code, rec.Body.String())
		}
		if code := decodeCostResp(t, rec).Code; code != 40001 {
			t.Fatalf("qs=%q code=%d, want 40001", qs, code)
		}
	}
}

// TestCostCurves_SaleOnly 成本曲线：**成本价已无配置源**（channel_models 被 0024 删除），
// 所以只可能出售价线；成本线恒为空（前端置灰）。hitRate 钳制与 ÷R 仍按售价线验证。
func TestCostCurves_SaleOnly(t *testing.T) {
	run := func(t *testing.T, rp costRProvider, wantTurn2 float64) {
		t.Helper()
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock: %v", err)
		}
		defer db.Close()

		rows := sqlmock.NewRows([]string{
			"channel_id", "channel_name", "internal_model_id", "external_name", "cost_rates", "sale_rates",
		}).AddRow(int64(0), "", "", "gpt", nil, []byte(`{"input":0,"output":0,"cache_read":10000}`))
		mock.ExpectQuery(`FROM external_models em`).WillReturnRows(rows)

		h := NewAdmin(db, billing.NewSqlStore(db), nil)
		if rp != nil {
			h.SetCostRProvider(rp)
		}
		rec := httptest.NewRecorder()
		// hitRate=5（>1）应被钳制为 1；perTurnInput=10000，cache_read 价=10000：
		// 第 2 轮 cacheRead = 1 × 10000 × 1 = 10000 → sale = 10000×10000 / R。
		h.HandleCostCurves(rec, httptest.NewRequest(http.MethodGet,
			"/api/v1/admin/cost/curves?perTurnInput=10000&hitRate=5&turns=2", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var out costCurvesResponse
		if err := json.Unmarshal(decodeCostResp(t, rec).Data, &out); err != nil {
			t.Fatalf("unmarshal data: %v", err)
		}
		if len(out.Curves) != 1 {
			t.Fatalf("curves len=%d, want 1", len(out.Curves))
		}
		if len(out.Curves[0].Cost) != 0 {
			t.Fatalf("成本线必须为空（成本价已无配置源），实际 %v", out.Curves[0].Cost)
		}
		if got := out.Curves[0].Sale[1]; got != wantTurn2 {
			t.Fatalf("turn2 sale=%v, want %v（hitRate 未钳制或未 ÷R）", got, wantTurn2)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	}

	t.Run("default R=10000", func(t *testing.T) { run(t, nil, 10000) })
	t.Run("custom R=20000", func(t *testing.T) { run(t, fakeRProvider{v: 20000}, 5000) })
}

// TestCostSession_MissingID_BadRequest 缺会话 ID 返回 400。
func TestCostSession_MissingID_BadRequest(t *testing.T) {
	h := NewAdmin(nil, nil, nil)
	rec := httptest.NewRecorder()
	h.HandleCostSession(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/session/", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if code := decodeCostResp(t, rec).Code; code != 40001 {
		t.Fatalf("code=%d, want 40001", code)
	}
}

// TestCostStats_Aggregation_ChannelDim 按实体聚合（channel_id 语义=渠道 ID），
// channel_key_id 为 NULL 的行 ChannelName 置 "(无渠道)"，Total 同口径累加。
// 改造后行结构多了 EntityKey / ChannelModelID / ExternalModelID / SnapshotNames（见 costStatsRow）。
// TestCostStats_Aggregation 成本统计聚合：**渠道维度已下线**（0024），
// 响应里 ChannelID/ChannelName/ChannelModelID 恒为 0/空，实体键只由对外模型 + 内部模型构成。
func TestCostStats_Aggregation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"entity_key", "channel_id", "channel_name", "channel_model_id", "external_model_id",
		"internal_model_id", "external_model_name", "cost_credits", "sale_credits", "requests",
		"input_tokens", "cache_read_tokens", "output_tokens", "snapshot_names",
	}).
		AddRow("name:ext1#int1", int64(0), "", int64(0), int64(31), "int1", "ext1", 10.0, 20.0, int64(3), int64(100), int64(50), int64(200), `["ext1"]`).
		AddRow("name:ext2#int2", int64(0), "", int64(0), int64(0), "int2", "ext2", 5.0, 6.0, int64(2), int64(10), int64(0), int64(20), `["ext2"]`)
	mock.ExpectQuery(`to_json\(array_agg\(DISTINCT b\.external_model_name\)\).*WHERE b\.status = 'completed'.*GROUP BY 1`).
		WillReturnRows(rows)

	h := NewAdmin(db, billing.NewSqlStore(db), nil)
	rec := httptest.NewRecorder()
	h.HandleCostStats(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var data struct {
		Rows  []costStatsRow `json:"Rows"`
		Total costStatsTotal `json:"Total"`
	}
	if err := json.Unmarshal(decodeCostResp(t, rec).Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if len(data.Rows) != 2 {
		t.Fatalf("rows len=%d, want 2", len(data.Rows))
	}
	for i, row := range data.Rows {
		if row.ChannelID != 0 || row.ChannelName != "" || row.ChannelModelID != 0 {
			t.Fatalf("row%d 渠道维度必须恒空（表已删）: %d/%q/%d", i, row.ChannelID, row.ChannelName, row.ChannelModelID)
		}
	}
	if data.Rows[0].EntityKey != "name:ext1#int1" {
		t.Fatalf("实体键口径应为 name:<模型>#<内部模型>，实际 %q", data.Rows[0].EntityKey)
	}
	if data.Total.CostCredits != 15 || data.Total.SaleCredits != 26 || data.Total.Requests != 5 {
		t.Fatalf("total=%+v, want cost=15 sale=26 req=5", data.Total)
	}
	if data.Total.InputTokens != 110 || data.Total.CacheReadTokens != 50 || data.Total.OutputTokens != 220 {
		t.Fatalf("total tokens=%+v, want 110/50/220", data.Total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
