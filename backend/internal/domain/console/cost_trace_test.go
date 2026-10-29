package console

// cost_trace_test.go 会话渠道轨迹 + 多线对比的单元/接口测试。
//
// 覆盖点：
//   - 轨迹分段与真实切换点（渠道变化 vs 同渠道换模型）；
//   - 从未切换 → 预测打平点（Kind 枚举与真实切换点不同）；
//   - 开关默认关闭 → 403（pricing-curve 的 ChannelModelIDs 与 trace 端点都拒）；
//   - ChannelModelIDs 解析（去重保序、非法、超上限）；
//   - 多线响应结构（每线独立渠道/模型标识 + 成本与售价序列）。

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
)

// ---------- 纯函数：参数解析 ----------

func TestParseChannelModelIDs(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    []int64
		wantErr bool
	}{
		{name: "空=单线模式", raw: "", want: nil},
		{name: "单个", raw: "900", want: []int64{900}},
		{name: "多个保序", raw: "903,900,901", want: []int64{903, 900, 901}},
		{name: "去空格与重复", raw: " 900 , 900 ,901,, ", want: []int64{900, 901}},
		{name: "非法字符", raw: "900,abc", wantErr: true},
		{name: "零与负数", raw: "0", wantErr: true},
		{name: "超过上限", raw: "1,2,3,4,5,6,7,8,9,10,11", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseChannelModelIDs(c.raw)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestParseConfigBool(t *testing.T) {
	cases := map[string]bool{
		`true`: true, `false`: false, `"true"`: true, `"on"`: true,
		`"1"`: true, `"no"`: false, ``: false, `null`: false, `1`: true, `乱码`: false,
	}
	for raw, want := range cases {
		if got := parseConfigBool([]byte(raw)); got != want {
			t.Fatalf("parseConfigBool(%q)=%v, want %v", raw, got, want)
		}
	}
}

// ---------- 纯函数：打平点插值 ----------

func TestFirstCrossTokens(t *testing.T) {
	// 实际线恒定 1.0；替代线从 2.0 线性降到 0.0（在第 3 个点反超）。
	cur := []pricingPoint{{Tokens: 1000, Credits: 1.0}, {Tokens: 2000, Credits: 1.0}, {Tokens: 3000, Credits: 1.0}}
	alt := []pricingPoint{{Tokens: 1000, Credits: 2.0}, {Tokens: 2000, Credits: 1.0}, {Tokens: 3000, Credits: 0.0}}
	got, ok := firstCrossTokens(cur, alt)
	if !ok {
		t.Fatalf("应当找到交叉点")
	}
	// diff: +1.0 → 0 → -1.0；在 [2000,3000] 内插值 frac=0/(0+1)=0 → 2000… 但段起点 diff=0 不算「严格更便宜」，
	// 因此交叉落在 2000 与 3000 之间且 frac=0 → 2000。
	if got != 2000 {
		t.Fatalf("got %d, want 2000", got)
	}

	// 起点就更便宜 → 取曲线左端。
	cur2 := []pricingPoint{{Tokens: 1000, Credits: 5.0}, {Tokens: 2000, Credits: 5.0}}
	alt2 := []pricingPoint{{Tokens: 1000, Credits: 1.0}, {Tokens: 2000, Credits: 1.0}}
	if got, ok := firstCrossTokens(cur2, alt2); !ok || got != 1000 {
		t.Fatalf("起点更便宜应返回左端 1000，got %d ok=%v", got, ok)
	}

	// 全程更贵 → 没有交叉点。
	alt3 := []pricingPoint{{Tokens: 1000, Credits: 9.0}, {Tokens: 2000, Credits: 9.0}, {Tokens: 3000, Credits: 9.0}}
	if _, ok := firstCrossTokens(cur, alt3); ok {
		t.Fatalf("全程更贵不应有交叉点")
	}

	// 超档段打断插值：任一侧 OverTier 一律跳过。
	cur4 := []pricingPoint{{Tokens: 1000, Credits: 1.0}, {Tokens: 2000, Credits: 1.0, OverTier: true}, {Tokens: 3000, Credits: 1.0}}
	alt4 := []pricingPoint{{Tokens: 1000, Credits: 2.0}, {Tokens: 2000, Credits: 0.1}, {Tokens: 3000, Credits: 0.1}}
	if got, ok := firstCrossTokens(cur4, alt4); !ok || got != 3000 {
		t.Fatalf("超档点应被跳过，交叉落在 3000，got %d ok=%v", got, ok)
	}
}

// ---------- 分段与真实切换点 ----------

func TestBuildTraceSegments_ActualSwitch(t *testing.T) {
	points := []costTracePoint{
		{TurnNo: 1, ChannelID: 5, ChannelName: "渠道A", ChannelModelID: 900, InternalModelID: "m1", CostCredits: 1},
		{TurnNo: 2, ChannelID: 5, ChannelName: "渠道A", ChannelModelID: 900, InternalModelID: "m1", CostCredits: 1},
		// 同渠道换模型：新段但不是渠道切换
		{TurnNo: 3, ChannelID: 5, ChannelName: "渠道A", ChannelModelID: 905, InternalModelID: "m2", CostCredits: 2},
		// 换渠道：真实切换点
		{TurnNo: 4, ChannelID: 6, ChannelName: "渠道B", ChannelModelID: 901, InternalModelID: "m1", CostCredits: 3},
	}
	segments, switches := buildTraceSegments(points)
	if len(segments) != 3 {
		t.Fatalf("段数=%d want 3: %+v", len(segments), segments)
	}
	if segments[0].Points != 2 || segments[0].SuccessPoints != 2 {
		t.Fatalf("首段点数错: %+v", segments[0])
	}
	if len(switches) != 1 {
		t.Fatalf("切换点数=%d want 1（同渠道换模型不算）: %+v", len(switches), switches)
	}
	sw := switches[0]
	if sw.Kind != traceKindActual || sw.TurnNo != 4 || sw.FromChannelID != 5 || sw.ToChannelID != 6 {
		t.Fatalf("切换点字段错: %+v", sw)
	}
	if sw.ToChannelModelID != 901 || sw.FromChannelModelID != 905 {
		t.Fatalf("切换点应带上两端的渠道内部模型: %+v", sw)
	}
}

func TestBuildTraceSegments_NoSwitch(t *testing.T) {
	points := []costTracePoint{
		{TurnNo: 1, ChannelID: 5, ChannelModelID: 900, InternalModelID: "m1", CostCredits: 1},
		{TurnNo: 2, ChannelID: 5, ChannelModelID: 900, InternalModelID: "m1", Failed: true},
	}
	segments, switches := buildTraceSegments(points)
	if len(segments) != 1 || len(switches) != 0 {
		t.Fatalf("单段无切换: seg=%d sw=%d", len(segments), len(switches))
	}
	if segments[0].Points != 2 || segments[0].SuccessPoints != 1 || segments[0].CostCredits != 1 {
		t.Fatalf("失败轮不计成功点数但占点位: %+v", segments[0])
	}
}

// ---------- 开关 ----------

func TestHandleCostPricingCurve_MultilineDisabled(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	// 开关默认关闭：读不到键（sql.ErrNoRows）或值为 false 都按关闭处理。
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
		WithArgs(costViewsSwitchKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`false`)))

	h := NewAdmin(db, nil, nil, nil)
	h.now = func() time.Time { return pricingTestAt(t) }
	rec := httptest.NewRecorder()
	h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/cost/pricing-curve?ChannelModelIDs=900,901", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("开关关闭时应 403，got %d body=%s", rec.Code, rec.Body.String())
	}
	if !contains(rec.Body.String(), "console.cost_multichannel_trace") {
		t.Fatalf("403 文案应指明开关键: %s", rec.Body.String())
	}
}

