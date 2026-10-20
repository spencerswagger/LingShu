package router

// 本文件为「成本比较纳入三档成本价 × 时段 × 分档」的失败测试（RED）。
//
// 依据控制器已定下的 API 契约编写（这些字段/签名当前尚不存在，故本文件当前
// 编译失败即为预期 RED）：
//   - CostConfig 接口新增 ContextTiers() ([]billing.TierRule, error)；
//   - RouteSpec 新增 Sample *billing.Usage（nil → 回落全局 routing.cost_sample）；
//   - candidate 新增模型级字段 contextTiers []billing.TierRule；
//   - costScalar 改为 costScalar(sample, rates, timeCoeff, ctxCoeff float64) (float64, bool)；
//   - 分档系数口径 billing.ContextTierCoeff(sample.Input+sample.CacheRead, tiers)，
//     模型级 tiers 优先、为空回落全局 tiers（CostConfig.ContextTiers）。
//
// 本步只写测试，不实现功能、不改动 engine.go 等实现文件。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
)

// costCfgFull 是满足「扩展后 CostConfig 接口」的测试配置：
// 相比既有 fakeCostConfig 额外提供 ContextTiers（新契约方法）。
type costCfgFull struct {
	sample   billing.Usage
	globalTC *billing.TimeCoeffConfig
	tiers    []billing.TierRule
}

func (f *costCfgFull) SampleUsage() (billing.Usage, error) {
	if f.sample == (billing.Usage{}) {
		return defaultCostSample, nil
	}
	return f.sample, nil
}

func (f *costCfgFull) TimeConfig() (billing.TimeCoeffConfig, error) {
	if f.globalTC == nil {
		return billing.TimeCoeffConfig{}, errors.New("未配置全局时段")
	}
	return *f.globalTC, nil
}

// ContextTiers 为控制器新契约方法（CostConfig 接口将新增该方法）。
func (f *costCfgFull) ContextTiers() ([]billing.TierRule, error) {
	return f.tiers, nil
}

// SampleProfile 满足 CostConfig 接口：返回缺省补齐比例（缓存读 0.9 / 输出 0.1）。
// 本文件用例均显式传入三段样值或只关心默认排序，故补齐逻辑不参与断言。
func (f *costCfgFull) SampleProfile() (billing.SampleProfile, error) {
	return billing.SampleProfile{CacheReadRatio: 0.9, OutputRatio: 0.1}, nil
}

// costOrderRates 构造含三段键的合法单价表。
func costOrderRates(input, output, cacheRead float64) billing.Rates {
	return billing.Rates{"input": input, "output": output, "cache_read": cacheRead}
}

// 场景 1：costScalar 必须把分档系数纳入标量——同一 rates、同一 timeCoeff 下，
// ctxCoeff=2 的标量应为 ctxCoeff=1 的 2 倍；ctxCoeff 不进即视为 bug。
func TestCostOrder_CostScalarIncludesContextCoeff(t *testing.T) {
	sample := billing.Usage{Input: 2000, Output: 1000, CacheRead: 500}
	rates := costOrderRates(2, 3, 1)

	base, ok := costScalar(sample, rates, 1, 1)
	if !ok {
		t.Fatalf("ctxCoeff=1 时应为有效成本，got known=%v", ok)
	}
	doubled, ok := costScalar(sample, rates, 1, 2)
	if !ok {
		t.Fatalf("ctxCoeff=2 时应为有效成本，got known=%v", ok)
	}
	if doubled != base*2 {
		t.Fatalf("分档系数未纳入成本标量：base=%v doubled=%v（期望 doubled=2×base）", base, doubled)
	}
}

// 接口契约：CostConfig 必须暴露 ContextTiers()，供回落全局分档。
func TestCostOrder_CostConfigExposesContextTiers(t *testing.T) {
	var cc CostConfig = &costCfgFull{tiers: []billing.TierRule{{Min: 0, Coeff: 1}}}
	tiers, err := cc.ContextTiers()
	if err != nil {
		t.Fatalf("ContextTiers 读取失败: %v", err)
	}
	if len(tiers) != 1 {
		t.Fatalf("期望带回 1 条分档规则，got %d", len(tiers))
	}
}

