package automodel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/pkg/gencache"
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

// TestService_ResolveMembers_CacheZeroDBAndInvalidate 验证 Auto 成员解析读缓存（STEP 24）：
// 命中后零 DB（连续多次解析仅回源一次）；删除写路径后缓存立即失效（再解析重新查库并读到新成员）。
func TestService_ResolveMembers_CacheZeroDBAndInvalidate(t *testing.T) {
	s, mock := newServiceMock(t)
	now := time.Now()
	// 首次解析：主表 1 次 + 成员 1 次。
	mock.ExpectQuery(regexp.QuoteMeta(getByNameSQL)).
		WithArgs("auto1", int64(7)).
		WillReturnRows(autoRow(10, "auto1", ScopeGlobal, nil, true, now))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{10}").
		WillReturnRows(sqlmock.NewRows(memberTestCols).
			AddRow(int64(1), int64(10), int64(101), 0, "model-a", true))

	for i := 0; i < 3; i++ {
		members, isAuto, err := s.ResolveMembers(context.Background(), "auto1", int64Ptr(7))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if !isAuto || len(members) != 1 {
			t.Fatalf("unexpected: (%+v,%v)", members, isAuto)
		}
	}
	// 仅一次回源：命中缓存不应再读库。
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("命中缓存不应再读库: %v", err)
	}

	// 写路径（Delete）后缓存立即失效：再解析重新查库并读到新成员列表。
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE auto_models SET deleted_at=now() WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(10)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE auto_model_members SET deleted_at=now() WHERE auto_model_id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(10)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := s.Delete(context.Background(), 10); err != nil {
		t.Fatalf("delete: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(getByNameSQL)).
		WithArgs("auto1", int64(7)).
		WillReturnRows(autoRow(10, "auto1", ScopeGlobal, nil, true, now))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{10}").
		WillReturnRows(sqlmock.NewRows(memberTestCols).
			AddRow(int64(1), int64(10), int64(101), 0, "model-a", true).
			AddRow(int64(2), int64(10), int64(102), 1, "model-b", true))
	members, isAuto, err := s.ResolveMembers(context.Background(), "auto1", int64Ptr(7))
	if err != nil {
		t.Fatalf("resolve after invalidate: %v", err)
	}
	if !isAuto || len(members) != 2 {
		t.Fatalf("失效后应重新查库并读到 2 个成员，got (%+v,%v)", members, isAuto)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestService_InvalidateReadCacheForcesReread 锁定 P1-4：导出的 InvalidateReadCache
// （供 model.Service 写路径跨包调用）后，ResolveMembers 必须重新查库而非命中旧缓存。
func TestService_InvalidateReadCacheForcesReread(t *testing.T) {
	s, mock := newServiceMock(t)
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta(getByNameSQL)).
		WithArgs("auto1", int64(7)).
		WillReturnRows(autoRow(10, "auto1", ScopeGlobal, nil, true, now))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{10}").
		WillReturnRows(sqlmock.NewRows(memberTestCols).
			AddRow(int64(1), int64(10), int64(101), 0, "model-a", true))

	// 首次解析回源，第二次命中缓存（不应再有 DB 调用）。
	if _, _, err := s.ResolveMembers(context.Background(), "auto1", int64Ptr(7)); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, _, err := s.ResolveMembers(context.Background(), "auto1", int64Ptr(7)); err != nil {
		t.Fatalf("resolve cached: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("命中缓存不应再读库: %v", err)
	}

	// 模拟外部模型写路径 → 跨包失效；再解析必须重新查库并读到新成员。
	s.InvalidateReadCache()
	mock.ExpectQuery(regexp.QuoteMeta(getByNameSQL)).
		WithArgs("auto1", int64(7)).
		WillReturnRows(autoRow(10, "auto1", ScopeGlobal, nil, true, now))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{10}").
		WillReturnRows(sqlmock.NewRows(memberTestCols).
			AddRow(int64(1), int64(10), int64(101), 0, "model-a", true).
			AddRow(int64(2), int64(10), int64(102), 1, "model-b", true))
	members, isAuto, err := s.ResolveMembers(context.Background(), "auto1", int64Ptr(7))
	if err != nil {
		t.Fatalf("resolve after invalidate: %v", err)
	}
	if !isAuto || len(members) != 2 {
		t.Fatalf("跨包失效后应重新查库并读到 2 个成员，got (%+v,%v)", members, isAuto)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestService_MembersCacheBounded 锁定 P1-5：ResolveMembers 缓存（键首段来自请求体、用户可控）
// 必须有界（FIFO 淘汰），防止认证用户以海量随机名打满负缓存造成内存耗尽 DoS。
func TestService_MembersCacheBounded(t *testing.T) {
	s := NewService(nil)
	// 直写底层原语上限（原语在构造时确定 max），验证有界 FIFO 淘汰。
	s.membersCache = gencache.New[resolveCacheEntry](8)
	for i := 0; i < 100; i++ {
		s.cacheResolvedMembers(fmt.Sprintf("n-%d\x00", i), nil, false, s.membersCache.Generation())
	}
	if got := s.membersCache.Count(); got > 8 {
		t.Fatalf("membersCache 应有界（<=8），got %d", got)
	}
}

// TestService_StaleMembersFillAfterInvalidateRejected 与 identity token_cache 的同名用例同构：
// 读路径在回源 DB 前捕获代际；若回源期间发生失效（Auto 写路径 / 跨包对外模型写路径 bump 代际），
// 携带陈旧代际的成员缓存回填必须被拒绝——否则失效前读到的旧成员列表会滞留（本缓存无 TTL）。
func TestService_StaleMembersFillAfterInvalidateRejected(t *testing.T) {
	s := NewService(nil)
	fresh := []Member{{ExternalModelID: 10, ExternalName: "a"}}
	key := membersCacheKey("auto1", nil)

	// 读路径：回源前捕获代际。
	gen := s.membersCache.Generation()
	// 期间失效（Auto 写路径或跨包 InvalidateReadCache）。
	s.invalidateMembersCache()
	// 读路径回来回填（携带陈旧代际）。
	s.cacheResolvedMembers(key, fresh, true, gen)

	if _, ok := s.membersCache.Get(key); ok {
		t.Fatal("失效后携带陈旧代际的回填必须被拒绝（否则旧成员列表无限期滞留）")
	}

	// 对照组：期间无失效时，携带当前代际的回填应正常命中。
	s.cacheResolvedMembers(key, fresh, true, s.membersCache.Generation())
	if _, ok := s.membersCache.Get(key); !ok {
		t.Fatal("无失效发生时回填应正常命中")
	}
}

// TestService_StaleNilMembersFillAfterInvalidateRejected 是上例的负结果(nil)变体：
// 失效后携带陈旧代际的 nil（「查无此名 / 已停用」负结果）回填同样必须被原语拒绝，
// 否则失效前的「非 Auto」结论会滞留，后续请求持续误判。
func TestService_StaleNilMembersFillAfterInvalidateRejected(t *testing.T) {
	s := NewService(nil)
	key := membersCacheKey("auto1", nil)

	gen := s.membersCache.Generation()
	s.invalidateMembersCache()
	s.cacheResolvedMembers(key, nil, false, gen)

	if _, ok := s.membersCache.Get(key); ok {
		t.Fatal("失效后携带陈旧代际的 nil 负结果回填必须被拒绝")
	}
}

// TestService_ResolveMembers_DisabledAlwaysQueriesDB 锁定「关闭读缓存 ⇒ 每次直查 DB」：
// SetReadCacheEnabled(false) 后连续两次 ResolveMembers 都必须回源（主表 + 成员各两次），
// 且不写入缓存；若误读/误写缓存，第二次的 sqlmock 期望将不被消费而报错。
func TestService_ResolveMembers_DisabledAlwaysQueriesDB(t *testing.T) {
	s, mock := newServiceMock(t)
	s.SetReadCacheEnabled(false)
	now := time.Now()
	for i := 0; i < 2; i++ {
		mock.ExpectQuery(regexp.QuoteMeta(getByNameSQL)).
			WithArgs("auto1", int64(7)).
			WillReturnRows(autoRow(10, "auto1", ScopeGlobal, nil, true, now))
		mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
			WithArgs("{10}").
			WillReturnRows(sqlmock.NewRows(memberTestCols).
				AddRow(int64(1), int64(10), int64(101), 0, "model-a", true))
	}
	for i := 0; i < 2; i++ {
		members, isAuto, err := s.ResolveMembers(context.Background(), "auto1", int64Ptr(7))
		if err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		if !isAuto || len(members) != 1 {
			t.Fatalf("resolve %d unexpected: (%+v,%v)", i, members, isAuto)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("关闭缓存时每次都应回源（主表+成员各两次）: %v", err)
	}
	if got := s.membersCache.Count(); got != 0 {
		t.Fatalf("关闭缓存时不得写入缓存，got %d 条", got)
	}
}

// TestService_ResolveMembers_DefensiveCopy 锁定 P2-3：缓存命中返回的成员切片为副本，
// 调用方就地修改不得污染缓存（再次解析应仍返回原始成员）。
func TestService_ResolveMembers_DefensiveCopy(t *testing.T) {
	s := NewService(nil)
	s.cacheResolvedMembers("auto1\x00", []Member{{ExternalModelID: 10, ExternalName: "a"}}, true, s.membersCache.Generation())

	first, isAuto, err := s.ResolveMembers(context.Background(), "auto1", nil)
	if err != nil || !isAuto {
		t.Fatalf("resolve: err=%v isAuto=%v", err, isAuto)
	}
	// 就地篡改返回值。
	first[0].ExternalModelID = 999
	first[0].ExternalName = "mutated"

	second, isAuto, err := s.ResolveMembers(context.Background(), "auto1", nil)
	if err != nil || !isAuto {
		t.Fatalf("resolve2: err=%v isAuto=%v", err, isAuto)
	}
	if second[0].ExternalModelID != 10 || second[0].ExternalName != "a" {
		t.Fatalf("缓存被返回值修改污染：%+v", second[0])
	}
}

// extModelSelectSQL 与 model 包 Store.GetByID 一致（跨包测试硬编码；列序与 external_models 表一致）。
const extModelSelectSQL = `SELECT id, external_name, description, enabled, sale_rates, time_config, context_tiers, created_at, updated_at FROM external_models WHERE id = $1 AND deleted_at IS NULL`

// extModelRow 构造 external_models 全列查询的一行（列序与 extModelSelectSQL 一致）。
func extModelRow(id int64, name string, enabled bool, now time.Time) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "external_name", "description", "enabled", "sale_rates", "time_config", "context_tiers", "created_at", "updated_at"}).
		AddRow(id, name, "", enabled, `{"input":1,"output":2,"cache_read":0.1}`, nil, nil, now, now)
}

// TestCrossPackage_ModelWriteInvalidatesResolveCache 跨包失效链路集成（P1-4）：
// 按装配层同款接线把 model.Service.SetNameWriteHook 接到 automodel.Service.InvalidateReadCache，
// 验证真实链路：ResolveMembers 命中缓存 → 零 DB；对外模型写路径（参数化覆盖多条）后
// → automodel 解析缓存被清空 → 下一次解析重新查库并读到新快照。
func TestCrossPackage_ModelWriteInvalidatesResolveCache(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	cases := []struct {
		name  string
		write func(t *testing.T, svc *model.Service, mock sqlmock.Sqlmock)
	}{
		{
			name: "BatchDeleteModels",
			write: func(t *testing.T, svc *model.Service, mock sqlmock.Sqlmock) {
				mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET deleted_at = now() WHERE id = ANY($1::bigint[])`)).
					WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
				if _, err := svc.BatchDeleteModels(ctx, []int64{101}); err != nil {
					t.Fatalf("批量删除对外模型失败: %v", err)
				}
			},
		},
		{
			name: "UpdateModel",
			write: func(t *testing.T, svc *model.Service, mock sqlmock.Sqlmock) {
				// UpdateModel：GetByID（既有）→ Update → GetByID（回读），名称不变故跳过 GetByName。
				mock.ExpectQuery(regexp.QuoteMeta(extModelSelectSQL)).WithArgs(int64(101)).
					WillReturnRows(extModelRow(101, "model-a", true, now))
				mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET external_name=$1, description=$2, enabled=$3`)).
					WithArgs("model-a", "", false, sqlmock.AnyArg(), nil, nil, int64(101)).
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(regexp.QuoteMeta(extModelSelectSQL)).WithArgs(int64(101)).
					WillReturnRows(extModelRow(101, "model-a", false, now))
				disabled := false
				if _, err := svc.UpdateModel(ctx, 101, model.ExternalModelInput{
					ExternalName: "model-a",
					Enabled:      &disabled,
					SaleRates:    billing.Rates{"input": 1, "output": 2, "cache_read": 0.1},
				}); err != nil {
					t.Fatalf("更新对外模型失败: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Auto 侧：真实 automodel.Service + sqlmock。
			autoSvc, autoMock := newServiceMock(t)

			// 对外模型侧：真实 model.Service + sqlmock，并按 main.go 同款接线注入跨包失效钩子。
			mdb, modelMock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			t.Cleanup(func() { _ = mdb.Close() })
			modelSvc := model.NewService(model.NewStore(mdb))
			modelSvc.SetNameWriteHook(autoSvc.InvalidateReadCache)

			// 首次解析回源（主表 + 成员各一次）。
			autoMock.ExpectQuery(regexp.QuoteMeta(getByNameSQL)).
				WithArgs("auto1", int64(7)).
				WillReturnRows(autoRow(10, "auto1", ScopeGlobal, nil, true, now))
			autoMock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
				WithArgs("{10}").
				WillReturnRows(sqlmock.NewRows(memberTestCols).
					AddRow(int64(1), int64(10), int64(101), 0, "model-a", true))
			if _, isAuto, rerr := autoSvc.ResolveMembers(ctx, "auto1", int64Ptr(7)); rerr != nil || !isAuto {
				t.Fatalf("首次解析失败: err=%v isAuto=%v", rerr, isAuto)
			}
			// 第二次命中缓存：不应再查库。
			if _, _, rerr := autoSvc.ResolveMembers(ctx, "auto1", int64Ptr(7)); rerr != nil {
				t.Fatalf("缓存命中解析失败: %v", rerr)
			}
			if merr := autoMock.ExpectationsWereMet(); merr != nil {
				t.Fatalf("命中缓存不应再读库: %v", merr)
			}

			// 触发跨包链路：对外模型写路径 → nameWriteHook → automodel 缓存清空。
			tc.write(t, modelSvc, modelMock)
			if merr := modelMock.ExpectationsWereMet(); merr != nil {
				t.Fatalf("对外模型写路径 mock 未满足: %v", merr)
			}

			// 跨包失效后：下一次解析必须重新查库，并读到「成员已停用」的新快照（证明未命中旧缓存）。
			autoMock.ExpectQuery(regexp.QuoteMeta(getByNameSQL)).
				WithArgs("auto1", int64(7)).
				WillReturnRows(autoRow(10, "auto1", ScopeGlobal, nil, true, now))
			autoMock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
				WithArgs("{10}").
				WillReturnRows(sqlmock.NewRows(memberTestCols).
					AddRow(int64(1), int64(10), int64(101), 0, "model-a", false))
			members, isAuto, rerr := autoSvc.ResolveMembers(ctx, "auto1", int64Ptr(7))
			if rerr != nil || !isAuto {
				t.Fatalf("跨包失效后解析失败: err=%v isAuto=%v", rerr, isAuto)
			}
			if len(members) != 1 || members[0].Enabled {
				t.Fatalf("跨包失效后应读到新快照（成员 disabled），got %+v", members)
			}
			if merr := autoMock.ExpectationsWereMet(); merr != nil {
				t.Fatalf("跨包失效后应重新查库: %v", merr)
			}
		})
	}
}
