package router

import (
	"math"
	"strings"
	"testing"
)

func veniceChannel() ChannelCost {
	return ChannelCost{
		Name:      "venice",
		Input:     0.138,
		CacheRead: 0.028,
		Output:    0.275,
		TimeCoeff: 1,
		CtxCoeff:  1,
	}
}

func deepSeekChannel() ChannelCost {
	return ChannelCost{
		Name:      "deepseek",
		Input:     0.150,
		CacheRead: 0.003,
		Output:    0.600,
		TimeCoeff: 1,
		CtxCoeff:  1,
	}
}

func sessionCostApprox(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Fatalf("%s 期望 %v±%v, 实际 %v", name, want, tol, got)
	}
}

func TestSessionCost_StartPickZeroRecharge(t *testing.T) {
	cfg := SessionCostConfig{RemainingA: 12, RemainingB: 1, RemainingMin: 1, RemainingMax: 20}
	const cr, newIn, out = int64(20000), int64(1000), int64(1000)

	cand := deepSeekChannel()
	d := decideSessionCost(cfg, nil, cand, cr, newIn, out, 0)

	if !d.Switch {
		t.Fatalf("起点(无旧渠道)应切换到候选: %+v", d)
	}
	if d.RechargeCost != 0 {
		t.Fatalf("起点重灌代价应为 0: got=%v", d.RechargeCost)
	}
	if d.PaybackTurns != 0 {
		t.Fatalf("起点回本轮数应为 0: got=%v", d.PaybackTurns)
	}
	if !strings.Contains(d.Reason, "起点") {
		t.Fatalf("起点 Reason 应包含\"起点\": got=%q", d.Reason)
	}
	sessionCostApprox(t, "起点 TurnCostNew", d.TurnCostNew, perTurnCost(cand, cr, newIn, out), 1e-12)

	a := veniceChannel()
	b := deepSeekChannel()
	costA := perTurnCost(a, cr, newIn, out)
	costB := perTurnCost(b, cr, newIn, out)
	if costB >= costA {
		t.Fatalf("用例构造失败: 期望 deepseek 摊还每轮成本更小, venice=%v deepseek=%v", costA, costB)
	}
	best := b
	if costA < costB {
		best = a
	}
	dBest := decideSessionCost(cfg, nil, best, cr, newIn, out, 0)
	minCost := math.Min(costA, costB)
	sessionCostApprox(t, "起点择优 TurnCostNew", dBest.TurnCostNew, minCost, 1e-12)
}

func TestSessionCost_MidSwitchWhenRemainingExceedsPayback(t *testing.T) {
	cfg := SessionCostConfig{RemainingA: 20, RemainingB: 0, RemainingMin: 1, RemainingMax: 20}
	old := veniceChannel()
	cand := deepSeekChannel()
	const cr, newIn, out = int64(20000), int64(1000), int64(1000)

	pb := paybackTurns(cr, old, cand, newIn, out)
	rem := remainingTurns(0, cfg)
	if float64(rem) <= pb {
		t.Fatalf("用例构造失败: 期望剩余轮数 > 回本轮数, rem=%d pb=%v", rem, pb)
	}

	d := decideSessionCost(cfg, &old, cand, cr, newIn, out, 0)
	if !d.Switch {
		t.Fatalf("剩余轮数 %d > 回本 %v 应切换: %+v", rem, pb, d)
	}
	sessionCostApprox(t, "中途 PaybackTurns", d.PaybackTurns, pb, 1e-9)
	sessionCostApprox(t, "中途 RechargeCost", d.RechargeCost, rechargeCost(cr, cand), 1e-12)
	sessionCostApprox(t, "中途 TurnCostOld", d.TurnCostOld, perTurnCost(old, cr, newIn, out), 1e-12)
	sessionCostApprox(t, "中途 TurnCostNew", d.TurnCostNew, perTurnCost(cand, cr, newIn, out), 1e-12)
}

