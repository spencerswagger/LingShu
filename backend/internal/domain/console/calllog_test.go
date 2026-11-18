package console

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/calllog"
)

// fakeCallLogStore 内存调用日志读取 fake（console 单测用，满足 callLogReader）。
type fakeCallLogStore struct {
	byBilling map[string]*calllog.CallLog
	bySession []calllog.CallLog
}

func (f *fakeCallLogStore) GetByBillingID(_ context.Context, bid string) (*calllog.CallLog, error) {
	return f.byBilling[bid], nil
}

func (f *fakeCallLogStore) ListBySession(_ context.Context, _ string) ([]calllog.CallLog, error) {
	return f.bySession, nil
}

// billingRecordRow 与 billing.SqlStore 的 recordCols 列顺序一致的一行 mock 数据。
func billingRecordRow(t *testing.T) ([]string, *sqlmock.Rows) {
	t.Helper()
	// 列顺序与 billing.SqlStore 的 recordCols 一致：channel_key_id 已不在表里（0021 删除），
	// 维度改由 call_logs 快照回填。
	cols := []string{"id", "billing_id", "user_id", "pricing_mode", "token_id", "external_model_name",
		"auto_model_name", "internal_model_id", "session_id", "session_name", "call_time", "tokens", "rates",
		"coefficients", "r_value", "raw_total", "credits_consumed", "cost_credits", "balance_before",
		"balance_after", "status", "error_message", "duration_ms", "first_token_ms",
		"upstream_status", "upstream_error_summary"}
	rows := sqlmock.NewRows(cols).AddRow(
		int64(1), "bill-0001", int64(7), "sale", nil, "gpt-4", nil, "gpt-4-internal", nil, nil,
		time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), []byte(`{"input":10}`), []byte(`{"input":1}`),
		[]byte(`{"time":1,"context":1}`), int64(10000), 1.0, 1.0, 0.0, 100.0, 99.0, "completed", nil, nil, nil, nil, nil)
	return cols, rows
}

// TestAdminHandleGetBilling_CallLog 断言 admin 账单详情联查 call_logs 并填充 CallLogView
// （ReqMessages/Decision 反序列化为 any；未装配时 CallLog 为 nil）。
func TestAdminHandleGetBilling_CallLog(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	admin := NewAdmin(db, billing.NewSqlStore(db), nil)
	admin.SetCallLogStore(&fakeCallLogStore{byBilling: map[string]*calllog.CallLog{
		"bill-0001": {
			BillingID: "bill-0001", RequestID: "req-1", SessionID: "sess-1",
			Model: "gpt-4", Status: "completed", RespKind: "non_stream",
			RespBody:    `{"id":"x"}`,
			ReqMessages: json.RawMessage(`[{"role":"user","content":"a"}]`),
			Decision:    json.RawMessage(`{"result":"success"}`),
			PricingMode: "sale", CreatedAt: time.Date(2026, 10, 1, 10, 1, 0, 0, time.UTC),
		},
	}})

	_, rows := billingRecordRow(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, billing_id, user_id, pricing_mode, token_id, external_model_name,
	auto_model_name, internal_model_id, session_id, session_name, call_time, tokens, rates, coefficients, r_value,
	raw_total, credits_consumed, cost_credits, balance_before, balance_after, status, error_message, duration_ms, first_token_ms,
	upstream_status, upstream_error_summary FROM billing_records WHERE billing_id=$1`)).
		WithArgs("bill-0001").WillReturnRows(rows)
	// 渠道密钥维度回填（billing_records 已无该列，读 call_logs 的 1:1 快照）。
	mock.ExpectQuery(`SELECT cl\.billing_id, cl\.channel_key_id`).
		WithArgs("bill-0001").
		WillReturnRows(sqlmock.NewRows([]string{"billing_id", "channel_key_id"}).AddRow("bill-0001", int64(1)))
	// 渠道名反查已随 0024 下线（channels / channel_keys 已删）：不再发查询，字段恒空。

	rec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/billings/bill-0001", nil)
	getReq.SetPathValue("billing_id", "bill-0001")
	admin.HandleGetBilling(rec, getReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
	var body struct {
		Data struct {
			CallLog *CallLogView `json:"CallLog"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cl := body.Data.CallLog
	if cl == nil {
		t.Fatal("BillingDetail.CallLog want non-nil, got nil")
	}
	if cl.BillingID != "bill-0001" || cl.Model != "gpt-4" || cl.RespKind != "non_stream" {
		t.Errorf("call log view fields wrong: %+v", cl)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"ReqMessages":[{"content":"a","role":"user"}]`)) &&
		!bytes.Contains(rec.Body.Bytes(), []byte(`"ReqMessages":[{"role":"user","content":"a"}]`)) {
		t.Errorf("ReqMessages 未反序列化输出: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"Decision":{"result":"success"}`)) {
		t.Errorf("Decision 未反序列化输出: %s", rec.Body.String())
	}
}

