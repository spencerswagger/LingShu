package channelprotocol

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/team/llmgateway/internal/pkg/idgen"
)

// cpCols 列出 channel_protocols 表查询使用的列。
const cpCols = `id, name, base_url, protocols, request_headers, models,
	preset_source_id, edited, source_status, created_at, updated_at`

// ErrNameExists 表示渠道协议名称唯一约束冲突（PG 23505）。
var ErrNameExists = errors.New("channel protocol name already exists")

// Store 提供 channel_protocols 表的基础数据访问。
type Store struct {
	db *sql.DB
}

// NewStore 创建渠道协议存储。
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func marshalJSONB(v any) []byte {
	b, _ := json.Marshal(v)
	if b == nil {
		return []byte("{}")
	}
	return b
}

// scanChannelProtocol 将一行扫描到 *ChannelProtocol，解析 JSONB 列。
func scanChannelProtocol(row interface{ Scan(...any) error }) (*ChannelProtocol, error) {
	var cp ChannelProtocol
	var protocolsRaw, headersRaw, modelsRaw []byte
	var status string
	err := row.Scan(&cp.ID, &cp.Name, &cp.BaseURL, &protocolsRaw, &headersRaw,
		&modelsRaw, &cp.PresetSourceID, &cp.Edited, &status, &cp.CreatedAt, &cp.UpdatedAt)
	if err != nil {
		return nil, err
	}
	cp.SourceStatus = status
	if len(protocolsRaw) > 0 {
		if err := json.Unmarshal(protocolsRaw, &cp.Protocols); err != nil {
			return nil, fmt.Errorf("parse protocols: %w", err)
		}
	}
	if len(headersRaw) > 0 {
		if err := json.Unmarshal(headersRaw, &cp.RequestHeaders); err != nil {
			return nil, fmt.Errorf("parse request_headers: %w", err)
		}
	}
	if len(modelsRaw) > 0 {
		if err := json.Unmarshal(modelsRaw, &cp.Models); err != nil {
			return nil, fmt.Errorf("parse models: %w", err)
		}
	}
	return &cp, nil
}

func classifyErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrNameExists
	}
	return err
}

// GetByID 按主键查询；不存在返回 sql.ErrNoRows。
func (s *Store) GetByID(ctx context.Context, id int64) (*ChannelProtocol, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cpCols+` FROM channel_protocols WHERE id=$1 AND deleted_at IS NULL`, id)
	return scanChannelProtocol(row)
}

// GetByName 按名称查询；不存在返回 sql.ErrNoRows。
func (s *Store) GetByName(ctx context.Context, name string) (*ChannelProtocol, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cpCols+` FROM channel_protocols WHERE name=$1 AND deleted_at IS NULL`, name)
	return scanChannelProtocol(row)
}

// List 返回全部渠道协议（按 id 升序）。
func (s *Store) List(ctx context.Context) ([]ChannelProtocol, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cpCols+` FROM channel_protocols WHERE deleted_at IS NULL ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list channel protocols: %w", err)
	}
	defer rows.Close()
	list := make([]ChannelProtocol, 0, 8)
	for rows.Next() {
		cp, err := scanChannelProtocol(rows)
		if err != nil {
			return nil, fmt.Errorf("scan channel protocol: %w", err)
		}
		list = append(list, *cp)
	}
	return list, rows.Err()
}

// ListByPresetSource 返回全部来自预设的协议（preset_source_id 非空）。
func (s *Store) ListByPresetSource(ctx context.Context) ([]ChannelProtocol, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cpCols+` FROM channel_protocols WHERE deleted_at IS NULL AND preset_source_id <> '' ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list preset channel protocols: %w", err)
	}
	defer rows.Close()
	list := make([]ChannelProtocol, 0, 8)
	for rows.Next() {
		cp, err := scanChannelProtocol(rows)
		if err != nil {
			return nil, fmt.Errorf("scan channel protocol: %w", err)
		}
		list = append(list, *cp)
	}
	return list, rows.Err()
}