func TestSessionCost_MidHoldWhenRemainingNotExceed(t *testing.T) {
	cfg := SessionCostConfig{RemainingA: 10, RemainingB: 0, RemainingMin: 1, RemainingMax: 20}
	old := veniceChannel()
	cand := deepSeekChannel()
	const cr, newIn, out = int64(20000), int64(1000), int64(1000)

	pb := paybackTurns(cr, old, cand, newIn, out)
	rem := remainingTurns(0, cfg)
	if float64(rem) > pb {
		t.Fatalf("用例构造失败: 期望剩余轮数 <= 回本轮数, rem=%d pb=%v", rem, pb)
	}

	d := decideSessionCost(cfg, &old, cand, cr, newIn, out, 0)
	if d.Switch {
		t.Fatalf("剩余轮数 %d <= 回本 %v 应保持亲和(不切换): %+v", rem, pb, d)
	}
}

func TestSessionCost_NeverPaybackWhenDenNonPositive(t *testing.T) {
	old := veniceChannel()
	cand := ChannelCost{
		Name:      "not-cheaper-cache",
		Input:     0.150,
		CacheRead: 0.030,
		Output:    0.600,
		TimeCoeff: 1,
		CtxCoeff:  1,
	}
	const cr, newIn, out = int64(20000), int64(1000), int64(1000)

	if cand.CacheRead < old.CacheRead {
		t.Fatalf("用例构造失败: 新渠道缓存读不应更便宜, old=%v new=%v", old.CacheRead, cand.CacheRead)
	}
	pb := paybackTurns(cr, old, cand, newIn, out)
	if !math.IsNaN(pb) {
		t.Fatalf("分母 <= 0 应返回 NaN, got=%v", pb)
	}

	cfg := SessionCostConfig{RemainingA: 20, RemainingB: 0, RemainingMin: 1, RemainingMax: 20}
	d := decideSessionCost(cfg, &old, cand, cr, newIn, out, 0)
	if d.Switch {
		t.Fatalf("永不回本应保持亲和(不切换): %+v", d)
	}
}

func TestSessionCost_PaybackExampleVeniceToDeepSeek(t *testing.T) {
	old := veniceChannel()
	cand := deepSeekChannel()

	sessionCostApprox(t, "paybackLimit(A,B)", paybackLimit(old, cand), 5.88, 0.05)
	sessionCostApprox(t, "paybackTurns(20000,A,B)", paybackTurns(20000, old, cand, 1000, 1000), 18.04, 0.05)

	got, ok := crossCacheRead(old, cand, 1000, 1000)
	if !ok {
		t.Fatalf("crossCacheRead 应对 Venice→DeepSeek 返回 ok=true")
	}
	sessionCostApprox(t, "crossCacheRead(A,B)", got, 13480, 134.8)
}

func TestSessionCost_RemainingTurnsClamp(t *testing.T) {
	cfg := SessionCostConfig{RemainingA: 12, RemainingB: 1, RemainingMin: 1, RemainingMax: 20}

	if got := remainingTurns(1000, cfg); got != cfg.RemainingMin {
		t.Fatalf("k 很大应夹取到下限 %d, got=%d", cfg.RemainingMin, got)
	}
	if got := remainingTurns(-1000, cfg); got != cfg.RemainingMax {
		t.Fatalf("k 很小应夹取到上限 %d, got=%d", cfg.RemainingMax, got)
	}
	if got := remainingTurns(0, cfg); got < cfg.RemainingMin || got > cfg.RemainingMax {
		t.Fatalf("k=0 结果应落在 [%d,%d] 区间, got=%d", cfg.RemainingMin, cfg.RemainingMax, got)
	}
}

func TestSessionCost_RechargeCostFormula(t *testing.T) {
	cand := deepSeekChannel()
	sessionCostApprox(t, "rechargeCost(20000,B)", rechargeCost(20000, cand), 0.00294, 1e-6)
}

