package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/pkg/dbx"
)

// fakePageSource 按调用顺序返回预置分页，并记录每次请求的窗口与 offset。
type fakePageSource struct {
	mu      sync.Mutex
	pages   []Page
	errs    map[int]error // offset → 错误
	offsets []int
	windows []Window
	calls   int
}

func (f *fakePageSource) FetchPage(_ context.Context, w Window, offset, limit int) (Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.windows = append(f.windows, w)
	f.offsets = append(f.offsets, offset)
	if err := f.errs[offset]; err != nil {
		return Page{}, err
	}
	idx := f.calls
	f.calls++
	if idx >= len(f.pages) {
		return Page{HasMore: false}, nil
	}
	return f.pages[idx], nil
}

// fakeLedger 是内存账本：把「同一事务内多页写入 + 幂等键去重 + 提交/回滚」的语义模拟出来，
// 让 Rounder 的行为（尤其 blocked 回滚）可断言。
type fakeLedger struct {
	mu       sync.Mutex
	rows     map[string]LedgerEntry
	commits  int
	rolls    int
	upserted int
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{rows: map[string]LedgerEntry{}}
}

func (f *fakeLedger) Begin(context.Context) (LedgerTx, error) {
	return &fakeTx{store: f, pending: map[string]LedgerEntry{}}, nil
}

type fakeTx struct {
	store   *fakeLedger
	pending map[string]LedgerEntry
}

func (t *fakeTx) Upsert(_ context.Context, entries []LedgerEntry) (int, error) {
	inserted := 0
	for _, e := range entries {
		if err := e.Validate(); err != nil {
			return inserted, err
		}
		if _, exists := t.store.rows[e.LedgerID]; exists {
			continue // ON CONFLICT (ledger_id) DO NOTHING
		}
		if _, dup := t.pending[e.LedgerID]; dup {
			continue
		}
		t.pending[e.LedgerID] = e
		inserted++
	}
	return inserted, nil
}

func (t *fakeTx) Commit() error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	for k, v := range t.pending {
		t.store.rows[k] = v
	}
	t.store.upserted += len(t.pending)
	t.store.commits++
	return nil
}

func (t *fakeTx) Execer() dbx.Execer { return nil }

func (t *fakeTx) Rollback() error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	t.pending = map[string]LedgerEntry{}
	t.store.rolls++
	return nil
}

func (f *fakeLedger) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func entries(ids ...string) []LedgerEntry {
	out := make([]LedgerEntry, 0, len(ids))
	for i, id := range ids {
		e := validEntry(id)
		e.OccurredAt = e.OccurredAt.Add(time.Duration(i) * time.Second)
		out = append(out, e)
	}
	return out
}

func newTestRounder(t *testing.T, src PageSource, led *fakeLedger, wins *MemoryWindowStore, now time.Time) *Rounder {
	t.Helper()
	return NewRounder(RounderOptions{
		Source:  src,
		Ledger:  led,
		Windows: wins,
		Now:     func() time.Time { return now },
		Logger:  testLogger(),
		// 用例假定上游已证过升序能力（F9 闸门的正向路径）；闸门本身另有专门用例。
		LedgerOrder:       LedgerOrderSupported,
		LedgerOrderReason: "用例假定",
	})
}

// TestWindowFor_OverlapTwoWindows 断言窗口口径：[上次结束-120s, now)，首轮只取 1 窗。
func TestWindowFor_OverlapTwoWindows(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 30, 0, 0, time.UTC)
	w := WindowFor(time.Time{}, now)
	if w.Duration() != WindowSize {
		t.Fatalf("首轮应只取 1 窗（60s），got %s", w.Duration())
	}
	lastEnd := now.Add(-time.Minute)
	w = WindowFor(lastEnd, now)
	if w.Start != lastEnd.Add(-OverlapWindows*WindowSize) {
		t.Fatalf("起点应为 上次结束-120s，got %s want %s", w.Start, lastEnd.Add(-OverlapWindows*WindowSize))
	}
	if w.End != now {
		t.Fatalf("终点应为 now，got %s", w.End)
	}
	// 落库水位线按 60s 向下对齐：下一轮起点只会更早（重叠只多不少）。
	rec := WindowRecord{End: now.Add(37 * time.Second)}
	if got := rec.Watermark(); got != now.Add(37*time.Second).Truncate(WindowSize) {
		t.Fatalf("水位线应按 60s 对齐，got %s", got)
	}
}

