package automodel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/pkg/resp"
)

// newHandlerUser 构造带当前用户注入的 Handler（模拟开发端已登录）。
func newHandlerUser(t *testing.T, userID int64) (*Handler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := NewHandler(NewService(NewStore(db)))
	h.SetUserIDFrom(func(_ context.Context) (int64, bool) { return userID, true })
	return h, mock
}

// mockGetByID 期望一次 GetByID（主表 + 成员）查询。
func mockGetByID(mock sqlmock.Sqlmock, m AutoModel) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + autoCols + ` FROM auto_models WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(m.ID).
		WillReturnRows(autoRow(m.ID, m.Name, m.Scope, m.OwnerUserID, m.Enabled, time.Now()))
	mock.ExpectQuery(regexp.QuoteMeta(membersSQL)).
		WithArgs("{" + strconv.FormatInt(m.ID, 10) + "}").
		WillReturnRows(sqlmock.NewRows(memberTestCols))
}

// dev GET：本人 private → 200。
func TestHandleDevGet_OwnPrivate(t *testing.T) {
	h, mock := newHandlerUser(t, 7)
	owner := int64(7)
	mockGetByID(mock, AutoModel{ID: 100, Name: "mine", Scope: ScopePrivate, OwnerUserID: &owner, Enabled: true})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dev/auto-models/100", nil)
	req.SetPathValue("id", "100")
	rec := httptest.NewRecorder()
	h.HandleDevGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("own private want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// dev GET：global（无 owner）→ 200。
func TestHandleDevGet_Global(t *testing.T) {
	h, mock := newHandlerUser(t, 7)
	mockGetByID(mock, AutoModel{ID: 100, Name: "shared", Scope: ScopeGlobal, Enabled: true})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dev/auto-models/100", nil)
	req.SetPathValue("id", "100")
	rec := httptest.NewRecorder()
	h.HandleDevGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("global want 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// dev GET：他人 private → 403（40301）。
func TestHandleDevGet_OtherPrivateForbidden(t *testing.T) {
	h, mock := newHandlerUser(t, 7)
	owner := int64(99)
	mockGetByID(mock, AutoModel{ID: 100, Name: "others", Scope: ScopePrivate, OwnerUserID: &owner, Enabled: true})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dev/auto-models/100", nil)
	req.SetPathValue("id", "100")
	rec := httptest.NewRecorder()
	h.HandleDevGet(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("other private want 403, got %d: %s", rec.Code, rec.Body.String())
	}
	var body resp.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != resp.CodeForbidden {
		t.Fatalf("want code %d, got %d", resp.CodeForbidden, body.Code)
	}
}

// dev GET：未登录 → 401（fail-closed）。
func TestHandleDevGet_Unauthenticated(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := NewHandler(NewService(NewStore(db))) // 未注入 userIDFrom

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dev/auto-models/100", nil)
	req.SetPathValue("id", "100")
	rec := httptest.NewRecorder()
	h.HandleDevGet(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated want 401, got %d", rec.Code)
	}
}
