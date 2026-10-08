package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/team/llmgateway/internal/domain/audit"
	"github.com/team/llmgateway/internal/pkg/crypto"
	"github.com/team/llmgateway/internal/pkg/jwtx"
	"github.com/team/llmgateway/internal/pkg/ratelimit"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// newTestManager 生成临时 RSA 密钥并构造 jwtx.Manager。
func newTestManager(t *testing.T) *jwtx.Manager {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	dir := t.TempDir()
	privPath := filepath.Join(dir, "priv.pem")
	pubPath := filepath.Join(dir, "pub.pem")

	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal pub: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		t.Fatalf("write priv: %v", err)
	}
	if err := os.WriteFile(pubPath, pubPEM, 0o600); err != nil {
		t.Fatalf("write pub: %v", err)
	}
	mgr, err := jwtx.NewManager(privPath, pubPath, 60)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return mgr
}

const selectUserByUsername = `SELECT id, username, password_hash, role, status, pricing_mode, nickname, is_system, must_change_password, token_version, totp_enabled, created_at, updated_at FROM users WHERE username = $1`

func userRow(u *User) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "username", "password_hash", "role", "status",
		"pricing_mode", "nickname", "is_system", "must_change_password", "token_version", "totp_enabled", "created_at", "updated_at"}).
		AddRow(u.ID, u.Username, u.PasswordHash, u.Role, u.Status,
			u.PricingMode, u.Nickname, false, u.MustChangePassword, u.TokenVersion, u.TOTPEnabled, u.CreatedAt, u.UpdatedAt)
}

