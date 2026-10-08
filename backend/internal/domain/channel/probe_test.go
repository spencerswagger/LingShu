package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/pkg/crypto"
)

// newProbeTestManager 返回一个可执行 doProbe 的 Manager（store 为空，仅用于探测链路）。
func newProbeTestManager() *Manager {
	return NewManager(nil, nil, []byte("0123456789abcdef"), &http.Client{Timeout: 2 * time.Second}, nil, time.Now)
}

func TestDoProbe_MarksProbeFeedback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("probe should POST /chat/completions, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	m := newProbeTestManager()
	fb, usage, errMsg := m.doProbe(context.Background(), srv.URL+"/chat/completions", "key", "m1", 2*time.Second)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if !fb.IsSuccess {
		t.Fatalf("expected success feedback, got %+v", fb)
	}
	if !fb.HasProbe || !fb.IsProbe() {
		t.Fatalf("expected probe feedback HasProbe=true, got %+v", fb)
	}
	if usage.TotalTokens != 2 {
		t.Fatalf("expected usage parsed, got %+v", usage)
	}
}

func TestDoProbe_MarksTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newProbeTestManager()
	fb, _, errMsg := m.doProbe(context.Background(), srv.URL+"/chat/completions", "key", "m1", 30*time.Millisecond)
	if errMsg == "" {
		t.Fatalf("expected timeout error, got nil, fb=%+v", fb)
	}
	if !fb.HasProbe {
		t.Fatalf("expected HasProbe on timeout, got %+v", fb)
	}
	if !fb.IsTimeout {
		t.Fatalf("expected IsTimeout=true on probe timeout, got IsTimeout=%v err=%v", fb.IsTimeout, errMsg)
	}
}

// TestProbeChain_DrainToHealthy 验证缺陷 1 的核心链路：
// doProbe 构造的探测反馈带 HasProbe，喂入状态机后触发连续成功 ≥ recoveryThreshold → HEALTHY，
// 且探测成功不会计入调用窗口（不污染错误率）。
func TestProbeChain_DrainToHealthy(t *testing.T) {
	m := NewMachine(DefaultMachineConfig())
	m.SetClock(func() time.Time { return time.Now() })

	// 真实调用失败进入 DRAIN（需达到 MinSamples=10，防止少样本误判）。
	for i := 0; i < 15; i++ {
		m.Feed(Feedback{IsSuccess: false, Now: time.Now().Add(time.Duration(i) * 200 * time.Millisecond)})
	}
	if m.State() != StateDrain {
		t.Fatalf("setup: expected DRAIN_ONLY, got %s", m.State())
	}

	// 用带 HasProbe 的成功反馈（等同 doProbe 返回）驱动恢复。
	for i := 0; i < DefaultMachineConfig().ProbeRecoveryThreshold; i++ {
		m.Feed(Feedback{IsSuccess: true, HasProbe: true, Now: time.Now().Add(time.Duration(i) * time.Second)})
	}
	if got := m.State(); got != StateNormal {
		t.Fatalf("expected HEALTHY via probe chain, got %s (reason=%s)", got, m.LastReason())
	}
}

// insertProbeLogSQL 是 InsertProbeLog 的落库 SQL（密钥维度 channel_key_id，与 0001_init.sql 对齐）。
const insertProbeLogSQL = `INSERT INTO probe_logs(id, channel_key_id, model_id, level, target, ok, error, input_tokens, output_tokens, cached_tokens, total_tokens, duration_ms)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`

// newKeyProbeTestManager 装载「1 渠道 + 若干内部模型 + cmStore」，返回 Manager 与 sqlmock。
// keyStore 故意留空（nil）：探测测试不应触碰密钥持久化；探测记录经 cmStore.InsertProbeLog 落 probe_logs。
func newKeyProbeTestManager(t *testing.T, baseURL string, hp HealthProbeConfig, models []ChannelModel) (*Manager, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }

	mgr := NewManager(nil, NewChannelModelStore(db), testSM4Key, &http.Client{Timeout: 2 * time.Second}, nil, nowFn)
	if hp.TimeoutMS == 0 {
		hp.TimeoutMS = 1000
	}
	ch := &Channel{
		ID: 1, Name: "c1", Protocol: ProtocolOpenAICompat, BaseURL: baseURL,
		State: StateNormal, RateLimit: RateLimitConfig{MaxConcurrent: 8},
		HealthProbe: hp,
		Reliability: ReliabilityConfig{WindowSeconds: 60, MinSamples: 10},
		CreatedAt:   now, UpdatedAt: now,
	}
	mgr.rt[1] = mgr.buildRuntime(ch)
	mgr.mrt[1] = map[int64]*ModelRuntime{}
	for i := range models {
		cm := models[i]
		cm.ChannelID = 1
		mgr.mrt[1][cm.ID] = mgr.buildModelRuntime(&cm, mgr.rt[1])
	}
	return mgr, mock
}

