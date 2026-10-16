package automodel

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/team/llmgateway/internal/pkg/resp"
)

const (
	getByNameAnySQL = `SELECT ` + autoCols + ` FROM auto_models WHERE name = $1 AND deleted_at IS NULL`
	getByNameSQL    = `SELECT ` + autoCols + ` FROM auto_models
		 WHERE name = $1 AND deleted_at IS NULL AND (scope = 'global' OR owner_user_id = $2)`
	getByIDSQL = `SELECT ` + autoCols + ` FROM auto_models WHERE id = $1 AND deleted_at IS NULL`
)

func newServiceMock(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewService(NewStore(db)), mock
}

func int64Ptr(v int64) *int64 { return &v }

func wantAPIErr(t *testing.T, err error, code int) *APIError {
	t.Helper()
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError(code=%d), got %v", code, err)
	}
	if apiErr.Code != code {
		t.Fatalf("want code=%d, got %d (%s)", code, apiErr.Code, apiErr.Message)
	}
	return apiErr
}

// --- validateInput ---

func TestService_ValidateInput(t *testing.T) {
	s := NewService(nil)

	valid := func() *AutoModelInput {
		return &AutoModelInput{
			Name: "auto1", Scope: ScopeGlobal,
			Members: []MemberInput{{ExternalModelID: 1, Position: 0}},
		}
	}

	// 名称空
	in := valid()
	in.Name = ""
	wantAPIErr(t, s.validateInput(in), resp.CodeBadRequest)

	// scope 非法
	in = valid()
	in.Scope = "team"
	wantAPIErr(t, s.validateInput(in), resp.CodeBadRequest)

	// private 缺 owner
	in = valid()
	in.Scope = ScopePrivate
	wantAPIErr(t, s.validateInput(in), resp.CodeBadRequest)

	// private owner<=0
	in = valid()
	in.Scope = ScopePrivate
	in.OwnerUserID = int64Ptr(0)
	wantAPIErr(t, s.validateInput(in), resp.CodeBadRequest)

	// global 传了 owner → 归一为 nil
	in = valid()
	in.OwnerUserID = int64Ptr(7)
	if err := s.validateInput(in); err != nil {
		t.Fatalf("global with owner should pass: %v", err)
	}
	if in.OwnerUserID != nil {
		t.Fatalf("global owner should be normalized to nil, got %v", *in.OwnerUserID)
	}

	// 成员空
	in = valid()
	in.Members = nil
	wantAPIErr(t, s.validateInput(in), resp.CodeBadRequest)

	// 成员 ExternalModelID<=0
	in = valid()
	in.Members = []MemberInput{{ExternalModelID: 0, Position: 0}}
	wantAPIErr(t, s.validateInput(in), resp.CodeBadRequest)

	// position 重复
	in = valid()
	in.Members = []MemberInput{{ExternalModelID: 1, Position: 0}, {ExternalModelID: 2, Position: 0}}
	wantAPIErr(t, s.validateInput(in), resp.CodeBadRequest)

	// 稳定排序：按 position 升序
	in = valid()
	in.Members = []MemberInput{{ExternalModelID: 30, Position: 5}, {ExternalModelID: 10, Position: 1}, {ExternalModelID: 20, Position: 3}}
	if err := s.validateInput(in); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if in.Members[0].ExternalModelID != 10 || in.Members[1].ExternalModelID != 20 || in.Members[2].ExternalModelID != 30 {
		t.Fatalf("members not sorted by position: %+v", in.Members)
	}
}

// --- Create ---

// 跨表重名：external_models.external_name 已占用 → 40901。
func TestService_Create_ExternalNameConflict(t *testing.T) {
	s, _ := newServiceMock(t)
	s.SetExternalNameChecker(func(_ context.Context, _ string) (bool, error) { return true, nil })

	_, err := s.Create(context.Background(), AutoModelInput{
		Name: "auto1", Scope: ScopeGlobal,
		Members: []MemberInput{{ExternalModelID: 1, Position: 0}},
	})
	wantAPIErr(t, err, resp.CodeConflict)
}