func TestHandleCostSessionTrace_Disabled(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
		WithArgs(costViewsSwitchKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`false`)))

	h := NewAdmin(db, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/session/sess-1/trace", nil)
	req.SetPathValue("id", "sess-1")
	rec := httptest.NewRecorder()
	h.HandleCostSessionTrace(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("开关关闭时应 403，got %d body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- 多线对比：接口层 ----------

func expectSwitchOn(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
		WithArgs(costViewsSwitchKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`true`)))
}

func expectGlobalTimeAndTiers(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
		WithArgs("billing.time_config").
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`{"Timezone":"Asia/Shanghai","Default":1.0,"Periodic":[{"Name":"高峰","Start":"18:00","End":"22:00","Coeff":1.5}]}`)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
		WithArgs("billing.context_tiers").
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`[]`)))
}

// TestHandleCostPricingCurve_Multiline 断言：两条线各带自己的渠道/模型标识与成本、售价序列，
// 且 PointCount、MaxTokens 在多线下按线数收敛到总点数上限以内。
func TestHandleCostPricingCurve_Multiline(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	expectSwitchOn(mock)
	expectGlobalTimeAndTiers(mock)
	// 第 1 条线：成本配置 + 反查对外模型
	mock.ExpectQuery(regexp.QuoteMeta(`FROM channel_models cm`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "internal_model_id", "cost_rates", "time_config", "context_tiers"}).
			AddRow(int64(900), int64(5), "渠道A", "gpt-4o-int", []byte(`{"input":1,"output":4,"cache_read":0.1}`), nil, nil))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT cm.external_model_id, em.external_name`)).
		WillReturnRows(sqlmock.NewRows([]string{"external_model_id", "external_name", "sale_rates", "time_config", "context_tiers"}).
			AddRow(int64(31), "gpt-4o", []byte(`{"input":1.5,"output":6,"cache_read":0.15}`), nil, nil))
	// 第 2 条线：更便宜的另一家渠道
	mock.ExpectQuery(regexp.QuoteMeta(`FROM channel_models cm`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "internal_model_id", "cost_rates", "time_config", "context_tiers"}).
			AddRow(int64(901), int64(6), "渠道B", "gpt-4o-int", []byte(`{"input":0.5,"output":2,"cache_read":0.05}`), nil, nil))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT cm.external_model_id, em.external_name`)).
		WillReturnRows(sqlmock.NewRows([]string{"external_model_id", "external_name", "sale_rates", "time_config", "context_tiers"}).
			AddRow(int64(31), "gpt-4o", []byte(`{"input":1.5,"output":6,"cache_read":0.15}`), nil, nil))

	h := NewAdmin(db, nil, nil, nil)
	h.now = func() time.Time { return pricingTestAt(t) }
	rec := httptest.NewRecorder()
	h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/cost/pricing-curve?ChannelModelIDs=900,901&Step=1000&MaxTokens=200000&HitRate=0&InOutRatio=10&At=19:30", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code int                  `json:"Code"`
		Data pricingCurveResponse `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d := body.Data
	if len(d.Lines) != 2 {
		t.Fatalf("应当两条线，got %d: %s", len(d.Lines), rec.Body.String())
	}
	// 多线总点数上限 2000：两条线 → 每条最多 1000 点 → MaxTokens=1000×1000=1e6 但原值 200000 更小，
	// 200000/1000=200 点/线 < 1000，所以 MaxTokens 保持 200000。
	if d.MaxTokens != 200000 || d.PointCount != 200 {
		t.Fatalf("MaxTokens=%d PointCount=%d，want 200000/200", d.MaxTokens, d.PointCount)
	}
	if d.Lines[0].ChannelModelID != 900 || d.Lines[0].ChannelName != "渠道A" || d.Lines[0].InternalModelID != "gpt-4o-int" {
		t.Fatalf("第 1 条线标识错: %+v", d.Lines[0])
	}
	if d.Lines[1].ChannelModelID != 901 || d.Lines[1].ChannelName != "渠道B" {
		t.Fatalf("第 2 条线标识错: %+v", d.Lines[1])
	}
	if d.Lines[1].LineIndex != 1 || d.Lines[0].LineIndex != 0 {
		t.Fatalf("LineIndex 应保持请求顺序: %+v", d.Lines)
	}
	if d.Lines[0].ExternalModelID != 31 || d.Lines[0].ExternalModelName != "gpt-4o" {
		t.Fatalf("对外模型标识错: %+v", d.Lines[0])
	}
	// 同一横轴上渠道 B 更便宜：第 1 点成本应当更低。
	if !(d.Lines[1].Cost.Points[0].Credits < d.Lines[0].Cost.Points[0].Credits) {
		t.Fatalf("渠道B 应更便宜: A=%v B=%v",
			d.Lines[0].Cost.Points[0].Credits, d.Lines[1].Cost.Points[0].Credits)
	}
	if d.Lines[0].Cost.Source != "channel_model" || !d.Lines[0].Available {
		t.Fatalf("线内成本线字段错: %+v", d.Lines[0].Cost)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("查询期望未满足: %v", err)
	}
}

// TestHandleCostPricingCurve_MultilinePointCap 线数多时按线数等比下调 MaxTokens（总点数不超上限）。
func TestHandleCostPricingCurve_MultilinePointCap(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	expectSwitchOn(mock)
	expectGlobalTimeAndTiers(mock)
	for _, id := range []int64{900, 901, 902, 903} {
		mock.ExpectQuery(regexp.QuoteMeta(`FROM channel_models cm`)).
			WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "internal_model_id", "cost_rates", "time_config", "context_tiers"}).
				AddRow(id, int64(5), "渠道", "m1", []byte(`{"input":1,"output":4,"cache_read":0.1}`), nil, nil))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT cm.external_model_id, em.external_name`)).
			WillReturnRows(sqlmock.NewRows([]string{"external_model_id", "external_name", "sale_rates", "time_config", "context_tiers"}).
				AddRow(int64(31), "gpt-4o", []byte(`{"input":1.5,"output":6,"cache_read":0.15}`), nil, nil))
	}
	h := NewAdmin(db, nil, nil, nil)
	h.now = func() time.Time { return pricingTestAt(t) }
	rec := httptest.NewRecorder()
	h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/cost/pricing-curve?ChannelModelIDs=900,901,902,903&Step=1000&MaxTokens=500000&HitRate=0&At=19:30", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data pricingCurveResponse `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 4 条线 → 每条最多 500 点 → MaxTokens = 1000×500 = 500000，恰好等于请求值。
	if body.Data.MaxTokens != 500000 || len(body.Data.Lines) != 4 {
		t.Fatalf("MaxTokens=%d lines=%d", body.Data.MaxTokens, len(body.Data.Lines))
	}
}

