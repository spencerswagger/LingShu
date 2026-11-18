package topology

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMockStore(t *testing.T) (*SQLStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewSQLStore(db), mock
}

// Providers：一条聚合 SQL，stale 用 FILTER 算（不许再查一次）。
// 空 provider_ref 也要出行：否则「供应商层条数之和 ≠ 内部模型条数」。
func TestSQLStore_Providers(t *testing.T) {
	st, mock := newMockStore(t)
	mock.ExpectQuery(regexp.QuoteMeta("FROM internal_models")).
		WillReturnRows(sqlmock.NewRows([]string{"provider_ref", "models", "stale"}).
			AddRow("acme", 3, 1).
			AddRow("", 1, 0))

	got, err := st.Providers(context.Background())
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	if len(got) != 2 || got[0].Provider != "acme" || got[0].Models != 3 || got[0].Stale != 1 {
		t.Fatalf("Providers = %+v", got)
	}
	if got[1].Provider != "" {
		t.Fatalf("空 provider_ref 不许被丢掉：%+v", got[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("期望未满足：%v", err)
	}
}

// Accounts：三处来源并集（账单 / 账本 / 订阅），订阅来源没有 last_seen。
// subscribed 用 bool_or：PG 没有 max(boolean)，写成 max 会在真库上 500。
func TestSQLStore_Accounts(t *testing.T) {
	st, mock := newMockStore(t)
	seen := time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("FROM ledger_entries")).
		WillReturnRows(sqlmock.NewRows([]string{"provider", "account", "subscribed", "last_seen"}).
			AddRow("acme", "acct-1", true, seen).
			AddRow("acme", "acct-2", false, nil))

	got, err := st.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(got) != 2 || !got[0].Subscribed || !got[0].LastSeenAt.Equal(seen) {
		t.Fatalf("Accounts = %+v", got)
	}
	if got[1].Subscribed || !got[1].LastSeenAt.IsZero() {
		t.Fatalf("没有账单痕迹的订阅账号：%+v", got[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("期望未满足：%v", err)
	}
}

// Externals：只取未软删的行，NULL 绑定归一成空串（「未绑定」只有一种写法）。
func TestSQLStore_Externals(t *testing.T) {
	st, mock := newMockStore(t)
	mock.ExpectQuery(regexp.QuoteMeta("FROM external_models")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "external_name", "enabled", "bound_internal_ref"}).
			AddRow(int64(300), "gpt-x", true, "acme/gpt-x").
			AddRow(int64(301), "gpt-y", false, "").
			AddRow(int64(302), "gpt-z", true, "   "))

	got, err := st.Externals(context.Background())
	if err != nil {
		t.Fatalf("Externals: %v", err)
	}
	if len(got) != 3 || got[0].BoundInternalRef != "acme/gpt-x" || got[0].ID != 300 {
		t.Fatalf("Externals = %+v", got)
	}
	if got[1].BoundInternalRef != "" || got[2].BoundInternalRef != "" {
		t.Fatalf("空白绑定必须归一成空串：%+v / %+v", got[1], got[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("期望未满足：%v", err)
	}
}

// SaveLinks：upsert 坐标；目标不存在时 0 行 → ErrLinkTargetMissing（业务错误，不是 500）。
func TestSQLStore_SaveLinks(t *testing.T) {
	st, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO model_links")).
		WithArgs("acme/gpt-x", int64(300), 240.0, 190.0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	saved, err := st.SaveLinks(context.Background(), []Link{
		{InternalRef: "acme/gpt-x", ExternalModelID: 300, PosX: 240, PosY: 190},
	})
	if err != nil || saved != 1 {
		t.Fatalf("saved=%d err=%v", saved, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("期望未满足：%v", err)
	}
}

// 目标不存在：整批回滚 + 指名是哪一条（运营要能照着改）。
func TestSQLStore_SaveLinks_TargetMissing(t *testing.T) {
	st, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO model_links")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	_, err := st.SaveLinks(context.Background(), []Link{{InternalRef: "nope", ExternalModelID: 999}})
	if !errors.Is(err, ErrLinkTargetMissing) {
		t.Fatalf("err = %v，期望 ErrLinkTargetMissing", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("期望未满足：%v", err)
	}
}