// 同名 Auto：GetByNameAny 命中 → 40901。
func TestService_Create_AutoNameConflict(t *testing.T) {
	s, mock := newServiceMock(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(getByNameAnySQL)).
		WithArgs("auto1").
		WillReturnRows(autoRow(10, "auto1", ScopeGlobal, nil, true, now))

	_, err := s.Create(context.Background(), AutoModelInput{
		Name: "auto1", Scope: ScopeGlobal,
		Members: []MemberInput{{ExternalModelID: 1, Position: 0}},
	})
	wantAPIErr(t, err, resp.CodeConflict)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// 成功路径。
func TestService_Create_Success(t *testing.T) {
	s, mock := newServiceMock(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(getByNameAnySQL)).
		WithArgs("auto1").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO auto_models(id, name, scope, owner_user_id, enabled, description)
		 VALUES($1,$2,$3,$4,$5,$6) RETURNING `+autoCols)).
		WithArgs(sqlmock.AnyArg(), "auto1", ScopeGlobal, nil, true, "d").
		WillReturnRows(autoRow(999, "auto1", ScopeGlobal, nil, true, now))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO auto_model_members(id, auto_model_id, external_model_id, position)
			 VALUES($1,$2,$3,$4)`)).
		WithArgs(sqlmock.AnyArg(), int64(999), int64(1), 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{999}").
		WillReturnRows(sqlmock.NewRows(memberTestCols).
			AddRow(int64(1), int64(999), int64(1), 0, "model-a", true))
	mock.ExpectCommit()

	created, err := s.Create(context.Background(), AutoModelInput{
		Name: "auto1", Scope: ScopeGlobal, Description: "d",
		Members: []MemberInput{{ExternalModelID: 1, Position: 0}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID != 999 || len(created.Members) != 1 {
		t.Fatalf("unexpected created: %+v", created)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// store 返回外键冲突 → 40001（错误文案「成员引用的对外模型不存在」）。
func TestService_Create_ExternalModelMissing(t *testing.T) {
	s, mock := newServiceMock(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(getByNameAnySQL)).
		WithArgs("auto1").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO auto_models(id, name, scope, owner_user_id, enabled, description)
		 VALUES($1,$2,$3,$4,$5,$6) RETURNING `+autoCols)).
		WithArgs(sqlmock.AnyArg(), "auto1", ScopeGlobal, nil, true, "").
		WillReturnRows(autoRow(999, "auto1", ScopeGlobal, nil, true, now))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO auto_model_members(id, auto_model_id, external_model_id, position)
			 VALUES($1,$2,$3,$4)`)).
		WithArgs(sqlmock.AnyArg(), int64(999), int64(1), 0).
		WillReturnError(&pgconn.PgError{Code: "23503"})
	mock.ExpectRollback()

	_, err := s.Create(context.Background(), AutoModelInput{
		Name: "auto1", Scope: ScopeGlobal,
		Members: []MemberInput{{ExternalModelID: 1, Position: 0}},
	})
	apiErr := wantAPIErr(t, err, resp.CodeBadRequest)
	if apiErr.Message != "成员引用的对外模型不存在" {
		t.Fatalf("unexpected message: %q", apiErr.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// D5：store 返回归属用户外键冲突（ErrOwnerUserMissing）→ 40001，文案「指定的归属用户不存在」。
func TestService_Create_OwnerUserMissing(t *testing.T) {
	s, mock := newServiceMock(t)
	mock.ExpectQuery(regexp.QuoteMeta(getByNameAnySQL)).
		WithArgs("auto1").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO auto_models(id, name, scope, owner_user_id, enabled, description)
		 VALUES($1,$2,$3,$4,$5,$6) RETURNING `+autoCols)).
		WithArgs(sqlmock.AnyArg(), "auto1", ScopePrivate, int64(7), true, "").
		WillReturnError(&pgconn.PgError{Code: "23503", ConstraintName: "auto_models_owner_user_id_fkey"})
	mock.ExpectRollback()

	_, err := s.Create(context.Background(), AutoModelInput{
		Name: "auto1", Scope: ScopePrivate, OwnerUserID: int64Ptr(7),
		Members: []MemberInput{{ExternalModelID: 1, Position: 0}},
	})
	apiErr := wantAPIErr(t, err, resp.CodeBadRequest)
	if apiErr.Message != "指定的归属用户不存在" {
		t.Fatalf("unexpected message: %q", apiErr.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// 重点：扁平命名空间——已有同名 global 时，创建 private 亦被拒（GetByNameAny 命中）→ 40901。
func TestService_Create_FlatNamespace_PrivateRejectedWhenGlobalExists(t *testing.T) {
	s, mock := newServiceMock(t)
	now := time.Now()
	// GetByNameAny 忽略作用域，命中既有 global。
	mock.ExpectQuery(regexp.QuoteMeta(getByNameAnySQL)).
		WithArgs("shared").
		WillReturnRows(autoRow(10, "shared", ScopeGlobal, nil, true, now))

	_, err := s.Create(context.Background(), AutoModelInput{
		Name: "shared", Scope: ScopePrivate, OwnerUserID: int64Ptr(7),
		Members: []MemberInput{{ExternalModelID: 1, Position: 0}},
	})
	wantAPIErr(t, err, resp.CodeConflict)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// --- Update ---

func TestService_Update_NotFound(t *testing.T) {
	s, mock := newServiceMock(t)
	mock.ExpectQuery(regexp.QuoteMeta(getByIDSQL)).
		WithArgs(int64(10)).
		WillReturnError(sql.ErrNoRows)

	_, err := s.Update(context.Background(), 10, AutoModelInput{
		Name: "auto1", Scope: ScopeGlobal,
		Members: []MemberInput{{ExternalModelID: 1, Position: 0}},
	})
	wantAPIErr(t, err, resp.CodeNotFound)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestService_Update_ScopeChangeRejected(t *testing.T) {
	s, mock := newServiceMock(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(getByIDSQL)).
		WithArgs(int64(10)).
		WillReturnRows(autoRow(10, "auto1", ScopeGlobal, nil, true, now))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{10}").
		WillReturnRows(sqlmock.NewRows(memberTestCols))

	_, err := s.Update(context.Background(), 10, AutoModelInput{
		Name: "auto1", Scope: ScopePrivate, OwnerUserID: int64Ptr(7),
		Members: []MemberInput{{ExternalModelID: 1, Position: 0}},
	})
	wantAPIErr(t, err, resp.CodeBadRequest)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// 改名冲突：GetByNameAny 命中另一 ID → 40901；命中自身（同 ID）则不算冲突。
func TestService_Update_RenameConflictExcludesSelf(t *testing.T) {
	s, mock := newServiceMock(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(getByIDSQL)).
		WithArgs(int64(10)).
		WillReturnRows(autoRow(10, "old", ScopeGlobal, nil, true, now))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{10}").
		WillReturnRows(sqlmock.NewRows(memberTestCols))
	mock.ExpectQuery(regexp.QuoteMeta(getByNameAnySQL)).
		WithArgs("new").
		WillReturnRows(autoRow(20, "new", ScopeGlobal, nil, true, now))

	_, err := s.Update(context.Background(), 10, AutoModelInput{
		Name: "new", Scope: ScopeGlobal,
		Members: []MemberInput{{ExternalModelID: 1, Position: 0}},
	})
	wantAPIErr(t, err, resp.CodeConflict)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestService_Update_RenameToSelfAllowed(t *testing.T) {
	s, mock := newServiceMock(t)
	now := time.Now()
	// GetByID（Service 入口）
	mock.ExpectQuery(regexp.QuoteMeta(getByIDSQL)).
		WithArgs(int64(10)).
		WillReturnRows(autoRow(10, "old", ScopeGlobal, nil, true, now))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{10}").
		WillReturnRows(sqlmock.NewRows(memberTestCols))
	// 改名预检查：GetByNameAny 命中自身（同 ID）→ 不冲突。
	mock.ExpectQuery(regexp.QuoteMeta(getByNameAnySQL)).
		WithArgs("new").
		WillReturnRows(autoRow(10, "new", ScopeGlobal, nil, true, now))
	// store.Update 事务
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE auto_models SET name=$1, scope=$2, owner_user_id=$3, enabled=$4, description=$5, updated_at=now()
		 WHERE id=$6 AND deleted_at IS NULL`)).
		WithArgs("new", ScopeGlobal, nil, true, "", int64(10)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE auto_model_members SET deleted_at=now() WHERE auto_model_id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(10)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO auto_model_members(id, auto_model_id, external_model_id, position)
			 VALUES($1,$2,$3,$4)`)).
		WithArgs(sqlmock.AnyArg(), int64(10), int64(1), 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// Update 末尾回读 GetByID
	mock.ExpectQuery(regexp.QuoteMeta(getByIDSQL)).
		WithArgs(int64(10)).
		WillReturnRows(autoRow(10, "new", ScopeGlobal, nil, true, now))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{10}").
		WillReturnRows(sqlmock.NewRows(memberTestCols))

	updated, err := s.Update(context.Background(), 10, AutoModelInput{
		Name: "new", Scope: ScopeGlobal,
		Members: []MemberInput{{ExternalModelID: 1, Position: 0}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "new" {
		t.Fatalf("unexpected updated: %+v", updated)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// --- ResolveMembers ---

func TestService_ResolveMembers_NotFound(t *testing.T) {
	s, mock := newServiceMock(t)
	mock.ExpectQuery(regexp.QuoteMeta(getByNameSQL)).
		WithArgs("nope", int64(7)).
		WillReturnError(sql.ErrNoRows)

	members, isAuto, err := s.ResolveMembers(context.Background(), "nope", int64Ptr(7))
	if err != nil || isAuto || members != nil {
		t.Fatalf("want (nil,false,nil), got (%v,%v,%v)", members, isAuto, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestService_ResolveMembers_DisabledAuto(t *testing.T) {
	s, mock := newServiceMock(t)
	now := time.Now()
	owner := int64(7)
	mock.ExpectQuery(regexp.QuoteMeta(getByNameSQL)).
		WithArgs("auto1", owner).
		WillReturnRows(autoRow(10, "auto1", ScopePrivate, &owner, false, now))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{10}").
		WillReturnRows(sqlmock.NewRows(memberTestCols))

	members, isAuto, err := s.ResolveMembers(context.Background(), "auto1", &owner)
	if err != nil || isAuto || members != nil {
		t.Fatalf("disabled auto want (nil,false,nil), got (%v,%v,%v)", members, isAuto, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestService_ResolveMembers_HitOrdered(t *testing.T) {
	s, mock := newServiceMock(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(getByNameSQL)).
		WithArgs("auto1", int64(7)).
		WillReturnRows(autoRow(10, "auto1", ScopeGlobal, nil, true, now))
	// store 已按 position 升序返回。
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{10}").
		WillReturnRows(sqlmock.NewRows(memberTestCols).
			AddRow(int64(1), int64(10), int64(101), 0, "model-a", true).
			AddRow(int64(2), int64(10), int64(102), 1, "model-b", true))

	members, isAuto, err := s.ResolveMembers(context.Background(), "auto1", int64Ptr(7))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !isAuto || len(members) != 2 {
		t.Fatalf("want 2 members auto, got (%+v,%v)", members, isAuto)
	}
	if members[0].ExternalName != "model-a" || members[1].ExternalName != "model-b" {
		t.Fatalf("members not in position order: %+v", members)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// --- Delete / Get ---

func TestService_Delete_NotFound(t *testing.T) {
	s, mock := newServiceMock(t)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE auto_models SET deleted_at=now() WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(10)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	wantAPIErr(t, s.Delete(context.Background(), 10), resp.CodeNotFound)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestService_Get_NotFound(t *testing.T) {
	s, mock := newServiceMock(t)
	mock.ExpectQuery(regexp.QuoteMeta(getByIDSQL)).
		WithArgs(int64(10)).
		WillReturnError(sql.ErrNoRows)

	_, err := s.Get(context.Background(), 10)
	wantAPIErr(t, err, resp.CodeNotFound)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