// TestHandleCostPricingCurve_MultilineMissingID 不存在的 ID 给一条不可用线（不 500）。
func TestHandleCostPricingCurve_MultilineMissingID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	expectSwitchOn(mock)
	expectGlobalTimeAndTiers(mock)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM channel_models cm`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "internal_model_id", "cost_rates", "time_config", "context_tiers"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT cm.external_model_id, em.external_name`)).
		WillReturnRows(sqlmock.NewRows([]string{"external_model_id", "external_name", "sale_rates", "time_config", "context_tiers"}))

	h := NewAdmin(db, nil, nil, nil)
	h.now = func() time.Time { return pricingTestAt(t) }
	rec := httptest.NewRecorder()
	h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/cost/pricing-curve?ChannelModelIDs=999&Step=1000&MaxTokens=5000&At=19:30", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data pricingCurveResponse `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data.Lines) != 1 {
		t.Fatalf("应当 1 条线，got %d", len(body.Data.Lines))
	}
	line := body.Data.Lines[0]
	if line.Available || line.ChannelModelID != 999 {
		t.Fatalf("不存在的 ID 应给不可用线并回吐 ID: %+v", line)
	}
	if line.Reason == "" || len(line.Cost.Points) != 0 {
		t.Fatalf("不可用线应有 Reason 且无点: %+v", line)
	}
}

