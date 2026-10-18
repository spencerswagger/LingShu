package router

import (
	"context"
	"errors"
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

// TestCostEvent_AsyncSubmitsAndIdempotentByTurn 锁定：启用异步时提交成功；
// 同一 IdempotencyKey（session_id:轮次）重复提交只落一次，不同轮次各落一次。
func TestCostEvent_AsyncSubmitsAndIdempotentByTurn(t *testing.T) {
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
		t.Fatalf("重复提交（同 IdempotencyKey）失败: %v", err)
	}
	if err := sink.Submit(SessionCostEvent{SessionID: "s1", Turn: 2, Decision: SessionCostDecision{Switch: false}}); err != nil {
		t.Fatalf("不同轮次提交失败: %v", err)
	}

	waitAsyncDepthZero(t, w)

	mu.Lock()
	got := append([]SessionCostEvent(nil), landed...)
	mu.Unlock()

	if len(got) != 2 {
		t.Fatalf("期望落库 2 条（turn1 幂等去重 + turn2），got %d: %+v", len(got), got)
	}
	var turn1 int
	for _, e := range got {
		if e.SessionID == "s1" && e.Turn == 1 {
			turn1++
		}
	}
	if turn1 != 1 {
		t.Fatalf("相同 IdempotencyKey 应只落一次，turn1 落库 %d 次", turn1)
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
