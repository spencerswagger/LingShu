package router

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/pkg/asyncwrite"
)

// fakeCostEventSink 记录引擎提交的决策产物，供断言「决策后经 sink 提交」与错误透传。
type fakeCostEventSink struct {
	mu     sync.Mutex
	events []SessionCostEvent
	err    error
}

func (f *fakeCostEventSink) Submit(ev SessionCostEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, ev)
	return nil
}

func (f *fakeCostEventSink) snapshot() []SessionCostEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SessionCostEvent(nil), f.events...)
}

// TestCostEvent_AsyncSubmitsEachEmission 锁定：启用异步时每一次发射都各自留痕。
// 注：不再按 (session, turn) 去重——网关 SessionTurn 恒 0，按轮次去重会把同轮次的多次独立决策
// 静默丢弃；幂等键改为每次发射唯一的 session:Seq（见 Submit 注释）。
func TestCostEvent_AsyncSubmitsEachEmission(t *testing.T) {
	ctx := context.Background()
	w := asyncwrite.New(asyncwrite.Config{
		WALPath:    filepath.Join(t.TempDir(), "asyncwrite.wal"),
		AsyncKinds: map[asyncwrite.Kind]bool{asyncwrite.KindSessionCostEvent: true},
	})
	w.Start(ctx)
	defer func() { _ = w.Shutdown(ctx) }()

	sink := NewAsyncCostEventSink(w).(*asyncCostEventSink)
	var mu sync.Mutex
	var landed []SessionCostEvent
	sink.persist = func(_ context.Context, ev SessionCostEvent) error {
		mu.Lock()
		landed = append(landed, ev)
		mu.Unlock()
		return nil
	}

	ev1 := SessionCostEvent{SessionID: "s1", Turn: 1, Decision: SessionCostDecision{Switch: true, Reason: "起点"}}
	if err := sink.Submit(ev1); err != nil {
		t.Fatalf("首次提交失败: %v", err)
	}
	if err := sink.Submit(ev1); err != nil {
		t.Fatalf("再次提交失败: %v", err)
	}
	if err := sink.Submit(SessionCostEvent{SessionID: "s1", Turn: 2, Decision: SessionCostDecision{Switch: false}}); err != nil {
		t.Fatalf("不同轮次提交失败: %v", err)
	}

	waitAsyncDepthZero(t, w)

	mu.Lock()
	got := append([]SessionCostEvent(nil), landed...)
	mu.Unlock()

	if len(got) != 3 {
		t.Fatalf("每次发射都应留痕（共 3 次提交），got %d: %+v", len(got), got)
	}
	if got[0].Seq == got[1].Seq || got[1].Seq == got[2].Seq || got[0].Seq == got[2].Seq {
		t.Fatalf("Seq 应每次发射唯一，got %d/%d/%d", got[0].Seq, got[1].Seq, got[2].Seq)
	}
}

// TestCostEvent_SameTurnSameTransitionNotDeduped 回归：网关 SessionTurn 恒 0 时，
// 同一会话内「同一轮次、同一 from>to 转移」的多次独立决策（如影子记录、来回切换）必须各自留痕，
// 不得因幂等键撞车被 asyncwrite 去重丢弃（旧实现用 session:turn:from>to 会把后发的静默丢弃）。
func TestCostEvent_SameTurnSameTransitionNotDeduped(t *testing.T) {
	ctx := context.Background()
	w := asyncwrite.New(asyncwrite.Config{
		WALPath:    filepath.Join(t.TempDir(), "asyncwrite.wal"),
		AsyncKinds: map[asyncwrite.Kind]bool{asyncwrite.KindSessionCostEvent: true},
	})
	w.Start(ctx)
	defer func() { _ = w.Shutdown(ctx) }()

	sink := NewAsyncCostEventSink(w).(*asyncCostEventSink)
	var mu sync.Mutex
	var landed int
	sink.persist = func(_ context.Context, _ SessionCostEvent) error {
		mu.Lock()
		landed++
		mu.Unlock()
		return nil
	}

	ev := SessionCostEvent{
		SessionID: "s-collide",
		Turn:      0, // 网关现状：恒 0
		Decision:  SessionCostDecision{Switch: false, FromChannelKeyID: 7, ToChannelKeyID: 9},
	}
	for i := 0; i < 3; i++ {
		if err := sink.Submit(ev); err != nil {
			t.Fatalf("第 %d 次提交失败: %v", i+1, err)
		}
	}
	waitAsyncDepthZero(t, w)

	mu.Lock()
	got := landed
	mu.Unlock()
	if got != 3 {
		t.Fatalf("同 (session, turn, from>to) 的 3 次独立决策都应留痕，got %d（旧实现会去重为 1）", got)
	}
}

