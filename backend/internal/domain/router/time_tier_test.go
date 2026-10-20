package router

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
)

// 本文件为 Step 16「闲忙时 / 分档联动收口」的端到端测试：
//   - 同一会话在闲时/忙时（不同 now）下，跨时段系数（CoeffFor(now)）随时刻变化并联动择渠道；
//   - 不同上下文规模（跨分档）下，跨分档系数（ContextTierCoeff）随规模变化并联动择渠道；
//   - 断言路由侧 resolveTimeCoeff/resolveCtxCoeff 与 billing.ComputeCredits 使用的
//     timeCoeff/ctxCoeff 对同一输入取值一致（同源），且路由成本标量与 billing.RawTotal 同源。

// timeTierRates 两渠道共用同一组三档成本价：使「谁更便宜」只由时段/分档系数决定。
func timeTierRates() billing.Rates {
	return billing.Rates{"input": 0.3, "output": 1.5, "cache_read": 0.05}
}

// newTimeTierEngine 构造同一对外模型下双候选（渠道 1/2，优先级 p1/p2）的引擎：
// 注入 Enabled 的会话成本提供者（起点择优生效）；queries 为本用例将发生的对外模型查询次数。
func newTimeTierEngine(t *testing.T, m1, m2 channel.ChannelModel, p1, p2, queries int) *Engine {
	t.Helper()
	svc, mock := newModelService(t, queries)
	t.Cleanup(func() { _ = mock.ExpectationsWereMet() })

	mgr := newManagerWithKeys(t,
		[]*channel.Channel{healthyCh(1, "a", nil, p1), healthyCh(2, "b", nil, p2)},
		[]channel.ChannelModel{m1, m2},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{m1, m2}}, mgr, nil)
	e.SetSessionCostProvider(sessionCostProvider(t, true))
	return e
}

// TestTimeTier_IdleVsPeakFlipsChannelSelection 锁定「跨时段联动择渠道」：
// ch1 在高峰时段（18:00-22:00）模型级系数 ×2、闲时 ×1；ch2 无模型级时段（系数恒 1）。
// 两渠道三档成本价相同，故闲时平价、高峰时 ch1 变贵 → 择渠道随 now 翻转（ch1 → ch2）。
func TestTimeTier_IdleVsPeakFlipsChannelSelection(t *testing.T) {
	rates := timeTierRates()
	peakTC := &billing.TimeCoeffConfig{Timezone: "UTC", Default: 1.0, Periodic: []billing.Segment{
		{Name: "peak", Start: "18:00", End: "22:00", Coeff: 2.0},
	}}
	m1 := cm(1, "int1")
	m1.CostRates = rates
	m1.TimeConfig = peakTC // 仅 ch1 有模型级时段配置
	m2 := cm(2, "int2")
	m2.CostRates = rates // 无时段配置 → 系数 1

	// 两渠道**同优先级**：平价时由稳定 id 兜底选 ch1；本用例只验证时段系数能否翻转择渠道
	//（priority 为硬分层，若 ch1 优先级更高则永远选 ch1，无法体现系数差异）。
	e := newTimeTierEngine(t, m1, m2, 100, 100, 2)
	tok := billing.Usage{Input: 1000, CacheRead: 2000, Output: 500}
	ctx := context.Background()

	idle := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC) // 闲时/普通：两渠道均系数 1
	rIdle, err := e.Route(ctx, RouteSpec{
		ExternalModel: "qw-max", SessionKey: "s-idle", SessionTokens: &tok, Now: idle,
	})
	if err != nil {
		t.Fatalf("route（闲时）: %v", err)
	}
	if rIdle.ChannelID != 1 {
		t.Fatalf("闲时两渠道平价应由稳定 id 兜底选 ch1，got channel %d", rIdle.ChannelID)
	}

	peak := time.Date(2026, 1, 2, 20, 0, 0, 0, time.UTC) // 高峰：ch1 系数 ×2 → ch1 更贵
	rPeak, err := e.Route(ctx, RouteSpec{
		ExternalModel: "qw-max", SessionKey: "s-peak", SessionTokens: &tok, Now: peak,
	})
	if err != nil {
		t.Fatalf("route（高峰）: %v", err)
	}
	if rPeak.ChannelID != 2 {
		t.Fatalf("高峰时段 ch1 系数 ×2 更贵，应择 ch2，got channel %d", rPeak.ChannelID)
	}

	// 时段系数确随 now 变化（闲 1 / 高峰 2），路由 TimeCoeff 与之一致。
	if got := resolveTimeCoeff(peakTC, nil, idle); got != 1 {
		t.Fatalf("闲时 ch1 时段系数应为 1，got %v", got)
	}
	if rPeak.TimeCoeff != 1 {
		t.Fatalf("高峰选中的 ch2 无模型级时段应回落系数 1，got %v", rPeak.TimeCoeff)
	}
}

