package v3billing

// v2 计价行（models.dev v2 对齐，四项定案 2026-11-20）的结算/补收专项：
//   - 售价 sale_v2 存在 → 四段价 + multipliers，时段/分档不叠加；
//   - tokens 四桶互斥拆分（cache_write 独立分桶，空价回落 = 0）；
//   - rates 列落 LegacyView 三段视图（console 渲染兼容）；
//   - 求值留痕 price_eval 在真求值后随 UPDATE 写入（幂等命中不覆盖）。
//
// 夹具口径：fakeRec 不会跑真 requestCredits（那是 billing 包的职责，已被
// costv2_test / service_test 钉死），这里用 evalRec 在 Record 里补齐
// 「求值收口已发生」的下游效果（Evaluated=true + Eval 填充），验证 settleOne 的
// 组包与留痕落库参数。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/domain/billing"
)

// evalRec 包着 *fakeRec：模拟「requestCredits 已求值」的下游效果
// （真 requestCreditsV2 的行为由 billing 包用例钉死，这里只补下游形态）。
type evalRec struct{ inner *fakeRec }

func (f *evalRec) Record(ctx context.Context, req billing.RecordReq) (*billing.Record, error) {
	if req.V2 != nil {
		req.V2.Eval = billing.EvaluateV2(req.V2, req.Tokens)
		req.V2.SetEvaluatedForTest()
	}
	if req.CostV2 != nil {
		req.CostV2.Eval = billing.EvaluateV2(req.CostV2, req.Tokens)
		req.CostV2.SetEvaluatedForTest()
	}
	rec, err := f.inner.Record(ctx, req)
	if rec != nil {
		rec.RValue = 10000 // 留痕 R 与复算口径同源
	}
	return rec, err
}

// expectPriceHitV2 装配带 sale_v2 的命中行（time/context 恒 null：v2 行不读它们）。
func (f *fixture) expectPriceHitV2(byBinding bool, wantValue, extName string, rates map[string]float64, saleV2 string) {
	raw, err := json.Marshal(rates)
	if err != nil {
		f.t.Fatalf("rates 序列化失败：%v", err)
	}
	f.mock.ExpectQuery(priceQueryPattern(byBinding)).WithArgs(wantValue).
		WillReturnRows(f.mock.NewRows([]string{"external_name", "sale_rates", "time_config", "context_tiers", "sale_v2"}).
			AddRow(extName, raw, []byte("null"), []byte("null"), []byte(saleV2)))
}

// expectPriceChainV2 与 expectPriceChain 同构，但命中行带 sale_v2。
func (f *fixture) expectPriceChainV2(reqModel, servedModel string, groups []groupNode, routeID int64, hitValue, extName string, rates map[string]float64, saleV2 string) {
	for _, c := range priceCandidates(reqModel, servedModel, groups, routeID) {
		if c.value == hitValue {
			f.expectPriceHitV2(c.byBinding, c.value, extName, rates, saleV2)
			return
		}
		f.expectPriceMiss(c.byBinding, c.value)
	}
	f.t.Fatalf("候选链里没有期望命中的值 %q（req=%q served=%q）", hitValue, reqModel, servedModel)
}

// expectCostHitV2 装配带 cost_v2 的成本命中行。
func (f *fixture) expectCostHitV2(provider, model string, costRates map[string]float64, costV2 string) {
	raw, _ := json.Marshal(costRates)
	f.mock.ExpectQuery("FROM internal_models WHERE ref =").
		WithArgs(provider + "/" + model).
		WillReturnRows(f.mock.NewRows([]string{"cost_rates", "cost_v2"}).AddRow(raw, []byte(costV2)))
}