// 场景 2：最低输入价 ≠ 最低总成本——候选 A 的 input 单价更低，但 output/cache_read
// 更贵使三档总和更高；候选 B 整体三档总和更低。排序应把 B 排在 A 前。
func TestCostOrder_LowestInputPriceNotLowestTotalCost(t *testing.T) {
	sample := billing.Usage{Input: 1000, Output: 1000}
	ratesA := costOrderRates(1, 10, 5) // input 更便宜，但总成本更高
	ratesB := costOrderRates(5, 1, 2)  // input 更贵，但总成本更低

	if ratesA["input"] >= ratesB["input"] {
		t.Fatalf("前置条件不成立：A 的 input 单价应低于 B")
	}

	scalarA, okA := costScalar(sample, ratesA, 1, 1)
	scalarB, okB := costScalar(sample, ratesB, 1, 1)
	if !okA || !okB {
		t.Fatalf("两候选都应有有效成本，got A=%v B=%v", okA, okB)
	}
	if scalarA <= scalarB {
		t.Fatalf("口径应为 A 总成本更高，got A=%v B=%v", scalarA, scalarB)
	}

	sess := NewSessionRegistry(nil)
	e := &Engine{sess: sess, now: time.Now, costCfg: &costCfgFull{}}
	cands := []candidate{
		{id: 1, keyID: 11, modelRow: 101, priority: 100, costRates: ratesA},
		{id: 2, keyID: 21, modelRow: 201, priority: 100, costRates: ratesB},
	}
	got := e.orderCandidates(cands, time.Now(), sample, nil, nil)
	if got[0].id != 2 {
		t.Fatalf("排序应把总成本更低的候选 B(id=2) 排前，got %v", candidateIDs(got))
	}
}

// 场景 3：分档影响排序——两候选 rates 相同，候选 A 的模型级 ContextTiers 在给定
// 上下文规模下 coeff=2、候选 B 无 tiers（=1）；断言 B 排前。
func TestCostOrder_ContextTierFlipsOrdering(t *testing.T) {
	sample := billing.Usage{Input: 4000}
	rates := costOrderRates(1, 1, 1)

	sess := NewSessionRegistry(nil)
	e := &Engine{sess: sess, now: time.Now, costCfg: &costCfgFull{}}
	cands := []candidate{
		{id: 1, keyID: 11, modelRow: 101, priority: 100, costRates: rates,
			contextTiers: []billing.TierRule{{Min: 0, Coeff: 2}}}, // 命中分档 ×2
		{id: 2, keyID: 21, modelRow: 201, priority: 100, costRates: rates}, // 无分档 ×1
	}
	got := e.orderCandidates(cands, time.Now(), sample, nil, nil)
	if got[0].id != 2 {
		t.Fatalf("分档使候选 A 更贵，应把 B(id=2) 排前，got %v", candidateIDs(got))
	}
}

// 场景 4：时段影响排序——两候选 rates 相同，候选 A 模型级 TimeConfig 在当前时刻
// coeff=2、候选 B 为 1；断言 B 排前。
func TestCostOrder_TimeCoeffFlipsOrdering(t *testing.T) {
	sample := billing.Usage{Input: 4000}
	rates := costOrderRates(1, 1, 1)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	sess := NewSessionRegistry(nil)
	e := &Engine{sess: sess, now: func() time.Time { return now }, costCfg: &costCfgFull{}}
	tc := &billing.TimeCoeffConfig{Timezone: "UTC", Default: 2}
	cands := []candidate{
		{id: 1, keyID: 11, modelRow: 101, priority: 100, costRates: rates, timeCfg: tc}, // 当前时段 ×2
		{id: 2, keyID: 21, modelRow: 201, priority: 100, costRates: rates},              // 时段 ×1
	}
	got := e.orderCandidates(cands, now, sample, nil, nil)
	if got[0].id != 2 {
		t.Fatalf("时段系数使候选 A 更贵，应把 B(id=2) 排前，got %v", candidateIDs(got))
	}
}

