// Package sync 提供外部模型价格同步（models.dev）与价格变更告警。
// 控制台维护 watchlist（关注项），定时从外部价格源拉取并比对其 prompt/completion
// 价格，首次同步记录基线，后续变化生成 price_change_alerts 告警。
package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 告警状态。
const (
	AlertStatusPending  = "pending"
	AlertStatusResolved = "resolved"
	AlertStatusIgnored  = "ignored"
)

// WatchlistItem 对应 watchlist_items 表一行。PromptPrice/CompletionPrice 为
// 最近一次成功同步时记录的基线价格（nil 表示尚未同步，即待建立基线）。
type WatchlistItem struct {
	ID              int64
	ExternalModelID string
	LocalModelName  string
	AlertOnChange   bool
	LastSyncedAt    *time.Time
	PromptPrice     *float64
	CompletionPrice *float64
	CreatedAt       time.Time
}

// Change 一次价格写入/变化维度（input/output）的变更明细。
type Change struct {
	Old       float64 `json:"old"`
	New       float64 `json:"new"`
	ChangePct float64 `json:"change_pct"` // 百分比，(new-old)/old*100
}

// PriceAlert 对应 price_change_alerts 表一行。Changes 键为 "input"/"output"。
type PriceAlert struct {
	ID              int64
	AdminID         *int64
	WatchlistItemID *int64
	ExternalModelID string
	LocalModelName  string
	Changes         map[string]Change
	Status          string
	DetectedAt      time.Time
	ResolvedAt      *time.Time
}

const watchlistCols = `id, external_model_id, local_model_name, alert_on_change,
	last_synced_at, prompt_price, completion_price, created_at`

const alertCols = `id, admin_id, watchlist_item_id, external_model_id, local_model_name,
	changes, status, detected_at, resolved_at`

// Store 提供 watchlist_items 与 price_change_alerts 的 CRUD 与基线读写。
type Store struct {
	db *sql.DB
}

// NewStore 创建同步存储。
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// scanWatchlist 扫描 watchlist_items 行。
func scanWatchlist(row interface{ Scan(...any) error }) (*WatchlistItem, error) {
	var it WatchlistItem
	var lastRaw sql.NullTime
	var promptRaw, compRaw sql.NullFloat64
	err := row.Scan(&it.ID, &it.ExternalModelID, &it.LocalModelName, &it.AlertOnChange,
		&lastRaw, &promptRaw, &compRaw, &it.CreatedAt)
	if err != nil {
		return nil, err
	}
	if lastRaw.Valid {
		t := lastRaw.Time
		it.LastSyncedAt = &t
	}
	if promptRaw.Valid {
		v := promptRaw.Float64
		it.PromptPrice = &v
	}
	if compRaw.Valid {
		v := compRaw.Float64
		it.CompletionPrice = &v
	}
	return &it, nil
}

// ListWatchlist 查询全部关注项（按 id 升序）。
func (s *Store) ListWatchlist(ctx context.Context) ([]WatchlistItem, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+watchlistCols+` FROM watchlist_items WHERE deleted_at IS NULL ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list watchlist: %w", err)
	}
	defer rows.Close()
	items := make([]WatchlistItem, 0, 8)
	for rows.Next() {
		it, err := scanWatchlist(rows)
		if err != nil {
			return nil, fmt.Errorf("scan watchlist: %w", err)
		}
		items = append(items, *it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// GetWatchlistByExternalID 按 external_model_id 查询；不存在返回 sql.ErrNoRows。
func (s *Store) GetWatchlistByExternalID(ctx context.Context, externalID string) (*WatchlistItem, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+watchlistCols+` FROM watchlist_items WHERE external_model_id = $1 AND deleted_at IS NULL`, externalID)
	return scanWatchlist(row)
}

// UpsertWatchlist 按 external_model_id 幂等插入/更新关注项，返回回填后的完整记录。
// external_model_id 无 DB 唯一约束（软删除后允许重建），改为查询后插入或更新；
// 已软删除的历史行保留审计，新建独立行。
func (s *Store) UpsertWatchlist(ctx context.Context, in *WatchlistItem) (*WatchlistItem, error) {
	existing, err := s.GetWatchlistByExternalID(ctx, in.ExternalModelID)
	switch {
	case err == nil:
		var placeholders = "local_model_name = $1, alert_on_change = $2"
		row := s.db.QueryRowContext(ctx,
			`UPDATE watchlist_items SET `+placeholders+` WHERE id = $3 RETURNING `+watchlistCols,
			in.LocalModelName, in.AlertOnChange, existing.ID)
		return scanWatchlist(row)
	case errors.Is(err, sql.ErrNoRows):
		row := s.db.QueryRowContext(ctx,
			`INSERT INTO watchlist_items(external_model_id, local_model_name, alert_on_change)
			 VALUES($1,$2,$3) RETURNING `+watchlistCols,
			in.ExternalModelID, in.LocalModelName, in.AlertOnChange)
		return scanWatchlist(row)
	default:
		return nil, err
	}
}

// DeleteWatchlist 批量软删除关注项，返回删除数量。
func (s *Store) DeleteWatchlist(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	anyArg := "{" + strings.Join(parts, ",") + "}"
	res, err := s.db.ExecContext(ctx,
		`UPDATE watchlist_items SET deleted_at = now() WHERE id = ANY($1::bigint[])`, anyArg)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UpdateSyncedAt 仅刷新 last_synced_at（价格未变化时调用）。
func (s *Store) UpdateSyncedAt(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE watchlist_items SET last_synced_at = $1 WHERE id = $2`, at, id)
	return err
}

