// Package identity 提供管理员/开发者账户的存储、认证（登录、JWT 签发）与用户管理。
package identity

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
)

// 账户角色 / 状态 / 计费模式常量。
const (
	RoleAdmin       = "ADMIN"
	RoleDeveloper   = "DEVELOPER"
	StatusActive    = "ACTIVE"
	StatusDisabled  = "DISABLED"
	PricingModeSale = "sale"
	PricingModeCost = "cost"
)

// User 对应 users 表的一行。
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Nickname     string // 展示用昵称，可为空
	Role         string
	Status       string
	PricingMode  string
	IsSystem     bool // 系统内置用户（健康探测开销归属），不出现在用户管理/登录等用户侧
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// 安全字段（API 响应可见，均非敏感密文）。
	MustChangePassword bool
	TokenVersion       int
	TOTPEnabled        bool
	// 查询聚合字段（非表列）：余额与累计消费（completed 账单求和），List 时填充。
	Balance    float64
	TotalSpent float64
}

// userCols 列出 users 表查询时使用的全部列，保持各查询一致。
const userCols = `id, username, password_hash, role, status, pricing_mode, nickname, is_system, must_change_password, token_version, totp_enabled, created_at, updated_at`

// scanUser 将一行扫描到 *User。
func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Status,
		&u.PricingMode, &u.Nickname, &u.IsSystem, &u.MustChangePassword,
		&u.TokenVersion, &u.TOTPEnabled, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// Store 提供 users 表的基础数据访问。
type Store struct {
	db *sql.DB
}

// NewStore 创建用户存储，db 为 pgx stdlib 连接池。
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// GetByUsername 按用户名查询用户；不存在返回 sql.ErrNoRows。
func (s *Store) GetByUsername(ctx context.Context, username string) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE username = $1 AND deleted_at IS NULL`, username)
	return scanUser(row)
}

// GetByID 按主键查询用户；不存在返回 sql.ErrNoRows。
func (s *Store) GetByID(ctx context.Context, id int64) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE id = $1 AND deleted_at IS NULL`, id)
	return scanUser(row)
}

// ErrUsernameExists 表示用户名唯一约束冲突（PG 23505）。
var ErrUsernameExists = errors.New("username already exists")

// Create 插入新用户并返回回填主键后完整的用户，username 冲突返回 ErrUsernameExists。
func (s *Store) Create(ctx context.Context, u *User) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO users(username, password_hash, role, status, pricing_mode, nickname, is_system)
		 VALUES($1, $2, $3, $4, $5, $6, $7)
		 RETURNING `+userCols,
		u.Username, u.PasswordHash, u.Role, u.Status, u.PricingMode, u.Nickname, u.IsSystem)
	created, err := scanUser(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrUsernameExists
		}
		return nil, err
	}
	return created, nil
}

// List 按过滤条件分页查询用户，返回列表与总条数。filter 为空表示不过滤用户名。
func (s *Store) List(ctx context.Context, filter, role, status string, page, size int) ([]User, int64, error) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	offset := int64((page - 1) * size)

	where := `deleted_at IS NULL AND is_system = false AND ($1 = '' OR username ILIKE '%' || $1 || '%')
	          AND ($2 = '' OR role = $2) AND ($3 = '' OR status = $3)`

	var total int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE `+where,
		filter, role, status).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count users: %w", err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userCols+` FROM users
		 WHERE `+where+`
		 ORDER BY id DESC LIMIT $4 OFFSET $5`,
		filter, role, status, size, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	users := make([]User, 0, size)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, *u)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

// Update 更新用户的状态、角色、计费模式与昵称，返回受影响行数。
func (s *Store) Update(ctx context.Context, u *User) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET role = $1, status = $2, pricing_mode = $3, nickname = $4, updated_at = now()
		 WHERE id = $5`,
		u.Role, u.Status, u.PricingMode, u.Nickname, u.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// BumpTokenVersion 会话代数 +1，返回新值（登录/登出用）。
