package channel

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// newDrainScheduleTestManager 构造带可控时钟的 Manager：cmStore 用 sqlmock（不设期望，
// 落库/事件写入走日志路径），渠道 1 + 一个内部模型 21 + 一个密钥 11。
func newDrainScheduleTestManager(t *testing.T, baseURL string, hp HealthProbeConfig, now func() time.Time) *Manager {
	t.Helper()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mgr := NewManager(nil, NewChannelModelStore(db), testSM4Key, &http.Client{Timeout: 2 * time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)), now)
	ch := &Channel{
		ID: 1, Name: "c1", Protocol: ProtocolOpenAICompat, BaseURL: baseURL,
		RateLimit:   RateLimitConfig{MaxConcurrent: 8},
		HealthProbe: hp,
		Reliability: ReliabilityConfig{WindowSeconds: 60, MinSamples: 10},
	}
	mgr.rt[1] = mgr.buildRuntime(ch)
	mgr.mrt[1] = map[int64]*ModelRuntime{}
	cm := ChannelModel{ID: 21, ChannelID: 1, InternalModelID: "m1", ExternalModelID: 1, State: StateNormal}
	mgr.mrt[1][21] = mgr.buildModelRuntime(&cm, mgr.rt[1])
	addKeyRuntime(t, mgr, 11, testKeyACred, StateNormal)
	return mgr
}

// TestNextKeyProbeWait_ThreeTiers 三档对象（密钥 / 内部模型 / 渠道配置）× 三态（NORMAL/DRAIN/DISABLED）
// 的等待计算：任一档进入排空都必须落到 drain_interval_seconds，只有全部 NORMAL 才回 cron。
func TestNextKeyProbeWait_ThreeTiers(t *testing.T) {
	now := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	// 「每天 00:00」的 cron（默认值）：只剩 cron 分支时等待 > 1h，足以与秒级排空间隔区分。
	const daily = "0 0 0 * * *"

	tests := []struct {
		name        string
		keyState    State
		modelState  State
		modelHP     *HealthProbeConfig // 模型级探测配置（nil=不覆盖）
		modelEnable State              // 实体状态（channel_models.state）
		wantBranch  string
		wantWait    time.Duration
		wantWaitMin time.Duration // 仅 cron 用：下界断言
	}{
		{
			name: "全 NORMAL → cron", keyState: StateNormal, modelState: StateNormal,
			wantBranch: branchCron, wantWaitMin: time.Hour,
		},
		{
			name: "密钥 DRAIN → drain_interval_seconds", keyState: StateDrain, modelState: StateNormal,
			wantBranch: branchKeyDrain, wantWait: 15 * time.Second,
		},
		{
			name: "内部模型 DRAIN（密钥 NORMAL）→ drain_interval_seconds", keyState: StateNormal, modelState: StateDrain,
			wantBranch: branchModelDrain, wantWait: 15 * time.Second,
		},
		{
			name: "密钥与模型同时 DRAIN → 取最小间隔", keyState: StateDrain, modelState: StateDrain,
			modelHP:    &HealthProbeConfig{DrainIntervalSeconds: 3},
			wantBranch: branchModelDrain, wantWait: 3 * time.Second,
		},
		{
			name: "模型级排空间隔覆盖渠道值", keyState: StateNormal, modelState: StateDrain,
			modelHP:    &HealthProbeConfig{DrainIntervalSeconds: 40},
			wantBranch: branchModelDrain, wantWait: 40 * time.Second,
		},
		{
			name: "密钥 DISABLED → 15s 复查（不探测）", keyState: StateDisabled, modelState: StateNormal,
			wantBranch: branchDisabled, wantWait: disabledRecheckSeconds * time.Second,
		},
		{
			name: "密钥 DISABLED 且模型 DRAIN → 仍是禁用复查（禁用不发探测）", keyState: StateDisabled, modelState: StateDrain,
			wantBranch: branchDisabled, wantWait: disabledRecheckSeconds * time.Second,
		},
		{
			name: "模型实体 DISABLED（即便状态机残留 DRAIN）→ 不代探，回 cron", keyState: StateNormal, modelState: StateDrain,
			modelEnable: StateDisabled,
			wantBranch:  branchCron, wantWaitMin: time.Hour,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hp := HealthProbeConfig{Interval: daily, DrainIntervalSeconds: 15, TimeoutMS: 1000, FailThreshold: 1, RecoveryThreshold: 1}
			mgr := newDrainScheduleTestManager(t, "http://up.example.com", hp, func() time.Time { return now })
			if tc.modelHP != nil {
				mgr.mu.Lock()
				mr := mgr.mrt[1][21]
				mr.Model.HealthProbe = *tc.modelHP
				mgr.mu.Unlock()
			}
			mgr.mu.Lock()
			if tc.modelEnable != "" {
				mgr.mrt[1][21].Model.State = tc.modelEnable
			}
			mgr.mrt[1][21].Machine.transition(tc.modelState, now, "")
			mgr.keys[11].Machine.transition(tc.keyState, now, "")
			mgr.mu.Unlock()

			wait, branch, ok := mgr.nextKeyProbeWait(11)
			if !ok {
				t.Fatalf("runtime 存在时 ok 必须为 true")
			}
			if branch != tc.wantBranch {
				t.Errorf("branch want %q, got %q", tc.wantBranch, branch)
			}
			if tc.wantWait > 0 && wait != tc.wantWait {
				t.Errorf("wait want %v, got %v", tc.wantWait, wait)
			}
			if tc.wantWaitMin > 0 && wait < tc.wantWaitMin {
				t.Errorf("cron 等待应 > %v，got %v", tc.wantWaitMin, wait)
			}
		})
	}

	// 运行时不存在：ok=false（调用方据此退出循环）。
	mgr := newDrainScheduleTestManager(t, "http://up.example.com",
		HealthProbeConfig{Interval: daily, DrainIntervalSeconds: 15, TimeoutMS: 1000, FailThreshold: 1, RecoveryThreshold: 1},
		func() time.Time { return now })
	if _, _, ok := mgr.nextKeyProbeWait(999); ok {
		t.Fatalf("缺失运行时必须返回 ok=false")
	}
}