// TestAdminHandleGetBilling_CallLogNotInjected 断言未注入调用日志存储时 Detail.CallLog 为 null（缺省不输出）。
func TestAdminHandleGetBilling_CallLogNotInjected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	admin := NewAdmin(db, billing.NewSqlStore(db), nil) // 不注入 callLogStore

	_, rows := billingRecordRow(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, billing_id, user_id, pricing_mode, token_id, external_model_name,
	auto_model_name, internal_model_id, session_id, session_name, call_time, tokens, rates, coefficients, r_value,
	raw_total, credits_consumed, cost_credits, balance_before, balance_after, status, error_message, duration_ms, first_token_ms,
	upstream_status, upstream_error_summary FROM billing_records WHERE billing_id=$1`)).
		WithArgs("bill-0001").WillReturnRows(rows)
	// 渠道密钥维度回填（billing_records 已无该列，读 call_logs 的 1:1 快照）。
	mock.ExpectQuery(`SELECT cl\.billing_id, cl\.channel_key_id`).
		WithArgs("bill-0001").
		WillReturnRows(sqlmock.NewRows([]string{"billing_id", "channel_key_id"}).AddRow("bill-0001", int64(1)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT c.name FROM channels c JOIN channel_keys k ON k.channel_id = c.id WHERE k.id = $1`)).
		WithArgs(int64(1)).WillReturnError(sql.ErrNoRows)

	rec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/billings/bill-0001", nil)
	getReq.SetPathValue("billing_id", "bill-0001")
	admin.HandleGetBilling(rec, getReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"CallLog"`)) {
		t.Errorf("未注入 callLogStore 时不应输出 CallLog 字段: %s", rec.Body.String())
	}
}

// TestAdminHandleListSessionCalls 断言 GET /api/v1/admin/sessions/{id}/calls 返回 {List, Total}（升序全量）。
func TestAdminHandleListSessionCalls(t *testing.T) {
	admin := NewAdmin(nil, nil, nil)
	admin.SetCallLogStore(&fakeCallLogStore{bySession: []calllog.CallLog{
		{BillingID: "bill-0001", SessionID: "sess-1", Model: "gpt-4", Status: "completed", RespKind: "non_stream",
			RespBody: `{"id":"x"}`, ReqMessages: json.RawMessage(`[{"role":"user","content":"a"}]`),
			Decision: json.RawMessage(`{"attempts":[]}`), PricingMode: "sale",
			CreatedAt: time.Date(2026, 10, 1, 10, 1, 0, 0, time.UTC)},
		{BillingID: "bill-0002", SessionID: "sess-1", Model: "gpt-4", Status: "failed", RespKind: "error",
			RespBody: "", ReqMessages: json.RawMessage(`[{"role":"user","content":"b"}]`),
			Decision: json.RawMessage(`{"result":"rejected"}`), PricingMode: "sale",
			ErrorMessage: "无可用渠道",
			CreatedAt:    time.Date(2026, 10, 1, 10, 2, 0, 0, time.UTC)},
	}})

	rec := httptest.NewRecorder()
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/sessions/sess-1/calls", nil)
	listReq.SetPathValue("session_id", "sess-1")
	admin.HandleListSessionCalls(rec, listReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			List  []CallLogView `json:"List"`
			Total int           `json:"Total"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Data.Total != 2 || len(body.Data.List) != 2 {
		t.Fatalf("Total/List want 2/2, got %d/%d", body.Data.Total, len(body.Data.List))
	}
	if body.Data.List[0].BillingID != "bill-0001" || body.Data.List[1].BillingID != "bill-0002" {
		t.Errorf("call list order wrong: %+v", body.Data.List)
	}
	if body.Data.List[1].Status != "failed" || body.Data.List[1].RespKind != "error" {
		t.Errorf("second item should be failed/error, got %+v", body.Data.List[1])
	}

	// 空列表：未装配存储时返回 List=[]（非 null）+ Total=0。
	empty := NewAdmin(nil, nil, nil)
	rec2 := httptest.NewRecorder()
	emptyReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/sessions/sess-x/calls", nil)
	emptyReq.SetPathValue("session_id", "sess-x")
	empty.HandleListSessionCalls(rec2, emptyReq)
	if rec2.Code != http.StatusOK {
		t.Fatalf("empty status=%d, want 200", rec2.Code)
	}
	if !bytes.Contains(rec2.Body.Bytes(), []byte(`"List":[]`)) || !bytes.Contains(rec2.Body.Bytes(), []byte(`"Total":0`)) {
		t.Errorf("empty list want List=[]/Total=0, got %s", rec2.Body.String())
	}
}
