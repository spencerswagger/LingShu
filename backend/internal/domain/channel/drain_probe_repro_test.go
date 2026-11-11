//go:build drainrepro

// 排空（drain）探测频率的现场复现 + 修复验收（不参与默认测试，避免几十秒墙钟开销）。
//
// 运行：
//
//	go test ./internal/domain/channel -tags drainrepro -run TestDrainProbeRepro -v
//
// 口径与验收剧本一致：渠道 health_probe.interval 设为「年内不触发的 cron」（等价长间隔），
// drain_interval_seconds=15；分别让 密钥 / 内部模型 / 渠道（=渠道下全部密钥）进入排空态，
// 打印状态机取值、实际探测时刻与间隔、恢复耗时，并断言：
// 首次探测 ≤18s、连续间隔 ∈[12,18]、无 <10s、窗口内 ≥3 次、恢复 ≤18s、恢复后 40s 内不再探测。
package channel

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// drainReproInterval 「年内不触发」的 cron：观测窗口内出现的任何探测只可能来自排空分支。
const drainReproInterval = "0 0 0 1 1 *"

// drainReproDrainSeconds 排空间隔（与 DefaultHealthProbe 一致）。
const drainReproDrainSeconds = 15

// 验收门限（秒）。
const (
	drainReproFirstMax   = 18.0
	drainReproGapMin     = 12.0
	drainReproGapMax     = 18.0
	drainReproHardMin    = 10.0 // 无 <10s 狂探
	drainReproWantProbes = 3
	drainReproRecoverMax = 18.0
	drainReproQuietAfter = 40 * time.Second
)

// probeRecorderCount 记录上游收到的探测请求（时间 + 凭据指纹，用于按密钥分组算间隔），
// 并可切换 ok/fail500。
type probeRecorderCount struct {
	mu   sync.Mutex
	hits []probeHit
	fail atomic.Bool
}

type probeHit struct {
	at   time.Time
	auth string
}

func (p *probeRecorderCount) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		p.mu.Lock()
		p.hits = append(p.hits, probeHit{at: time.Now(), auth: auth})
		p.mu.Unlock()
		if p.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream down","code":"boom"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
}

func (p *probeRecorderCount) snapshot() []probeHit {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]probeHit, len(p.hits))
	copy(out, p.hits)
	return out
}

func (p *probeRecorderCount) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.hits)
}

// byAuth 按凭据分组（同渠道多密钥靠 Authorization 区分），保持各自时间顺序。
func (p *probeRecorderCount) byAuth() map[string][]time.Time {
	out := map[string][]time.Time{}
	for _, h := range p.snapshot() {
		out[h.auth] = append(out[h.auth], h.at)
	}
	return out
}

// gaps 返回相邻探测的间隔秒数。
func gaps(ts []time.Time) []float64 {
	var out []float64
	for i := 1; i < len(ts); i++ {
		out = append(out, ts[i].Sub(ts[i-1]).Seconds())
	}
	return out
}

func fmtGaps(gs []float64) string {
	ss := make([]string, 0, len(gs))
	for _, g := range gs {
		ss = append(ss, fmt.Sprintf("%.1f", g))
	}
	return fmt.Sprint(ss)
}

// scheduleBranch 复述调度分支（仅用于打印诊断，与被测实现同源思路）。
func scheduleBranch(mgr *Manager) string {
	_, branch, ok := mgr.nextKeyProbeWait(11)
	if !ok {
		return "branch=<no runtime>"
	}
	return "branch=" + branch
}

type drainReproCase struct {
	name string
	// keys 该用例创建的密钥（凭据不同 → 上游命中可按凭据分组）。
	keys []int64
	// enterDrain 让对应档位进入排空态。
	enterDrain func(t *testing.T, mgr *Manager)
	// probeModel 渠道 health_probe.probe_model（空=模型级全探）。
	probeModel string
	// wantQuiet 期望窗口内 0 次探测（作用域外模型排空：不放大成排空探测）。
	wantQuiet bool
	// states 打印三档当前状态。
	states func(t *testing.T, mgr *Manager) string
	// allRecovered 密钥与模型是否都已回 NORMAL（用于判定恢复成立）。
	allRecovered func(mgr *Manager) bool
	// assertQuiet 是否断言「恢复后 40s 内不再探测」（相当于回到 cron）。
	assertQuiet bool
}

