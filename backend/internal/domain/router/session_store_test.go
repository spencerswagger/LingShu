package router

import (
	"context"
	"database/sql/driver"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// sqlmockArrayConverter 透传 []string（ANY($1) 数组参数），其余类型委托标准转换。
type sqlmockArrayConverter struct{}

func (sqlmockArrayConverter) ConvertValue(v any) (driver.Value, error) {
	if _, ok := v.([]string); ok {
		return v, nil
	}
	return driver.DefaultParameterConverter.ConvertValue(v)
}

// newSessionStore 构造 *SessionStore + sqlmock（支持 []string 数组参数断言）。
func newSessionStore(t *testing.T) (*SessionStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.ValueConverterOption(sqlmockArrayConverter{}))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewSessionStore(db), mock
}

func TestSessionStoreUpsert(t *testing.T) {
	store, mock := newSessionStore(t)
	ctx := context.Background()
	now := time.Now()
	sess := &Session{
		SessionID:       "abc",
		UserID:          1,
		TokenID:         2,
		Model:           "qw-max",
		SessionRaw:      "sess",
		ChannelKeyID:    7,
		InternalModelID: "int1",
		CreatedAt:       now,
		LastActive:      now,
		ExpireAt:        now.Add(time.Hour),
	}

	mock.ExpectExec(regexp.QuoteMeta(sessionUpsertSQL)).
		WithArgs("abc", int64(1), int64(2), "qw-max", "sess", int64(7), "int1", "", now, now, now.Add(time.Hour)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.Upsert(ctx, sess); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestSessionStoreUpsert_NullableToken(t *testing.T) {
	store, mock := newSessionStore(t)
	ctx := context.Background()
	now := time.Now()
	sess := &Session{
		SessionID:       "abc",
		UserID:          1,
		Model:           "qw-max",
		ChannelKeyID:    7,
		InternalModelID: "int1",
		CreatedAt:       now,
		LastActive:      now,
		ExpireAt:        now.Add(time.Hour),
	}

	mock.ExpectExec(regexp.QuoteMeta(sessionUpsertSQL)).
		WithArgs("abc", int64(1), nil, "qw-max", "", int64(7), "int1", "", now, now, now.Add(time.Hour)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.Upsert(ctx, sess); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestSessionStoreUpsert_UsesChannelKeyOnly D3 起无 KeyID()/ChannelID 回退：Upsert 唯一取 ChannelKeyID。
func TestSessionStoreUpsert_UsesChannelKeyOnly(t *testing.T) {
	store, mock := newSessionStore(t)
	ctx := context.Background()
	now := time.Now()
	sess := &Session{
		SessionID:       "abc",
		UserID:          1,
		Model:           "qw-max",
		ChannelKeyID:    7,
		InternalModelID: "int1",
		CreatedAt:       now,
		LastActive:      now,
		ExpireAt:        now.Add(time.Hour),
	}

	mock.ExpectExec(regexp.QuoteMeta(sessionUpsertSQL)).
		WithArgs("abc", int64(1), nil, "qw-max", "", int64(7), "int1", "", now, now, now.Add(time.Hour)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.Upsert(ctx, sess); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestSessionStoreDelete(t *testing.T) {
	store, mock := newSessionStore(t)
	ctx := context.Background()

	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM sessions WHERE session_id=$1`)).
		WithArgs("abc").WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.Delete(ctx, "abc"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestSessionStoreDeleteMany(t *testing.T) {
	store, mock := newSessionStore(t)
	ctx := context.Background()

	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM sessions WHERE session_id = ANY($1)`)).
		WithArgs([]string{"a", "b", "c"}).WillReturnResult(sqlmock.NewResult(0, 3))

	if err := store.DeleteMany(ctx, []string{"a", "b", "c"}); err != nil {
		t.Fatalf("delete many: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestSessionStoreDeleteMany_EmptyIsNoop(t *testing.T) {
	store, _ := newSessionStore(t)
	ctx := context.Background()

	if err := store.DeleteMany(ctx, nil); err != nil {
		t.Fatalf("delete many(nil): %v", err)
	}
	if err := store.DeleteMany(ctx, []string{}); err != nil {
		t.Fatalf("delete many(empty): %v", err)
	}
}

func TestSessionStoreLoadActive(t *testing.T) {
	store, mock := newSessionStore(t)
	ctx := context.Background()
	now := time.Now()

	rows := sqlmock.NewRows([]string{"session_id", "user_id", "token_id", "model", "session_raw",
		"channel_key_id", "internal_model_id", "created_at", "last_active", "expire_at", "name", "closed"}).
		AddRow("abc", int64(1), int64(2), "qw-max", "sess", int64(7), "int1", now, now, now.Add(time.Hour), "今天的会话", false).
		AddRow("def", int64(2), nil, "qw-max", "", int64(8), "int2", now, now, now.Add(2*time.Hour), "", true)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + sessionCols + ` FROM sessions WHERE expire_at > $1`)).
		WithArgs(now).WillReturnRows(rows)

	list, err := store.LoadActive(ctx, now)
	if err != nil {
		t.Fatalf("load active: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expect 2 rows, got %d", len(list))
	}
	if list[0].SessionID != "abc" || list[0].TokenID != 2 || list[0].ChannelKeyID != 7 || list[0].InternalModelID != "int1" {
		t.Fatalf("unexpected first session: %+v", list[0])
	}
	// D3：Session 无 ChannelID/KeyID() 回退，LoadActive 不产生误导性渠道值，会话维度只有 channel_key_id。
	if list[1].SessionID != "def" || list[1].TokenID != 0 || list[1].ChannelKeyID != 8 || !list[1].Closed {
		t.Fatalf("unexpected second session: %+v", list[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