// addKeyRuntime 向 Manager 预置一个密钥运行时（测试专用，绕过 keyStore 装载）。
func addKeyRuntime(t *testing.T, mgr *Manager, keyID int64, credential string, state State) {
	t.Helper()
	enc := ""
	if credential != "" {
		var err error
		enc, err = crypto.SM4Encrypt(testSM4Key, []byte(credential))
		if err != nil {
			t.Fatalf("encrypt key %d: %v", keyID, err)
		}
	}
	k := ChannelKey{ID: keyID, ChannelID: 1, Name: fmt.Sprintf("key-%d", keyID), CredentialEnc: enc, State: state}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.keys[keyID] = mgr.buildKeyRuntime(&mgr.rt[1].Channel, k)
	if mgr.keyOfCh[1] == nil {
		mgr.keyOfCh[1] = map[int64]bool{}
	}
	mgr.keyOfCh[1][keyID] = true
}

// TestKeyProbeOnce_DualKeys_RecordsPerKey 双密钥各自探测：probe_logs 分别以正确 channel_key_id 落库且互不影响。
// 模型层全探（无显式 probe_model）→ level='model'。
func TestKeyProbeOnce_DualKeys_RecordsPerKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer " + testKeyACred:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	hp := HealthProbeConfig{Interval: "0 * * * * *", TimeoutMS: 1000, FailThreshold: 1, RecoveryThreshold: 2}
	mgr, mock := newKeyProbeTestManager(t, srv.URL, hp,
		[]ChannelModel{{ID: 21, InternalModelID: "m-default", ExternalModelID: 1, State: StateNormal}})
	addKeyRuntime(t, mgr, 11, testKeyACred, StateNormal)
	addKeyRuntime(t, mgr, 12, testKeyBCred, StateNormal)

	target := srv.URL + "/chat/completions"
	mock.ExpectExec(regexp.QuoteMeta(insertProbeLogSQL)).
		WithArgs(sqlmock.AnyArg(), int64(11), "m-default", "model", target, true, "", 3, 4, 0, 7, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(insertProbeLogSQL)).
		WithArgs(sqlmock.AnyArg(), int64(12), "m-default", "model", target, false, "probe http status: 500", 0, 0, 0, 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(2, 1))

	mgr.keyProbeOnce(context.Background(), 11)
	mgr.keyProbeOnce(context.Background(), 12)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("probe logs must be written per channel_key_id: %v", err)
	}
}

// TestKeyProbeOnce_EmptyCredentialFailsOnlyThatKey 空凭据密钥探测失败并记 key 维度失败，另一密钥不受影响。
func TestKeyProbeOnce_EmptyCredentialFailsOnlyThatKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	}))
	defer srv.Close()

	mgr, mock := newKeyProbeTestManager(t, srv.URL, HealthProbeConfig{FailThreshold: 1, TimeoutMS: 1000},
		[]ChannelModel{{ID: 21, InternalModelID: "m1", ExternalModelID: 1, State: StateNormal}})
	addKeyRuntime(t, mgr, 11, testKeyACred, StateNormal)
	addKeyRuntime(t, mgr, 12, "", StateNormal)

	mock.ExpectExec(regexp.QuoteMeta(insertProbeLogSQL)).
		WithArgs(sqlmock.AnyArg(), int64(12), "", "key", "", false, "密钥凭据为空或解密失败，无法探测", 0, 0, 0, 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mgr.keyProbeOnce(context.Background(), 12)

	kr12, _ := mgr.GetKeyRuntime(12)
	kr11, _ := mgr.GetKeyRuntime(11)
	if kr12.CredentialPlain != "" {
		t.Fatalf("empty credential key must keep empty plain, got %q", kr12.CredentialPlain)
	}
	if kr11.Machine.State() != StateNormal {
		t.Fatalf("sibling key must stay NORMAL, got %s", kr11.Machine.State())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("empty credential probe log not written for key: %v", err)
	}
}