// TestRounder_OverlapDedupeByLedgerID 断言窗口重叠不会重复计数：
// 第二轮把上一轮的两条又拉了一遍（迟到/重叠），只有新的一条被插入，库内总数不翻倍。
func TestRounder_OverlapDedupeByLedgerID(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	src := &fakePageSource{pages: []Page{
		{Entries: entries("led-a", "led-b"), HasMore: false},          // 第一轮
		{Entries: entries("led-a", "led-b", "led-c"), HasMore: false}, // 第二轮（重叠 2 窗，含旧数据）
	}}
	led := newFakeLedger()
	wins := NewMemoryWindowStore()

	r1 := newTestRounder(t, src, led, wins, now)
	res1, err := r1.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if res1.Inserted != 2 || led.count() != 2 {
		t.Fatalf("第一轮应插入 2 条，got inserted=%d stored=%d", res1.Inserted, led.count())
	}

	// 第二轮：now 前进 60s，窗口起点 = 上次水位线 - 120s（必然覆盖上一轮的数据）。
	r2 := newTestRounder(t, src, led, wins, now.Add(WindowSize))
	res2, err := r2.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	if res2.Inserted != 1 {
		t.Fatalf("重叠窗口里旧数据不应重复计数：应只插入 1 条，got %d", res2.Inserted)
	}
	if led.count() != 3 {
		t.Fatalf("库内总数应为 3（不重复计数），got %d", led.count())
	}
	// 第二轮窗口起点确实回退了 120s（重叠口径落地）。
	lastWin := src.windows[len(src.windows)-1]
	wantStart := wins.Records()[0].Watermark().Add(-OverlapWindows * WindowSize)
	if !lastWin.Start.Equal(wantStart) {
		t.Fatalf("第二轮起点应为 上次水位线-120s（%s），got %s", wantStart, lastWin.Start)
	}
}

// TestRounder_OffsetPagingBoundary 断言 offset 翻页边界：每页偏移按已拉条数推进，
// has_more=false 立即停（不多翻一页），空页 + has_more=false 也算结束。
func TestRounder_OffsetPagingBoundary(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	src := &fakePageSource{pages: []Page{
		{Entries: entries("led-1", "led-2"), HasMore: true},
		{Entries: entries("led-3", "led-4"), HasMore: true},
		{Entries: entries("led-5"), HasMore: false},
	}}
	led := newFakeLedger()
	r := newTestRounder(t, src, led, NewMemoryWindowStore(), now)

	res, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	wantOffsets := []int{0, 2, 4}
	if len(src.offsets) != len(wantOffsets) {
		t.Fatalf("翻页次数不符：got %v want %v", src.offsets, wantOffsets)
	}
	for i := range wantOffsets {
		if src.offsets[i] != wantOffsets[i] {
			t.Fatalf("offset 序列不符：got %v want %v", src.offsets, wantOffsets)
		}
	}
	if res.Pages != 3 || res.Fetched != 5 || res.Inserted != 5 {
		t.Fatalf("轮次统计不符：pages=%d fetched=%d inserted=%d", res.Pages, res.Fetched, res.Inserted)
	}

	// 恰好整页但 has_more=false：不许再翻一页。
	src2 := &fakePageSource{pages: []Page{{Entries: entries("led-1", "led-2"), HasMore: false}}}
	led2 := newFakeLedger()
	if _, err := newTestRounder(t, src2, led2, NewMemoryWindowStore(), now).RunOnce(context.Background()); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(src2.offsets) != 1 {
		t.Fatalf("has_more=false 应立即停止，got offsets=%v", src2.offsets)
	}
}