func TestDrainProbeRepro(t *testing.T) {
	keyStates := func(t *testing.T, mgr *Manager) string {
		return fmt.Sprintf("key=%s model=%s", mustKeyState(t, mgr), mustModelState(t, mgr))
	}
	bothNormal := func(mgr *Manager) bool {
		kr, ok := mgr.GetKeyRuntime(11)
		if !ok || kr.Machine.State() != StateNormal {
			return false
		}
		mr := mgr.mrt[1][21]
		return mr != nil && mr.Machine.State() == StateNormal
	}
	cases := []drainReproCase{
		{
			name:         "A-密钥排空(模型保持NORMAL)",
			keys:         []int64{11},
			enterDrain:   func(t *testing.T, mgr *Manager) { mustSetKeyState(t, mgr, 11, "drain") },
			states:       keyStates,
			allRecovered: bothNormal,
			assertQuiet:  true,
		},
		{
			name: "B-内部模型排空(密钥保持NORMAL)",
			keys: []int64{11},
			enterDrain: func(t *testing.T, mgr *Manager) {
				// 与运营后台「模型排空」同一入口：只改模型状态机。
				if err := mgr.ManualSetModelState(1, 21, StateDrain); err != nil {
					t.Fatalf("manual model drain: %v", err)
				}
			},
			states:       keyStates,
			allRecovered: bothNormal,
		},
		{
			name: "C-渠道排空(渠道下全部密钥)",
			keys: []int64{11, 12},
			enterDrain: func(t *testing.T, mgr *Manager) {
				mustSetKeyState(t, mgr, 11, "drain")
				mustSetKeyState(t, mgr, 12, "drain")
			},
			states: func(t *testing.T, mgr *Manager) string {
				return fmt.Sprintf("key11=%s key12=%s model=%s", mustKeyState(t, mgr), mustKeyState2(t, mgr), mustModelState(t, mgr))
			},
			allRecovered: func(mgr *Manager) bool {
				for _, id := range []int64{11, 12} {
					kr, ok := mgr.GetKeyRuntime(id)
					if !ok || kr.Machine.State() != StateNormal {
						return false
					}
				}
				mr := mgr.mrt[1][21]
				return mr != nil && mr.Machine.State() == StateNormal
			},
			assertQuiet: true,
		},
		{
			name:       "E-probe_model 渠道排空作用域外模型（不得放大成排空探测）",
			keys:       []int64{11, 12},
			probeModel: "probe-only", // 不在该渠道模型表里 → 选型只探它，模型 21 在作用域外
			enterDrain: func(t *testing.T, mgr *Manager) {
				if err := mgr.ManualSetModelState(1, 21, StateDrain); err != nil {
					t.Fatalf("manual model drain: %v", err)
				}
			},
			states:       keyStates,
			allRecovered: func(mgr *Manager) bool { return true },
			wantQuiet:    true,
		},
		{
			name:       "F-probe_model 渠道排空探测目标模型（按排空间隔代探并恢复）",
			keys:       []int64{11},
			probeModel: "drain-m1", // = 模型 21 的内部模型 ID
			enterDrain: func(t *testing.T, mgr *Manager) {
				if err := mgr.ManualSetModelState(1, 21, StateDrain); err != nil {
					t.Fatalf("manual model drain: %v", err)
				}
			},
			states:       keyStates,
			allRecovered: bothNormal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runDrainReproCase(t, tc)
		})
	}
}