// SetBaseline 更新基线价格并刷新 last_synced_at。
func (s *Store) SetBaseline(ctx context.Context, id int64, prompt, completion float64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE watchlist_items SET prompt_price = $1, completion_price = $2, last_synced_at = $3
		 WHERE id = $4`, prompt, completion, at, id)
	return err
}

// InsertAlert 写入一条价格变更告警（pending），返回回填后的完整告警。
func (s *Store) InsertAlert(ctx context.Context, a *PriceAlert) (*PriceAlert, error) {
	changesRaw, _ := json.Marshal(a.Changes)
	var adminID, watchID any
	if a.AdminID != nil {
		adminID = *a.AdminID
	}
	if a.WatchlistItemID != nil {
		watchID = *a.WatchlistItemID
	}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO price_change_alerts(admin_id, watchlist_item_id, external_model_id,
			local_model_name, changes, status, detected_at)
		 VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+alertCols,
		adminID, watchID, a.ExternalModelID, a.LocalModelName, changesRaw, a.Status, a.DetectedAt)
	return scanAlert(row)
}

// scanAlert 扫描 price_change_alerts 行。
func scanAlert(row interface{ Scan(...any) error }) (*PriceAlert, error) {
	var al PriceAlert
	var adminID, watchID sql.NullInt64
	var changesRaw []byte
	var resolvedAt sql.NullTime
	err := row.Scan(&al.ID, &adminID, &watchID, &al.ExternalModelID, &al.LocalModelName,
		&changesRaw, &al.Status, &al.DetectedAt, &resolvedAt)
	if err != nil {
		return nil, err
	}
	if adminID.Valid {
		v := adminID.Int64
		al.AdminID = &v
	}
	if watchID.Valid {
		v := watchID.Int64
		al.WatchlistItemID = &v
	}
	if len(changesRaw) > 0 {
		_ = json.Unmarshal(changesRaw, &al.Changes)
	}
	if resolvedAt.Valid {
		t := resolvedAt.Time
		al.ResolvedAt = &t
	}
	return &al, nil
}

// ListAlerts 按状态过滤查询告警（status 空表示全部），按 id 倒序。
func (s *Store) ListAlerts(ctx context.Context, status string) ([]PriceAlert, error) {
	query := `SELECT ` + alertCols + ` FROM price_change_alerts`
	args := []any{}
	if status != "" {
		query += ` WHERE status = $1`
		args = append(args, status)
	}
	query += ` ORDER BY id DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	defer rows.Close()
	list := make([]PriceAlert, 0, 8)
	for rows.Next() {
		al, err := scanAlert(rows)
		if err != nil {
			return nil, fmt.Errorf("scan alert: %w", err)
		}
		list = append(list, *al)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// GetAlert 按主键查询告警；不存在返回 sql.ErrNoRows。
func (s *Store) GetAlert(ctx context.Context, id int64) (*PriceAlert, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+alertCols+` FROM price_change_alerts WHERE id = $1`, id)
	return scanAlert(row)
}

// ResolveAlert 将告警置为 resolved 并写 resolved_at。
func (s *Store) ResolveAlert(ctx context.Context, id, adminID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE price_change_alerts SET status = $1, admin_id = $2, resolved_at = now()
		 WHERE id = $3`, AlertStatusResolved, adminID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// IsNoRows 判断查询结果是否为「不存在」。
func IsNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