// TestTimeTier_ContextTierFlipsChannelSelection 锁定「跨分档联动择渠道」：
// ch1 模型级分档在高上下文规模（Input+CacheRead ≥ 8000）系数 ×2；ch2 无分档（恒 1）。
// 两渠道三档成本价相同，故小上下文平价、大上下文 ch1 变贵 → 择渠道随上下文规模翻转（ch1 → ch2）。
func TestTimeTier_ContextTierFlipsChannelSelection(t *testing.T) {
	rates := timeTierRates()
	tiers := []billing.TierRule{{Min: 8000, Coeff: 2.0}, {Min: 0, Coeff: 1.0}}
	m1 := cm(1, "int1")
	m1.CostRates = rates
	m1.ContextTiers = tiers // 仅 ch1 有模型级分档
	m2 := cm(2, "int2")
	m2.CostRates = rates // 无分档 → 系数 1

	// 两渠道同优先级：小上下文平价时由稳定 id 兜底选 ch1，大上下文由分档系数翻转择 ch2。
	e := newTimeTierEngine(t, m1, m2, 100, 100, 2)
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	small := billing.Usage{Input: 1000, CacheRead: 500, Output: 200} // Input+CacheRead=1500 < 8000 → 系数 1
	rSmall, err := e.Route(ctx, RouteSpec{
		ExternalModel: "qw-max", SessionKey: "s-small", SessionTokens: &small, Now: now,
	})
	if err != nil {
		t.Fatalf("route（小上下文）: %v", err)
	}
	if rSmall.ChannelID != 1 {
		t.Fatalf("小上下文两渠道平价应由稳定 id 兜底选 ch1，got channel %d", rSmall.ChannelID)
	}

	big := billing.Usage{Input: 6000, CacheRead: 4000, Output: 200} // 10000 ≥ 8000 → ch1 系数 2 → 更贵
	rBig, err := e.Route(ctx, RouteSpec{
		ExternalModel: "qw-max", SessionKey: "s-big", SessionTokens: &big, Now: now,
	})
	if err != nil {
		t.Fatalf("route（大上下文）: %v", err)
	}
	if rBig.ChannelID != 2 {
		t.Fatalf("大上下文 ch1 分档系数 ×2 更贵，应择 ch2，got channel %d", rBig.ChannelID)
	}

	// 分档系数确随上下文规模变化（小 1 / 大 2），口径含缓存读。
	if got := resolveCtxCoeff(tiers, nil, small); got != 1 {
		t.Fatalf("小上下文分档系数应为 1，got %v", got)
	}
	if got := resolveCtxCoeff(tiers, nil, big); got != 2 {
		t.Fatalf("大上下文分档系数应为 2（口径含缓存读），got %v", got)
	}
}