func TestService_Login_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	hash, err := crypto.HashPassword("correct-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	now := time.Now()
	u := &User{ID: 1, Username: "admin", PasswordHash: hash, Role: RoleAdmin,
		Status: StatusActive, PricingMode: PricingModeSale, CreatedAt: now, UpdatedAt: now}

	mock.ExpectQuery(regexp.QuoteMeta(selectUserByUsername)).
		WithArgs("admin").WillReturnRows(userRow(u))
	mock.ExpectQuery(regexp.QuoteMeta(
		`UPDATE users SET token_version = token_version + 1, updated_at = now() WHERE id = $1 AND deleted_at IS NULL RETURNING token_version`)).
		WithArgs(int64(1)).WillReturnRows(sqlmock.NewRows([]string{"token_version"}).AddRow(1))

	mgr := newTestManager(t)
	svc := NewService(NewStore(db), mgr)

	lr, err := svc.Login(context.Background(), "admin", "correct-password")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if lr.Token == "" {
		t.Fatal("expected non-empty token")
	}
	got := lr.User
	if got == nil || got.ID != 1 || got.Role != RoleAdmin {
		t.Fatalf("unexpected user: %+v", got)
	}
	if got.PasswordHash != "" {
		t.Fatal("password_hash should be cleared")
	}
	if lr.TokenVersion != 1 {
		t.Fatalf("expected token_version 1, got %d", lr.TokenVersion)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestService_Login_RequiresTOTP(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	hash, _ := crypto.HashPassword("pass")
	now := time.Now()
	u := &User{ID: 3, Username: "sec", PasswordHash: hash, Role: RoleDeveloper,
		Status: StatusActive, PricingMode: PricingModeSale, TOTPEnabled: true,
		CreatedAt: now, UpdatedAt: now}
	mock.ExpectQuery(regexp.QuoteMeta(selectUserByUsername)).WithArgs("sec").WillReturnRows(userRow(u))
	svc := NewService(NewStore(db), newTestManager(t))
	lr, err := svc.Login(context.Background(), "sec", "pass")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !lr.NeedTOTP || lr.Token != "" {
		t.Fatalf("expected need_totp with no token, got %+v", lr)
	}
}

func TestValidatePasswordStrength(t *testing.T) {
	for _, ok := range []string{"Ab123456", "abcdefgh1", "12345678!"} {
		if err := validatePasswordStrength(ok); err != nil {
			t.Fatalf("should accept %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"short", "12345678", "abcdefgh"} {
		if err := validatePasswordStrength(bad); err == nil {
			t.Fatalf("should reject %q", bad)
		}
	}
}

func TestService_Login_WrongPassword(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	hash, _ := crypto.HashPassword("right-password")
	now := time.Now()
	u := &User{ID: 1, Username: "admin", PasswordHash: hash, Role: RoleAdmin,
		Status: StatusActive, PricingMode: PricingModeSale, CreatedAt: now, UpdatedAt: now}

	mock.ExpectQuery(regexp.QuoteMeta(selectUserByUsername)).
		WithArgs("admin").WillReturnRows(userRow(u))

	svc := NewService(NewStore(db), newTestManager(t))

	_, err = svc.Login(context.Background(), "admin", "wrong-password")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeUnauthorized {
		t.Fatalf("expected 40101, got %v", err)
	}
}

func TestService_Login_UserNotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// 用户不存在与密码错误应返回相同的 40101，防止账户枚举。
	mock.ExpectQuery(regexp.QuoteMeta(selectUserByUsername)).
		WithArgs("ghost").WillReturnRows(sqlmock.NewRows([]string{"id"}))

	svc := NewService(NewStore(db), newTestManager(t))

	_, err = svc.Login(context.Background(), "ghost", "whatever")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeUnauthorized {
		t.Fatalf("expected 40101, got %v", err)
	}
	if apiErr.Message != "用户名或密码错误" {
		t.Fatalf("message should be unified, got %q", apiErr.Message)
	}
}

func TestService_Login_Disabled(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	hash, _ := crypto.HashPassword("pass")
	now := time.Now()
	u := &User{ID: 2, Username: "dev", PasswordHash: hash, Role: RoleDeveloper,
		Status: StatusDisabled, PricingMode: PricingModeSale, CreatedAt: now, UpdatedAt: now}

	mock.ExpectQuery(regexp.QuoteMeta(selectUserByUsername)).
		WithArgs("dev").WillReturnRows(userRow(u))

	svc := NewService(NewStore(db), newTestManager(t))

	_, err = svc.Login(context.Background(), "dev", "pass")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeForbidden {
		t.Fatalf("expected 40301, got %v", err)
	}
	if apiErr.Message != "账户已被禁用，请联系管理员" {
		t.Fatalf("unexpected message: %q", apiErr.Message)
	}
}

func TestService_AdminCreateUser_DuplicateName(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	insertQuery := `INSERT INTO users(id, username, password_hash, role, status, pricing_mode, nickname, is_system, must_change_password)
		 VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING `
	mock.ExpectQuery(regexp.QuoteMeta(insertQuery)).
		WithArgs(sqlmock.AnyArg(), "dup", sqlmock.AnyArg(), RoleDeveloper, StatusActive, PricingModeSale, "", false, true).
		WillReturnError(&pgconn.PgError{Code: "23505", Message: "duplicate key"})

	svc := NewService(NewStore(db), newTestManager(t))

	_, err = svc.AdminCreateUser(context.Background(), "dup", "DupPass@123", RoleDeveloper, PricingModeSale, "")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeConflict {
		t.Fatalf("expected 40901, got %v", err)
	}
	if apiErr.Message != "用户名已存在" {
		t.Fatalf("unexpected message: %q", apiErr.Message)
	}
}

func TestService_Login_EmptyInput(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	svc := NewService(NewStore(db), newTestManager(t))

	_, err = svc.Login(context.Background(), "", "")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeBadRequest {
		t.Fatalf("expected 40001, got %v", err)
	}
}

// 回归 N2：超长用户名按认证失败处理（401，与"用户不存在"同构），且不查库。
func TestService_Login_UsernameTooLong(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	svc := NewService(NewStore(db), newTestManager(t))

	long := strings.Repeat("a", 200)
	_, err = svc.Login(context.Background(), long, "whatever")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeUnauthorized {
		t.Fatalf("expected 40101 for over-long username, got %v", err)
	}
}

func TestService_BatchDeleteUsers_BillingRecorded(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// Store 在事务内先检查账单记录，发现引用则回滚并返回冲突错误。
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT u\.username FROM users u WHERE u\.id = ANY\(\$1::bigint\[\]\)`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"username"}).AddRow("dev1"))
	mock.ExpectRollback()

	svc := NewService(NewStore(db), newTestManager(t))
	_, err = svc.BatchDeleteUsers(context.Background(), []int64{1, 2})
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeConflict {
		t.Fatalf("expected 40901, got %v", err)
	}
	if !strings.Contains(apiErr.Message, "dev1") {
		t.Fatalf("message should mention offending username, got %q", apiErr.Message)
	}
	if !strings.Contains(apiErr.Message, "禁用") {
		t.Fatalf("message should suggest disabling account, got %q", apiErr.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestStore_BatchDelete_CascadesDepsInOrder(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// 无账单记录时，同一事务内软删除用户及其令牌；钱包与流水（财务数据）保留。
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT u\.username FROM users u WHERE u\.id = ANY\(\$1::bigint\[\]\)`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"username"}))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE users SET deleted_at = now() WHERE id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE tokens SET deleted_at = now() WHERE user_id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()

	svc := NewService(NewStore(db), newTestManager(t))
	n, err := svc.BatchDeleteUsers(context.Background(), []int64{1, 2})
	if err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// recordingAudit 记录型审计实现，用于断言认证事件确实落库（而非仅断言返回码）。
type recordingAudit struct{ entries []audit.Entry }

func (r *recordingAudit) Insert(_ context.Context, e audit.Entry) error {
	r.entries = append(r.entries, e)
	return nil
}

// 回归 R1：多字节超长用户名的审计截断必须落在 rune 边界（合法 UTF-8），
// 且必须真的留下一条审计——否则攻击者可用中文/emoji 用户名再次规避审计。
func TestService_Login_UsernameTooLong_Multibyte(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	svc := NewService(NewStore(db), newTestManager(t))
	sink := &recordingAudit{}
	svc.SetAudit(sink)

	long := strings.Repeat("管", 22) // 66 字节 > 64，截断点落在某字符的中间字节
	_, err = svc.Login(context.Background(), long, "whatever")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeUnauthorized {
		t.Fatalf("expected 40101, got %v", err)
	}
	if len(sink.entries) != 1 {
		t.Fatalf("expected exactly 1 audit entry, got %d", len(sink.entries))
	}
	got := sink.entries[0]
	if !utf8.ValidString(got.Username) {
		t.Fatalf("audit username must stay valid UTF-8, got %q", got.Username)
	}
	if len(got.Username) > maxUsernameLen {
		t.Fatalf("audit username must be capped to %d bytes, got %d", maxUsernameLen, len(got.Username))
	}
	if got.Action != "auth.login.fail" {
		t.Fatalf("unexpected audit action %q", got.Action)
	}
}

func TestTruncateUTF8(t *testing.T) {
	cases := []string{
		strings.Repeat("管", 22),  // 3 字节字符
		strings.Repeat("a", 200), // ASCII
		strings.Repeat("🙂", 40),  // 4 字节 emoji
		strings.Repeat("管", 21) + "a",
	}
	for _, in := range cases {
		out := truncateUTF8(in, maxUsernameLen)
		if len(out) > maxUsernameLen {
			t.Fatalf("len(out)=%d exceeds %d", len(out), maxUsernameLen)
		}
		if !utf8.ValidString(out) {
			t.Fatalf("truncated result must be valid UTF-8: %q", out)
		}
	}
	if truncateUTF8("short", maxUsernameLen) != "short" {
		t.Fatal("input within cap should be returned unchanged")
	}
}

// 回归 N6/R6：角色变更由单条 SQL 原子完成——role <> $1 时 token_version 递增。
// 断言 SQL 使用 CASE WHEN role <> $1（PG 的 SET 各表达式按 OLD 行求值，语义正确），
// 并透传 RETURNING 的新版本号，避免"角色已改但令牌未失效"的窗口。
func TestStore_Update_BumpsVersionOnRoleChange(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`UPDATE users\s+SET role = \$1[\s\S]*CASE WHEN role <> \$1 THEN token_version \+ 1[\s\S]*RETURNING token_version, status, must_change_password`).
		WithArgs(RoleDeveloper, StatusActive, PricingModeSale, "", int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"token_version", "status", "must_change_password"}).
			AddRow(5, StatusActive, false))

	store := NewStore(db)
	ver, status, mustChange, err := store.Update(context.Background(), &User{
		ID: 7, Role: RoleDeveloper, Status: StatusActive, PricingMode: PricingModeSale,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if ver != 5 || status != StatusActive || mustChange {
		t.Fatalf("unexpected return: ver=%d status=%s mustChange=%v", ver, status, mustChange)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestVerifyCurrentPassword_RateLimited 二次验证口令尝试应受限流约束：
// 账号进入锁定后直接返回 429，且不再查库（拦截发生在 PBKDF2 之前，防 CPU 放大）。
func TestVerifyCurrentPassword_RateLimited(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	svc := NewService(NewStore(db), newTestManager(t))
	rl := ratelimit.New()
	svc.SetRateLimiter(rl)

	key := secondFactorKey(9)
	// 账号维度累计 10 次失败即进入退避锁定（与 IP 无关，故与请求 ip 取值无关）。
	for i := 0; i < 10; i++ {
		rl.RecordFailure("1.2.3.4", key)
	}

	err = svc.VerifyCurrentPassword(context.Background(), 9, "guess")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeRateLimited {
		t.Fatalf("expected %d, got %v", resp.CodeRateLimited, err)
	}
	// 限流路径不应触达数据库（本用例未设置任何查询期望）。
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func asAPIError(err error, target **APIError) bool {
	if err == nil {
		return false
	}
	ae, ok := err.(*APIError)
	if !ok {
		return false
	}
	*target = ae
	return true
}
