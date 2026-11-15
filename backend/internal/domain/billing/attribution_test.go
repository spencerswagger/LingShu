package billing

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestAttributionArgs_Normalization 断言七列的归一口径：
// 缺失留 NULL（不猜）、cost_source 空归 none、**非法枚举一律归一而非报错**
// （2026-11-15 预审 P1：归因非法曾经让账单写不进去 → 扣了钱没账）。
func TestAttributionArgs_Normalization(t *testing.T) {
	ctx := context.Background()
	rec := &Record{BillingID: "bill-1"}
	args := attributionArgs(ctx, rec)
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
	args = attributionArgs(ctx, full)
	want := []any{"req-2", "anthropic", "acct-9", "anthropic-messages", "claude-sonnet-4", "led-9", CostSourceLedger}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("第 %d 列应为 %v，got %v", i+1, want[i], args[i])
		}
	}

	// 非法协议 → NULL（不报错），非法成本来源 → none（不报错）。
	bad := &Record{BillingID: "bill-3", UpstreamProtocol: "openai-embeddings", CostSource: "invoice"}
	got := attributionArgs(ctx, bad)
	if got[3] != nil {
		t.Fatalf("非法协议应归一为 NULL，got %v", got[3])
	}
	if got[6] != CostSourceNone {
		t.Fatalf("非法成本来源应归一为 none，got %v", got[6])
	}

	// 带首尾空白的合法值：trim 后判合法，并按 trim 后的枚举落库（CHECK 约束只认枚举本身）。
	spaced := &Record{BillingID: "bill-5", UpstreamProtocol: " openai-chat ", CostSource: " ledger "}
	gotSpaced := attributionArgs(ctx, spaced)
	if gotSpaced[3] != "openai-chat" {
		t.Fatalf("带空白的合法协议应按 trim 后落库，got %v", gotSpaced[3])
	}
	if gotSpaced[6] != "ledger" {
		t.Fatalf("带空白的合法成本来源应按 trim 后落库，got %v", gotSpaced[6])
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
	args = append(append([]driver.Value{}, args[:25]...),
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
