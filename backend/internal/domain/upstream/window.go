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

// 同步窗口口径（已批准，见 docs/designs/2026-11-15-v2-p1-data-contract.md §6.2；
// offset 持久化与补拉口径按 2026-11-15 裁定 F9 修订）：
//   - 窗口长度固定 60s（与 0023 的 reconcile_windows 同一口径，库里有 CHECK 断言）；
//   - 迟到数据用「重叠 2 窗」吸收：本轮拉取区间 = [上次成功结束 - 120s, now)；
//   - 翻页**显式按 occurred_at ASC, ledger_id ASC**（数据源契约，见 PageSource），offset 从 0 起；
//   - offset **持久化**在 reconcile_windows.scroll_offset（按「本轮扫描起点所在窗口」那一行记）：
//     blocked 后下一轮从该 window_start + 该 offset 继续拉，直到把窗口清成 ok。
//     幂等靠 ledger_entries.ledger_id 的 ON CONFLICT 兜住，所以「重拉一段」不会重复计账；
//     持久化 offset 只省掉「已确认那段的重复拉取」，不承担正确性（正确性在幂等键上）。
//     上一版是「offset 不持久化 + 靠人工介入」，结果是积压窗口永远清不完（每轮都从 0 翻、
//     每轮都撞上限）——这就是 F9 要改掉的那条；
//   - 单轮 ≤ 20 页 / ≤ 4000 条（两个上限同源，任一命中即 blocked）：超限记 `blocked` + 告警，
//     **不引入人工介入**，下一轮自动从 scroll_offset 继续补拉，直到清零；
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
	// MaxRowsPerRound 是单轮条数上限（4000 = 20 页 × 200 条，两个上限同源；F9 保留该上限）。
	MaxRowsPerRound = MaxPagesPerRound * DefaultPageSize
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
// **排序契约（F9）**：实现方必须显式 `ORDER BY occurred_at ASC, ledger_id ASC`，再按
// offset/limit 切页。不写 ORDER BY 靠数据库默认顺序翻页，同一条记录可能出现在两页里
// （重复被幂等键吃掉，不算错）或**完全不出现**（漏账，而且是静默的）——offset 一旦
// 持久化，漏页就不会自愈，所以排序必须写死在这里。
//
// offset 语义：本轮相对「扫描起点窗口」的第 N 条，从 0 开始；持久化在
// reconcile_windows.scroll_offset，blocked 后下一轮从这里继续。
type PageSource interface {
	FetchPage(ctx context.Context, w Window, offset, limit int) (Page, error)
}

// WindowStatus 是窗口处理结果（取值域逐字等于 0023 的 CHECK：ok/alarm/error/blocked/skipped）。
//
// 同步轮次只写 ok / blocked / error；alarm / skipped 由对账服务写（D4），
// 但 Validate 必须认全部五个值——同一个 Store 同时承担两边写入。
type WindowStatus string

const (
	// WindowOK 表示窗口内数据已全部落库。
	WindowOK WindowStatus = "ok"
	// WindowBlocked 表示单轮上限（4000 条 / 20 页）内没拉完（积压），本轮已回滚，
	// 下一轮从 scroll_offset 继续补拉。
	WindowBlocked WindowStatus = "blocked"
	// WindowError 表示拉取/写入失败，本轮已回滚。与 blocked 的区别：
	// error 是「这次没跑成」，blocked 是「该拉的数据比单轮上限多」。
	WindowError WindowStatus = "error"
	// WindowAlarm 表示对账 token 偏差超阈（对账服务写）。
	WindowAlarm WindowStatus = "alarm"
	// WindowSkipped 表示本窗不参与比对（账本关闭/降级；对账服务写）。
	// 与 blocked 区分：skipped 是「按口径不比」，blocked 是「该比但没拉完」。
	WindowSkipped WindowStatus = "skipped"
)

// WindowRecord 是一条窗口行（落库到 0023 的 reconcile_windows）。
//
// 只写 status / detail / checked_at / scroll_offset，**不写** local_count / upstream_* 这些对账计数：
// 那是「灵枢账单 vs 账本」比对的结果，由对账服务在每窗结束时统一写（见 CountWriter /
// reconcile.go）；同步轮次拿它当计数会污染对账口径（一个是「拉了多少条」，一个是「两边各多少笔」）。
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
	// ScrollOffset 是该行对应窗口的下次补拉 offset（F9：持久化，ok 后归 0）。
	// 语义：本轮扫描起点 = 本行 window_start 时，下一次从第 ScrollOffset 条开始拉。
	ScrollOffset int64
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
	case WindowOK, WindowBlocked, WindowError, WindowAlarm, WindowSkipped:
	default:
		return fmt.Errorf("窗口状态 %q 不在取值域（ok/alarm/error/blocked/skipped）", r.Status)
	}
	if r.ScrollOffset < 0 {
		return fmt.Errorf("窗口行 scroll_offset 为负（%d）", r.ScrollOffset)
	}
	return nil
}

