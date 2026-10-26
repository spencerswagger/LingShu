package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// costSessionResp /cost/session 响应结构（测试用）。
type costSessionResp struct {
	Code int `json:"Code"`
	Data struct {
		SessionID string             `json:"SessionID"`
		Points    []costSessionPoint `json:"Points"`
		Totals    costSessionTotals  `json:"Totals"`
		Total     int64              `json:"Total"`
		Limit     int                `json:"Limit"`
		Truncated bool               `json:"Truncated"`
	} `json:"Data"`
}

// sessionReq 构造带 path value 的会话成本请求（PathValue 只在 ServeMux 分发时自动填充，
// 直接调用 handler 时必须手动 SetPathValue）。
func sessionReq(sid, query string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/session/"+sid+query, nil)
	req.SetPathValue("id", sid)
	return req
}

// mustTime 解析 RFC3339 时间（测试辅助）。
func mustTime(t *testing.T, raw string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("解析时间 %q 失败: %v", raw, err)
	}
	return tm
}

// costSessionRowCols /cost/session 明细查询列（顺序与实现一致）。
func costSessionRowCols() []string {
	return []string{"call_time", "channel_name", "channel_key_id", "internal_model_id", "status",
		"input_tokens", "cache_read_tokens", "output_tokens", "cost_credits", "sale_credits"}
}

