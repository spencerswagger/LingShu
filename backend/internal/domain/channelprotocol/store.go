package channelprotocol

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/team/llmgateway/internal/pkg/idgen"
)

// cpCols 列出 channel_protocols 表查询使用的列。
const cpCols = `id, name, base_url, protocols, auth, request_headers, models,
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
	var protocolsRaw, authRaw, headersRaw, modelsRaw []byte
	var status string
	err := row.Scan(&cp.ID, &cp.Name, &cp.BaseURL, &protocolsRaw, &authRaw, &headersRaw,
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
	if len(authRaw) > 0 {
		if err := json.Unmarshal(authRaw, &cp.Auth); err != nil {
			return nil, fmt.Errorf("parse auth: %w", err)
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

// GetByPresetSourceID 按预设来源 ID 查询；不存在返回 sql.ErrNoRows。
func (s *Store) GetByPresetSourceID(ctx context.Context, sourceID string) (*ChannelProtocol, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cpCols+` FROM channel_protocols WHERE preset_source_id=$1 AND deleted_at IS NULL`, sourceID)
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

// Insert 插入新渠道协议并返回回填主键后的完整记录。
func (s *Store) Insert(ctx context.Context, cp *ChannelProtocol) (*ChannelProtocol, error) {
	if cp.ID == 0 {
		cp.ID = idgen.New()
	}
	if cp.SourceStatus == "" {
		cp.SourceStatus = SourceActive
	}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO channel_protocols(id, name, base_url, protocols, auth, request_headers, models,
		        preset_source_id, edited, source_status)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING `+cpCols,
		cp.ID, cp.Name, cp.BaseURL, marshalJSONB(cp.Protocols), marshalJSONB(cp.Auth),
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
	res, err := s.db.ExecContext(ctx,
		`UPDATE channel_protocols SET name=$1, base_url=$2, protocols=$3, auth=$4,
		        request_headers=$5, models=$6, preset_source_id=$7, edited=$8, source_status=$9,
		        updated_at=now()
		 WHERE id=$10 AND deleted_at IS NULL`,
		cp.Name, cp.BaseURL, marshalJSONB(cp.Protocols), marshalJSONB(cp.Auth),
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
	res, err := s.db.ExecContext(ctx,
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

// sourceIDList 把来源 ID 列表拼成 SQL 数组字面量（沿用仓库既有 ANY 写法）。
func sourceIDList(ids []string) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, "'"+strings.ReplaceAll(id, "'", "''")+"'")
	}
	return "{" + strings.Join(parts, ",") + "}"
}