func (s *Store) BumpTokenVersion(ctx context.Context, id int64) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx,
		`UPDATE users SET token_version = token_version + 1, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL RETURNING token_version`, id).Scan(&v)
	return v, err
}

// ChangePassword 本人改密：更新哈希、清除 must_change_password、会话代数 +1，返回新版本。
func (s *Store) ChangePassword(ctx context.Context, id int64, hash string) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx,
		`UPDATE users SET password_hash = $2, must_change_password = false,
		   token_version = token_version + 1, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL RETURNING token_version`, id, hash).Scan(&v)
	return v, err
}

// UpdatePassword 管理员重置口令：更新哈希、会话代数 +1（不改 must_change_password），返回新版本。
func (s *Store) UpdatePassword(ctx context.Context, id int64, hash string) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx,
		`UPDATE users SET password_hash = $2, token_version = token_version + 1, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL RETURNING token_version`, id, hash).Scan(&v)
	return v, err
}

// ClearMustChange 清除强制改密标记（改密页成功后的兜底）。
func (s *Store) ClearMustChange(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET must_change_password = false WHERE id = $1`, id)
	return err
}

// UpdateNickname 仅更新昵称（本人资料修改入口），返回更新后的用户。
func (s *Store) UpdateNickname(ctx context.Context, id int64, nickname string) (*User, error) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE users SET nickname = $2, updated_at = now() WHERE id = $1`, id, nickname); err != nil {
		return nil, err
	}
	return s.GetByID(ctx, id)
}

// Ledger 是用户列表聚合出的钱包级数据：当前余额与累计消费额（completed 账单积分数之和）。
type Ledger struct {
	Balance    float64
	TotalSpent float64
}

