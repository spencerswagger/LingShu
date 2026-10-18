package identity

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/pkg/session"
)

// getTokenByIDSQL 与 TokenStore.GetByID 一致（tests 用 regexp.QuoteMeta 精确匹配）。
const getTokenByIDSQL = `SELECT ` + tokenCols + ` FROM tokens WHERE id = $1`

// updateTokenStatusSQL 与 TokenStore.UpdateStatus 一致。
const updateTokenStatusSQL = `UPDATE tokens SET status = $1 WHERE id = $2`

// hashesByIDsSQL 与 TokenStore.HashesByIDs 一致。
const hashesByIDsSQL = `SELECT token_hash FROM tokens WHERE id = ANY($1::bigint[]) AND deleted_at IS NULL`

// deleteTokensSQL 与 TokenStore.DeleteTokens 一致。
const deleteTokensSQL = `UPDATE tokens SET deleted_at = now() WHERE id = ANY($1::bigint[])`

// selectUserByIDSQL 与 identity.Store.GetByID 一致。
const selectUserByIDSQL = `SELECT ` + userCols + ` FROM users WHERE id = $1 AND deleted_at IS NULL`

// seedTokenCache 直接向缓存写入一条「用户 ACTIVE、令牌 ACTIVE」的成功鉴权快照，便于断言写路径失效。
func seedTokenCache(svc *TokenService, hash string, userID int64) {
	svc.cache.put(hash, &Token{ID: 1, TokenHash: hash, UserID: userID, Status: StatusTokenActive}, StatusActive)
}

