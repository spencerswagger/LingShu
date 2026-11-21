package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/domain/billing/mult"
)

// ===== ParseV2Cost =====

const fullV2Shape = `{
  "text": {
    "input":  {"price": 1.25, "cache": {"read": 0.10, "write": 1.875}},
    "output": {"price": 10.0, "reasoning": 40.0}
  },
  "web_search": {"price": 25.0},
  "multipliers": [
    {"id": "night", "when": {"utc_hours": {"gte": "22:00"}}, "factor": 0.5},
    {"id": "bad", "when": {"unknown_key": {"equals": "x"}}, "factor": 9}
  ]
}`

func TestParseV2Cost_FullShape(t *testing.T) {
	c, err := ParseV2Cost([]byte(fullV2Shape))
	if err != nil {
		t.Fatalf("完整形状应解析成功: %v", err)
	}
	if c.Input != 1.25 || c.Output != 10 || c.CacheRead != 0.10 || c.CacheWrite != 1.875 {
		t.Fatalf("四价解析错: %+v", c)
	}
	if c.Reasoning != 40 {
		t.Fatalf("reasoning 留位解析错: %v", c.Reasoning)
	}
	if c.WebSearch == nil || *c.WebSearch != 25 {
		t.Fatalf("web_search 应照存: %+v", c.WebSearch)
	}
	if len(c.Multipliers) != 1 {
		t.Fatalf("坏规则应丢弃只剩 1 条: %+v", c.Multipliers)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0].Reason, "未知条件键") {
		t.Fatalf("坏规则应有告警留痕: %+v", c.Warnings)
	}
}

func TestParseV2Cost_CacheWriteMissingFallsBackZero(t *testing.T) {
	// 四项定案①空价回落：cache.write 缺省 = 0（宁少不多收，运营填价后才产生金额）。
	c, err := ParseV2Cost([]byte(`{"text":{"input":{"price":1,"cache":{"read":0.1}},"output":{"price":2}}}`))
	if err != nil {
		t.Fatalf("缺 cache.write 应按空价回落成功: %v", err)
	}
	if c.CacheWrite != 0 {
		t.Fatalf("缺省 cache_write 应为 0: %v", c.CacheWrite)
	}
}

func TestParseV2Cost_AbsentMultipliersNoWarning(t *testing.T) {
	// multipliers 键缺省 = 合法「无规则」：不得背「非数组」噪音告警
	// （每个干净形状都会被 warnMult 打日志——回归钉）。
	c, err := ParseV2Cost([]byte(`{"text":{"input":{"price":1,"cache":{"read":0.1}},"output":{"price":2}}}`))
	if err != nil {
		t.Fatalf("解析：%v", err)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("键缺省不应告警：%+v", c.Warnings)
	}
	if len(c.Multipliers) != 0 {
		t.Fatalf("无规则应为空：%d", len(c.Multipliers))
	}
	// 真的非数组仍告警（穿到 v2Parse 的可见性口径不变）。
	c2, err := ParseV2Cost([]byte(`{"text":{"input":{"price":1,"cache":{"read":0.1}},"output":{"price":2}},"multipliers":{"a":1}}`))
	if err != nil {
		t.Fatalf("解析：%v", err)
	}
	if len(c2.Warnings) != 1 {
		t.Fatalf("非数组形状应告警：%+v", c2.Warnings)
	}
}

func TestParseV2Cost_RequiredFields(t *testing.T) {
	bad := map[string]string{
		"缺 text 节点":       `{}`,
		"缺 input":         `{"text":{"output":{"price":2}}}`,
		"缺 output":        `{"text":{"input":{"price":1,"cache":{"read":0.1}}}}`,
		"缺 cache.read 必填": `{"text":{"input":{"price":1,"cache":{"write":1}},"output":{"price":2}}}`,
		"缺 cache 节点":      `{"text":{"input":{"price":1},"output":{"price":2}}}`,
		"负价":              `{"text":{"input":{"price":-1,"cache":{"read":0}},"output":{"price":2}}}`,
		"价全空":             `{"text":{"input":{},"output":{}}}`,
	}
	for name, raw := range bad {
		if _, err := ParseV2Cost([]byte(raw)); err == nil {
			t.Fatalf("%s 必须解析失败（调用方回落旧三段）", name)
		}
	}
	// 显式全 0 价（免费模型）是合法形状：cache.read 必须显式写 0。
	c, err := ParseV2Cost([]byte(`{"text":{"input":{"price":0,"cache":{"read":0,"write":0}},"output":{"price":0}}}`))
	if err != nil || c.Input != 0 || c.CacheRead != 0 {
		t.Fatalf("显式零价应合法: %+v err=%v", c, err)
	}
}