// execer 抽象 *sql.DB / *sql.Tx 的写入能力，使 Store 的写方法可在事务内复用。
type execer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// WithTx 在单个事务内执行 fn：fn 返回错误则回滚，否则提交。用于预设同步的原子落库。
func (s *Store) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// Insert 插入新渠道协议并返回回填主键后的完整记录。
func (s *Store) Insert(ctx context.Context, cp *ChannelProtocol) (*ChannelProtocol, error) {
	return insertOn(ctx, s.db, cp)
}

// insertOn 在给定执行器（库或事务）上插入。
func insertOn(ctx context.Context, e execer, cp *ChannelProtocol) (*ChannelProtocol, error) {
	if cp.ID == 0 {
		cp.ID = idgen.New()
	}
	if cp.SourceStatus == "" {
		cp.SourceStatus = SourceActive
	}
	row := e.QueryRowContext(ctx,
		`INSERT INTO channel_protocols(id, name, base_url, protocols, request_headers, models,
		        preset_source_id, edited, source_status)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+cpCols,
		cp.ID, cp.Name, cp.BaseURL, marshalJSONB(cp.Protocols),
		marshalJSONB(cp.RequestHeaders), marshalJSONB(cp.Models),
		cp.PresetSourceID, cp.Edited, cp.SourceStatus)
	created, err := scanChannelProtocol(row)
	if err != nil {
		return nil, classifyErr(err)
	}
	return created, nil
}

// Update 全量更新渠道协议可写字段（含 edited/source_status），刷新 updated_at。
func (s *Store) Update(ctx context.Context, cp *ChannelProtocol) error {
	return updateOn(ctx, s.db, cp)
}

// updateOn 在给定执行器（库或事务）上全量更新。
func updateOn(ctx context.Context, e execer, cp *ChannelProtocol) error {
	res, err := e.ExecContext(ctx,
		`UPDATE channel_protocols SET name=$1, base_url=$2, protocols=$3,
		        request_headers=$4, models=$5, preset_source_id=$6, edited=$7, source_status=$8,
		        updated_at=now()
		 WHERE id=$9 AND deleted_at IS NULL`,
		cp.Name, cp.BaseURL, marshalJSONB(cp.Protocols),
		marshalJSONB(cp.RequestHeaders), marshalJSONB(cp.Models),
		cp.PresetSourceID, cp.Edited, cp.SourceStatus, cp.ID)
	if err != nil {
		return classifyErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateSourceStatus 仅更新来源状态（ACTIVE/STALE）。
func (s *Store) UpdateSourceStatus(ctx context.Context, id int64, status string) error {
	return updateSourceStatusOn(ctx, s.db, id, status)
}

// updateSourceStatusOn 在给定执行器（库或事务）上更新来源状态。
func updateSourceStatusOn(ctx context.Context, e execer, id int64, status string) error {
	res, err := e.ExecContext(ctx,
		`UPDATE channel_protocols SET source_status=$1, updated_at=now() WHERE id=$2 AND deleted_at IS NULL`,
		status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Delete 软删除单条渠道协议。不存在返回 sql.ErrNoRows。
func (s *Store) Delete(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE channel_protocols SET deleted_at=now(), updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CountChannelRefs 统计引用某渠道协议的渠道数（删除前置校验用）。
func (s *Store) CountChannelRefs(ctx context.Context, id int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM channels WHERE channel_protocol_id=$1 AND deleted_at IS NULL`, id).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count channel refs: %w", err)
	}
	return n, nil
}

// initialSyncedConfigKey 是「首次启动已同步预设」标记在 sys_configs 中的键。
const initialSyncedConfigKey = "channel_protocols.initial_synced"

// InitialSynced 判断首次启动同步是否已执行过（跨重启持久，AC-12）。
func (s *Store) InitialSynced(ctx context.Context) (bool, error) {
	var v []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM sys_configs WHERE key=$1`, initialSyncedConfigKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read initial-synced flag: %w", err)
	}
	return true, nil
}

// MarkInitialSynced 落「首次启动已同步」标记（幂等，ON CONFLICT DO NOTHING）。
func (s *Store) MarkInitialSynced(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sys_configs(key, value) VALUES ($1, 'true'::jsonb) ON CONFLICT (key) DO NOTHING`,
		initialSyncedConfigKey)
	if err != nil {
		return fmt.Errorf("mark initial-synced: %w", err)
	}
	return nil
}
