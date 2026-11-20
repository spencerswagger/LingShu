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

// TestKeyStore_NameOccupied_AnchorsSQL 评审追加 P1 的口径锚：NameOccupied 的查询
// **不得排停用行**（0019b 的 key_name 是全列 UNIQUE、不排 DISABLED；查询若排除
// 停用行，会出现「判空闲但 Provision 撞 23505」）。核心谓词全文锚定（不引用实现
// 常量，防同源假绿），并锚定停用行占名即占用（count=1 → true）。
func TestKeyStore_NameOccupied_AnchorsSQL(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := NewKeyStore(db)

	// 全文锚定：谓词逐字一致，任何把 state 过滤加回来的改动都会红
	//（\s+ 桥接源码换行缩进，不依赖缩进格式）。
	// 尾部 \$ 行尾锚：任何在 token_id <> $2 之后追加的谓词（如评审缝的
	// state <> 'DISABLED'）都不再匹配——子串匹配会放过它。
	nameOccupiedSQL := `SELECT count\(\*\) FROM magpie_keys\s+WHERE \(key_name = \$1 OR legacy_key_name = \$1\) AND token_id <> \$2$`
	mock.ExpectQuery(nameOccupiedSQL).WithArgs("alice-bot", int64(102)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))
	occupied, err := store.NameOccupied(context.Background(), "alice-bot", 102)
	if err != nil || !occupied {
		t.Fatalf("占用行（含停用）必须判占用：occupied=%v err=%v", occupied, err)
	}

	// 排除本令牌：行属于自己 → 空闲。
	mock.ExpectQuery(nameOccupiedSQL).WithArgs("alice-bot", int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	occupied, err = store.NameOccupied(context.Background(), "alice-bot", 7)
	if err != nil || occupied {
		t.Fatalf("排除本令牌后应空闲：occupied=%v err=%v", occupied, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

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

// renameAndActivateStmt 是 RenameAndActivate 完整语句的**逐字拷贝**（含 keyScanCols 展开）。
// 故意不引用 renameAndActivateSQL 常量：期望与实现同源会让「改实现」的变异也改掉期望（假绿）。
// 全文 ^…$ 锚定后，任何谓词/兜底列写入被静默改写（如 legacy_key_name = $2 改成 ”）
// 都会因不再匹配而红 —— 这是评审 P2 指出的「用例只钉 SQL 前缀」缺口的堵法。
var renameAndActivateStmt = regexp.MustCompile(`^UPDATE magpie_keys SET key_name = \$3, legacy_key_name = \$2, key_cipher = \$4,
		        state = 'ACTIVE', last_synced_at = now\(\), last_error = NULL, updated_at = now\(\)
		 WHERE token_id = \$1 AND state <> 'DISABLED'
		 RETURNING id, token_id, key_name, coalesce\(legacy_key_name,''\), coalesce\(key_id,''\), coalesce\(key_cipher,''\), state$`)

// renameColumns 与 keyColumns 同形状（+legacy_key_name）。
func TestKeyStore_RenameAndActivate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewKeyStore(db)

	// rename + activate 一条 UPDATE：旧名落 legacy_key_name、新名生效、state ACTIVE。
	// WithArgs 逐位钉死 $2=旧名（兜底列写入值）——参数位序错或写空串都会红。
	mock.ExpectQuery(renameAndActivateStmt.String()).
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
	mock.ExpectQuery(renameAndActivateStmt.String()).
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

// TestKeyStateLiterals 状态字面量锚（identity.backfill 的 disabled 分桶用同值字符串比较，
// 两侧漂移会让「停用行跳过」静默失效——按字面量钉死）。
func TestKeyStateLiterals(t *testing.T) {
	if KeyStateActive != "ACTIVE" || KeyStateDisabled != "DISABLED" {
		t.Fatalf("状态字面量漂移：ACTIVE=%q DISABLED=%q", KeyStateActive, KeyStateDisabled)
	}
}