func TestParseV2Cost_ToleratesUnknownNodes(t *testing.T) {
	// image/audio/video/overrides 等目录节点一律容忍忽略（元数据归 catalog 暂存区）。
	raw := `{"text":{"input":{"price":1,"cache":{"read":0.1}},"output":{"price":2}},
	          "image":{"price":5},"overrides":[],"context_window":128000}`
	if _, err := ParseV2Cost([]byte(raw)); err != nil {
		t.Fatalf("未知节点应容忍: %v", err)
	}
}

// ===== ComputeCreditsV2 =====

func TestComputeCreditsV2_FourSegmentsAndFactor(t *testing.T) {
	u := Usage{Input: 900, Output: 200, CacheRead: 100, CacheWrite: 50}
	p := V2Prices{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1.2}
	// 分桶互斥直接求和：900×1 + 200×2 + 100×0.1 + 50×1.2 = 900+400+10+60 = 1370
	// 命中因子 0.5：1370×0.5÷10000 = 0.0685。
	got, err := ComputeCreditsV2(u, p, 0.5, 10000)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if got != 0.0685 {
		t.Fatalf("四段×因子÷R = %v，期望 0.0685", got)
	}
}

func TestComputeCreditsV2_CacheWriteEmptyPrice(t *testing.T) {
	// 空价回落：cache_write 段价 0 时该段不产生金额（宁少不多收）。
	u := Usage{Input: 900, Output: 200, CacheRead: 100, CacheWrite: 5000}
	p := V2Prices{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 0}
	got, err := ComputeCreditsV2(u, p, 1, 10000)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if got != 0.131 { // (900×1+200×2+100×0.1)÷10000
		t.Fatalf("空写价应只计三段: %v", got)
	}
}

func TestComputeCreditsV2_Guards(t *testing.T) {
	if _, err := ComputeCreditsV2(Usage{}, V2Prices{}, 1, 0); err == nil {
		t.Fatalf("R<=0 必须拒绝")
	}
	if _, err := ComputeCreditsV2(Usage{}, V2Prices{Input: -1}, 1, 10000); err == nil {
		t.Fatalf("负价必须拒绝")
	}
}

// ===== requestCreditsV2（结算与预扣共用的求值收口）=====

func TestRequestCredits_V2Branch(t *testing.T) {
	svc := &Service{cfg: newFakeConfig(), clock: func() time.Time { return time.Now() }}
	req := RecordReq{
		Tokens: Usage{Input: 900, Output: 200, CacheRead: 100, CacheWrite: 50},
		Rates:  V2Prices{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1.2}.LegacyView(),
		V2: PricingFromV2Cost(&V2Cost{
			Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1.2,
			Multipliers: mult.MustParseForTest(`[{"id":"night","when":{"utc_hours":{"gte":"22:00"}},"factor":0.5}]`),
		}, time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC)),
	}
	credits, timeCoeff, ctxCoeff, r, err := svc.requestCredits(req)
	if err != nil {
		t.Fatalf("v2 求值: %v", err)
	}
	if credits != 0.0685 {
		t.Fatalf("credits=%v 期望 0.0685（23:00 UTC 命中夜窗 0.5）", credits)
	}
	if timeCoeff != 1 || ctxCoeff != 1 {
		t.Fatalf("v2 路径 coefficients 应为乘法单位元: %v %v", timeCoeff, ctxCoeff)
	}
	if r != 10000 {
		t.Fatalf("R 应透传: %v", r)
	}
	if len(req.V2.Eval.Hits) != 1 || req.V2.Eval.Hits[0].Rule != "night" {
		t.Fatalf("求值留痕应回填: %+v", req.V2.Eval.Hits)
	}
	if req.V2.Eval.FactorFor("text") != 0.5 {
		t.Fatalf("text 因子应为 0.5: %v", req.V2.Eval.FactorFor("text"))
	}
}