// TestRounder_MaxPagesBlocks 断言单轮上限：超限即 blocked + 告警错误，并把 offset 持久化。
//
// 本切片的**有意变更**（F9）：不再「整轮回滚」，而是「每页一事务 + 同页持久化断点」。
// 理由：整轮回滚 + 持久化 offset 会自相矛盾 —— 数据被擦掉了、断点却说「拉到第 N 条」，
// 下一轮从 N 继续就把前 N 条永久跳过（静默漏账）。断点必须代表**已落库**的进度。
func TestRounder_MaxPagesBlocks(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	var pages []Page
	for i := 0; i < 25; i++ { // 数据源永远声称还有更多
		pages = append(pages, Page{Entries: entries("led-" + strings.Repeat("x", i+1)), HasMore: true})
	}
	src := &fakePageSource{pages: pages}
	led := newFakeLedger()
	wins := NewMemoryWindowStore()
	r := newTestRounder(t, src, led, wins, now)

	_, err := r.RunOnce(context.Background())
	if !IsBlocked(err) {
		t.Fatalf("超页上限应返回 blocked 错误，got %T %v", err, err)
	}
	if len(src.offsets) != MaxPagesPerRound {
		t.Fatalf("应恰好翻 %d 页就停，got %d", MaxPagesPerRound, len(src.offsets))
	}
	// 一页一事务：已翻的页已提交（断点代表已落库），本轮没有整轮回滚。
	if led.commits != MaxPagesPerRound || led.count() != MaxPagesPerRound {
		t.Fatalf("已翻的页应逐页提交：commits=%d stored=%d want %d",
			led.commits, led.count(), MaxPagesPerRound)
	}
	if led.rolls != 0 {
		t.Fatalf("blocked 不该回滚已提交的页（rolls=%d）", led.rolls)
	}
	recs := wins.Records()
	if len(recs) != 1 || recs[0].Status != WindowBlocked {
		t.Fatalf("窗口应落一条 blocked 记录，got %+v", recs)
	}
	if !strings.Contains(recs[0].Reason, "单轮上限") {
		t.Fatalf("blocked 原因应点名单轮上限，got %q", recs[0].Reason)
	}
	if recs[0].ScrollOffset != MaxPagesPerRound {
		t.Fatalf("blocked 行应持久化 offset=%d，got %d", MaxPagesPerRound, recs[0].ScrollOffset)
	}
	// blocked 不推进水位线：下一轮继续补这段窗口。
	if _, ok, _ := wins.LastSuccessEnd(context.Background()); ok {
		t.Fatal("blocked 不应推进水位线")
	}

	// 下一轮必须从 offset 续拉（不是从 0 重拉）：这才是「积压自愈、不需要人工介入」。
	src2 := &fakePageSource{pages: []Page{{Entries: entries("led-tail"), HasMore: false}}}
	if _, err := newTestRounder(t, src2, led, wins, now.Add(WindowSize)).RunOnce(context.Background()); err != nil {
		t.Fatalf("补拉轮应成功，got %v", err)
	}
	if len(src2.offsets) == 0 || src2.offsets[0] != MaxPagesPerRound {
		t.Fatalf("补拉轮应从 blocked 行的 offset 起（%d），got %v", MaxPagesPerRound, src2.offsets)
	}
	// 补平后 blocked 清成 ok 且 offset 归零。
	for _, rec := range wins.Records() {
		if rec.Status == WindowBlocked {
			t.Fatalf("补平后不应再有 blocked，records=%+v", wins.Records())
		}
	}

	// 未超限（20 页正好拉完）不算 blocked。
	src3 := &fakePageSource{pages: append(pages[:MaxPagesPerRound-1:MaxPagesPerRound-1],
		Page{Entries: entries("led-last"), HasMore: false})}
	led3 := newFakeLedger()
	wins3 := NewMemoryWindowStore()
	res, err := newTestRounder(t, src3, led3, wins3, now).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("20 页拉完不应报错，got %v", err)
	}
	if res.Pages != MaxPagesPerRound || res.Status != WindowOK {
		t.Fatalf("20 页拉完应成功，got pages=%d status=%s", res.Pages, res.Status)
	}
}

// TestRounder_FetchErrorKeepsCommittedPages 断言拉取中途失败：已提交的页留下（不丢数据）、
// 窗口记 error 并带上断点 offset、水位线不动。
func TestRounder_FetchErrorKeepsCommittedPages(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	boom := errors.New("上游 502")
	src := &fakePageSource{
		pages: []Page{{Entries: entries("led-1"), HasMore: true}},
		errs:  map[int]error{1: boom},
	}
	led := newFakeLedger()
	wins := NewMemoryWindowStore()
	_, err := newTestRounder(t, src, led, wins, now).RunOnce(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("应透传拉取错误，got %v", err)
	}
	if led.count() != 1 || led.commits != 1 {
		t.Fatalf("已提交的第 1 页必须留下（否则断点会跳过它）：commits=%d stored=%d",
			led.commits, led.count())
	}
	recs := wins.Records()
	if len(recs) != 1 || recs[0].Status != WindowError {
		t.Fatalf("窗口应落 error 记录，got %+v", recs)
	}
	if recs[0].ScrollOffset != 1 {
		t.Fatalf("error 行应带上已落库的位置（1），got %d", recs[0].ScrollOffset)
	}
}

