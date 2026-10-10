package channel

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/pkg/crypto"
)

const (
	testKeyACred = "sk-live-abcdef123456" // 尾号 123456
	testKeyBCred = "sk-proj-wxyz543210"   // 尾号 543210
)

// newManagerWithTwoKeys 用 sqlmock 装载「1 渠道 + 2 密钥」，返回 Manager 与 mock。
func newManagerWithTwoKeys(t *testing.T) (*Manager, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }

	ch := &Channel{
		ID: 1, Name: "c1", Protocol: ProtocolOpenAICompat,
		BaseURL: "https://api.example.com", State: StateNormal,
		RateLimit:   RateLimitConfig{MaxConcurrent: 8},
		HealthProbe: HealthProbeConfig{FailThreshold: 1, RecoveryThreshold: 2},
		Reliability: ReliabilityConfig{WindowSeconds: 60, MinSamples: 10},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE deleted_at IS NULL ORDER BY priority DESC, id ASC`)).
		WillReturnRows(channelRow(ch))

	encA, err := crypto.SM4Encrypt(testSM4Key, []byte(testKeyACred))
	if err != nil {
		t.Fatalf("encrypt key-a: %v", err)
	}
	encB, err := crypto.SM4Encrypt(testSM4Key, []byte(testKeyBCred))
	if err != nil {
		t.Fatalf("encrypt key-b: %v", err)
	}
	keyRows := sqlmock.NewRows([]string{"id", "channel_id", "name", "credential_enc", "state", "last_err", "deleted_at", "created_at", "updated_at"}).
		AddRow(int64(11), int64(1), "key-a", encA, string(StateNormal), "", nil, now, now).
		AddRow(int64(12), int64(1), "key-b", encB, string(StateNormal), "", nil, now, now)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE deleted_at IS NULL ORDER BY channel_id, id`)).
		WithArgs().WillReturnRows(keyRows)

	mgr := NewManager(NewStore(db), nil, testSM4Key, nil, nil, nowFn, NewKeyStore(db))
	if err := mgr.SyncFromDB(context.Background()); err != nil {
		t.Fatalf("SyncFromDB: %v", err)
	}
	return mgr, mock
}

