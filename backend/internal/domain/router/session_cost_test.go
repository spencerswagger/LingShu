package router

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
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

// ---- Step 9：起点择优端到端测试 ----

// sessionCostProvider 构造可注入的会话成本路由配置提供者（Enabled 由参数决定）。
func sessionCostProvider(t *testing.T, enabled bool) *SessionCostProvider {
	t.Helper()
	raw := []byte(`{"enabled":false}`)
	if enabled {
		raw = []byte(`{"enabled":true,"remaining_a":12,"remaining_b":1,"remaining_min":1,"remaining_max":20,"min_remaining":1,"cooldown_sec":0}`)
	}
	return NewSessionCostProvider(newFakeConfigSource(map[string][]byte{cfgSessionCost: raw}))
}

// startPickSample 起点择优用例的本轮三段样值：缓存读占主导，
// 用于体现「按本轮花费择优」而非「最低输入价」（两渠道输入价可反转与本轮花费大小）。
func startPickSample() billing.Usage {
	return billing.Usage{Input: 1000, CacheRead: 20000, Output: 500}
}

// newStartPickEngine 构造同一对外模型下双候选（渠道 1/2）的引擎与会话注册表：
// 注入 Enabled 的会话成本提供者；queries 为本用例将发生的对外模型查询次数。
func newStartPickEngine(t *testing.T, enabled bool, ch1Rates, ch2Rates billing.Rates, p1, p2, queries int) (*Engine, *SessionRegistry) {
	t.Helper()
	svc, mock := newModelService(t, queries)
	t.Cleanup(func() { _ = mock.ExpectationsWereMet() })

	m1 := cm(1, "int1")
	m1.CostRates = ch1Rates
	m2 := cm(2, "int2")
	m2.CostRates = ch2Rates
	mgr := newManagerWithKeys(t,
		[]*channel.Channel{healthyCh(1, "a", nil, p1), healthyCh(2, "b", nil, p2)},
		[]channel.ChannelModel{m1, m2},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})

	sess := NewSessionRegistry(nil)
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{m1, m2}}, mgr, sess)
	e.SetSessionCostProvider(sessionCostProvider(t, enabled))
	return e, sess
}

// TestStartPick_Enabled_ChoosesMinPerTurnCost：同一对外模型多候选、新会话（起点）；
// 断言 Enabled=true 时按「本轮花费（重灌代价=0）最小」选渠道，并与手算一致；
// 同时断言 Enabled=false 门控下完全保持现状（priority 硬分层），渠道随开关切换。
func TestStartPick_Enabled_ChoosesMinPerTurnCost(t *testing.T) {
	tok := startPickSample()

	// ch1：缓存读更便宜 → 本轮花费最小；ch2：输入价更低但缓存读更贵 → 本轮花费更大。
	rates1 := billing.Rates{"input": 0.5, "output": 1.0, "cache_read": 0.05}
	rates2 := billing.Rates{"input": 0.3, "output": 1.5, "cache_read": 0.10}
	if rates2["input"] >= rates1["input"] {
		t.Fatalf("用例构造失败：ch2 输入价应低于 ch1（体现「最低输入价 ≠ 最低本轮花费」）")
	}

	cc1 := ChannelCost{Name: "int1", Input: 0.5, CacheRead: 0.05, Output: 1.0, TimeCoeff: 1, CtxCoeff: 1}
	cc2 := ChannelCost{Name: "int2", Input: 0.3, CacheRead: 0.10, Output: 1.5, TimeCoeff: 1, CtxCoeff: 1}
	want1 := perTurnCost(cc1, tok.CacheRead, tok.Input, tok.Output)
	want2 := perTurnCost(cc2, tok.CacheRead, tok.Input, tok.Output)
	sessionCostApprox(t, "手算 ch1 本轮花费", want1, 0.002, 1e-9)
	sessionCostApprox(t, "手算 ch2 本轮花费", want2, 0.00305, 1e-9)
	if want1 >= want2 {
		t.Fatalf("用例构造失败：期望 ch1 本轮花费更小, ch1=%v ch2=%v", want1, want2)
	}

	// ch1 与 ch2 **同优先级**：起点择优在同层内按本轮花费选 ch1；
	// ch2 的高优先级版本由 TestStartPick_PriorityOutranksCost 单独锁定（priority 不起作用时才比成本）。
	e, _ := newStartPickEngine(t, true, rates1, rates2, 100, 100, 2)

	r, err := e.Route(context.Background(), RouteSpec{
		ExternalModel: "qw-max", SessionKey: "s-start", SessionTokens: &tok,
	})
	if err != nil {
		t.Fatalf("route（启用起点择优）: %v", err)
	}
	if r.ChannelID != 1 {
		t.Fatalf("启用起点择优应按本轮花费最小选 ch1，got channel %d", r.ChannelID)
	}
	// 与手算一致：起点决策的 TurnCostNew 应等于手算 perTurnCost，且重灌代价=0（起点无旧缓存）。
	d := decideSessionCost(SessionCostConfig{Enabled: true}, nil, cc1, tok.CacheRead, tok.Input, tok.Output, 0)
	sessionCostApprox(t, "起点决策 TurnCostNew", d.TurnCostNew, want1, 1e-12)
	if !d.Switch {
		t.Fatalf("起点应切换/采用候选: %+v", d)
	}
	if d.RechargeCost != 0 {
		t.Fatalf("起点重灌代价应为 0: got=%v", d.RechargeCost)
	}

	// Enabled=false 门控：完全保持现状——默认排序用全局 routing.cost_sample（默认样值
	// 10000/9000/1000 下 ch2 更便宜），不使用会话本轮样值，故落在 ch2；与启用态（ch1）可鉴别。
	e.SetSessionCostProvider(sessionCostProvider(t, false))
	r0, err := e.Route(context.Background(), RouteSpec{
		ExternalModel: "qw-max", SessionKey: "s-start-off", SessionTokens: &tok,
	})
	if err != nil {
		t.Fatalf("route（未启用）: %v", err)
	}
	if r0.ChannelID != 2 {
		t.Fatalf("未启用时应保持现状（priority 更高的 ch2），got channel %d", r0.ChannelID)
	}
}

