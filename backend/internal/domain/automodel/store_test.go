package automodel

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
)

// autoTestCols 与 store.autoCols 一致，供构造 mock 行（用 Split 保证列顺序永远同步）。
var autoTestCols = strings.Split(autoCols, ", ")

// memberTestCols 与 loadMembersQ 的 SELECT 列顺序一致。
var memberTestCols = []string{"id", "auto_model_id", "external_model_id", "position", "external_name", "enabled"}

// membersSQL 是 loadMembersQ 的精确 SQL 文本（供 regexp.QuoteMeta 匹配）。
const membersSQL = `SELECT m.id, m.auto_model_id, m.external_model_id, m.position, em.external_name, em.enabled
		 FROM auto_model_members m
		 LEFT JOIN external_models em ON em.id = m.external_model_id
		 WHERE m.auto_model_id = ANY($1::bigint[]) AND m.deleted_at IS NULL
		 ORDER BY m.auto_model_id ASC, m.position ASC, m.id ASC`

func newStoreMock(t *testing.T) (*Store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db), mock
}

// autoRow 构造 auto_models 查询的一行（owner 为 nil 时写 NULL）。
func autoRow(id int64, name, scope string, owner *int64, enabled bool, now time.Time) *sqlmock.Rows {
	var o any
	if owner != nil {
		o = *owner
	}
	return sqlmock.NewRows(autoTestCols).
		AddRow(id, name, scope, o, enabled, "", now, now)
}

// --- GetByName ---

