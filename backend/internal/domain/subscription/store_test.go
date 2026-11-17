package subscription

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
)

// SQLStore.Create 必须把 PG 23505 映射成 ErrDuplicate：
// Service 靠 errors.Is(err, ErrDuplicate) 决定 409 + ExistingID，映射丢了就变成 500「写入失败」，
// 前端也拿不到「改为编辑」的入口。这条映射以前零覆盖（变异验证抓到的）。
func TestSQLStore_Create_DuplicateMapping(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO subscriptions`)).
		WillReturnError(&pgconn.PgError{Code: "23505", ConstraintName: "uq_subscriptions_account"})

	st := NewSQLStore(db)
	err = st.Create(context.Background(), &Row{ID: 1, Provider: "acme", Account: "a"})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v，期望 ErrDuplicate", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("期望未满足：%v", err)
	}
}

// 非唯一冲突的错误必须原样上抛（不能一律吞成 ErrDuplicate，否则 500 会被伪装成 409）。
func TestSQLStore_Create_OtherErrorPassthrough(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO subscriptions`)).
		WillReturnError(errors.New("connection reset"))

	st := NewSQLStore(db)
	err = st.Create(context.Background(), &Row{ID: 1, Provider: "acme", Account: "a"})
	if err == nil || errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v，期望普通错误", err)
	}
}

// GetByNaturalKey：无行 → ErrNotFound（handler 层据此区分「不存在」与「查询失败」）。
func TestSQLStore_GetByNaturalKey_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`FROM subscriptions WHERE provider_ref = $1 AND account_ref = $2`)).
		WithArgs("acme", "a").
		WillReturnError(sql.ErrNoRows)

	st := NewSQLStore(db)
	if _, err := st.GetByNaturalKey(context.Background(), "acme", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v，期望 ErrNotFound", err)
	}
}

// 已知账号来自账单 ∪ 账本：只查一边会把「账本已到、账单未归因」的账号误报为未验证。
func TestSQLStore_KnownAccounts_UnionsBothSources(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	since := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM billing_records`)).
		WithArgs(since).
		WillReturnRows(sqlmock.NewRows([]string{"account"}).AddRow("acct-1").AddRow("acct-2"))

	st := NewSQLStore(db)
	got, err := st.KnownAccounts(context.Background(), since)
	if err != nil {
		t.Fatalf("KnownAccounts: %v", err)
	}
	if _, ok := got["acct-1"]; !ok {
		t.Fatalf("缺少 acct-1：%+v", got)
	}
	if _, ok := got["acct-2"]; !ok {
		t.Fatalf("缺少 acct-2：%+v", got)
	}
}