// TestStartPick_Enabled_AffinitySemantics：锁定「起点无旧缓存、非起点保持亲和」两类亲和语义：
//   - 起点（无活跃亲和）即便存在历史会话行残留（已关闭），仍按成本择优；
//   - 活跃会话（非起点）保持会话亲和，不受起点择优影响。
func TestStartPick_Enabled_AffinitySemantics(t *testing.T) {
	tok := startPickSample()
	// ch1 本轮花费最小（缓存读更便宜）；ch2 更贵。
	rates1 := billing.Rates{"input": 0.3, "output": 1.5, "cache_read": 0.05}
	rates2 := billing.Rates{"input": 0.3, "output": 1.5, "cache_read": 0.10}

	t.Run("起点含亲和残留仍按成本择优", func(t *testing.T) {
		e, sess := newStartPickEngine(t, true, rates1, rates2, 100, 100, 1)
		key := "s-residual"
		attachTo(t, sess, key, 21, "int2") // 历史会话行残留：绑定到更贵的 ch2
		if !sess.Close(key) {
			t.Fatal("关闭残留会话失败")
		}
		// 已关闭会话不构成活跃亲和（起点无旧缓存）→ 按本轮花费最小选 ch1，不沿用残留的 ch2。
		r, err := e.Route(context.Background(), RouteSpec{
			ExternalModel: "qw-max", SessionKey: key, SessionTokens: &tok,
		})
		if err != nil {
			t.Fatalf("route: %v", err)
		}
		if r.ChannelID != 1 {
			t.Fatalf("起点应忽略残留亲和、按成本选 ch1，got channel %d", r.ChannelID)
		}
	})

	t.Run("活跃会话非起点保持亲和", func(t *testing.T) {
		e, sess := newStartPickEngine(t, true, rates1, rates2, 100, 100, 1)
		key := "s-active"
		attachTo(t, sess, key, 21, "int2") // 活跃会话绑定更贵的 ch2
		r, err := e.Route(context.Background(), RouteSpec{
			ExternalModel: "qw-max", SessionKey: key, SessionTokens: &tok,
		})
		if err != nil {
			t.Fatalf("route: %v", err)
		}
		if r.ChannelID != 2 {
			t.Fatalf("非起点应保持会话亲和（ch2），got channel %d", r.ChannelID)
		}
	})
}

// ---- Step 12：中途切换门控测试 ----

// sessionCostProviderJSON 构造以给定 JSON 为源的可注入配置提供者（供中途切换用例定制开关/门槛）。
func sessionCostProviderJSON(t *testing.T, raw string) *SessionCostProvider {
	t.Helper()
	return NewSessionCostProvider(newFakeConfigSource(map[string][]byte{cfgSessionCost: []byte(raw)}))
}

// midSwitchEnabledJSON 启用「起点择优 + 中途切换」的配置（P2 子开关打开），k 经验式取常剩余 20 轮。
const midSwitchEnabledJSON = `{"enabled":true,"mid_switch":true,"remaining_a":20,"remaining_b":0,"remaining_min":1,"remaining_max":20,"min_remaining":1,"cooldown_sec":0,"min_improve":0}`

