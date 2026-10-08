package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/router"
)

// fixedNow 会话管理测试统一时钟。
func fixedNow() time.Time { return time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC) }

// attachTestSession 构造并挂载一条会话到注册表（active=false 表示已过期）。
func attachTestSession(t *testing.T, reg *router.SessionRegistry, id string, userID, tokenID, keyID int64, active bool) {
	t.Helper()
	exp := fixedNow().Add(2 * time.Hour)
	if !active {
		exp = fixedNow().Add(-2 * time.Hour)
	}
	ok, _ := reg.Attach(&router.Session{
		SessionID:       id,
		UserID:          userID,
		TokenID:         tokenID,
		Model:           "gpt-4",
		SessionRaw:      "raw-" + id,
		ChannelKeyID:    keyID,
		InternalModelID: "gpt-4-int",
		ExpireAt:        exp,
	}, 100)
	if !ok {
		t.Fatalf("attach session %s failed", id)
	}
}

// sessionListBody 会话列表响应结构。
type sessionListBody struct {
	Code int `json:"code"`
	Data struct {
		List  []sessionListItem `json:"list"`
		Total int               `json:"total"`
		Page  int               `json:"page"`
		Size  int               `json:"size"`
	} `json:"data"`
}

// TestHandleListSessions_FilterAndNames 断言用户/令牌过滤 + user/token/key 名称批量反查 + expired 标记。
func TestHandleListSessions_FilterAndNames(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	admin := NewAdmin(db, nil, nil, nil)
	admin.now = fixedNow
	reg := router.NewSessionRegistry(fixedNow)
	admin.SetSessions(reg)

	var resolvedKeys []int64
	admin.SetKeyNameResolver(func(_ context.Context, ids []int64) (map[int64]string, error) {
		resolvedKeys = ids
		return map[int64]string{100: "密钥A", 101: "密钥B"}, nil
	})

	attachTestSession(t, reg, "alpha-0001", 1, 10, 100, true)
	attachTestSession(t, reg, "alpha-0002", 1, 20, 100, false)
	attachTestSession(t, reg, "beta-0001", 2, 30, 101, true)
	attachTestSession(t, reg, "beta-0002", 2, 0, 101, true)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, username, nickname FROM users WHERE id = ANY('{1}'::bigint[])`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username", "nickname"}).AddRow(int64(1), "u1", "昵称一"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, display_name FROM tokens WHERE id = ANY('{10,20}'::bigint[])`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "display_name"}).
			AddRow(int64(10), "tok-a").AddRow(int64(20), "tok-b"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/sessions?user_id=1&expired=all", nil)
	rec := httptest.NewRecorder()
	admin.HandleListSessions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body sessionListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Data.Total != 2 || len(body.Data.List) != 2 {
		t.Fatalf("total=%d len=%d, want 2/2", body.Data.Total, len(body.Data.List))
	}
	a1, a2 := body.Data.List[0], body.Data.List[1]
	if a1.SessionID != "alpha-0001" || a1.UserID != 1 || a1.TokenID != 10 {
		t.Fatalf("item1=%+v", a1)
	}
	if a1.UserName != "u1" || a1.TokenName != "tok-a" || a1.ChannelKeyName != "密钥A" {
		t.Fatalf("item1 names=%+v", a1)
	}
	if a1.Expired || a1.ChannelKeyID != 100 || a1.Model != "gpt-4" || a1.SessionRaw != "raw-alpha-0001" {
		t.Fatalf("item1 fields not expired: %+v", a1)
	}
	if a2.SessionID != "alpha-0002" || a2.TokenName != "tok-b" || !a2.Expired {
		t.Fatalf("item2 fields: %+v", a2)
	}
	if len(resolvedKeys) != 1 || resolvedKeys[0] != 100 {
		t.Fatalf("keyNameByIDs 应仅查询展示项的 keyID=%d, got %v", 100, resolvedKeys)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestHandleListSessions_ExpiredFilter 断言 expired=active|expired|all 与 q/渠道密钥/用户+令牌组合过滤（db=nil 跳过名称反查）。
func TestHandleListSessions_ExpiredFilter(t *testing.T) {
	admin := NewAdmin(nil, nil, nil, nil)
	admin.now = fixedNow
	reg := router.NewSessionRegistry(fixedNow)
	admin.SetSessions(reg)

	attachTestSession(t, reg, "alpha-0001", 1, 10, 100, true)
	attachTestSession(t, reg, "alpha-0002", 1, 20, 100, false)
	attachTestSession(t, reg, "beta-0001", 2, 30, 101, true)
	attachTestSession(t, reg, "beta-0002", 2, 40, 101, false)

	cases := []struct {
		query string
		total int
	}{
		{"/api/v1/admin/sessions?expired=active", 2},
		{"/api/v1/admin/sessions?expired=expired", 2},
		{"/api/v1/admin/sessions?expired=all", 4},
		{"/api/v1/admin/sessions", 4},
		{"/api/v1/admin/sessions?channel_key_id=100", 2},
		{"/api/v1/admin/sessions?q=alpha", 2},
		{"/api/v1/admin/sessions?user_id=2&token_id=40", 1},
		{"/api/v1/admin/sessions?channel_key_id=101&expired=expired", 1},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		admin.HandleListSessions(rec, httptest.NewRequest(http.MethodGet, c.query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d; body=%s", c.query, rec.Code, rec.Body.String())
		}
		var body sessionListBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s unmarshal: %v", c.query, err)
		}
		if body.Data.Total != c.total {
			t.Fatalf("%s total=%d, want %d", c.query, body.Data.Total, c.total)
		}
	}
}

// TestHandleListSessions_Pagination 断言 page/size 切片（排序：last_active 倒序，同值时 session_id 升序）。
func TestHandleListSessions_Pagination(t *testing.T) {
	admin := NewAdmin(nil, nil, nil, nil)
	admin.now = fixedNow
	reg := router.NewSessionRegistry(fixedNow)
	admin.SetSessions(reg)

	for _, id := range []string{"a1", "a2", "a3"} {
		attachTestSession(t, reg, id, 1, 10, 100, true)
	}

	rec := httptest.NewRecorder()
	admin.HandleListSessions(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/sessions?page=2&size=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d; body=%s", rec.Code, rec.Body.String())
	}
	var body sessionListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Data.Total != 3 || body.Data.Page != 2 || body.Data.Size != 1 || len(body.Data.List) != 1 {
		t.Fatalf("pagination meta=%+v", body.Data)
	}
	if body.Data.List[0].SessionID != "a2" {
		t.Fatalf("page2 首项应为 a2（升序第 2 位），got %s", body.Data.List[0].SessionID)
	}
}

// TestHandleKickSessions 断言多维度踢下线：session_ids 先杀 + 其它维度 KillByFilter 不重复计数 + AND 语义 + 空体 400。
func TestHandleKickSessions(t *testing.T) {
	admin := NewAdmin(nil, nil, nil, nil)
	reg := router.NewSessionRegistry(fixedNow)
	admin.SetSessions(reg)

	attachTestSession(t, reg, "k1", 1, 10, 100, true)
	attachTestSession(t, reg, "k2", 2, 20, 100, true)
	attachTestSession(t, reg, "k3", 1, 10, 200, true)
	attachTestSession(t, reg, "k4", 3, 30, 300, true)

	post := func(t *testing.T, body string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		admin.HandleKickSessions(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/sessions/kick", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("kick %s status=%d; body=%s", body, rec.Code, rec.Body.String())
		}
		var res struct {
			Data struct {
				Affected int `json:"affected"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return res.Data.Affected
	}

	// 组合：session_ids（k1 + 不存在）先杀，随后 user_id=1 过滤再杀 k3；k1 不重复计数。
	if got := post(t, `{"SessionIDs":["k1","not-exist"],"UserID":"1"}`); got != 2 {
		t.Fatalf("affected want 2, got %d", got)
	}
	// AND：user 1 + token 20 无人同时满足。
	if got := post(t, `{"UserID":"1","TokenID":"20"}`); got != 0 {
		t.Fatalf("AND affected want 0, got %d", got)
	}
	// 单维度：渠道密钥 100 + 用户 2。
	if got := post(t, `{"UserID":"2","ChannelKeyID":"100"}`); got != 1 {
		t.Fatalf("affected want 1, got %d", got)
	}
	// session_ids 单维度。
	if got := post(t, `{"SessionIDs":["k4"]}`); got != 1 {
		t.Fatalf("affected want 1, got %d", got)
	}

	// 关闭语义：记录保留在列表中（Closed=true），仅停止路由命中与释放并发槽。
	left := reg.ListAll()
	if len(left) != 4 {
		t.Fatalf("关闭后会话应全部保留在列表，got %d: %+v", len(left), left)
	}
	for _, s := range left {
		if !s.Closed {
			t.Fatalf("会话 %s 未标记为已关闭: %+v", s.SessionID, s)
		}
	}
	if n := reg.CountActive(100); n != 0 {
		t.Fatalf("关闭后并发计数应清零，got %d", n)
	}

	// 空体 → 400。
	rec := httptest.NewRecorder()
	admin.HandleKickSessions(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/sessions/kick", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空体 status=%d, want 400", rec.Code)
	}
}
