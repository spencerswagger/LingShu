package automodel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/team/llmgateway/internal/pkg/idgen"
)

// autoCols 列出 auto_models 表查询使用的全部列，保持各查询一致。
const autoCols = `id, name, scope, owner_user_id, enabled, description, created_at, updated_at`

// scanAuto 将一行扫描到 *AutoModel（owner_user_id 可空）。
func scanAuto(row interface{ Scan(...any) error }) (*AutoModel, error) {
	var m AutoModel
	var owner sql.NullInt64
	err := row.Scan(&m.ID, &m.Name, &m.Scope, &owner, &m.Enabled, &m.Description, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if owner.Valid {
		v := owner.Int64
		m.OwnerUserID = &v
	}
	return &m, nil
}

// rowQuerier 抽象 *sql.DB / *sql.Tx 的查询能力，使成员加载可在事务内复用。
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Store 提供 auto_models / auto_model_members 表的基础数据访问。
type Store struct {
	db *sql.DB
}

// NewStore 创建 Auto 模型存储，db 为 pgx stdlib 连接池。
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// GetByName 按名解析对请求用户可见的 Auto（名称全局唯一 = 扁平命名空间，至多命中一行）：
//   - scope='global' 对所有用户可见；
//   - scope='private' 仅对 owner 可见（ownerUserID 为 nil 时不可见）。
//
// 命中即返回（含按 position 升序的成员列表）。查不到（无此名或不可见）返回 sql.ErrNoRows。
// 单条查询完成作用域可见性过滤，避免热路径上 private/global 两次串行查询。
func (s *Store) GetByName(ctx context.Context, name string, ownerUserID *int64) (*AutoModel, error) {
	query := `SELECT ` + autoCols + ` FROM auto_models
		 WHERE name = $1 AND deleted_at IS NULL AND (scope = 'global'`
	args := []any{name}
	if ownerUserID != nil {
		args = append(args, *ownerUserID)
		query += ` OR owner_user_id = $2`
	}
	query += `)`
	m, err := scanAuto(s.db.QueryRowContext(ctx, query, args...))
	if err != nil {
		return nil, err
	}
	if err := s.attachMembers(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

// GetByNameAny 按名查任意作用域/归属的 Auto（忽略可见性），供唯一性校验使用。
// 名称全局唯一（扁平命名空间），至多命中一行；查不到返回 sql.ErrNoRows。
func (s *Store) GetByNameAny(ctx context.Context, name string) (*AutoModel, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+autoCols+` FROM auto_models WHERE name = $1 AND deleted_at IS NULL`, name)
	m, err := scanAuto(row)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// GetByID 按主键查（含成员列表）；不存在返回 sql.ErrNoRows。
func (s *Store) GetByID(ctx context.Context, id int64) (*AutoModel, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+autoCols+` FROM auto_models WHERE id = $1 AND deleted_at IS NULL`, id)
	m, err := scanAuto(row)
	if err != nil {
		return nil, err
	}
	if err := s.attachMembers(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

// List 查询 Auto 列表：
//
//	scope 非空时按 scope 精确过滤；ownerUserID 非空时再叠加可见性过滤
//	（global 恒可见 + 该 owner 的 private）。两个谓词以 AND 合并：
//	  例：scope='private' + owner=7 → scope='private' AND owner_user_id=7（即用户 7 的私有）；
//	      scope='global'  + owner=7 → scope='global'（全局）；
//	      scope 空 + owner=7        → global + 用户 7 的 private（用户可见集）。
//	每条含成员列表，按 position 升序。排序：id 升序。
func (s *Store) List(ctx context.Context, scope string, ownerUserID *int64) ([]AutoModel, error) {
	query := `SELECT ` + autoCols + ` FROM auto_models WHERE deleted_at IS NULL`
	args := []any{}
	if scope != "" {
		args = append(args, scope)
		query += ` AND scope = $` + strconv.Itoa(len(args))
	}
	if ownerUserID != nil {
		args = append(args, *ownerUserID)
		query += ` AND (scope = 'global' OR owner_user_id = $` + strconv.Itoa(len(args)) + `)`
	}
	query += ` ORDER BY id ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list auto models: %w", err)
	}
	defer rows.Close()
	list := make([]AutoModel, 0, 8)
	ids := make([]int64, 0, 8)
	for rows.Next() {
		m, err := scanAuto(rows)
		if err != nil {
			return nil, fmt.Errorf("scan auto model: %w", err)
		}
		list = append(list, *m)
		ids = append(ids, m.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return list, nil
	}
	// 批量加载成员，避免 N+1。
	members, err := s.loadMembers(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].Members = members[list[i].ID]
	}
	return list, nil
}

// insertMembers 在同一事务内插入某 Auto 的全部成员行（ID 缺省时用雪花 ID 生成）。
func insertMembers(ctx context.Context, tx *sql.Tx, autoModelID int64, members []Member) error {
	for _, m := range members {
		id := m.ID
		if id == 0 {
			id = idgen.New()
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO auto_model_members(id, auto_model_id, external_model_id, position)
			 VALUES($1,$2,$3,$4)`, id, autoModelID, m.ExternalModelID, m.Position); err != nil {
			return err
		}
	}
	return nil
}

// Insert 插入 Auto 及其成员（同一事务）；主表名冲突返回 ErrNameExists（PG 23505）。
func (s *Store) Insert(ctx context.Context, m *AutoModel) (*AutoModel, error) {
	if m.ID == 0 {
		m.ID = idgen.New()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx,
		`INSERT INTO auto_models(id, name, scope, owner_user_id, enabled, description)
		 VALUES($1,$2,$3,$4,$5,$6) RETURNING `+autoCols,
		m.ID, m.Name, m.Scope, nullableID(m.OwnerUserID), m.Enabled, m.Description)
	created, err := scanAuto(row)
	if err != nil {
		return nil, classifyAutoErr(err)
	}
	if err := insertMembers(ctx, tx, created.ID, m.Members); err != nil {
		return nil, classifyAutoErr(err)
	}
	// 事务内回读成员，返回附带 external_name/enabled 的完整记录。
	loaded, err := loadMembersQ(ctx, tx, []int64{created.ID})
	if err != nil {
		return nil, err
	}
	created.Members = loaded[created.ID]
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit insert auto model: %w", err)
	}
	return created, nil
}

// Update 更新主表字段 + 重建成员列表（软删旧成员行、插入新成员行，同一事务）；
// 不存在返回 sql.ErrNoRows；名冲突返回 ErrNameExists。
func (s *Store) Update(ctx context.Context, m *AutoModel) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE auto_models SET name=$1, scope=$2, owner_user_id=$3, enabled=$4, description=$5, updated_at=now()
		 WHERE id=$6 AND deleted_at IS NULL`,
		m.Name, m.Scope, nullableID(m.OwnerUserID), m.Enabled, m.Description, m.ID)
	if err != nil {
		return classifyAutoErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	// 重建成员：先软删旧行再插入新行，保证 position 与提交的列表一致。
	if _, err := tx.ExecContext(ctx,
		`UPDATE auto_model_members SET deleted_at=now() WHERE auto_model_id=$1 AND deleted_at IS NULL`, m.ID); err != nil {
		return err
	}
	if err := insertMembers(ctx, tx, m.ID, m.Members); err != nil {
		return classifyAutoErr(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit update auto model: %w", err)
	}
	return nil
}

// Delete 软删除 Auto 主表与其全部成员行；不存在返回 sql.ErrNoRows。
func (s *Store) Delete(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE auto_models SET deleted_at=now() WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE auto_model_members SET deleted_at=now() WHERE auto_model_id=$1 AND deleted_at IS NULL`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete auto model: %w", err)
	}
	return nil
}