// bigintArray 生成 PG bigint[] 字面量（如 `{1,2,3}`），兼容 pgx stdlib 与 sqlmock。
func bigintArray(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// LoadLedger 批量查询多个用户的余额与累计消费额（按 user_id 索引返回）。
// 数组按 BatchDelete 同款方式传参：字符串字面量 `{1,2}` 绑定到 $1::bigint[]，
// 兼容 pgx stdlib 与 sqlmock。
func (s *Store) LoadLedger(ctx context.Context, ids []int64) (map[int64]Ledger, error) {
	out := make(map[int64]Ledger, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	arg := bigintArray(ids)

	rows, err := s.db.QueryContext(ctx,
		`SELECT u.id, COALESCE(w.balance, 0)::float8
		 FROM users u LEFT JOIN credit_wallets w ON w.user_id = u.id
		 WHERE u.id = ANY($1::bigint[])`, arg)
	if err != nil {
		return nil, fmt.Errorf("load balances: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var bal float64
		if err := rows.Scan(&id, &bal); err != nil {
			return nil, err
		}
		e := out[id]
		e.Balance = bal
		out[id] = e
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx,
		`SELECT user_id, COALESCE(SUM(credits_consumed), 0)::float8
		 FROM billing_records
		 WHERE status = 'completed' AND user_id = ANY($1::bigint[])
		 GROUP BY user_id`, arg)
	if err != nil {
		return nil, fmt.Errorf("load spent: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var spent float64
		if err := rows.Scan(&id, &spent); err != nil {
			return nil, err
		}
		e := out[id]
		e.TotalSpent = spent
		out[id] = e
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// BillingRecordedError 表示待删除用户存在不可删除的账单记录（审计轨迹）。
type BillingRecordedError struct {
	Username string // 首个存在账单记录的用户名，用于冲突提示；为空表示未命名。
}

func (e *BillingRecordedError) Error() string {
	if e.Username != "" {
		return fmt.Sprintf("user %s has billing records", e.Username)
	}
	return "user has billing records"
}

// BatchDelete 批量软删除用户，返回删除行数。若这批用户中存在账单记录（审计轨迹，
// 不可删除、不可级联），则返回 *BillingRecordedError 而不做任何删除。否则在
// 同一事务内将用户与其令牌置为软删除（令牌立即失效）。
func (s *Store) BatchDelete(ctx context.Context, ids []int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// bigint[] 字面量参数，用于 ANY($1::bigint[])，兼容 pgx stdlib 与 sqlmock。
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	anyArg := "{" + strings.Join(parts, ",") + "}"
	var usersRes sql.Result

	// 先检查这批用户是否有账单记录：billing 是审计轨迹，不能删也不能级联。
	var username sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT u.username FROM users u
		 WHERE u.id = ANY($1::bigint[])
		   AND EXISTS (SELECT 1 FROM billing_records b WHERE b.user_id = u.id)
		 ORDER BY u.id LIMIT 1`, anyArg).Scan(&username)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("check billing records: %w", err)
	}
	if username.Valid {
		return 0, &BillingRecordedError{Username: username.String}
	}

	// 无账单记录才可安全删除：软删除用户及其令牌（凭据立即失效）。
	// 钱包余额与流水属财务数据，保留不删。
	var res sql.Result
	for _, stmt := range []string{
		`UPDATE users SET deleted_at = now() WHERE id = ANY($1::bigint[])`,
		`UPDATE tokens SET deleted_at = now() WHERE user_id = ANY($1::bigint[])`,
	} {
		res, err = tx.ExecContext(ctx, stmt, anyArg)
		if err != nil {
			return 0, fmt.Errorf("soft delete cascade: %w", err)
		}
		if stmt == `UPDATE users SET deleted_at = now() WHERE id = ANY($1::bigint[])` {
			// 记录用户软删行数作为最终返回值（不影响 tokens 的后续执行）
			usersRes = res
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return usersRes.RowsAffected()
}

// EnsureWallet 为指定用户创建 credit_wallets 行（已存在则跳过），余额初始为 0。
func (s *Store) EnsureWallet(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO credit_wallets(user_id, balance) VALUES ($1, 0)
		 ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		return fmt.Errorf("ensure wallet: %w", err)
	}
	return nil
}

// ===== TOTP 专用（不进入 userCols，避免密文外泄） =====

// TOTPSecret 返回用户 TOTP 密文与启用状态；用户不存在返回 sql.ErrNoRows。
func (s *Store) TOTPSecret(ctx context.Context, id int64) (cipher string, enabled bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(totp_secret_cipher, ''), totp_enabled FROM users WHERE id = $1 AND deleted_at IS NULL`,
		id).Scan(&cipher, &enabled)
	return
}

// SetTOTPSecret 保存待确认的 TOTP 密文（未启用态）。
func (s *Store) SetTOTPSecret(ctx context.Context, id int64, cipher string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET totp_secret_cipher = $2, totp_enabled = false WHERE id = $1`, id, cipher)
	return err
}

// EnableTOTP 启用 TOTP 并落库恢复码哈希（jsonb）。
func (s *Store) EnableTOTP(ctx context.Context, id int64, hashes []string) error {
	b, err := json.Marshal(hashes)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE users SET totp_enabled = true, totp_recovery_hashes = $2::jsonb WHERE id = $1`, id, string(b))
	return err
}

// GetRecoveryHashes 返回用户剩余恢复码哈希；未设置返回空切片。
func (s *Store) GetRecoveryHashes(ctx context.Context, id int64) ([]string, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT totp_recovery_hashes::text FROM users WHERE id = $1 AND deleted_at IS NULL`, id).Scan(&raw)
	if err != nil {
		return nil, err
	}
	if !raw.Valid || raw.String == "" || raw.String == "null" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetRecoveryHashes 更新用户剩余恢复码哈希。
func (s *Store) SetRecoveryHashes(ctx context.Context, id int64, hashes []string) error {
	b, err := json.Marshal(hashes)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE users SET totp_recovery_hashes = $2::jsonb WHERE id = $1`, id, string(b))
	return err
}

// DisableTOTP 解绑 TOTP：清空密文、恢复码并置未启用。
func (s *Store) DisableTOTP(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET totp_enabled = false, totp_secret_cipher = NULL, totp_recovery_hashes = NULL WHERE id = $1`, id)
	return err
}
