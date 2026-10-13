package identity

import (
	"context"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestService_Register_InvalidUsername 用户名不合法（过短/含非法字符）应在任何查询前直接拒绝。
func TestService_Register_InvalidUsername(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	svc := NewService(NewStore(db), newTestManager(t))

	for _, name := range []string{"ab", "bad name"} {
		_, err = svc.Register(context.Background(), name, "Pass1234", "")
		var apiErr *APIError
		if !asAPIError(err, &apiErr) || apiErr.HTTPStatus != http.StatusBadRequest {
			t.Fatalf("username %q: expected 400, got %v", name, err)
		}
	}
	// 未设置任何查询期望：若注册逻辑误触达数据库，sqlmock 会返回错误使用例失败。
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestService_Register_WeakPassword 弱口令在创建前被拒绝，不产生 Create 调用。
func TestService_Register_WeakPassword(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	svc := NewService(NewStore(db), newTestManager(t))

	_, err = svc.Register(context.Background(), "devuser1", "12345678", "")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("expected 400, got %v", err)
	}
	// 未设置 Create 期望：一旦误触发即失败。
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestService_Register_DuplicateUsername 用户名已存在应返回 409（冲突），且不再执行 Create。
func TestService_Register_DuplicateUsername(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Now()
	existing := &User{ID: 1, Username: "dup", PasswordHash: "hash", Role: RoleDeveloper,
		Status: StatusActive, PricingMode: PricingModeSale, CreatedAt: now, UpdatedAt: now}
	mock.ExpectQuery(regexp.QuoteMeta(selectUserByUsername)).WithArgs("dup").WillReturnRows(userRow(existing))

	svc := NewService(NewStore(db), newTestManager(t))

	_, err = svc.Register(context.Background(), "dup", "Pass1234", "")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.HTTPStatus != http.StatusConflict {
		t.Fatalf("expected 409, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestService_Register_Success 自助注册成功路径：GetByUsername 无结果 → Create →
// EnsureWallet → BumpTokenVersion（注册即登录）；返回带 token 的开发者账号，且不强制改密。
func TestService_Register_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(selectUserByUsername)).
		WithArgs("devuser1").WillReturnRows(sqlmock.NewRows([]string{"id"}))

	insertQuery := `INSERT INTO users(id, username, password_hash, role, status, pricing_mode, nickname, is_system, must_change_password)
		 VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING `
	now := time.Now()
	created := &User{ID: 1, Username: "devuser1", PasswordHash: "hash", Role: RoleDeveloper,
		Status: StatusActive, PricingMode: PricingModeSale, CreatedAt: now, UpdatedAt: now}
	mock.ExpectQuery(regexp.QuoteMeta(insertQuery)).
		WithArgs(sqlmock.AnyArg(), "devuser1", sqlmock.AnyArg(), RoleDeveloper, StatusActive, PricingModeSale, "", false, false).
		WillReturnRows(userRow(created))

	mock.ExpectExec(`INSERT INTO credit_wallets\(user_id, balance\) VALUES \(\$1, 0\)\s+ON CONFLICT \(user_id\) DO NOTHING`).
		WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery(regexp.QuoteMeta(
		`UPDATE users SET token_version = token_version + 1, updated_at = now() WHERE id = $1 AND deleted_at IS NULL RETURNING token_version`)).
		WithArgs(int64(1)).WillReturnRows(sqlmock.NewRows([]string{"token_version"}).AddRow(1))

	svc := NewService(NewStore(db), newTestManager(t))

	lr, err := svc.Register(context.Background(), "devuser1", "Pass1234", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if lr.Token == "" {
		t.Fatal("expected non-empty token")
	}
	if lr.MustChangePassword {
		t.Fatal("self-registered user must not be forced to change password")
	}
	if lr.User == nil {
		t.Fatal("expected user in login result")
	}
	if lr.User.Role != RoleDeveloper || lr.User.Status != StatusActive {
		t.Fatalf("unexpected user: %+v", lr.User)
	}
	if lr.User.PricingMode != PricingModeSale || lr.User.MustChangePassword {
		t.Fatalf("unexpected user: %+v", lr.User)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