func TestShadow_DisabledHoldsChannelButKeepsAssumption(t *testing.T) {
	old := veniceChannel()
	cand := deepSeekChannel()
	const cr, newIn, out = int64(20000), int64(1000), int64(1000)
	base := SessionCostConfig{RemainingA: 20, RemainingB: 0, RemainingMin: 1, RemainingMax: 20}

	raw := decideSessionCost(base, &old, cand, cr, newIn, out, 0)
	if !raw.Switch {
		t.Fatalf("用例构造失败: 期望启用时会切换, got=%+v", raw)
	}

	cfg := base
	cfg.Enabled = false
	d := shadowDecision(cfg, raw)

	if d.Switch {
		t.Fatalf("影子模式关闭时不应切换(渠道不变): %+v", d)
	}
	if !strings.Contains(d.Reason, "影子") && !strings.Contains(d.Reason, "未启用") {
		t.Fatalf("影子模式 Reason 应含\"影子\"或\"未启用\": got=%q", d.Reason)
	}
	if d.PaybackTurns == 0 || math.IsNaN(d.PaybackTurns) {
		t.Fatalf("影子模式仍应保留有效回本轮数假设: got=%v", d.PaybackTurns)
	}
	sessionCostApprox(t, "影子 RechargeCost", d.RechargeCost, rechargeCost(cr, cand), 1e-12)
	if d.RechargeCost == 0 {
		t.Fatalf("影子模式重灌代价假设应非零: got=%v", d.RechargeCost)
	}
	sessionCostApprox(t, "影子 TurnCostOld", d.TurnCostOld, perTurnCost(old, cr, newIn, out), 1e-12)
	sessionCostApprox(t, "影子 TurnCostNew", d.TurnCostNew, perTurnCost(cand, cr, newIn, out), 1e-12)
	if d.TurnCostOld == 0 || d.TurnCostNew == 0 {
		t.Fatalf("影子模式每轮成本假设应非零: old=%v new=%v", d.TurnCostOld, d.TurnCostNew)
	}
}

func TestShadow_EnabledPassthrough(t *testing.T) {
	old := veniceChannel()
	cand := deepSeekChannel()
	const cr, newIn, out = int64(20000), int64(1000), int64(1000)
	base := SessionCostConfig{RemainingA: 20, RemainingB: 0, RemainingMin: 1, RemainingMax: 20}

	raw := decideSessionCost(base, &old, cand, cr, newIn, out, 0)
	if !raw.Switch {
		t.Fatalf("用例构造失败: 期望启用时会切换, got=%+v", raw)
	}

	cfg := base
	cfg.Enabled = true
	d := shadowDecision(cfg, raw)

	if d != raw {
		t.Fatalf("开关开启应原样返回: got=%+v want=%+v", d, raw)
	}
}

func TestShadow_DisabledNeverSwitchesEvenForStart(t *testing.T) {
	cand := deepSeekChannel()
	const cr, newIn, out = int64(20000), int64(1000), int64(1000)
	base := SessionCostConfig{RemainingA: 20, RemainingB: 0, RemainingMin: 1, RemainingMax: 20}

	raw := decideSessionCost(base, nil, cand, cr, newIn, out, 0)
	if !raw.Switch {
		t.Fatalf("用例构造失败: 起点应切换, got=%+v", raw)
	}

	cfg := base
	cfg.Enabled = false
	d := shadowDecision(cfg, raw)

	if d.Switch {
		t.Fatalf("影子模式关闭时起点也不应切换: %+v", d)
	}
	if !strings.Contains(d.Reason, "影子") && !strings.Contains(d.Reason, "未启用") {
		t.Fatalf("影子模式 Reason 应含\"影子\"或\"未启用\": got=%q", d.Reason)
	}
	if d.TurnCostNew == 0 || math.IsNaN(d.TurnCostNew) {
		t.Fatalf("影子模式起点仍应保留每轮成本假设: got=%v", d.TurnCostNew)
	}
	sessionCostApprox(t, "影子起点 TurnCostNew", d.TurnCostNew, perTurnCost(cand, cr, newIn, out), 1e-12)
	if d.RechargeCost != 0 {
		t.Fatalf("起点重灌代价假设应为 0: got=%v", d.RechargeCost)
	}
}
