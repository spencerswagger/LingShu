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
	"github.com/team/llmgateway/internal/domain/gateway"
)

// fakeCallLogStore 内存调用日志读取 fake（console 单测用，满足 callLogReader）。
type fakeCallLogStore struct {
	byBilling map[string]*gateway.CallLog
	bySession []gateway.CallLog
}

func (f *fakeCallLogStore) GetByBillingID(_ context.Context, bid string) (*gateway.CallLog, error) {
	return f.byBilling[bid], nil
}

func (f *fakeCallLogStore) ListBySession(_ context.Context, _ string) ([]gateway.CallLog, error) {
	return f.bySession, nil
}

// billingRecordRow 与 billing.SqlStore 的 recordCols 列顺序一致的一行 mock 数据。
func billingRecordRow(t *testing.T) ([]string, *sqlmock.Rows) {
	t.Helper()
	cols := []string{"id", "billing_id", "user_id", "pricing_mode", "token_id", "external_model_name",
		"internal_model_id", "channel_key_id", "session_id", "session_name", "call_time", "tokens", "rates",
		"coefficients", "r_value", "raw_total", "credits_consumed", "cost_credits", "balance_before",
		"balance_after", "status", "error_message", "duration_ms", "first_token_ms"}
	rows := sqlmock.NewRows(cols).AddRow(
		int64(1), "bill-0001", int64(7), "sale", nil, "gpt-4", "gpt-4-internal", int64(1), nil, nil,
		time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), []byte(`{"input":10}`), []byte(`{"input":1}`),
		[]byte(`{"time":1,"context":1}`), int64(10000), 1.0, 1.0, 0.0, 100.0, 99.0, "completed", nil, nil, nil)
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

	admin := NewAdmin(db, billing.NewSqlStore(db), nil, nil)
	admin.SetCallLogStore(&fakeCallLogStore{byBilling: map[string]*gateway.CallLog{
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
	internal_model_id, channel_key_id, session_id, session_name, call_time, tokens, rates, coefficients, r_value,
	raw_total, credits_consumed, cost_credits, balance_before, balance_after, status, error_message, duration_ms, first_token_ms FROM billing_records WHERE billing_id=$1`)).
		WithArgs("bill-0001").WillReturnRows(rows)
	// 渠道名反查：密钥 id=1 无对应渠道行 → 空名（nil 影响展示）。
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT c.name FROM channels c JOIN channel_keys k ON k.channel_id = c.id WHERE k.id = $1`)).
		WithArgs(int64(1)).WillReturnError(sql.ErrNoRows)

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

	admin := NewAdmin(db, billing.NewSqlStore(db), nil, nil) // 不注入 callLogStore

	_, rows := billingRecordRow(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, billing_id, user_id, pricing_mode, token_id, external_model_name,
	internal_model_id, channel_key_id, session_id, session_name, call_time, tokens, rates, coefficients, r_value,
	raw_total, credits_consumed, cost_credits, balance_before, balance_after, status, error_message, duration_ms, first_token_ms FROM billing_records WHERE billing_id=$1`)).
		WithArgs("bill-0001").WillReturnRows(rows)
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
	admin := NewAdmin(nil, nil, nil, nil)
	admin.SetCallLogStore(&fakeCallLogStore{bySession: []gateway.CallLog{
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
	empty := NewAdmin(nil, nil, nil, nil)
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

// TestNewSQLCallLogStore_InsertAndQuery 用 sqlmock 验证 SQLCallLogStore 的 Insert（ON CONFLICT 幂等）与 ListBySession 读取。
func TestNewSQLCallLogStore_InsertAndQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := gateway.NewSQLCallLogStore(db)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	dur := int64(123)
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO call_logs(id, billing_id, request_id, session_id, user_id, channel_id, channel_key_id,
	internal_model_id, external_model_id, model, pricing_mode, status, req_messages, resp_body, resp_kind,
	decision, error_message, duration_ms, first_token_ms, created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
ON CONFLICT (billing_id) DO NOTHING`)).
		WithArgs(sqlmock.AnyArg(), "bill-0001", "req-1", "sess-1", int64(7), int64(1), int64(2), "gpt-4-internal",
			int64(1), "gpt-4", "sale", "completed", []byte(`[{"role":"user","content":"a"}]`), `{"id":"x"}`,
			"non_stream", []byte(`{"result":"success"}`), "", int64(123), nil, now).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.Insert(context.Background(), &gateway.CallLog{
		BillingID: "bill-0001", RequestID: "req-1", SessionID: "sess-1",
		ChannelID: ptr64(1), ChannelKeyID: ptr64(2), UserID: ptr64(7), ExternalModelID: ptr64(1),
		InternalModelID: "gpt-4-internal", Model: "gpt-4", PricingMode: "sale", Status: "completed",
		ReqMessages: []json.RawMessage{json.RawMessage(`{"role":"user","content":"a"}`)},
		RespBody:    `{"id":"x"}`, RespKind: "non_stream",
		Decision: map[string]any{"result": "success"}, DurationMs: &dur, FirstTokenMs: nil, CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// ListBySession 读取（ReqMessages/Decision 为 JSONB bytes）。
	rows := sqlmock.NewRows([]string{"billing_id", "request_id", "session_id", "user_id", "channel_id", "channel_key_id",
		"internal_model_id", "external_model_id", "model", "pricing_mode", "status", "req_messages", "resp_body", "resp_kind",
		"decision", "error_message", "duration_ms", "first_token_ms", "created_at"}).
		AddRow("bill-0001", "req-1", "sess-1", int64(7), int64(1), int64(2), "gpt-4-internal", int64(1), "gpt-4",
			"sale", "completed", []byte(`[{"role":"user","content":"a"}]`), `{"id":"x"}`, "non_stream",
			[]byte(`{"result":"success"}`), "", int64(123), nil, now)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT billing_id, request_id, session_id, user_id, channel_id, channel_key_id,
	internal_model_id, external_model_id, model, pricing_mode, status, req_messages, resp_body, resp_kind,
	decision, error_message, duration_ms, first_token_ms, created_at FROM call_logs WHERE session_id=$1 ORDER BY created_at ASC`)).
		WithArgs("sess-1").WillReturnRows(rows)

	logs, err := store.ListBySession(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("list by session: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("list len want 1, got %d", len(logs))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func ptr64(v int64) *int64 { return &v }