// TestRounder_EmptyPageWithHasMoreIsError 断言「空页 + has_more」是数据源缺陷，必须报错而不是死循环。
func TestRounder_EmptyPageWithHasMoreIsError(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	src := &fakePageSource{pages: []Page{{Entries: nil, HasMore: true}}}
	led := newFakeLedger()
	_, err := newTestRounder(t, src, led, NewMemoryWindowStore(), now).RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "has_more") {
		t.Fatalf("空页 + has_more 应报错，got %v", err)
	}
	// 空页没有可提交的账本，也就没有回滚；但必须落 error 行（否则静默死循环）。
	if led.rolls != 0 || led.commits != 0 {
		t.Fatalf("空页不应产生任何账本事务：rolls=%d commits=%d", led.rolls, led.commits)
	}
}

// slicePageSource 是「遵守排序与 offset 语义」的分页源：按 occurred_at ASC 排序后切片。
//
// 为什么不用 fakePageSource：它按调用顺序返回预置页，**忽略 offset**，
// 所以「offset 每轮从 0 重拉」这种缺陷在它上面是隐形的。本用例要抓的正是那个。
type slicePageSource struct {
	mu      sync.Mutex
	all     []LedgerEntry
	offsets []int
}

func (s *slicePageSource) FetchPage(_ context.Context, w Window, offset, limit int) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offsets = append(s.offsets, offset)
	if offset < 0 {
		offset = 0
	}
	in := make([]LedgerEntry, 0, len(s.all))
	for _, e := range s.all {
		if !e.OccurredAt.Before(w.Start) && e.OccurredAt.Before(w.End) {
			in = append(in, e)
		}
	}
	if offset >= len(in) {
		return Page{}, nil
	}
	end := offset + limit
	hasMore := false
	if end < len(in) {
		hasMore = true
	} else {
		end = len(in)
	}
	return Page{Entries: in[offset:end], HasMore: hasMore}, nil
}

