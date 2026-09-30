package channel

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

// channelKeyRow 构造 channel_keys 查询行；列顺序与 channelKeyCols / DDL 一致。
func channelKeyRow(k *ChannelKey) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "channel_id", "name", "credential_enc", "state", "last_err", "deleted_at", "created_at", "updated_at"}).
		AddRow(k.ID, k.ChannelID, k.Name, k.CredentialEnc, string(k.State), k.LastErr, k.DeletedAt, k.CreatedAt, k.UpdatedAt)
}

func TestKeyStoreInsert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ks := NewKeyStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO channel_keys(channel_id, name, credential_enc) VALUES($1,$2,$3) RETURNING id`)).
		WithArgs(int64(1), "默认", "enc").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))

	id, err := ks.Insert(context.Background(), ChannelKey{ChannelID: 1, Name: "默认", CredentialEnc: "enc"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id != 7 {
		t.Fatalf("unexpected id: %d", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyStoreInsert_UniqueViolation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ks := NewKeyStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO channel_keys(channel_id, name, credential_enc) VALUES($1,$2,$3) RETURNING id`)).
		WithArgs(int64(1), "默认", "enc").
		WillReturnError(&pgconn.PgError{Code: "23505"})

	_, err = ks.Insert(context.Background(), ChannelKey{ChannelID: 1, Name: "默认", CredentialEnc: "enc"})
	if !errors.Is(err, ErrNameExists) {
		t.Fatalf("expected ErrNameExists, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyStoreList(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ks := NewKeyStore(db)

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE channel_id=$1 AND deleted_at IS NULL ORDER BY id`)).
		WithArgs(int64(1)).
		WillReturnRows(channelKeyRow(&ChannelKey{
			ID: 7, ChannelID: 1, Name: "默认", CredentialEnc: "enc", State: StateNormal,
			CreatedAt: now, UpdatedAt: now,
		}))

	list, err := ks.List(context.Background(), 1)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != 7 || list[0].Name != "默认" || list[0].State != StateNormal {
		t.Fatalf("unexpected keys: %+v", list)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyStoreListAll(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ks := NewKeyStore(db)

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE deleted_at IS NULL ORDER BY channel_id, id`)).
		WithArgs().
		WillReturnRows(channelKeyRow(&ChannelKey{
			ID: 7, ChannelID: 1, Name: "默认", CredentialEnc: "enc", State: StateNormal,
			CreatedAt: now, UpdatedAt: now,
		}))

	list, err := ks.ListAll(context.Background())
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(list) != 1 || list[0].Name != "默认" {
		t.Fatalf("unexpected keys: %+v", list)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyStoreGetByID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ks := NewKeyStore(db)

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(7)).
		WillReturnRows(channelKeyRow(&ChannelKey{
			ID: 7, ChannelID: 1, Name: "默认", CredentialEnc: "enc", State: StateDrain, LastErr: "boom",
			CreatedAt: now, UpdatedAt: now,
		}))

	k, err := ks.GetByID(context.Background(), 7)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if k.ID != 7 || k.State != StateDrain || k.LastErr != "boom" {
		t.Fatalf("unexpected key: %+v", k)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyStoreGetByID_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ks := NewKeyStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "credential_enc", "state", "last_err", "deleted_at", "created_at", "updated_at"}))

	_, err = ks.GetByID(context.Background(), 42)
	if err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyStoreGetByChannelName(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ks := NewKeyStore(db)

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT `+channelKeyCols+` FROM channel_keys WHERE channel_id=$1 AND name=$2 AND deleted_at IS NULL`)).
		WithArgs(int64(1), "默认").
		WillReturnRows(channelKeyRow(&ChannelKey{
			ID: 7, ChannelID: 1, Name: "默认", CredentialEnc: "enc", State: StateNormal,
			CreatedAt: now, UpdatedAt: now,
		}))

	k, err := ks.GetByChannelName(context.Background(), 1, "默认")
	if err != nil {
		t.Fatalf("get by channel name: %v", err)
	}
	if k.ID != 7 || k.ChannelID != 1 || k.Name != "默认" {
		t.Fatalf("unexpected key: %+v", k)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyStoreUpdateInfo(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ks := NewKeyStore(db)

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET name=$1, credential_enc=$2, updated_at=now() WHERE id=$3`)).
		WithArgs("new", "enc2", int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := ks.UpdateInfo(context.Background(), 7, "new", "enc2", true); err != nil {
		t.Fatalf("update info (with cred): %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET name=$1, updated_at=now() WHERE id=$2`)).
		WithArgs("new", int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := ks.UpdateInfo(context.Background(), 7, "new", "", false); err != nil {
		t.Fatalf("update info (no cred): %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyStoreUpdateInfo_UniqueViolation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ks := NewKeyStore(db)

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET name=$1, updated_at=now() WHERE id=$2`)).
		WithArgs("dup", int64(7)).
		WillReturnError(&pgconn.PgError{Code: "23505"})

	err = ks.UpdateInfo(context.Background(), 7, "dup", "", false)
	if !errors.Is(err, ErrNameExists) {
		t.Fatalf("expected ErrNameExists, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyStoreSetState(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ks := NewKeyStore(db)

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET state=$1, last_err=$2, updated_at=now() WHERE id=$3`)).
		WithArgs("DISABLED", "auth_failure", int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))

	if err := ks.SetState(context.Background(), 7, StateDisabled, "auth_failure"); err != nil {
		t.Fatalf("set state: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyStoreSoftDelete(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ks := NewKeyStore(db)

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET deleted_at=now() WHERE id=$1`)).
		WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))

	if err := ks.SoftDelete(context.Background(), 7); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyStoreInsertEvent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ks := NewKeyStore(db)

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO channel_key_events(channel_key_id, from_state, to_state, reason) VALUES($1,$2,$3,$4)`)).
		WithArgs(int64(7), "NORMAL", "DISABLED", "auth_failure").WillReturnResult(sqlmock.NewResult(10, 1))

	if err := ks.InsertEvent(context.Background(), 7, StateNormal, StateDisabled, "auth_failure"); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyStoreListEvents(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	ks := NewKeyStore(db)

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT `+channelKeyEventCols+` FROM channel_key_events WHERE channel_key_id=$1 ORDER BY created_at DESC LIMIT $2`)).
		WithArgs(int64(7), 50).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_key_id", "from_state", "to_state", "reason", "created_at"}).
			AddRow(int64(10), int64(7), "NORMAL", "DISABLED", "auth_failure", now))

	events, err := ks.ListEvents(context.Background(), 7, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].ID != 10 || events[0].ToState != StateDisabled {
		t.Fatalf("unexpected events: %+v", events)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
