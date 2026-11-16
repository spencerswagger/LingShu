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
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
)

// traceRequest 构造 /trace 请求（path value 已设置），供返修用例复用。
func traceRequest(sid, query string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/session/"+sid+"/trace"+query, nil)
	req.SetPathValue("id", sid)
	return req
}

// decodeTrace 解出 {Code,Data} 壳里的 Data，并顺带断言 Code=0。
func decodeTrace(t *testing.T, rec *httptest.ResponseRecorder) costTraceResponse {
	t.Helper()
	var body struct {
		Code int               `json:"Code"`
		Data costTraceResponse `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if body.Code != 0 {
		t.Fatalf("Code=%d body=%s", body.Code, rec.Body.String())
	}
	return body.Data
}

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

// ---------- 纯函数：打平点插值 ----------

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

// ---------- 通用查询期望 ----------

// expectSwitchOn 保留为空操作：门控已删除，两个只读接口恒可用（用例不再需要预置开关行）。
func expectSwitchOn(mock sqlmock.Sqlmock) {}

func expectGlobalTimeAndTiers(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
		WithArgs("billing.time_config").
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`{"Timezone":"Asia/Shanghai","Default":1.0,"Periodic":[{"Name":"高峰","Start":"18:00","End":"22:00","Coeff":1.5}]}`)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
		WithArgs("billing.context_tiers").
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`[]`)))
}

// ---------- 会话渠道轨迹：接口层 ----------

var traceCols = []string{"call_time", "status", "channel_id", "channel_name", "channel_model_id",
	"internal_model_id", "input_tokens", "cache_read_tokens", "output_tokens", "cost_credits", "sale_credits"}

func traceRow(t *testing.T, at time.Time, status string, chID int64, chName string, cmID int64, model string,
	cost, sale float64) []driver.Value {
	t.Helper()
	return []driver.Value{at, status, chID, chName, cmID, model, int64(1000), int64(0), int64(100), cost, sale}
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
		WithArgs("gpt-4o-int", int64(900)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "internal_model_id", "cost_rates", "time_config", "context_tiers"}).
			AddRow(int64(900), int64(5), "渠道A", "gpt-4o-int", []byte(`{"input":1,"output":4,"cache_read":0.1}`), nil, nil).
			AddRow(int64(901), int64(6), "渠道B", "gpt-4o-int", []byte(`{"input":9,"output":40,"cache_read":1}`), nil, nil))

	h := NewAdmin(db, nil, nil)
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
		WithArgs("m1", int64(900)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "internal_model_id", "cost_rates", "time_config", "context_tiers"}).
			AddRow(int64(900), int64(5), "渠道A", "m1", []byte(`{"input":1,"output":4,"cache_read":0.1}`), nil, nil))

	h := NewAdmin(db, nil, nil)
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

// TestHandleCostSessionTrace_NilDB 未装配 DB 时仍返回 200 + 空轨迹（接口恒可用，不再有开关门控）。
func TestHandleCostSessionTrace_NilDB(t *testing.T) {
	h := NewAdmin(nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/session/sess-x/trace", nil)
	req.SetPathValue("id", "sess-x")
	rec := httptest.NewRecorder()
	h.HandleCostSessionTrace(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("接口应恒可用（无门控），got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data costTraceResponse `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data.Points) != 0 || body.Data.Segments == nil || body.Data.ActualSwitches == nil {
		t.Fatalf("空集合必须是 []，不能是 null: %+v", body.Data)
	}
}

// TestPredictBreakEven_AxisAlwaysReported 断言每条提前返回的分支都带上横轴（不会留 0）。
// ---------- PR #65 Owl 返修用例（P2-1 ~ P2-4）----------

// TestBuildTraceSegments_FailoverIsNotSwitch 请求内 failover（A 429 失败 → B 成功）不是会话级渠道切换。
func TestBuildTraceSegments_FailoverIsNotSwitch(t *testing.T) {
	points := []costTracePoint{
		{TurnNo: 1, Failed: true, Status: billing.StatusFailed, ChannelID: 5, ChannelName: "渠道A", ChannelModelID: 900, InternalModelID: "m1"},
		{TurnNo: 2, ChannelID: 6, ChannelName: "渠道B", ChannelModelID: 901, InternalModelID: "m1", CostCredits: 1},
	}
	_, switches := buildTraceSegments(points)
	if len(switches) != 0 {
		t.Fatalf("请求内 failover 不应算切换: %+v", switches)
	}
}

