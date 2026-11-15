package upstream

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSavedWindowFor_AlignedToSixtySeconds 断言落库窗口必须整 60s（0023 的 CHECK 口径）：
// 拉取区间含 120s 重叠、长度可能不是 60s，但落库行永远是 [n*60, (n+1)*60)。
func TestSavedWindowFor_AlignedToSixtySeconds(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 30, 0, time.UTC)
	fetch := WindowFor(time.Time{}, now) // 首轮 [09:59:30, 10:00:30)
	got := savedWindowFor(fetch)
	if !got.End.Equal(time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("应取「拉取终点向下取整」那一窗，got %s", got.End)
	}
	if got.Duration() != WindowSize {
		t.Fatalf("落库窗口必须是整 60s，got %s", got.Duration())
	}
	if err := (WindowRecord{Start: got.Start, End: got.End, Status: WindowOK}).Validate(); err != nil {
		t.Fatalf("落库窗口形状应通过校验: %v", err)
	}

	// 已对齐的拉取区间：落库窗口 = 最后一整窗。
	aligned := WindowFor(time.Time{}, time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC))
	got = savedWindowFor(aligned)
	if !got.End.Equal(aligned.End) || !got.Start.Equal(aligned.Start) {
		t.Fatalf("对齐区间应原样落库，got %s（拉取 %s）", got, aligned)
	}

	// 落库窗口不能违反 60s 断言（否则库里 CHECK 会拒）。
	bad := WindowRecord{Start: got.Start, End: got.End.Add(time.Second), Status: WindowOK}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "60s") {
		t.Fatalf("非整 60s 窗口应在写库前被拦下，got %v", err)
	}
}

// TestRounder_BlockedSurvivesRestartAndIsReloaded 断言 blocked 不因重启而消失，也不会静默：
//  1. 第一轮超页上限 → 落 blocked；
//  2. 换一个 Rounder 实例（模拟进程重启，同一个库）：本轮必须看得见这条 blocked（BlockedPending>0），
//     并且拉取起点要前移到那条 blocked 窗口，否则永远补不到它；
//  3. 再跑一轮仍然拉不完 → 依旧 blocked（不许悄悄当成 ok）；
//  4. 能拉完的那一轮 → blocked 被同事务清成 ok，水位线推进。
func TestRounder_BlockedSurvivesRestartAndIsReloaded(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 30, 0, time.UTC)
	led := newFakeLedger()
	wins := NewMemoryWindowStore()

	alwaysMore := &fakePageSource{pages: []Page{
		{Entries: entries("led-1"), HasMore: true},
		{Entries: entries("led-2"), HasMore: true},
	}}
	r1 := newTestRounder(t, alwaysMore, led, wins, now)
	r1.maxPages = 2
	if _, err := r1.RunOnce(context.Background()); !IsBlocked(err) {
		t.Fatalf("第一轮应 blocked，got %v", err)
	}
	blockedRecs := wins.Records()
	if len(blockedRecs) != 1 || blockedRecs[0].Status != WindowBlocked {
		t.Fatalf("应落一条 blocked 窗口，got %+v", blockedRecs)
	}
	blockedStart := blockedRecs[0].Start

	// 重启：新的 Rounder 实例 + 同一个库。
	morePages := &fakePageSource{pages: []Page{
		{Entries: entries("led-1"), HasMore: true},
		{Entries: entries("led-2"), HasMore: true},
	}}
	r2 := newTestRounder(t, morePages, led, wins, now.Add(WindowSize))
	r2.maxPages = 2
	res2, err := r2.RunOnce(context.Background())
	if !IsBlocked(err) {
		t.Fatalf("重启后仍拉不完应保持 blocked，got %v", err)
	}
	if res2.BlockedPending == 0 {
		t.Fatal("重启后必须看得见之前的 blocked（BlockedPending>0），不许静默")
	}
	if !morePages.windows[0].Start.Equal(blockedStart) {
		t.Fatalf("拉取起点应前移到最旧的 blocked 窗口 %s，got %s",
			blockedStart.Format(time.RFC3339), morePages.windows[0].Start.Format(time.RFC3339))
	}
	// blocked 没有被悄悄改写成 ok。
	var blockedCount, okCount int
	for _, rec := range wins.Records() {
		switch rec.Status {
		case WindowBlocked:
			blockedCount++
		case WindowOK:
			okCount++
		}
	}
	if blockedCount == 0 {
		t.Fatalf("blocked 不应消失，records=%+v", wins.Records())
	}
	if _, ok, _ := wins.LastSuccessEnd(context.Background()); ok {
		t.Fatal("blocked 期间不该有水位数（否则等于跳过没落库的数据）")
	}

	// 积压清掉：这一轮能拉完 → blocked 清成 ok + 水位线推进。
	drained := &fakePageSource{pages: []Page{{Entries: entries("led-1", "led-2"), HasMore: false}}}
	r3 := newTestRounder(t, drained, led, wins, now.Add(2*WindowSize))
	res3, err := r3.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("补平轮应成功，got %v", err)
	}
	if res3.BlockedPending == 0 {
		t.Fatal("补平轮也应先看得见积压（BlockedPending>0）")
	}
	for _, rec := range wins.Records() {
		if rec.Status == WindowBlocked {
			t.Fatalf("补平后不应再有 blocked，records=%+v", wins.Records())
		}
	}
	if _, ok, _ := wins.LastSuccessEnd(context.Background()); !ok {
		t.Fatal("补平后应推进水位线")
	}
}