// TestRounder_RealSQL_Backlog4200_TwoRounds 复现 Gate 的基线红场景：
// 账本积压 4200 条 → 第一轮必须落 4000 条 + blocked + offset=4000；
// 第二轮必须**从 4000 续拉**（不是从 0 重拉）、补完 200 条、blocked 清零、offset 归零。
//
// 用真实迁移库（真实 ledger_entries / reconcile_windows），分页源换成遵守 offset 语义的实现。
func TestRounder_RealSQL_Backlog4200_TwoRounds(t *testing.T) {
	conn := openMigratedTempDB(t)
	ctx := context.Background()

	total := 4200
	base := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	all := make([]LedgerEntry, 0, total)
	for i := 0; i < total; i++ {
		all = append(all, LedgerEntry{
			LedgerID:   fmt.Sprintf("backlog-%05d", i),
			Provider:   "p",
			Model:      "m",
			TokensIn:   1,
			TokensOut:  2,
			OccurredAt: base.Add(time.Duration(i) * time.Millisecond),
		})
	}
	src := &slicePageSource{all: all}

	wins := NewSQLWindowStore(conn)
	led := NewLedgerStore(conn)
	clock := base.Add(WindowSize)
	r := NewRounder(RounderOptions{
		Source: src, Ledger: led, Windows: wins,
		Now: func() time.Time { return clock }, Logger: testLogger(),
		LedgerOrder: LedgerOrderSupported, LedgerOrderReason: "用例假定",
	})

	res1, err := r.RunOnce(ctx)
	if !IsBlocked(err) {
		t.Fatalf("4200 条积压第一轮必须 blocked，got %v (res=%+v)", err, res1)
	}
	if res1.Inserted != MaxRowsPerRound {
		t.Fatalf("第一轮应落 %d 条，got %d", MaxRowsPerRound, res1.Inserted)
	}
	var rows int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM `+LedgerTable).Scan(&rows); err != nil {
		t.Fatalf("统计账本失败: %v", err)
	}
	if rows != MaxRowsPerRound {
		t.Fatalf("第一轮后库里应有 %d 条（断点代表已落库），got %d", MaxRowsPerRound, rows)
	}
	pending, err := wins.PendingWindows(ctx, blockedProbeLimit)
	if err != nil {
		t.Fatalf("读待补拉失败: %v", err)
	}
	if len(pending) != 1 || pending[0].Status != WindowBlocked {
		t.Fatalf("应有一条 blocked 窗口，got %+v", pending)
	}
	if pending[0].ScrollOffset != MaxRowsPerRound {
		t.Fatalf("blocked 行必须持久化 offset=%d，got %d", MaxRowsPerRound, pending[0].ScrollOffset)
	}

	clock = clock.Add(WindowSize)
	res2, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("第二轮补拉应成功，got %v", err)
	}
	if len(src.offsets) < 2 || src.offsets[MaxPagesPerRound] != MaxRowsPerRound {
		t.Fatalf("第二轮首个 offset 必须是 %d（不许从 0 重拉），got %v",
			MaxRowsPerRound, src.offsets)
	}
	if res2.ResumeOffset != MaxRowsPerRound {
		t.Fatalf("第二轮应从 blocked 行的 offset 续拉（%d），got %d", MaxRowsPerRound, res2.ResumeOffset)
	}
	if got := total - MaxRowsPerRound; res2.Inserted != got {
		t.Fatalf("第二轮应补 %d 条，got %d", got, res2.Inserted)
	}
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM `+LedgerTable).Scan(&rows); err != nil {
		t.Fatalf("统计账本失败: %v", err)
	}
	if rows != total {
		t.Fatalf("两轮后账本应为 %d 条，got %d", total, rows)
	}
	pending, err = wins.PendingWindows(ctx, blockedProbeLimit)
	if err != nil {
		t.Fatalf("读待补拉失败: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("补平后不应再有 blocked/error 窗口，got %+v", pending)
	}
	var off int64
	if err := conn.QueryRowContext(ctx,
		`SELECT scroll_offset FROM reconcile_windows ORDER BY window_start DESC LIMIT 1`).Scan(&off); err != nil {
		t.Fatalf("读 offset 失败: %v", err)
	}
	if off != 0 {
		t.Fatalf("补平后 offset 必须归零，got %d", off)
	}
	// 计数列：同步轮次不写 → 必须仍是 NULL（不是 0）。
	var cnt *int64
	if err := conn.QueryRowContext(ctx,
		`SELECT local_tokens FROM reconcile_windows ORDER BY window_start DESC LIMIT 1`).Scan(&cnt); err != nil {
		t.Fatalf("读计数失败: %v", err)
	}
	if cnt != nil {
		t.Fatalf("同步轮次不该写计数列（应为 NULL），got %d", *cnt)
	}
}

// TestRounder_OrderUnprovenBlocksWithoutFetching 覆盖 F9「先证后写」：
// 排序能力未证实（或已证否）时，本窗直接记 blocked、一条都不拉、offset 保持 0。
//
// 为什么必须「先证」而不是「拉一半发现再退」：翻页一旦跑过，offset 就写进库了，
// 等于用一个不可信的排序去证明进度；之后要么漏账（静默）要么永远补不平。
func TestRounder_OrderUnprovenBlocksWithoutFetching(t *testing.T) {
	for _, order := range []LedgerOrder{LedgerOrderUnverified, LedgerOrderUnsupported} {
		t.Run(order.String(), func(t *testing.T) {
			now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
			src := &fakePageSource{pages: []Page{{Entries: entries("led-1"), HasMore: true}}}
			led := newFakeLedger()
			wins := NewMemoryWindowStore()
			r := NewRounder(RounderOptions{
				Source: src, Ledger: led, Windows: wins,
				Now:               func() time.Time { return now },
				Logger:            testLogger(),
				LedgerOrder:       order,
				LedgerOrderReason: "上游未提供稳定升序承诺",
			})

			_, err := r.RunOnce(context.Background())
			if !IsLedgerOrderUnproven(err) {
				t.Fatalf("应返回排序能力缺失的 blocked 错误，got %T %v", err, err)
			}
			if len(src.offsets) != 0 {
				t.Fatalf("证不了排序就不许翻页，got offsets=%v", src.offsets)
			}
			if led.count() != 0 || led.commits != 0 {
				t.Fatalf("不许写账本，got stored=%d commits=%d", led.count(), led.commits)
			}
			recs := wins.Records()
			if len(recs) != 1 || recs[0].Status != WindowBlocked {
				t.Fatalf("应落一条 blocked 窗口，got %+v", recs)
			}
			if recs[0].ScrollOffset != 0 {
				t.Fatalf("不许假装有进度（offset 必须 0），got %d", recs[0].ScrollOffset)
			}
			if !strings.Contains(recs[0].Reason, "排序能力") {
				t.Fatalf("blocked 原因要点名排序能力，got %q", recs[0].Reason)
			}
			if _, ok, _ := wins.LastSuccessEnd(context.Background()); ok {
				t.Fatal("blocked 不许推进水位线")
			}
		})
	}
}

