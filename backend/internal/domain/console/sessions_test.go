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
	Code int `json:"Code"`
	Data struct {
		List  []sessionListItem `json:"List"`
		Total int               `json:"Total"`
		Page  int               `json:"Page"`
		Size  int               `json:"Size"`
	} `json:"Data"`
}

// sessionListColsForTest sessions 列表查询列名（与 sessionListCols 顺序一致，供 sqlmock 构造行）。
func sessionListColsForTest() []string {
	return []string{"session_id", "user_id", "token_id", "model", "session_raw", "channel_key_id",
		"internal_model_id", "created_at", "last_active", "expire_at", "name", "closed"}
}

// TestHandleListSessions_FilterAndNames 断言「读 sessions 表」的列表：过滤 + 名称反查 + 历史会话 + 内存 Inflight。
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
		return map[int64]string{100: "密钥A"}, nil
	})

	// 内存注册表只提供 Inflight：alpha-0001 有一个进行中请求；alpha-0002 为历史会话（不在内存）。
	attachTestSession(t, reg, "alpha-0001", 1, 10, 100, true)
	reg.BeginRequest("alpha-0001")

	now := fixedNow()
	rows := sqlmock.NewRows(sessionListColsForTest()).
		AddRow("alpha-0001", int64(1), int64(10), "gpt-4", "raw-alpha-0001", int64(100), "gpt-4-int",
			now.Add(-time.Hour), now, now.Add(2*time.Hour), "会话一", false).
		AddRow("alpha-0002", int64(1), nil, "gpt-4", "raw-alpha-0002", int64(100), "gpt-4-int",
			now.Add(-3*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour), "", false)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM sessions WHERE user_id = $1`)).
		WithArgs(int64(1)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(2)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT `+sessionListCols+` FROM sessions WHERE user_id = $1 ORDER BY last_active DESC, session_id ASC LIMIT $2 OFFSET $3`)).
		WithArgs(int64(1), 20, 0).WillReturnRows(rows)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, username, nickname FROM users WHERE id = ANY('{1}'::bigint[])`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username", "nickname"}).AddRow(int64(1), "u1", "昵称一"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, display_name FROM tokens WHERE id = ANY('{10}'::bigint[])`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "display_name"}).AddRow(int64(10), "tok-a"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/sessions?UserID=1&Expired=all", nil)
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
	if a1.SessionID != "alpha-0001" || a1.SessionName != "会话一" || a1.UserID != 1 || a1.TokenID != 10 {
		t.Fatalf("item1=%+v", a1)
	}
	if a1.UserName != "u1" || a1.TokenName != "tok-a" || a1.ChannelKeyName != "密钥A" {
		t.Fatalf("item1 names=%+v", a1)
	}
	if a1.Inflight != 1 {
		t.Fatalf("alpha-0001 进行中请求数应为 1，got %d", a1.Inflight)
	}
	if a1.Expired || a1.ChannelKeyID != 100 || a1.Model != "gpt-4" || a1.SessionRaw != "raw-alpha-0001" {
		t.Fatalf("item1 fields: %+v", a1)
	}
	// 历史会话（不在内存）：Expired=true、TokenID=0、Inflight=0。
	if a2.SessionID != "alpha-0002" || !a2.Expired || a2.TokenID != 0 || a2.Inflight != 0 {
		t.Fatalf("item2 fields: %+v", a2)
	}
	if len(resolvedKeys) != 1 || resolvedKeys[0] != 100 {
		t.Fatalf("keyNameByIDs 应仅查询展示项的 keyID=%d, got %v", 100, resolvedKeys)
	}
}

// TestSessionListWhere 断言会话列表过滤 WHERE 子句与参数绑定（编号连续、无条件为空串）。
func TestSessionListWhere(t *testing.T) {
	now := fixedNow()
	cases := []struct {
		name                string
		user, tok, k        int64
		prefix, exp, closed string
		wantWhere           string
		wantArgs            int
	}{
		{name: "no-filter", wantWhere: "", wantArgs: 0},
		{name: "by-user", user: 1, wantWhere: " WHERE user_id = $1", wantArgs: 1},
		{name: "by-token", tok: 10, wantWhere: " WHERE token_id = $1", wantArgs: 1},
		{name: "by-key", k: 100, wantWhere: " WHERE channel_key_id = $1", wantArgs: 1},
		{name: "by-prefix", prefix: "abc", wantWhere: " WHERE starts_with(session_id, $1)", wantArgs: 1},
		{name: "active", exp: "active", wantWhere: " WHERE expire_at > $1", wantArgs: 1},
		{name: "expired", exp: "expired", wantWhere: " WHERE expire_at <= $1", wantArgs: 1},
		{name: "closed-only", closed: "1", wantWhere: " WHERE closed = true", wantArgs: 0},
		{name: "open-only", closed: "0", wantWhere: " WHERE closed = false", wantArgs: 0},
		{name: "combined", user: 1, tok: 10, exp: "active", closed: "0",
			wantWhere: " WHERE user_id = $1 AND token_id = $2 AND expire_at > $3 AND closed = false", wantArgs: 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			where, args := sessionListWhere(c.user, c.tok, c.k, c.prefix, c.exp, c.closed, now)
			if where != c.wantWhere {
				t.Fatalf("where=%q, want %q", where, c.wantWhere)
			}
			if len(args) != c.wantArgs {
				t.Fatalf("args=%d, want %d", len(args), c.wantArgs)
			}
		})
	}
}

// TestHandleListSessions_Pagination 断言分页参数（无过滤时 LIMIT/OFFSET 绑定 $1/$2）。
func TestHandleListSessions_Pagination(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	admin := NewAdmin(db, nil, nil, nil)
	admin.now = fixedNow
	now := fixedNow()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM sessions`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(3)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT `+sessionListCols+` FROM sessions ORDER BY last_active DESC, session_id ASC LIMIT $1 OFFSET $2`)).
		WithArgs(1, 1).
		WillReturnRows(sqlmock.NewRows(sessionListColsForTest()).
			AddRow("a2", int64(1), nil, "gpt-4", "raw-a2", int64(100), "gpt-4-int",
				now.Add(-time.Hour), now, now.Add(time.Hour), "", false))

	rec := httptest.NewRecorder()
	admin.HandleListSessions(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/sessions?Page=2&Size=1", nil))
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
		t.Fatalf("page2 首项应为 a2，got %s", body.Data.List[0].SessionID)
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
				Affected int `json:"Affected"`
			} `json:"Data"`
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
