package upstream

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeFeed 按调用顺序返回预置的 cursor feed 页面，并记录调用参数。
type fakeFeed struct {
	mu      sync.Mutex
	pages   []FeedPage
	errs    map[int]error
	cursors []string
	limits  []int
	seq     int // 调用序号（错误按下标取）
	pageIdx int // 页面只在成功调用时消费，避免错误调用吃掉一页
}

func (f *fakeFeed) FetchFeed(_ context.Context, cursor string, limit int) (FeedPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursors = append(f.cursors, cursor)
	f.limits = append(f.limits, limit)
	idx := f.seq
	f.seq++
	if err := f.errs[idx]; err != nil {
		return FeedPage{}, err
	}
	if f.pageIdx >= len(f.pages) {
		return FeedPage{HasMore: false}, nil
	}
	page := f.pages[f.pageIdx]
	f.pageIdx++
	return page, nil
}

func feedEntry(id string) LedgerEntry {
	return LedgerEntry{
		LedgerID: id, Provider: "anthropic", ProviderAccount: "acct", Model: "claude",
		TokensIn: 10, TokensOut: 5, OccurredAt: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC),
	}
}

func feedTestRounder(feed LedgerFeed, led *fakeLedger, store *MemoryWindowStore, now time.Time) *Rounder {
	return NewRounder(RounderOptions{
		Feed:    feed,
		Ledger:  led,
		Windows: store,
		Now:     func() time.Time { return now },
		Logger:  testLogger(),
	})
}

// TestRounder_CursorFeed_AdvancesCursorAndStops 断言 cursor feed 的主路径：
// 逐页拉取、按 id upsert、结束后把最后游标写进窗口（下一轮从这里继续，不重放整本账）。
func TestRounder_CursorFeed_AdvancesCursorAndStops(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 30, 0, time.UTC)
	feed := &fakeFeed{pages: []FeedPage{
		{Entries: []LedgerEntry{feedEntry("u_1"), feedEntry("u_2")}, NextCursor: "c1", HasMore: true},
		{Entries: []LedgerEntry{feedEntry("u_3")}, NextCursor: "c2", HasMore: false},
	}}
	led := newFakeLedger()
	store := NewMemoryWindowStore()
	r := feedTestRounder(feed, led, store, now)

	res, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Pages != 2 || res.Inserted != 3 || res.Fetched != 3 {
		t.Fatalf("页数/条数不符：pages=%d inserted=%d fetched=%d", res.Pages, res.Inserted, res.Fetched)
	}
	if res.Cursor != "c2" {
		t.Fatalf("收尾游标应为 c2，实际 %q", res.Cursor)
	}
	if got := feed.cursors; len(got) != 2 || got[0] != MagpieCursorStart || got[1] != "c1" {
		t.Fatalf("游标序列应为 [start c1]，实际 %v", got)
	}
	if led.count() != 3 {
		t.Fatalf("账本应有 3 条，实际 %d", led.count())
	}
	if c, ok, err := store.LastLedgerCursor(context.Background()); err != nil || !ok || c != "c2" {
		t.Fatalf("窗口必须落最后游标 c2，实际 ok=%v cursor=%q err=%v", ok, c, err)
	}
}

// TestRounder_CursorFeed_ResumesFromStoredCursor 断言重启/下一轮从持久化游标继续，
// 不从 start 重放（否则每 60s 全量扫一遍账本）。
func TestRounder_CursorFeed_ResumesFromStoredCursor(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 1, 30, 0, time.UTC)
	store := NewMemoryWindowStore()
	base := now.Add(-2 * time.Minute).Truncate(WindowSize)
	if err := store.Save(context.Background(), WindowRecord{
		Start: base, End: base.Add(WindowSize), Status: WindowOK, RecordedAt: base.Add(WindowSize),
		LedgerCursor: "c9",
	}); err != nil {
		t.Fatalf("预置游标失败: %v", err)
	}
	feed := &fakeFeed{pages: []FeedPage{{Entries: nil, NextCursor: "", HasMore: false}}}
	r := feedTestRounder(feed, newFakeLedger(), store, now)

	res, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(feed.cursors) != 1 || feed.cursors[0] != "c9" {
		t.Fatalf("应从持久化游标 c9 开始，实际 %v", feed.cursors)
	}
	if res.Cursor != "c9" {
		t.Fatalf("空轮也要保留游标 c9，实际 %q", res.Cursor)
	}
}

