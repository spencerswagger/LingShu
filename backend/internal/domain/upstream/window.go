package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/team/llmgateway/internal/pkg/logger"
)

// 同步窗口口径（已批准）：
//   - 窗口长度固定 60s（与对账窗口同源）；
//   - 迟到数据用「重叠 2 窗」吸收：本轮拉取区间 = [上次成功结束 - 120s, now)；
//   - offset 翻页但**不持久化**：每轮从 offset=0 重新翻，翻不完不许把 offset 存起来当进度
//     （存量 offset 一旦漂移会把同一条账算两次、或永久跳过一条）；
//   - 单轮 ≤ 20 页；20 页还没拉完 → 本轮置 `blocked` 并告警（积压要么扩页上限，要么人工介入）。
const (
	// WindowSize 是同步窗口长度。
	WindowSize = 60 * time.Second
	// OverlapWindows 是重叠窗口数（2 窗 = 120s）。
	OverlapWindows = 2
	// DefaultPageSize 是单页条数。
	DefaultPageSize = 200
	// MaxPagesPerRound 是单轮页数上限；超出即 blocked。
	MaxPagesPerRound = 20
)

// Window 是左闭右开的同步窗口 [Start, End)。
type Window struct {
	Start time.Time
	End   time.Time
}

// String 渲染成日志可读区间。
func (w Window) String() string {
	return fmt.Sprintf("[%s, %s)", w.Start.Format(time.RFC3339), w.End.Format(time.RFC3339))
}

// Duration 返回窗口长度。
func (w Window) Duration() time.Duration { return w.End.Sub(w.Start) }

// WindowFor 计算本轮拉取窗口：Start = lastEnd - 2×60s，End = now。
//
// 首次同步（没有水位线）只取 1 窗 [now-60s, now)：没有历史水位线时凭空空拉 3 窗没有意义，
// 且「第一轮拉太多」更容易撞上 20 页上限。
// 落库的水位线按 60s 对齐（见 WindowRecord.End 的写法），所以下一轮的 Start 至少比
// 「上次真实结束时刻 - 120s」更早，重叠只会多不会少。
func WindowFor(lastEnd, now time.Time) Window {
	end := now
	start := end.Add(-WindowSize)
	if !lastEnd.IsZero() {
		start = lastEnd.Add(-OverlapWindows * WindowSize)
	}
	if !start.Before(end) {
		start = end.Add(-WindowSize)
	}
	return Window{Start: start, End: end}
}

// Page 是一页拉取结果。
type Page struct {
	// Entries 是本页账本条目（可以为空）。
	Entries []LedgerEntry
	// HasMore 表示上游还有下一页。
	HasMore bool
}

// PageSource 按窗口 + offset 拉取一页账本（实现方通常是网关的用量查询接口）。
//
// offset **不持久化**：每次调用都是「本轮第 N 页」的 N，从 0 开始；不要把它写成游标。
type PageSource interface {
	FetchPage(ctx context.Context, w Window, offset, limit int) (Page, error)
}

// WindowStatus 是窗口处理结果。
type WindowStatus string

const (
	// WindowOK 表示窗口内数据已全部落库。
	WindowOK WindowStatus = "ok"
	// WindowBlocked 表示 20 页上限内没拉完（积压），本轮已回滚。
	WindowBlocked WindowStatus = "blocked"
	// WindowError 表示拉取/写入失败，本轮已回滚。
	WindowError WindowStatus = "error"
)

// WindowRecord 是一条窗口处理记录（落库形态由架构侧 0023 的 reconcile 窗口表决定）。
type WindowRecord struct {
	Start time.Time
	End   time.Time
	// End 落库时按 60s 对齐（水位线口径），见 Rounder 的说明。
	Status     WindowStatus
	Pages      int
	Fetched    int
	Inserted   int
	Reason     string
	RecordedAt time.Time
}

// Watermark 返回用于下一轮水位线的结束时刻（60s 对齐，向下取整）。
func (r WindowRecord) Watermark() time.Time { return r.End.Truncate(WindowSize) }

