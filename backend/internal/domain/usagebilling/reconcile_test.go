package usagebilling

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ---- 判定口径 ----

func g(callerKey, model string, reqs, in, cache, out int64) Group {
	return Group{CallerKey: callerKey, Model: model, Requests: reqs, Input: in, CacheRead: cache, Output: out}
}

var (
	wStart = time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	wEnd   = wStart.Add(time.Minute)
)

// 分组：笔数达标且某段相对偏差 >5% → alarm（分母是代理侧）。
func TestCompareWindow_GroupDeviationAlarms(t *testing.T) {
	local := []Group{g("k1", "m1", 10, 110, 0, 100)}
	magpie := []Group{g("k1", "m1", 10, 100, 0, 100)}

	w := compareWindow(wStart, wEnd, local, magpie, 0)
	if w.Status != "alarm" {
		t.Fatalf("status=%s（输入 110 vs 100 应超 5%%）", w.Status)
	}
	if w.AlarmStreak != 1 {
		t.Fatalf("streak=%d", w.AlarmStreak)
	}
	groups, _ := w.Detail["alarm_groups"].([]map[string]any)
	if len(groups) != 1 || groups[0]["segment"] != "input" {
		t.Fatalf("命中段=%+v", groups)
	}
}

// 阈值内（恰好 5% 不算超）→ ok。
func TestCompareWindow_WithinThresholdIsOK(t *testing.T) {
	local := []Group{g("k1", "m1", 10, 105, 0, 100)}
	magpie := []Group{g("k1", "m1", 10, 100, 0, 100)}
	if w := compareWindow(wStart, wEnd, local, magpie, 0); w.Status != "ok" {
		t.Fatalf("status=%s（5%% 不应报警）", w.Status)
	}
}

// 笔数不足样本门槛 → 即使偏差很大也不 alarm（笔数只进 detail）。
func TestCompareWindow_SmallSampleDoesNotAlarm(t *testing.T) {
	local := []Group{g("k1", "m1", 1, 1000, 0, 1000)}
	magpie := []Group{g("k1", "m1", 1, 1, 0, 1)}
	w := compareWindow(wStart, wEnd, local, magpie, 0)
	if w.Status != "ok" {
		t.Fatalf("status=%s（1 笔样本不该报警）", w.Status)
	}
	if w.Detail["req_count_mismatch"] != false {
		t.Fatalf("笔数一致时不该标记 mismatch: %+v", w.Detail["req_count_mismatch"])
	}
}

// magpie=0 且 ls>0 → 该段直接命中；两边同为 0 → 跳过。
func TestCompareWindow_ZeroDenominatorRules(t *testing.T) {
	local := []Group{g("k1", "m1", 5, 10, 0, 0)}
	magpie := []Group{g("k1", "m1", 5, 0, 0, 0)}
	if w := compareWindow(wStart, wEnd, local, magpie, 0); w.Status != "alarm" {
		t.Fatalf("magpie=0 且 ls>0 应命中, status=%s", w.Status)
	}
	local = []Group{g("k1", "m1", 5, 0, 0, 0)}
	magpie = []Group{g("k1", "m1", 5, 0, 0, 0)}
	if w := compareWindow(wStart, wEnd, local, magpie, 0); w.Status != "ok" {
		t.Fatalf("两边同为 0 应跳过, status=%s", w.Status)
	}
}

// estimated 单不进主口径（不参与 token 对比），只在 detail 里计数。
func TestCompareWindow_EstimatedGroupsExcluded(t *testing.T) {
	local := []Group{{CallerKey: "k1", Model: "m1", Requests: 10, Input: 9999, Estimated: true}}
	magpie := []Group{g("k1", "m1", 10, 10, 0, 10)}
	w := compareWindow(wStart, wEnd, local, magpie, 0)
	if w.Status == "alarm" {
		t.Fatalf("estimated 组不应参与报警: %+v", w.Detail)
	}
	if w.Detail["estimated_groups"] != 1 {
		t.Fatalf("estimated_groups=%v", w.Detail["estimated_groups"])
	}
	if w.TokensLS.Input != 0 {
		t.Fatalf("主口径 token 应排除 estimated: %+v", w.TokensLS)
	}
}

// 窗口整体阈值：加权偏差 >2% 且总量 ≥10000 才 alarm。
func TestCompareWindow_WindowLevelThreshold(t *testing.T) {
	// 每段偏差 3%，总量 ≥10000 → 窗口级 alarm（分组笔数不足门槛，故只可能由窗口触发）。
	local := []Group{g("k1", "m1", 1, 5150, 0, 5150)}
	magpie := []Group{g("k1", "m1", 1, 5000, 0, 5000)}
	w := compareWindow(wStart, wEnd, local, magpie, 0)
	if w.Status != "alarm" {
		t.Fatalf("status=%s（3%% > 2%% 且总量达标）", w.Status)
	}
	// 总量不足 10000 → 不 alarm。
	local = []Group{g("k1", "m1", 1, 1030, 0, 1030)}
	magpie = []Group{g("k1", "m1", 1, 1000, 0, 1000)}
	if w := compareWindow(wStart, wEnd, local, magpie, 0); w.Status != "ok" {
		t.Fatalf("总量不足不应 alarm, status=%s", w.Status)
	}
}