// TestRounder_CursorFeed_CursorResetReplaysFromStart 断言 410 cursor_reset 后从 start 重放一次，
// 且不是死循环（第二次仍失败就报错）。
func TestRounder_CursorFeed_CursorResetReplaysFromStart(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 2, 30, 0, time.UTC)
	store := NewMemoryWindowStore()
	base := now.Add(-2 * time.Minute).Truncate(WindowSize)
	if err := store.Save(context.Background(), WindowRecord{
		Start: base, End: base.Add(WindowSize), Status: WindowOK, RecordedAt: base.Add(WindowSize),
		LedgerCursor: "old",
	}); err != nil {
		t.Fatalf("预置游标失败: %v", err)
	}
	feed := &fakeFeed{
		errs:  map[int]error{0: &MagpieCursorError{StatusCode: 410, Code: "cursor_reset"}},
		pages: []FeedPage{{Entries: []LedgerEntry{feedEntry("u_9")}, NextCursor: "c-new", HasMore: false}},
	}
	led := newFakeLedger()
	r := feedTestRounder(feed, led, store, now)

	res, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(feed.cursors) != 2 || feed.cursors[0] != "old" || feed.cursors[1] != MagpieCursorStart {
		t.Fatalf("游标序列应为 [old start]，实际 %v", feed.cursors)
	}
	if led.count() != 1 || res.Cursor != "c-new" {
		t.Fatalf("重放后应写入 1 条且游标为 c-new，实际 rows=%d cursor=%q", led.count(), res.Cursor)
	}
}

// TestRounder_CursorFeed_CursorResetTwiceFails 断言「从 start 重放后仍失效」要报错，
// 不能在同一轮里无限重试。
func TestRounder_CursorFeed_CursorResetTwiceFails(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 3, 30, 0, time.UTC)
	store := NewMemoryWindowStore()
	base := now.Add(-2 * time.Minute).Truncate(WindowSize)
	if err := store.Save(context.Background(), WindowRecord{
		Start: base, End: base.Add(WindowSize), Status: WindowOK, RecordedAt: base.Add(WindowSize),
		LedgerCursor: "old",
	}); err != nil {
		t.Fatalf("预置游标失败: %v", err)
	}
	feed := &fakeFeed{errs: map[int]error{
		0: &MagpieCursorError{StatusCode: 410, Code: "cursor_reset"},
		1: &MagpieCursorError{StatusCode: 410, Code: "cursor_reset"},
	}}
	r := feedTestRounder(feed, newFakeLedger(), store, now)

	if _, err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("重放后仍失效必须报错")
	}
	if len(feed.cursors) != 2 {
		t.Fatalf("只允许从 start 重放一次，实际调用 %d 次", len(feed.cursors))
	}
}

// TestRounder_CursorFeed_CursorResetReplacesLocalLedger 断言 410 之后的语义是「整段替换」而不是
// 「只 upsert」：代理侧的 cursor=start **不返回 deleted 列表**，所以本地那些代理侧已经删掉/合并掉的
// 旧 id 只能靠 reset 时清空来去掉。只 upsert 的旧实现会让 u_session 与 u_gateway 并存 → 同一通调用
// 被计两次（对账每窗虚高、实际漏账看不见）。
func TestRounder_CursorFeed_CursorResetReplacesLocalLedger(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 7, 30, 0, time.UTC)
	store := NewMemoryWindowStore()
	base := now.Add(-2 * time.Minute).Truncate(WindowSize)
	if err := store.Save(context.Background(), WindowRecord{
		Start: base, End: base.Add(WindowSize), Status: WindowOK, RecordedAt: base.Add(WindowSize),
		LedgerCursor: "old",
	}); err != nil {
		t.Fatalf("预置游标失败: %v", err)
	}
	led := newFakeLedger()
	// 本地存量：一条是代理侧已合并掉的 u_session（start 不会再有它），一条是重放也会给的新 id。
	led.rows["u_session"] = feedEntry("u_session")
	led.rows["u_gateway"] = feedEntry("u_gateway")

	feed := &fakeFeed{
		errs: map[int]error{0: &MagpieCursorError{StatusCode: 410, Code: "cursor_reset"}},
		pages: []FeedPage{{
			Entries:    []LedgerEntry{feedEntry("u_gateway")},
			NextCursor: "c-new", HasMore: false,
		}},
	}
	r := feedTestRounder(feed, led, store, now)

	res, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Replaced != 2 {
		t.Fatalf("应报告清空了 2 条本地账本，实际 %d", res.Replaced)
	}
	if led.truncates != 1 {
		t.Fatalf("reset 必须做且只做一次整段替换，实际 %d 次", led.truncates)
	}
	led.mu.Lock()
	defer led.mu.Unlock()
	if _, exists := led.rows["u_session"]; exists {
		t.Fatalf("重放后本地不许残留代理侧已删除的旧 id（u_session）")
	}
	if len(led.rows) != 1 {
		t.Fatalf("本地账本应只剩重放回来的 1 条，实际 %d 条：%+v", len(led.rows), led.rows)
	}
}

