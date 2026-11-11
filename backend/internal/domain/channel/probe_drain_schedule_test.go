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
			wantBranch: branchCron, wantWait: 13*time.Hour + 30*time.Minute, // 冻结在 10:30 → 次日 00:00
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
			wantBranch:  branchCron, wantWait: 13*time.Hour + 30*time.Minute,
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

// TestKeyProbeLoop_NormalStateWakeDoesNotProbe 唤醒本身不产生探测：常态（cron 分支）下反复唤醒
// 不得把探测频率抬到秒级（反向要求：常态渠道仍按 cron，不能被唤醒机制变成高频探测）。
func TestKeyProbeLoop_NormalStateWakeDoesNotProbe(t *testing.T) {
	c := &probeCounter{}
	srv := httptest.NewServer(c.handler(true))
	defer srv.Close()

	// 冻结时钟 + 每小时整点 cron：自 10:30 起下次探测在 11:00（30 分钟），秒级窗口内不可能触发。
	frozen := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	hp := HealthProbeConfig{Interval: "0 0 * * * *", DrainIntervalSeconds: 1, TimeoutMS: 1000, FailThreshold: 1, RecoveryThreshold: 1}
	mgr := newDrainScheduleTestManager(t, srv.URL, hp, func() time.Time { return frozen })

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() {
		cancel()
		mgr.Stop()
	}()
	time.Sleep(200 * time.Millisecond)

	// 模拟同渠道其它对象流转 / 配置刷新带来的连续唤醒。
	for i := 0; i < 20; i++ {
		mgr.wakeProbeScheduling()
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if n := c.count(); n != 0 {
		t.Fatalf("常态下仅唤醒不应产生探测，got %d 次（唤醒不得把常态抬成秒级探测）", n)
	}
	wait, branch, ok := mgr.nextKeyProbeWait(11)
	if !ok || branch != branchCron {
		t.Fatalf("常态分支应为 cron，got branch=%q ok=%v", branch, ok)
	}
	if wait <= 0 || wait > 30*time.Minute {
		t.Fatalf("常态等待应指向 11:00 的 cron 边界（≤30min），got %v", wait)
	}
}

// TestNextKeyProbeWait_ProbeModelScope 探测作用域决定「模型排空」是否参与调度。
// 未配 probe_model → 探测目标是模型级（level=model，回喂模型状态机），模型排空必须 15s 代探；
// 配了 probe_model → 只做密钥级定向探测（level=key，不回喂模型状态机），模型排空态既探不到也
// 恢复不了，不能拿它决定等待（否则该渠道全部密钥永久秒级探测、永不回 cron）。
func TestNextKeyProbeWait_ProbeModelScope(t *testing.T) {
	now := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	const daily = "0 0 0 * * *"
	const cronWait = 13*time.Hour + 30*time.Minute // 冻结在 10:30 → 次日 00:00

	tests := []struct {
		name       string
		probeModel string
		keyState   State
		modelState State
		wantBranch string
		wantWait   time.Duration
	}{
		{
			name:     "未配 probe_model + 模型 DRAIN → 按模型排空 15s 代探",
			keyState: StateNormal, modelState: StateDrain,
			wantBranch: branchModelDrain, wantWait: 15 * time.Second,
		},
		{
			name:       "配了 probe_model + 作用域外模型 DRAIN → 动态代探（按排空间隔，不是 cron）",
			probeModel: "m-probe",
			keyState:   StateNormal, modelState: StateDrain,
			wantBranch: branchModelDrain, wantWait: 15 * time.Second,
		},
		{
			name:       "配了 probe_model + 排空的正是该模型 → 按模型排空间隔代探（探测会回喂它）",
			probeModel: "m1",
			keyState:   StateNormal, modelState: StateDrain,
			wantBranch: branchModelDrain, wantWait: 15 * time.Second,
		},
		{
			name:       "配了 probe_model + 密钥 DRAIN → 仍按密钥排空间隔",
			probeModel: "m-probe",
			keyState:   StateDrain, modelState: StateNormal,
			wantBranch: branchKeyDrain, wantWait: 15 * time.Second,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hp := HealthProbeConfig{
				Interval: daily, DrainIntervalSeconds: 15, TimeoutMS: 1000,
				FailThreshold: 1, RecoveryThreshold: 1, ProbeModel: tc.probeModel,
			}
			mgr := newDrainScheduleTestManager(t, "http://up.example.com", hp, func() time.Time { return now })
			mgr.mu.Lock()
			mgr.mrt[1][21].Machine.transition(tc.modelState, now, "")
			mgr.keys[11].Machine.transition(tc.keyState, now, "")
			mgr.mu.Unlock()

			wait, branch, ok := mgr.nextKeyProbeWait(11)
			if !ok {
				t.Fatalf("运行时存在时 ok 必须为 true")
			}
			if branch != tc.wantBranch {
				t.Errorf("branch want %q, got %q", tc.wantBranch, branch)
			}
			if wait != tc.wantWait {
				t.Errorf("wait want %v, got %v", tc.wantWait, wait)
			}
		})
	}
}