// expectSettledV2 断言留痕 UPDATE 的第 4 参逐字节等于期望 JSON。
func (f *fixture) expectSettledV2(ledgerID string, cost float64, wantEval string) {
	f.mock.ExpectExec("UPDATE billing_records SET magpie_request_id").
		WithArgs(ledgerID, cost, "v3-"+ledgerID, []byte(wantEval)).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// 固定求值时刻：留痕 JSON 里带 At，必须确定性可比。
var v2EvalAt = time.Date(2026, 11, 20, 3, 30, 0, 0, time.UTC)

// 最小合法 v2 售价形状（cache_write 空价回落 0，无规则 → 因子恒 1）。
const v2SaleShape = `{"text":{"input":{"price":1,"cache":{"read":0.1}},"output":{"price":2}},"multipliers":[]}`

// TestSettleV2_FourBucketsAndTrace v2 行主链：
// 四桶互斥拆分 + multipliers 命中 + LegacyView 留 rates + price_eval 留痕随 UPDATE 落库。
func TestSettleV2_FourBucketsAndTrace(t *testing.T) {
	f := newFixture(t)
	inner := f.rec
	f.p.bill = &evalRec{inner: inner}
	lg := "lg-v2main"
	served := "gpt-x"
	caller := "ck-v2"
	// 账本行：tokens_in=1000（含 cache_read 100 + cache_write 50）、out=200。
	f.expectOnceRows([]any{lg, v2EvalAt, "prov", served, served, caller,
		int64(1), int64(1000), int64(200), int64(100), int64(50), float64(0)})
	f.expectGroups()
	f.expectToken(caller)
	// 成本：cost_v2 存在（含夜间因子 1.1，与售价层规则不同 → 两层留痕各自独立）。
	costShape := `{"text":{"input":{"price":0.5,"cache":{"read":0.05}},"output":{"price":1}},"multipliers":[{"when":{"all":[{"utc_hours":{"gte":"03:00"}},{"utc_hours":{"lt":"05:00"}}]},"factor":{"text":1.1}}]}`
	f.expectCostHitV2("prov", served, map[string]float64{"input": 0.5}, costShape)
	// 售价：四价（含 cache_write 0.2）+ 夜间因子 1.2。
	saleShape := `{"text":{"input":{"price":1,"cache":{"read":0.1,"write":0.2}},"output":{"price":2}},"multipliers":[{"when":{"all":[{"utc_hours":{"gte":"03:00"}},{"utc_hours":{"lt":"05:00"}}]},"factor":{"text":1.2}}]}`
	f.expectPriceChainV2(served, served, nil, 1, "group/"+served, "ext-v2",
		map[string]float64{"input": 9, "output": 9, "cache_read": 9}, saleShape)

	// 留痕期望：与实现同一构造路径（同形状、同求值时刻、同用量、同 R）→ 逐字节可比。
	// 预计算侧也要先走求值（与 evalRec 的下游形态一致），否则 factor=0 对不上。
	wantUsage := billing.Usage{Input: 850, Output: 200, CacheRead: 100, CacheWrite: 50}
	saleWant := mustV2(t, saleShape, v2EvalAt)
	saleWant.Eval = billing.EvaluateV2(saleWant, wantUsage)
	saleWant.SetEvaluatedForTest()
	costWant := mustV2(t, costShape, v2EvalAt)
	costWant.Eval = billing.EvaluateV2(costWant, wantUsage)
	costWant.SetEvaluatedForTest()
	wantBytes, err := json.Marshal(billing.PriceEvalTrace(saleWant, costWant, wantUsage, 10000))
	if err != nil {
		t.Fatalf("trace 序列化：%v", err)
	}
	f.expectSettledV2(lg, 0, string(wantBytes))

	if _, err := f.p.Once(context.Background()); err != nil {
		t.Fatalf("Once：%v", err)
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("DB 期望未对齐：%v", err)
	}
	reqs := inner.reqs
	if len(reqs) != 1 {
		t.Fatalf("应落 1 笔账单，got %d", len(reqs))
	}
	req := reqs[0]
	// 四桶互斥：1000-100-50=850，cache_write 独立分桶。
	if req.Tokens != wantUsage {
		t.Fatalf("v2 行四桶拆分错误：got %+v want %+v", req.Tokens, wantUsage)
	}
	if req.V2 == nil {
		t.Fatalf("v2 行应挂 V2 求值输入")
	}
	// 时段/分档不叠加（v2 行由规则键吸收时间/上下文条件）。
	if req.TimeConfig != nil || req.ContextTiers != nil {
		t.Fatalf("v2 行不得叠加时段/分档：tc=%v tiers=%v", req.TimeConfig, req.ContextTiers)
	}
	// rates 列 = LegacyView 三段视图（old sale_rates 的 9/9/9 必须被忽略）。
	if got := req.Rates; got["input"] != 1 || got["output"] != 2 || got["cache_read"] != 0.1 {
		t.Fatalf("LegacyView 错误（sale_v2 必须压过旧三段）：%v", got)
	}
	if req.CostV2 == nil {
		t.Fatalf("cost_v2 命中应挂 CostV2（成本层留痕）")
	}
	// 计价复算（四价 × 因子 1.2 ÷ R）：
	// (850×1 + 200×2 + 100×0.1 + 50×0.2)×1.2/10000 = 1270×1.2/10000 = 0.1524。
	got, err := billing.ComputeCreditsV2(req.Tokens, req.V2.Prices, req.V2.Eval.FactorFor("text"), 10000)
	if err != nil {
		t.Fatalf("复算失败：%v", err)
	}
	if got != 0.1524 {
		t.Fatalf("v2 计价不符：got %v want 0.1524", got)
	}
	if strings.Contains(f.logs.String(), "单条结算失败") {
		t.Fatalf("不应出现结算失败：%s", f.logs.String())
	}
}

// TestSettleV2_BadShapeFallsBack 售价形状不合法 → 回落旧三段 + 告警可见（不静默）。
func TestSettleV2_BadShapeFallsBack(t *testing.T) {
	f := newFixture(t)
	lg := "lg-v2bad"
	served := "m-bad"
	caller := "ck-bad"
	f.expectOnceRows([]any{lg, v2EvalAt, "prov", served, served, caller,
		int64(1), int64(1000), int64(200), int64(100), int64(50), float64(0)})
	f.expectGroups()
	f.expectToken(caller)
	f.expectCostMiss("prov", served)
	// 缺 output.price（必填三件套之一）→ 解析失败。
	f.expectPriceChainV2(served, served, nil, 1, "group/"+served, "ext-bad",
		map[string]float64{"input": 1, "output": 2, "cache_read": 0.1},
		`{"text":{"input":{"price":1,"cache":{"read":0.1}}}}`)
	f.expectSettled(lg, 0)

	if _, err := f.p.Once(context.Background()); err != nil {
		t.Fatalf("Once：%v", err)
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("DB 期望未对齐：%v", err)
	}
	reqs := f.rec.reqs
	if len(reqs) != 1 {
		t.Fatalf("回落应照常落账单，got %d", len(reqs))
	}
	if reqs[0].V2 != nil {
		t.Fatalf("形状不合法不得走 v2 求值")
	}
	// 三段口径拆分（cache_write 折回 input）：1000-100=900。
	if reqs[0].Tokens.Input != 900 || reqs[0].Tokens.CacheWrite != 0 {
		t.Fatalf("回落应保持三段拆分：got %+v", reqs[0].Tokens)
	}
	if reqs[0].Rates["input"] != 1 || reqs[0].Rates["output"] != 2 || reqs[0].Rates["cache_read"] != 0.1 {
		t.Fatalf("回落应使用旧三段价：%v", reqs[0].Rates)
	}
	if !strings.Contains(f.logs.String(), "v2 形状不合法") {
		t.Fatalf("形状不合法应打告警（静默回落 = 价格漂移不可见），logs=%s", f.logs.String())
	}
}

// TestSettleV2_SplitAnomaly 四桶拆分数据异常（tokens_in < cache_read+cache_write）
// → 回退三段口径（cache_write 不拆、input 沿用旧防御链），售价仍按 v2 四价。
func TestSettleV2_SplitAnomaly(t *testing.T) {
	f := newFixture(t)
	lg := "lg-v2anom"
	served := "m-anom"
	caller := "ck-anom"
	// tokens_in=120 < cache_read=100 + cache_write=50 → 拆分不成立。
	f.expectOnceRows([]any{lg, v2EvalAt, "prov", served, served, caller,
		int64(1), int64(120), int64(200), int64(100), int64(50), float64(0)})
	f.expectGroups()
	f.expectToken(caller)
	f.expectCostMiss("prov", served)
	f.expectPriceChainV2(served, served, nil, 1, "group/"+served, "ext-anom",
		map[string]float64{"input": 1, "output": 2, "cache_read": 0.1}, v2SaleShape)
	f.expectSettled(lg, 0)

	if _, err := f.p.Once(context.Background()); err != nil {
		t.Fatalf("Once：%v", err)
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("DB 期望未对齐：%v", err)
	}
	reqs := f.rec.reqs
	if len(reqs) != 1 {
		t.Fatalf("异常行应照常结算（不阻塞批次），got %d", len(reqs))
	}
	// 120-100=20 ≥ 0 → 防御链第二档：cache_write 折回 input。
	if reqs[0].Tokens.Input != 20 || reqs[0].Tokens.CacheWrite != 0 {
		t.Fatalf("异常行应回退三段拆分：got %+v", reqs[0].Tokens)
	}
	if reqs[0].V2 == nil {
		t.Fatalf("形状合法的 v2 售价照常走 v2 求价（只是分桶回退）")
	}
}

// TestSettleV2_IdempotentKeepsTrace 幂等命中（Record 未重新求值）→ 留痕参数必须 nil，
// 不覆盖首次结算写入的 price_eval（COALESCE 不动）。
func TestSettleV2_IdempotentKeepsTrace(t *testing.T) {
	f := newFixture(t) // 不用 evalRec：fakeRec 原样返回 = 幂等命中形态（Evaluated=false）
	lg := "lg-v2idem"
	served := "m-idem"
	caller := "ck-idem"
	f.expectOnceRows([]any{lg, v2EvalAt, "prov", served, served, caller,
		int64(1), int64(1000), int64(200), int64(100), int64(50), float64(0)})
	f.expectGroups()
	f.expectToken(caller)
	f.expectCostMiss("prov", served)
	f.expectPriceChainV2(served, served, nil, 1, "group/"+served, "ext-idem",
		map[string]float64{"input": 1, "output": 2, "cache_read": 0.1}, v2SaleShape)
	// 留痕参数必须 nil。
	f.mock.ExpectExec("UPDATE billing_records SET magpie_request_id").
		WithArgs(lg, float64(0), "v3-"+lg, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := f.p.Once(context.Background()); err != nil {
		t.Fatalf("Once：%v", err)
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("DB 期望未对齐：%v", err)
	}
	if reqs := f.rec.reqs; len(reqs) != 1 || reqs[0].V2 == nil {
		t.Fatalf("v2 组包应照常发生（幂等是 Record 内部判定）：%+v", reqs)
	}
}

// TestRebillV2_AttachesSale 补收链 v2：售价解析复用 attachSale（求值时刻=补收时刻），
// tokens 沿用原账单行 JSONB——历史三段行无 cache_write 键 → 该段按 0 计（不可复原）。
func TestRebillV2_AttachesSale(t *testing.T) {
	f := newFixture(t)
	inner := f.rec
	f.p.bill = &evalRec{inner: inner} // 真链路 requestCreditsV2 会求值；夹具补下游形态
	groupID := rseg(t)
	groupRef := "group/" + groupID
	display := "组" + rseg(t)
	extName := "ext-" + rseg(t)
	// 旧三段价故意与 v2 形状不同：证明 sale_v2 命中时压过旧三段。
	rates := map[string]float64{"input": 1, "output": 2, "cache_read": 0.1}
	saleShape := `{"text":{"input":{"price":3,"cache":{"read":0.5}},"output":{"price":4}},"multipliers":[]}`
	groups := []groupNode{{Ref: groupRef, DisplayName: display}}
	newID := rebillNewBillingID("bill-v2r")
	rebillCost := 0.125

	f.expectGroups([2]string{groupRef, display})
	// 预检：组 ref 直中绑定档（sale_v2 行）。
	f.expectPriceChainV2(groupRef, display, groups, 1, groupRef, extName, rates, saleShape)
	f.expectScan(rebillRow{
		BillingID: "bill-v2r", UserID: 42, TokenID: int64Ptr(7),
		ExternalModel: groupRef, InternalModelID: "prov/m",
		Tokens:      billing.Usage{Input: 900, Output: 200, CacheRead: 100}, // 历史三段行：无 cache_write
		CostCredits: &rebillCost, ErrorMessage: "未定价组：" + groupRef,
		ReqModel: groupRef, ServedModel: "served-v2", RouteID: 5,
	})
	f.expectState()
	// 行级解析：同候选链。
	f.expectPriceChainV2(groupRef, "served-v2", groups, 5, groupRef, extName, rates, saleShape)
	// Record → 新账单；成本沿用原行。
	f.rec.bals = []float64{88.5}
	f.mock.ExpectExec("UPDATE billing_records SET cost_credits").
		WithArgs(rebillCost, newID).WillReturnResult(sqlmock.NewResult(0, 1))
	f.expectMark("bill-v2r", newID)

	res, err := f.p.Rebill(context.Background(), groupRef)
	if err != nil {
		t.Fatalf("Rebill 失败：%v", err)
	}
	if res.Rebilled != 1 || len(res.NewBillingIDs) != 1 {
		t.Fatalf("汇总不符：%+v", res)
	}
	req := inner.reqs[0]
	if req.V2 == nil {
		t.Fatalf("补收 v2 行应挂 V2 求价输入")
	}
	// sale_v2 压过旧三段（3/4/0.5，LegacyView 落 rates）。
	if req.Rates["input"] != 3 || req.Rates["output"] != 4 || req.Rates["cache_read"] != 0.5 {
		t.Fatalf("补收售价应来自 sale_v2：%v", req.Rates)
	}
	if req.V2.EvalAt.IsZero() {
		t.Fatalf("补收 v2 求值时刻应为补收时刻（非零）")
	}
	if req.Tokens.CacheWrite != 0 {
		t.Fatalf("历史三段行的 cache_write 不可复原，应为 0：got %+v", req.Tokens)
	}
	// 计价复算：(900×3+200×4+100×0.5)/10000 = 3550/10000 = 0.355。
	got, cerr := billing.ComputeCreditsV2(req.Tokens, req.V2.Prices, req.V2.Eval.FactorFor("text"), 10000)
	if cerr != nil || got != 0.355 {
		t.Fatalf("补收 v2 计价不符：got %v want 0.355", got)
	}
}

// mustV2 由形状构造求值输入（测试脚手架）。
func mustV2(t *testing.T, shape string, at time.Time) *billing.V2Pricing {
	t.Helper()
	c, err := billing.ParseV2Cost([]byte(shape))
	if err != nil {
		t.Fatalf("形状不合法：%v", err)
	}
	return billing.PricingFromV2Cost(c, at)
}