// TestRounder_CursorFeed_ResetTruncatesEvenOnEmptyFirstPage 断言重放第一页为空也要执行清空：
// 代理侧账本已经空了（或这一页恰好没有行），不清空就等于「旧 id 永久残留」。
func TestRounder_CursorFeed_ResetTruncatesEvenOnEmptyFirstPage(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 8, 30, 0, time.UTC)
	store := NewMemoryWindowStore()
	base := now.Add(-2 * time.Minute).Truncate(WindowSize)
	if err := store.Save(context.Background(), WindowRecord{
		Start: base, End: base.Add(WindowSize), Status: WindowOK, RecordedAt: base.Add(WindowSize),
		LedgerCursor: "old",
	}); err != nil {
		t.Fatalf("预置游标失败: %v", err)
	}
	led := newFakeLedger()
	led.rows["u_stale"] = feedEntry("u_stale")

	feed := &fakeFeed{
		errs:  map[int]error{0: &MagpieCursorError{StatusCode: 410, Code: "cursor_reset"}},
		pages: []FeedPage{{Entries: nil, NextCursor: "", HasMore: false}},
	}
	r := feedTestRounder(feed, led, store, now)
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if led.count() != 0 {
		t.Fatalf("代理侧账本为空时本地必须被清空，实际还剩 %d 条", led.count())
	}
}

// TestRounder_CursorFeed_DeletesRows 断言代理侧 deleted 列表真的删本地账本
// （不删就会把已合并/清除的行继续计入对账）。
func TestRounder_CursorFeed_DeletesRows(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 4, 30, 0, time.UTC)
	led := newFakeLedger()
	led.rows["u_gone"] = feedEntry("u_gone")
	feed := &fakeFeed{pages: []FeedPage{
		{Deleted: []string{"u_gone"}, NextCursor: "c1", HasMore: false},
	}}
	r := feedTestRounder(feed, led, NewMemoryWindowStore(), now)

	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if led.count() != 0 {
		t.Fatalf("deleted 列表必须删除本地行，实际还剩 %d", led.count())
	}
}

// TestRounder_CursorFeed_BlockedPersistsCursor 断言单轮上限打住时：
// 已提交的页保留、窗口置 blocked 且带上续读游标，下一轮从该游标继续。
func TestRounder_CursorFeed_BlockedPersistsCursor(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 5, 30, 0, time.UTC)
	feed := &fakeFeed{pages: []FeedPage{
		{Entries: []LedgerEntry{feedEntry("u_1")}, NextCursor: "c1", HasMore: true},
	}}
	led := newFakeLedger()
	store := NewMemoryWindowStore()
	r := NewRounder(RounderOptions{
		Feed: feed, Ledger: led, Windows: store, MaxPages: 1,
		Now: func() time.Time { return now }, Logger: testLogger(),
	})

	_, err := r.RunOnce(context.Background())
	var blocked *ErrWindowBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("应返回 ErrWindowBlocked，实际 %v", err)
	}
	if blocked.Cursor != "c1" {
		t.Fatalf("blocked 应带续读游标 c1，实际 %q", blocked.Cursor)
	}
	pending, err := store.PendingWindows(context.Background(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("应有一条 blocked 窗口，实际 %d err=%v", len(pending), err)
	}
	if pending[0].LedgerCursor != "c1" {
		t.Fatalf("blocked 窗口必须持久化游标 c1，实际 %q", pending[0].LedgerCursor)
	}
}

// TestRounder_CursorFeed_HasMoreWithoutCursorFails 断言协议漂移要显式报错：
// has_more=true 但没有 next_cursor 会死循环，不能静默 ignored。
func TestRounder_CursorFeed_HasMoreWithoutCursorFails(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 6, 30, 0, time.UTC)
	feed := &fakeFeed{pages: []FeedPage{{Entries: []LedgerEntry{feedEntry("u_1")}, HasMore: true}}}
	r := feedTestRounder(feed, newFakeLedger(), NewMemoryWindowStore(), now)

	if _, err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("has_more 但缺 next_cursor 必须报错")
	}
}