// 连续 3 窗 alarm → streak 递增（升级由 RunOnce 依据 streak 判定）。
func TestCompareWindow_StreakAccumulates(t *testing.T) {
	local := []Group{g("k1", "m1", 10, 200, 0, 100)}
	magpie := []Group{g("k1", "m1", 10, 100, 0, 100)}
	w := compareWindow(wStart, wEnd, local, magpie, 2)
	if w.AlarmStreak != 3 {
		t.Fatalf("streak=%d", w.AlarmStreak)
	}
	// 恢复正常 → streak 归零。
	magpie = []Group{g("k1", "m1", 10, 200, 0, 100)}
	if w := compareWindow(wStart, wEnd, local, magpie, 5); w.AlarmStreak != 0 {
		t.Fatalf("恢复后 streak 应归零, got %d", w.AlarmStreak)
	}
}

// 对账绝不回写账单：Reconciler 只有 SaveWindow 一条写路径。
type fakeStore struct {
	local    []Group
	magpie   []Group
	keys     []string
	prev     int
	saved    []Window
	saveErr  error
	localErr error
	keysErr  error
	prevErr  error
}

func (f *fakeStore) LocalGroups(context.Context, time.Time, time.Time) ([]Group, error) {
	return f.local, f.localErr
}
func (f *fakeStore) CallerKeys(context.Context) ([]string, error) { return f.keys, f.keysErr }
func (f *fakeStore) PreviousStreak(context.Context) (int, error)  { return f.prev, f.prevErr }
func (f *fakeStore) SaveWindow(_ context.Context, w Window) error {
	f.saved = append(f.saved, w)
	return f.saveErr
}

type fakeLedger struct {
	items []Group
	err   error
	from  time.Time
	to    time.Time
	keys  []string
}

func (f *fakeLedger) FetchWindow(_ context.Context, from, to time.Time, keys []string) ([]Group, error) {
	f.from, f.to, f.keys = from, to, keys
	return f.items, f.err
}

type fakeAudit struct {
	calls  int
	detail map[string]any
	err    error
}

func (f *fakeAudit) InsertReconcileAlarm(_ context.Context, detail map[string]any) error {
	f.calls++
	f.detail = detail
	return f.err
}

func newReconciler(store *fakeStore, ledger *fakeLedger, audit *fakeAudit) *Reconciler {
	r := NewReconciler(store, ledger, audit, nil)
	now := wEnd
	r.now = func() time.Time { return now }
	return r
}

func TestRunOnce_SavesAlarmWindow(t *testing.T) {
	store := &fakeStore{
		local: []Group{g("k1", "m1", 10, 200, 0, 100)},
		keys:  []string{"k1"},
		prev:  2,
	}
	ledger := &fakeLedger{items: []Group{g("k1", "m1", 10, 100, 0, 100)}}
	audit := &fakeAudit{}
	r := newReconciler(store, ledger, audit)
	r.RunOnce(context.Background())

	if len(store.saved) != 1 {
		t.Fatalf("窗口落库条数=%d", len(store.saved))
	}
	w := store.saved[0]
	if w.Status != "alarm" || w.AlarmStreak != 3 {
		t.Fatalf("窗口=%s", w.String())
	}
	if !w.WindowStart.Equal(wEnd.Add(-ReconcileInterval)) || !w.WindowEnd.Equal(wEnd) {
		t.Fatalf("窗口区间=%v..%v", w.WindowStart, w.WindowEnd)
	}
	if audit.calls != 1 {
		t.Fatalf("连续 3 窗应升级审计一次, calls=%d", audit.calls)
	}
	if ledger.from.IsZero() || len(ledger.keys) != 1 {
		t.Fatalf("账本拉取参数=%v/%v", ledger.from, ledger.keys)
	}
}

func TestRunOnce_LedgerErrorRecordsErrorWindow(t *testing.T) {
	store := &fakeStore{keys: []string{"k1"}}
	ledger := &fakeLedger{err: errors.New("connect: refused")}
	audit := &fakeAudit{}
	r := newReconciler(store, ledger, audit)
	r.RunOnce(context.Background())

	w := store.saved[0]
	if w.Status != "error" {
		t.Fatalf("status=%s", w.Status)
	}
	if w.Detail["error"] == nil {
		t.Fatalf("error 窗口应带原因: %+v", w.Detail)
	}
	if audit.calls != 0 {
		t.Fatalf("拉取失败不该升级审计")
	}
}

// 未装配账本 → skipped（可观测「没在对账」，不误报 alarm）。
func TestRunOnce_NoLedgerSavesSkipped(t *testing.T) {
	store := &fakeStore{}
	r := NewReconciler(store, nil, nil, nil)
	r.now = func() time.Time { return wEnd }
	r.RunOnce(context.Background())
	if len(store.saved) != 1 || store.saved[0].Status != "skipped" {
		t.Fatalf("saved=%+v", store.saved)
	}
}

// 拿不到 advisory lock → 本窗跳过（多副本只跑一个）。
func TestRunOnce_LockNotAcquiredSkips(t *testing.T) {
	store := &fakeStore{}
	ledger := &fakeLedger{}
	r := newReconciler(store, ledger, nil)
	r.SetLock(func(context.Context) (func(), bool) { return func() {}, false })
	r.RunOnce(context.Background())
	if len(store.saved) != 0 {
		t.Fatalf("拿不到锁不该落库: %+v", store.saved)
	}
}

// 上一窗读失败不阻断本窗（按 0 处理）。
func TestRunOnce_PreviousStreakErrorDoesNotBlock(t *testing.T) {
	store := &fakeStore{keys: []string{"k1"}, prevErr: errors.New("no table")}
	ledger := &fakeLedger{}
	r := newReconciler(store, ledger, nil)
	r.RunOnce(context.Background())
	if len(store.saved) != 1 || store.saved[0].Status != "ok" {
		t.Fatalf("saved=%+v", store.saved)
	}
}