// WindowStore 持久化窗口状态（0023 的 reconcile_windows）。
//
// 水位线口径：只认 status=ok 的最大 window_end；blocked/error 不推进水位线（下一轮继续补）。
type WindowStore interface {
	// LastSuccessEnd 返回最后一次成功窗口的结束时刻（无记录时 ok=false）。
	LastSuccessEnd(ctx context.Context) (time.Time, bool, error)
	// PendingWindows 返回仍未补平的行（status IN ('blocked','error')，按 window_start 升序），
	// 带上各自的 scroll_offset 作为续拉位置。
	//
	// 为什么把 error 也算进来：error 行可能已经有「已提交的页」（本切片一页一事务），
	// 它的位置在水位线之后就不会再被重叠区间覆盖 —— 只认 blocked 会让这段数据永久缺口。
	PendingWindows(ctx context.Context, limit int) ([]WindowRecord, error)
	// Save 独立写一条窗口行（blocked/error 用：此时账本事务已回滚，只能另写一条）。
	Save(ctx context.Context, rec WindowRecord) error
	// SaveTx 在同一事务里写窗口行（成功路径用，契约 §6.2）。
	SaveTx(ctx context.Context, ex dbx.Execer, rec WindowRecord) error
}

// WindowCounts 是一次对账的落库形态（D4：四个计数列由对账服务在每窗结束时统一写）。
//
// 与 WindowRecord 的分工：
//   - WindowRecord 由**同步轮次**写（status/scroll_offset/detail：拉了多少、补到哪）；
//   - WindowCounts 由**对账服务**写（四个计数列 + 判定结果：两边各多少、是否偏差）。
//
// 两者写同一张表的不同列，各有各的调用点，别混用一个结构（混用必踩「谁覆盖谁」）。
type WindowCounts struct {
	Start time.Time
	End   time.Time
	// LocalCount / LocalTokens 是灵枢侧（billing_records）的笔数与三段 token 合计。
	LocalCount  int64
	LocalTokens int64
	// UpstreamCount / UpstreamTokens 是账本侧（ledger_entries）的笔数与 in+out 合计。
	UpstreamCount  int64
	UpstreamTokens int64
	// Status 是判定结果：ok / alarm / skipped（blocked / error 由同步轮次持有，对账不得覆盖）。
	Status WindowStatus
	// AlarmStreak 是连续告警窗数（>=3 升级审计；ok 归 0）。
	AlarmStreak int16
	// Detail 是兜底明细（分组/原因/偏差率）。**判定必须落在计数列上**，
	// detail 只为排障；查历史不许靠解析它。
	Detail map[string]any
	// CheckedAt 写 checked_at。
	CheckedAt time.Time
}

// Validate 校验对账落库形状。
func (c WindowCounts) Validate() error {
	if c.Start.IsZero() || c.End.IsZero() {
		return fmt.Errorf("对账窗口缺少起止时刻")
	}
	if !c.End.Equal(c.Start.Add(WindowSize)) {
		return fmt.Errorf("对账窗口必须是整 60s（%s → %s）",
			c.Start.Format(time.RFC3339), c.End.Format(time.RFC3339))
	}
	switch c.Status {
	case WindowOK, WindowAlarm, WindowSkipped:
	default:
		return fmt.Errorf("对账状态 %q 非法（只允许 ok/alarm/skipped；blocked/error 归同步轮次）", c.Status)
	}
	if c.LocalCount < 0 || c.UpstreamCount < 0 || c.LocalTokens < 0 || c.UpstreamTokens < 0 {
		return fmt.Errorf("对账计数出现负数：%+v", c)
	}
	return nil
}

// CountWriter 写对账计数（实现方是 SQLWindowStore；对账服务只依赖这个窄接口）。
type CountWriter interface {
	SaveCounts(ctx context.Context, c WindowCounts) error
}