// newMidSwitchEngine 构造同一对外模型下双候选（渠道 1/2，密钥 11/21）的引擎与会话注册表：
// 注入给定 JSON 的会话成本配置；ch1（廉价缓存读）与 ch2（昂贵缓存读）用于体现「中途切到更便宜渠道」。
func newMidSwitchEngine(t *testing.T, cfgJSON string, ch1Rates, ch2Rates billing.Rates, p1, p2, queries int) (*Engine, *SessionRegistry) {
	t.Helper()
	svc, mock := newModelService(t, queries)
	t.Cleanup(func() { _ = mock.ExpectationsWereMet() })

	m1 := cm(1, "int1")
	m1.CostRates = ch1Rates
	m2 := cm(2, "int2")
	m2.CostRates = ch2Rates
	mgr := newManagerWithKeys(t,
		[]*channel.Channel{healthyCh(1, "a", nil, p1), healthyCh(2, "b", nil, p2)},
		[]channel.ChannelModel{m1, m2},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})

	sess := NewSessionRegistry(nil)
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{m1, m2}}, mgr, sess)
	e.SetSessionCostProvider(sessionCostProviderJSON(t, cfgJSON))
	return e, sess
}

// midSwitchRates 中途切换用例的两渠道成本价：ch1 缓存读更便宜（本轮花费更小），ch2 缓存读更贵。
func midSwitchRates() (billing.Rates, billing.Rates) {
	return billing.Rates{"input": 0.3, "output": 1.5, "cache_read": 0.05},
		billing.Rates{"input": 0.3, "output": 1.5, "cache_read": 0.10}
}

// ① 预计剩余轮数 > 回本轮数 → 切换。
func TestMidSwitch_DecisionSwitchesWhenRemainingExceedsPayback(t *testing.T) {
	cfg := SessionCostConfig{RemainingA: 20, RemainingB: 0, RemainingMin: 1, RemainingMax: 20}
	old := veniceChannel()
	cand := deepSeekChannel()
	const cr, newIn, out = int64(20000), int64(1000), int64(1000)

	pb := paybackTurns(cr, old, cand, newIn, out)
	rem := remainingTurns(0, cfg)
	if float64(rem) <= pb {
		t.Fatalf("用例构造失败：期望剩余轮数 > 回本轮数, rem=%d pb=%v", rem, pb)
	}
	d := decideSessionCost(cfg, &old, cand, cr, newIn, out, 0)
	if !d.Switch {
		t.Fatalf("剩余轮数 %d > 回本 %v 应切换: %+v", rem, pb, d)
	}
	if !strings.Contains(d.Reason, "切换") {
		t.Fatalf("切换 Reason 应说明切换: %q", d.Reason)
	}
	sessionCostApprox(t, "MidSwitch ① PaybackTurns", d.PaybackTurns, pb, 1e-9)
}

// ② 预计剩余轮数 ≤ 回本轮数 → 不切换（保持亲和）。
func TestMidSwitch_DecisionHoldsWhenRemainingNotExceed(t *testing.T) {
	cfg := SessionCostConfig{RemainingA: 10, RemainingB: 0, RemainingMin: 1, RemainingMax: 20}
	old := veniceChannel()
	cand := deepSeekChannel()
	const cr, newIn, out = int64(20000), int64(1000), int64(1000)

	pb := paybackTurns(cr, old, cand, newIn, out)
	rem := remainingTurns(0, cfg)
	if float64(rem) > pb {
		t.Fatalf("用例构造失败：期望剩余轮数 ≤ 回本轮数, rem=%d pb=%v", rem, pb)
	}
	d := decideSessionCost(cfg, &old, cand, cr, newIn, out, 0)
	if d.Switch {
		t.Fatalf("剩余轮数 %d ≤ 回本 %v 应保持亲和(不切换): %+v", rem, pb, d)
	}
	if !strings.Contains(d.Reason, "亲和") {
		t.Fatalf("保持亲和 Reason 应说明保持亲和: %q", d.Reason)
	}
}

// ③ 分母 ≤ 0 → 永不回本不切换。
func TestMidSwitch_DecisionNeverPaybackWhenDenNonPositive(t *testing.T) {
	old := veniceChannel()
	cand := ChannelCost{Name: "not-cheaper-cache", Input: 0.150, CacheRead: 0.030, Output: 0.600, TimeCoeff: 1, CtxCoeff: 1}
	const cr, newIn, out = int64(20000), int64(1000), int64(1000)

	if cand.CacheRead < old.CacheRead {
		t.Fatalf("用例构造失败：候选缓存读不应更便宜, old=%v new=%v", old.CacheRead, cand.CacheRead)
	}
	if pb := paybackTurns(cr, old, cand, newIn, out); !math.IsNaN(pb) {
		t.Fatalf("分母 ≤ 0 应返回 NaN, got=%v", pb)
	}
	cfg := SessionCostConfig{RemainingA: 20, RemainingB: 0, RemainingMin: 1, RemainingMax: 20}
	if d := decideSessionCost(cfg, &old, cand, cr, newIn, out, 0); d.Switch {
		t.Fatalf("永不回本应保持亲和(不切换): %+v", d)
	}
}