// TestHandleCostSession_FailedRounds 断言失败轮出行且占位：
// Turn 只数成功轮（不重排）、TurnNo 数时间线（含失败轮）、失败轮金额 0、合计只算成功轮。
func TestHandleCostSession_FailedRounds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM billing_records WHERE session_id = $1`)).
		WithArgs("s-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(5)))

	rows := sqlmock.NewRows(costSessionRowCols()).
		AddRow(mustTime(t, "2026-10-25T10:00:00+08:00"), "渠道A", int64(100), "gpt-4o-int", "completed", int64(100), int64(10), int64(50), 1.5, 3.0).
		// 失败轮在库里刻意给**非零**金额：接口必须把它们归零（否则下面「金额 0」的断言会平凡通过）。
		AddRow(mustTime(t, "2026-10-25T10:01:00+08:00"), "渠道A", int64(100), "gpt-4o-int", "failed", int64(80), int64(0), int64(0), 9.99, 19.99).
		AddRow(mustTime(t, "2026-10-25T10:02:00+08:00"), "渠道B", int64(200), "gpt-4o-int", "completed", int64(200), int64(20), int64(80), 2.5, 5.0).
		AddRow(mustTime(t, "2026-10-25T10:03:00+08:00"), "渠道B", int64(200), "gpt-4o-int", "failed", int64(0), int64(0), int64(0), 0.12345, 0.54321).
		AddRow(mustTime(t, "2026-10-25T10:04:00+08:00"), "渠道A", int64(100), "gpt-4o-int", "completed", int64(300), int64(30), int64(90), 3.5, 7.0)
	mock.ExpectQuery(regexp.QuoteMeta(`ORDER BY b.call_time ASC, b.id ASC`)).
		WithArgs("s-1", nil).
		WillReturnRows(rows)

	h := NewAdmin(db, nil, nil, nil)
	rec := httptest.NewRecorder()
	h.HandleCostSession(rec, sessionReq("s-1", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body costSessionResp
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pts := body.Data.Points
	if len(pts) != 5 {
		t.Fatalf("点数=%d，want 5（失败轮也要出行）", len(pts))
	}
	// TurnNo 是时间线序号（含失败轮），Turn 只数成功轮 —— 失败轮不把后面的成功轮往后推。
	wantTurn := []int{1, 0, 2, 0, 3}
	wantStatus := []string{"completed", "failed", "completed", "failed", "completed"}
	for i, pt := range pts {
		if pt.TurnNo != i+1 {
			t.Fatalf("第 %d 点 TurnNo=%d，want %d", i, pt.TurnNo, i+1)
		}
		if pt.Turn != wantTurn[i] {
			t.Fatalf("第 %d 点 Turn=%d，want %d（成功轮序号不重排）", i, pt.Turn, wantTurn[i])
		}
		if pt.Status != wantStatus[i] {
			t.Fatalf("第 %d 点 Status=%q，want %q", i, pt.Status, wantStatus[i])
		}
		if pt.Failed != (wantStatus[i] == "failed") {
			t.Fatalf("第 %d 点 Failed=%v，want %v", i, pt.Failed, wantStatus[i] == "failed")
		}
		if pt.Failed && (pt.CostCredits != 0 || pt.SaleCredits != 0) {
			t.Fatalf("第 %d 点失败轮金额必须 0（库里给的是非零）：%+v", i, pt)
		}
		if !pt.Failed && (pt.CostCredits == 0 || pt.SaleCredits == 0) {
			t.Fatalf("第 %d 点成功轮金额不应为 0：%+v", i, pt)
		}
	}
	// 失败轮保留实际 token（上游可能已消耗），只是不计金额。
	if pts[1].InputTokens != 80 || pts[1].OutputTokens != 0 {
		t.Fatalf("失败轮 token 应保留实际值: %+v", pts[1])
	}
	// 合计只算成功轮：cost 1.5+2.5+3.5=7.5，sale 3+5+7=15。
	if body.Data.Totals.CostCredits != 7.5 || body.Data.Totals.SaleCredits != 15 {
		t.Fatalf("合计=%+v，want cost=7.5 sale=15", body.Data.Totals)
	}
	if body.Data.Total != 5 || body.Data.Limit != costSessionDefaultLimit || body.Data.Truncated {
		t.Fatalf("元信息错: %+v", body.Data)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的期望: %v", err)
	}
}

// TestHandleCostSession_OldCallerCompat 断言全是成功轮时，老调用方读到的
// Turn 序列、顺序、金额与合计都与改造前一致（TurnNo 恒等于 Turn）。
func TestHandleCostSession_OldCallerCompat(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM billing_records WHERE session_id = $1`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(3)))
	mock.ExpectQuery(regexp.QuoteMeta(`ORDER BY b.call_time ASC, b.id ASC`)).
		WillReturnRows(sqlmock.NewRows(costSessionRowCols()).
			AddRow(mustTime(t, "2026-10-25T10:00:00+08:00"), "", nil, "m", "completed", int64(10), int64(0), int64(5), 0.00001, 0.00002).
			AddRow(mustTime(t, "2026-10-25T10:01:00+08:00"), "", nil, "m", "completed", int64(20), int64(0), int64(5), 0.00002, 0.00003).
			AddRow(mustTime(t, "2026-10-25T10:02:00+08:00"), "", nil, "m", "completed", int64(30), int64(0), int64(5), 0.00003, 0.00004))

	h := NewAdmin(db, nil, nil, nil)
	rec := httptest.NewRecorder()
	h.HandleCostSession(rec, sessionReq("s-2", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body costSessionResp
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for i, pt := range body.Data.Points {
		if pt.Turn != i+1 || pt.TurnNo != i+1 {
			t.Fatalf("第 %d 点 Turn/TurnNo=%d/%d，want %d/%d", i, pt.Turn, pt.TurnNo, i+1, i+1)
		}
		if pt.Failed || pt.Status != "completed" {
			t.Fatalf("成功行不应被标失败: %+v", pt)
		}
	}
	// 渠道名缺失且无密钥 ID 时保持空串（老行为）。
	if body.Data.Points[0].ChannelName != "" {
		t.Fatalf("无渠道名时应为空串，got %q", body.Data.Points[0].ChannelName)
	}
	if body.Data.Totals.CostCredits != 0.00006 || body.Data.Totals.SaleCredits != 0.00009 {
		t.Fatalf("合计=%+v，want 0.00006/0.00009", body.Data.Totals)
	}
	if body.Data.Truncated {
		t.Fatalf("未截断时 Truncated 应为 false")
	}
}

// TestHandleCostSession_Truncated 断言行数上限：Limit 生效并回报 Total/Truncated。
func TestHandleCostSession_Truncated(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM billing_records WHERE session_id = $1`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(900)))
	mock.ExpectQuery(regexp.QuoteMeta(`ORDER BY b.call_time ASC, b.id ASC`)).
		WithArgs("s-3", 2).
		WillReturnRows(sqlmock.NewRows(costSessionRowCols()).
			AddRow(mustTime(t, "2026-10-25T10:00:00+08:00"), "c", nil, "m", "completed", int64(1), int64(0), int64(1), 0.1, 0.2).
			AddRow(mustTime(t, "2026-10-25T10:01:00+08:00"), "c", nil, "m", "failed", int64(1), int64(0), int64(0), 0, 0))

	h := NewAdmin(db, nil, nil, nil)
	rec := httptest.NewRecorder()
	h.HandleCostSession(rec, sessionReq("s-3", "?Limit=2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body costSessionResp
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data.Points) != 2 || body.Data.Total != 900 || !body.Data.Truncated || body.Data.Limit != 2 {
		t.Fatalf("截断元信息错: %+v", body.Data)
	}
}

// TestParseCostSessionLimit 断言 Limit 缺省/收敛/报错。
func TestParseCostSessionLimit(t *testing.T) {
	cases := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", 0, false}, // 缺省 = 不截断
		{"100", 100, false},
		{"0", 0, false},
		{"-5", 0, false},
		{"999999", costSessionMaxLimit, false},
		{"abc", 0, true},
	}
	for _, c := range cases {
		got, err := parseCostSessionLimit(c.raw)
		if c.wantErr {
			if err == nil {
				t.Fatalf("raw=%q 应报错", c.raw)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("raw=%q got=%d err=%v，want %d", c.raw, got, err, c.want)
		}
	}
}

// TestCostSessionQuery_IncludesFailed 守护：会话明细查询不得再过滤 status（失败轮必须出行），
// 且必须带 LIMIT 上限。
func TestCostSessionQuery_IncludesFailed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM billing_records WHERE session_id = $1`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta(`ORDER BY b.call_time ASC, b.id ASC`)).
		WillReturnRows(sqlmock.NewRows(costSessionRowCols()))

	h := NewAdmin(db, nil, nil, nil)
	rec := httptest.NewRecorder()
	h.HandleCostSession(rec, sessionReq("s-4", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 空会话：Points 必须是 []，Totals 为 0（不返回 null）。
	if !regexp.MustCompile(`"Points":\[\]`).MatchString(rec.Body.String()) {
		t.Fatalf("空会话 Points 应为 []，body=%s", rec.Body.String())
	}
}

// TestHandleCostSession_DefaultNoLimit 断言缺省**不截断**：超过 500 行的会话全量返回，
// Truncated=false、Limit=0（= 不限），老调用方不会少拿尾部数据。
func TestHandleCostSession_DefaultNoLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	const n = 600
	rows := sqlmock.NewRows(costSessionRowCols())
	for i := 0; i < n; i++ {
		status := "completed"
		if i%7 == 3 {
			status = "failed"
		}
		rows.AddRow(mustTime(t, "2026-10-25T10:00:00+08:00"), "c", int64(100), "m", status,
			int64(10), int64(0), int64(5), 0.00001, 0.00002)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM billing_records WHERE session_id = $1`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(n)))
	// 缺省 → LIMIT 传 NULL（= 不限），不能用 500 之类的默认值。
	mock.ExpectQuery(regexp.QuoteMeta(`ORDER BY b.call_time ASC, b.id ASC`)).
		WithArgs("s-long", nil).
		WillReturnRows(rows)

	h := NewAdmin(db, nil, nil, nil)
	rec := httptest.NewRecorder()
	h.HandleCostSession(rec, sessionReq("s-long", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body costSessionResp
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data.Points) != n {
		t.Fatalf("行数=%d，want %d（缺省不得截断）", len(body.Data.Points), n)
	}
	if body.Data.Truncated || body.Data.Limit != 0 || body.Data.Total != n {
		t.Fatalf("元信息错: Limit=%d Total=%d Truncated=%v", body.Data.Limit, body.Data.Total, body.Data.Truncated)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的期望: %v", err)
	}
}

// TestHandleCostSession_ExplicitLimitOverCap 断言显式 Limit 超上限按 2000 收敛（仍能截断）。
func TestHandleCostSession_ExplicitLimitOverCap(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM billing_records WHERE session_id = $1`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(5000)))
	mock.ExpectQuery(regexp.QuoteMeta(`ORDER BY b.call_time ASC, b.id ASC`)).
		WithArgs("s-huge", costSessionMaxLimit).
		WillReturnRows(sqlmock.NewRows(costSessionRowCols()).
			AddRow(mustTime(t, "2026-10-25T10:00:00+08:00"), "c", nil, "m", "completed", int64(1), int64(0), int64(1), 0.1, 0.2))

	h := NewAdmin(db, nil, nil, nil)
	rec := httptest.NewRecorder()
	h.HandleCostSession(rec, sessionReq("s-huge", "?Limit=99999"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body costSessionResp
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Data.Limit != costSessionMaxLimit {
		t.Fatalf("Limit=%d，want %d（显式超上限按上限收敛）", body.Data.Limit, costSessionMaxLimit)
	}
	if !body.Data.Truncated || body.Data.Total != 5000 {
		t.Fatalf("截断元信息错: Total=%d Truncated=%v", body.Data.Total, body.Data.Truncated)
	}
}
