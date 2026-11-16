package billing

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/pkg/dbx"
)

// hold_expiry_test.go 覆盖「预扣已被过期扫描退回之后再结算」这一条资金路径。
//
// 缺陷（Owl 真库 A/B 实测，2026-11-16）：`ExpireHolds` 在请求活过 15 分钟后会把整笔预扣
// 全额退回钱包（`main.go` 每 60s 扫一次）；随后结算仍按 `credits - PreConsumed` 扣，
// 等于把已经退过的那笔预扣又减一次 —— 实测 credits=12、预扣 10 时，过期先到余额成 98（而非 88），
// 多出的 10 恰好是预扣额。
//
// 根因：终结（CloseHoldTx）放在事务尾、且丢弃了 owned=false。
// 现口径：**事务开头先抢终结权**；owned=false 说明预扣已不在 held（典型是被过期退回），
// 本次必须按全额扣，不得再退预扣。

// fakeCloser 是预扣终结能力的桩：按预设序列返回 owned，并记录调用顺序。
type fakeCloser struct {
	mu    sync.Mutex
	owned []bool // 依次返回；用完后沿用最后一个
	errs  []error
	calls int
	order *[]string // 可选：与 fakeCredit 共享的顺序记录
}

func (f *fakeCloser) CloseHoldTx(_ context.Context, _ dbx.Execer, billingID, status, ledgerEntryID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := f.calls
	f.calls++
	if f.order != nil {
		*f.order = append(*f.order, "close-hold")
	}
	var owned bool
	switch {
	case len(f.owned) == 0:
		owned = true
	case idx < len(f.owned):
		owned = f.owned[idx]
	default:
		owned = f.owned[len(f.owned)-1]
	}
	var err error
	if idx < len(f.errs) {
		err = f.errs[idx]
	}
	return owned, err
}

// holdReq 构造一个「带预扣」的结算请求。
func holdReq(closer HoldCloser, pre float64, usage Usage, rates Rates) RecordReq {
	return RecordReq{
		BillingID: billingID, UserID: testUserID, PricingMode: "sale",
		ExternalModel: "ext-model", InternalModelID: "int-model", ChannelKeyID: 1, SessionID: "sess-1",
		Tokens: usage, Rates: rates,
		PreConsumed: pre,
		Hold:        &HoldCloseReq{BillingID: billingID, Status: "settled", Closer: closer},
	}
}

// TestRecord_HoldExpiredBeforeSettle_ChargesFull 是重复退款缺陷的主回归用例：
// 预扣已被过期扫描退回（owned=false）时，本次结算必须按**全额**扣，不得再减 PreConsumed。
func TestRecord_HoldExpiredBeforeSettle_ChargesFull(t *testing.T) {
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
	const pre = 10.0
	before, after := 98.0, 98.0-credits // 过期已退回 10 → 先回到 98

	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery(insertBill.String()).WithArgs(txInsertArgs(credits, before, after, "completed")...).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))
	mock.ExpectCommit()

	closer := &fakeCloser{owned: []bool{false}} // 过期扫描先到：hold 已不在 held
	credit := &fakeCredit{before: before, after: after}
	svc := newTxTestService(db, credit, newFakeConfig(), time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t)))

	rec, err := svc.Record(context.Background(), holdReq(closer, pre, usage, rates))
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.Status != StatusCompleted {
		t.Fatalf("status=%s, want completed", rec.Status)
	}
	// 全额扣走的是 ConsumeTx（不是差额 SettleTx）：此时若仍走 SettleTx 就是又退了一遍预扣。
	if credit.txCalls != 1 || credit.txSettleCalls != 0 {
		t.Fatalf("预扣已退回时应整笔扣款（ConsumeTx 1 次、SettleTx 0 次），got tx=%d settle=%d",
			credit.txCalls, credit.txSettleCalls)
	}
	if credit.amount != credits {
		t.Fatalf("预扣已被退回，必须按全额扣 %v，实际扣除 %v", credits, credit.amount)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestRecord_HoldOwned_SettlesDeltaOnly 守住反向变异：hold 正常在 held 时仍按差额结算
// （改成「永远全额」会让正常链路多扣一笔预扣额）。
func TestRecord_HoldOwned_SettlesDeltaOnly(t *testing.T) {
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
	const pre = 10.0
	before, after := 88.0, 88.0-(credits-pre)

	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery(insertBill.String()).WithArgs(txInsertArgs(credits, before, after, "completed")...).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))
	mock.ExpectCommit()

	closer := &fakeCloser{owned: []bool{true}} // 结算先到：由本次终结
	credit := &fakeCredit{before: before, after: after}
	svc := newTxTestService(db, credit, newFakeConfig(), time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t)))

	if _, err := svc.Record(context.Background(), holdReq(closer, pre, usage, rates)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if want := credits - pre; credit.delta != want {
		t.Fatalf("hold 仍 held 时应按差额结算 %v，got %v", want, credit.delta)
	}
}

// TestRecord_HoldCloseRunsBeforeCharge 断言「先抢终结权、再动钱」的顺序。
//
// 顺序反了（先扣款后终结）就会出现 Owl 实测的窗口：扣款成功、终结失败/未做，
// 过期扫描随后把整笔预扣退回 → 白送一笔。这里用共享顺序切片把事实钉住。
func TestRecord_HoldCloseRunsBeforeCharge(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	usage := Usage{Input: 100, Output: 10}
	rates := fullRates()
	credits, _ := ComputeCredits(usage, rates, 1.0, 1.0, 10000)
	before, after := 50.0, 50.0-(credits-5)

	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery(insertBill.String()).WithArgs(txInsertArgs(credits, before, after, "completed")...).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))
	mock.ExpectCommit()

	var order []string
	closer := &fakeCloser{owned: []bool{true}, order: &order}
	credit := &orderedCredit{fakeCredit: &fakeCredit{before: before, after: after}, order: &order, label: "settle"}
	svc := newTxTestService(db, credit, newFakeConfig(), time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t)))

	if _, err := svc.Record(context.Background(), holdReq(closer, 5, usage, rates)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if len(order) < 2 || order[0] != "close-hold" || order[1] != "settle" {
		t.Fatalf("必须先终结预扣再动钱，实际顺序 %v", order)
	}
}