func TestRequestCredits_V2BranchOutsideWindow(t *testing.T) {
	svc := &Service{cfg: newFakeConfig(), clock: func() time.Time { return time.Now() }}
	req := RecordReq{
		Tokens: Usage{Input: 1000},
		V2: PricingFromV2Cost(&V2Cost{
			Input: 1, CacheRead: 0,
			Multipliers: mult.MustParseForTest(`[{"id":"night","when":{"utc_hours":{"gte":"22:00"}},"factor":0.5}]`),
		}, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)),
	}
	credits, _, _, _, err := svc.requestCredits(req)
	if err != nil {
		t.Fatalf("v2 求值: %v", err)
	}
	if credits != 0.1 {
		t.Fatalf("窗外不应折价: %v", credits)
	}
}

func TestRequestCredits_V2MissingEvalAtRejected(t *testing.T) {
	svc := &Service{cfg: newFakeConfig(), clock: func() time.Time { return time.Now() }}
	req := RecordReq{Tokens: Usage{Input: 100}, V2: PricingFromV2Cost(&V2Cost{Input: 1, CacheRead: 0}, time.Time{})}
	if _, _, _, _, err := svc.requestCredits(req); err == nil {
		t.Fatalf("缺 EvalAt 必须拒绝（时间条件规则会按零值时刻静默漏判）")
	}
}

// ===== Record v2 全链（四段分桶 + 因子 + coefficients=1/1 + CostV2）=====