// TestTokenCache_HitZeroDB：首次回源写缓存，第二次命中缓存不产生任何 DB 查询。
func TestTokenCache_HitZeroDB(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	plain := "sk-gw-cachehit"
	hash := tokenHash(plain)
	tok := &Token{ID: 100, TokenHash: hash, TokenDisplay: "sk-gw-****",
		UserID: 1, DisplayName: "k", Status: StatusTokenActive, CreatedAt: time.Now()}

	// 仅第一次回源需要期望；第二次若仍查询，sqlmock 会返回「unexpected call」错误。
	mock.ExpectQuery(regexp.QuoteMeta(getTokenByHashSQL)).WithArgs(hash).WillReturnRows(tokenRow(tok))
	mock.ExpectQuery(regexp.QuoteMeta(userStatusSQL)).WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"role", "status"}).AddRow(RoleDeveloper, StatusActive))

	svc := NewTokenService(NewTokenStore(db))
	first, err := svc.LookupByPlain(context.Background(), plain)
	if err != nil || first == nil {
		t.Fatalf("first lookup: %v", err)
	}

	second, err := svc.LookupByPlain(context.Background(), plain)
	if err != nil {
		t.Fatalf("cached lookup should be 0 DB, got error: %v", err)
	}
	if second == nil || second.TokenHash != hash || second.UserID != 1 {
		t.Fatalf("cached token mismatch: %+v", second)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestTokenCache_ExpiryBoundary：命中后按令牌自身 ExpiresAt 精确过期——now==ExpiresAt 即视为未命中。
func TestTokenCache_ExpiryBoundary(t *testing.T) {
	svc := NewTokenService(nil)
	exp := time.Now().Add(time.Hour)
	// 写入时刻取 exp-1s：保证两次读取都落在安全 TTL（60s）窗口内，从而只验证「精确过期」而非 TTL。
	t0 := exp.Add(-time.Second)
	newEntry := func() *Token {
		return &Token{ID: 1, UserID: 7, TokenHash: "h", Status: StatusTokenActive, ExpiresAt: &exp}
	}

	svc.cache.now = func() time.Time { return t0 }
	svc.cache.put("h", newEntry(), StatusActive)
	svc.cache.now = func() time.Time { return exp.Add(-time.Nanosecond) }
	if _, ok := svc.cache.get("h"); !ok {
		t.Fatal("过期前 1ns 应命中")
	}

	svc.cache.put("h", newEntry(), StatusActive)
	svc.cache.now = func() time.Time { return exp }
	if _, ok := svc.cache.get("h"); ok {
		t.Fatal("now==ExpiresAt 应视为未命中（精确过期边界）")
	}
}

// TestTokenCache_SafetyTTL：安全 TTL 仅作最后防线——条目超过 TTL 一律回源。
func TestTokenCache_SafetyTTL(t *testing.T) {
	svc := NewTokenService(nil)
	svc.SetTokenCacheTTL(60 * time.Second)
	t0 := time.Now()
	svc.cache.now = func() time.Time { return t0 }
	svc.cache.put("h", &Token{ID: 1, UserID: 7, TokenHash: "h", Status: StatusTokenActive}, StatusActive)

	svc.cache.now = func() time.Time { return t0.Add(60*time.Second - time.Nanosecond) }
	if _, ok := svc.cache.get("h"); !ok {
		t.Fatal("TTL 内应命中")
	}

	svc.cache.now = func() time.Time { return t0.Add(60 * time.Second) }
	if _, ok := svc.cache.get("h"); ok {
		t.Fatal("达到安全 TTL 应视为未命中")
	}
}

// TestTokenCache_Disabled：关闭开关后 LookupByPlain 退回纯 DB 直查（每次各查一次）。
func TestTokenCache_Disabled(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	plain := "sk-gw-nocache"
	hash := tokenHash(plain)
	tok := &Token{ID: 1, TokenHash: hash, TokenDisplay: "sk-gw-****",
		UserID: 1, DisplayName: "k", Status: StatusTokenActive, CreatedAt: time.Now()}
	for i := 0; i < 2; i++ {
		mock.ExpectQuery(regexp.QuoteMeta(getTokenByHashSQL)).WithArgs(hash).WillReturnRows(tokenRow(tok))
		mock.ExpectQuery(regexp.QuoteMeta(userStatusSQL)).WithArgs(int64(1)).
			WillReturnRows(sqlmock.NewRows([]string{"role", "status"}).AddRow(RoleDeveloper, StatusActive))
	}

	svc := NewTokenService(NewTokenStore(db))
	svc.SetTokenCacheEnabled(false)
	for i := 0; i < 2; i++ {
		if _, err := svc.LookupByPlain(context.Background(), plain); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("关闭缓存时每次都应查库: %v", err)
	}
}

// TestTokenCache_InvalidateAll：整体失效后无残留条目。
func TestTokenCache_InvalidateAll(t *testing.T) {
	svc := NewTokenService(nil)
	seedTokenCache(svc, "h1", 7)
	seedTokenCache(svc, "h2", 8)
	svc.InvalidateAll()
	if svc.cache.count() != 0 {
		t.Fatalf("InvalidateAll 后应无残留，got %d", svc.cache.count())
	}
}

// TestTokenCache_StalePutAfterInvalidateRejected 锁定 P1（认证绕过窗口）：
// 读路径在回源 DB 前捕获代际；若期间发生失效（写路径 bump 代际，例如禁用/软删用户），
// 读到旧快照后的回填必须被拒绝——否则陈旧 ACTIVE 条目会存活至安全 TTL，被禁用用户的令牌
// 在该窗口内仍可鉴权，违背「撤销/禁用/删除经钩子即时生效」承诺。
func TestTokenCache_StalePutAfterInvalidateRejected(t *testing.T) {
	c := newTokenCache()
	tok := &Token{ID: 1, TokenHash: "h", UserID: 7, Status: StatusTokenActive}

	// 读路径：回源前捕获代际。
	gen := c.generation()
	// 期间写路径失效（用户被禁用/软删）。
	c.invalidateByUserID(7)
	// 读路径回来回填（携带陈旧代际）。
	c.putAtGen("h", tok, StatusActive, gen)

	if _, ok := c.get("h"); ok {
		t.Fatal("失效后携带陈旧代际的回填必须被拒绝，否则禁用用户在该条目存活期内仍可鉴权")
	}

	// 对照组：期间无失效时，携带当前代际的回填应正常命中（证明不会误杀正常回填路径）。
	c.putAtGen("h", tok, StatusActive, c.generation())
	if _, ok := c.get("h"); !ok {
		t.Fatal("无失效发生时回填应正常命中")
	}
}

// TestTokenCache_EvictionBound：条目超过上限时被淘汰，不无界增长。
func TestTokenCache_EvictionBound(t *testing.T) {
	svc := NewTokenService(nil)
	svc.cache.maxEntries = 4
	for i := 0; i < 20; i++ {
		h := fmt.Sprintf("h%d", i)
		svc.cache.put(h, &Token{ID: int64(i), UserID: int64(i), TokenHash: h, Status: StatusTokenActive}, StatusActive)
	}
	if got := svc.cache.count(); got > 4 {
		t.Fatalf("缓存条目应受上限约束（<=4），got %d", got)
	}
}

// TestTokenCache_DefensiveCopy 锁定 P2-5：get/put 深拷贝指针字段（TagID/ExpiresAt/LastUsedAt），
// 缓存与调用方互不共享指针地址，任一方写 *ptr 都不得污染另一方。
func TestTokenCache_DefensiveCopy(t *testing.T) {
	svc := NewTokenService(nil)
	c := svc.cache

	tagID := int64(11)
	exp := time.Now().Add(time.Hour)
	last := time.Now().Add(-time.Minute)
	// 先快照期望值：下方对 *tok 指针字段的写入会连带改写 tagID/exp/last 本身。
	wantTag, wantExp, wantLast := tagID, exp, last
	tok := &Token{
		ID: 1, TokenHash: "h", UserID: 7, Status: StatusTokenActive,
		TagID: &tagID, ExpiresAt: &exp, LastUsedAt: &last,
	}
	c.put("h", tok, StatusActive)

	// 调用方修改自己持有的 Token 指针字段 → 缓存不受影响。
	*tok.TagID = 99
	*tok.ExpiresAt = exp.Add(48 * time.Hour)
	*tok.LastUsedAt = time.Now()

	got, ok := c.get("h")
	if !ok {
		t.Fatal("应命中缓存")
	}
	if *got.TagID != wantTag {
		t.Fatalf("put 未深拷贝：TagID=%d", *got.TagID)
	}
	if !got.ExpiresAt.Equal(wantExp) {
		t.Fatalf("put 未深拷贝：ExpiresAt=%v", *got.ExpiresAt)
	}
	if !got.LastUsedAt.Equal(wantLast) {
		t.Fatalf("put 未深拷贝：LastUsedAt=%v", *got.LastUsedAt)
	}

	// 调用方修改 get 返回值的指针字段 → 缓存不受影响。
	*got.TagID = 1234
	*got.ExpiresAt = wantExp.Add(72 * time.Hour)

	got2, ok := c.get("h")
	if !ok {
		t.Fatal("应再次命中缓存")
	}
	if *got2.TagID != wantTag || !got2.ExpiresAt.Equal(wantExp) {
		t.Fatalf("get 未深拷贝：TagID=%d ExpiresAt=%v", *got2.TagID, *got2.ExpiresAt)
	}
}

// TestTokenInvalidate_WritePaths：令牌域每条写路径写入后，受影响的缓存项必须「立即」失效。
func TestTokenInvalidate_WritePaths(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		hash  string
		setup func(t *testing.T, svc *TokenService, mock sqlmock.Sqlmock, hash string)
		call  func(t *testing.T, svc *TokenService)
	}{
		{
			name: "Create",
			hash: tokenHash("sk-gw-seed-create"),
			setup: func(t *testing.T, svc *TokenService, mock sqlmock.Sqlmock, hash string) {
				// 该用户已缓存了另一个令牌；创建新令牌后按其 userID 保守全清。
				seedTokenCache(svc, hash, 7)
				mock.ExpectQuery(regexp.QuoteMeta(userStatusSQL)).WithArgs(int64(7)).
					WillReturnRows(sqlmock.NewRows([]string{"role", "status"}).AddRow(RoleDeveloper, StatusActive))
				mock.ExpectQuery(regexp.QuoteMeta(insertTokenSQL)).
					WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), nil, int64(7), "n", nil, nil).
					WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(200), time.Now()))
			},
			call: func(t *testing.T, svc *TokenService) {
				if _, err := svc.Create(ctx, 7, "n", nil, nil); err != nil {
					t.Fatalf("create: %v", err)
				}
			},
		},
		{
			name: "BatchDelete",
			hash: tokenHash("sk-gw-seed-batchdelete"),
			setup: func(t *testing.T, svc *TokenService, mock sqlmock.Sqlmock, hash string) {
				seedTokenCache(svc, hash, 7)
				mock.ExpectQuery(regexp.QuoteMeta(hashesByIDsSQL)).WithArgs(sqlmock.AnyArg()).
					WillReturnRows(sqlmock.NewRows([]string{"token_hash"}).AddRow(hash))
				mock.ExpectExec(regexp.QuoteMeta(deleteTokensSQL)).WithArgs(sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(0, 1))
			},
			call: func(t *testing.T, svc *TokenService) {
				if _, err := svc.BatchDelete(ctx, []int64{3}); err != nil {
					t.Fatalf("batch delete: %v", err)
				}
			},
		},
		{
			name: "Rotate",
			hash: tokenHash("sk-gw-seed-rotate"),
			setup: func(t *testing.T, svc *TokenService, mock sqlmock.Sqlmock, hash string) {
				seedTokenCache(svc, hash, 7)
				old := &Token{ID: 5, TokenHash: hash, TokenDisplay: "sk-gw-****",
					UserID: 7, DisplayName: "n", Status: StatusTokenActive, CreatedAt: time.Now()}
				mock.ExpectQuery(regexp.QuoteMeta(getTokenByIDSQL)).WithArgs(int64(5)).WillReturnRows(tokenRow(old))
				mock.ExpectExec(regexp.QuoteMeta(updateTokenStatusSQL)).
					WithArgs(StatusTokenDisabled, int64(5)).WillReturnResult(sqlmock.NewResult(0, 1))
				// Rotate 末尾调用 Create：查用户状态 + 插入新令牌。
				mock.ExpectQuery(regexp.QuoteMeta(userStatusSQL)).WithArgs(int64(7)).
					WillReturnRows(sqlmock.NewRows([]string{"role", "status"}).AddRow(RoleDeveloper, StatusActive))
				mock.ExpectQuery(regexp.QuoteMeta(insertTokenSQL)).
					WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), nil, int64(7), "n", nil, nil).
					WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(201), time.Now()))
			},
			call: func(t *testing.T, svc *TokenService) {
				if _, err := svc.Rotate(ctx, 7, 5); err != nil {
					t.Fatalf("rotate: %v", err)
				}
			},
		},
		{
			name: "Toggle",
			hash: tokenHash("sk-gw-seed-toggle"),
			setup: func(t *testing.T, svc *TokenService, mock sqlmock.Sqlmock, hash string) {
				seedTokenCache(svc, hash, 7)
				old := &Token{ID: 5, TokenHash: hash, TokenDisplay: "sk-gw-****",
					UserID: 7, DisplayName: "n", Status: StatusTokenActive, CreatedAt: time.Now()}
				mock.ExpectQuery(regexp.QuoteMeta(getTokenByIDSQL)).WithArgs(int64(5)).WillReturnRows(tokenRow(old))
				mock.ExpectExec(regexp.QuoteMeta(updateTokenStatusSQL)).
					WithArgs(StatusTokenDisabled, int64(5)).WillReturnResult(sqlmock.NewResult(0, 1))
			},
			call: func(t *testing.T, svc *TokenService) {
				if _, err := svc.Toggle(ctx, 7, 5); err != nil {
					t.Fatalf("toggle: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()

			svc := NewTokenService(NewTokenStore(db))
			tc.setup(t, svc, mock, tc.hash)
			if _, ok := svc.cache.get(tc.hash); !ok {
				t.Fatalf("%s: 前置缓存项应存在", tc.name)
			}
			tc.call(t, svc)
			if _, ok := svc.cache.get(tc.hash); ok {
				t.Fatalf("%s: 写后缓存项应立即失效", tc.name)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("mock expectations: %v", err)
			}
		})
	}
}