// TestKeyProbeOnce_AuthFailureDisablesOnlyThatKey 401 → 仅该密钥状态机 DISABLED，兄弟密钥不变。
func TestKeyProbeOnce_AuthFailureDisablesOnlyThatKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+testKeyACred {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	}))
	defer srv.Close()

	mgr, mock := newKeyProbeTestManager(t, srv.URL, HealthProbeConfig{FailThreshold: 1, RecoveryThreshold: 2, TimeoutMS: 1000},
		[]ChannelModel{{ID: 21, InternalModelID: "m1", ExternalModelID: 1, State: StateNormal}})
	addKeyRuntime(t, mgr, 11, testKeyACred, StateNormal)
	addKeyRuntime(t, mgr, 12, testKeyBCred, StateNormal)

	mock.ExpectExec(regexp.QuoteMeta(insertProbeLogSQL)).
		WithArgs(sqlmock.AnyArg(), int64(11), "m1", "model", srv.URL+"/chat/completions", false, sqlmock.AnyArg(), 0, 0, 0, 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mgr.keyProbeOnce(context.Background(), 11)

	kr11, _ := mgr.GetKeyRuntime(11)
	kr12, _ := mgr.GetKeyRuntime(12)
	if kr11.Machine.State() != StateDisabled {
		t.Fatalf("401 key want DISABLED, got %s", kr11.Machine.State())
	}
	if kr11.Machine.LastReason() != reasonAuthFailure {
		t.Fatalf("401 key reason want %q, got %q", reasonAuthFailure, kr11.Machine.LastReason())
	}
	if kr12.Machine.State() != StateNormal {
		t.Fatalf("sibling key must stay NORMAL after 401, got %s", kr12.Machine.State())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("auth failure probe log not written: %v", err)
	}
}

// TestKeyProbeOnce_AllModelsDisabledFailsNoFallback 全禁用模型 → 探测失败，且不发起任何 HTTP（不回退 GET /models）。
func TestKeyProbeOnce_AllModelsDisabledFailsNoFallback(t *testing.T) {
	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mgr, mock := newKeyProbeTestManager(t, srv.URL, HealthProbeConfig{FailThreshold: 1, TimeoutMS: 1000},
		[]ChannelModel{{ID: 21, InternalModelID: "m-off", ExternalModelID: 1, State: StateDisabled}})
	addKeyRuntime(t, mgr, 11, testKeyACred, StateNormal)

	mock.ExpectExec(regexp.QuoteMeta(insertProbeLogSQL)).
		WithArgs(sqlmock.AnyArg(), int64(11), "", "key", "", false, "无可用探测模型（全部禁用或无模型）", 0, 0, 0, 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mgr.keyProbeOnce(context.Background(), 11)

	if n := atomic.LoadInt32(&called); n != 0 {
		t.Fatalf("target-less probe must not fall back to HTTP, got %d calls", n)
	}
	kr, _ := mgr.GetKeyRuntime(11)
	if kr.Machine.State() != StateDrain {
		t.Fatalf("target-less probe want DRAIN, got %s", kr.Machine.State())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("failure probe log not written: %v", err)
	}
}

// TestKeyProbeOnce_ProbeModelTakesPriority 显式 probe_model 优先，且记为模型级探测 level='model'。
func TestKeyProbeOnce_ProbeModelTakesPriority(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	}))
	defer srv.Close()

	hp := HealthProbeConfig{ProbeModel: "explicit-m", FailThreshold: 1, TimeoutMS: 1000}
	mgr, mock := newKeyProbeTestManager(t, srv.URL, hp,
		[]ChannelModel{{ID: 21, InternalModelID: "m-default", ExternalModelID: 1, State: StateNormal}})
	addKeyRuntime(t, mgr, 11, testKeyACred, StateNormal)

	mock.ExpectExec(regexp.QuoteMeta(insertProbeLogSQL)).
		WithArgs(sqlmock.AnyArg(), int64(11), "explicit-m", "key", srv.URL+"/chat/completions", true, "", 0, 0, 0, 1, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mgr.keyProbeOnce(context.Background(), 11)

	if gotModel != "explicit-m" {
		t.Fatalf("probe must use explicit probe_model, got %q", gotModel)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("explicit model probe log not written with level=model: %v", err)
	}
}