// TestCostEvent_SyncSafeModeWhenKindNotEnabled 锁定安全态：KindSessionCostEvent 未登记进
// AsyncKinds 时同步执行（Submit 返回前即已落地），不改变既有默认行为。
func TestCostEvent_SyncSafeModeWhenKindNotEnabled(t *testing.T) {
	w := asyncwrite.New(asyncwrite.Config{
		WALPath:    filepath.Join(t.TempDir(), "asyncwrite.wal"),
		AsyncKinds: map[asyncwrite.Kind]bool{asyncwrite.KindCallLog: true}, // 未登记成本事件 → 安全态
	})
	sink := NewAsyncCostEventSink(w).(*asyncCostEventSink)
	var n int64
	sink.persist = func(_ context.Context, _ SessionCostEvent) error {
		atomic.AddInt64(&n, 1)
		return nil
	}

	// 同步执行：无需 Start / 异步消费者，Submit 返回时副作用已完成。
	if err := sink.Submit(SessionCostEvent{SessionID: "s", Turn: 1}); err != nil {
		t.Fatalf("同步安全态提交失败: %v", err)
	}
	if got := atomic.LoadInt64(&n); got != 1 {
		t.Fatalf("未登记类别应同步执行 1 次，got %d", got)
	}
	if d := w.Stats().Depth; d != 0 {
		t.Fatalf("同步执行不应入队，Depth=%d", d)
	}
}

// TestCostEvent_EngineNilSinkNoPanicAndRouteUnaffected 锁定：sink 未注入时决策产出提交 no-op、
// 不 panic，且路由照常（不因未注入 sink 而阻断）。
func TestCostEvent_EngineNilSinkNoPanicAndRouteUnaffected(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	cheap := cmCost(1, "int1", 1)
	mgr := newManagerWithKeys(t, []*channel.Channel{healthyCh(1, "a", nil, 100)},
		[]channel.ChannelModel{cheap},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{cheap}}, mgr, nil)

	if err := e.EmitSessionCostEvent("s1", 1, SessionCostDecision{Switch: true}); err != nil {
		t.Fatalf("未注入 sink 时应 no-op 返回 nil，got %v", err)
	}

	r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 1 {
		t.Fatalf("sink 未注入不应影响路由结果，got channel %d", r.ChannelID)
	}
}

// TestCostEvent_EngineForwardsDecisionToSink 锁定：决策产物经注入的 sink 原样提交；
// sink 提交错误原样返回（供调用方按需记录），不 panic。
func TestCostEvent_EngineForwardsDecisionToSink(t *testing.T) {
	e := NewEngine(nil, nil, nil, nil)
	fs := &fakeCostEventSink{}
	e.SetCostEventSink(fs)

	d := SessionCostDecision{Switch: true, TurnCostNew: 0.5, Reason: "起点（会话注册表未命中）"}
	if err := e.EmitSessionCostEvent("s1", 3, d); err != nil {
		t.Fatalf("提交决策产物失败: %v", err)
	}
	got := fs.snapshot()
	if len(got) != 1 {
		t.Fatalf("期望 sink 收到 1 条，got %d", len(got))
	}
	if got[0].SessionID != "s1" || got[0].Turn != 3 || got[0].Decision != d {
		t.Fatalf("sink 收到的决策产物不符: %+v", got[0])
	}

	// sink 错误原样返回。
	e.SetCostEventSink(&fakeCostEventSink{err: errors.New("sink 故障")})
	if err := e.EmitSessionCostEvent("s1", 4, d); err == nil {
		t.Fatal("sink 提交错误应原样返回")
	}
}

// TestCostEvent_WALRoundTripPreservesNonFinite 锁定：非有限浮点（「未知」）经 WAL 线格式
// 编码 → 重放解码后仍为 NaN，不会退化成 0（否则崩溃重启后「未知」会被当成真实值 0）。
func TestCostEvent_WALRoundTripPreservesNonFinite(t *testing.T) {
	ev := SessionCostEvent{
		SessionID:      "s-nan",
		Turn:           7,
		CrossCacheRead: math.Inf(1),
		Decision: SessionCostDecision{
			Switch:           true,
			FromChannelKeyID: 1,
			ToChannelKeyID:   2,
			TurnCostOld:      math.NaN(),
			TurnCostNew:      1.5,
			RechargeCost:     math.NaN(),
			PaybackTurns:     math.NaN(),
			PaybackLimit:     math.NaN(),
		},
	}
	payload, err := marshalEventForJSON(ev)
	if err != nil {
		t.Fatalf("marshalEventForJSON: %v", err)
	}
	if !math.IsInf(ev.CrossCacheRead, 1) || !math.IsNaN(ev.Decision.PaybackLimit) {
		t.Fatalf("marshalEventForJSON 不应修改入参，got %+v", ev)
	}

	sink := NewAsyncCostEventSink(nil).(*asyncCostEventSink)
	var got SessionCostEvent
	var landed bool
	sink.persist = func(_ context.Context, e SessionCostEvent) error {
		got = e
		landed = true
		return nil
	}
	if err := sink.replay(context.Background(), "s-nan", payload); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !landed {
		t.Fatal("replay 未调用落地回调")
	}
	if !math.IsNaN(got.CrossCacheRead) {
		t.Fatalf("CrossCacheRead 应还原为 NaN（未知），got %v", got.CrossCacheRead)
	}
	if !math.IsNaN(got.Decision.TurnCostOld) || !math.IsNaN(got.Decision.RechargeCost) ||
		!math.IsNaN(got.Decision.PaybackTurns) || !math.IsNaN(got.Decision.PaybackLimit) {
		t.Fatalf("未知字段应还原为 NaN，got %+v", got.Decision)
	}
	if got.Decision.TurnCostNew != 1.5 {
		t.Fatalf("有限值应原样保留，got %v", got.Decision.TurnCostNew)
	}
	if got.SessionID != "s-nan" || got.Turn != 7 || !got.Decision.Switch {
		t.Fatalf("普通字段应保留，got %+v", got)
	}
}