// ④ 缓存读低于临界（crossCacheRead ≈ 1.35 万）时起点/中途均不切换。
// 「起点/中途」用会话位置 k 刻画：k=0 为起点位置、k>0 为中途位置；两方向均不得切换。
func TestMidSwitch_HoldsBelowCriticalCacheRead(t *testing.T) {
	a := veniceChannel()   // 缓存读更贵（0.028），低缓存读时本轮更便宜
	b := deepSeekChannel() // 缓存读更便宜（0.003），低缓存读时本轮更贵
	const newIn, out = int64(1000), int64(1000)

	crit, ok := crossCacheRead(a, b, newIn, out)
	if !ok {
		t.Fatalf("crossCacheRead(Venice,DeepSeek) 应返回 ok=true")
	}
	sessionCostApprox(t, "crossCacheRead 临界", crit, 13480, 134.8)

	// 低于临界的缓存读：Venice(A) 每轮更便宜。
	cr := int64(13000)
	if perTurnCost(a, cr, newIn, out) >= perTurnCost(b, cr, newIn, out) {
		t.Fatalf("用例构造失败：低于临界应 A(Venice) 每轮更便宜")
	}

	cfg := SessionCostConfig{RemainingA: 20, RemainingB: 0, RemainingMin: 1, RemainingMax: 20}
	for _, k := range []int{0, 1, 5, 20} { // k=0 起点位置；k>0 中途位置
		// 当前为更便宜的 A、候选为更贵的 B：分母 ≤ 0 → 永不回本 → 不切换。
		if d := decideSessionCost(cfg, &a, b, cr, newIn, out, k); d.Switch {
			t.Fatalf("低于临界且当前更便宜(k=%d)不应切到更贵渠道: %+v", k, d)
		}
		// 当前为更贵的 B、候选为更便宜的 A：回本轮数(≈119)远超剩余轮数 → 不切换。
		if d := decideSessionCost(cfg, &b, a, cr, newIn, out, k); d.Switch {
			t.Fatalf("低于临界时切换到 A 的回本成本过高(k=%d)不应切换: %+v", k, d)
		}
	}
}