// TestProbeLedgerOrder_Cases 覆盖能力探测本身的三种结论（含「样本不足 = 未证实」）。
func TestProbeLedgerOrder_Cases(t *testing.T) {
	base := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Second) }
	mkFrom := func(from int, ids ...string) []LedgerEntry {
		out := make([]LedgerEntry, 0, len(ids))
		for i, id := range ids {
			out = append(out, LedgerEntry{LedgerID: id, OccurredAt: at(from + i)})
		}
		return out
	}

	cases := []struct {
		name  string
		pages []Page
		want  LedgerOrder
	}{
		{
			name:  "升序且跨页不回退 → 支持",
			pages: []Page{{Entries: mkFrom(0, "a", "b"), HasMore: true}, {Entries: mkFrom(2, "c", "d"), HasMore: false}},
			want:  LedgerOrderSupported,
		},
		{
			name:  "同值按 ledger_id 升序 → 支持",
			pages: []Page{{Entries: []LedgerEntry{{LedgerID: "a", OccurredAt: base}, {LedgerID: "b", OccurredAt: base}}, HasMore: true}, {Entries: []LedgerEntry{{LedgerID: "c", OccurredAt: base}}, HasMore: false}},
			want:  LedgerOrderSupported,
		},
		{
			name: "occurred_at 回退 → 不支持",
			pages: []Page{
				{Entries: []LedgerEntry{{LedgerID: "b", OccurredAt: at(1)}, {LedgerID: "a", OccurredAt: at(0)}}, HasMore: true},
				{Entries: mkFrom(2, "c"), HasMore: false},
			},
			want: LedgerOrderUnsupported,
		},
		{
			name:  "同值缺 tie-break（b 在 a 之前）→ 不支持",
			pages: []Page{{Entries: []LedgerEntry{{LedgerID: "b", OccurredAt: base}, {LedgerID: "a", OccurredAt: base}}, HasMore: false}},
			want:  LedgerOrderUnsupported,
		},
		{
			name:  "跨页回退 → 不支持",
			pages: []Page{{Entries: mkFrom(0, "a", "c"), HasMore: true}, {Entries: mkFrom(1, "b"), HasMore: false}},
			want:  LedgerOrderUnsupported,
		},
		{
			name:  "样本不足（1 条）→ 未证实",
			pages: []Page{{Entries: mkFrom(0, "a"), HasMore: false}, {}},
			want:  LedgerOrderUnverified,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ProbeLedgerOrder(context.Background(), &fakePageSource{pages: c.pages},
				Window{Start: base, End: base.Add(WindowSize)}, 10)
			if got.Order != c.want {
				t.Fatalf("探测结论不符：got %s (%s) want %s", got.Order, got.Reason, c.want)
			}
			if got.Reason == "" {
				t.Fatal("结论必须带人能看懂的依据（要进窗 detail）")
			}
			if got.CheckedAt.IsZero() {
				t.Fatal("探测结论必须带探测时刻")
			}
		})
	}
}

