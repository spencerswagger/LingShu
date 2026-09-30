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

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/team/llmgateway/internal/pkg/crypto"
	"github.com/team/llmgateway/internal/pkg/jwtx"
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

	mgr := newTestManager(t)
	svc := NewService(NewStore(db), mgr)

	token, got, err := svc.Login(context.Background(), "admin", "correct-password")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
	if got == nil || got.ID != 1 || got.Role != RoleAdmin {
		t.Fatalf("unexpected user: %+v", got)
	}
	if got.PasswordHash != "" {
		t.Fatal("password_hash should be cleared")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
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

	_, _, err = svc.Login(context.Background(), "admin", "wrong-password")
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

	_, _, err = svc.Login(context.Background(), "ghost", "whatever")
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

	_, _, err = svc.Login(context.Background(), "dev", "pass")
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

	insertQuery := `INSERT INTO users(username, password_hash, role, status, pricing_mode, nickname, is_system)
		 VALUES($1, $2, $3, $4, $5, $6, $7)
		 RETURNING `
	mock.ExpectQuery(regexp.QuoteMeta(insertQuery)).
		WithArgs("dup", sqlmock.AnyArg(), RoleDeveloper, StatusActive, PricingModeSale, "", false).
		WillReturnError(&pgconn.PgError{Code: "23505", Message: "duplicate key"})

	svc := NewService(NewStore(db), newTestManager(t))

	_, err = svc.AdminCreateUser(context.Background(), "dup", "pwd", RoleDeveloper, PricingModeSale, "")
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

	_, _, err = svc.Login(context.Background(), "", "")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeBadRequest {
		t.Fatalf("expected 40001, got %v", err)
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
