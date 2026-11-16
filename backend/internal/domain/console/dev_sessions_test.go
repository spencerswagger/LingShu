package console

// dev_sessions_test.go 回归 Owl 复审 #85 P1：`GET /api/v1/dev/sessions` 恒 500。
//
// 根因：`sessionListCols` 摘掉 sessions.channel_key_id 后只剩 11 列，
// 而 dev.go 的 rows.Scan 还留着 `&s.ChannelKeyID` 第 12 个目标 →
// 「库里只要有一行会话」就 `expected 11 destination arguments in Scan, not 12` → 500。
// 该路径此前零用例（management 侧有、dev 侧没有），所以静态摘除时漏了。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// devSessionListBody 是 dev 会话列表响应结构（字段名与 admin 侧同源）。
type devSessionListBody struct {
	Code int `json:"Code"`
	Data struct {
		List  []sessionListItem `json:"List"`
		Total int               `json:"Total"`
		Page  int               `json:"Page"`
		Size  int               `json:"Size"`
	} `json:"Data"`
}

// TestHandleDevListSessions_WithRows 有会话数据时也必须 200（Scan 目标与列清单逐列对齐）。
func TestHandleDevListSessions_WithRows(t *testing.T) {
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer conn.Close()

	dev := newTestDev(conn, 7)
	dev.SetDB(conn)
	now := fixedNow()
	dev.now = fixedNow

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM sessions WHERE user_id = $1`)).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))

	rows := sqlmock.NewRows(sessionListColsForTest()).
		AddRow("sess-1", int64(7), int64(10), "gpt-4", "raw-sess-1", "gpt-4-int",
			now.Add(-time.Hour), now, now.Add(2*time.Hour), "我的会话", false)
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT `+sessionListCols+` FROM sessions WHERE user_id = $1 ORDER BY last_active DESC, session_id ASC LIMIT $2 OFFSET $3`)).
		WithArgs(int64(7), 20, 0).WillReturnRows(rows)

	// sessionRuntimes 的两条真实查询这里不设期望：库里没有账单行时它们返回空，
	// 与本用例要守的「Scan 目标数 ≡ 列数」无关（返回值非 nil 的行集）。

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dev/sessions", nil)
	rec := httptest.NewRecorder()
	dev.HandleListSessions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200；body=%s", rec.Code, rec.Body.String())
	}
	var body devSessionListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v（原始响应 %s）", err, rec.Body.String())
	}
	if body.Data.Total != 1 || len(body.Data.List) != 1 {
		t.Fatalf("total=%d len=%d, want 1/1", body.Data.Total, len(body.Data.List))
	}
	item := body.Data.List[0]
	if item.SessionID != "sess-1" || item.SessionName != "我的会话" || item.TokenID != 10 {
		t.Fatalf("会话字段未正确回填：%+v", item)
	}
	// 渠道维度已随 0024 下线：展示名恒空（不是 500、也不是脏值）。
	if item.ChannelKeyID != 0 || item.ChannelKeyName != "" || item.ChannelName != "" {
		t.Fatalf("渠道维度应恒零/空，实际 %+v", item)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的 SQL 期望：%v", err)
	}
}

// TestHandleDevListSessions_EmptyRows 无会话时返回空列表（确认 0 行也不炸）。
func TestHandleDevListSessions_EmptyRows(t *testing.T) {
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer conn.Close()

	dev := newTestDev(conn, 7)
	dev.SetDB(conn)
	dev.now = fixedNow

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM sessions WHERE user_id = $1`)).
		WithArgs(int64(7)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT ` + sessionListCols + ` FROM sessions WHERE user_id = $1`)).
		WillReturnRows(sqlmock.NewRows(sessionListColsForTest()))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dev/sessions", nil)
	rec := httptest.NewRecorder()
	dev.HandleListSessions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200；body=%s", rec.Code, rec.Body.String())
	}
	var body devSessionListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Data.Total != 0 || len(body.Data.List) != 0 {
		t.Fatalf("应为空列表，实际 total=%d len=%d", body.Data.Total, len(body.Data.List))
	}
}
