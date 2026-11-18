package upstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/pkg/dbx"
)

// 窗口状态落 0023 的 reconcile_windows（架构侧定稿，列名/约束以迁移文件为准）：
//
//	window_start TIMESTAMPTZ PK · window_end TIMESTAMPTZ（CHECK = start + 60s）·
//	local_count/upstream_count · local_tokens/upstream_tokens ·
//	status TEXT CHECK (ok|alarm|error|blocked|skipped) · alarm_streak ·
//	scroll_offset BIGINT（补拉断点）· detail JSONB · checked_at
//
// 本包只写 status / detail / checked_at / scroll_offset：
// 四个计数列是「灵枢账单 vs 账本」的对账结果，由对账服务在每窗结束时统一写（D4，见 reconcile.go）；
// 同步轮次拿它当计数会污染对账口径。
// detail 用 `||` 合并而不是覆盖，避免把对账服务写进去的分组明细抹掉。
const (
	// ReconcileWindowsTable 是窗口表名（0023 口径）。
	ReconcileWindowsTable = "reconcile_windows"
	// windowDetailPhase 是 detail 里标记写入方来源的键（同步轮次）。
	windowDetailPhase = "sync"
	// windowDetailPhaseReconcile 是对账服务写 detail 时的来源标记。
	windowDetailPhaseReconcile = "reconcile"
)

// SQLWindowStore 是 WindowStore 的 PostgreSQL 实现（reconcile_windows）。
type SQLWindowStore struct {
	db *sql.DB
}

// NewSQLWindowStore 创建窗口存储（db 为 nil 时各方法返回错误，不 panic）。
func NewSQLWindowStore(db *sql.DB) *SQLWindowStore { return &SQLWindowStore{db: db} }

var _ WindowStore = (*SQLWindowStore)(nil)
var _ CountWriter = (*SQLWindowStore)(nil)