// probeCounter 统计上游探测命中数。
type probeCounter struct{ n atomic.Int32 }

func (p *probeCounter) handler(ok bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.n.Add(1)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream down"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
}

func (p *probeCounter) count() int { return int(p.n.Load()) }

// TestKeyProbeLoop_WakesOnManualDrain 排空发生在循环按 cron 长睡期间也必须被唤醒：
// 密钥从 NORMAL 手动排空后，首个探测应在一个 drain 周期内出现（历史上要等下一次 cron）。
func TestKeyProbeLoop_WakesOnManualDrain(t *testing.T) {
	c := &probeCounter{}
	srv := httptest.NewServer(c.handler(false))
	defer srv.Close()

	// 冻结时钟：cron 等待恒为「距 00:00 的时长」，不会因为测试正好跨真实整点而误触发；
	// 排空分支的 1s 间隔与时钟无关，仍按真实时间生效。
	frozen := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	hp := HealthProbeConfig{Interval: "0 0 0 * * *", DrainIntervalSeconds: 1, TimeoutMS: 1000, FailThreshold: 1, RecoveryThreshold: 1}
	mgr := newDrainScheduleTestManager(t, srv.URL, hp, func() time.Time { return frozen })

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() {
		cancel()
		mgr.Stop()
	}()

	// 先让循环进入 cron 长睡，确认此刻不会有探测。
	time.Sleep(300 * time.Millisecond)
	if n := c.count(); n != 0 {
		t.Fatalf("cron 长睡期间不应探测，got %d", n)
	}

	if err := mgr.ManualSetKeyState(11, "drain"); err != nil {
		t.Fatalf("manual drain: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && c.count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := c.count(); n == 0 {
		t.Fatalf("排空后 3s 内 0 次探测（唤醒路径未生效）")
	}
	if kr, _ := mgr.GetKeyRuntime(11); kr.Machine.State() != StateDrain {
		t.Fatalf("上游持续失败时密钥应保持 DRAIN，got %s", kr.Machine.State())
	}
}

// TestKeyProbeLoop_ModelDrainRecoversThenBackToCron 模型排空（密钥保持 NORMAL）：
// 探测成功达阈值后模型退出排空，且退出后不再按秒级探测（回到 cron）。
func TestKeyProbeLoop_ModelDrainRecoversThenBackToCron(t *testing.T) {
	c := &probeCounter{}
	srv := httptest.NewServer(c.handler(true))
	defer srv.Close()

	frozen := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	hp := HealthProbeConfig{Interval: "0 0 0 * * *", DrainIntervalSeconds: 1, TimeoutMS: 1000, FailThreshold: 1, RecoveryThreshold: 1}
	mgr := newDrainScheduleTestManager(t, srv.URL, hp, func() time.Time { return frozen })

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() {
		cancel()
		mgr.Stop()
	}()
	time.Sleep(300 * time.Millisecond)

	if err := mgr.ManualSetModelState(1, 21, StateDrain); err != nil {
		t.Fatalf("manual model drain: %v", err)
	}
	mr := mgr.mrt[1][21]
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && mr.Machine.State() != StateNormal {
		time.Sleep(20 * time.Millisecond)
	}
	if mr.Machine.State() != StateNormal {
		t.Fatalf("模型排空后 3s 内未通过探测恢复，got %s", mr.Machine.State())
	}
	if kr, _ := mgr.GetKeyRuntime(11); kr.Machine.State() != StateNormal {
		t.Fatalf("模型恢复不应把密钥打成非 NORMAL，got %s", kr.Machine.State())
	}

	// 退出排空后应回到 cron（年内不触发）：静默期内不得再有探测。
	mark := c.count()
	time.Sleep(1500 * time.Millisecond)
	if extra := c.count() - mark; extra != 0 {
		t.Fatalf("退出排空后仍按秒级探测（%d 次/1.5s），应回到 cron", extra)
	}
}

// TestDrainProbe_FailureReasonRecorded 排空态探测失败必须把原因（含上游状态码）写进 probe_logs，
// 运营能在探活历史里看到，而不是只有「失败」两字。
func TestDrainProbe_FailureReasonRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream down","type":"server_error"}}`))
	}))
	defer srv.Close()

	hp := HealthProbeConfig{Interval: "0 0 0 * * *", DrainIntervalSeconds: 15, TimeoutMS: 1000, FailThreshold: 1, RecoveryThreshold: 1}
	now := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mgr := NewManager(nil, NewChannelModelStore(db), testSM4Key, &http.Client{Timeout: 2 * time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return now })
	ch := &Channel{
		ID: 1, Name: "c1", Protocol: ProtocolOpenAICompat, BaseURL: srv.URL,
		RateLimit:   RateLimitConfig{MaxConcurrent: 8},
		HealthProbe: hp,
		Reliability: ReliabilityConfig{WindowSeconds: 60, MinSamples: 10},
	}
	mgr.rt[1] = mgr.buildRuntime(ch)
	mgr.mrt[1] = map[int64]*ModelRuntime{}
	cm := ChannelModel{ID: 21, ChannelID: 1, InternalModelID: "m1", ExternalModelID: 1, State: StateNormal}
	mgr.mrt[1][21] = mgr.buildModelRuntime(&cm, mgr.rt[1])
	addKeyRuntime(t, mgr, 11, testKeyACred, StateNormal)
	if err := mgr.ManualSetKeyState(11, "drain"); err != nil {
		t.Fatalf("manual drain: %v", err)
	}

	target := srv.URL + "/chat/completions"
	mock.ExpectExec(regexp.QuoteMeta(insertProbeLogSQL)).
		WithArgs(sqlmock.AnyArg(), int64(11), "m1", "model", target, false, "probe http status: 500", 0, 0, 0, 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mgr.keyProbeOnce(context.Background(), 11)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("排空探测失败原因未落 probe_logs：%v", err)
	}
	// 失败原因同时留在运行时视图（服务状态/密钥列表展示用）。
	kr, _ := mgr.GetKeyRuntime(11)
	if kr.Key.LastErr != "probe http status: 500" {
		t.Fatalf("LastErr want 含状态码的失败原因, got %q", kr.Key.LastErr)
	}
	if kr.Machine.State() != StateDrain {
		t.Fatalf("持续失败应保持 DRAIN，got %s", kr.Machine.State())
	}
	_ = json.Valid
}

// TestProbeWaker_WakesAllSubscribers 唤醒是广播语义：一次 notify 让所有订阅者都醒（不是只醒一个），
// 且醒后重订阅拿到的是新通道（不会立刻又醒，避免空转）。
func TestProbeWaker_WakesAllSubscribers(t *testing.T) {
	w := newProbeWaker()
	subs := make([]<-chan struct{}, 5)
	for i := range subs {
		subs[i] = w.subscribe()
	}
	w.notify()
	for i, ch := range subs {
		select {
		case <-ch:
		default:
			t.Fatalf("订阅者 %d 未被唤醒（唤醒必须是广播）", i)
		}
	}
	next := w.subscribe()
	select {
	case <-next:
		t.Fatalf("notify 之后的新订阅不应立刻再次触发（否则探测循环会空转）")
	default:
	}
}