// WindowStore 持久化窗口结果（LastSuccessEnd 只认 status=ok 的最大 End）。
//
// 为什么窗口状态**不在**账本事务里写：blocked/error 恰好在回滚之后才需要落库，
// 写进同一事务会被一起回滚，等于「出事了却没留下证据」。
type WindowStore interface {
	LastSuccessEnd(ctx context.Context) (time.Time, bool, error)
	Save(ctx context.Context, rec WindowRecord) error
}

// MemoryWindowStore 是进程内实现：0023 的窗口表落地前用它占位（重启丢水位线，首轮重拉 1 窗，
// 由 ledger_id 幂等键兜住，不会重复计数）。
type MemoryWindowStore struct {
	mu      sync.Mutex
	lastEnd time.Time
	records []WindowRecord
}

// NewMemoryWindowStore 创建内存窗口存储。
func NewMemoryWindowStore() *MemoryWindowStore { return &MemoryWindowStore{} }

var _ WindowStore = (*MemoryWindowStore)(nil)

// LastSuccessEnd 实现 WindowStore。
func (m *MemoryWindowStore) LastSuccessEnd(_ context.Context) (time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastEnd.IsZero() {
		return time.Time{}, false, nil
	}
	return m.lastEnd, true, nil
}

// Save 实现 WindowStore：只有 ok 才推进水位线（blocked/error 不推进，下一轮继续补）。
func (m *MemoryWindowStore) Save(_ context.Context, rec WindowRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, rec)
	if rec.Status == WindowOK && rec.Watermark().After(m.lastEnd) {
		m.lastEnd = rec.Watermark()
	}
	return nil
}

// Records 返回已保存的窗口记录（排障与用例断言用）。
func (m *MemoryWindowStore) Records() []WindowRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]WindowRecord(nil), m.records...)
}

// ErrWindowBlocked 表示单轮页数上限用尽仍未拉完：本轮账本写入已回滚，窗口置 blocked 并告警。
type ErrWindowBlocked struct {
	Window Window
	Pages  int
	// Fetched 是本轮已拉到的条数（回滚后不落库，仅供排障）。
	Fetched int
}

func (e *ErrWindowBlocked) Error() string {
	if e == nil {
		return "同步窗口 blocked"
	}
	return fmt.Sprintf("同步窗口 %s 在 %d 页上限内未拉完（已拉 %d 条，本轮已回滚）：积压需扩页上限或人工介入",
		e.Window, e.Pages, e.Fetched)
}

// RoundResult 是一轮同步的结果（也用于日志/排障）。
type RoundResult struct {
	Window   Window
	Pages    int
	Fetched  int
	Inserted int
	Status   WindowStatus
	Reason   string
}

// RounderOptions 是 NewRounder 的入参。
type RounderOptions struct {
	Source  PageSource
	Ledger  LedgerTxBeginner
	Windows WindowStore
	// PageSize 为 0 时用 DefaultPageSize。
	PageSize int
	// MaxPages 为 0 时用 MaxPagesPerRound。
	MaxPages int
	// Now 为 nil 时用 time.Now（用例注入固定时钟）。
	Now    func() time.Time
	Logger *slog.Logger
}

// Rounder 执行一轮「算窗口 → 翻页拉取 → 单事务幂等写入 → 落窗口状态」。
type Rounder struct {
	source   PageSource
	ledger   LedgerTxBeginner
	windows  WindowStore
	pageSize int
	maxPages int
	now      func() time.Time
	log      *slog.Logger
}

// NewRounder 创建同步轮次执行器。
func NewRounder(opts RounderOptions) *Rounder {
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	maxPages := opts.MaxPages
	if maxPages <= 0 {
		maxPages = MaxPagesPerRound
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = logger.Fallback()
	}
	return &Rounder{
		source:   opts.Source,
		ledger:   opts.Ledger,
		windows:  opts.Windows,
		pageSize: pageSize,
		maxPages: maxPages,
		now:      now,
		log:      log,
	}
}