// 补例：fillCost 应回填「含分档系数」的成本标量。
func TestCostOrder_FillCostAppliesContextTier(t *testing.T) {
	e := &Engine{sess: NewSessionRegistry(nil), now: time.Now, costCfg: &costCfgFull{}}
	sample := billing.Usage{Input: 1000}
	rates := costOrderRates(1, 1, 1)

	c := &candidate{id: 1, keyID: 11, modelRow: 101, costRates: rates,
		contextTiers: []billing.TierRule{{Min: 0, Coeff: 2}}}
	e.fillCost(c, sample, nil, nil, time.Now())
	if !c.costKnown {
		t.Fatalf("应为有效成本")
	}
	if c.cost != 2000 {
		t.Fatalf("fillCost 应回填含分档系数的成本 2000，got %v", c.cost)
	}
}

// 场景 5：样值来源——RouteSpec 带 Sample（本轮三段 token）时，成本应按该样值计算
// 而非全局 routing.cost_sample；用样值差异构造成本相对大小反转来断言。
func TestCostOrder_RouteSpecSampleBeatsGlobalSample(t *testing.T) {
	svc, mock := newModelService(t, 2)
	defer func() { _ = mock.ExpectationsWereMet() }()

	// 渠道 1 只按 output 计价；渠道 2 只按 input 计价。
	m1 := cm(1, "int1")
	m1.CostRates = billing.Rates{"input": 0, "output": 1, "cache_read": 0}
	m2 := cm(2, "int2")
	m2.CostRates = billing.Rates{"input": 1, "output": 0, "cache_read": 0}
	mgr := newManagerWithKeys(t,
		[]*channel.Channel{healthyCh(1, "a", nil, 100), healthyCh(2, "b", nil, 100)},
		[]channel.ChannelModel{m1, m2},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{m1, m2}}, mgr, nil)

	// 全局样值：输入极大、输出极小 → 全局口径下渠道 1（output 计价）最便宜。
	e.SetCostConfig(&costCfgFull{sample: billing.Usage{Input: 100000, Output: 1}})

	rGlobal, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("route（全局样值）: %v", err)
	}
	if rGlobal.ChannelID != 1 {
		t.Fatalf("回落全局样值时渠道 1（output 计价）应最便宜，got %d", rGlobal.ChannelID)
	}

	// RouteSpec.Sample 提供本轮三段 token（输入极小、输出极大）→ 应按本轮样值翻转选渠道 2。
	local := billing.Usage{Input: 1, Output: 100000}
	rLocal, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max", Sample: &local})
	if err != nil {
		t.Fatalf("route（本轮样值）: %v", err)
	}
	if rLocal.ChannelID != 2 {
		t.Fatalf("RouteSpec.Sample 应覆盖全局样值并翻转选中渠道 2，got %d", rLocal.ChannelID)
	}
}