// TestUserInvalidate_WritePaths：用户域每条写路径写后，令牌缓存的该用户条目与 pkg/session 用户状态缓存
// 都必须「同点、立即」失效（不得留下双缓存漂移窗口）。
func TestUserInvalidate_WritePaths(t *testing.T) {
	ctx := context.Background()
	// 命中缓存时不应回源 loader；若被调用说明对应缓存未失效。
	loadFails := func(_ context.Context, _ int64) (session.Entry, error) {
		return session.Entry{}, errors.New("loader should not be called when cache invalidated")
	}

	cases := []struct {
		name         string
		userID       int64
		hash         string
		seedSession  func(reg *session.Registry)
		setup        func(t *testing.T, mock sqlmock.Sqlmock)
		call         func(t *testing.T, svc *Service)
		checkSession func(t *testing.T, reg *session.Registry)
	}{
		{
			name:   "AdminCreateUser",
			userID: 42,
			hash:   tokenHash("sk-gw-seed-user-create"),
			// 新建用户无既有会话缓存，故不设置 seedSession/checkSession。
			setup: func(t *testing.T, mock sqlmock.Sqlmock) {
				mock.ExpectQuery(`INSERT INTO users\(id, username, password_hash, role, status, pricing_mode, nickname, is_system, must_change_password\)`).
					WithArgs(sqlmock.AnyArg(), "newbie", sqlmock.AnyArg(), RoleDeveloper, StatusActive, PricingModeSale, "", false, true).
					WillReturnRows(userRow(&User{ID: 42, Username: "newbie", Role: RoleDeveloper,
						Status: StatusActive, PricingMode: PricingModeSale, MustChangePassword: true}))
				mock.ExpectExec(`INSERT INTO credit_wallets`).WithArgs(int64(42)).
					WillReturnResult(sqlmock.NewResult(0, 1))
			},
			call: func(t *testing.T, svc *Service) {
				if _, err := svc.AdminCreateUser(ctx, "newbie", "Newbie@1234", RoleDeveloper, PricingModeSale, ""); err != nil {
					t.Fatalf("admin create user: %v", err)
				}
			},
		},
		{
			name:   "UpdateUser",
			userID: 7,
			hash:   tokenHash("sk-gw-seed-user-update"),
			seedSession: func(reg *session.Registry) {
				reg.Set(7, session.Entry{Version: 999, Status: StatusActive})
			},
			setup: func(t *testing.T, mock sqlmock.Sqlmock) {
				mock.ExpectQuery(regexp.QuoteMeta(selectUserByIDSQL)).WithArgs(int64(7)).
					WillReturnRows(userRow(&User{ID: 7, Username: "dev", Role: RoleAdmin, Status: StatusActive, PricingMode: PricingModeSale}))
				mock.ExpectQuery(`UPDATE users\s+SET role = \$1`).
					WithArgs(RoleDeveloper, StatusActive, PricingModeSale, "", int64(7)).
					WillReturnRows(sqlmock.NewRows([]string{"token_version", "status", "must_change_password"}).
						AddRow(5, StatusActive, false))
				mock.ExpectQuery(regexp.QuoteMeta(selectUserByIDSQL)).WithArgs(int64(7)).
					WillReturnRows(userRow(&User{ID: 7, Username: "dev", Role: RoleDeveloper, Status: StatusActive, PricingMode: PricingModeSale}))
			},
			call: func(t *testing.T, svc *Service) {
				if _, err := svc.UpdateUser(ctx, 7, RoleDeveloper, StatusActive, PricingModeSale, ""); err != nil {
					t.Fatalf("update user: %v", err)
				}
			},
			checkSession: func(t *testing.T, reg *session.Registry) {
				// session 缓存已被新版本覆盖：旧版本 999 必须校验失败。
				if _, err := reg.Check(ctx, 7, 999); err == nil {
					t.Fatal("session 缓存应已被 Set 覆盖（旧版本 999 应失效）")
				}
			},
		},
		{
			name:   "BatchDeleteUsers",
			userID: 7,
			hash:   tokenHash("sk-gw-seed-user-batchdelete"),
			seedSession: func(reg *session.Registry) {
				reg.Set(7, session.Entry{Version: 1, Status: StatusActive})
			},
			setup: func(t *testing.T, mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(`SELECT u\.username FROM users u`).WithArgs(sqlmock.AnyArg()).
					WillReturnRows(sqlmock.NewRows([]string{"username"}))
				mock.ExpectExec(regexp.QuoteMeta(`UPDATE users SET deleted_at = now() WHERE id = ANY($1::bigint[])`)).
					WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(regexp.QuoteMeta(`UPDATE tokens SET deleted_at = now() WHERE user_id = ANY($1::bigint[])`)).
					WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 2))
				mock.ExpectCommit()
			},
			call: func(t *testing.T, svc *Service) {
				if _, err := svc.BatchDeleteUsers(ctx, []int64{7}); err != nil {
					t.Fatalf("batch delete users: %v", err)
				}
			},
			checkSession: func(t *testing.T, reg *session.Registry) {
				// session 缓存条目已被 Delete：再次校验会回源 loader 并失败（ErrRevoked）。
				if _, err := reg.Check(ctx, 7, 1); err == nil {
					t.Fatal("session 缓存应已被删除（用户删除即断权）")
				}
			},
		},
		{
			name:   "AdminResetPassword",
			userID: 7,
			hash:   tokenHash("sk-gw-seed-user-resetpw"),
			seedSession: func(reg *session.Registry) {
				reg.Set(7, session.Entry{Version: 999, Status: StatusActive})
			},
			setup: func(t *testing.T, mock sqlmock.Sqlmock) {
				mock.ExpectQuery(regexp.QuoteMeta(selectUserByIDSQL)).WithArgs(int64(7)).
					WillReturnRows(userRow(&User{ID: 7, Username: "dev", Role: RoleDeveloper, Status: StatusActive, PricingMode: PricingModeSale}))
				mock.ExpectQuery(`UPDATE users SET password_hash = \$2, must_change_password = true`).
					WithArgs(int64(7), sqlmock.AnyArg()).
					WillReturnRows(sqlmock.NewRows([]string{"token_version"}).AddRow(5))
				// sessions 非空时重置后会再查一次用户以同步会话缓存。
				mock.ExpectQuery(regexp.QuoteMeta(selectUserByIDSQL)).WithArgs(int64(7)).
					WillReturnRows(userRow(&User{ID: 7, Username: "dev", Role: RoleDeveloper, Status: StatusActive, PricingMode: PricingModeSale}))
			},
			call: func(t *testing.T, svc *Service) {
				if err := svc.AdminResetPassword(ctx, 7, "Reset@1234"); err != nil {
					t.Fatalf("admin reset password: %v", err)
				}
			},
			checkSession: func(t *testing.T, reg *session.Registry) {
				if _, err := reg.Check(ctx, 7, 999); err == nil {
					t.Fatal("session 缓存应已被 Set 覆盖（旧版本 999 应失效）")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()

			// 真实令牌缓存实例：用生产入口 InvalidateByUserID 作为用户写钩子，断言该用户条目被清。
			ts := NewTokenService(nil)
			seedTokenCache(ts, tc.hash, tc.userID)

			reg := session.NewRegistry(loadFails)
			if tc.seedSession != nil {
				tc.seedSession(reg)
			}

			svc := NewService(NewStore(db), nil)
			svc.SetSessionRegistry(reg)
			svc.SetUserWriteHook(ts.InvalidateByUserID)

			tc.setup(t, mock)
			if _, ok := ts.cache.get(tc.hash); !ok {
				t.Fatalf("%s: 前置令牌缓存项应存在", tc.name)
			}
			tc.call(t, svc)
			if _, ok := ts.cache.get(tc.hash); ok {
				t.Fatalf("%s: 用户写后令牌缓存项应立即失效", tc.name)
			}
			if tc.checkSession != nil {
				tc.checkSession(t, reg)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("mock expectations: %v", err)
			}
		})
	}
}