// TestBuildTraceSegments_RejectInMiddleNoFakeSwitch A(成功) → 0(无渠道拒绝) → A(成功)：0 次切换、无空渠道名。
func TestBuildTraceSegments_RejectInMiddleNoFakeSwitch(t *testing.T) {
	points := []costTracePoint{
		{TurnNo: 1, ChannelID: 5, ChannelName: "渠道A", ChannelModelID: 900, InternalModelID: "m1"},
		{TurnNo: 2, Failed: true, Status: billing.StatusFailed, ChannelID: 0},
		{TurnNo: 3, ChannelID: 5, ChannelName: "渠道A", ChannelModelID: 900, InternalModelID: "m1"},
	}
	_, switches := buildTraceSegments(points)
	if len(switches) != 0 {
		t.Fatalf("A→空→A 不应产生切换（旧实现会算 2 次假切换）: %+v", switches)
	}
}

// TestBuildTraceSegments_SwitchAfterFailoverAtSuccessPoint 成功轮 A → (下一请求) B 失败 → B 成功：
// 真实切换只有 1 次，且挂在**成功点**上（失败点只是同一次请求内的候选重试）。
func TestBuildTraceSegments_SwitchAfterFailoverAtSuccessPoint(t *testing.T) {
	points := []costTracePoint{
		{TurnNo: 1, ChannelID: 5, ChannelName: "渠道A", ChannelModelID: 900, InternalModelID: "m1"},
		{TurnNo: 2, Failed: true, Status: billing.StatusFailed, ChannelID: 6, ChannelName: "渠道B", ChannelModelID: 901, InternalModelID: "m1"},
		{TurnNo: 3, ChannelID: 6, ChannelName: "渠道B", ChannelModelID: 901, InternalModelID: "m1"},
	}
	_, switches := buildTraceSegments(points)
	if len(switches) != 1 {
		t.Fatalf("应只有 1 次真实切换: %+v", switches)
	}
	sw := switches[0]
	if sw.TurnNo != 3 || sw.FromChannelID != 5 || sw.ToChannelID != 6 {
		t.Fatalf("切换点必须落在成功点（TurnNo=3）且带前后渠道: %+v", sw)
	}
}

// TestLoadSiblingChannelModels_FiltersDeadChannels 候选渠道必须排除软删/停用渠道，但**当前渠道那条要保留**。

// TestHandleCostSessionTrace_ChannelRetired 会话轨迹在「渠道体系下线」后的口径（0024）：
//   - 逐条点不带渠道维度（channel_id / channel_name / channel_model_id 恒 0/空）；
//   - 因此不可能再出现「渠道切换」事件；
//   - 预测打平点必须回一个**显式不可用 + 原因**的对象（不是 null，也不是 0）。
func TestHandleCostSessionTrace_ChannelRetired(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM billing_records WHERE session_id = $1`)).
		WithArgs("sess-r").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(2)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT b.call_time`)).
		WithArgs("sess-r", nil).
		WillReturnRows(sqlmock.NewRows([]string{
			"call_time", "status", "channel_id", "channel_name", "channel_model_id", "internal_model_id",
			"input_tokens", "cache_read_tokens", "output_tokens", "cost_credits", "sale_credits",
		}).
			AddRow(time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC), "completed", int64(0), "", int64(0), "gpt-4o-int",
				int64(10), int64(0), int64(5), 0.1, 0.2).
			AddRow(time.Date(2026, 10, 25, 10, 1, 0, 0, time.UTC), "completed", int64(0), "", int64(0), "gpt-4o-int",
				int64(20), int64(0), int64(10), 0.2, 0.4))

	h := NewAdmin(db, nil, nil)
	rec := httptest.NewRecorder()
	traceReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/session/sess-r/trace", nil)
	traceReq.SetPathValue("id", "sess-r")
	h.HandleCostSessionTrace(rec, traceReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data costTraceResponse `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data.Points) != 2 {
		t.Fatalf("points=%d, want 2", len(body.Data.Points))
	}
	for i, pt := range body.Data.Points {
		if pt.ChannelID != 0 || pt.ChannelName != "" || pt.ChannelModelID != 0 {
			t.Fatalf("第 %d 点不应带渠道维度（表已删）: %+v", i, pt)
		}
	}
	if len(body.Data.ActualSwitches) != 0 {
		t.Fatalf("渠道已下线，不可能有切换事件: %+v", body.Data.ActualSwitches)
	}
	if body.Data.PredictedBreakEven == nil {
		t.Fatalf("预测打平点必须是显式对象（前端要能展示原因），不能是 null")
	}
	if body.Data.PredictedBreakEven.Predicted {
		t.Fatalf("预测打平点不可计算，Predicted 必须为 false: %+v", body.Data.PredictedBreakEven)
	}
	if body.Data.PredictedBreakEven.Reason == "" {
		t.Fatalf("不可用的预测必须带原因: %+v", body.Data.PredictedBreakEven)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
