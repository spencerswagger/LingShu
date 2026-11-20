// keystore_test.go 覆盖 magpie_keys 的 SQL 层（写入方的真实语句）：
// Provision 的 upsert 语义、Disable 的行数判断、ByTokenID 的未绑定口径。
package magpie

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

var (
	provisionStmt = regexp.MustCompile(`INSERT INTO magpie_keys`)
	activeSQL     = regexp.MustCompile(`FROM magpie_keys WHERE token_id = \$1 AND state = 'ACTIVE'`)
	anyByTokenSQL = regexp.MustCompile(`FROM magpie_keys WHERE token_id = \$1`)
	disableStmt   = regexp.MustCompile(`UPDATE magpie_keys SET state = 'DISABLED'`)
)

func keyColumns() []string {
	return []string{"id", "token_id", "key_name", "legacy_key_name", "key_id", "key_cipher", "state"}
}

// Provision 走 ON CONFLICT (token_id)：重复绑定只更新，不新增行。
func TestKeyStore_ProvisionUpserts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewKeyStore(db)

	mock.ExpectQuery(provisionStmt.String()).
		WithArgs(sqlmock.AnyArg(), int64(7), "core-k1", "cipher-text").
		WillReturnRows(sqlmock.NewRows(keyColumns()).AddRow(int64(1), int64(7), "core-k1", "", "", "cipher-text", KeyStateActive))

	k, err := s.Provision(context.Background(), 7, "core-k1", "cipher-text")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if k.TokenID != 7 || k.KeyName != "core-k1" || k.State != KeyStateActive {
		t.Fatalf("返回=%+v", k)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

// ActiveByTokenID：无映射 → ErrKeyNotProvisioned（不是 nil,nil）。
func TestKeyStore_ActiveByTokenID_NotProvisioned(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewKeyStore(db)

	mock.ExpectQuery(activeSQL.String()).WithArgs(int64(7)).WillReturnError(sql.ErrNoRows)
	if _, err := s.ActiveByTokenID(context.Background(), 7); !errors.Is(err, ErrKeyNotProvisioned) {
		t.Fatalf("err=%v", err)
	}
}

// ByTokenID 不筛 state：停用后仍能查到（管理端复核需要）。
func TestKeyStore_ByTokenID_IncludesDisabled(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewKeyStore(db)

	mock.ExpectQuery(anyByTokenSQL.String()).WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows(keyColumns()).AddRow(int64(1), int64(7), "core-k1", "", "", "cipher", KeyStateDisabled))
	k, err := s.ByTokenID(context.Background(), 7)
	if err != nil {
		t.Fatalf("ByTokenID: %v", err)
	}
	if k.State != KeyStateDisabled {
		t.Fatalf("state=%s", k.State)
	}
}

// Disable：影响 0 行 = 没有映射 → ErrKeyNotProvisioned。
func TestKeyStore_Disable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewKeyStore(db)

	mock.ExpectExec(disableStmt.String()).WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := s.Disable(context.Background(), 7); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	mock.ExpectExec(disableStmt.String()).WithArgs(int64(8)).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := s.Disable(context.Background(), 8); !errors.Is(err, ErrKeyNotProvisioned) {
		t.Fatalf("err=%v", err)
	}
}

// Provision 保留 key_id 列（POC 未证实字段名，读回时允许为空）。
func TestKeyStore_ProvisionKeepsKeyID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewKeyStore(db)

	mock.ExpectQuery(provisionStmt.String()).WithArgs(sqlmock.AnyArg(), int64(9), "k2", "c").
		WillReturnRows(sqlmock.NewRows(keyColumns()).AddRow(int64(2), int64(9), "k2", "", "remote-id", "c", KeyStateActive))
	k, err := s.Provision(context.Background(), 9, "k2", "c")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if k.KeyID != "remote-id" {
		t.Fatalf("key_id=%q", k.KeyID)
	}
}

var renameStmt = regexp.MustCompile(`UPDATE magpie_keys SET key_name = \$3`)

// renameColumns 与 keyColumns 同形状（+legacy_key_name）。
func TestKeyStore_RenameAndActivate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewKeyStore(db)

	// rename + activate 一条 UPDATE：旧名落 legacy_key_name、新名生效、state ACTIVE。
	mock.ExpectQuery(renameStmt.String()).
		WithArgs(int64(7), "ls-token-7", "ls-u1-t7-alpha", "new-cipher").
		WillReturnRows(sqlmock.NewRows(keyColumns()).
			AddRow(int64(1), int64(7), "ls-u1-t7-alpha", "ls-token-7", "", "new-cipher", KeyStateActive))

	k, err := s.RenameAndActivate(context.Background(), 7, "ls-token-7", "ls-u1-t7-alpha", "new-cipher")
	if err != nil {
		t.Fatalf("RenameAndActivate: %v", err)
	}
	if k.KeyName != "ls-u1-t7-alpha" || k.LegacyKeyName != "ls-token-7" || k.State != KeyStateActive {
		t.Fatalf("返回=%+v", k)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

// 无行（DISABLED 或不存在）→ ErrKeyNotProvisioned，不是 nil,nil。
func TestKeyStore_RenameAndActivate_NotProvisioned(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewKeyStore(db)
	mock.ExpectQuery(renameStmt.String()).
		WithArgs(int64(7), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)
	if _, err := s.RenameAndActivate(context.Background(), 7, "o", "n", "c"); !errors.Is(err, ErrKeyNotProvisioned) {
		t.Fatalf("err=%v", err)
	}
}

// scanKey 全列扫描：legacy_key_name 空 → 空串（NOT NULL DEFAULT ”）。
func TestKeyStore_ByTokenID_ScansLegacyName(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewKeyStore(db)
	mock.ExpectQuery(anyByTokenSQL.String()).WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows(keyColumns()).
			AddRow(int64(1), int64(7), "ls-u1-t7-x", "ls-token-7", "", "c", KeyStateActive))
	k, err := s.ByTokenID(context.Background(), 7)
	if err != nil {
		t.Fatalf("ByTokenID: %v", err)
	}
	if k.LegacyKeyName != "ls-token-7" {
		t.Fatalf("legacy 名未扫到：%+v", k)
	}
}

// NamesByTokenIDs 批量取名：有映射的进 map，无映射的缺席（展示层按空串渲染）。
func TestKeyStore_NamesByTokenIDs(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewKeyStore(db)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT token_id, key_name FROM magpie_keys WHERE token_id IN ($1,$2,$3)`)).
		WithArgs(int64(7), int64(8), int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"token_id", "key_name"}).
			AddRow(int64(7), "ls-u1-t7-alpha").
			AddRow(int64(8), "ops-manual"))
	names, err := s.NamesByTokenIDs(context.Background(), []int64{7, 8, 9})
	if err != nil {
		t.Fatalf("NamesByTokenIDs: %v", err)
	}
	if names[7] != "ls-u1-t7-alpha" || names[8] != "ops-manual" {
		t.Fatalf("names = %v", names)
	}
	if _, present := names[9]; present {
		t.Fatalf("无映射令牌不得出现在 map：%v", names)
	}
}

// NamesByTokenIDs 空入参直接返回空 map（不发 SQL）。
func TestKeyStore_NamesByTokenIDs_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewKeyStore(db)
	names, err := s.NamesByTokenIDs(context.Background(), nil)
	if err != nil {
		t.Fatalf("NamesByTokenIDs: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("names = %v", names)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("空入参不应发 SQL：%v", err)
	}
}