// LastSuccessEnd 返回最后一次成功窗口的结束时刻。
//
// 只认 status=ok：alarm 也算「已拉完但对不上账」，进度可以推进；blocked/error 没拉完，
// 推进水位线就等于把没落库的数据永久跳过。
//
// ⚠️ 还必须只认**同步轮次写过的行**（`detail ? 'fetched'`）：对账服务现在会在「同步没跑过」
// 的窗口上自行建行（SaveCounts 用 upsert），那些行同样是 status='ok'/'skipped'。
// 若把它们算进水位线，同步下一轮就会从「对账刚建的窗」之后开始拉 —— 中间那段账本从来没拉过，
// 却因为水位线越过它而永久丢失（这是比漏账更难发现的一类数据丢失）。
// 同步轮次每行必写 `fetched`（见 save 的 detail），对账不写这个键，所以它是「同步所有」的可靠标记；
// 不用 `phase` 判：对账会把自己的 phase 合并进同步行，那个键会变。
func (s *SQLWindowStore) LastSuccessEnd(ctx context.Context) (time.Time, bool, error) {
	if s == nil || s.db == nil {
		return time.Time{}, false, fmt.Errorf("窗口存储未装配数据库连接")
	}
	var end sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT max(window_end) FROM `+ReconcileWindowsTable+`
		  WHERE status IN ('ok','alarm') AND detail ? 'fetched'`).Scan(&end)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("读取窗口水位线失败：%w", err)
	}
	if !end.Valid {
		return time.Time{}, false, nil
	}
	return end.Time, true, nil
}

// PendingWindows 返回仍未补平的行（blocked / error，按起点升序），并带上 scroll_offset。
//
// 两个状态合在一起查：error 行可能已经有「已提交的页」（一页一事务），
// 只认 blocked 会让那段数据永久缺口（它已经在水位线之后，不会再被重叠区间覆盖）。
func (s *SQLWindowStore) PendingWindows(ctx context.Context, limit int) ([]WindowRecord, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("窗口存储未装配数据库连接")
	}
	if limit <= 0 {
		limit = blockedProbeLimit
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT window_start, window_end, scroll_offset, checked_at, detail, status
		   FROM `+ReconcileWindowsTable+`
		  WHERE status IN ('blocked','error')
		  ORDER BY window_start
		  LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("读取待补拉窗口失败：%w", err)
	}
	defer rows.Close()

	var out []WindowRecord
	for rows.Next() {
		var (
			rec     WindowRecord
			detail  []byte
			checked time.Time
			status  string
		)
		if err := rows.Scan(&rec.Start, &rec.End, &rec.ScrollOffset, &checked, &detail, &status); err != nil {
			return nil, fmt.Errorf("扫描待补拉窗口失败：%w", err)
		}
		rec.Status = WindowStatus(status)
		rec.RecordedAt = checked
		rec.Reason = decodeWindowDetail(detail)
		rec.LedgerCursor = decodeWindowCursor(detail)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历待补拉窗口失败：%w", err)
	}
	return out, nil
}

// Save 独立写一条窗口行（blocked/error 用：账本事务此时已回滚）。
func (s *SQLWindowStore) Save(ctx context.Context, rec WindowRecord) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("窗口存储未装配数据库连接")
	}
	return s.save(ctx, s.db, rec)
}

// SaveTx 在给定事务里写窗口行（成功路径，与账本同事务）。
func (s *SQLWindowStore) SaveTx(ctx context.Context, ex dbx.Execer, rec WindowRecord) error {
	if ex == nil {
		return fmt.Errorf("窗口存储未拿到事务执行器")
	}
	return s.save(ctx, ex, rec)
}

func (s *SQLWindowStore) save(ctx context.Context, ex dbx.Execer, rec WindowRecord) error {
	if err := rec.Validate(); err != nil {
		return err
	}
	recordedAt := rec.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = time.Now()
	}
	detailMap := map[string]any{
		"phase":    windowDetailPhase,
		"pages":    rec.Pages,
		"fetched":  rec.Fetched,
		"inserted": rec.Inserted,
		"reason":   rec.Reason,
	}
	// 只有 cursor feed 模式才写；空串不写，避免把已有的断点游标覆盖成空。
	if c := strings.TrimSpace(rec.LedgerCursor); c != "" {
		detailMap["ledger_cursor"] = c
		// 游标的写入时刻：LastLedgerCursor 靠它判「哪一行的游标更新」。
		// 不能按 window_start 排序 —— blocked 断点行可能落在比上次成功行更早的窗口里。
		detailMap["ledger_cursor_at"] = recordedAt.UTC().Format(time.RFC3339Nano)
	}
	detail, err := json.Marshal(detailMap)
	if err != nil {
		return fmt.Errorf("序列化窗口明细失败：%w", err)
	}
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO `+ReconcileWindowsTable+`
		    (window_start, window_end, status, scroll_offset, detail, checked_at)
		 VALUES ($1, $2, $3, $4, $5::jsonb, $6)
		 ON CONFLICT (window_start) DO UPDATE
		    SET window_end    = EXCLUDED.window_end,
		        status        = EXCLUDED.status,
		        scroll_offset = EXCLUDED.scroll_offset,
		        detail        = `+ReconcileWindowsTable+`.detail || EXCLUDED.detail,
		        checked_at    = EXCLUDED.checked_at`,
		rec.Start, rec.End, string(rec.Status), rec.ScrollOffset, string(detail), recordedAt); err != nil {
		return fmt.Errorf("写入窗口行（%s→%s，%s）失败：%w",
			rec.Start.Format(time.RFC3339), rec.End.Format(time.RFC3339), rec.Status, err)
	}
	// 成功补平后，把这段区间里仍挂着 blocked 的旧窗口置 ok（同事务，避免「假告警」长期留存）。
	if rec.Status == WindowOK && !rec.ClearFrom.IsZero() {
		if _, err := ex.ExecContext(ctx,
			`UPDATE `+ReconcileWindowsTable+`
			    SET status = 'ok', scroll_offset = 0, checked_at = $3
			  WHERE status IN ('blocked','error')
			    AND window_start >= $1
			    AND window_start < $2`,
			rec.ClearFrom, rec.End, recordedAt); err != nil {
			return fmt.Errorf("清理 blocked 窗口失败：%w", err)
		}
	}
	return nil
}

