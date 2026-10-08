package identity

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/pkg/resp"
)

const userStatusSQL = `SELECT role, status FROM users WHERE id = $1`

const insertTokenSQL = `INSERT INTO tokens(id, token_hash, token_display, secret_cipher, user_id, display_name, tag_id, expires_at)
			 VALUES($1, $2, $3, $4, $5, $6, $7, $8)
			 RETURNING id, created_at`

const getTokenByHashSQL = `SELECT ` + tokenCols + ` FROM tokens WHERE token_hash = $1`

func tokenRow(tok *Token) *sqlmock.Rows {
	// 列顺序与 tokenCols 一致：id, token_hash, token_display, user_id, display_name,
	// tag_id, expires_at, last_used_at, status, created_at
	var tagID, expiresAt, lastUsedAt any = nil, nil, nil
	if tok.TagID != nil {
		tagID = *tok.TagID
	}
	if tok.ExpiresAt != nil {
		expiresAt = *tok.ExpiresAt
	}
	if tok.LastUsedAt != nil {
		lastUsedAt = *tok.LastUsedAt
	}
	return sqlmock.NewRows([]string{"id", "token_hash", "token_display", "user_id", "display_name",
		"tag_id", "expires_at", "last_used_at", "status", "created_at"}).
		AddRow(tok.ID, tok.TokenHash, tok.TokenDisplay, tok.UserID, tok.DisplayName,
			tagID, expiresAt, lastUsedAt, tok.Status, tok.CreatedAt)
}

func TestToken_CreateAndLookupByPlain_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Now()
	// 创建：用户存在且 ACTIVE。
	mock.ExpectQuery(regexp.QuoteMeta(userStatusSQL)).
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"role", "status"}).AddRow(RoleDeveloper, StatusActive))
	// 插入：secret_cipher 空串（未注入 SM4 密钥）+ hash/display 动态，user=1、name、tag=nil、expires=nil。
	mock.ExpectQuery(regexp.QuoteMeta(insertTokenSQL)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), nil, int64(1), "dev-key", nil, nil).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(100), now))

	svc := NewTokenService(NewTokenStore(db))
	plain, err := svc.Create(context.Background(), 1, "dev-key", nil, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if !strings.HasPrefix(plain, "sk-gw-") || len(plain) != len("sk-gw-")+32 {
		t.Fatalf("unexpected plain token format: %q", plain)
	}

	// 校验命中：Gateway 用明文可查到对应令牌。
	hash := tokenHash(plain)
	mock.ExpectQuery(regexp.QuoteMeta(getTokenByHashSQL)).
		WithArgs(hash).
		WillReturnRows(tokenRow(&Token{
			ID: 100, TokenHash: hash, TokenDisplay: tokenDisplay(plain),
			UserID: 1, DisplayName: "dev-key", Status: StatusTokenActive, CreatedAt: now,
		}))
	mock.ExpectQuery(regexp.QuoteMeta(userStatusSQL)).
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"role", "status"}).AddRow(RoleDeveloper, StatusActive))

	got, err := svc.LookupByPlain(context.Background(), plain)
	if err != nil {
		t.Fatalf("lookup by plain: %v", err)
	}
	if got == nil || got.TokenHash != hash {
		t.Fatalf("lookup mismatch: %+v", got)
	}
	// 明文不可从 store 还原：库里存的是哈希而非明文。
	if got.TokenHash == plain {
		t.Fatal("token_hash must be a hash, not the plaintext")
	}
	if strings.Contains(got.TokenDisplay, plain[len(plain)-8:]) {
		t.Fatal("token_display must not leak the full token tail")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestToken_Create_TagIDNil_InsertsNull(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(userStatusSQL)).
		WithArgs(int64(2)).
		WillReturnRows(sqlmock.NewRows([]string{"role", "status"}).AddRow(RoleDeveloper, StatusActive))
	// tag_id 与 expires_at 传 nil，落库即 NULL。
	mock.ExpectQuery(regexp.QuoteMeta(insertTokenSQL)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), nil, int64(2), "名-key", nil, nil).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))

	svc := NewTokenService(NewTokenStore(db))
	if _, err := svc.Create(context.Background(), 2, "名-key", nil, nil); err != nil {
		t.Fatalf("create with nil tag: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestToken_Create_UserMissing_Fails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// 用户不存在：仅 userStatus 查询返回无行，不执行插入。
	mock.ExpectQuery(regexp.QuoteMeta(userStatusSQL)).
		WithArgs(int64(99)).
		WillReturnRows(sqlmock.NewRows([]string{"role", "status"}))

	svc := NewTokenService(NewTokenStore(db))
	_, err = svc.Create(context.Background(), 99, "key", nil, nil)
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeNotFound {
		t.Fatalf("expected 40401, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestToken_Create_UserDisabled_Fails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(userStatusSQL)).
		WithArgs(int64(3)).
		// 未返回行（模拟用户缺失走 status 检查亦可）；这里模拟 DISABLED 用户。
		WillReturnRows(sqlmock.NewRows([]string{"role", "status"}))

	svc := NewTokenService(NewTokenStore(db))
	_, err = svc.Create(context.Background(), 3, "key", nil, nil)
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeNotFound {
		t.Fatalf("expected 40401 for missing/disabled user, got %v", err)
	}
}

func TestToken_LookupByPlain_Expired_Fails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	past := time.Now().Add(-time.Hour)
	hash := tokenHash("sk-gw-expired")
	mock.ExpectQuery(regexp.QuoteMeta(getTokenByHashSQL)).
		WithArgs(hash).
		WillReturnRows(tokenRow(&Token{
			ID: 1, TokenHash: hash, TokenDisplay: "sk-gw-****",
			UserID: 1, DisplayName: "k", ExpiresAt: &past, Status: StatusTokenActive, CreatedAt: past,
		}))

	svc := NewTokenService(NewTokenStore(db))
	_, err = svc.LookupByPlain(context.Background(), "sk-gw-expired")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("expected APIError, got %v", err)
	}
	if apiErr.HTTPStatus != 401 {
		t.Fatalf("expected 401 for expired token, got %d", apiErr.HTTPStatus)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestToken_LookupByPlain_Disabled_Fails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	hash := tokenHash("sk-gw-disabled")
	mock.ExpectQuery(regexp.QuoteMeta(getTokenByHashSQL)).
		WithArgs(hash).
		WillReturnRows(tokenRow(&Token{
			ID: 2, TokenHash: hash, TokenDisplay: "sk-gw-**",
			UserID: 1, DisplayName: "k", Status: StatusTokenDisabled, CreatedAt: time.Now(),
		}))

	svc := NewTokenService(NewTokenStore(db))
	if _, err := svc.LookupByPlain(context.Background(), "sk-gw-disabled"); err == nil {
		t.Fatal("expected error for disabled token")
	}
}