// 场景 6：分档口径含缓存读——Input 小、Input+CacheRead 跨档时，应取到高档系数；
// 仅看 Input 则落低档。经 resolveCtxCoeff 与 costScalar 双重断言。
func TestCostOrder_CacheReadJoinsTier(t *testing.T) {
	tiers := []billing.TierRule{{Min: 1000, Coeff: 2}, {Min: 0, Coeff: 1}}

	// Input 未单独跨档（<1000），Input+CacheRead=1200 应落高档。
	sample := billing.Usage{Input: 400, CacheRead: 800}
	if sample.Input >= 1000 {
		t.Fatalf("前置不成立：Input=%d 应小于高档下界 1000", sample.Input)
	}
	if got := resolveCtxCoeff(nil, tiers, sample); got != 2 {
		t.Fatalf("分档口径应含 CacheRead（Input+CacheRead=%d 落高档系数 2），got %v", sample.Input+sample.CacheRead, got)
	}
	// 反证：仅 Input 时不跨档 → 系数 1，说明跨档来自 CacheRead。
	if got := resolveCtxCoeff(nil, tiers, billing.Usage{Input: sample.Input}); got != 1 {
		t.Fatalf("仅 Input=%d 应落低档系数 1，got %v", sample.Input, got)
	}

	// 经 costScalar：高档系数使标量为无分档的 2 倍。
	rates := costOrderRates(1, 1, 1)
	base, ok := costScalar(sample, rates, 1, 1)
	if !ok {
		t.Fatalf("无分档时应为有效成本")
	}
	withTier, ok := costScalar(sample, rates, 1, 2)
	if !ok {
		t.Fatalf("含分档时应为有效成本")
	}
	if withTier != base*2 {
		t.Fatalf("高档分档系数应使标量翻倍：base=%v withTier=%v", base, withTier)
	}
}

// 场景 7：全局分档回落——候选模型级 contextTiers 为空时，CostConfig.ContextTiers()
// 返回的全局分档应经 Route/orderCandidates 生效：相对无分档时成本改变。
func TestCostOrder_GlobalTiersFallback(t *testing.T) {
	svc, mock := newModelService(t, 2)
	defer func() { _ = mock.ExpectationsWereMet() }()

	m1 := cm(1, "int1") // 模型级 contextTiers 为空
	mgr := newManagerWithKeys(t,
		[]*channel.Channel{healthyCh(1, "a", nil, 100)},
		[]channel.ChannelModel{m1},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{m1}}, mgr, nil)

	sample := billing.Usage{Input: 2000}
	// 无全局分档：系数 1 → 成本 = 2000 × input 单价 1 = 2000。
	e.SetCostConfig(&costCfgFull{sample: sample})
	rNo, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("route（无全局分档）: %v", err)
	}
	if rNo.Cost != 2000 {
		t.Fatalf("无全局分档应系数 1 → 成本 2000，got %v", rNo.Cost)
	}

	// 全局分档：Input=2000 落 Min=1000 档系数 2 → 成本 4000（模型级为空，回落全局）。
	e.SetCostConfig(&costCfgFull{sample: sample, tiers: []billing.TierRule{{Min: 1000, Coeff: 2}, {Min: 0, Coeff: 1}}})
	rTier, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("route（全局分档回落）: %v", err)
	}
	if rTier.Cost != 4000 {
		t.Fatalf("全局分档应回落生效 → 成本 4000，got %v", rTier.Cost)
	}
}

// costCfgTiersErr 在 costCfgFull 基础上让 ContextTiers 返回错误（模拟全局分档读取失败）。
type costCfgTiersErr struct{ costCfgFull }

func (f *costCfgTiersErr) ContextTiers() ([]billing.TierRule, error) {
	return nil, errors.New("全局分档读取失败")
}

// 场景 8：全局分档读取 error 回落——ContextTiers() 返回 error 时，分档系数按 1、
// Route 不失败，且成本/排序与「无分档」一致。
func TestCostOrder_ContextTiersErrorFallback(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	m1 := cm(1, "int1")
	mgr := newManagerWithKeys(t,
		[]*channel.Channel{healthyCh(1, "a", nil, 100)},
		[]channel.ChannelModel{m1},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{m1}}, mgr, nil)

	e.SetCostConfig(&costCfgTiersErr{costCfgFull{sample: billing.Usage{Input: 2000}}})
	r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("分档读取失败不应使 Route 失败: %v", err)
	}
	// 分档系数按 1 → 与「无分档」一致的成本 2000。
	if r.Cost != 2000 {
		t.Fatalf("分档读取失败应回落系数 1 → 成本 2000，got %v", r.Cost)
	}
}
