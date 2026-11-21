// token_keyname_view_test.go 定案①（2026-11-20）：管理端令牌列表展示 callerKey 名。
//
// 覆盖三条契约：
//  1. 注入钩子后 admin 列表每行带 MagpieKeyName（批量一次解析）；
//  2. dev 列表（HandleDevList）不带该字段（MagpieKeyName 不出现在开发者侧响应）；
//  3. 钩子为 nil 时 admin 列表照常返回（空串 omitempty 不输出，列表不 500）。
package identity

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// keynameTokenRow 构造一行 tokenInfoCols 形状的列表行。
func keynameTokenRow(id int64) []driver.Value {
	return []driver.Value{id, "hash", "disp", int64(7), "tok", nil, nil, "ACTIVE", time.Now(), "user", ""}
}

func tokenInfoRows() *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"t.id", "t.token_hash", "t.token_display", "t.user_id", "t.display_name",
		"t.expires_at", "t.last_used_at", "t.status", "t.created_at", "u.username", "u.nickname"})
	rows.AddRow(keynameTokenRow(1)...)
	rows.AddRow(keynameTokenRow(2)...)
	return rows
}

func expectTokenListQueries(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(`SELECT count\(\*\) FROM tokens t JOIN users u`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(2)))
	mock.ExpectQuery(regexp.QuoteMeta(listTokensPageSQL)).WithArgs(nil, "", 20, 0).WillReturnRows(rows)
}

// decodeTokenList 解析 tokenResponse 列表。
func decodeTokenList(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var body struct {
		Data struct {
			List []map[string]any `json:"List"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, rec.Body.String())
	}
	return body.Data.List
}

// TestAdminTokenList_ShowsMagpieKeyName 管理端列表每行带 callerKey 名（一次批量解析）。
func TestAdminTokenList_ShowsMagpieKeyName(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	expectTokenListQueries(mock, tokenInfoRows())

	h := NewTokenHandler(NewTokenService(NewTokenStore(db)), func(context.Context) (int64, bool) { return 1, true })

	var batched [][]int64
	h.SetMagpieKeyNameOf(func(_ context.Context, ids []int64) (map[int64]string, error) {
		batched = append(batched, ids)
		return map[int64]string{1: "ls-u7-t1-alpha", 2: "ls-u7-t2-beta"}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/tokens", nil)
	rec := httptest.NewRecorder()
	h.HandleAdminList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin list status = %d: %s", rec.Code, rec.Body.String())
	}
	list := decodeTokenList(t, rec)
	if len(list) != 2 {
		t.Fatalf("list len = %d", len(list))
	}
	if len(batched) != 1 || len(batched[0]) != 2 {
		t.Fatalf("批量解析应一次两 id：got %v", batched)
	}
	if list[0]["MagpieKeyName"] != "ls-u7-t1-alpha" || list[1]["MagpieKeyName"] != "ls-u7-t2-beta" {
		t.Fatalf("MagpieKeyName 未注入：%v %v", list[0]["MagpieKeyName"], list[1]["MagpieKeyName"])
	}
}

// TestAdminTokenList_NilResolverStillOK 未注入钩子（旧装配）：列表照常，不带该字段。
func TestAdminTokenList_NilResolverStillOK(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	expectTokenListQueries(mock, tokenInfoRows())

	h := NewTokenHandler(NewTokenService(NewTokenStore(db)), func(context.Context) (int64, bool) { return 1, true })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/tokens", nil)
	rec := httptest.NewRecorder()
	h.HandleAdminList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin list status = %d: %s", rec.Code, rec.Body.String())
	}
	list := decodeTokenList(t, rec)
	if len(list) != 2 {
		t.Fatalf("list len = %d", len(list))
	}
	if _, present := list[0]["MagpieKeyName"]; present {
		t.Fatalf("未注入钩子不得输出 MagpieKeyName：%s", rec.Body.String())
	}
}

// TestDevTokenList_NoMagpieKeyName dev 列表（HandleDevList）不带 MagpieKeyName。
func TestDevTokenList_NoMagpieKeyName(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	// dev 列表按本人过滤：WithArgs(7,...)
	mock.ExpectQuery(`SELECT count\(\*\) FROM tokens t JOIN users u`).
		WithArgs(int64(7), "").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(2)))
	mock.ExpectQuery(regexp.QuoteMeta(listTokensPageSQL)).WithArgs(int64(7), "", 20, 0).WillReturnRows(tokenInfoRows())

	h := NewTokenHandler(NewTokenService(NewTokenStore(db)), func(context.Context) (int64, bool) { return 7, true })
	// 即使误注入（装配层不会），dev 路径也绝不输出该字段。
	h.SetMagpieKeyNameOf(func(_ context.Context, _ []int64) (map[int64]string, error) {
		return map[int64]string{1: "ls-u7-t1-alpha", 2: "ls-u7-t2-beta"}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dev/tokens", nil)
	rec := httptest.NewRecorder()
	h.HandleDevList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dev list status = %d: %s", rec.Code, rec.Body.String())
	}
	list := decodeTokenList(t, rec)
	if len(list) != 2 {
		t.Fatalf("list len = %d", len(list))
	}
	for i := range list {
		if _, present := list[i]["MagpieKeyName"]; present {
			t.Fatalf("dev 列表第 %d 行不得带 MagpieKeyName", i)
		}
	}
}