// MemoryWindowStore 是进程内实现，作单测替身（重启丢状态，真实运行请用 SQLWindowStore）。
type MemoryWindowStore struct {
	mu      sync.Mutex
	lastEnd time.Time
	records []WindowRecord
	counts  []WindowCounts
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

// PendingWindows 实现 WindowStore。
func (m *MemoryWindowStore) PendingWindows(_ context.Context, limit int) ([]WindowRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []WindowRecord
	for _, rec := range m.records {
		if rec.Status != WindowBlocked && rec.Status != WindowError {
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
			if (r.Status == WindowBlocked || r.Status == WindowError) &&
				!r.Start.Before(rec.ClearFrom) && r.Start.Before(rec.End) {
				r.Status = WindowOK
				r.ScrollOffset = 0
			}
		}
	}
	return nil
}

// SaveCounts 实现 CountWriter：内存实现把计数挂在对应窗口行上（用例断言用）。
func (m *MemoryWindowStore) SaveCounts(_ context.Context, c WindowCounts) error {
	if err := c.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.records {
		if m.records[i].Start.Equal(c.Start) {
			m.records[i].Status = c.Status
			m.counts = append(m.counts, c)
			return nil
		}
	}
	m.records = append(m.records, WindowRecord{Start: c.Start, End: c.End, Status: c.Status})
	m.counts = append(m.counts, c)
	return nil
}

// Counts 返回已写入的对账结果（用例断言用）。
func (m *MemoryWindowStore) Counts() []WindowCounts {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]WindowCounts(nil), m.counts...)
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
	// ScrollOffset 是下一轮继续拉的位置（持久化进 reconcile_windows.scroll_offset）。
	ScrollOffset int64
}

func (e *ErrWindowBlocked) Error() string {
	if e == nil {
		return "同步窗口 blocked"
	}
	return fmt.Sprintf("同步窗口 %s 在单轮上限（%d 页 / %d 条）内未拉完（已拉 %d 条），本轮已回滚；"+
		"下一轮从 scroll_offset=%d 继续补拉，直到清零（不引入人工介入）",
		e.Window, MaxPagesPerRound, MaxRowsPerRound, e.Fetched, e.ScrollOffset)
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
	// ScanStart 是本轮扫描起点的窗口（60s 对齐）：blocked/error 行记在这一行上，
	// 它的 scroll_offset 就是下次补拉的位置。
	ScanStart time.Time
	// ResumeOffset 是本轮开始时的 offset（0 = 从头拉；>0 = 上一轮 blocked 留下的位置）。
	ResumeOffset int64
	// Empty 表示本轮**一条都没拉到**（Fetched == Inserted == 0）。
	//
	// 这个字段存在的理由：以前「空轮」与「正常轮」返回值完全一样（0 行 + nil），
	// 后台 ticker 每分钟静默空转，账本/计数列永远是 NULL 也没人知道。
	// 现在空轮会打一条 WARN（sync.round empty）并置位，调用方可以据此做指标/告警。
	Empty bool
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
	// LedgerOrder 是上游账本排序能力的**实测结论**（见 ProbeLedgerOrder）。
	// 零值 = LedgerOrderUnverified（未证过）：此时本轮直接记 blocked，不翻页。
	// 依据 F9 裁定：证不了稳定升序就不许清零该窗，也不许自己造排序。
	LedgerOrder LedgerOrder
	// LedgerOrderReason 是探测依据（进窗 detail，便于排障）。
	LedgerOrderReason string
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
	order    LedgerOrder
	orderWhy string
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
		order:    opts.LedgerOrder,
		orderWhy: opts.LedgerOrderReason,
	}
}