// TestKeyProbeOnce_ProbeModelIsKeyLevel 配了 probe_model 时探测记 level=key 且指向该模型；
// 该模型能落到 channel_models 行时同时回喂其状态机——否则定向探测成功也恢复不了排空的目标模型。
func TestKeyProbeOnce_ProbeModelIsKeyLevel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	now := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	hp := HealthProbeConfig{
		Interval: "0 0 * * * *", DrainIntervalSeconds: 15, TimeoutMS: 1000,
		FailThreshold: 1, RecoveryThreshold: 1, ProbeModel: "m1",
	}
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
	if err := mgr.ManualSetModelState(1, 21, StateDrain); err != nil {
		t.Fatalf("manual model drain: %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta(insertProbeLogSQL)).
		WithArgs(sqlmock.AnyArg(), int64(11), "m1", "key", srv.URL+"/chat/completions", true, "", 1, 1, 0, 2, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mgr.keyProbeOnce(context.Background(), 11)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("probe_model 探测应记 level=key 且 ModelID=探测模型：%v", err)
	}
	// 回喂目标模型状态机：RecoveryThreshold=1，一次成功即恢复 NORMAL。
	if mr := mgr.mrt[1][21]; mr.Machine.State() != StateNormal {
		t.Fatalf("probe_model 目标模型应由探测成功恢复 NORMAL，got %s", mr.Machine.State())
	}
}

// TestKeyProbeLoop_ProbeModelTargetDrainRecovers 配了 probe_model 的渠道：被排空的正是该探测目标时，
// 必须按排空间隔探测并由此恢复（修复不能退回「probe_model 渠道下模型排空永远不探」）。
func TestKeyProbeLoop_ProbeModelTargetDrainRecovers(t *testing.T) {
	c := &probeCounter{}
	srv := httptest.NewServer(c.handler(true))
	defer srv.Close()

	frozen := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	hp := HealthProbeConfig{
		Interval: "0 0 * * * *", DrainIntervalSeconds: 1, TimeoutMS: 1000,
		FailThreshold: 1, RecoveryThreshold: 1, ProbeModel: "m1", // m1 = 模型 21
	}
	mgr := newDrainScheduleTestManager(t, srv.URL, hp, func() time.Time { return frozen })

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() {
		cancel()
		mgr.Stop()
	}()
	time.Sleep(200 * time.Millisecond)

	if err := mgr.ManualSetModelState(1, 21, StateDrain); err != nil {
		t.Fatalf("manual model drain: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && c.count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if c.count() == 0 {
		t.Fatalf("排空的是 probe_model 目标模型时，3s 内应有探测（不能退化成永不探）")
	}
	if mr := mgr.mrt[1][21]; mr.Machine.State() != StateNormal {
		t.Fatalf("定向探测成功应让目标模型恢复 NORMAL，got %s", mr.Machine.State())
	}
}

// TestWaitUntilNextKeyProbe_CronResetAfterManualRecover 唤醒后必须按新状态重算截止：
// 密钥从 DRAIN 手动恢复 NORMAL 时，旧的排空截止必须作废（否则会多出一次「排空遗留」的探测，
// 即恢复后没有立刻回到 cron）。删除 waitUntilNextKeyProbe 里的 cron 重置分支，本用例必红。
func TestWaitUntilNextKeyProbe_CronResetAfterManualRecover(t *testing.T) {
	c := &probeCounter{}
	srv := httptest.NewServer(c.handler(true))
	defer srv.Close()

	frozen := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	hp := HealthProbeConfig{Interval: "0 0 * * * *", DrainIntervalSeconds: 1, TimeoutMS: 1000, FailThreshold: 1, RecoveryThreshold: 1}
	mgr := newDrainScheduleTestManager(t, srv.URL, hp, func() time.Time { return frozen })
	// 以 DRAIN 起步：循环首轮就排定一个 1s 的排空截止。
	mgr.mu.Lock()
	mgr.keys[11].Machine.transition(StateDrain, frozen, "")
	mgr.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() {
		cancel()
		mgr.Stop()
	}()
	time.Sleep(200 * time.Millisecond) // 循环已在 1s 截止上等待

	if err := mgr.ManualSetKeyState(11, "normal"); err != nil {
		t.Fatalf("manual recover: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if n := c.count(); n != 0 {
		t.Fatalf("恢复 NORMAL 后不应残留排空探测（cron 截止未重算），got %d 次", n)
	}
	if _, branch, ok := mgr.nextKeyProbeWait(11); !ok || branch != branchCron {
		t.Fatalf("恢复后分支应为 cron，got %q（ok=%v）", branch, ok)
	}
}

// TestKeyProbeLoop_WakesOnRealTrafficDrain 真实调用把模型打到 DRAIN 时也必须唤醒探测循环：
// 删除 feedAndTransit 里的广播，本用例必红（循环会一直睡到 cron 边界）。
func TestKeyProbeLoop_WakesOnRealTrafficDrain(t *testing.T) {
	c := &probeCounter{}
	srv := httptest.NewServer(c.handler(true))
	defer srv.Close()

	frozen := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	hp := HealthProbeConfig{Interval: "0 0 * * * *", DrainIntervalSeconds: 1, TimeoutMS: 1000, FailThreshold: 1, RecoveryThreshold: 1}
	mgr := newDrainScheduleTestManager(t, srv.URL, hp, func() time.Time { return frozen })

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() {
		cancel()
		mgr.Stop()
	}()
	time.Sleep(200 * time.Millisecond)
	if n := c.count(); n != 0 {
		t.Fatalf("常态不应探测，got %d 次", n)
	}

	// 真实调用连续失败达到 MinSamples（10）→ 可靠性窗口把模型（与密钥）打到 DRAIN。
	for i := 0; i < 10; i++ {
		mgr.FeedResult(11, 21, Feedback{IsSuccess: false})
	}
	if mr := mgr.mrt[1][21]; mr.Machine.State() != StateDrain {
		t.Fatalf("真实调用失败应把模型打到 DRAIN，got %s", mr.Machine.State())
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && c.count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if c.count() == 0 {
		t.Fatalf("真实调用打到 DRAIN 后 3s 内应开始按排空间隔探测（真实流量唤醒缺失）")
	}
}

// addModelRuntime 测试辅助：追加一个内部模型运行时（同渠道 1）。
func addModelRuntime(t *testing.T, mgr *Manager, id int64, internal string, st State) {
	t.Helper()
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	cm := ChannelModel{ID: id, ChannelID: 1, InternalModelID: internal, ExternalModelID: id, State: st}
	if mgr.mrt[1] == nil {
		mgr.mrt[1] = map[int64]*ModelRuntime{}
	}
	mgr.mrt[1][id] = mgr.buildModelRuntime(&cm, mgr.rt[1])
}

// TestNextKeyProbeWait_OnlyElectedKeyProbesModelDrain 模型排空只借 1 个密钥代探：
// 被选中的密钥按排空间隔，其余密钥保持 cron（不得把整渠道拖进秒级）。
func TestNextKeyProbeWait_OnlyElectedKeyProbesModelDrain(t *testing.T) {
	now := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	hp := HealthProbeConfig{Interval: "0 0 0 * * *", DrainIntervalSeconds: 15, TimeoutMS: 1000,
		FailThreshold: 1, RecoveryThreshold: 1, ProbeModel: "probe-only"}
	mgr := newDrainScheduleTestManager(t, "http://up.example.com", hp, func() time.Time { return now })
	addKeyRuntime(t, mgr, 12, testKeyBCred, StateNormal)
	if err := mgr.ManualSetModelState(1, 21, StateDrain); err != nil {
		t.Fatalf("manual model drain: %v", err)
	}

	wait11, branch11, _ := mgr.nextKeyProbeWait(11) // 最小 keyID = 代探密钥
	if branch11 != branchModelDrain || wait11 != 15*time.Second {
		t.Fatalf("代探密钥应走模型排空档，got branch=%q wait=%v", branch11, wait11)
	}
	wait12, branch12, _ := mgr.nextKeyProbeWait(12)
	if branch12 != branchCron || wait12 <= time.Hour {
		t.Fatalf("非代探密钥应保持 cron，got branch=%q wait=%v", branch12, wait12)
	}
}

// TestPickKeyProbeTargets_DrainProbeRotationAndCap 多个模型同时排空：每个间隔只代探 1 个模型并轮转，
// 且只有代探密钥带这个额外目标（每渠道每间隔最多 1 次，成本有上限）。
func TestPickKeyProbeTargets_DrainProbeRotationAndCap(t *testing.T) {
	now := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	hp := HealthProbeConfig{Interval: "0 0 0 * * *", DrainIntervalSeconds: 15, TimeoutMS: 1000,
		FailThreshold: 1, RecoveryThreshold: 1, ProbeModel: "probe-only"}
	mgr := newDrainScheduleTestManager(t, "http://up.example.com", hp, func() time.Time { return now })
	addKeyRuntime(t, mgr, 12, testKeyBCred, StateNormal)
	addModelRuntime(t, mgr, 22, "m2", StateNormal)
	for _, id := range []int64{21, 22} {
		if err := mgr.ManualSetModelState(1, id, StateDrain); err != nil {
			t.Fatalf("manual model drain %d: %v", id, err)
		}
	}
	kr11, _ := mgr.GetKeyRuntime(11)
	kr12, _ := mgr.GetKeyRuntime(12)

	drainTargets := func(targets []probeTarget) []probeTarget {
		var out []probeTarget
		for _, tg := range targets {
			if tg.DrainOnly {
				out = append(out, tg)
			}
		}
		return out
	}
	first := drainTargets(mgr.pickKeyProbeTargetsFor(kr11, branchModelDrain))
	second := drainTargets(mgr.pickKeyProbeTargetsFor(kr11, branchModelDrain))
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("每个间隔只能代探 1 个模型，got %d/%d", len(first), len(second))
	}
	if first[0].Model == second[0].Model {
		t.Fatalf("两个模型同时排空应轮转，got 同一目标 %q", first[0].Model)
	}
	if first[0].Level != "model" || first[0].ModelRowID == 0 {
		t.Fatalf("代探应是模型级并带行 ID，got %+v", first[0])
	}
	if extra := drainTargets(mgr.pickKeyProbeTargetsFor(kr12, branchModelDrain)); len(extra) != 0 {
		t.Fatalf("非代探密钥不应获得额外代探目标，got %+v", extra)
	}
}

// TestKeyProbeLoop_OutOfScopeModelDrainRecovers probe_model 渠道里「非探测目标模型」排空后，
// 必须由动态代探在排空间隔内探到它并恢复（probe_model 是偏好，不是白名单）。
func TestKeyProbeLoop_OutOfScopeModelDrainRecovers(t *testing.T) {
	c := &probeCounter{}
	srv := httptest.NewServer(c.handler(true))
	defer srv.Close()

	frozen := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	hp := HealthProbeConfig{Interval: "0 0 * * * *", DrainIntervalSeconds: 1, TimeoutMS: 1000,
		FailThreshold: 1, RecoveryThreshold: 1, ProbeModel: "probe-only"}
	mgr := newDrainScheduleTestManager(t, srv.URL, hp, func() time.Time { return frozen })
	addKeyRuntime(t, mgr, 12, testKeyBCred, StateNormal)

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() {
		cancel()
		mgr.Stop()
	}()
	time.Sleep(200 * time.Millisecond)

	if err := mgr.ManualSetModelState(1, 21, StateDrain); err != nil {
		t.Fatalf("manual model drain: %v", err)
	}
	mr := mgr.mrt[1][21]
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && mr.Machine.State() != StateNormal {
		time.Sleep(20 * time.Millisecond)
	}
	if mr.Machine.State() != StateNormal {
		t.Fatalf("作用域外模型排空后应由代探恢复，got %s", mr.Machine.State())
	}
	// 代探不把密钥拖进 DRAIN。
	for _, id := range []int64{11, 12} {
		if kr, ok := mgr.GetKeyRuntime(id); ok && kr.Machine.State() != StateNormal {
			t.Fatalf("代探不应改变密钥 %d 状态，got %s", id, kr.Machine.State())
		}
	}
}

// TestKeyProbeLoop_DrainProbeBackoffReturnsToCron 同一模型连续代探失败 3 次后退避回 cron：
// 不再每间隔打它，密钥也保持 NORMAL（失败不牵连密钥）。
func TestKeyProbeLoop_DrainProbeBackoffReturnsToCron(t *testing.T) {
	var probes int32
	// 只让「被排空的模型 m1」失败：静态 probe_model 探测成功 → 密钥保持 NORMAL，
	// 这样退避后的调度分支才能干净地落回 cron（否则密钥自身排空会掩盖结论）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&probes, 1)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model == "m1" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"model broken"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	}))
	defer srv.Close()

	frozen := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	hp := HealthProbeConfig{Interval: "0 0 * * * *", DrainIntervalSeconds: 1, TimeoutMS: 1000,
		FailThreshold: 1, RecoveryThreshold: 1, ProbeModel: "probe-only"}
	mgr := newDrainScheduleTestManager(t, srv.URL, hp, func() time.Time { return frozen })

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() {
		cancel()
		mgr.Stop()
	}()
	time.Sleep(200 * time.Millisecond)

	if err := mgr.ManualSetModelState(1, 21, StateDrain); err != nil {
		t.Fatalf("manual model drain: %v", err)
	}
	// 1s 排空间隔：等三次失败（退避阈值）落定。
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		mgr.mu.RLock()
		fails := mgr.mrt[1][21].drainProbeFails
		mgr.mu.RUnlock()
		if fails >= drainProbeBackoffFails {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mgr.mu.RLock()
	fails := mgr.mrt[1][21].drainProbeFails
	mgr.mu.RUnlock()
	if fails < drainProbeBackoffFails {
		t.Fatalf("连续失败应达退避阈值 %d，got %d", drainProbeBackoffFails, fails)
	}
	if _, branch, _ := mgr.nextKeyProbeWait(11); branch != branchCron {
		t.Fatalf("退避后应回 cron，got %q", branch)
	}
	mark := int(atomic.LoadInt32(&probes))
	time.Sleep(1500 * time.Millisecond)
	if extra := int(atomic.LoadInt32(&probes)) - mark; extra != 0 {
		t.Fatalf("退避后 1.5s 内不应再有探测，got %d 次", extra)
	}
	if kr, _ := mgr.GetKeyRuntime(11); kr.Machine.State() != StateNormal {
		t.Fatalf("代探失败不应把密钥打成非 NORMAL，got %s", kr.Machine.State())
	}
}