func TestRecord_V2Pricing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	const billingID = "bill-v2-1"
	usage := Usage{Input: 900, Output: 200, CacheRead: 100, CacheWrite: 50}
	prices := V2Prices{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1.2}
	// 1370 × 0.5 ÷ 10000 = 0.0685（成本口径独立求值，同价不同规则）。
	before, after := 10.0, 9.9315

	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(insertBill.String()).WithArgs(
		sqlmock.AnyArg(), // id
		billingID, testUserID, "sale", sqlmock.AnyArg(), "ext-model", sqlmock.AnyArg(), "int-model",
		sqlmock.AnyArg(),                 // session_id
		sqlmock.AnyArg(),                 // session_name
		sqlmock.AnyArg(),                 // call_time
		v2TokensJSON(t, usage),           // tokens（cache_write 键 omitempty：非零才落键）
		v2RatesJSON(t, prices),           // rates = LegacyView 三段
		[]byte(`{"time":1,"context":1}`), // coefficients（v2 恒单位元，真因子在 price_eval）
		int64(10000),                     // r_value
		685.0,                            // raw_total（未除 R：1370×0.5）
		0.0685,                           // credits_consumed
		0.0685,                           // cost_credits（CostV2 同价夜窗 0.5）
		before, after, "completed",
		sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
		nil, nil, nil, nil, nil, nil, CostSourceNone, nil, nil).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))

	credit := &fakeCredit{before: before, after: after}
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t))
	svc := newTestService(db, credit, newFakeConfig(), now)

	night := time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC)
	sale := PricingFromV2Cost(&V2Cost{
		Input: prices.Input, Output: prices.Output, CacheRead: prices.CacheRead, CacheWrite: prices.CacheWrite,
		Multipliers: mult.MustParseForTest(`[{"id":"night","when":{"utc_hours":{"gte":"22:00"}},"factor":0.5}]`),
	}, night)
	cost := PricingFromV2Cost(&V2Cost{
		Input: prices.Input, Output: prices.Output, CacheRead: prices.CacheRead, CacheWrite: prices.CacheWrite,
		Multipliers: mult.MustParseForTest(`[{"id":"night-cost","when":{"utc_days":{"equals":"fri"}},"factor":0.5}]`),
	}, night)

	rec, err := svc.Record(context.Background(), RecordReq{
		BillingID: billingID, UserID: testUserID, PricingMode: "sale",
		ExternalModel: "ext-model", InternalModelID: "int-model", ChannelKeyID: 1,
		Tokens: usage,
		Rates:  prices.LegacyView(),
		V2:     sale,
		CostV2: cost,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.CreditsConsumed != 0.0685 {
		t.Fatalf("credits=%v 期望 0.0685", rec.CreditsConsumed)
	}
	if rec.CostCredits != 0.0685 {
		t.Fatalf("cost=%v 期望 0.0685（成本层独立规则命中）", rec.CostCredits)
	}
	if rec.Coefficients.Time != 1 || rec.Coefficients.Context != 1 {
		t.Fatalf("v2 行 coefficients 应存 1/1: %+v", rec.Coefficients)
	}
	if rec.RawTotal != 685 {
		t.Fatalf("raw_total=%v 期望 685（未除 R 的原始价值）", rec.RawTotal)
	}
	if len(sale.Eval.Hits) != 1 || sale.Eval.Hits[0].Rule != "night" {
		t.Fatalf("售价求值留痕应回填: %+v", sale.Eval.Hits)
	}
	if len(cost.Eval.Hits) != 1 || cost.Eval.Hits[0].Rule != "night-cost" {
		t.Fatalf("成本求值留痕应回填: %+v", cost.Eval.Hits)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestRecord_LegacyUnchangedWhenNoV2(t *testing.T) {
	// 回归钉：V2/CostV2 均空 → 旧三段公式逐字节不变（既有全量测试已覆盖，
	// 这里补一条「v2 输入存在但字段空」的防御断言）。
	svc := &Service{cfg: newFakeConfig(), clock: func() time.Time { return time.Now() }}
	req := RecordReq{
		Tokens: Usage{Input: 1234, Output: 567},
		Rates:  fullRates(),
	}
	got, timeCoeff, ctxCoeff, r, err := svc.requestCredits(req)
	if err != nil {
		t.Fatalf("legacy: %v", err)
	}
	want, err := ComputeCredits(req.Tokens, req.Rates, 1.0, 1.0, 10000)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if got != want || timeCoeff != 1 || ctxCoeff != 1 || r != 10000 {
		t.Fatalf("legacy 路径漂移: got=%v want=%v %v %v %v", got, want, timeCoeff, ctxCoeff, r)
	}
}

// ===== Usage.CacheWrite 存储契约 =====

func TestUsage_CacheWriteJSONContract(t *testing.T) {
	// omitempty：0 时不落键 —— 旧三段行的 tokens JSONB 形状逐字节不变。
	b, err := json.Marshal(Usage{Input: 1, Output: 2, CacheRead: 3})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"input":1,"output":2,"cache_read":3}` {
		t.Fatalf("零值 cache_write 不得落键: %s", b)
	}
	b, err = json.Marshal(Usage{Input: 1, CacheWrite: 9})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"input":1,"output":0,"cache_read":0,"cache_write":9}` {
		t.Fatalf("非零 cache_write 应落键: %s", b)
	}
	// 历史行缺键回读按 0。
	var u Usage
	if err := json.Unmarshal([]byte(`{"input":5,"output":6,"cache_read":7}`), &u); err != nil || u.CacheWrite != 0 {
		t.Fatalf("历史行缺键应按 0: %+v err=%v", u, err)
	}
	// WireUsage Pascal 契约。
	wb, err := json.Marshal(WireUsage(Usage{Input: 1, CacheWrite: 4}))
	if err != nil || string(wb) != `{"Input":1,"Output":0,"CacheRead":0,"CacheWrite":4}` {
		t.Fatalf("Wire 契约: %s err=%v", wb, err)
	}
	wb0, err := json.Marshal(WireUsage(Usage{Input: 1}))
	if err != nil || string(wb0) != `{"Input":1,"Output":0,"CacheRead":0}` {
		t.Fatalf("Wire 零值不得落键: %s err=%v", wb0, err)
	}
}

func TestLegacyView(t *testing.T) {
	p := V2Prices{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1.2}
	got := p.LegacyView()
	want := Rates{"input": 1.0, "output": 2.0, "cache_read": 0.1}
	if !reflect.DeepEqual(map[string]float64(got), map[string]float64(want)) {
		t.Fatalf("LegacyView=%v want=%v", got, want)
	}
	if err := ValidateRates(got); err != nil {
		t.Fatalf("LegacyView 必须是合法旧三段: %v", err)
	}
}

func TestParseV2Cost_RejectsBadJSON(t *testing.T) {
	if _, err := ParseV2Cost([]byte(`not-json`)); err == nil {
		t.Fatalf("非法 JSON 必须报错")
	}
}

// v2TokensJSON 生成 insertBill tokens 参数的期望值（存储契约 snake 键）。
func v2TokensJSON(t *testing.T, u Usage) []byte {
	t.Helper()
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal tokens: %v", err)
	}
	return b
}

// v2RatesJSON 生成 insertBill rates 参数的期望值（LegacyView 三段）。
func v2RatesJSON(t *testing.T, p V2Prices) []byte {
	t.Helper()
	b, err := json.Marshal(p.LegacyView())
	if err != nil {
		t.Fatalf("marshal rates: %v", err)
	}
	return b
}
