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

// 同一 (key, model) 同时有估算单与正常单时，**不能混成一组**：
// 旧实现把两行并成一组并打上 estimated，整组被剔出主口径 → 真实 token 不参与比对，
// 代理侧全量进偏差分子 → 窗口过 1e4 即误报（Owl 实测 P1）。
func TestCompareWindow_MixedEstimatedGroupKeepsRealTokens(t *testing.T) {
	local := []Group{
		{CallerKey: "k1", Model: "m1", Requests: 1, Input: 50, Output: 20, Estimated: true},
		g("k1", "m1", 10, 1000, 0, 400),
	}
	magpie := []Group{g("k1", "m1", 10, 1000, 0, 400)}

	w := compareWindow(wStart, wEnd, local, magpie, 0)
	if w.Status != "ok" {
		t.Fatalf("真实用量一致不应报警（旧实现这里会 alarm）: %+v", w.Detail)
	}
	if w.TokensLS.Input != 1000 || w.TokensLS.Output != 400 {
		t.Fatalf("主口径必须只含真实单: %+v", w.TokensLS)
	}
	if w.TokensMag.Input != 1000 {
		t.Fatalf("代理侧口径: %+v", w.TokensMag)
	}
	if w.Detail["estimated_groups"] != 1 || w.Detail["estimated_tokens"] != int64(70) {
		t.Fatalf("估算单要单独记数: %+v", w.Detail)
	}
	if absent, _ := w.Detail["absent_in_ls"].([]string); len(absent) != 0 {
		t.Fatalf("两侧都有该组，不应记成单侧缺账: %v", absent)
	}
	if w.Detail["token_sum_ls"] != int64(1400) || w.Detail["token_sum_magpie"] != int64(1400) {
		t.Fatalf("窗口 token 合计只含真实单: %+v", w.Detail)
	}
}

// 纯正常组：混合维度改造后照常参与比对（不许因切开维度而漏比）。
func TestCompareWindow_PureNormalGroupStillCompared(t *testing.T) {
	local := []Group{g("k1", "m1", 10, 200, 0, 100)}
	magpie := []Group{g("k1", "m1", 10, 100, 0, 100)}
	w := compareWindow(wStart, wEnd, local, magpie, 0)
	if w.Status != "alarm" {
		t.Fatalf("纯正常组偏差 100%% 必须 alarm: %+v", w.Detail)
	}
	if w.TokensLS.Input != 200 {
		t.Fatalf("主口径 token=%+v", w.TokensLS)
	}
}

// 场景①：灵枢只有估算单、代理侧真实用量与估算值**完全一致** → 不得给出偏差结论、不报警。
// 旧实现把「只有估算单」的组算进窗口分子/分母（localMain 空 → ls 侧全 0），
// 实测 dev_pct=100 且 alarm，与设计稿「estimated 不计入主口径」直接冲突（复审 P1）。
func TestCompareWindow_OnlyEstimatedLocalNoWindowDeviation(t *testing.T) {
	local := []Group{{CallerKey: "k1", Model: "m1", Requests: 5, Input: 15000, Output: 5000, Estimated: true}}
	magpie := []Group{g("k1", "m1", 5, 15000, 0, 5000)}

	w := compareWindow(wStart, wEnd, local, magpie, 0)
	if w.DevPct != nil {
		t.Fatalf("只有估算单时不得给出偏差结论: dev_pct=%v", *w.DevPct)
	}
	if w.Status != "ok" || w.AlarmStreak != 0 {
		t.Fatalf("不得报警: status=%s streak=%d detail=%+v", w.Status, w.AlarmStreak, w.Detail)
	}
	if w.Detail["compared_groups"] != 0 || w.Detail["token_sum_magpie_compared"] != int64(0) {
		t.Fatalf("偏差口径的可比分组应为 0: %+v", w.Detail)
	}
	// 报告口径仍要看得见代理侧全量（否则运维会以为没数据）。
	if w.Detail["token_sum_magpie"] != int64(20000) {
		t.Fatalf("token_sum_magpie 应保留全量: %+v", w.Detail)
	}
	if absent, _ := w.Detail["absent_in_ls"].([]string); len(absent) != 1 {
		t.Fatalf("应记成灵枢缺真实账: %v", w.Detail["absent_in_ls"])
	}
}

