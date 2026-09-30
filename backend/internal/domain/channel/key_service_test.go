package channel

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/team/llmgateway/internal/pkg/crypto"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// credentialEncMatcher 校验落库凭据：既非明文，又能被 SM4 解回原文（往返断言）。
type credentialEncMatcher struct {
	key  []byte
	want string
}

func (m credentialEncMatcher) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok || s == m.want {
		return false
	}
	plain, err := crypto.SM4Decrypt(m.key, s)
	return err == nil && string(plain) == m.want
}

const (
	keyInsertSQL = `INSERT INTO channel_keys(channel_id, name, credential_enc) VALUES($1,$2,$3) RETURNING id`
	keyGetSQL    = `SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`
	keySetSQL    = `UPDATE channel_keys SET state=$1, last_err=$2, updated_at=now() WHERE id=$3`
	keyEventSQL  = `INSERT INTO channel_key_events(channel_key_id, from_state, to_state, reason) VALUES($1,$2,$3,$4)`
)

func TestKeyService_Create_EncryptAndInsert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewKeyService(NewKeyStore(db), testSM4Key)

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(keyInsertSQL)).
		WithArgs(int64(1), "主", credentialEncMatcher{key: testSM4Key, want: "sk-secret"}).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))

	mock.ExpectQuery(regexp.QuoteMeta(keyGetSQL)).
		WithArgs(int64(7)).
		WillReturnRows(channelKeyRow(&ChannelKey{
			ID: 7, ChannelID: 1, Name: "主", CredentialEnc: "cipher", State: StateNormal,
			CreatedAt: now, UpdatedAt: now,
		}))

	k, err := svc.Create(context.Background(), 1, "主", "sk-secret")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if k.ID != 7 || k.ChannelID != 1 || k.Name != "主" || k.State != StateNormal {
		t.Fatalf("unexpected key: %+v", k)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyService_Create_Validation(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewKeyService(NewKeyStore(db), testSM4Key)

	for _, tc := range []struct {
		name string
		cred string
	}{{name: "", cred: "sk"}, {name: "主", cred: ""}} {
		_, err := svc.Create(context.Background(), 1, tc.name, tc.cred)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeBadRequest {
			t.Fatalf("expected 40001 for name=%q cred=%q, got %v", tc.name, tc.cred, err)
		}
	}
}

func TestKeyService_Create_UniqueConflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewKeyService(NewKeyStore(db), testSM4Key)

	mock.ExpectQuery(regexp.QuoteMeta(keyInsertSQL)).
		WithArgs(int64(1), "主", sqlmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23505"})

	_, err = svc.Create(context.Background(), 1, "主", "sk-secret")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeConflict {
		t.Fatalf("expected 40901, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyService_Update(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewKeyService(NewKeyStore(db), testSM4Key)
	now := time.Now()

	// 分支一：仅改名，凭据为空不改。
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET name=$1, updated_at=now() WHERE id=$2`)).
		WithArgs("新名", int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(keyGetSQL)).
		WithArgs(int64(7)).
		WillReturnRows(channelKeyRow(&ChannelKey{
			ID: 7, ChannelID: 1, Name: "新名", CredentialEnc: "cipher", State: StateNormal,
			CreatedAt: now, UpdatedAt: now,
		}))

	k, err := svc.Update(context.Background(), 7, "新名", "")
	if err != nil {
		t.Fatalf("update name only: %v", err)
	}
	if k.Name != "新名" || k.State != StateNormal {
		t.Fatalf("unexpected key: %+v", k)
	}

	// 分支二：改名且换凭据（加密规则与 updateCred=true 一致）。
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET name=$1, credential_enc=$2, updated_at=now() WHERE id=$3`)).
		WithArgs("新名2", credentialEncMatcher{key: testSM4Key, want: "new-secret"}, int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(keyGetSQL)).
		WithArgs(int64(7)).
		WillReturnRows(channelKeyRow(&ChannelKey{
			ID: 7, ChannelID: 1, Name: "新名2", CredentialEnc: "cipher2", State: StateNormal,
			CreatedAt: now, UpdatedAt: now,
		}))

	k, err = svc.Update(context.Background(), 7, "新名2", "new-secret")
	if err != nil {
		t.Fatalf("update name+cred: %v", err)
	}
	if k.Name != "新名2" {
		t.Fatalf("unexpected key: %+v", k)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyService_Update_EmptyName(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewKeyService(NewKeyStore(db), testSM4Key)

	_, err = svc.Update(context.Background(), 7, "", "sk")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeBadRequest {
		t.Fatalf("expected 40001, got %v", err)
	}
}

func TestKeyService_Update_UniqueConflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewKeyService(NewKeyStore(db), testSM4Key)

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET name=$1, updated_at=now() WHERE id=$2`)).
		WithArgs("dup", int64(7)).
		WillReturnError(&pgconn.PgError{Code: "23505"})

	_, err = svc.Update(context.Background(), 7, "dup", "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeConflict {
		t.Fatalf("expected 40901, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyService_Delete(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewKeyService(NewKeyStore(db), testSM4Key)

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET deleted_at=now() WHERE id=$1`)).
		WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))

	if err := svc.Delete(context.Background(), 7); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyService_List(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewKeyService(NewKeyStore(db), testSM4Key)
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE channel_id=$1 AND deleted_at IS NULL ORDER BY id`)).
		WithArgs(int64(1)).
		WillReturnRows(channelKeyRow(&ChannelKey{
			ID: 7, ChannelID: 1, Name: "主", CredentialEnc: "enc", State: StateNormal,
			CreatedAt: now, UpdatedAt: now,
		}))

	keys, err := svc.List(context.Background(), 1)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != 7 || keys[0].Name != "主" {
		t.Fatalf("unexpected keys: %+v", keys)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyService_ForceState(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		action string
		from   State
		to     State
		reason string
	}{
		{"normal", ActionNormal, StateDrain, StateNormal, reasonManualNormal},
		{"drain", ActionDrain, StateNormal, StateDrain, reasonManualDrain},
		{"disable", ActionDisable, StateNormal, StateDisabled, reasonManualDisable},
		{"recover", ActionRecover, StateDisabled, StateNormal, "manual recover"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			svc := NewKeyService(NewKeyStore(db), testSM4Key)

			mock.ExpectQuery(regexp.QuoteMeta(keyGetSQL)).
				WithArgs(int64(7)).
				WillReturnRows(channelKeyRow(&ChannelKey{
					ID: 7, ChannelID: 1, Name: "k", CredentialEnc: "enc", State: tc.from,
					CreatedAt: now, UpdatedAt: now,
				}))
			mock.ExpectExec(regexp.QuoteMeta(keySetSQL)).
				WithArgs(string(tc.to), "", int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(regexp.QuoteMeta(keyEventSQL)).
				WithArgs(int64(7), string(tc.from), string(tc.to), tc.reason).
				WillReturnResult(sqlmock.NewResult(10, 1))
			mock.ExpectQuery(regexp.QuoteMeta(keyGetSQL)).
				WithArgs(int64(7)).
				WillReturnRows(channelKeyRow(&ChannelKey{
					ID: 7, ChannelID: 1, Name: "k", CredentialEnc: "enc", State: tc.to,
					CreatedAt: now, UpdatedAt: now,
				}))

			k, err := svc.ForceState(context.Background(), 7, tc.action)
			if err != nil {
				t.Fatalf("force state: %v", err)
			}
			if k.State != tc.to {
				t.Fatalf("expected %s, got %s", tc.to, k.State)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("mock expectations: %v", err)
			}
		})
	}
}

func TestKeyService_ForceState_HomomorphicNoop(t *testing.T) {
	// 同态直接返回：不触发 SetState 与 InsertEvent。
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewKeyService(NewKeyStore(db), testSM4Key)
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta(keyGetSQL)).
		WithArgs(int64(7)).
		WillReturnRows(channelKeyRow(&ChannelKey{
			ID: 7, ChannelID: 1, Name: "k", CredentialEnc: "enc", State: StateDisabled,
			CreatedAt: now, UpdatedAt: now,
		}))

	k, err := svc.ForceState(context.Background(), 7, ActionDisable)
	if err != nil {
		t.Fatalf("force state: %v", err)
	}
	if k.State != StateDisabled {
		t.Fatalf("expected DISABLED, got %s", k.State)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyService_ForceState_InvalidAction(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewKeyService(NewKeyStore(db), testSM4Key)

	_, err = svc.ForceState(context.Background(), 7, "purge")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeBadRequest {
		t.Fatalf("expected 40001, got %v", err)
	}
}

func TestKeyService_ForceState_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewKeyService(NewKeyStore(db), testSM4Key)

	mock.ExpectQuery(regexp.QuoteMeta(keyGetSQL)).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "credential_enc", "state", "last_err", "deleted_at", "created_at", "updated_at"}))

	_, err = svc.ForceState(context.Background(), 42, ActionDrain)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeNotFound {
		t.Fatalf("expected 40401, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestKeyService_ListEvents(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewKeyService(NewKeyStore(db), testSM4Key)
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT `+channelKeyEventCols+` FROM channel_key_events WHERE channel_key_id=$1 ORDER BY created_at DESC LIMIT $2`)).
		WithArgs(int64(7), 50).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_key_id", "from_state", "to_state", "reason", "created_at"}).
			AddRow(int64(10), int64(7), "NORMAL", "DISABLED", "manual_disable", now))

	events, err := svc.ListEvents(context.Background(), 7, 0)
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