// ---------- 会话渠道轨迹：接口层 ----------

var traceCols = []string{"call_time", "status", "channel_id", "channel_name", "channel_model_id",
	"internal_model_id", "input_tokens", "cache_read_tokens", "output_tokens", "cost_credits", "sale_credits"}

func traceRow(t *testing.T, at time.Time, status string, chID int64, chName string, cmID int64, model string,
	cost, sale float64) []driver.Value {
	t.Helper()
	return []driver.Value{at, status, chID, chName, cmID, model, int64(1000), int64(0), int64(100), cost, sale}
}

// TestHandleCostSessionTrace_ActualSwitch 断言：真实切换过的会话给出切换点，且不预测打平点。
func TestHandleCostSessionTrace_ActualSwitch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	expectSwitchOn(mock)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM billing_records WHERE session_id = $1`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(3)))
	base := pricingTestAt(t)
	rows := sqlmock.NewRows(traceCols).
		AddRow(traceRow(t, base, "completed", int64(5), "渠道A", int64(900), "gpt-4o-int", 1.5, 2.0)...).
		AddRow(traceRow(t, base.Add(time.Minute), "completed", int64(5), "渠道A", int64(900), "gpt-4o-int", 1.5, 2.0)...).
		AddRow(traceRow(t, base.Add(2*time.Minute), "completed", int64(6), "渠道B", int64(901), "gpt-4o-int", 0.8, 2.0)...)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM billing_records b`)).
		WithArgs("sess-1", nil).
		WillReturnRows(rows)

	h := NewAdmin(db, nil, nil, nil)
	h.now = func() time.Time { return pricingTestAt(t) }
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/session/sess-1/trace", nil)
	req.SetPathValue("id", "sess-1")
	rec := httptest.NewRecorder()
	h.HandleCostSessionTrace(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data costTraceResponse `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d := body.Data
	if len(d.Points) != 3 || d.Total != 3 || d.Truncated {
		t.Fatalf("点/合计错: points=%d total=%d trunc=%v", len(d.Points), d.Total, d.Truncated)
	}
	if len(d.Segments) != 2 {
		t.Fatalf("应当 2 段: %+v", d.Segments)
	}
	if len(d.ActualSwitches) != 1 || d.ActualSwitches[0].Kind != traceKindActual {
		t.Fatalf("应当 1 个真实切换点: %+v", d.ActualSwitches)
	}
	if d.PredictedBreakEven != nil {
		t.Fatalf("已真实切换过就不应再给预测点: %+v", d.PredictedBreakEven)
	}
	if !d.Points[2].ChannelChanged || d.Points[1].ChannelChanged {
		t.Fatalf("ChannelChanged 标记错: %+v", d.Points)
	}
	if d.Points[2].CumulativeCostCredits != 3.8 {
		t.Fatalf("累计成本=%v，want 3.8", d.Points[2].CumulativeCostCredits)
	}
	if d.SwitchKindActual != "actual" || d.SwitchKindPredicted != "predicted" {
		t.Fatalf("枚举字段错: %+v", d)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("查询期望未满足: %v", err)
	}
}

// TestHandleCostSessionTrace_PredictedBreakEven 断言：从未切换过 → 给预测打平点（Kind=predicted），
// 且打平点来自「替代渠道更便宜」的交叉位置。
func TestHandleCostSessionTrace_PredictedBreakEven(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	expectSwitchOn(mock)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM billing_records WHERE session_id = $1`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(2)))
	base := pricingTestAt(t)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM billing_records b`)).
		WithArgs("sess-2", nil).
		WillReturnRows(sqlmock.NewRows(traceCols).
			AddRow(traceRow(t, base, "completed", int64(5), "渠道A", int64(900), "gpt-4o-int", 1.5, 2.0)...).
			AddRow(traceRow(t, base.Add(time.Minute), "completed", int64(5), "渠道A", int64(900), "gpt-4o-int", 1.5, 2.0)...))
	// 打平点计算：全局时段/分档 → 同模型所有渠道
	expectGlobalTimeAndTiers(mock)
	// 实际渠道 900：高峰 1.5、输入 1/输出 4/缓存 0.1；替代 901：输入 0.2（分档/时段相同 → 全程更便宜）。
	mock.ExpectQuery(regexp.QuoteMeta(`WHERE cm.deleted_at IS NULL AND cm.internal_model_id = $1`)).
		WithArgs("gpt-4o-int").
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "internal_model_id", "cost_rates", "time_config", "context_tiers"}).
			AddRow(int64(900), int64(5), "渠道A", "gpt-4o-int", []byte(`{"input":1,"output":4,"cache_read":0.1}`), nil, nil).
			AddRow(int64(901), int64(6), "渠道B", "gpt-4o-int", []byte(`{"input":0.2,"output":0.8,"cache_read":0.02}`), nil, nil))

	h := NewAdmin(db, nil, nil, nil)
	h.now = func() time.Time { return pricingTestAt(t) }
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/cost/session/sess-2/trace?Step=1000&MaxTokens=20000&HitRate=0.5&At=19:30", nil)
	req.SetPathValue("id", "sess-2")
	rec := httptest.NewRecorder()
	h.HandleCostSessionTrace(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data costTraceResponse `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d := body.Data
	if len(d.ActualSwitches) != 0 {
		t.Fatalf("没切过就不应有真实切换点: %+v", d.ActualSwitches)
	}
	be := d.PredictedBreakEven
	if be == nil {
		t.Fatalf("没切过应当给预测打平点: %s", rec.Body.String())
	}
	if be.Kind != traceKindPredicted || !be.Predicted {
		t.Fatalf("预测点必须与真实切换点区分: %+v", be)
	}
	if be.ChannelModelID != 900 || be.AlternativeChannelModelID != 901 {
		t.Fatalf("打平点两端渠道错: %+v", be)
	}
	if !be.Matched || be.Tokens != 1000 {
		t.Fatalf("替代渠道从第一个采样点就更便宜 → Tokens 应为 1000: %+v", be)
	}
	if be.MaxTokens != 20000 || be.Step != 1000 {
		t.Fatalf("采样参数应回吐: %+v", be)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("查询期望未满足: %v", err)
	}
}