// 场景②：单侧缺账（灵枢 16000 一组、代理侧 4000 另一组）→ 不出偏差结论、不报警；
// 只记进 absent_in_*（设计稿「单侧缺账不做偏差判定」）。
// 旧实现：分子 20000 / 分母 4000 → dev_pct=500%。
func TestCompareWindow_SingleSideMissingNoWindowDeviation(t *testing.T) {
	local := []Group{g("k1", "m1", 10, 16000, 0, 0)}
	magpie := []Group{g("k2", "m2", 10, 4000, 0, 0)}

	w := compareWindow(wStart, wEnd, local, magpie, 0)
	if w.DevPct != nil {
		t.Fatalf("单侧缺账不得给出偏差结论: dev_pct=%v", *w.DevPct)
	}
	if w.Status != "ok" {
		t.Fatalf("status=%s（单侧缺账不该报警）: %+v", w.Status, w.Detail)
	}
	absentLS, _ := w.Detail["absent_in_ls"].([]string)
	absentMag, _ := w.Detail["absent_in_magpie"].([]string)
	if len(absentLS) != 1 || len(absentMag) != 1 {
		t.Fatalf("两侧缺账都要留痕: ls=%v magpie=%v", absentLS, absentMag)
	}
	if w.Detail["compared_groups"] != 0 {
		t.Fatalf("可比分组=%v", w.Detail["compared_groups"])
	}
}

// 可比分组与单侧缺账混在一起：偏差只按可比分组算（单侧缺账不稀释也不放大分子/分母）。
func TestCompareWindow_DeviationOnlyOverComparableGroups(t *testing.T) {
	local := []Group{
		g("k1", "m1", 10, 1000, 0, 1000), // 与代理侧一致
		g("k9", "m9", 10, 16000, 0, 0),   // 代理侧没有 → 单侧缺账
	}
	magpie := []Group{
		g("k1", "m1", 10, 1000, 0, 1000),
		g("k8", "m8", 10, 4000, 0, 0), // 灵枢没有 → 单侧缺账
	}
	w := compareWindow(wStart, wEnd, local, magpie, 0)
	if w.DevPct == nil {
		t.Fatalf("可比分组完全一致时 dev_pct 应为 0，实际为 nil")
	}
	if *w.DevPct != 0 {
		t.Fatalf("可比分组完全一致时 dev_pct 应为 0，实际 %v", *w.DevPct)
	}
	if w.Status != "ok" {
		t.Fatalf("status=%s（旧实现这里是 alarm）", w.Status)
	}
	if w.Detail["compared_groups"] != 1 || w.Detail["token_sum_magpie_compared"] != int64(2000) {
		t.Fatalf("偏差口径只看可比分组: %+v", w.Detail)
	}
	if w.Detail["token_sum_magpie"] != int64(6000) {
		t.Fatalf("报告口径仍是全量: %+v", w.Detail)
	}
}