// ownerUserID 非空：SQL 含可见性谓词 (scope='global' OR owner_user_id=$2)，且只做 1 条主表查询。
func TestStore_GetByName_WithOwnerVisibility(t *testing.T) {
	s, mock := newStoreMock(t)
	now := time.Now()
	owner := int64(7)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT `+autoCols+` FROM auto_models
		 WHERE name = $1 AND deleted_at IS NULL AND (scope = 'global' OR owner_user_id = $2)`)).
		WithArgs("auto1", owner).
		WillReturnRows(autoRow(10, "auto1", ScopePrivate, &owner, true, now))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{10}").
		WillReturnRows(sqlmock.NewRows(memberTestCols).
			AddRow(int64(1), int64(10), int64(101), 0, "model-a", true))

	m, err := s.GetByName(context.Background(), "auto1", &owner)
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if m.ID != 10 || m.Name != "auto1" || m.OwnerUserID == nil || *m.OwnerUserID != 7 {
		t.Fatalf("unexpected auto: %+v", m)
	}
	if len(m.Members) != 1 || m.Members[0].ExternalName != "model-a" || !m.Members[0].Enabled {
		t.Fatalf("unexpected members: %+v", m.Members)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// ownerUserID 为 nil：SQL 只含 scope='global'，无 OR 子句、无第二个参数。
func TestStore_GetByName_NilOwnerGlobalOnly(t *testing.T) {
	s, mock := newStoreMock(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + autoCols + ` FROM auto_models
		 WHERE name = $1 AND deleted_at IS NULL AND (scope = 'global')`)).
		WithArgs("auto1").
		WillReturnRows(autoRow(10, "auto1", ScopeGlobal, nil, true, now))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{10}").
		WillReturnRows(sqlmock.NewRows(memberTestCols))

	m, err := s.GetByName(context.Background(), "auto1", nil)
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if m.OwnerUserID != nil {
		t.Fatalf("global should have nil owner, got %v", *m.OwnerUserID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// --- GetByNameAny ---

func TestStore_GetByNameAny(t *testing.T) {
	s, mock := newStoreMock(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + autoCols + ` FROM auto_models WHERE name = $1 AND deleted_at IS NULL`)).
		WithArgs("auto1").
		WillReturnRows(autoRow(10, "auto1", ScopeGlobal, nil, true, now))

	m, err := s.GetByNameAny(context.Background(), "auto1")
	if err != nil {
		t.Fatalf("get by name any: %v", err)
	}
	if m.ID != 10 {
		t.Fatalf("unexpected: %+v", m)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// --- List ---

// scope 空 + owner nil → 无 WHERE 附加条件，仅 deleted_at + ORDER BY。
func TestStore_List_NoFilter(t *testing.T) {
	s, mock := newStoreMock(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + autoCols + ` FROM auto_models WHERE deleted_at IS NULL ORDER BY id ASC`)).
		WillReturnRows(sqlmock.NewRows(autoTestCols))

	list, err := s.List(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("want empty, got %+v", list)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// scope 与 owner 同时传 → 两谓词以 AND 合并（scope=private + owner=7 → 用户 7 的私有）。
func TestStore_List_ScopeAndOwnerCombined(t *testing.T) {
	s, mock := newStoreMock(t)
	now := time.Now()
	owner := int64(7)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT `+autoCols+` FROM auto_models WHERE deleted_at IS NULL AND scope = $1 AND (scope = 'global' OR owner_user_id = $2) ORDER BY id ASC`)).
		WithArgs(ScopePrivate, owner).
		WillReturnRows(autoRow(10, "auto1", ScopePrivate, &owner, true, now))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{10}").
		WillReturnRows(sqlmock.NewRows(memberTestCols).
			AddRow(int64(1), int64(10), int64(101), 0, "model-a", true))

	list, err := s.List(context.Background(), ScopePrivate, &owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != 10 {
		t.Fatalf("unexpected list: %+v", list)
	}
	if len(list[0].Members) != 1 {
		t.Fatalf("members not loaded: %+v", list[0].Members)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// owner 非空、scope 空 → 仅可见性谓词（global + 该 owner 的 private）。
func TestStore_List_OwnerVisibilityOnly(t *testing.T) {
	s, mock := newStoreMock(t)
	now := time.Now()
	owner := int64(7)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + autoCols + ` FROM auto_models WHERE deleted_at IS NULL AND (scope = 'global' OR owner_user_id = $1) ORDER BY id ASC`)).
		WithArgs(owner).
		WillReturnRows(autoRow(10, "auto1", ScopePrivate, &owner, true, now))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{10}").
		WillReturnRows(sqlmock.NewRows(memberTestCols).
			AddRow(int64(1), int64(10), int64(101), 0, "model-a", true))

	list, err := s.List(context.Background(), "", &owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != 10 {
		t.Fatalf("unexpected list: %+v", list)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// --- Insert ---

func TestStore_Insert_Success(t *testing.T) {
	s, mock := newStoreMock(t)
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO auto_models(id, name, scope, owner_user_id, enabled, description)
		 VALUES($1,$2,$3,$4,$5,$6) RETURNING `+autoCols)).
		WithArgs(sqlmock.AnyArg(), "auto1", ScopeGlobal, nil, true, "").
		WillReturnRows(autoRow(999, "auto1", ScopeGlobal, nil, true, now))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO auto_model_members(id, auto_model_id, external_model_id, position)
			 VALUES($1,$2,$3,$4)`)).
		WithArgs(sqlmock.AnyArg(), int64(999), int64(101), 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{999}").
		WillReturnRows(sqlmock.NewRows(memberTestCols).
			AddRow(int64(1), int64(999), int64(101), 0, "model-a", true))
	mock.ExpectCommit()

	created, err := s.Insert(context.Background(), &AutoModel{
		Name: "auto1", Scope: ScopeGlobal, Enabled: true,
		Members: []Member{{ExternalModelID: 101, Position: 0}},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if created.ID != 999 || len(created.Members) != 1 || created.Members[0].ExternalName != "model-a" {
		t.Fatalf("unexpected created: %+v", created)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestStore_Insert_NameExists(t *testing.T) {
	s, mock := newStoreMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO auto_models(id, name, scope, owner_user_id, enabled, description)
		 VALUES($1,$2,$3,$4,$5,$6) RETURNING `+autoCols)).
		WithArgs(sqlmock.AnyArg(), "dup", ScopeGlobal, nil, true, "").
		WillReturnError(&pgconn.PgError{Code: "23505"})
	mock.ExpectRollback()

	_, err := s.Insert(context.Background(), &AutoModel{Name: "dup", Scope: ScopeGlobal, Enabled: true})
	if !errors.Is(err, ErrNameExists) {
		t.Fatalf("want ErrNameExists, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestStore_Insert_ExternalModelMissing(t *testing.T) {
	s, mock := newStoreMock(t)
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO auto_models(id, name, scope, owner_user_id, enabled, description)
		 VALUES($1,$2,$3,$4,$5,$6) RETURNING `+autoCols)).
		WithArgs(sqlmock.AnyArg(), "auto1", ScopeGlobal, nil, true, "").
		WillReturnRows(autoRow(999, "auto1", ScopeGlobal, nil, true, now))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO auto_model_members(id, auto_model_id, external_model_id, position)
			 VALUES($1,$2,$3,$4)`)).
		WithArgs(sqlmock.AnyArg(), int64(999), int64(101), 0).
		WillReturnError(&pgconn.PgError{Code: "23503"})
	mock.ExpectRollback()

	_, err := s.Insert(context.Background(), &AutoModel{
		Name: "auto1", Scope: ScopeGlobal, Enabled: true,
		Members: []Member{{ExternalModelID: 101, Position: 0}},
	})
	if !errors.Is(err, ErrExternalModelMissing) {
		t.Fatalf("want ErrExternalModelMissing, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// --- Update ---

func TestStore_Update_Success(t *testing.T) {
	s, mock := newStoreMock(t)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE auto_models SET name=$1, scope=$2, owner_user_id=$3, enabled=$4, description=$5, updated_at=now()
		 WHERE id=$6 AND deleted_at IS NULL`)).
		WithArgs("auto1", ScopeGlobal, nil, true, "", int64(999)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE auto_model_members SET deleted_at=now() WHERE auto_model_id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(999)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO auto_model_members(id, auto_model_id, external_model_id, position)
			 VALUES($1,$2,$3,$4)`)).
		WithArgs(sqlmock.AnyArg(), int64(999), int64(101), 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err := s.Update(context.Background(), &AutoModel{
		ID: 999, Name: "auto1", Scope: ScopeGlobal, Enabled: true,
		Members: []Member{{ExternalModelID: 101, Position: 0}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestStore_Update_NoRows(t *testing.T) {
	s, mock := newStoreMock(t)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE auto_models SET name=$1, scope=$2, owner_user_id=$3, enabled=$4, description=$5, updated_at=now()
		 WHERE id=$6 AND deleted_at IS NULL`)).
		WithArgs("auto1", ScopeGlobal, nil, true, "", int64(999)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	err := s.Update(context.Background(), &AutoModel{ID: 999, Name: "auto1", Scope: ScopeGlobal, Enabled: true})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("want sql.ErrNoRows, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// --- Delete ---

func TestStore_Delete_Success(t *testing.T) {
	s, mock := newStoreMock(t)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE auto_models SET deleted_at=now() WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(999)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE auto_model_members SET deleted_at=now() WHERE auto_model_id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(999)).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()

	if err := s.Delete(context.Background(), 999); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestStore_Delete_NoRows(t *testing.T) {
	s, mock := newStoreMock(t)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE auto_models SET deleted_at=now() WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(999)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	if err := s.Delete(context.Background(), 999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("want sql.ErrNoRows, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// --- loadMembers ---

// LEFT JOIN 后 external_name / enabled 为 NULL → Member.ExternalName 空串、Enabled false；
// 同时验证 external_name/enabled 非空时正确回填。
func TestStore_LoadMembers_NullAndFilled(t *testing.T) {
	s, mock := newStoreMock(t)
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{10}").
		WillReturnRows(sqlmock.NewRows(memberTestCols).
			AddRow(int64(1), int64(10), int64(101), 0, "model-a", true).
			AddRow(int64(2), int64(10), int64(102), 1, nil, nil))

	out, err := s.loadMembers(context.Background(), []int64{10})
	if err != nil {
		t.Fatalf("load members: %v", err)
	}
	ms := out[10]
	if len(ms) != 2 {
		t.Fatalf("want 2 members, got %+v", ms)
	}
	if ms[0].ExternalName != "model-a" || !ms[0].Enabled || ms[0].Position != 0 {
		t.Fatalf("unexpected first member: %+v", ms[0])
	}
	if ms[1].ExternalName != "" || ms[1].Enabled {
		t.Fatalf("NULL external model should be empty/disabled, got %+v", ms[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