// RunOnce 执行一轮同步。
//
// 事务边界：**整轮一事务**——所有页写在同一个事务里，成功才提交；任何失败或超限整轮回滚。
// 半提交的窗口会让「水位线」与「已落库数据」不一致，下一轮重叠补不回来（漏账）。
//
// blocked 口径：单轮翻页达到上限且上游仍有下一页 → 回滚 + 落 blocked 状态 + ERROR 告警。
// 注意 offset 不持久化，所以积压不会自愈：每轮都从 offset=0 重新翻，仍然撞上限。
// 这正是要告警的原因（扩页上限或人工介入，而不是偷偷记个 offset 假装有进度）。
func (r *Rounder) RunOnce(ctx context.Context) (RoundResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	log := r.log

	lastEnd, hasLast, err := r.windows.LastSuccessEnd(ctx)
	if err != nil {
		return RoundResult{}, fmt.Errorf("读取同步水位线失败：%w", err)
	}
	now := r.now()
	w := WindowFor(lastEnd, now)
	if hasLast {
		// 防御：时钟回拨/水位线在未来时窗口会倒挂。
		if !w.Start.Before(w.End) {
			return RoundResult{}, &SkipError{Op: "上游同步", Reason: "同步窗口为空（时钟回拨或水位线超前）"}
		}
	}
	result := RoundResult{Window: w, Status: WindowOK}

	tx, err := r.ledger.Begin(ctx)
	if err != nil {
		r.saveStatus(ctx, result, WindowError, err.Error())
		return result, err
	}

	offset := 0
	for {
		page, ferr := r.source.FetchPage(ctx, w, offset, r.pageSize)
		if ferr != nil {
			_ = tx.Rollback()
			r.saveStatus(ctx, result, WindowError, ferr.Error())
			return result, fmt.Errorf("拉取同步窗口 %s（offset=%d）失败：%w", w, offset, ferr)
		}
		if len(page.Entries) > 0 {
			n, uerr := tx.Upsert(ctx, page.Entries)
			if uerr != nil {
				_ = tx.Rollback()
				r.saveStatus(ctx, result, WindowError, uerr.Error())
				return result, uerr
			}
			result.Inserted += n
			result.Fetched += len(page.Entries)
			offset += len(page.Entries)
		}
		result.Pages++

		if !page.HasMore {
			break
		}
		if len(page.Entries) == 0 {
			// 空页却声称还有更多：数据源的翻页语义有问题，必须报错而不是死循环。
			_ = tx.Rollback()
			reason := fmt.Sprintf("上游返回空页但标记 has_more（offset=%d）", offset)
			r.saveStatus(ctx, result, WindowError, reason)
			return result, fmt.Errorf("同步窗口 %s：%s", w, reason)
		}
		if result.Pages >= r.maxPages {
			_ = tx.Rollback()
			blocked := &ErrWindowBlocked{Window: w, Pages: result.Pages, Fetched: result.Fetched}
			r.saveStatus(ctx, result, WindowBlocked, blocked.Error())
			// 告警：这是「数据积压」不是「偶发失败」，重试同一轮不会变好。
			log.Error("上游同步窗口 blocked（页数上限）",
				"window", w.String(), "pages", result.Pages, "max_pages", r.maxPages,
				"fetched", result.Fetched, "err", blocked.Error())
			return result, blocked
		}
	}

	if err := tx.Commit(); err != nil {
		r.saveStatus(ctx, result, WindowError, err.Error())
		return result, fmt.Errorf("提交账本事务失败：%w", err)
	}
	r.saveStatus(ctx, result, WindowOK, "")
	log.Info("上游同步窗口完成",
		"window", w.String(), "pages", result.Pages,
		"fetched", result.Fetched, "inserted", result.Inserted)
	return result, nil
}

// saveStatus 落窗口状态（失败只告警：不能因为「记录状态失败」把已经成功的账本事务判成失败）。
func (r *Rounder) saveStatus(ctx context.Context, result RoundResult, status WindowStatus, reason string) {
	result.Status = status
	result.Reason = reason
	rec := WindowRecord{
		Start:      result.Window.Start,
		End:        result.Window.End,
		Status:     status,
		Pages:      result.Pages,
		Fetched:    result.Fetched,
		Inserted:   result.Inserted,
		Reason:     reason,
		RecordedAt: r.now(),
	}
	if err := r.windows.Save(ctx, rec); err != nil {
		r.log.Error("写入同步窗口状态失败", "window", result.Window.String(), "status", status, "err", err)
	}
}

// IsBlocked 判断错误是否为「页数上限导致的 blocked」。
func IsBlocked(err error) bool {
	var b *ErrWindowBlocked
	return errors.As(err, &b)
}