func TestManager_SyncFromDB_TwoKeysPlainCredentials(t *testing.T) {
	mgr, mock := newManagerWithTwoKeys(t)

	krA, ok := mgr.GetKeyRuntime(11)
	if !ok {
		t.Fatalf("key 11 runtime missing")
	}
	if krA.CredentialPlain != testKeyACred {
		t.Fatalf("key-a plain want %q got %q", testKeyACred, krA.CredentialPlain)
	}
	if krA.Machine == nil || krA.Limiter == nil {
		t.Fatalf("key-a must own Machine/Limiter, got %+v", &krA.Machine)
	}
	if krA.Machine.State() != StateNormal || krA.Key.State != StateNormal {
		t.Fatalf("key-a initial state want NORMAL, got %s", krA.Machine.State())
	}
	if krA.Channel == nil || krA.Channel.ID != 1 {
		t.Fatalf("key-a channel ref want channel 1, got %+v", krA.Channel)
	}

	krB, ok := mgr.GetKeyRuntime(12)
	if !ok {
		t.Fatalf("key 12 runtime missing")
	}
	if krB.CredentialPlain != testKeyBCred {
		t.Fatalf("key-b plain want %q got %q", testKeyBCred, krB.CredentialPlain)
	}

	// 渠道运行时 Keys 视图与 keys map 同一指针。
	rt, ok := mgr.GetRuntime(1)
	if !ok {
		t.Fatalf("channel 1 runtime missing")
	}
	if len(rt.Keys) != 2 {
		t.Fatalf("channel Keys view want 2, got %d", len(rt.Keys))
	}
	for _, view := range rt.Keys {
		kr, ok := mgr.GetKeyRuntime(view.Key.ID)
		if !ok || kr != view {
			t.Fatalf("channel Keys view must reference same KeyRuntime pointer")
		}
	}
	if len(mgr.keys) != 2 || len(mgr.keyOfCh[1]) != 2 {
		t.Fatalf("keys map/keyOfCh want 2 entries, got %d/%d", len(mgr.keys), len(mgr.keyOfCh[1]))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// countingRoundTripper 计数探测 HTTP 请求，模拟上游成功响应。
type countingRoundTripper struct {
	n int64
}

func (c *countingRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	atomic.AddInt64(&c.n, 1)
	return &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body: io.NopCloser(strings.NewReader(
			`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)),
	}, nil
}

// TestManager_StartMarksAllKeysProbeRunning 启动时为全部已装载密钥登记探测循环。
func TestManager_StartMarksAllKeysProbeRunning(t *testing.T) {
	mgr, mock := newManagerWithTwoKeys(t)
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() { cancel(); mgr.Stop() }()

	if !mgr.probeRunning[11] || !mgr.probeRunning[12] {
		t.Fatalf("Start 后全部密钥应登记探测循环，probeRunning=%v", mgr.probeRunning)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestManager_UpsertKey_StartsWithProbeLoop 运行期新建密钥（UpsertKey）也应启动探测循环：
// 用户在线新增密钥并配置每分钟探测后，该密钥应能立即产生探测，无需重启服务。
func TestManager_UpsertKey_StartsWithProbeLoop(t *testing.T) {
	rt := &countingRoundTripper{}
	nowFn := func() time.Time { return time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC) }
	mgr := NewManager(nil, nil, testSM4Key, &http.Client{Transport: rt},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nowFn)
	mgr.Upsert(&Channel{
		ID: 1, Name: "c1", Protocol: ProtocolOpenAICompat,
		BaseURL: "http://probe.local/v1", State: StateNormal,
		HealthProbe: HealthProbeConfig{Interval: "* * * * * *", TimeoutMS: 1000,
			FailThreshold: 1, RecoveryThreshold: 2},
	})
	mgr.UpsertModel(&ChannelModel{ID: 1, ChannelID: 1, InternalModelID: "m1", State: StateNormal})

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() { cancel(); mgr.Stop() }()

	enc, err := crypto.SM4Encrypt(testSM4Key, []byte("sk-new-probe-123456"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	mgr.UpsertKey(ChannelKey{ID: 21, ChannelID: 1, Name: "k-new", CredentialEnc: enc})

	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) && atomic.LoadInt64(&rt.n) == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if atomic.LoadInt64(&rt.n) == 0 {
		t.Fatalf("运行期 UpsertKey 新建密钥后探测循环未触发任何真实探测")
	}
}

// TestManager_KeyProbeOnce_ProbesAllNormalModels 每轮全探：一轮探测应对渠道全部非禁用内部模型各发起一次
// 真实探测 HTTP（原 pickKeyProbeModel 随机单挑一个模型，已改为多目标轮询）。
func TestManager_KeyProbeOnce_ProbesAllNormalModels(t *testing.T) {
	rt := &countingRoundTripper{}
	nowFn := func() time.Time { return time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC) }
	mgr := NewManager(nil, nil, testSM4Key, &http.Client{Transport: rt},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nowFn)
	mgr.Upsert(&Channel{
		ID: 1, Name: "c1", Protocol: ProtocolOpenAICompat,
		BaseURL: "http://probe.local/v1", State: StateNormal,
		HealthProbe: HealthProbeConfig{Interval: "* * * * * *", TimeoutMS: 1000,
			FailThreshold: 1, RecoveryThreshold: 2},
	})
	mgr.UpsertModel(&ChannelModel{ID: 2, ChannelID: 1, InternalModelID: "m2", State: StateNormal})
	mgr.UpsertModel(&ChannelModel{ID: 1, ChannelID: 1, InternalModelID: "m1", State: StateNormal})
	mgr.UpsertModel(&ChannelModel{ID: 3, ChannelID: 1, InternalModelID: "m-off", State: StateDisabled})

	enc, err := crypto.SM4Encrypt(testSM4Key, []byte("sk-probe-all-123456"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	mgr.UpsertKey(ChannelKey{ID: 21, ChannelID: 1, Name: "k-all", CredentialEnc: enc})

	mgr.keyProbeOnce(context.Background(), 21)
	if got := atomic.LoadInt64(&rt.n); got != 2 {
		t.Fatalf("每轮全探应对非禁用模型各发 1 次 HTTP，want 2, got %d", got)
	}
}

// TestManager_PickKeyProbeTargets_MultiModelSorted 目标选型：未显式配置 probe_model 时返回全部非禁用模型
// （NORMAL 组在前、DRAIN 次之，组内按模型 ID 升序稳定排列）；显式配置时仅返回该模型（level='model'）。
func TestManager_PickKeyProbeTargets_MultiModelSorted(t *testing.T) {
	nowFn := func() time.Time { return time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC) }
	mgr := NewManager(nil, nil, testSM4Key, nil, nil, nowFn, nil)
	mgr.Upsert(&Channel{
		ID: 1, Name: "c1", Protocol: ProtocolOpenAICompat,
		BaseURL: "http://probe.local/v1", State: StateNormal,
		HealthProbe: HealthProbeConfig{Interval: "* * * * * *", TimeoutMS: 1000,
			FailThreshold: 1, RecoveryThreshold: 2},
	})
	mgr.UpsertModel(&ChannelModel{ID: 2, ChannelID: 1, InternalModelID: "m2", State: StateNormal})
	mgr.UpsertModel(&ChannelModel{ID: 1, ChannelID: 1, InternalModelID: "m1", State: StateDrain})
	mgr.UpsertModel(&ChannelModel{ID: 3, ChannelID: 1, InternalModelID: "m-off", State: StateDisabled})

	enc, err := crypto.SM4Encrypt(testSM4Key, []byte("sk-probe-sel-123456"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	mgr.UpsertKey(ChannelKey{ID: 21, ChannelID: 1, Name: "k-sel", CredentialEnc: enc})
	kr, ok := mgr.GetKeyRuntime(21)
	if !ok {
		t.Fatalf("key 21 runtime missing")
	}

	targets := mgr.pickKeyProbeTargets(kr)
	want := []probeTarget{
		{Model: "m2", Level: "model"}, // 模型层全探：NORMAL 组在前（m1 为 DRAIN，排在其后）
		{Model: "m1", Level: "model"},
	}
	if !slices.Equal(targets, want) {
		t.Fatalf("targets want %+v got %+v", want, targets)
	}

	kr.Channel.HealthProbe.ProbeModel = "m1"
	targets = mgr.pickKeyProbeTargets(kr)
	wantExplicit := []probeTarget{{Model: "m1", Level: "key"}} // 密钥层定向探测
	if !slices.Equal(targets, wantExplicit) {
		t.Fatalf("explicit targets want %+v got %+v", wantExplicit, targets)
	}
}

// TestManager_RemoveKey_CleansProbeRunning 密钥摘除后不再保留探测登记。
func TestManager_RemoveKey_CleansProbeRunning(t *testing.T) {
	mgr, mock := newManagerWithTwoKeys(t)
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() { cancel(); mgr.Stop() }()

	mgr.RemoveKey(11)
	if mgr.probeRunning[11] {
		t.Fatalf("RemoveKey 后 probeRunning 仍保留 key 11")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestManager_Drop_CleansProbeRunningAndRuntime 渠道删除（Drop）后其全部密钥运行时与探测登记应一并清除，
// 探测循环随 GetKeyRuntime 丢失退出（生命周期与渠道/密钥一致）。
func TestManager_Drop_CleansProbeRunningAndRuntime(t *testing.T) {
	mgr, mock := newManagerWithTwoKeys(t)
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() { cancel(); mgr.Stop() }()

	if !mgr.probeRunning[11] || !mgr.probeRunning[12] {
		t.Fatalf("前置：Start 后密钥应登记探测循环")
	}
	mgr.Drop(1)
	if _, ok := mgr.GetRuntime(1); ok {
		t.Fatalf("Drop 后渠道运行时应摘除")
	}
	if _, ok := mgr.GetKeyRuntime(11); ok || mgr.probeRunning[11] {
		t.Fatalf("Drop 后 key 11 运行时/探测登记应清理")
	}
	if _, ok := mgr.GetKeyRuntime(12); ok || mgr.probeRunning[12] {
		t.Fatalf("Drop 后 key 12 运行时/探测登记应清理")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestManager_RebuildKeys_RemovedKeyCleansProbeRunning 渠道保存重建密钥后，被剔除密钥的探测登记应清理
// （与 RemoveKey 同等生命周期语义）。
func TestManager_RebuildKeys_RemovedKeyCleansProbeRunning(t *testing.T) {
	mgr, mock := newManagerWithTwoKeys(t)
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() { cancel(); mgr.Stop() }()

	mgr.mu.Lock()
	mgr.rebuildChannelKeysLocked(1, []ChannelKey{mgr.keys[12].Key})
	mgr.mu.Unlock()

	if _, ok := mgr.GetKeyRuntime(11); ok {
		t.Fatalf("重建后 key 11 运行时应被剔除")
	}
	if mgr.probeRunning[11] {
		t.Fatalf("重建后 key 11 探测登记应清理")
	}
	if !mgr.probeRunning[12] {
		t.Fatalf("保留的 key 12 探测登记仍应在")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestManager_RebuildModels_PreservesMemoryState 渠道保存重建全部模型时不得重置内存权威状态
// （配置保存不重置状态：模型被探测置为 DRAIN/DISABLED 后，渠道配置保存不应把它拉回 DB 值）。
func TestManager_RebuildModels_PreservesMemoryState(t *testing.T) {
	nowFn := func() time.Time { return time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC) }
	mgr := NewManager(nil, nil, testSM4Key, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nowFn)
	mgr.Upsert(&Channel{ID: 1, Name: "c1", Protocol: ProtocolOpenAICompat,
		BaseURL: "https://api.example.com", State: StateNormal,
		HealthProbe: HealthProbeConfig{FailThreshold: 1, RecoveryThreshold: 2}})
	mgr.UpsertModel(&ChannelModel{ID: 5, ChannelID: 1, InternalModelID: "m5", State: StateNormal})

	mr, ok := mgr.GetModelRuntime(1, 5)
	if !ok {
		t.Fatalf("GetModelRuntime missing")
	}
	mr.Machine.transition(StateDrain, nowFn(), "probe_fail")

	// 渠道保存重建：DB 值为 NORMAL，但内存权威状态为 DRAIN，重建后应保留。
	mgr.mu.Lock()
	mgr.rebuildModelsLocked(1, []ChannelModel{{ID: 5, ChannelID: 1, InternalModelID: "m5", State: StateNormal}})
	mgr.mu.Unlock()

	mr2, ok := mgr.GetModelRuntime(1, 5)
	if !ok {
		t.Fatalf("重建后模型运行时缺失")
	}
	if mr2.Machine.State() != StateDrain {
		t.Fatalf("渠道保存重建模型后内存权威状态被重置：want DRAIN, got %s", mr2.Machine.State())
	}
}

func TestManager_ManualSetKeyState_TargetsOnly(t *testing.T) {
	mgr, mock := newManagerWithTwoKeys(t)

	if err := mgr.ManualSetKeyState(11, "disable"); err != nil {
		t.Fatalf("manual disable key-a: %v", err)
	}
	krA, _ := mgr.GetKeyRuntime(11)
	krB, _ := mgr.GetKeyRuntime(12)
	if krA.Machine.State() != StateDisabled {
		t.Fatalf("key-a want DISABLED, got %s", krA.Machine.State())
	}
	if krB.Machine.State() != StateNormal {
		t.Fatalf("key-b must stay NORMAL, got %s", krB.Machine.State())
	}
	if reason := krA.Machine.LastReason(); reason != reasonManualDisable {
		t.Fatalf("key-a reason want %q, got %q", reasonManualDisable, reason)
	}

	// 恢复动作 recover → NORMAL。
	if err := mgr.ManualSetKeyState(11, "recover"); err != nil {
		t.Fatalf("manual recover key-a: %v", err)
	}
	if krA.Machine.State() != StateNormal {
		t.Fatalf("key-a after recover want NORMAL, got %s", krA.Machine.State())
	}

	if err := mgr.ManualSetKeyState(11, "bogus"); err == nil {
		t.Fatalf("invalid action must error")
	}
	if err := mgr.ManualSetKeyState(999, "disable"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing key want sql.ErrNoRows, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestManager_ForceChannelStateBatch_DisablesAllAndViewsLinked(t *testing.T) {
	mgr, mock := newManagerWithTwoKeys(t)

	mgr.ForceChannelStateBatch(1, "disable")

	krA, _ := mgr.GetKeyRuntime(11)
	krB, _ := mgr.GetKeyRuntime(12)
	if krA.Machine.State() != StateDisabled || krB.Machine.State() != StateDisabled {
		t.Fatalf("both keys want DISABLED, got %s / %s", krA.Machine.State(), krB.Machine.State())
	}

	// 渠道 Keys 视图联动（同一指针，状态实时反映）。
	rt, _ := mgr.GetRuntime(1)
	for _, kv := range rt.Keys {
		if kv.Machine.State() != StateDisabled {
			t.Fatalf("channel Keys view state want DISABLED, got %s", kv.Machine.State())
		}
	}
	if views := mgr.KeyViews(1); len(views) != 2 {
		t.Fatalf("KeyViews want 2, got %d", len(views))
	} else {
		for _, v := range views {
			if v.State != StateDisabled {
				t.Fatalf("KeyViews state want DISABLED, got %s", v.State)
			}
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestManager_KeyViews_CredentialTailOnly(t *testing.T) {
	mgr, mock := newManagerWithTwoKeys(t)

	views := mgr.KeyViews(1)
	if len(views) != 2 {
		t.Fatalf("KeyViews want 2, got %d", len(views))
	}
	if views[0].KeyID != 11 || views[1].KeyID != 12 {
		t.Fatalf("KeyViews must sort by key id, got %+v", views)
	}
	wantTails := map[int64]string{int64(11): testKeyACred[len(testKeyACred)-6:], int64(12): testKeyBCred[len(testKeyBCred)-6:]}
	for _, v := range views {
		if v.Name == "" || v.State != StateNormal {
			t.Fatalf("unexpected view: %+v", v)
		}
		tail := wantTails[v.KeyID]
		if v.CredentialTail != tail {
			t.Fatalf("view(key %d) tail want %q got %q", v.KeyID, tail, v.CredentialTail)
		}
		if len(v.CredentialTail) != 6 {
			t.Fatalf("view(key %d) tail must be 6 chars, got %d", v.KeyID, len(v.CredentialTail))
		}
		// 视图任何字段不得携带明文/密文。
		if strings.Contains(v.CredentialTail, testKeyACred) || strings.Contains(v.CredentialTail, testKeyBCred) {
			t.Fatalf("view(key %d) leaked plaintext: %q", v.KeyID, v.CredentialTail)
		}
		if v.LastErr != "" {
			t.Fatalf("view(key %d) LastErr want empty, got %q", v.KeyID, v.LastErr)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestManager_RemoveKey_UntracksRuntime(t *testing.T) {
	mgr, mock := newManagerWithTwoKeys(t)

	mgr.RemoveKey(11)
	if _, ok := mgr.GetKeyRuntime(11); ok {
		t.Fatalf("key 11 must be removed from runtime")
	}
	if _, ok := mgr.GetKeyRuntime(12); !ok {
		t.Fatalf("key 12 must survive removal")
	}
	rt, _ := mgr.GetRuntime(1)
	if len(rt.Keys) != 1 || rt.Keys[0].Key.ID != 12 {
		t.Fatalf("channel Keys view want only key 12, got %+v", rt.Keys)
	}
	if views := mgr.KeyViews(1); len(views) != 1 || views[0].KeyID != 12 {
		t.Fatalf("KeyViews want only key 12, got %+v", views)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestManager_KeyMaxSessions_ReadsChannelConfig(t *testing.T) {
	mgr, mock := newManagerWithTwoKeys(t)

	if got := mgr.KeyMaxSessions(11); got != 8 {
		t.Fatalf("KeyMaxSessions want 8, got %d", got)
	}
	if got := mgr.KeyMaxSessions(999); got != DefaultRateLimit().MaxConcurrent {
		t.Fatalf("KeyMaxSessions(missing) want default %d, got %d", DefaultRateLimit().MaxConcurrent, got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestManager_UpsertKey_AddsNewAndPreservesState(t *testing.T) {
	mgr, mock := newManagerWithTwoKeys(t)

	if err := mgr.ManualSetKeyState(11, "drain"); err != nil {
		t.Fatalf("manual drain key-a: %v", err)
	}

	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	encC, err := crypto.SM4Encrypt(testSM4Key, []byte("sk-new-secret"))
	if err != nil {
		t.Fatalf("encrypt key-c: %v", err)
	}
	mgr.UpsertKey(ChannelKey{ID: 13, ChannelID: 1, Name: "key-c", CredentialEnc: encC, State: StateNormal, CreatedAt: now, UpdatedAt: now})

	krC, ok := mgr.GetKeyRuntime(13)
	if !ok {
		t.Fatalf("new key 13 runtime missing")
	}
	if krC.CredentialPlain != "sk-new-secret" {
		t.Fatalf("key-c plain want %q got %q", "sk-new-secret", krC.CredentialPlain)
	}
	if krC.Machine.State() != StateNormal {
		t.Fatalf("key-c initial state want NORMAL, got %s", krC.Machine.State())
	}

	// 更新已存在密钥：替换 Machine/Limiter 但保留内存权威状态（DRAIN）。
	mgr.UpsertKey(ChannelKey{ID: 11, ChannelID: 1, Name: "key-a", CredentialEnc: encC, State: StateNormal})
	krA, _ := mgr.GetKeyRuntime(11)
	if krA.Machine.State() != StateDrain {
		t.Fatalf("key-a after upsert must preserve DRAIN, got %s", krA.Machine.State())
	}

	if views := mgr.KeyViews(1); len(views) != 3 {
		t.Fatalf("KeyViews want 3 after UpsertKey, got %d", len(views))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestManager_KeyOrderForRouting_RoundRobin(t *testing.T) {
	mgr, _ := newManagerWithTwoKeys(t)

	// 初始按密钥 id 升序；每次调用旋转一位。
	if got := mgr.KeyOrderForRouting(1); !slices.Equal(got, []int64{11, 12}) {
		t.Fatalf("第 1 次调用期望 [11 12]，got %v", got)
	}
	if got := mgr.KeyOrderForRouting(1); !slices.Equal(got, []int64{12, 11}) {
		t.Fatalf("第 2 次调用（旋转一位）期望 [12 11]，got %v", got)
	}
	if got := mgr.KeyOrderForRouting(1); !slices.Equal(got, []int64{11, 12}) {
		t.Fatalf("第 3 次调用期望回到 [11 12]，got %v", got)
	}

	// 返回切片必须是防御拷贝：外部修改返回值不得影响内部轮询游标。
	first := mgr.KeyOrderForRouting(1) // 此刻返回 [12 11]
	first[0] = 0
	if got := mgr.KeyOrderForRouting(1); !slices.Equal(got, []int64{11, 12}) {
		t.Fatalf("外部修改返回值影响后续轮询：%v", got)
	}

	// 密钥摘除后 keyOrder 重置重建，仅剩的密钥自成一序。
	mgr.RemoveKey(11)
	if got := mgr.KeyOrderForRouting(1); !slices.Equal(got, []int64{12}) {
		t.Fatalf("移除 key 11 后期望 [12]，got %v", got)
	}

	// 未知渠道返回 nil。
	if got := mgr.KeyOrderForRouting(99); got != nil {
		t.Fatalf("未知渠道期望 nil，got %v", got)
	}
}

// TestManager_Snapshot_NoCredentialWordingAtChannelLevel D3 核验（审查发现项）：
// 渠道模板层 LastErr「渠道凭据为空/解密失败」文案已随 A5 删除，渠道快照不再误导；
// 凭据类错误只出现在密钥级视图（KeyViews/KeyRuntimeView）。
func TestManager_Snapshot_NoCredentialWordingAtChannelLevel(t *testing.T) {
	mgr, mock := newManagerWithTwoKeys(t)

	// key-a 模拟凭据不可用：错误文案只落密钥级 LastErr。
	krA, _ := mgr.GetKeyRuntime(11)
	krA.Key.LastErr = "密钥凭据为空，请先配置后再启用"

	views := mgr.Snapshot()
	if len(views) != 1 {
		t.Fatalf("want 1 channel view, got %d", len(views))
	}
	if views[0].LastErr != "" {
		t.Fatalf("渠道模板层 LastErr 不应携带凭据文案，got %q", views[0].LastErr)
	}

	// 密钥级视图仍按密钥展示该错误（作用域正确，不误导为渠道故障）。
	keyViews := mgr.KeyViews(1)
	if len(keyViews) != 2 {
		t.Fatalf("KeyViews want 2, got %d", len(keyViews))
	}
	found := false
	for _, v := range keyViews {
		if v.KeyID == 11 && v.LastErr == "密钥凭据为空，请先配置后再启用" {
			found = true
		}
	}
	if !found {
		t.Fatalf("密钥级 LastErr 应展示凭据文案，got %+v", keyViews)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestDefaultConfigs 锁定渠道/模型默认配置（rc1 问题 1 修正后）：
// 健康探测每天一次、P99 阈值 120 秒、TPM 10 百万。
func TestDefaultConfigs(t *testing.T) {
	if got := DefaultHealthProbe().Interval; got != "0 0 * * * *" {
		t.Fatalf("DefaultHealthProbe.Interval want 每天一次(0 0 * * * *), got %q", got)
	}
	if got := DefaultReliability().P99LatencyMS; got != 120000 {
		t.Fatalf("DefaultReliability.P99LatencyMS want 120000(120s), got %d", got)
	}
	if got := DefaultMachineConfig().P99LatencyMS; got != 120000 {
		t.Fatalf("DefaultMachineConfig.P99LatencyMS want 120000(120s), got %d", got)
	}
	if got := DefaultRateLimit().TPM; got != 10000000 {
		t.Fatalf("DefaultRateLimit.TPM want 10000000(10M), got %d", got)
	}
}