// TestTimeTier_RouteAndBillingShareCoeffs 锁定「同源」：路由决策/成本比较用的 timeCoeff/ctxCoeff
// 与 billing（CoeffFor/ContextTierCoeff/ComputeCredits）对同一输入取到完全相同的系数，
// 且路由成本标量与 billing.RawTotal 在相同系数下同源。
func TestTimeTier_RouteAndBillingShareCoeffs(t *testing.T) {
	rates := timeTierRates()
	modelTC := &billing.TimeCoeffConfig{Timezone: "UTC", Default: 1.0, Periodic: []billing.Segment{
		{Name: "peak", Start: "18:00", End: "22:00", Coeff: 2.0},
	}}
	tiers := []billing.TierRule{{Min: 8000, Coeff: 2.0}, {Min: 0, Coeff: 1.0}}
	m1 := cm(1, "int1")
	m1.CostRates = rates
	m1.TimeConfig = modelTC
	m1.ContextTiers = tiers

	svc, mock := newModelService(t, 1)
	t.Cleanup(func() { _ = mock.ExpectationsWereMet() })
	mgr := newManagerWithKeys(t,
		[]*channel.Channel{healthyCh(1, "a", nil, 100)},
		[]channel.ChannelModel{m1},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{m1}}, mgr, nil)
	e.SetSessionCostProvider(sessionCostProvider(t, true))
	now := time.Date(2026, 1, 2, 20, 0, 0, 0, time.UTC) // 高峰
	tok := billing.Usage{Input: 6000, CacheRead: 4000, Output: 200}
	ctx := context.Background()

	r, err := e.Route(ctx, RouteSpec{
		ExternalModel: "qw-max", SessionKey: "s-share", SessionTokens: &tok, Now: now,
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}

	// billing 侧口径（计费真实入口）：CoeffFor + ContextTierCoeff（含缓存读）。
	wantTC, err := modelTC.CoeffFor(now)
	if err != nil {
		t.Fatalf("billing CoeffFor: %v", err)
	}
	wantCtx, err := billing.ContextTierCoeff(tok.Input+tok.CacheRead, tiers)
	if err != nil {
		t.Fatalf("billing ContextTierCoeff: %v", err)
	}
	if wantTC != 2 || wantCtx != 2 {
		t.Fatalf("用例构造失败：期望高峰系数 2 / 分档系数 2，got %v/%v", wantTC, wantCtx)
	}

	// 路由侧：同源解析（模型级优先，回落全局）必须与 billing 取值一致。
	if got := resolveTimeCoeff(modelTC, nil, now); got != wantTC {
		t.Fatalf("路由 resolveTimeCoeff=%v 与 billing CoeffFor=%v 不同源", got, wantTC)
	}
	if got := resolveCtxCoeff(tiers, nil, tok); got != wantCtx {
		t.Fatalf("路由 resolveCtxCoeff=%v 与 billing ContextTierCoeff=%v 不同源", got, wantCtx)
	}
	// resolveCostCoeffs 作为决策与成本比较的唯一致数来源，取值亦与 billing 一致。
	tc, ctxCoeff := resolveCostCoeffs(modelTC, nil, tiers, nil, now, tok)
	if tc != wantTC || ctxCoeff != wantCtx {
		t.Fatalf("resolveCostCoeffs=%v/%v 与 billing=%v/%v 不同源", tc, ctxCoeff, wantTC, wantCtx)
	}

	// 成本比较同源：路由成本标量 = billing.RawTotal(同一入参、同一系数)。
	wantRaw, err := billing.RawTotal(tok, rates, wantTC, wantCtx)
	if err != nil {
		t.Fatalf("billing RawTotal: %v", err)
	}
	if r.Cost != wantRaw {
		t.Fatalf("路由成本标量=%v 与 billing.RawTotal=%v 不同源", r.Cost, wantRaw)
	}

	// 计费口径同源：ComputeCredits(同一入参、同一系数) = RawTotal ÷ R（R=10000）。
	credits, err := billing.ComputeCredits(tok, rates, wantTC, wantCtx, 10000)
	if err != nil {
		t.Fatalf("billing ComputeCredits: %v", err)
	}
	if math.Abs(credits-wantRaw/10000) > 1e-9 {
		t.Fatalf("ComputeCredits=%v 与 RawTotal/R=%v 不一致（口径不同源）", credits, wantRaw/10000)
	}
}