func runDrainReproCase(t *testing.T, tc drainReproCase) {
	rec := &probeRecorderCount{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	hp := HealthProbeConfig{
		Interval:             drainReproInterval,
		DrainIntervalSeconds: drainReproDrainSeconds,
		TimeoutMS:            2000,
		FailThreshold:        1,
		RecoveryThreshold:    1, // 与验收剧本 seed 一致：一次成功即退出排空
	}
	mgr := newDrainReproManager(t, hp)
	ch := &Channel{
		ID: 1, Name: "c1", Protocol: ProtocolOpenAICompat, BaseURL: srv.URL,
		RateLimit:   RateLimitConfig{MaxConcurrent: 8},
		HealthProbe: hp,
		Reliability: ReliabilityConfig{WindowSeconds: 60, MinSamples: 10},
		CreatedAt:   time.Now(), UpdatedAt: time.Now(),
	}
	ch.HealthProbe.ProbeModel = tc.probeModel
	mgr.rt[1] = mgr.buildRuntime(ch)
	mgr.mrt[1] = map[int64]*ModelRuntime{}
	cm := ChannelModel{ID: 21, ChannelID: 1, InternalModelID: "drain-m1", ExternalModelID: 1, State: StateNormal}
	mgr.mrt[1][21] = mgr.buildModelRuntime(&cm, mgr.rt[1])
	for _, id := range tc.keys {
		cred := testKeyACred
		if id != 11 {
			cred = testKeyBCred
		}
		addKeyRuntime(t, mgr, id, cred, StateNormal)
	}

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() {
		cancel() // 先取消 Start 传入的 ctx：Stop 只取消 Manager 自建 ctx，不取消外部 ctx
		mgr.Stop()
	}()

	// 让循环先算出一段长等待（cron 年内不触发），证明「排空发生在等待中途」也能被唤醒。
	time.Sleep(1200 * time.Millisecond)
	t.Logf("[%s] 初态 %s %s（等待已进入 cron 长睡）", tc.name, tc.states(t, mgr), scheduleBranch(mgr))

	rec.fail.Store(true)
	tc.enterDrain(t, mgr)
	enterAt := time.Now()
	t.Logf("[%s] 排空瞬间 %s %s", tc.name, tc.states(t, mgr), scheduleBranch(mgr))

	// 观测：48s 内按 15s 应出现 3 次以上探测（按密钥分组，口径与验收剧本一致）。
	window := 48 * time.Second
	deadline := enterAt.Add(window)
	for time.Now().Before(deadline) {
		if rec.total() >= 4*len(tc.keys) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	perKey := rec.byAuth()
	t.Logf("[%s] 观测 %v：探测 %d 次（%d 个密钥），末态 %s", tc.name, window, rec.total(), len(perKey), tc.states(t, mgr))
	if tc.wantQuiet {
		if rec.total() != 0 {
			t.Errorf("[%s] 作用域外模型排空不应触发探测，实测 %d 次", tc.name, rec.total())
		}
		if _, branch, ok := mgr.nextKeyProbeWait(11); !ok || branch != branchCron {
			t.Errorf("[%s] 分支应为 cron，got %q（ok=%v）", tc.name, branch, ok)
		}
		return
	}
	for auth, times := range perKey {
		gs := gaps(times)
		first := times[0].Sub(enterAt).Seconds()
		t.Logf("[%s] %s 首次 %+.1fs，间隔 %s，共 %d 次", tc.name, authLabel(auth), first, fmtGaps(gs), len(times))
		if first > drainReproFirstMax {
			t.Errorf("[%s] %s 首次探测 %.1fs > %.0fs", tc.name, authLabel(auth), first, drainReproFirstMax)
		}
		if len(times) < drainReproWantProbes {
			t.Errorf("[%s] %s 窗口内仅 %d 次探测 < %d", tc.name, authLabel(auth), len(times), drainReproWantProbes)
		}
		for i, g := range gs {
			if g > drainReproGapMax || g < drainReproGapMin {
				t.Errorf("[%s] %s 第 %d 个间隔 %.1fs 不在 [%.0f,%.0f]", tc.name, authLabel(auth), i+1, g, drainReproGapMin, drainReproGapMax)
			}
			if g < drainReproHardMin {
				t.Errorf("[%s] %s 第 %d 个间隔 %.1fs < %.0fs（狂探）", tc.name, authLabel(auth), i+1, g, drainReproHardMin)
			}
		}
	}
	if len(perKey) == 0 {
		t.Fatalf("[%s] 排空后 %v 内 0 次探测", tc.name, window)
	}

	// 上游转 ok：测恢复耗时。
	rec.fail.Store(false)
	recoverAt := time.Now()
	mark := rec.total()
	for time.Since(recoverAt) < 20*time.Second {
		if tc.allRecovered(mgr) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	recovered := tc.allRecovered(mgr)
	recoverTook := time.Since(recoverAt).Seconds()
	t.Logf("[%s] 恢复：成立=%v 耗时=%.1fs 期间新增探测=%d 末态 %s",
		tc.name, recovered, recoverTook, rec.total()-mark, tc.states(t, mgr))
	if !recovered {
		t.Fatalf("[%s] 上游恢复后 20s 内未退出排空", tc.name)
	}
	if recoverTook > drainReproRecoverMax {
		t.Errorf("[%s] 恢复耗时 %.1fs > %.0fs", tc.name, recoverTook, drainReproRecoverMax)
	}

	// 恢复后应回到 cron：40s 内不再有探测。
	if tc.assertQuiet {
		quietMark := rec.total()
		time.Sleep(drainReproQuietAfter)
		if extra := rec.total() - quietMark; extra != 0 {
			t.Errorf("[%s] 恢复后 %v 内仍有 %d 次探测（未回到 cron）", tc.name, drainReproQuietAfter, extra)
		} else {
			t.Logf("[%s] 恢复后 %v 内无新探测（已回到 cron）", tc.name, drainReproQuietAfter)
		}
	}
}

// TestDrainProbeRepro_StartupDrained 启动时就处于排空态（DB 里已是 DRAIN，构建运行时时带入）不得漏探。
func TestDrainProbeRepro_StartupDrained(t *testing.T) {
	rec := &probeRecorderCount{}
	rec.fail.Store(true)
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	hp := HealthProbeConfig{
		Interval:             drainReproInterval,
		DrainIntervalSeconds: drainReproDrainSeconds,
		TimeoutMS:            2000,
		FailThreshold:        1,
		RecoveryThreshold:    1,
	}
	mgr := newDrainReproManager(t, hp)
	ch := &Channel{
		ID: 1, Name: "c1", Protocol: ProtocolOpenAICompat, BaseURL: srv.URL,
		RateLimit:   RateLimitConfig{MaxConcurrent: 8},
		HealthProbe: hp,
		Reliability: ReliabilityConfig{WindowSeconds: 60, MinSamples: 10},
	}
	mgr.rt[1] = mgr.buildRuntime(ch)
	mgr.mrt[1] = map[int64]*ModelRuntime{}
	// 模型启动即 DRAIN、密钥启动即 NORMAL（模拟「模型排空后进程重启」）。
	cm := ChannelModel{ID: 21, ChannelID: 1, InternalModelID: "drain-m1", ExternalModelID: 1, State: StateDrain}
	mgr.mrt[1][21] = mgr.buildModelRuntime(&cm, mgr.rt[1])
	addKeyRuntime(t, mgr, 11, testKeyACred, StateNormal)

	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	mgr.Start(ctx)
	defer func() {
		cancel()
		mgr.Stop()
	}()

	deadline := start.Add(30 * time.Second)
	for time.Now().Before(deadline) && rec.total() < 2 {
		time.Sleep(100 * time.Millisecond)
	}
	hits := rec.snapshot()
	times := make([]time.Time, 0, len(hits))
	for _, h := range hits {
		times = append(times, h.at)
	}
	first := -1.0
	if len(times) > 0 {
		first = times[0].Sub(start).Seconds()
	}
	t.Logf("[启动即排空] 探测 %d 次，首次 %+.1fs，间隔 %s，key=%s model=%s",
		len(times), first, fmtGaps(gaps(times)), mustKeyState(t, mgr), mustModelState(t, mgr))
	if len(times) == 0 {
		t.Fatalf("[启动即排空] 模型启动即 DRAIN，30s 内 0 次探测")
	}
	if first > drainReproFirstMax {
		t.Errorf("[启动即排空] 首次探测 %.1fs > %.0fs", first, drainReproFirstMax)
	}
}

// newDrainReproManager 构造带真实时钟的 Manager：cmStore 用 sqlmock（不设期望，落库/事件写入走
// 错误日志路径，不影响状态机），使 ManualSetModelState 等持久化路径可用。
func newDrainReproManager(t *testing.T, hp HealthProbeConfig) *Manager {
	t.Helper()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewManager(nil, NewChannelModelStore(db), testSM4Key,
		&http.Client{Timeout: 3 * time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
}

// authLabel 凭据标签（不打印凭据本身，只标 A/B 便于读日志）。
func authLabel(auth string) string {
	switch auth {
	case "Bearer " + testKeyACred:
		return "keyA"
	case "Bearer " + testKeyBCred:
		return "keyB"
	}
	return "key?"
}

func mustKeyRuntime(t *testing.T, mgr *Manager) *KeyRuntime {
	t.Helper()
	kr, ok := mgr.GetKeyRuntime(11)
	if !ok {
		t.Fatalf("key runtime 11 missing")
	}
	return kr
}

func mustKeyState(t *testing.T, mgr *Manager) State {
	t.Helper()
	return mustKeyRuntime(t, mgr).Machine.State()
}

func mustKeyState2(t *testing.T, mgr *Manager) State {
	t.Helper()
	kr, ok := mgr.GetKeyRuntime(12)
	if !ok {
		t.Fatalf("key runtime 12 missing")
	}
	return kr.Machine.State()
}

func mustModelState(t *testing.T, mgr *Manager) State {
	t.Helper()
	mr := mgr.mrt[1][21]
	if mr == nil {
		t.Fatalf("model runtime 21 missing")
	}
	return mr.Machine.State()
}

func mustSetKeyState(t *testing.T, mgr *Manager, keyID int64, action string) {
	t.Helper()
	if err := mgr.ManualSetKeyState(keyID, action); err != nil {
		t.Fatalf("manual key state %s: %v", action, err)
	}
}