// TestMidSwitch_EngineDecidesWithoutMigrating（F1）引擎级：Enabled+MidSwitch 且为中途（亲和命中）时，
// 按回本门控决策切到本轮更便宜的渠道，但**路由阶段不迁移会话**——决策产物与切换前归属挂在目标候选上，
// 由网关在候选通过可用性/限流校验后调用 ConfirmSessionSwitch 才真正迁移（Lookup/计数随之更新）。
//
// 这样避免「会话已迁到某渠道、但该渠道随后被限流/跳过」导致的归属偏置与并发计数错位。
func TestMidSwitch_EngineDecidesWithoutMigrating(t *testing.T) {
	rates1, rates2 := midSwitchRates()
	tok := startPickSample()

	e, sess := newMidSwitchEngine(t, midSwitchEnabledJSON, rates1, rates2, 100, 100, 1)
	key := "s-mid"
	attachTo(t, sess, key, 21, "int2") // 中途：会话亲和命中更贵的 ch2

	res, err := e.RouteAllWithExcluded(context.Background(), RouteSpec{
		ExternalModel: "qw-max", SessionKey: key, SessionTokens: &tok,
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if len(res.Routes) == 0 {
		t.Fatal("应有候选")
	}
	cr := res.Routes[0]
	if cr.ChannelID != 1 || cr.ChannelKeyID != 11 {
		t.Fatalf("中途应决策切到本轮更便宜的 ch1，got channel %d key=%d", cr.ChannelID, cr.ChannelKeyID)
	}
	if cr.Switch == nil {
		t.Fatal("目标候选应携带切换决策产物（Switch 非 nil）")
	}
	if cr.SwitchFromKeyID != 21 {
		t.Fatalf("应记录切换前归属密钥 21，got %d", cr.SwitchFromKeyID)
	}
	if !cr.Switch.Switch || cr.Switch.ToChannelKeyID != 11 || cr.Switch.FromChannelKeyID != 21 {
		t.Fatalf("决策产物不符：%+v", cr.Switch)
	}
	// F4：目标候选必须回填成本标量/时段系数（否则决策轨迹显示「成本 0/未知」）。
	if !cr.CostKnown || cr.Cost <= 0 {
		t.Fatalf("目标候选应回填有效成本，got known=%v cost=%v", cr.CostKnown, cr.Cost)
	}
	if cr.TimeCoeff <= 0 {
		t.Fatalf("目标候选应回填时段系数，got %v", cr.TimeCoeff)
	}

	// 路由阶段**不得**迁移会话（仍是旧的 key 21 / int2）。
	if s, ok := sess.Lookup(key); !ok || s.ChannelKeyID != 21 || s.InternalModelID != "int2" {
		t.Fatalf("路由阶段不应迁移会话，got %+v ok=%v", s, ok)
	}
	if sess.CountActive(21) != 1 || sess.CountActive(11) != 0 {
		t.Fatalf("路由阶段计数不应变化：key21=%d key11=%d", sess.CountActive(21), sess.CountActive(11))
	}

	// 网关确认采用该候选后才迁移（F8：带真实 ttl，ExpireAt 只增不减）。
	before, _ := sess.Lookup(key)
	if !e.ConfirmSessionSwitch(key, cr, 0, time.Hour) {
		t.Fatal("确认切换应成功迁移会话")
	}
	s, ok := sess.Lookup(key)
	if !ok || s.ChannelKeyID != 11 || s.InternalModelID != "int1" {
		t.Fatalf("确认后会话应迁移到新渠道(key=11,int1)，got %+v ok=%v", s, ok)
	}
	if !s.ExpireAt.After(before.ExpireAt) {
		t.Fatalf("迁移应滑动延长 ExpireAt，before=%v after=%v", before.ExpireAt, s.ExpireAt)
	}
	if sess.CountActive(11) != 1 || sess.CountActive(21) != 0 {
		t.Fatalf("迁移后计数异常：key11=%d key21=%d", sess.CountActive(11), sess.CountActive(21))
	}
}

// TestMidSwitch_PriorityOutranksCost（F6）起点择优的 priority 是硬分层：低优先级渠道即便本轮更便宜，
// 也不得压过高优先级渠道（与默认排序同语义）。
func TestMidSwitch_PriorityOutranksCost(t *testing.T) {
	tok := startPickSample()
	// ch1 本轮花费更小（缓存读更便宜），但优先级只有 50；ch2 更贵但优先级 100。
	rates1 := billing.Rates{"input": 0.3, "output": 1.5, "cache_read": 0.05}
	rates2 := billing.Rates{"input": 0.3, "output": 1.5, "cache_read": 0.10}

	e, _ := newStartPickEngine(t, true, rates1, rates2, 50, 100, 2)
	r, err := e.Route(context.Background(), RouteSpec{
		ExternalModel: "qw-max", SessionKey: "s-priority", SessionTokens: &tok,
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 2 {
		t.Fatalf("priority 应优先于成本：应选高优先级的 ch2，got channel %d", r.ChannelID)
	}

	// 同优先级时才按本轮花费择优（此时 ch1 胜出）。
	e2, _ := newStartPickEngine(t, true, rates1, rates2, 100, 100, 2)
	r2, err := e2.Route(context.Background(), RouteSpec{
		ExternalModel: "qw-max", SessionKey: "s-same-priority", SessionTokens: &tok,
	})
	if err != nil {
		t.Fatalf("route2: %v", err)
	}
	if r2.ChannelID != 1 {
		t.Fatalf("同优先级应按本轮花费选 ch1，got channel %d", r2.ChannelID)
	}
}

// TestExpandSample_InputOnlySplitByProfile（F3）路由时刻只有「总输入」时，按
// routing.sample_profile 的比例拆成三段（缓存读/纯输入/输出），避免退化为「只按输入价择优」。
func TestExpandSample_InputOnlySplitByProfile(t *testing.T) {
	e := &Engine{costCfg: &fakeCostConfig{}}
	got := e.expandSample(billing.Usage{Input: 10000}) // 总输入 10000
	// 默认比例 cache_read_ratio=0.9 → 纯输入 = 10000/1.9 ≈ 5263，缓存读 ≈ 4737，输出 ≈ 526。
	sessionCostApprox(t, "纯输入", float64(got.Input), 5263, 2)
	sessionCostApprox(t, "缓存读", float64(got.CacheRead), 4737, 2)
	sessionCostApprox(t, "输出", float64(got.Output), 526, 2)
	if got.Input+got.CacheRead != 10000 {
		t.Fatalf("纯输入 + 缓存读应等于总输入 10000，got %d", got.Input+got.CacheRead)
	}

	// 已含缓存读/输出的样值原样返回（不重复展开）。
	tri := billing.Usage{Input: 1, CacheRead: 2, Output: 3}
	if out := e.expandSample(tri); out != tri {
		t.Fatalf("三段样值不应被改写，got %+v", out)
	}
	// 全零样值（显式关闭成本择优）原样返回。
	if out := e.expandSample(billing.Usage{}); out != (billing.Usage{}) {
		t.Fatalf("全零样值不应被改写，got %+v", out)
	}
	// 未注入 CostConfig 时原样返回（不猜测比例）。
	if out := (&Engine{}).expandSample(billing.Usage{Input: 10000}); out != (billing.Usage{Input: 10000}) {
		t.Fatalf("未注入配置时应原样返回，got %+v", out)
	}
}

// TestMidSwitch_EngineKeepsAffinityWhenMidSwitchOff Enabled=true 但未开 MidSwitch（P2 安全态）：
// 中途保持会话亲和、渠道不变（与既有 StartPick 亲和语义一致，零行为变化）。
func TestMidSwitch_EngineKeepsAffinityWhenMidSwitchOff(t *testing.T) {
	rates1, rates2 := midSwitchRates()
	tok := startPickSample()

	const offJSON = `{"enabled":true,"remaining_a":20,"remaining_b":0,"remaining_min":1,"remaining_max":20,"min_remaining":1,"cooldown_sec":0}`
	e, sess := newMidSwitchEngine(t, offJSON, rates1, rates2, 100, 100, 1)
	key := "s-off"
	attachTo(t, sess, key, 21, "int2")

	r, err := e.Route(context.Background(), RouteSpec{
		ExternalModel: "qw-max", SessionKey: key, SessionTokens: &tok,
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 2 {
		t.Fatalf("未开 MidSwitch 应保持会话亲和（ch2），got channel %d", r.ChannelID)
	}
	if s, _ := sess.Lookup(key); s.ChannelKeyID != 21 {
		t.Fatalf("未开 MidSwitch 不应迁移会话，got key %d", s.ChannelKeyID)
	}
}

// TestMidSwitch_MinRemainingSuppressesSwitch 抖动抑制：剩余轮数低于 MinRemaining 门槛时不切换。
func TestMidSwitch_MinRemainingSuppressesSwitch(t *testing.T) {
	rates1, rates2 := midSwitchRates()
	tok := startPickSample()

	// remaining 恒为 20（a=20,b=0,max=20），门槛设为 25 → 20 < 25 → 抑制切换。
	const gateJSON = `{"enabled":true,"mid_switch":true,"remaining_a":20,"remaining_b":0,"remaining_min":1,"remaining_max":20,"min_remaining":25,"cooldown_sec":0,"min_improve":0}`
	e, sess := newMidSwitchEngine(t, gateJSON, rates1, rates2, 100, 100, 1)
	key := "s-minrem"
	attachTo(t, sess, key, 21, "int2")

	r, err := e.Route(context.Background(), RouteSpec{
		ExternalModel: "qw-max", SessionKey: key, SessionTokens: &tok,
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 2 {
		t.Fatalf("剩余轮数低于门槛应保持亲和（ch2），got channel %d", r.ChannelID)
	}
}

// TestMidSwitch_CooldownSuppressesSwitch 抖动抑制：距上次切换不足 CooldownSec 时不切换。
func TestMidSwitch_CooldownSuppressesSwitch(t *testing.T) {
	rates1, rates2 := midSwitchRates()
	tok := startPickSample()

	const cdJSON = `{"enabled":true,"mid_switch":true,"remaining_a":20,"remaining_b":0,"remaining_min":1,"remaining_max":20,"min_remaining":1,"cooldown_sec":600,"min_improve":0}`
	e, sess := newMidSwitchEngine(t, cdJSON, rates1, rates2, 100, 100, 1)
	key := "s-cd"
	attachTo(t, sess, key, 21, "int2")
	if s, ok := sess.Lookup(key); !ok {
		t.Fatal("会话应存在")
	} else {
		s.LastSwitchAt = time.Now() // 刚刚切换过 → 冷却生效
	}

	r, err := e.Route(context.Background(), RouteSpec{
		ExternalModel: "qw-max", SessionKey: key, SessionTokens: &tok,
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 2 {
		t.Fatalf("冷却期内应保持亲和（ch2），got channel %d", r.ChannelID)
	}
}

// TestMidSwitch_HysteresisSuppressesSwitch 抖动抑制：新渠道相对改善未超滞回阈值 MinImprove 时不切换。
func TestMidSwitch_HysteresisSuppressesSwitch(t *testing.T) {
	rates1, rates2 := midSwitchRates()
	tok := startPickSample()

	// ch1 相对 ch2 的本轮改善约 32.8%（(0.00305-0.00205)/0.00305）；阈值设为 0.5 → 不达标 → 抑制。
	const hystJSON = `{"enabled":true,"mid_switch":true,"remaining_a":20,"remaining_b":0,"remaining_min":1,"remaining_max":20,"min_remaining":1,"cooldown_sec":0,"min_improve":0.5}`
	e, sess := newMidSwitchEngine(t, hystJSON, rates1, rates2, 100, 100, 1)
	key := "s-hyst"
	attachTo(t, sess, key, 21, "int2")

	r, err := e.Route(context.Background(), RouteSpec{
		ExternalModel: "qw-max", SessionKey: key, SessionTokens: &tok,
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 2 {
		t.Fatalf("改善未超滞回阈值应保持亲和（ch2），got channel %d", r.ChannelID)
	}
}

// ---- FIX-A：影子假设记录（未启用切换仍产出「若启用则会切换」的假设节省事件） ----

// TestShadow_EngineEmitsAssumptionWhenMidSwitchOff Enabled=true、MidSwitch=false 且显式开启
// shadow_enabled=true（F5：影子观测为独立开关，默认关闭）：
// 渠道保持亲和不变（ch2），但经既有 sink 产出一条「若启用则会切换」的影子假设事件（Shadow=true、
// Switch=false、含 TurnCostOld/New、RechargeCost、PaybackTurns、PaybackLimit、Reason 带影子前缀）。
func TestShadow_EngineEmitsAssumptionWhenMidSwitchOff(t *testing.T) {
	rates1, rates2 := midSwitchRates()
	tok := startPickSample()

	const offJSON = `{"enabled":true,"shadow_enabled":true,"remaining_a":20,"remaining_b":0,"remaining_min":1,"remaining_max":20,"min_remaining":1,"cooldown_sec":0}`
	e, sess := newMidSwitchEngine(t, offJSON, rates1, rates2, 100, 100, 1)
	sink := &fakeCostEventSink{}
	e.SetCostEventSink(sink)
	key := "s-shadow-off"
	attachTo(t, sess, key, 21, "int2") // 亲和命中更贵的 ch2（若启用则会切到更便宜的 ch1）

	r, err := e.Route(context.Background(), RouteSpec{
		ExternalModel: "qw-max", SessionKey: key, SessionTokens: &tok,
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 2 {
		t.Fatalf("MidSwitch 关闭时应保持亲和（ch2），渠道不得变，got channel %d", r.ChannelID)
	}
	evs := sink.snapshot()
	if len(evs) != 1 {
		t.Fatalf("应产出 1 条影子假设事件，got %d: %+v", len(evs), evs)
	}
	ev := evs[0]
	if !ev.Shadow {
		t.Fatalf("事件应标记 Shadow=true：%+v", ev)
	}
	if ev.Decision.Switch {
		t.Fatalf("影子事件不得切换（Switch=false）：%+v", ev.Decision)
	}
	if !strings.Contains(ev.Decision.Reason, "影子") {
		t.Fatalf("影子事件 Reason 应含\"影子\"：%q", ev.Decision.Reason)
	}
	if ev.Decision.FromChannelKeyID != 21 || ev.Decision.ToChannelKeyID != 11 {
		t.Fatalf("影子事件转向应 21→11，got %d→%d", ev.Decision.FromChannelKeyID, ev.Decision.ToChannelKeyID)
	}
	if ev.Decision.TurnCostOld <= 0 || ev.Decision.TurnCostNew <= 0 || ev.Decision.RechargeCost <= 0 {
		t.Fatalf("影子事件应保留非零假设字段：%+v", ev.Decision)
	}
	if ev.Decision.PaybackTurns <= 0 || math.IsNaN(ev.Decision.PaybackTurns) {
		t.Fatalf("影子事件应保留有效回本假设：%v", ev.Decision.PaybackTurns)
	}
	if ev.Decision.PaybackLimit <= 0 || math.IsNaN(ev.Decision.PaybackLimit) {
		t.Fatalf("影子事件应携带正 PaybackLimit：%v", ev.Decision.PaybackLimit)
	}
}

// TestShadow_EngineEmitsAssumptionWhenDisabled Enabled=false（存在 provider）：渠道保持现状（亲和 ch2），
// 仅多一条影子假设事件（Reason 带「未启用」前缀）；锁定「Enabled=false 对外路由行为零变化」。
func TestShadow_EngineEmitsAssumptionWhenDisabled(t *testing.T) {
	rates1, rates2 := midSwitchRates()
	tok := startPickSample()

	const disabledJSON = `{"enabled":false,"shadow_enabled":true,"remaining_a":20,"remaining_b":0,"remaining_min":1,"remaining_max":20}`
	e, sess := newMidSwitchEngine(t, disabledJSON, rates1, rates2, 100, 100, 1)
	sink := &fakeCostEventSink{}
	e.SetCostEventSink(sink)
	key := "s-shadow-disabled"
	attachTo(t, sess, key, 21, "int2")

	r, err := e.Route(context.Background(), RouteSpec{
		ExternalModel: "qw-max", SessionKey: key, SessionTokens: &tok,
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 2 {
		t.Fatalf("Enabled=false 应保持现状（亲和 ch2），got channel %d", r.ChannelID)
	}
	evs := sink.snapshot()
	if len(evs) != 1 {
		t.Fatalf("应产出 1 条影子假设事件，got %d", len(evs))
	}
	ev := evs[0]
	if !ev.Shadow || ev.Decision.Switch {
		t.Fatalf("影子事件应 Shadow=true 且 Switch=false：%+v", ev)
	}
	if !strings.Contains(ev.Decision.Reason, "未启用") && !strings.Contains(ev.Decision.Reason, "影子") {
		t.Fatalf("影子事件 Reason 应含\"未启用\"或\"影子\"：%q", ev.Decision.Reason)
	}
}

// TestShadow_NotEmittedWhenNoWouldSwitch 亲和命中更便宜的 ch1（若启用也不会切换）→ 不产出影子事件。
func TestShadow_NotEmittedWhenNoWouldSwitch(t *testing.T) {
	rates1, rates2 := midSwitchRates()
	tok := startPickSample()

	const offJSON = `{"enabled":true,"shadow_enabled":true,"remaining_a":20,"remaining_b":0,"remaining_min":1,"remaining_max":20,"min_remaining":1,"cooldown_sec":0}`
	e, sess := newMidSwitchEngine(t, offJSON, rates1, rates2, 100, 100, 1)
	sink := &fakeCostEventSink{}
	e.SetCostEventSink(sink)
	key := "s-shadow-cheap"
	attachTo(t, sess, key, 11, "int1") // 亲和命中更便宜的 ch1：若启用也不切换

	r, err := e.Route(context.Background(), RouteSpec{
		ExternalModel: "qw-max", SessionKey: key, SessionTokens: &tok,
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 1 {
		t.Fatalf("应保持亲和（ch1），got channel %d", r.ChannelID)
	}
	if evs := sink.snapshot(); len(evs) != 0 {
		t.Fatalf("若启用也不会切换时不应产出影子事件，got %d: %+v", len(evs), evs)
	}
}

// TestShadow_NotEmittedByDefault（F5）影子观测默认关闭：未显式开启 shadow_enabled 时，
// 即便「若启用则会切换」，也不得产出任何事件（安全态零额外异步写与可观测副作用）。
func TestShadow_NotEmittedByDefault(t *testing.T) {
	rates1, rates2 := midSwitchRates()
	tok := startPickSample()

	const offJSON = `{"enabled":true,"remaining_a":20,"remaining_b":0,"remaining_min":1,"remaining_max":20,"min_remaining":1,"cooldown_sec":0}`
	e, sess := newMidSwitchEngine(t, offJSON, rates1, rates2, 100, 100, 1)
	sink := &fakeCostEventSink{}
	e.SetCostEventSink(sink)
	key := "s-shadow-default-off"
	attachTo(t, sess, key, 21, "int2") // 亲和命中更贵 ch2 → 若开启影子会记录切到 ch1

	r, err := e.Route(context.Background(), RouteSpec{
		ExternalModel: "qw-max", SessionKey: key, SessionTokens: &tok,
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 2 {
		t.Fatalf("应保持亲和（ch2），got channel %d", r.ChannelID)
	}
	if evs := sink.snapshot(); len(evs) != 0 {
		t.Fatalf("影子观测默认关闭时不应产出事件，got %d: %+v", len(evs), evs)
	}
}

// TestSessionCost_DecisionCarriesPaybackLimit FIX-B：中途决策携带 paybackLimit(旧,新)，
// 起点（无旧渠道）置 0（无意义）。
func TestSessionCost_DecisionCarriesPaybackLimit(t *testing.T) {
	old := veniceChannel()
	cand := deepSeekChannel()
	const cr, newIn, out = int64(20000), int64(1000), int64(1000)
	cfg := SessionCostConfig{RemainingA: 20, RemainingB: 0, RemainingMin: 1, RemainingMax: 20}

	d := decideSessionCost(cfg, &old, cand, cr, newIn, out, 0)
	sessionCostApprox(t, "中途 PaybackLimit", d.PaybackLimit, paybackLimit(old, cand), 1e-12)
	if math.IsNaN(d.PaybackLimit) || d.PaybackLimit <= 0 {
		t.Fatalf("中途 PaybackLimit 应为正有限值，got %v", d.PaybackLimit)
	}

	start := decideSessionCost(cfg, nil, cand, cr, newIn, out, 0)
	if start.PaybackLimit != 0 {
		t.Fatalf("起点无旧渠道，PaybackLimit 应为 0，got %v", start.PaybackLimit)
	}
}
