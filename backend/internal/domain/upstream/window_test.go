package upstream

import (
	"context"
	"errors"
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

// TestRounder_MaxPagesBlocks 断言单轮 20 页上限：超限即整轮回滚 + 窗口置 blocked + 告警错误。
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
	if led.rolls != 1 {
		t.Fatalf("blocked 应回滚整轮账本事务（rolls=1），got %d", led.rolls)
	}
	if led.commits != 0 || led.count() != 0 {
		t.Fatalf("blocked 不许提交任何账本：commits=%d stored=%d", led.commits, led.count())
	}
	recs := wins.Records()
	if len(recs) != 1 || recs[0].Status != WindowBlocked {
		t.Fatalf("窗口应落一条 blocked 记录，got %+v", recs)
	}
	if !strings.Contains(recs[0].Reason, "20") {
		t.Fatalf("blocked 原因应点名页数上限，got %q", recs[0].Reason)
	}
	// blocked 不推进水位线：下一轮继续补这段窗口。
	if _, ok, _ := wins.LastSuccessEnd(context.Background()); ok {
		t.Fatal("blocked 不应推进水位线")
	}

	// 未超限（20 页正好拉完）不算 blocked。
	src2 := &fakePageSource{pages: append(pages[:MaxPagesPerRound-1:MaxPagesPerRound-1],
		Page{Entries: entries("led-last"), HasMore: false})}
	led2 := newFakeLedger()
	wins2 := NewMemoryWindowStore()
	res, err := newTestRounder(t, src2, led2, wins2, now).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("20 页拉完不应报错，got %v", err)
	}
	if res.Pages != MaxPagesPerRound || res.Status != WindowOK {
		t.Fatalf("20 页拉完应成功，got pages=%d status=%s", res.Pages, res.Status)
	}
}

// TestRounder_FetchErrorRollsBack 断言拉取失败：整轮回滚 + 窗口记 error + 不推进水位线。
func TestRounder_FetchErrorRollsBack(t *testing.T) {
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
	if led.rolls != 1 || led.count() != 0 {
		t.Fatalf("失败应回滚且不落账：rolls=%d stored=%d", led.rolls, led.count())
	}
	recs := wins.Records()
	if len(recs) != 1 || recs[0].Status != WindowError {
		t.Fatalf("窗口应落 error 记录，got %+v", recs)
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
	if led.rolls != 1 {
		t.Fatalf("应回滚，got rolls=%d", led.rolls)
	}
}
