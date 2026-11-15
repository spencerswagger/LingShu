package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/team/llmgateway/internal/pkg/dbx"
	"github.com/team/llmgateway/internal/pkg/logger"
)

// 同步窗口口径（已批准，见 docs/designs/2026-11-15-v2-p1-data-contract.md §6.2）：
//   - 窗口长度固定 60s（与 0023 的 reconcile_windows 同一口径，库里有 CHECK 断言）；
//   - 迟到数据用「重叠 2 窗」吸收：本轮拉取区间 = [上次成功结束 - 120s, now)；
//   - offset 翻页但**不持久化**：每轮从 offset=0 重新翻，翻不完不许把 offset 存起来当进度
//     （存量 offset 一旦漂移会把同一条账算两次、或永久跳过一条）；
//   - 单轮 ≤ 20 页；20 页还没拉完 → 本轮置 `blocked` 并告警（积压要么扩页上限，要么人工介入）；
//   - 整轮一个事务：账本条目与窗口行**同事务**写（契约 §6.2「随后同事务写窗口行」）。
const (
	// WindowSize 是同步窗口长度（库层 CHECK 写死为 60s）。
	WindowSize = 60 * time.Second
	// OverlapWindows 是重叠窗口数（2 窗 = 120s）。
	OverlapWindows = 2
	// DefaultPageSize 是单页条数。
	DefaultPageSize = 200
	// MaxPagesPerRound 是单轮页数上限；超出即 blocked。
	MaxPagesPerRound = 20
	// blockedProbeLimit 是一轮开始时读取「未补平 blocked 窗口」的条数上限。
	blockedProbeLimit = 50
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

// WindowStatus 是窗口处理结果（取值域逐字等于 0023 的 CHECK）。
type WindowStatus string

const (
	// WindowOK 表示窗口内数据已全部落库。
	WindowOK WindowStatus = "ok"
	// WindowBlocked 表示 20 页上限内没拉完（积压），本轮已回滚。
	WindowBlocked WindowStatus = "blocked"
	// WindowError 表示拉取/写入失败，本轮已回滚。
	WindowError WindowStatus = "error"
)

// WindowRecord 是一条窗口行（落库到 0023 的 reconcile_windows）。
//
// 只写 status / detail / checked_at，**不写** local_count / upstream_* 这些对账计数：
// 那是「灵枢账单 vs 账本」比对的结果，由对账作业写；同步轮次拿它当计数会污染对账口径。
type WindowRecord struct {
	// Start 是 60s 对齐的窗口起点（reconcile_windows.window_start 主键）。
	Start time.Time
	// End 必须等于 Start + 60s（库层 CHECK，不许绕过）。
	End time.Time
	// Status 是窗口状态。
	Status WindowStatus
	// Pages / Fetched / Inserted 是本轮统计，写进 detail（不进对账计数列）。
	Pages    int
	Fetched  int
	Inserted int
	// Reason 是 blocked/error 的原文原因（写进 detail）。
	Reason string
	// RecordedAt 写 checked_at。
	RecordedAt time.Time
	// ClearFrom 仅在 status=ok 时有意义：把 [ClearFrom, End) 内仍为 blocked 的窗口一并置 ok
	// ——这段区间本轮已重拉并成功补平，不该留假告警。
	ClearFrom time.Time
}

// Watermark 返回用于下一轮水位线的结束时刻（60s 对齐，向下取整）。
func (r WindowRecord) Watermark() time.Time { return r.End.Truncate(WindowSize) }

// Validate 校验落库形状：窗口必须整 60s，且状态在 0023 的 CHECK 取值域内。
func (r WindowRecord) Validate() error {
	if r.Start.IsZero() || r.End.IsZero() {
		return fmt.Errorf("窗口行缺少起止时刻")
	}
	if !r.End.Equal(r.Start.Add(WindowSize)) {
		return fmt.Errorf("窗口行必须是整 60s（%s → %s），否则 reconcile_windows 的 CHECK 会拒绝",
			r.Start.Format(time.RFC3339), r.End.Format(time.RFC3339))
	}
	switch r.Status {
	case WindowOK, WindowBlocked, WindowError:
	default:
		return fmt.Errorf("窗口状态 %q 不在取值域（ok/blocked/error）", r.Status)
	}
	return nil
}

// WindowStore 持久化窗口状态（0023 的 reconcile_windows）。
//
// 水位线口径：只认 status=ok 的最大 window_end；blocked/error 不推进水位线（下一轮继续补）。
type WindowStore interface {
	// LastSuccessEnd 返回最后一次成功窗口的结束时刻（无记录时 ok=false）。
	LastSuccessEnd(ctx context.Context) (time.Time, bool, error)
	// BlockedWindows 返回仍未补平的 blocked 窗口（按 window_start 升序）。
	// 重启后要靠它「不静默」：积压要重拉且要告警，不许当没发生。
	BlockedWindows(ctx context.Context, limit int) ([]WindowRecord, error)
	// Save 独立写一条窗口行（blocked/error 用：此时账本事务已回滚，只能另写一条）。
	Save(ctx context.Context, rec WindowRecord) error
	// SaveTx 在同一事务里写窗口行（成功路径用，契约 §6.2）。
	SaveTx(ctx context.Context, ex dbx.Execer, rec WindowRecord) error
}

// MemoryWindowStore 是进程内实现，作单测替身（重启丢状态，真实运行请用 SQLWindowStore）。
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

// BlockedWindows 实现 WindowStore。
func (m *MemoryWindowStore) BlockedWindows(_ context.Context, limit int) ([]WindowRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []WindowRecord
	for _, rec := range m.records {
		if rec.Status != WindowBlocked {
			continue
		}
		out = append(out, rec)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Save 实现 WindowStore。
func (m *MemoryWindowStore) Save(ctx context.Context, rec WindowRecord) error {
	return m.SaveTx(ctx, nil, rec)
}

// SaveTx 实现 WindowStore：ex 被忽略（内存实现没有事务概念）。
func (m *MemoryWindowStore) SaveTx(_ context.Context, _ dbx.Execer, rec WindowRecord) error {
	if err := rec.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// 同窗口覆盖（ON CONFLICT (window_start) 的语义），保留首次顺序之外的覆盖行为。
	replaced := false
	for i := range m.records {
		if m.records[i].Start.Equal(rec.Start) {
			m.records[i] = rec
			replaced = true
			break
		}
	}
	if !replaced {
		m.records = append(m.records, rec)
	}
	if rec.Status == WindowOK && rec.Watermark().After(m.lastEnd) {
		m.lastEnd = rec.Watermark()
	}
	if rec.Status == WindowOK && !rec.ClearFrom.IsZero() {
		for i := range m.records {
			r := &m.records[i]
			if r.Status == WindowBlocked && !r.Start.Before(rec.ClearFrom) && r.Start.Before(rec.End) {
				r.Status = WindowOK
			}
		}
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
	// Window 是本轮**拉取区间**（含 120s 重叠，可能长于 60s）。
	Window Window
	// Saved 是本轮落库的 60s 窗口（水位线口径），blocked/error 时同样落这一格。
	Saved Window
	Pages int
	// Fetched / Inserted 是本轮拉取与真实新增条数。
	Fetched  int
	Inserted int
	Status   WindowStatus
	Reason   string
	// BlockedPending 是本轮开始时仍未补平的 blocked 窗口数（>0 表示上一轮/重启前的积压还在）。
	BlockedPending int
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

// Rounder 执行一轮「算窗口 → 翻页拉取 → 单事务幂等写入 + 落窗口行」。
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
// 事务边界：**整轮一事务**——所有页 + 窗口行写在同一个事务里，成功才提交；任何失败或超限整轮回滚。
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
	blocked, err := r.windows.BlockedWindows(ctx, blockedProbeLimit)
	if err != nil {
		return RoundResult{}, fmt.Errorf("读取 blocked 窗口失败：%w", err)
	}

	now := r.now()
	w := WindowFor(lastEnd, now)
	result := RoundResult{Window: w, Status: WindowOK, BlockedPending: len(blocked)}

	// 积压重拉：blocked 窗口可能比水位线更早（重启后没有水位线时尤其明显），
	// 只拉「水位线-120s」会永远盖不到它们 → 必须把起点前移到最旧的 blocked 窗口。
	clearFrom := w.Start
	if len(blocked) > 0 {
		oldest := blocked[0].Start
		for _, rec := range blocked[1:] {
			if rec.Start.Before(oldest) {
				oldest = rec.Start
			}
		}
		if oldest.Before(clearFrom) {
			clearFrom = oldest
			w.Start = oldest
			result.Window = w
		}
		// 不是静默重拉：重启后积压还在就要看得见。
		log.Warn("存在未补平的 blocked 窗口，本轮重拉该区间",
			"blocked", len(blocked), "oldest", oldest.Format(time.RFC3339),
			"window", w.String())
	}
	if hasLast {
		// 防御：时钟回拨/水位线在未来时窗口会倒挂。
		if !w.Start.Before(w.End) {
			return RoundResult{}, &SkipError{Op: "上游同步", Reason: "同步窗口为空（时钟回拨或水位线超前）"}
		}
	}

	saved := savedWindowFor(w)
	result.Saved = saved

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
			blockedErr := &ErrWindowBlocked{Window: w, Pages: result.Pages, Fetched: result.Fetched}
			r.saveStatus(ctx, result, WindowBlocked, blockedErr.Error())
			// 告警：这是「数据积压」不是「偶发失败」，重试同一轮不会变好。
			log.Error("上游同步窗口 blocked（页数上限）",
				"window", w.String(), "pages", result.Pages, "max_pages", r.maxPages,
				"fetched", result.Fetched, "err", blockedErr.Error())
			return result, blockedErr
		}
	}

	// 成功路径：窗口行与账本**同事务**写（契约 §6.2）。
	okRec := WindowRecord{
		Start:      saved.Start,
		End:        saved.End,
		Status:     WindowOK,
		Pages:      result.Pages,
		Fetched:    result.Fetched,
		Inserted:   result.Inserted,
		RecordedAt: r.now(),
		ClearFrom:  clearFrom,
	}
	if err := r.windows.SaveTx(ctx, tx.Execer(), okRec); err != nil {
		_ = tx.Rollback()
		log.Error("写同步窗口行失败，本轮已回滚", "window", saved.String(), "err", err)
		return result, fmt.Errorf("写同步窗口行失败（本轮已回滚）：%w", err)
	}
	if err := tx.Commit(); err != nil {
		r.saveStatus(ctx, result, WindowError, err.Error())
		return result, fmt.Errorf("提交账本事务失败：%w", err)
	}
	result.Status = WindowOK
	log.Info("上游同步窗口完成",
		"window", w.String(), "saved", saved.String(), "pages", result.Pages,
		"fetched", result.Fetched, "inserted", result.Inserted)
	return result, nil
}

// savedWindowFor 把拉取区间收敛成落库的 60s 窗口（reconcile_windows 的 CHECK 要求整 60s）。
//
// 取「拉取终点向下取整」那一窗：它代表本轮最新进度，水位线 = 它的 window_end。
// 更早的窗口由以前的轮次写过行；本轮重拉（重叠 120s）覆盖到的不一致由 ClearFrom 修正。
func savedWindowFor(w Window) Window {
	end := w.End.Truncate(WindowSize)
	if !end.After(w.Start) {
		end = w.Start.Truncate(WindowSize).Add(WindowSize)
	}
	return Window{Start: end.Add(-WindowSize), End: end}
}

// saveStatus 落 blocked/error 状态（此时账本事务已回滚，只能另写一条；失败只告警，
// 不能因为「记录状态失败」把已经回滚的轮次判成另一种失败）。
func (r *Rounder) saveStatus(ctx context.Context, result RoundResult, status WindowStatus, reason string) {
	result.Status = status
	result.Reason = reason
	saved := result.Saved
	if saved.End.IsZero() {
		saved = savedWindowFor(result.Window)
	}
	rec := WindowRecord{
		Start:      saved.Start,
		End:        saved.End,
		Status:     status,
		Pages:      result.Pages,
		Fetched:    result.Fetched,
		Inserted:   result.Inserted,
		Reason:     reason,
		RecordedAt: r.now(),
	}
	if err := r.windows.Save(ctx, rec); err != nil {
		r.log.Error("写入同步窗口状态失败", "window", saved.String(), "status", status, "err", err)
	}
}

// IsBlocked 判断错误是否为「页数上限导致的 blocked」。
func IsBlocked(err error) bool {
	var b *ErrWindowBlocked
	return errors.As(err, &b)
}
