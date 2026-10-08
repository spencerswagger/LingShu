// Package model 提供对外模型（external_models）的存储与 CRUD：对外名称 + 售价定价
// （五段 rates + 模型级 time_config/context_tiers）。渠道内部模型与成本定价在
// channel 包（channel_models）管理，此处不涉及。
//
// 数据模型对齐 db/migrations/0001_init.sql 中 external_models 表。
package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/idgen"
)

// ExternalModel 对应 external_models 表的一行。
// SaleRates 为售价（五段单价）；TimeConfig/ContextTiers 可空，空则运行时回落全局。
type ExternalModel struct {
	ID           int64
	ExternalName string
	Description  string
	Enabled      bool
	SaleRates    billing.Rates
	TimeConfig   *billing.TimeCoeffConfig
	ContextTiers []billing.TierRule
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RateKeys 是计价费率 JSONB 必须包含的五个键。
var RateKeys = []string{"input", "output", "cache_read", "cache_write", "reasoning"}

// cols 列出 external_models 表查询使用的全部列。
const cols = `id, external_name, description, enabled, sale_rates, time_config, context_tiers, created_at, updated_at`

// scanExternal 将一行扫描到 *ExternalModel，解析 sale_rates/time_config/context_tiers JSONB。
func scanExternal(row interface{ Scan(...any) error }) (*ExternalModel, error) {
	var m ExternalModel
	var saleRaw, timeRaw, tiersRaw []byte
	err := row.Scan(&m.ID, &m.ExternalName, &m.Description, &m.Enabled, &saleRaw,
		&timeRaw, &tiersRaw, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if len(saleRaw) > 0 {
		if err := json.Unmarshal(saleRaw, &m.SaleRates); err != nil {
			return nil, fmt.Errorf("parse sale_rates: %w", err)
		}
	}
	if len(timeRaw) > 0 && string(timeRaw) != "null" {
		var tc billing.TimeCoeffConfig
		if err := json.Unmarshal(timeRaw, &tc); err != nil {
			return nil, fmt.Errorf("parse time_config: %w", err)
		}
		m.TimeConfig = &tc
	}
	if len(tiersRaw) > 0 && string(tiersRaw) != "null" {
		if err := json.Unmarshal(tiersRaw, &m.ContextTiers); err != nil {
			return nil, fmt.Errorf("parse context_tiers: %w", err)
		}
	}
	return &m, nil
}

// Store 提供 external_models 表的基础数据访问。
type Store struct {
	db *sql.DB
}

// NewStore 创建对外模型存储，db 为 pgx stdlib 连接池。
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// GetByID 按主键查询；不存在返回 sql.ErrNoRows。
func (s *Store) GetByID(ctx context.Context, id int64) (*ExternalModel, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM external_models WHERE id = $1 AND deleted_at IS NULL`, id)
	return scanExternal(row)
}

// GetByName 按对外名称查询（忽略 enabled），供唯一性检查与更新使用；不存在返回 sql.ErrNoRows。
func (s *Store) GetByName(ctx context.Context, name string) (*ExternalModel, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM external_models WHERE external_name = $1 AND deleted_at IS NULL`, name)
	return scanExternal(row)
}

// GetByExternalName 返回指定对外名称的启用模型（供路由/计费使用）。
// 无启用模型返回 sql.ErrNoRows。
func (s *Store) GetByExternalName(ctx context.Context, externalName string) (*ExternalModel, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cols+` FROM external_models WHERE external_name = $1 AND enabled = true AND deleted_at IS NULL LIMIT 1`, externalName)
	return scanExternal(row)
}

// List 按 enabled 过滤查询，未指定时返回全部。
func (s *Store) List(ctx context.Context, enabled *bool) ([]ExternalModel, error) {
	query := `SELECT ` + cols + ` FROM external_models WHERE deleted_at IS NULL`
	args := []any{}
	if enabled != nil {
		query += ` AND enabled = $` + strconv.Itoa(len(args)+1)
		args = append(args, *enabled)
	}
	query += ` ORDER BY id ASC`
	return s.list(ctx, query, args...)
}

func (s *Store) list(ctx context.Context, query string, args ...any) ([]ExternalModel, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list external models: %w", err)
	}
	defer rows.Close()
	models := make([]ExternalModel, 0, 8)
	for rows.Next() {
		m, err := scanExternal(rows)
		if err != nil {
			return nil, fmt.Errorf("scan external model: %w", err)
		}
		models = append(models, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return models, nil
}

// ErrNameExists 表示对外名称唯一约束冲突（PG 23505）。
var ErrNameExists = errors.New("external model name already exists")

// Insert 插入对外模型并返回回填主键后的完整记录；external_name 冲突返回 ErrNameExists。
func (s *Store) Insert(ctx context.Context, m *ExternalModel) (*ExternalModel, error) {
	if m.ID == 0 {
		m.ID = idgen.New()
	}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO external_models(id, external_name, description, enabled, sale_rates, time_config, context_tiers)
		 VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+cols,
		m.ID, m.ExternalName, m.Description, m.Enabled, jsonB(m.SaleRates),
		nullableJSON(m.TimeConfig), nullableJSON(m.ContextTiers))
	created, err := scanExternal(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrNameExists
		}
		return nil, err
	}
	return created, nil
}

// Update 全量更新对外模型可变字段（不含 id/created_at），并刷新 updated_at。
func (s *Store) Update(ctx context.Context, m *ExternalModel) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE external_models SET external_name=$1, description=$2, enabled=$3,
		        sale_rates=$4, time_config=$5, context_tiers=$6, updated_at=now()
		 WHERE id=$7`,
		m.ExternalName, m.Description, m.Enabled, jsonB(m.SaleRates),
		nullableJSON(m.TimeConfig), nullableJSON(m.ContextTiers), m.ID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrNameExists
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ApplySaleRates 仅更新售价与 updated_at（一键应用 models.dev 参考价），不动其它字段。
func (s *Store) ApplySaleRates(ctx context.Context, id int64, rates billing.Rates) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE external_models SET sale_rates=$1, updated_at=now() WHERE id=$2`, jsonB(rates), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// BatchDelete 批量软删除外部模型，返回删除行数。
func (s *Store) BatchDelete(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	anyArg := "{" + strings.Join(parts, ",") + "}"
	res, err := s.db.ExecContext(ctx,
		`UPDATE external_models SET deleted_at = now() WHERE id = ANY($1::bigint[])`, anyArg)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// jsonB 将值序列化为 JSON，供 NOT NULL JSONB 列写入；空值写 '{}' 而非 null。
func jsonB(v any) []byte {
	b, _ := json.Marshal(v)
	if b == nil {
		b = []byte("{}")
	}
	return b
}

// nullableJSON 将可空字段序列化：指向 nil 时写 NULL（回落全局），否则写 JSON。
func nullableJSON(v any) any {
	if v == nil {
		return nil
	}
	// 空指针/空切片视为未提供。
	if s, ok := v.(*billing.TimeCoeffConfig); ok && s == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return nil
	}
	return json.RawMessage(b)
}

// ErrNotFound 表示对外模型不存在（路由层据此判定「模型未配置或已停用」）。
var ErrNotFound = errors.New("external model not found")