// TestRecord_HoldCloseError_RollsBackThenRetrySucceeds 覆盖「终结失败 → 整体回滚 → 重试成功」：
// 第一次终结报错时不得扣钱、不得落 completed 账单；重试（hold 仍在 held）应正常结算。
func TestRecord_HoldCloseError_RollsBackThenRetrySucceeds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	usage := Usage{Input: 200, Output: 20}
	rates := fullRates()
	credits, _ := ComputeCredits(usage, rates, 1.0, 1.0, 10000)
	const pre = 10.0
	before, after := 60.0, 60.0-(credits-pre)

	// 第一次：只到 Begin，终结失败 → 回滚（无扣款、无账单写入）。
	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectRollback()
	// 第二次（重试）：完整成功。
	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery(insertBill.String()).WithArgs(txInsertArgs(credits, before, after, "completed")...).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))
	mock.ExpectCommit()

	closer := &fakeCloser{
		owned: []bool{true, true},
		errs:  []error{errors.New("终结预扣登记失败：connection reset"), nil},
	}
	credit := &fakeCredit{before: before, after: after}
	svc := newTxTestService(db, credit, newFakeConfig(), time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t)))

	if _, err := svc.Record(context.Background(), holdReq(closer, pre, usage, rates)); err == nil {
		t.Fatal("终结失败应整体失败（不成交、不扣款）")
	}
	if credit.txSettleCalls != 0 {
		t.Fatalf("终结失败时不得动钱，got SettleTx=%d", credit.txSettleCalls)
	}

	rec, err := svc.Record(context.Background(), holdReq(closer, pre, usage, rates))
	if err != nil {
		t.Fatalf("重试应成功: %v", err)
	}
	if rec.Status != StatusCompleted || credit.txSettleCalls != 1 {
		t.Fatalf("重试后应正常结算一次，status=%s settle=%d", rec.Status, credit.txSettleCalls)
	}
	if want := credits - pre; credit.delta != want {
		t.Fatalf("重试成功时 hold 由本次终结，应按差额 %v 结算，got %v", want, credit.delta)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestRecord_HoldNotOwned_NeverRefunds 守死「owned=false 时绝不出现负差额（退款）」这条底线：
// 两个结算先后到同一条 hold，只有先到者 owned=true；后到者必须按全额扣，
// 任何一次出现 delta<0 都意味着又退了一遍预扣额。
func TestRecord_HoldNotOwned_NeverRefunds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	usage := Usage{Input: 100, Output: 0}
	rates := fullRates()
	credits, _ := ComputeCredits(usage, rates, 1.0, 1.0, 10000)
	pre := credits * 3 // 预扣比实际用量大 → 正常路径会退钱（delta<0）

	// 第一次 owned=true：应退差额（负 delta）。
	mock.ExpectQuery(selectByBillingID.String()).WithArgs("bill-a").WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery(insertBill.String()).WillReturnRows(
		sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))
	mock.ExpectCommit()
	// 第二次 owned=false（过期先到）：必须全额扣，不允许再退。
	mock.ExpectQuery(selectByBillingID.String()).WithArgs("bill-b").WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery(insertBill.String()).WillReturnRows(
		sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(2), time.Now()))
	mock.ExpectCommit()

	closer := &fakeCloser{owned: []bool{true, false}}
	credit := &fakeCredit{before: 100, after: 90}
	svc := newTxTestService(db, credit, newFakeConfig(), time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t)))

	reqA := holdReq(closer, pre, usage, rates)
	reqA.BillingID = "bill-a"
	if _, err := svc.Record(context.Background(), reqA); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	if credit.txSettleCalls != 1 || credit.delta >= 0 {
		t.Fatalf("owned=true 且预扣>实际时应走差额结算且退钱（delta<0），got settle=%d delta=%v",
			credit.txSettleCalls, credit.delta)
	}

	reqB := holdReq(closer, pre, usage, rates)
	reqB.BillingID = "bill-b"
	reqB.Hold.BillingID = "bill-b"
	if _, err := svc.Record(context.Background(), reqB); err != nil {
		t.Fatalf("第二次: %v", err)
	}
	if credit.txSettleCalls != 1 {
		t.Fatalf("owned=false 时不得再走差额结算（那会再退一次预扣），settle 调用次数=%d", credit.txSettleCalls)
	}
	if credit.txCalls != 1 || credit.amount != credits {
		t.Fatalf("owned=false 时应按全额 %v 扣（ConsumeTx 1 次），got tx=%d amount=%v",
			credits, credit.txCalls, credit.amount)
	}
}

// orderedCredit 在 fakeCredit 上追加顺序记录，用来断言「终结早于扣款」。
type orderedCredit struct {
	*fakeCredit
	order *[]string
	label string
}

func (o *orderedCredit) SettleTx(ctx context.Context, ex dbx.Execer, userID int64, delta float64, sessionID, remark string) (float64, float64, error) {
	if o.order != nil {
		*o.order = append(*o.order, "settle")
	}
	return o.fakeCredit.SettleTx(ctx, ex, userID, delta, sessionID, remark)
}