// TestManager_Start_NoKeyStoreDoesNotPanic keyStore 未装配（生产暂未注入）时 Start 空转且不 panic。
func TestManager_Start_NoKeyStoreDoesNotPanic(t *testing.T) {
	mgr := NewManager(nil, nil, testSM4Key, &http.Client{Timeout: time.Second}, nil, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx) // 无密钥运行时：仅告警日志，不启动 goroutine
	cancel()
	mgr.Stop()
}

// TestManager_Start_LaunchesPerKeyAndStops Start 为每个密钥启动独立探测循环，取消 ctx 后可全部退出。
func TestManager_Start_LaunchesPerKeyAndStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hp := HealthProbeConfig{Interval: "* * * * * *", DrainIntervalSeconds: 1, TimeoutMS: 1000, FailThreshold: 1, RecoveryThreshold: 2}
	mgr, _ := newKeyProbeTestManager(t, srv.URL, hp,
		[]ChannelModel{{ID: 21, InternalModelID: "m1", ExternalModelID: 1, State: StateNormal}})
	addKeyRuntime(t, mgr, 11, testKeyACred, StateNormal)
	addKeyRuntime(t, mgr, 12, testKeyBCred, StateNormal)

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	cancel()
	mgr.Stop() // 取消后全部探测 goroutine 应退出，不阻塞
}

// TestKeyProbeLoop_ExitsWhenRuntimeRemoved 探测循环每轮复查运行时；密钥被 RemoveKey 摘除后退出。
func TestKeyProbeLoop_ExitsWhenRuntimeRemoved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hp := HealthProbeConfig{Interval: "* * * * * *", DrainIntervalSeconds: 1, TimeoutMS: 1000, FailThreshold: 1, RecoveryThreshold: 2}
	mgr, _ := newKeyProbeTestManager(t, srv.URL, hp,
		[]ChannelModel{{ID: 21, InternalModelID: "m1", ExternalModelID: 1, State: StateNormal}})
	addKeyRuntime(t, mgr, 11, testKeyACred, StateNormal)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		mgr.keyProbeLoop(ctx, 11)
		close(done)
	}()
	mgr.RemoveKey(11)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("keyProbeLoop must exit after its runtime is removed")
	}
}

// TestKeyProbeOnceUnlessDisabled_DisabledKeySkipsProbe DISABLED 态守卫不发真实探测：
// 不产生 HTTP、不写 probe_logs、不产生记账；手动恢复 NORMAL 后下一轮正常探测并落库。
func TestKeyProbeOnceUnlessDisabled_DisabledKeySkipsProbe(t *testing.T) {
	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	}))
	defer srv.Close()

	hp := HealthProbeConfig{FailThreshold: 1, RecoveryThreshold: 2, TimeoutMS: 1000}
	mgr, mock := newKeyProbeTestManager(t, srv.URL, hp,
		[]ChannelModel{{ID: 21, InternalModelID: "m1", ExternalModelID: 1, State: StateNormal}})
	addKeyRuntime(t, mgr, 11, testKeyACred, StateDisabled)

	// DISABLED：守卫直接跳过，无 HTTP、无 probe_logs（无相应 mock expectation，ExpectationsWereMet 通过）。
	mgr.keyProbeOnceUnlessDisabled(context.Background(), 11)
	if n := atomic.LoadInt32(&called); n != 0 {
		t.Fatalf("DISABLED 态不应发起真实探测，got %d HTTP calls", n)
	}
	if kr, _ := mgr.GetKeyRuntime(11); kr.Machine.State() != StateDisabled {
		t.Fatalf("DISABLED 态守卫不应改变状态，got %s", kr.Machine.State())
	}

	// 手动恢复 NORMAL：守卫放行，正常探测并落 probe_logs。
	mock.ExpectExec(regexp.QuoteMeta(insertProbeLogSQL)).
		WithArgs(sqlmock.AnyArg(), int64(11), "m1", "model", srv.URL+"/chat/completions", true, "", 0, 0, 0, 1, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := mgr.ManualSetKeyState(11, "recover"); err != nil {
		t.Fatalf("manual recover key: %v", err)
	}
	mgr.keyProbeOnceUnlessDisabled(context.Background(), 11)
	if n := atomic.LoadInt32(&called); n != 1 {
		t.Fatalf("恢复 NORMAL 后应正常探测一次，got %d HTTP calls", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("恢复后探测记录应写入：%v", err)
	}
}