// attachMembers 为单个 Auto 填充成员列表。
func (s *Store) attachMembers(ctx context.Context, m *AutoModel) error {
	if m == nil {
		return nil
	}
	members, err := s.loadMembers(ctx, []int64{m.ID})
	if err != nil {
		return err
	}
	m.Members = members[m.ID]
	return nil
}

// loadMembers 批量加载一批 Auto 的成员（按 position 升序），避免 N+1。
// 返回以 auto_model_id 为键的成员列表，未命中的 Auto 对应空切片。
func (s *Store) loadMembers(ctx context.Context, autoModelIDs []int64) (map[int64][]Member, error) {
	return loadMembersQ(ctx, s.db, autoModelIDs)
}

// loadMembersQ 在指定查询器（*sql.DB 或 *sql.Tx）上批量加载成员。
// 成员仅取自身未软删的行；对外模型用 LEFT JOIN，避免对外模型被软删时成员丢行
// （此时 external_name 为空、enabled 为 false）。成员按 position ASC, id ASC 排序。
func loadMembersQ(ctx context.Context, q rowQuerier, autoModelIDs []int64) (map[int64][]Member, error) {
	out := make(map[int64][]Member, len(autoModelIDs))
	for _, id := range autoModelIDs {
		out[id] = make([]Member, 0)
	}
	if len(autoModelIDs) == 0 {
		return out, nil
	}
	parts := make([]string, 0, len(autoModelIDs))
	for _, id := range autoModelIDs {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	anyArg := "{" + strings.Join(parts, ",") + "}"

	rows, err := q.QueryContext(ctx,
		`SELECT m.id, m.auto_model_id, m.external_model_id, m.position, em.external_name, em.enabled
		 FROM auto_model_members m
		 LEFT JOIN external_models em ON em.id = m.external_model_id
		 WHERE m.auto_model_id = ANY($1::bigint[]) AND m.deleted_at IS NULL
		 ORDER BY m.auto_model_id ASC, m.position ASC, m.id ASC`, anyArg)
	if err != nil {
		return nil, fmt.Errorf("list auto model members: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var member Member
		var autoModelID int64
		var extName sql.NullString
		var extEnabled sql.NullBool
		if err := rows.Scan(&member.ID, &autoModelID, &member.ExternalModelID, &member.Position,
			&extName, &extEnabled); err != nil {
			return nil, fmt.Errorf("scan auto model member: %w", err)
		}
		member.ExternalName = extName.String
		member.Enabled = extEnabled.Bool
		out[autoModelID] = append(out[autoModelID], member)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// classifyAutoErr 把数据库约束冲突映射为领域错误：
//   - 23505（唯一约束）：主表名冲突 或 同 Auto 内 position 重复 → ErrNameExists；
//   - 23503（外键约束）：按约束名区分——成员引用 auto_model_members.external_model_id
//     → ErrExternalModelMissing；owner_user_id → users 的归属用户不存在 → ErrOwnerUserMissing。
func classifyAutoErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return ErrNameExists
		case "23503":
			// 仅「成员引用对外模型」的 FK 归一为 ErrExternalModelMissing；
			// owner_user_id → users 的 FK 单独归一，避免误报成「成员模型不存在」。
			if strings.Contains(pgErr.ConstraintName, "external_model") {
				return ErrExternalModelMissing
			}
			if strings.Contains(pgErr.ConstraintName, "owner_user_id") {
				return ErrOwnerUserMissing
			}
			return ErrExternalModelMissing
		}
	}
	return err
}

// ErrExternalModelMissing 表示成员引用的对外模型不存在（PG 23503 外键冲突）。
var ErrExternalModelMissing = errors.New("external model not found")

// ErrOwnerUserMissing 表示私有 Auto 指定的归属用户不存在（owner_user_id 外键冲突）。
var ErrOwnerUserMissing = errors.New("owner user not found")

// nullableID 把可空外键转为可入库的 any（nil → NULL）。
func nullableID(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