// TestSQLWindowStore_BlockedSurvivesRestart 是真实库用例（真实迁移 0001–0023）：
// blocked 行落 reconcile_windows，换一个 store 实例（模拟重启）仍然读得到；
// 只有 ok 才推进水位线；成功补平后 blocked 被清成 ok。
func TestSQLWindowStore_BlockedSurvivesRestart(t *testing.T) {
	conn := openMigratedTempDB(t)
	ctx := context.Background()
	wins := NewSQLWindowStore(conn)

	blockedStart := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	blockedRec := WindowRecord{
		Start:      blockedStart,
		End:        blockedStart.Add(WindowSize),
		Status:     WindowBlocked,
		Pages:      MaxPagesPerRound,
		Fetched:    4000,
		Reason:     "单轮上限内未拉完（积压），下一轮从 offset 续拉",
		RecordedAt: blockedStart.Add(WindowSize),
	}
	if err := wins.Save(ctx, blockedRec); err != nil {
		t.Fatalf("写 blocked 窗口失败: %v", err)
	}

	// 重启：新实例 + 新连接池语义（这里换实例即可；库里才是状态本体）。
	restarted := NewSQLWindowStore(conn)
	pending, err := restarted.PendingWindows(ctx, 10)
	if err != nil {
		t.Fatalf("重启后读取 blocked 失败: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("重启后应仍有 1 条 blocked，got %d", len(pending))
	}
	if !pending[0].Start.Equal(blockedStart) || pending[0].Status != WindowBlocked {
		t.Fatalf("blocked 窗口口径不符：%+v", pending[0])
	}
	if !strings.Contains(pending[0].Reason, "积压") {
		t.Fatalf("重启后应能读回原因（detail 里的 reason），got %q", pending[0].Reason)
	}
	// 库里窗口长度由 CHECK 保证：window_end = window_start + 60s。
	var gapSeconds float64
	if err := conn.QueryRowContext(ctx,
		`SELECT extract(epoch FROM (window_end - window_start))
		   FROM `+ReconcileWindowsTable+` WHERE status='blocked'`).Scan(&gapSeconds); err != nil {
		t.Fatalf("读取窗口长度失败: %v", err)
	}
	if gapSeconds != WindowSize.Seconds() {
		t.Fatalf("库内窗口长度应为 60s，got %.0fs", gapSeconds)
	}
	// blocked 不推进水位线。
	if _, ok, err := restarted.LastSuccessEnd(ctx); err != nil || ok {
		t.Fatalf("blocked 期间不该有水位线，got ok=%v err=%v", ok, err)
	}

	// 补平：成功窗口（整 60s）+ ClearFrom 覆盖 blocked 窗口 → blocked 清成 ok。
	okStart := blockedStart.Add(WindowSize)
	okRec := WindowRecord{
		Start:      okStart,
		End:        okStart.Add(WindowSize),
		Status:     WindowOK,
		Pages:      1,
		Fetched:    2,
		Inserted:   2,
		RecordedAt: okStart.Add(WindowSize),
		ClearFrom:  blockedStart,
	}
	if err := wins.Save(ctx, okRec); err != nil {
		t.Fatalf("写 ok 窗口失败: %v", err)
	}
	pending, err = wins.PendingWindows(ctx, 10)
	if err != nil {
		t.Fatalf("补平后读取 blocked 失败: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("补平后不应再有 blocked，got %+v", pending)
	}
	end, ok, err := wins.LastSuccessEnd(ctx)
	if err != nil || !ok {
		t.Fatalf("补平后应有水位线，got ok=%v err=%v", ok, err)
	}
	if !end.Equal(okRec.End) {
		t.Fatalf("水位线应等于成功窗口的 window_end，got %s want %s", end, okRec.End)
	}
	// 老 blocked 行的 detail 保留（合并语义），状态改成 ok：
	// 掉 detail 会丢掉排障线索，这是 `||` 合并而不是覆盖的原因。
	var detail []byte
	if err := conn.QueryRowContext(ctx,
		`SELECT detail FROM `+ReconcileWindowsTable+` WHERE window_start = $1`, blockedStart).Scan(&detail); err != nil {
		t.Fatalf("读取 detail 失败: %v", err)
	}
	if !strings.Contains(string(detail), "积压") {
		t.Fatalf("清成 ok 后应保留原 detail（合并而非覆盖），got %s", detail)
	}
}

// TestSQLWindowStore_RejectsMisalignedWindow 断言「窗口长度」这条断言真的在库里：
//   - 代码侧在写库前就拦掉非整 60s（Validate）；
//   - 即使绕过代码直接写 SQL，PG 的 CHECK 也会拒（ck_reconcile_window_len）。
//
// 两条都验，才不是「只在文档里声明了一个约束」。
func TestSQLWindowStore_RejectsMisalignedWindow(t *testing.T) {
	conn := openMigratedTempDB(t)
	ctx := context.Background()
	wins := NewSQLWindowStore(conn)

	start := time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC)
	bad := WindowRecord{Start: start, End: start.Add(90 * time.Second), Status: WindowOK}
	if err := wins.Save(ctx, bad); err == nil || !strings.Contains(err.Error(), "60s") {
		t.Fatalf("代码侧应先拦下非整 60s 窗口，got %v", err)
	}

	// 绕过代码直接写库：CHECK 必须拒。
	_, err := conn.ExecContext(ctx,
		`INSERT INTO `+ReconcileWindowsTable+`(window_start, window_end, status)
		 VALUES ($1, $2, 'ok')`, start, start.Add(90*time.Second))
	if err == nil {
		t.Fatal("库层 CHECK 应拒绝非 60s 窗口（否则「断言进库里」是空话）")
	}
	if !strings.Contains(err.Error(), "ck_reconcile_window_len") {
		t.Fatalf("应是窗口长度约束报错，got %v", err)
	}
}