// TestPOC_UpstreamLedgerOrder_Mock 是「先证后写」的 POC 本身：走真实 HTTP 分页源 +
// 能力探测，对着**同一个 mock 上游**的两种排序跑一遍——升序判「支持」、乱序判「不支持」。
//
// 这一条的用途：证明探测链路（HTTP 源 → ProbeLedgerOrder → 结论）是通的、且能识别坏排序。
// 真实上游的 POC 见 TestPOC_UpstreamLedgerOrder_Real（需 UPSTREAM_LEDGER_URL）。
func TestPOC_UpstreamLedgerOrder_Mock(t *testing.T) {
	base := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	itemsAsc := []map[string]any{}
	itemsDesc := []map[string]any{}
	for i := 0; i < 5; i++ {
		item := map[string]any{
			"id":           fmt.Sprintf("rec-%d", i),
			"occurred_at":  base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
			"model":        "m",
			"input_tokens": 1, "output_tokens": 2,
		}
		itemsAsc = append(itemsAsc, item)
	}
	for i := 4; i >= 0; i-- {
		itemsDesc = append(itemsDesc, itemsAsc[i])
	}

	serve := func(items []map[string]any) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != LedgerPath {
				t.Errorf("POC 应打 %s，got %s", LedgerPath, r.URL.Path)
			}
			off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			lim, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if lim <= 0 {
				lim = 1000
			}
			end := off + lim
			if end > len(items) {
				end = len(items)
			}
			var out []map[string]any
			if off < len(items) {
				out = items[off:end]
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"items": out})
		}))
	}

	w := Window{Start: base.Add(-time.Minute), End: base.Add(time.Hour)}
	for _, c := range []struct {
		name  string
		items []map[string]any
		want  LedgerOrder
	}{
		{"上游升序 → 支持", itemsAsc, LedgerOrderSupported},
		{"上游乱序 → 不支持", itemsDesc, LedgerOrderUnsupported},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := serve(c.items)
			defer srv.Close()
			src := NewHTTPLedgerSource(srv.URL, "admin-key", "", nil)
			got := ProbeLedgerOrder(context.Background(), src, w, 3)
			if got.Order != c.want {
				t.Fatalf("POC 结论不符：got %s（%s）want %s", got.Order, got.Reason, c.want)
			}
			if got.Samples < 2 {
				t.Fatalf("POC 必须真的比到数据，got samples=%d", got.Samples)
			}
		})
	}
}

// TestPOC_UpstreamLedgerOrder_Real 是对真实上游的排序能力 POC（F9 裁定要求「先证后写」）。
//
// 运行方式（只在能连上上游时才有意义）：
//
//	UPSTREAM_LEDGER_URL=https://<上游基址> UPSTREAM_ADMIN_KEY=<管理端 key> \
//	  go test ./internal/domain/upstream/ -run TestPOC_UpstreamLedgerOrder_Real -v
//
// 没有 URL 就跳过——沙盒里没有上游服务，跳过时结论只能是「未证实」，
// 而「未证实」的默认行为是**本窗 blocked 且不可清零**（不许自己造排序）。
func TestPOC_UpstreamLedgerOrder_Real(t *testing.T) {
	baseURL := os.Getenv("UPSTREAM_LEDGER_URL")
	if baseURL == "" {
		t.Skip("未设置 UPSTREAM_LEDGER_URL：真实上游 POC 未执行 → 排序能力记为 unverified（本窗不可清零）")
	}
	src := NewHTTPLedgerSource(baseURL, os.Getenv("UPSTREAM_ADMIN_KEY"), "", nil)
	w := Window{Start: time.Now().Add(-30 * time.Minute), End: time.Now()}
	res := ProbeLedgerOrder(context.Background(), src, w, 50)
	t.Logf("POC 结论：order=%s samples=%d reason=%s", res.Order, res.Samples, res.Reason)
	if res.Order != LedgerOrderSupported {
		t.Fatalf("上游排序能力未达标（%s）：%s —— 需按 F9 走「本窗 blocked 不可清零」分支", res.Order, res.Reason)
	}
}