// TestHandleCostSessionTrace_NoCrossing 断言：没有更便宜的替代渠道时 Matched=false 且 Reason 有值。
func TestHandleCostSessionTrace_NoCrossing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	expectSwitchOn(mock)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM billing_records WHERE session_id = $1`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))
	base := pricingTestAt(t)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM billing_records b`)).
		WithArgs("sess-3", nil).
		WillReturnRows(sqlmock.NewRows(traceCols).
			AddRow(traceRow(t, base, "completed", int64(5), "渠道A", int64(900), "gpt-4o-int", 1.5, 2.0)...))
	expectGlobalTimeAndTiers(mock)
	mock.ExpectQuery(regexp.QuoteMeta(`WHERE cm.deleted_at IS NULL AND cm.internal_model_id = $1`)).
		WithArgs("gpt-4o-int").
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "internal_model_id", "cost_rates", "time_config", "context_tiers"}).
			AddRow(int64(900), int64(5), "渠道A", "gpt-4o-int", []byte(`{"input":1,"output":4,"cache_read":0.1}`), nil, nil).
			AddRow(int64(901), int64(6), "渠道B", "gpt-4o-int", []byte(`{"input":9,"output":40,"cache_read":1}`), nil, nil))

	h := NewAdmin(db, nil, nil, nil)
	h.now = func() time.Time { return pricingTestAt(t) }
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/cost/session/sess-3/trace?Step=1000&MaxTokens=20000&At=19:30", nil)
	req.SetPathValue("id", "sess-3")
	rec := httptest.NewRecorder()
	h.HandleCostSessionTrace(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data costTraceResponse `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	be := body.Data.PredictedBreakEven
	if be == nil || be.Matched || be.Tokens != 0 || be.Reason == "" {
		t.Fatalf("无交叉点应 Matched=false 且有 Reason: %+v", be)
	}
}

// TestHandleCostSessionTrace_SingleChannelOnly 只有一个渠道配置 → 无对比对象，Reason 说明。
func TestHandleCostSessionTrace_SingleChannelOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	expectSwitchOn(mock)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM billing_records WHERE session_id = $1`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))
	base := pricingTestAt(t)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM billing_records b`)).
		WithArgs("sess-4", nil).
		WillReturnRows(sqlmock.NewRows(traceCols).
			AddRow(traceRow(t, base, "completed", int64(5), "渠道A", int64(900), "gpt-4o-int", 1.5, 2.0)...))
	expectGlobalTimeAndTiers(mock)
	mock.ExpectQuery(regexp.QuoteMeta(`WHERE cm.deleted_at IS NULL AND cm.internal_model_id = $1`)).
		WithArgs("gpt-4o-int").
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "internal_model_id", "cost_rates", "time_config", "context_tiers"}).
			AddRow(int64(900), int64(5), "渠道A", "gpt-4o-int", []byte(`{"input":1,"output":4,"cache_read":0.1}`), nil, nil))

	h := NewAdmin(db, nil, nil, nil)
	h.now = func() time.Time { return pricingTestAt(t) }
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/session/sess-4/trace?MaxTokens=5000&At=19:30", nil)
	req.SetPathValue("id", "sess-4")
	rec := httptest.NewRecorder()
	h.HandleCostSessionTrace(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data costTraceResponse `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	be := body.Data.PredictedBreakEven
	if be == nil || be.Matched || !contains(be.Reason, "只有一条渠道") {
		t.Fatalf("单渠道应给出无对比对象的说明: %+v", be)
	}
}