// RunOnce 执行一轮同步。
//
// ── 事务边界（本切片按 F9 修订，与契约 §6.2「整轮一事务」不同，属**有意变更**）──────
// 每一页一个事务：`Upsert(本页) + 写扫描起点窗口行的 scroll_offset` 同事务提交。
// 为什么放弃「整轮一事务」：offset 一旦要持久化当断点，整轮回滚就与断点语义冲突——
// 回滚把本页数据擦掉了，断点却记着「拉到第 N 条」，下一轮从 N 继续 = 把前 N 条永久跳过
// （静默漏账，比 blocked 更糟）。要么不做断点，要么让断点**代表已落库的进度**，这里选后者。
// 代价：单轮中途失败会留下「已提交的若干页 + 未完成的窗口状态」，但窗口状态仍是 blocked/error
// （水位线不前进），下一轮重叠重拉且 ledger_id 幂等，不会重复计账也不会漏。
//
// blocked 口径（F9）：单轮达到上限（4000 条 / 20 页）且上游仍有下一页 → 落 blocked + ERROR 告警，
// 并把**本次到达的 offset** 持久化进扫描起点窗口行的 scroll_offset。
// 下一轮从那个 window_start + scroll_offset 继续拉，直到清零——积压会自愈，不需要人工介入。
// 幂等前提：账本写入按 ledger_id ON CONFLICT DO NOTHING，所以「同一条被拉两次」不会重复计账。
func (r *Rounder) RunOnce(ctx context.Context) (RoundResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	log := r.log

	lastEnd, hasLast, err := r.windows.LastSuccessEnd(ctx)
	if err != nil {
		return RoundResult{}, fmt.Errorf("读取同步水位线失败：%w", err)
	}
	blocked, err := r.windows.PendingWindows(ctx, blockedProbeLimit)
	if err != nil {
		return RoundResult{}, fmt.Errorf("读取待补拉窗口失败：%w", err)
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
	// 扫描起点窗口：blocked/error 行与 scroll_offset 都记在这一行上（F9）。
	// 为什么不是记在 saved（最新那一格）：offset 是「从扫描起点往后数第 N 条」，
	// 只有和起点窗口绑在一起才有意义；记在最新窗口上，下一轮从最新窗口 + 旧 offset 起步
	// 会跳过中间那段（漏账），这是最容易写错的一处。
	scanStart := w.Start.Truncate(WindowSize)
	result.ScanStart = scanStart
	// ClearFrom 必须能盖住扫描起点那一行：w.Start 可能落在窗口中间（比 scanStart 晚），
	// 用 w.Start 当清理下界会漏掉自己刚写的 blocked 行（永远清不掉）。
	if scanStart.Before(clearFrom) {
		clearFrom = scanStart
	}

	// 断点续拉：从最旧 blocked 窗口留下的 offset 起（该窗口之外的部分还没有 offset 语义）。
	var resumeOffset int64
	if len(blocked) > 0 {
		resumeOffset = blocked[0].ScrollOffset
	}
	result.ResumeOffset = resumeOffset

	// 能力闸门（F9「先证后写」）：排序能力未证实/已证否 → 本窗直接 blocked，**不翻页**。
	// 为什么不在翻页中途发现再退：翻页一旦跑过，offset 已写进库，等于用一个不可信的
	// 排序「证明」了进度；宁可停在 0，把「上游支持前不可清零」写在窗上。
	if r.order != LedgerOrderSupported {
		order := r.order
		reason := r.orderWhy
		if reason == "" {
			reason = "排序能力未经证实"
		}
		unproven := &ErrLedgerOrderUnproven{Window: w, Order: order, Reason: reason}
		r.saveStatus(ctx, result, WindowBlocked, unproven.Error(), 0, scanStart)
		log.Error("上游同步跳过并记 blocked：账本排序能力未证实（本窗不可清零）",
			"window", w.String(), "order", order.String(), "reason", reason)
		return result, unproven
	}

	offset := int(resumeOffset)
	for {
		page, ferr := r.source.FetchPage(ctx, w, offset, r.pageSize)
		if ferr != nil {
			r.saveStatus(ctx, result, WindowError, ferr.Error(), int64(offset), scanStart)
			return result, fmt.Errorf("拉取同步窗口 %s（offset=%d）失败：%w", w, offset, ferr)
		}
		result.Pages++

		if len(page.Entries) > 0 {
			next := offset + len(page.Entries)
			// 一页一事务：本页账本 + 扫描起点的 offset 一起提交（断点必须代表「已落库」）。
			tx, terr := r.ledger.Begin(ctx)
			if terr != nil {
				r.saveStatus(ctx, result, WindowError, terr.Error(), int64(offset), scanStart)
				return result, terr
			}
			n, uerr := tx.Upsert(ctx, page.Entries)
			if uerr != nil {
				_ = tx.Rollback()
				r.saveStatus(ctx, result, WindowError, uerr.Error(), int64(offset), scanStart)
				return result, uerr
			}
			if serr := r.windows.SaveTx(ctx, tx.Execer(), WindowRecord{
				Start:        scanStart,
				End:          scanStart.Add(WindowSize),
				Status:       WindowBlocked, // 未跑完前不把它标成 ok：水位线只能由完整轮次推进。
				Pages:        result.Pages,
				Fetched:      result.Fetched + len(page.Entries),
				Inserted:     result.Inserted + n,
				Reason:       "分页进行中（本页已提交，下一轮从 scroll_offset 续拉）",
				ScrollOffset: int64(next),
				RecordedAt:   r.now(),
			}); serr != nil {
				_ = tx.Rollback()
				r.saveStatus(ctx, result, WindowError, serr.Error(), int64(offset), scanStart)
				return result, fmt.Errorf("写分页断点失败（本页已回滚）：%w", serr)
			}
			if cerr := tx.Commit(); cerr != nil {
				r.saveStatus(ctx, result, WindowError, cerr.Error(), int64(offset), scanStart)
				return result, fmt.Errorf("提交分页事务失败：%w", cerr)
			}
			result.Inserted += n
			result.Fetched += len(page.Entries)
			offset = next
		}

		if !page.HasMore {
			break
		}
		if len(page.Entries) == 0 {
			// 空页却声称还有更多：数据源的翻页语义有问题，必须报错而不是死循环。
			reason := fmt.Sprintf("上游返回空页但标记 has_more（offset=%d）", offset)
			r.saveStatus(ctx, result, WindowError, reason, int64(offset), scanStart)
			return result, fmt.Errorf("同步窗口 %s：%s", w, reason)
		}
		// 两个上限同源：页数（20）与条数（4000）。条数上限要单独判，因为 page_size 可配，
		// 调大 page_size 时「20 页 = 4000 条」不再成立，只判页数会偷偷放大单轮负载。
		if result.Pages >= r.maxPages || result.Fetched >= MaxRowsPerRound {
			blockedErr := &ErrWindowBlocked{
				Window: w, Pages: result.Pages, Fetched: result.Fetched, ScrollOffset: int64(offset),
			}
			// 持久化 offset 是 blocked 与「失败」的区别：进度要留给下一轮，否则积压永不清零。
			r.saveStatus(ctx, result, WindowBlocked, blockedErr.Error(), int64(offset), scanStart)
			log.Error("上游同步窗口 blocked（单轮上限）",
				"window", w.String(), "pages", result.Pages, "max_pages", r.maxPages,
				"max_rows", MaxRowsPerRound, "fetched", result.Fetched,
				"scroll_offset", offset, "err", blockedErr.Error())
			return result, blockedErr
		}
	}

	// 空轮可观测（F9/P1 复验项）：0 行 + nil 不许静默。
	// 首轮无水位线时空是正常的（上游本来就没数据），但稳态下每分钟都空说明上游没数据、
	// 窗口没覆盖到、或凭据/端点错了——必须能在日志里看见，而不是「什么都没发生」。
	if result.Fetched == 0 && result.Inserted == 0 {
		result.Empty = true
		log.Warn("本轮同步未拉到任何账本条目（空轮）",
			"window", w.String(), "scan_start", scanStart.Format(time.RFC3339),
			"pages", result.Pages, "resume_offset", result.ResumeOffset,
			"hint", "上游无数据 / 窗口未覆盖 / 凭据或端点配置错误")
	}

	// 收尾：把最新窗口置 ok（推进水位线），并把 [ClearFrom, End) 里仍挂 blocked/error 的行
	// 一并置 ok + offset 归零 —— 这段区间本轮已拉完，不该留假告警。
	tx, err := r.ledger.Begin(ctx)
	if err != nil {
		r.saveStatus(ctx, result, WindowError, err.Error(), int64(offset), scanStart)
		return result, err
	}
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
		r.saveStatus(ctx, result, WindowError, err.Error(), int64(offset), scanStart)
		return result, fmt.Errorf("提交窗口收尾事务失败：%w", err)
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

// saveStatus 落 blocked/error 状态（账本页已提交或未开始；本行写入失败只告警，
// 不能因为「记录状态失败」把已经落库的页判成另一种失败）。
//
// 记在**扫描起点窗口**那一行上（不是最新窗口）：scroll_offset 的含义是「从扫描起点往后数第 N 条」，
// 只有绑定起点窗口才能被下一轮正确续拉。
func (r *Rounder) saveStatus(ctx context.Context, result RoundResult, status WindowStatus, reason string,
	offset int64, scanStart time.Time) {
	result.Status = status
	result.Reason = reason
	start := scanStart
	if start.IsZero() {
		start = result.Window.Start.Truncate(WindowSize)
	}
	rec := WindowRecord{
		Start:        start,
		End:          start.Add(WindowSize),
		Status:       status,
		Pages:        result.Pages,
		Fetched:      result.Fetched,
		Inserted:     result.Inserted,
		Reason:       reason,
		ScrollOffset: offset,
		RecordedAt:   r.now(),
	}
	if err := r.windows.Save(ctx, rec); err != nil {
		r.log.Error("写入同步窗口状态失败", "window", rec.Start.Format(time.RFC3339), "status", status, "err", err)
	}
}

// IsBlocked 判断错误是否为「页数上限导致的 blocked」。
func IsBlocked(err error) bool {
	var b *ErrWindowBlocked
	return errors.As(err, &b)
}