// TestRounder_EmptyRoundIsObservable 覆盖 P1 复验项：空轮不许「0 行 + nil」静默。
//
// 以前空轮与正常轮返回值完全一样，后台 ticker 每分钟静默空转、计数列永远 NULL 也没人知道。
// 现在空轮必须置 Empty 并打一条 WARN（日志是唯一的可观测点，用例断言这条日志存在）。
func TestRounder_EmptyRoundIsObservable(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	src := &fakePageSource{pages: []Page{{Entries: nil, HasMore: false}}}
	wins := NewMemoryWindowStore()
	r := NewRounder(RounderOptions{
		Source: src, Ledger: newFakeLedger(), Windows: wins,
		Now: func() time.Time { return now }, Logger: logger,
		LedgerOrder: LedgerOrderSupported,
	})
	res, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("空轮不该报错，got %v", err)
	}
	if !res.Empty {
		t.Fatalf("空轮必须置 Empty（调用方据此告警），got %+v", res)
	}
	if !strings.Contains(buf.String(), "空轮") {
		t.Fatalf("空轮必须留一条 WARN，日志：%s", buf.String())
	}

	// 非空轮不该被误标。
	src2 := &fakePageSource{pages: []Page{{Entries: entries("led-1"), HasMore: false}}}
	buf.Reset()
	r2 := NewRounder(RounderOptions{
		Source: src2, Ledger: newFakeLedger(), Windows: wins,
		Now: func() time.Time { return now.Add(WindowSize) }, Logger: logger,
		LedgerOrder: LedgerOrderSupported,
	})
	res2, err := r2.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("非空轮失败: %v", err)
	}
	if res2.Empty {
		t.Fatalf("非空轮不该标 Empty，got %+v", res2)
	}
	if strings.Contains(buf.String(), "空轮") {
		t.Fatalf("非空轮不该打空轮告警，日志：%s", buf.String())
	}
}

// TestRounder_BlockedStatusConsistent 覆盖 Gate 复验的 P2：
// 窗口被判 blocked 时，**库记录、返回值、日志三者必须一致**，不许「库是 blocked、返回值是 ok」。
//
// 三种 blocked 来源都钉住：①排序能力未证实（capability gate）
// ②排序已证否（unsupported）③页数超限（ErrWindowBlocked）。
// 一致性 = Status==blocked && Reason 与日志同源 && Empty==false（失败不算空轮）。
func TestRounder_BlockedStatusConsistent(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)

	run := func(name string, order LedgerOrder, src *fakePageSource) {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			wins := NewMemoryWindowStore()
			r := NewRounder(RounderOptions{
				Source: src, Ledger: newFakeLedger(), Windows: wins,
				Now: func() time.Time { return now }, Logger: logger,
				LedgerOrder:       order,
				LedgerOrderReason: "排序能力未经证实（测试注入）",
			})
			res, err := r.RunOnce(context.Background())
			if err == nil {
				t.Fatalf("blocked 必须以错误返回（调用方不得误判成功）")
			}
			if res.Status != WindowBlocked {
				t.Fatalf("返回值 Status 必须是 blocked（库已落 blocked），got %s（reason=%s）", res.Status, res.Reason)
			}
			if res.Reason == "" {
				t.Fatal("blocked 必须带 reason（与日志同源）")
			}
			if res.Empty {
				t.Fatal("blocked 的 0 行是失败不是空轮，Empty 必须为 false")
			}
			// 库记录一致（PendingWindows 只返回 blocked/error 窗口——有记录即证明库里落了 blocked）。
			recs, err := wins.PendingWindows(context.Background(), 10)
			if err != nil || len(recs) == 0 {
				t.Fatalf("库应留一条 blocked 窗口记录: %v", err)
			}
			last := recs[len(recs)-1]
			if last.Status != WindowBlocked {
				t.Fatalf("库记录 Status=%s, want blocked", last.Status)
			}
			if last.Reason == "" {
				t.Fatal("库记录必须带 reason")
			}
			// 日志一致： WARN/ERROR 里必须点明 blocked 与原因，且不许打「空轮」。
			if !strings.Contains(buf.String(), "blocked") {
				t.Fatalf("日志必须写明 blocked，日志：%s", buf.String())
			}
			if strings.Contains(buf.String(), "空轮") {
				t.Fatalf("blocked 不得打空轮告警，日志：%s", buf.String())
			}
		})
	}

	// ① 排序能力未证实（unverified：未探测）。
	run("unverified-order", LedgerOrderUnverified,
		&fakePageSource{pages: []Page{{Entries: entries("led-1"), HasMore: false}}})
	// ② 已证否（unsupported）。
	run("unsupported-order", LedgerOrderUnsupported,
		&fakePageSource{pages: []Page{{Entries: entries("led-1"), HasMore: false}}})
	// ③ 页数超限 blocked（supported，但页数上限被 20 页闸门拦住）。
	many := make([]Page, 0, 30)
	for i := 0; i < 30; i++ {
		many = append(many, Page{Entries: entries(fmt.Sprintf("led-%d", i)), HasMore: i < 29})
	}
	run("page-limit", LedgerOrderSupported, &fakePageSource{pages: many})
}