// TestHandleCostSessionTrace_Truncated 断言 Limit 生效时 Truncated=true 并回报 Total。
func TestHandleCostSessionTrace_Truncated(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	expectSwitchOn(mock)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM billing_records WHERE session_id = $1`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(5)))
	base := pricingTestAt(t)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM billing_records b`)).
		WithArgs("sess-5", 2).
		WillReturnRows(sqlmock.NewRows(traceCols).
			AddRow(traceRow(t, base, "completed", int64(5), "渠道A", int64(900), "m1", 1, 1)...).
			AddRow(traceRow(t, base.Add(time.Minute), "completed", int64(5), "渠道A", int64(900), "m1", 1, 1)...))
	// 截断后仍无切换 → 会进预测；给它准备最少的期望（同模型只有一条 → 直接给说明）。
	expectGlobalTimeAndTiers(mock)
	mock.ExpectQuery(regexp.QuoteMeta(`WHERE cm.deleted_at IS NULL AND cm.internal_model_id = $1`)).
		WithArgs("m1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "internal_model_id", "cost_rates", "time_config", "context_tiers"}).
			AddRow(int64(900), int64(5), "渠道A", "m1", []byte(`{"input":1,"output":4,"cache_read":0.1}`), nil, nil))

	h := NewAdmin(db, nil, nil, nil)
	h.now = func() time.Time { return pricingTestAt(t) }
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/session/sess-5/trace?Limit=2&MaxTokens=5000&At=19:30", nil)
	req.SetPathValue("id", "sess-5")
	rec := httptest.NewRecorder()
	h.HandleCostSessionTrace(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data costTraceResponse `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.Data.Truncated || body.Data.Total != 5 || len(body.Data.Points) != 2 || body.Data.Limit != 2 {
		t.Fatalf("截断信息错: total=%d points=%d trunc=%v limit=%d",
			body.Data.Total, len(body.Data.Points), body.Data.Truncated, body.Data.Limit)
	}
}

// TestHandleCostSessionTrace_NilDB 未装配 DB：开关读不到 → 关闭 → 403（默认安全态）。
func TestHandleCostSessionTrace_NilDB(t *testing.T) {
	h := NewAdmin(nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/session/sess-x/trace", nil)
	req.SetPathValue("id", "sess-x")
	rec := httptest.NewRecorder()
	h.HandleCostSessionTrace(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("无 DB 时开关按关闭处理，应 403，got %d", rec.Code)
	}
}

// TestPredictBreakEven_UnresolvedChannel 账单没解析到渠道内部模型 → 明确说明，不瞎算。
func TestPredictBreakEven_UnresolvedChannel(t *testing.T) {
	h := NewAdmin(nil, nil, nil, nil)
	got := h.predictBreakEven(t.Context(), costTracePoint{ChannelID: 5}, pricingCurveParams{Step: 1000}, 10000)
	if got == nil || got.Matched || !contains(got.Reason, "未解析到渠道内部模型") {
		t.Fatalf("应给出无法预测的原因: %+v", got)
	}
	if got.Kind != traceKindPredicted {
		t.Fatalf("Kind 必须是 predicted: %+v", got)
	}
}

// ---------- 开关的读写（系统配置） ----------

// TestHandleGetSystemConfig_CostViewsSwitch 断言系统配置 GET 下发 Console.MultichannelTrace，
// 且库中值为 true 时如实返回 true。
func TestHandleGetSystemConfig_CostViewsSwitch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, nil)

	expectConfigRow(mock, "billing.r", []byte(`10000`))
	expectConfigRow(mock, "billing.cny_rate", []byte(`6.8`))
	expectConfigRow(mock, "billing.context_tiers", []byte(`[]`))
	expectConfigRow(mock, "billing.time_config", []byte(`{"Timezone":"Asia/Shanghai","Default":1}`))
	expectConfigRow(mock, "routing.cost_sample", []byte(`{"input":10000,"cache_read":9000,"output":1000}`))
	expectConfigRow(mock, "session.expiration", []byte(`{"default_ttl_minutes":5,"context_tiers":[],"turn_tiers":[]}`))
	expectConfigRow(mock, "gateway.upstream_response_header_whitelist", []byte(`[]`))
	// 开关键最后读（Handler 里 Console 块在 map 字面量末尾求值）；
	// 注意 sysConfigRaw 的 SQL 是 `key = $1`（带空格），与 admin_test 里的 helper 不同源。
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
		WithArgs(costViewsSwitchKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`true`)))

	rec := httptest.NewRecorder()
	admin.HandleGetSystemConfig(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/configs/system", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Console struct {
				MultichannelTrace bool `json:"MultichannelTrace"`
			} `json:"Console"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.Data.Console.MultichannelTrace {
		t.Fatalf("库中为 true 时应下发 true: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("查询期望未满足: %v", err)
	}
}

// TestHandleGetSystemConfig_CostViewsSwitchMissing 断言键缺失时默认 false（默认关闭 = 现状不变）。
func TestHandleGetSystemConfig_CostViewsSwitchMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, nil)

	expectConfigRow(mock, "billing.r", []byte(`10000`))
	expectConfigRow(mock, "billing.cny_rate", []byte(`6.8`))
	expectConfigRow(mock, "billing.context_tiers", []byte(`[]`))
	expectConfigRow(mock, "billing.time_config", []byte(`{"Timezone":"Asia/Shanghai","Default":1}`))
	expectConfigRow(mock, "routing.cost_sample", []byte(`{"input":10000,"cache_read":9000,"output":1000}`))
	expectConfigRow(mock, "session.expiration", []byte(`{"default_ttl_minutes":5,"context_tiers":[],"turn_tiers":[]}`))
	expectConfigRow(mock, "gateway.upstream_response_header_whitelist", []byte(`[]`))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
		WithArgs(costViewsSwitchKey).
		WillReturnError(sql.ErrNoRows) // 缺失 → 默认关闭

	rec := httptest.NewRecorder()
	admin.HandleGetSystemConfig(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/configs/system", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if contains(rec.Body.String(), `"MultichannelTrace":true`) {
		t.Fatalf("缺失时应默认 false: %s", rec.Body.String())
	}
}

