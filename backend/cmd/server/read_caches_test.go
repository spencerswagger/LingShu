package main

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/gateway"
	"github.com/team/llmgateway/internal/domain/identity"
)

// userRows 构造 users 表全列查询的一行（列顺序与 identity.userCols 一致）。
func userRows(id int64, pricingMode string, now time.Time) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "username", "password_hash", "role", "status", "pricing_mode", "nickname",
		"is_system", "must_change_password", "token_version", "totp_enabled", "created_at", "updated_at",
	}).AddRow(id, "u", "h", "DEVELOPER", "ACTIVE", pricingMode, "", false, false, int64(1), false, now, now)
}

const (
	userSelectSQL = `(?s)SELECT .* FROM users WHERE id = \$1 AND deleted_at IS NULL`
	userUpdateSQL = `(?s)UPDATE users.*RETURNING token_version, status, must_change_password`
)

// TestPricingModeCache_HitZeroDBAndInvalidate 验证定价模式读缓存（STEP 23）：
// 命中后零 DB（连续多次解析仅回源一次）；identity 用户写路径（UpdateUser）经 SetUserWriteHook
// 回调立即失效，下一次解析重新查库并读到新模式。
func TestPricingModeCache_HitZeroDBAndInvalidate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now()

	store := identity.NewStore(db)
	svc := identity.NewService(store, nil)
	cache := newPricingModeCache()
	// 交付顺序：失效钩子先于缓存启用接线（与 main.go 装配一致）。
	svc.SetUserWriteHook(cache.Invalidate)
	resolver := newPricingModeResolver(store, cache)

	// 首次解析：直查一次，命中用户为 cost。
	mock.ExpectQuery(userSelectSQL).WithArgs(int64(7)).WillReturnRows(userRows(7, identity.PricingModeCost, now))
	for i := 0; i < 3; i++ {
		mode, err := resolver(context.Background(), 7)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if mode != identity.PricingModeCost {
			t.Fatalf("want cost, got %q", mode)
		}
	}
	// 仅一次回源：命中缓存不应再读库。
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("命中缓存不应再读库: %v", err)
	}

	// 用户写路径（UpdateUser：改计费模式为 sale）后缓存立即失效。
	mock.ExpectQuery(userSelectSQL).WithArgs(int64(7)).WillReturnRows(userRows(7, identity.PricingModeCost, now)) // prev
	mock.ExpectQuery(userUpdateSQL).WillReturnRows(sqlmock.NewRows([]string{"token_version", "status", "must_change_password"}).
		AddRow(int64(2), identity.StatusActive, false))
	mock.ExpectQuery(userSelectSQL).WithArgs(int64(7)).WillReturnRows(userRows(7, identity.PricingModeSale, now)) // got
	if _, err := svc.UpdateUser(context.Background(), 7, identity.RoleDeveloper, identity.StatusActive, identity.PricingModeSale, ""); err != nil {
		t.Fatalf("update user: %v", err)
	}

	// 缓存已失效：下一次解析重新查库并读到新模式 sale。
	mock.ExpectQuery(userSelectSQL).WithArgs(int64(7)).WillReturnRows(userRows(7, identity.PricingModeSale, now))
	mode, err := resolver(context.Background(), 7)
	if err != nil {
		t.Fatalf("resolve after invalidate: %v", err)
	}
	if mode != identity.PricingModeSale {
		t.Fatalf("失效后应读到新计费模式 sale，got %q", mode)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestPricingModeResolver_NoCacheAlwaysReadsDB 验证缓存开关关闭（cache=nil）时退回直查（STEP 28）。
func TestPricingModeResolver_NoCacheAlwaysReadsDB(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now()

	store := identity.NewStore(db)
	resolver := newPricingModeResolver(store, nil)

	for i := 0; i < 3; i++ {
		mock.ExpectQuery(userSelectSQL).WithArgs(int64(7)).WillReturnRows(userRows(7, identity.PricingModeCost, now))
		mode, err := resolver(context.Background(), 7)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if mode != identity.PricingModeCost {
			t.Fatalf("want cost, got %q", mode)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("关闭缓存应每次直查: %v", err)
	}
}

// TestConfigValueCache_HitZeroDBAndInvalidate 验证上游响应头白名单读缓存（STEP 27）：
// 命中后零 DB；console 写配置经 billing.UpsertConfig → SetConfigWriteHook 回调立即失效，再读重取新值。
func TestConfigValueCache_HitZeroDBAndInvalidate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := billing.NewSqlStore(db)
	cache := newConfigValueCache(store, gateway.UpstreamResponseHeaderWhitelistKey)
	// 模拟装配层：sys_configs 写钩子先于缓存启用接线。
	store.SetConfigWriteHook(cache.Invalidate)

	sel := `SELECT value FROM sys_configs WHERE key=\$1`
	mock.ExpectQuery(sel).WithArgs(gateway.UpstreamResponseHeaderWhitelistKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`["x-trace"]`)))

	for i := 0; i < 3; i++ {
		raw, err := cache.Get(context.Background())
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if string(raw) != `["x-trace"]` {
			t.Fatalf("unexpected: %s", raw)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("命中缓存不应再读库: %v", err)
	}

	// console 写该配置：UpsertConfig 触发写钩子 → 缓存失效。
	mock.ExpectExec(`(?s)INSERT INTO sys_configs.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpsertConfig(context.Background(), gateway.UpstreamResponseHeaderWhitelistKey, []byte(`["y-trace"]`), 1); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// 失效后重新读库并拿到新值。
	mock.ExpectQuery(sel).WithArgs(gateway.UpstreamResponseHeaderWhitelistKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`["y-trace"]`)))
	raw, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("get after invalidate: %v", err)
	}
	if string(raw) != `["y-trace"]` {
		t.Fatalf("失效后应读到新值，got %s", raw)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