// SaveCounts 写对账的四个计数列 + 判定结果（D4：计数列的唯一写入方）。
//
// 三件事必须一起做（分开写会出现「计数新、状态旧」或反过来的半套状态）：
//  1. 四个计数列 + status + alarm_streak + checked_at；
//  2. detail 用 `||` 合并（同步轮次写的 pages/fetched 不抹掉），并写入对账自己的偏差率；
//  3. **不碰 scroll_offset**：那是同步轮次的断点，对账写它等于把补拉位置重置。
//
// 防呆：blocked / error 由同步轮次持有，对账服务不许覆盖（覆盖 = 把「该拉没拉完」洗成 ok，
// 漏账从此隐身）。冲突分支的 `WHERE w.status NOT IN ('blocked','error')` 表达这条；
// 影响 0 行时返回 ErrWindowNotReconcilable，让调用方知道这一窗本轮没算成。
//
// ⚠️ 必须是 **upsert**（2026-11-16 修，P0 半成品）：同步轮次只在自己真的拉过数据的窗口上建行，
// 而 OLD 实现是 UPDATE-only —— 于是「同步没跑/降级/刚重启」的时间段**一行都没有**，
// 对账每轮算完写不进去、静默返回 ErrWindowNotReconcilable，四个计数列永远 NULL：
// 漏账在这条路径上是完全不可见的（正是裁定要防的事）。
// 现在：有行就更新（受 blocked/error 保护），没行就**由对账建行**（ls/up 两侧计数照样落）。
// 内存实现 MemoryWindowStore.SaveCounts 本来就是建行的，两边语义由此对齐。
func (s *SQLWindowStore) SaveCounts(ctx context.Context, c WindowCounts) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("窗口存储未装配数据库连接")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	checkedAt := c.CheckedAt
	if checkedAt.IsZero() {
		checkedAt = time.Now()
	}
	detail := map[string]any{"phase": windowDetailPhaseReconcile}
	for k, v := range c.Detail {
		detail[k] = v
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("序列化对账明细失败：%w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO `+ReconcileWindowsTable+` AS w
		        (window_start, window_end, local_count, local_tokens,
		         upstream_count, upstream_tokens, status, alarm_streak, detail, checked_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10)
		 ON CONFLICT (window_start) DO UPDATE
		    SET window_end      = EXCLUDED.window_end,
		        local_count     = EXCLUDED.local_count,
		        local_tokens    = EXCLUDED.local_tokens,
		        upstream_count  = EXCLUDED.upstream_count,
		        upstream_tokens = EXCLUDED.upstream_tokens,
		        status          = EXCLUDED.status,
		        alarm_streak    = EXCLUDED.alarm_streak,
		        detail          = w.detail || EXCLUDED.detail,
		        checked_at      = EXCLUDED.checked_at
		  WHERE w.status NOT IN ('blocked','error')`,
		c.Start, c.End, c.LocalCount, c.LocalTokens, c.UpstreamCount, c.UpstreamTokens,
		string(c.Status), int16(c.AlarmStreak), string(raw), checkedAt)
	if err != nil {
		return fmt.Errorf("写入对账计数失败（%s）：%w", c.Start.Format(time.RFC3339), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取对账行数失败：%w", err)
	}
	if n == 0 {
		return ErrWindowNotReconcilable
	}
	return nil
}

// ErrWindowNotReconcilable 表示该窗口行当前由同步轮次持有（blocked/error），对账不得覆盖。
var ErrWindowNotReconcilable = errors.New("窗口处于 blocked/error，对账本轮跳过（等同步补平）")

// WindowStates 读取 [from, to) 内各窗口的当前状态（对账前先看，避免覆盖同步轮次的状态）。
func (s *SQLWindowStore) WindowStates(ctx context.Context, from, to time.Time) (map[time.Time]WindowStatus, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("窗口存储未装配数据库连接")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT window_start, status FROM `+ReconcileWindowsTable+`
		  WHERE window_start >= $1 AND window_start < $2`, from, to)
	if err != nil {
		return nil, fmt.Errorf("读取窗口状态失败：%w", err)
	}
	defer rows.Close()
	out := make(map[time.Time]WindowStatus)
	for rows.Next() {
		var (
			start  time.Time
			status string
		)
		if err := rows.Scan(&start, &status); err != nil {
			return nil, fmt.Errorf("扫描窗口状态失败：%w", err)
		}
		out[start] = WindowStatus(status)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历窗口状态失败：%w", err)
	}
	return out, nil
}

// LastAlarmStreak 返回某窗口之前最近一次告警的连续窗数（无告警返回 0）。
func (s *SQLWindowStore) LastAlarmStreak(ctx context.Context, before time.Time) (int16, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("窗口存储未装配数据库连接")
	}
	var streak sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT alarm_streak FROM `+ReconcileWindowsTable+`
		  WHERE window_start < $1
		  ORDER BY window_start DESC
		  LIMIT 1`, before).Scan(&streak)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取上一窗的告警连击失败：%w", err)
	}
	if !streak.Valid {
		return 0, nil
	}
	return int16(streak.Int64), nil
}

// LastLedgerCursor 返回最近一条带 ledger_cursor 的窗口行的游标（cursor feed 断点）。
//
// 只认详情里的 `ledger_cursor` 键：对账服务建的行没有该键，不会被误当成同步进度。
func (s *SQLWindowStore) LastLedgerCursor(ctx context.Context) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("窗口存储未装配数据库连接")
	}
	var cursor sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT detail->>'ledger_cursor' FROM `+ReconcileWindowsTable+`
		  WHERE detail ? 'ledger_cursor'
		    AND COALESCE(detail->>'ledger_cursor','') <> ''
		  ORDER BY (NULLIF(detail->>'ledger_cursor_at',''))::timestamptz DESC NULLS LAST,
		           window_start DESC
		  LIMIT 1`).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取账本续读游标失败：%w", err)
	}
	if !cursor.Valid || strings.TrimSpace(cursor.String) == "" {
		return "", false, nil
	}
	return cursor.String, true, nil
}

// decodeWindowCursor 从 detail 里取回 cursor feed 的续读游标（取不到返回空串）。
func decodeWindowCursor(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	c, _ := m["ledger_cursor"].(string)
	return c
}

// decodeWindowDetail 从 detail 里取回上次记录的原因（排障用；取不到不算错）。
func decodeWindowDetail(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	reason, _ := m["reason"].(string)
	return reason
}