// TestHandlePutSystemConfig_CostViewsSwitch 断言带 Console 块时按开关键落库，不带则完全不动（老客户端行为不变）。
func TestHandlePutSystemConfig_CostViewsSwitch(t *testing.T) {
	baseBody := `{"Billing":{"R":10000,"CnyRate":6.8,"ContextTiers":[],` +
		`"TimeConfig":{"Timezone":"Asia/Shanghai","Default":1},` +
		`"CostSample":{"input":12345,"cache_read":6000,"output":2000}},` +
		`"SessionExpiration":{"default_ttl_minutes":5,"context_tiers":[{"max_tokens":2000,"ttl_minutes":5}],` +
		`"turn_tiers":[{"max_turns":2,"ttl_minutes":5}]},` +
		`"UpstreamResponseHeaderWhitelist":["traceparent"]}`

	// ① 带 Console 块 → 多写一个开关键
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, testUserIDFrom)
	for _, key := range []string{
		"billing.r", "billing.cny_rate", "billing.context_tiers", "billing.time_config",
		"routing.cost_sample", "session.expiration", "gateway.upstream_response_header_whitelist",
		costViewsSwitchKey,
	} {
		mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO sys_configs(key, value, updated_by)`)).
			WithArgs(key, sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	body := strings.TrimSuffix(baseBody, "}") + `,"Console":{"MultichannelTrace":true}}`
	rec := httptest.NewRecorder()
	admin.HandlePutSystemConfig(rec, httptest.NewRequest(http.MethodPut, "/api/v1/admin/configs/system", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("开关键未落库: %v", err)
	}

	// ② 不带 Console 块 → 只有原来的七个键（逐位兼容）
	db2, mock2, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db2.Close()
	admin2 := NewAdmin(db2, billing.NewSqlStore(db2), nil, testUserIDFrom)
	for _, key := range []string{
		"billing.r", "billing.cny_rate", "billing.context_tiers", "billing.time_config",
		"routing.cost_sample", "session.expiration", "gateway.upstream_response_header_whitelist",
	} {
		mock2.ExpectExec(regexp.QuoteMeta(`INSERT INTO sys_configs(key, value, updated_by)`)).
			WithArgs(key, sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	rec2 := httptest.NewRecorder()
	admin2.HandlePutSystemConfig(rec2, httptest.NewRequest(http.MethodPut, "/api/v1/admin/configs/system", strings.NewReader(baseBody)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatalf("老请求不应多写开关键: %v", err)
	}
}

var _ = billing.Rates{}
