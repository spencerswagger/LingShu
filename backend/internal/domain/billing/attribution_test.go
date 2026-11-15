package billing

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestAttributionArgs_Normalization 断言六列的归一/校验口径：
// 缺失留 NULL（不猜）、cost_source 空归 none、非法枚举在写库前就报错（不留到 CHECK 约束里）。
func TestAttributionArgs_Normalization(t *testing.T) {
	rec := &Record{BillingID: "bill-1"}
	args, err := attributionArgs(rec)
	if err != nil {
		t.Fatalf("空归因不应报错: %v", err)
	}
	for i := 0; i < 6; i++ { // request_id / upstream_provider / upstream_account / upstream_protocol / upstream_model / ledger_entry_id
		if args[i] != nil {
			t.Fatalf("第 %d 列缺失时应写 NULL，got %v", i+1, args[i])
		}
	}
	if args[6] != CostSourceNone {
		t.Fatalf("cost_source 空值应归一为 none，got %v", args[6])
	}

	full := &Record{
		BillingID:        "bill-2",
		RequestID:        "req-2",
		UpstreamProvider: "anthropic",
		UpstreamAccount:  "acct-9",
		UpstreamProtocol: "anthropic-messages",
		UpstreamModel:    "claude-sonnet-4",
		LedgerEntryID:    "led-9",
		CostSource:       CostSourceLedger,
	}
	args, err = attributionArgs(full)
	if err != nil {
		t.Fatalf("合法归因不应报错: %v", err)
	}
	want := []any{"req-2", "anthropic", "acct-9", "anthropic-messages", "claude-sonnet-4", "led-9", CostSourceLedger}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("第 %d 列应为 %v，got %v", i+1, want[i], args[i])
		}
	}

	bad := &Record{BillingID: "bill-3", UpstreamProtocol: "openai-embeddings"}
	if _, err := attributionArgs(bad); err == nil || !strings.Contains(err.Error(), "upstream_protocol") {
		t.Fatalf("非法协议应报错并点名字段，got %v", err)
	}
	bad = &Record{BillingID: "bill-4", CostSource: "invoice"}
	if _, err := attributionArgs(bad); err == nil || !strings.Contains(err.Error(), "cost_source") {
		t.Fatalf("非法成本来源应报错并点名字段，got %v", err)
	}
}

// TestRecord_InsertsAttributionColumns 断言归因值真的进了 INSERT 的第 26–31 个参数
// （不是只在结构体里挂着）。
func TestRecord_InsertsAttributionColumns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	usage := Usage{Input: 1234, Output: 567}
	rates := fullRates()
	credits, err := ComputeCredits(usage, rates, 1.0, 1.0, 10000)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	before, after := 10.0, 9.7632

	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).WillReturnError(sql.ErrNoRows)

	args := txInsertArgs(credits, before, after, "completed")
	// 末 6 位换成真实归因值（前 25 位沿用既有断言）。
	args = append(append([]driver.Value{}, args[:24]...),
		"req-attr-1", "openai", "acct-7", "openai-chat", "gpt-4o", "led-attr-1", CostSourceLedger)
	mock.ExpectQuery(insertBill.String()).WithArgs(args...).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))

	credit := &fakeCredit{before: before, after: after}
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, shLocForTest(t))
	svc := newTestService(db, credit, newFakeConfig(), now)

	rec, err := svc.Record(context.Background(), RecordReq{
		BillingID: billingID, UserID: testUserID, PricingMode: "sale",
		ExternalModel: "ext-model", InternalModelID: "int-model", ChannelKeyID: 1, SessionID: "sess-1",
		CallTime: now, Tokens: usage, Rates: rates,
		RequestID:        "req-attr-1",
		UpstreamProvider: "openai",
		UpstreamAccount:  "acct-7",
		UpstreamProtocol: "openai-chat",
		UpstreamModel:    "gpt-4o",
		LedgerEntryID:    "led-attr-1",
		CostSource:       CostSourceLedger,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.UpstreamProtocol != "openai-chat" || rec.CostSource != CostSourceLedger {
		t.Fatalf("返回记录应带回归因值，got %+v", rec)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