// 只有估算单的组：记进 absent_in_ls（灵枢只有估算单），但不参与偏差判定。
func TestCompareWindow_OnlyEstimatedGroupNotCompared(t *testing.T) {
	local := []Group{{CallerKey: "k2", Model: "m2", Requests: 5, Input: 900, Estimated: true}}
	magpie := []Group{g("k2", "m2", 5, 100, 0, 100)}
	w := compareWindow(wStart, wEnd, local, magpie, 0)
	if w.Status == "alarm" {
		t.Fatalf("纯估算组不应报警: %+v", w.Detail)
	}
	if absent, _ := w.Detail["absent_in_ls"].([]string); len(absent) != 1 {
		t.Fatalf("应记成灵枢缺真实账: %v", w.Detail["absent_in_ls"])
	}
	if w.TokensLS.Input != 0 {
		t.Fatalf("主口径不含估算: %+v", w.TokensLS)
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
	// report 为零值时按「字段齐全 + cursor 分页」处理（既有用例的默认口径）。
	report *LedgerReport
	err    error
	from   time.Time
	to     time.Time
	keys   []string
}

func (f *fakeLedger) FetchWindow(_ context.Context, from, to time.Time, keys []string) ([]Group, LedgerReport, error) {
	f.from, f.to, f.keys = from, to, keys
	if f.report != nil {
		return f.items, *f.report, f.err
	}
	return f.items, fullyVerifiedReport(), f.err
}

// fullyVerifiedReport 是「9 项齐全 + cursor 分页」的报告（可进入主口径比对）。
func fullyVerifiedReport() LedgerReport {
	return LedgerReport{Items: 1, Records: 1, Paging: "cursor"}
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

// 拿不到 advisory lock → 本窗跳过（多副本只跑一个），但必须留可观测痕迹。
func TestRunOnce_LockNotAcquiredSkips(t *testing.T) {
	store := &fakeStore{}
	ledger := &fakeLedger{}
	r := newReconciler(store, ledger, nil)
	r.SetLock(func(context.Context) (func(), bool) { return func() {}, false })
	r.RunOnce(context.Background())
	if len(store.saved) != 0 {
		t.Fatalf("拿不到锁不该落库: %+v", store.saved)
	}
	if r.LockSkips() != 1 {
		t.Fatalf("拿不到锁必须计数（否则停摆是静默的）: %d", r.LockSkips())
	}
	r.RunOnce(context.Background())
	if r.LockSkips() != 2 {
		t.Fatalf("连续跳过的窗口都要计: %d", r.LockSkips())
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

// 契约 §8.2：字段不全 → 窗口标 blocked（既不算通过也不算不通过），
// 本地比对只作参考留在 detail.local_comparison，且不参与连续告警升级。
func TestRunOnce_BlockedWhenLedgerFieldsMissing(t *testing.T) {
	store := &fakeStore{
		// 本地侧确实有偏差（10 vs 100），但账本缺字段 → 不许判 alarm、也不许判 ok。
		local: []Group{g("k1", "m1", 10, 200, 0, 100)},
		keys:  []string{"k1"},
		prev:  2,
	}
	ledger := &fakeLedger{items: []Group{g("k1", "m1", 10, 100, 0, 100)}}
	ledger.report = &LedgerReport{Items: 1, Records: 1, Paging: "offset", Missing: []string{FieldRecordKey}}
	audit := &fakeAudit{}
	r := newReconciler(store, ledger, audit)
	r.RunOnce(t.Context())

	if len(store.saved) != 1 {
		t.Fatalf("saved=%d", len(store.saved))
	}
	w := store.saved[0]
	if w.Status != "blocked" {
		t.Fatalf("status=%s（缺字段必须 blocked）", w.Status)
	}
	if w.AlarmStreak != 0 {
		t.Fatalf("blocked 窗不该累计 alarm_streak: %d", w.AlarmStreak)
	}
	if w.Detail["blocked"] != BlockedPendingPOC {
		t.Fatalf("detail.blocked=%v", w.Detail["blocked"])
	}
	reasons, _ := w.Detail["blocked_reasons"].([]string)
	if len(reasons) == 0 {
		t.Fatalf("必须留下不可对账原因: %v", w.Detail)
	}
	if _, ok := w.Detail["local_comparison"]; !ok {
		t.Fatalf("本地比对结果应作为参考留在 detail: %v", w.Detail)
	}
	if _, ok := w.Detail["ledger_report"]; !ok {
		t.Fatalf("账本字段可见性必须落 detail: %v", w.Detail)
	}
	if audit.calls != 0 {
		t.Fatalf("blocked 窗不许升级审计: %d", audit.calls)
	}
}

// 字段齐全但 cache_read 缺失：可降级 → 仍走主口径（ok/alarm 都能出现），
// 且 detail 里显式记下缺失。
func TestRunOnce_CacheReadMissingStillCompares(t *testing.T) {
	store := &fakeStore{
		local: []Group{g("k1", "m1", 10, 100, 0, 100)},
		keys:  []string{"k1"},
	}
	ledger := &fakeLedger{items: []Group{g("k1", "m1", 10, 100, 0, 100)}}
	ledger.report = &LedgerReport{Items: 1, Records: 1, Paging: "cursor", CacheReadMissing: true}
	r := newReconciler(store, ledger, &fakeAudit{})
	r.RunOnce(t.Context())

	w := store.saved[0]
	if w.Status != "ok" {
		t.Fatalf("cache_read 缺失属可降级，仍应比对: %s (%v)", w.Status, w.Detail["blocked_reasons"])
	}
	rep, ok := w.Detail["ledger_report"].(LedgerReport)
	if !ok || !rep.CacheReadMissing {
		t.Fatalf("缺失声明必须落 detail: %v", w.Detail["ledger_report"])
	}
}
