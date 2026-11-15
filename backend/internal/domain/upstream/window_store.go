package upstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/team/llmgateway/internal/pkg/dbx"
)

// 窗口状态落 0023 的 reconcile_windows（架构侧定稿，列名/约束以迁移文件为准）：
//
//	window_start TIMESTAMPTZ PK · window_end TIMESTAMPTZ（CHECK = start + 60s）·
//	local_count/upstream_count · local_tokens/upstream_tokens ·
//	status TEXT CHECK (ok|alarm|error|blocked) · alarm_streak · detail JSONB · checked_at
//
// 本包只写 status / detail / checked_at：
// 四个计数列是「灵枢账单 vs 账本」的对账结果，由对账作业写；同步轮次拿它当计数会污染对账口径。
// detail 用 `||` 合并而不是覆盖，避免把对账作业写进去的分组明细抹掉。
const (
	// ReconcileWindowsTable 是窗口表名（0023 口径）。
	ReconcileWindowsTable = "reconcile_windows"
	// windowDetailPhase 是 detail 里标记写入方来源的键。
	windowDetailPhase = "sync"
)

// SQLWindowStore 是 WindowStore 的 PostgreSQL 实现（reconcile_windows）。
type SQLWindowStore struct {
	db *sql.DB
}

// NewSQLWindowStore 创建窗口存储（db 为 nil 时各方法返回错误，不 panic）。
func NewSQLWindowStore(db *sql.DB) *SQLWindowStore { return &SQLWindowStore{db: db} }

var _ WindowStore = (*SQLWindowStore)(nil)

// LastSuccessEnd 返回最后一次成功窗口的结束时刻。
//
// 只认 status=ok：alarm 也算「已拉完但对不上账」，进度可以推进；blocked/error 没拉完，
// 推进水位线就等于把没落库的数据永久跳过。
func (s *SQLWindowStore) LastSuccessEnd(ctx context.Context) (time.Time, bool, error) {
	if s == nil || s.db == nil {
		return time.Time{}, false, fmt.Errorf("窗口存储未装配数据库连接")
	}
	var end sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT max(window_end) FROM `+ReconcileWindowsTable+` WHERE status IN ('ok','alarm')`).Scan(&end)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("读取窗口水位线失败：%w", err)
	}
	if !end.Valid {
		return time.Time{}, false, nil
	}
	return end.Time, true, nil
}

// BlockedWindows 返回仍未补平的 blocked 窗口（按起点升序）。
func (s *SQLWindowStore) BlockedWindows(ctx context.Context, limit int) ([]WindowRecord, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("窗口存储未装配数据库连接")
	}
	if limit <= 0 {
		limit = blockedProbeLimit
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT window_start, window_end, checked_at, detail
		   FROM `+ReconcileWindowsTable+`
		  WHERE status = 'blocked'
		  ORDER BY window_start
		  LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("读取 blocked 窗口失败：%w", err)
	}
	defer rows.Close()

	var out []WindowRecord
	for rows.Next() {
		var (
			rec     WindowRecord
			detail  []byte
			checked time.Time
		)
		if err := rows.Scan(&rec.Start, &rec.End, &checked, &detail); err != nil {
			return nil, fmt.Errorf("扫描 blocked 窗口失败：%w", err)
		}
		rec.Status = WindowBlocked
		rec.RecordedAt = checked
		rec.Reason = decodeWindowDetail(detail)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 blocked 窗口失败：%w", err)
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
	detail, err := json.Marshal(map[string]any{
		"phase":    windowDetailPhase,
		"pages":    rec.Pages,
		"fetched":  rec.Fetched,
		"inserted": rec.Inserted,
		"reason":   rec.Reason,
	})
	if err != nil {
		return fmt.Errorf("序列化窗口明细失败：%w", err)
	}
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO `+ReconcileWindowsTable+`
		    (window_start, window_end, status, detail, checked_at)
		 VALUES ($1, $2, $3, $4::jsonb, $5)
		 ON CONFLICT (window_start) DO UPDATE
		    SET window_end = EXCLUDED.window_end,
		        status     = EXCLUDED.status,
		        detail     = `+ReconcileWindowsTable+`.detail || EXCLUDED.detail,
		        checked_at = EXCLUDED.checked_at`,
		rec.Start, rec.End, string(rec.Status), string(detail), recordedAt); err != nil {
		return fmt.Errorf("写入窗口行（%s→%s，%s）失败：%w",
			rec.Start.Format(time.RFC3339), rec.End.Format(time.RFC3339), rec.Status, err)
	}
	// 成功补平后，把这段区间里仍挂着 blocked 的旧窗口置 ok（同事务，避免「假告警」长期留存）。
	if rec.Status == WindowOK && !rec.ClearFrom.IsZero() {
		if _, err := ex.ExecContext(ctx,
			`UPDATE `+ReconcileWindowsTable+`
			    SET status = 'ok', checked_at = $3
			  WHERE status = 'blocked'
			    AND window_start >= $1
			    AND window_start < $2`,
			rec.ClearFrom, rec.End, recordedAt); err != nil {
			return fmt.Errorf("清理 blocked 窗口失败：%w", err)
		}
	}
	return nil
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
