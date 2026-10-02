package identity

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/pquerna/otp/totp"
	"github.com/team/llmgateway/internal/pkg/crypto"
)

// newTOTPService 构造绑定了 sqlmock 的 TOTPService。
func newTOTPService(t *testing.T) (*TOTPService, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	svc := NewTOTPService(NewStore(db), []byte("0123456789abcdef"))
	return svc, mock
}

// secretRow 生成真实 TOTP secret 与 SM4 密文。
func secretRow(t *testing.T, enabled bool) (string, string) {
	t.Helper()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "llmgateway", AccountName: "u", SecretSize: 20})
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	cipher, err := crypto.SM4Encrypt([]byte("0123456789abcdef"), []byte(key.Secret()))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return key.Secret(), cipher
}

// genCode 生成 at 时刻的动态码。
func genCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCode(secret, at)
	if err != nil {
		t.Fatalf("gen code: %v", err)
	}
	return code
}

// 回归 C1：Confirm 在"待确认态"（enabled=false）也能取到 secret 并启用。
func TestTOTPService_Confirm_PendingState(t *testing.T) {
	svc, mock := newTOTPService(t)
	secret, cipher := secretRow(t, false)

	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT COALESCE(totp_secret_cipher, ''), totp_enabled FROM users WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).WillReturnRows(sqlmock.NewRows([]string{"cipher", "enabled"}).AddRow(cipher, false))
	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE users SET totp_enabled = true, totp_recovery_hashes = $2::jsonb WHERE id = $1`)).
		WithArgs(int64(1), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))

	codes, err := svc.Confirm(context.Background(), 1, genCode(t, secret, time.Now()))
	if err != nil {
		t.Fatalf("confirm should succeed in pending state, got %v", err)
	}
	if len(codes) != recoveryNum {
		t.Fatalf("expected %d recovery codes, got %d", recoveryNum, len(codes))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// 回归 M10：同一时间步的动态码第二次校验被拒绝（防重放）。
func TestTOTPService_Verify_ReplayRejected(t *testing.T) {
	svc, mock := newTOTPService(t)
	secret, cipher := secretRow(t, true)
	code := genCode(t, secret, time.Now())

	expectSecret(t, mock, cipher, true)
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT COALESCE(totp_last_step, 0) FROM users WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).WillReturnRows(sqlmock.NewRows([]string{"step"}).AddRow(int64(0)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE users SET totp_last_step = $2 WHERE id = $1`)).
		WithArgs(int64(1), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))

	ok, err := svc.Verify(context.Background(), 1, code)
	if err != nil || !ok {
		t.Fatalf("first verify should pass, ok=%v err=%v", ok, err)
	}

	// 第二次：同一时间步 → last >= step，判定重放
	expectSecret(t, mock, cipher, true)
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT COALESCE(totp_last_step, 0) FROM users WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).WillReturnRows(sqlmock.NewRows([]string{"step"}).AddRow(time.Now().Unix() / 30))

	ok, err = svc.Verify(context.Background(), 1, code)
	if err != nil {
		t.Fatalf("replay verify should return (false, nil), got err %v", err)
	}
	if ok {
		t.Fatal("same time-step code should be rejected as replay")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func expectSecret(t *testing.T, mock sqlmock.Sqlmock, cipher string, enabled bool) {
	t.Helper()
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT COALESCE(totp_secret_cipher, ''), totp_enabled FROM users WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).WillReturnRows(sqlmock.NewRows([]string{"cipher", "enabled"}).AddRow(cipher, enabled))
}
