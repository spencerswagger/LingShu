package identity

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/pquerna/otp/totp"
	"github.com/team/llmgateway/internal/pkg/crypto"
	"github.com/team/llmgateway/internal/pkg/resp"
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

// 回归 R3：已启用 2FA 时调用 Setup 必须被拒绝，且不改动 totp_enabled。
// 否则 Setup 会把 enabled 置回 false，形成一条"免动态码静默关闭 2FA"的捷径。
func TestTOTPService_Setup_RejectedWhenEnabled(t *testing.T) {
	svc, mock := newTOTPService(t)
	_, cipher := secretRow(t, true)

	// 只应发生一次 SELECT；未设置 UPDATE 期望即代表 totp_enabled 未被改写。
	expectSecret(t, mock, cipher, true)

	_, _, err := svc.Setup(context.Background(), 1, "u")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeBadRequest {
		t.Fatalf("setup must be rejected when already enabled, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// 回归 N4：解绑路径不消费时间步——同一窗口内刚用于登录的码应能直接用于关闭 2FA。
func TestTOTPService_Disable_DoesNotConsumeStep(t *testing.T) {
	svc, mock := newTOTPService(t)
	secret, cipher := secretRow(t, true)

	// 仅取 secret + 执行 DisableTOTP；不应出现 TOTPLastStep 的读写期望（未设置即代表未调用）。
	expectSecret(t, mock, cipher, true)
	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE users SET totp_enabled = false, totp_secret_cipher = NULL, totp_recovery_hashes = NULL, totp_last_step = 0 WHERE id = $1`)).
		WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(1, 1))

	if err := svc.Disable(context.Background(), 1, genCode(t, secret, time.Now())); err != nil {
		t.Fatalf("disable should succeed without consuming step, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestPreAuthStore_PeekDoesNotConsume 校验 Peek 只读：可重复校验，Consume 后才失效。
func TestPreAuthStore_PeekDoesNotConsume(t *testing.T) {
	p := NewPreAuthStore()
	tok, ok := p.Issue(7)
	if !ok {
		t.Fatalf("issue 应成功")
	}
	for i := 0; i < 3; i++ {
		if uid, ok := p.Peek(tok); !ok || uid != 7 {
			t.Fatalf("第 %d 次 Peek 应命中用户 7，实际 (%d,%v)", i+1, uid, ok)
		}
	}
	if uid, ok := p.Consume(tok); !ok || uid != 7 {
		t.Fatalf("Consume 应命中用户 7，实际 (%d,%v)", uid, ok)
	}
	if _, ok := p.Peek(tok); ok {
		t.Fatalf("Consume 后 Peek 不应再命中")
	}
}

// TestPreAuthStore_IssueKeepsLatestPerUser 同一用户重复签发时旧 token 立即作废（防单账号灌满存储）。
func TestPreAuthStore_IssueKeepsLatestPerUser(t *testing.T) {
	p := NewPreAuthStore()
	first, _ := p.Issue(7)
	second, _ := p.Issue(7)
	if first == second {
		t.Fatalf("两次签发应产生不同 token")
	}
	if _, ok := p.Peek(first); ok {
		t.Fatalf("旧 token 应被新签发作废")
	}
	if uid, ok := p.Peek(second); !ok || uid != 7 {
		t.Fatalf("最新 token 应有效，实际 (%d,%v)", uid, ok)
	}
}

// TestPreAuthStore_EvictsOldestAtCapacity 达上限时淘汰最旧条目而非拒绝签发（避免 2FA 全体登录失败）。
func TestPreAuthStore_EvictsOldestAtCapacity(t *testing.T) {
	p := NewPreAuthStore()
	oldest, ok := p.Issue(1)
	if !ok {
		t.Fatalf("首次签发应成功")
	}
	for i := 0; i < maxPreAuthEntries-1; i++ {
		if _, ok := p.Issue(int64(i + 2)); !ok {
			t.Fatalf("第 %d 次签发不应被拒绝", i+2)
		}
	}
	// 已满：再签发一次应成功且淘汰最旧（用户 1 的 token）。
	newest, ok := p.Issue(999999)
	if !ok {
		t.Fatalf("满员时签发不应被拒绝")
	}
	if _, ok := p.Peek(oldest); ok {
		t.Fatalf("最旧 token 应被淘汰")
	}
	if uid, ok := p.Peek(newest); !ok || uid != 999999 {
		t.Fatalf("新签发 token 应有效，实际 (%d,%v)", uid, ok)
	}
}

// TestPreAuthStore_ExpiredTokenRejected 过期 token 在 Peek/Consume 中均被拒绝。
func TestPreAuthStore_ExpiredTokenRejected(t *testing.T) {
	p := NewPreAuthStore()
	base := time.Now()
	p.now = func() time.Time { return base }
	tok, _ := p.Issue(7)
	p.now = func() time.Time { return base.Add(preAuthTTL + time.Second) }
	if _, ok := p.Peek(tok); ok {
		t.Fatalf("过期 token 不应被 Peek 命中")
	}
	if _, ok := p.Consume(tok); ok {
		t.Fatalf("过期 token 不应被 Consume 命中")
	}
}
