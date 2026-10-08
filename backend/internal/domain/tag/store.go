// Package tag 提供语义标签（KV 对集合）的存储、CRUD 与严格包含匹配。
//
// 数据模型对齐 db/migrations/0001_init.sql 中 semantic_tags 表。
package tag

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
	"github.com/team/llmgateway/internal/pkg/idgen"
)

// Tag 对应 semantic_tags 表的一行。雪花 ID 以字符串序列化。
type Tag struct {
	ID          int64             `json:"ID,string"`
	Name        string            `json:"Name"`
	Description string            `json:"Description"`
	KVPairs     map[string]string `json:"KVPairs"`
	Enabled     bool              `json:"Enabled"`
	CreatedBy   int64             `json:"CreatedBy,string"` // 创建者 user_id；0 表示系统/无用户场景（admin 注入）
	CreatedAt   time.Time         `json:"CreatedAt"`
}

// cols 列出 semantic_tags 表查询使用的全部列，保持各查询一致。
const cols = `id, name, description, kv_pairs, enabled, created_by, created_at`

// scanTag 将一行扫描到 *Tag。
func scanTag(row interface{ Scan(...any) error }) (*Tag, error) {
	var t Tag
	var kvRaw []byte
	var createdBy sql.NullInt64
	err := row.Scan(&t.ID, &t.Name, &t.Description, &kvRaw, &t.Enabled, &createdBy, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	if createdBy.Valid {
		t.CreatedBy = createdBy.Int64
	}
	if len(kvRaw) > 0 {
		if err := json.Unmarshal(kvRaw, &t.KVPairs); err != nil {
			return nil, fmt.Errorf("parse kv_pairs: %w", err)
		}
	}
	return &t, nil
}

// nullableID 将 0 视为 NULL（created_by 可空），否则返回 id。
func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// Store 提供 semantic_tags 表的基础数据访问。
type Store struct {
	db *sql.DB
}

// NewStore 创建标签存储，db 为 pgx stdlib 连接池。
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// GetByID 按主键查询；不存在返回 sql.ErrNoRows。
func (s *Store) GetByID(ctx context.Context, id int64) (*Tag, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cols+` FROM semantic_tags WHERE id = $1 AND deleted_at IS NULL`, id)
	return scanTag(row)
}

// GetByName 按名称查询；不存在返回 sql.ErrNoRows。
func (s *Store) GetByName(ctx context.Context, name string) (*Tag, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cols+` FROM semantic_tags WHERE name = $1 AND deleted_at IS NULL`, name)
	return scanTag(row)
}

// List 按 enabled 过滤查询，未指定时返回全部。
func (s *Store) List(ctx context.Context, enabled *bool) ([]Tag, error) {
	query := `SELECT ` + cols + ` FROM semantic_tags WHERE deleted_at IS NULL`
	args := []any{}
	if enabled != nil {
		query += ` AND enabled = $` + strconv.Itoa(len(args)+1)
		args = append(args, *enabled)
	}
	query += ` ORDER BY id ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	defer rows.Close()

	tags := make([]Tag, 0, 8)
	for rows.Next() {
		t, err := scanTag(rows)
		if err != nil {
			return nil, fmt.Errorf("scan tag: %w", err)
		}
		tags = append(tags, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tags, nil
}

// ListByIDs 按主键批量查询未删除标签。
func (s *Store) ListByIDs(ctx context.Context, ids []int64) ([]Tag, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	anyArg := "{" + strings.Join(parts, ",") + "}"
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cols+` FROM semantic_tags WHERE id = ANY($1::bigint[]) AND deleted_at IS NULL`, anyArg)
	if err != nil {
		return nil, fmt.Errorf("list tags by ids: %w", err)
	}
	defer rows.Close()
	tags := make([]Tag, 0, len(ids))
	for rows.Next() {
		t, err := scanTag(rows)
		if err != nil {
			return nil, err
		}
		tags = append(tags, *t)
	}
	return tags, rows.Err()
}

// ErrNameExists 表示标签名称唯一约束冲突（PG 23505）。
var ErrNameExists = errors.New("tag name already exists")

// Insert 插入标签并返回回填主键后的完整记录，name 冲突返回 ErrNameExists。
func (s *Store) Insert(ctx context.Context, t *Tag) (*Tag, error) {
	if t.ID == 0 {
		t.ID = idgen.New()
	}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO semantic_tags(id, name, description, kv_pairs, enabled, created_by)
		 VALUES($1,$2,$3,$4,$5,$6) RETURNING `+cols,
		t.ID, t.Name, t.Description, jsonB(t.KVPairs), t.Enabled, nullableID(t.CreatedBy))
	created, err := scanTag(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrNameExists
		}
		return nil, err
	}
	return created, nil
}

// Update 全量更新标签可变字段（不含 id/created_by/created_at）。name 冲突返回 ErrNameExists。
func (s *Store) Update(ctx context.Context, t *Tag) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE semantic_tags SET name=$1, description=$2, kv_pairs=$3, enabled=$4 WHERE id=$5`,
		t.Name, t.Description, jsonB(t.KVPairs), t.Enabled, t.ID)
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

// ErrTagInUse 表示标签正被令牌引用，无法删除。
var ErrTagInUse = errors.New("tag is referenced by tokens")

// CountTokenRefs 统计未软删除令牌中引用给定标签的数量（业务级引用检查，替代 PG 23503）。
func (s *Store) CountTokenRefs(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	anyArg := "{" + strings.Join(parts, ",") + "}"
	var n int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM tokens WHERE tag_id = ANY($1::bigint[]) AND deleted_at IS NULL`,
		anyArg).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// CountChannelRefs 统计标签被渠道绑定数。
func (s *Store) CountChannelRefs(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	anyArg := "{" + strings.Join(parts, ",") + "}"
	var n int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM channel_tags WHERE tag_id = ANY($1::bigint[])`, anyArg).Scan(&n); err != nil {
		return 0, fmt.Errorf("count channel refs: %w", err)
	}
	return n, nil
}

// BatchDelete 批量软删除标签，返回删除行数。若存在令牌引用该标签，返回 ErrTagInUse。
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
		`UPDATE semantic_tags SET deleted_at = now() WHERE id = ANY($1::bigint[])`, anyArg)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// jsonB 将值序列化为 JSON，供 JSONB 列写入；空值写 '{}' 而非 null。
func jsonB(v any) []byte {
	b, _ := json.Marshal(v)
	if b == nil {
		b = []byte("{}")
	}
	return b
}
