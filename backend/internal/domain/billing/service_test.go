package billing

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/pkg/dbx"
)

// ---- fakes ----

type fakeCredit struct {
	before, after float64
	err           error
	amount        float64
	delta         float64
	calls         int
	settleCalls   int
	txCalls       int // ConsumeTx 调用次数（事务路径）
	txSettleCalls int // SettleTx 调用次数（事务路径）
}

func (f *fakeCredit) Consume(_ context.Context, _ int64, amount float64, _, _, _ string) (float64, float64, error) {
	f.calls++
	f.amount = amount
	return f.before, f.after, f.err
}

func (f *fakeCredit) Settle(_ context.Context, _ int64, delta float64, _, _ string) (float64, float64, error) {
	f.settleCalls++
	f.delta = delta
	return f.before, f.after, f.err
}

func (f *fakeCredit) ConsumeTx(_ context.Context, _ dbx.Execer, _ int64, amount float64, _, _, _ string) (float64, float64, error) {
	f.txCalls++
	f.amount = amount
	return f.before, f.after, f.err
}

func (f *fakeCredit) SettleTx(_ context.Context, _ dbx.Execer, _ int64, delta float64, _, _ string) (float64, float64, error) {
	f.txSettleCalls++
	f.delta = delta
	return f.before, f.after, f.err
}

func newFakeConfig() *fakeConfig {
	eightK, thirty2K, one28K := int64(8000), int64(32000), int64(128000)
	return &fakeConfig{
		r: 10000,
		tiers: []TierRule{
			{Min: 0, Max: &eightK, Coeff: 1.0},
			{Min: eightK, Max: &thirty2K, Coeff: 1.2},
			{Min: thirty2K, Max: &one28K, Coeff: 1.5},
			{Min: one28K, Max: nil, Coeff: 2.0},
		},
		tc: TimeCoeffConfig{
			Timezone: "Asia/Shanghai", Default: 1.0,
			Periodic: []Segment{{Name: "全天", Start: "00:00", End: "24:00", Coeff: 1.0}},
		},
	}
}

type fakeConfig struct {
	r       int64
	cnyRate float64
	tiers   []TierRule
	tc      TimeCoeffConfig
}

func (f *fakeConfig) R() (int64, error)                    { return f.r, nil }
func (f *fakeConfig) CnyRate() (float64, error)            { return f.cnyRate, nil }
func (f *fakeConfig) ContextTiers() ([]TierRule, error)    { return f.tiers, nil }
func (f *fakeConfig) TimeConfig() (TimeCoeffConfig, error) { return f.tc, nil }

// ---- helpers ----

var selectByBillingID = regexp.MustCompile(`SELECT .* FROM billing_records WHERE billing_id=\$1`)
var insertBill = regexp.MustCompile(`INSERT INTO billing_records.*`)

const (
	billingID  = "bill-20260102-000001"
	testUserID = int64(7)
)

// recordColsForTest 与 recordCols 一致，仅供构造 mock 行。
var recordColsForTest = []string{
	"id", "billing_id", "user_id", "pricing_mode", "token_id", "external_model_name",
	"internal_model_id", "channel_key_id", "session_id", "session_name", "call_time", "tokens", "rates",
	"coefficients", "r_value", "raw_total", "credits_consumed", "cost_credits",
	"balance_before", "balance_after", "status", "error_message", "duration_ms",
	"first_token_ms",
}

func completedRow(t *testing.T) *sqlmock.Rows {
	t.Helper()
	return sqlmock.NewRows(recordColsForTest).AddRow(
		int64(5), billingID, int64(7), "sale", nil, "ext-model", "int-model", int64(1), nil, "",
		time.Now(), `{}`, `{}`, []byte(`{"time":1,"context":1}`), int64(10000),
		nil, nil, nil, nil, nil, "completed", nil, nil, nil,
	)
}

func newTestService(db *sql.DB, credit *fakeCredit, cfg ConfigProvider, now time.Time) *Service {
	return &Service{
		store:  NewSqlStore(db),
		cfg:    cfg,
		credit: credit,
		clock:  func() time.Time { return now },
	}
}

// newTxTestService 装配事务路径的 Service（txdb 注入 *sql.DB）。
func newTxTestService(db *sql.DB, credit *fakeCredit, cfg ConfigProvider, now time.Time) *Service {
	svc := newTestService(db, credit, cfg, now)
	svc.SetTxBeginner(db)
	return svc
}

// ---- tests ----

func TestRecord_Idempotent_AlreadyCompleted(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).
		WillReturnRows(completedRow(t))

	credit := &fakeCredit{before: 100, after: 90}
	svc := newTestService(db, credit, newFakeConfig(), time.Now())

	rec, err := svc.Record(context.Background(), RecordReq{BillingID: billingID, UserID: testUserID,
		ExternalModel: "ext-model", InternalModelID: "int-model", ChannelKeyID: 1, SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.Status != StatusCompleted || rec.BillingID != billingID {
		t.Fatalf("应返回已存在 completed 记录, got %+v", rec)
	}
	if credit.calls != 0 {
		t.Fatalf("已 completed 不应再次 Consume, calls=%d", credit.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestRecord_Success(t *testing.T) {
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
	before, after := 10.0, 9.7632 // after = Round5(10 - 0.2368)

	// 无历史记录 → 未命中。
	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).
		WillReturnError(sql.ErrNoRows)

	// Insert completed（断言关键字段）。
	mock.ExpectQuery(insertBill.String()).WithArgs(
		sqlmock.AnyArg(), // id
		billingID, testUserID, "sale", sqlmock.AnyArg(), "ext-model", "int-model", int64(1),
		sqlmock.AnyArg(),                 // session_id
		sqlmock.AnyArg(),                 // session_name
		sqlmock.AnyArg(),                 // call_time
		sqlmock.AnyArg(),                 // tokens
		sqlmock.AnyArg(),                 // rates
		[]byte(`{"time":1,"context":1}`), // coefficients
		int64(10000),                     // r_value
		sqlmock.AnyArg(),                 // raw_total
		credits,                          // credits_consumed
		sqlmock.AnyArg(),                 // cost_credits
		before,                           // balance_before
		after,                            // balance_after
		"completed",                      // status
		sqlmock.AnyArg(),                 // error_message
		sqlmock.AnyArg(),                 // duration_ms
		sqlmock.AnyArg(),                 // first_token_ms
	).WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))

	credit := &fakeCredit{before: before, after: after}
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t))
	svc := newTestService(db, credit, newFakeConfig(), now)

	rec, err := svc.Record(context.Background(), RecordReq{
		BillingID: billingID, UserID: testUserID, PricingMode: "sale",
		ExternalModel: "ext-model", InternalModelID: "int-model", ChannelKeyID: 1, SessionID: "sess-1",
		CallTime: now, Tokens: usage, Rates: rates,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.Status != StatusCompleted {
		t.Fatalf("status=%s, want completed", rec.Status)
	}
	if rec.CreditsConsumed != credits {
		t.Fatalf("credits=%v, want %v", rec.CreditsConsumed, credits)
	}
	if rec.BalanceBefore != before || rec.BalanceAfter != after {
		t.Fatalf("balance before/after = %v/%v, want %v/%v", rec.BalanceBefore, rec.BalanceAfter, before, after)
	}
	if rec.RValue != 10000 {
		t.Fatalf("r_value=%d, want 10000", rec.RValue)
	}
	if rec.Coefficients.Time != 1.0 || rec.Coefficients.Context != 1.0 {
		t.Fatalf("coeff = %+v, want time=1 context=1", rec.Coefficients)
	}
	if credit.calls != 1 {
		t.Fatalf("Consume 应调用 1 次, calls=%d", credit.calls)
	}
	if credit.amount != credits {
		t.Fatalf("Consume amount=%v, want credits=%v", credit.amount, credits)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestRecord_ConsumeError_RecordsFailed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).
		WillReturnError(sql.ErrNoRows)

	consumeErr := errors.New("40201: 积分余额不足，本次消耗需 0.2368 积分")

	// 失败 → Insert failed；内部错误细节只进日志，落库统一为中文业务文案。
	mock.ExpectQuery(insertBill.String()).WithArgs(
		sqlmock.AnyArg(), // id
		billingID, testUserID, "sale", sqlmock.AnyArg(), "ext-model", "int-model", int64(1),
		sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), []byte(`{"time":1,"context":1}`),
		int64(10000), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
		sqlmock.AnyArg(), sqlmock.AnyArg(),
		"failed", "计费结算失败",
		sqlmock.AnyArg(), sqlmock.AnyArg(),
	).WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(2), time.Now()))

	credit := &fakeCredit{err: consumeErr}
	svc := newTestService(db, credit, newFakeConfig(), time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t)))

	rec, err := svc.Record(context.Background(), RecordReq{
		BillingID: billingID, UserID: testUserID, PricingMode: "sale",
		ExternalModel: "ext-model", InternalModelID: "int-model", ChannelKeyID: 1, SessionID: "sess-1",
		Tokens: Usage{Input: 1234, Output: 567}, Rates: fullRates(),
	})
	if err == nil {
		t.Fatal("期望消费失败透传错误, got nil")
	}
	if !errors.Is(err, consumeErr) || err.Error() != consumeErr.Error() {
		t.Fatalf("应透传原始错误, got %v", err)
	}
	if rec != nil {
		t.Fatalf("失败路径应返回 nil 记录, got %+v", rec)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// bizTestErr 模拟实现了 UserMessage 的业务错误（如 identity.APIError）。
type bizTestErr struct{ msg string }

func (e bizTestErr) Error() string       { return e.msg }
func (e bizTestErr) UserMessage() string { return e.msg }

// TestFailureMessage 校验失败原因归一：业务错误用其中文文案，内部错误归为通用中文提示。
func TestFailureMessage(t *testing.T) {
	if got := failureMessage(bizTestErr{"积分余额不足，本次消耗需 1 积分"}); got != "积分余额不足，本次消耗需 1 积分" {
		t.Fatalf("业务错误应保留中文文案, got %q", got)
	}
	if got := failureMessage(errors.New("dial tcp 10.0.0.1:443: connect: connection refused")); got != "计费结算失败" {
		t.Fatalf("内部错误应归一为通用中文提示, got %q", got)
	}
}

func TestRecord_FailedHistorical_NoDoubleConsume(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// 历史 failed 记录。
	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).
		WillReturnRows(sqlmock.NewRows(recordColsForTest).AddRow(
			int64(9), billingID, testUserID, "sale", nil, "ext-model", "int-model", int64(1), nil, "",
			time.Now(), `{}`, `{}`, []byte(`{"time":1,"context":1}`), int64(10000),
			nil, nil, nil, nil, nil, "failed", "40201: 余额不足", nil, nil,
		))

	credit := &fakeCredit{}
	svc := newTestService(db, credit, newFakeConfig(), time.Now())

	rec, err := svc.Record(context.Background(), RecordReq{BillingID: billingID, UserID: testUserID,
		ChannelKeyID: 1})
	if err == nil || err.Error() != "40201: 余额不足" {
		t.Fatalf("应透传历史失败原因, got rec=%+v err=%v", rec, err)
	}
	if credit.calls != 0 {
		t.Fatalf("历史 failed 不应再 Consume, calls=%d", credit.calls)
	}
}

func TestRecord_CostMode_RecordsCostCredits(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	usage := Usage{Input: 1000}
	saleRates := fullRates()
	costRates := Rates{"input": 0.8, "output": 0.16, "cache_read": 0.05, "cache_write": 0.2, "reasoning": 0.8}
	credits, err := ComputeCredits(usage, saleRates, 1.0, 1.0, 10000)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	costCredits, err := ComputeCredits(usage, costRates, 1.0, 1.0, 10000)
	if err != nil {
		t.Fatalf("compute cost: %v", err)
	}
	if costCredits != 0.08 {
		t.Fatalf("cost=%.5f, want 0.08", costCredits) // 1000×0.8/10000=0.08
	}

	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).
		WillReturnError(sql.ErrNoRows)

	// cost 模式：cost_credits 记录成本积分。
	mock.ExpectQuery(insertBill.String()).WithArgs(
		sqlmock.AnyArg(), // id
		billingID, testUserID, "cost", sqlmock.AnyArg(), "ext-model", "int-model", int64(1),
		sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), []byte(`{"time":1,"context":1}`),
		int64(10000), sqlmock.AnyArg(), credits, costCredits,
		sqlmock.AnyArg(), sqlmock.AnyArg(), "completed", sqlmock.AnyArg(),
		sqlmock.AnyArg(), sqlmock.AnyArg(),
	).WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(3), time.Now()))

	credit := &fakeCredit{before: 10, after: 10 - credits}
	svc := newTestService(db, credit, newFakeConfig(), time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t)))

	rec, err := svc.Record(context.Background(), RecordReq{
		BillingID: billingID, UserID: testUserID, PricingMode: "cost",
		ExternalModel: "ext-model", InternalModelID: "int-model", ChannelKeyID: 1,
		Tokens: usage, Rates: saleRates, CostRates: costRates,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.CostCredits != costCredits {
		t.Fatalf("cost_credits=%v, want %v", rec.CostCredits, costCredits)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestRecord_SaleMode_CostCreditsDistinctFromCredits(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	usage := Usage{Input: 1000}
	saleRates := fullRates()
	costRates := Rates{"input": 0.8, "output": 0.16, "cache_read": 0.05, "cache_write": 0.2, "reasoning": 0.8}
	credits, err := ComputeCredits(usage, saleRates, 1.0, 1.0, 10000)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	costCredits, err := ComputeCredits(usage, costRates, 1.0, 1.0, 10000)
	if err != nil {
		t.Fatalf("compute cost: %v", err)
	}
	if credits == costCredits {
		t.Fatalf("测试前提不成立：售价/成本积分应不同 (credits=%v cost=%v)", credits, costCredits)
	}

	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).
		WillReturnError(sql.ErrNoRows)

	// sale 模式：credits_consumed 按售价扣，cost_credits 记录成本（与售价不同）。
	mock.ExpectQuery(insertBill.String()).WithArgs(
		sqlmock.AnyArg(), // id
		billingID, testUserID, "sale", sqlmock.AnyArg(), "ext-model", "int-model", int64(1),
		sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), []byte(`{"time":1,"context":1}`),
		int64(10000), sqlmock.AnyArg(), credits, costCredits,
		sqlmock.AnyArg(), sqlmock.AnyArg(), "completed", sqlmock.AnyArg(),
		sqlmock.AnyArg(), sqlmock.AnyArg(),
	).WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(4), time.Now()))

	credit := &fakeCredit{before: 10, after: 10 - credits}
	svc := newTestService(db, credit, newFakeConfig(), time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t)))

	rec, err := svc.Record(context.Background(), RecordReq{
		BillingID: billingID, UserID: testUserID, PricingMode: "sale",
		ExternalModel: "ext-model", InternalModelID: "int-model", ChannelKeyID: 1,
		Tokens: usage, Rates: saleRates, CostRates: costRates,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	// 核心断言：sale 模式下 cost_credits 反映真实成本，区别于售价积分 credits_consumed。
	if rec.CreditsConsumed != credits {
		t.Fatalf("credits_consumed=%v, want sale credits %v", rec.CreditsConsumed, credits)
	}
	if rec.CostCredits != costCredits {
		t.Fatalf("cost_credits=%v, want cost %v", rec.CostCredits, costCredits)
	}
	if rec.CostCredits == rec.CreditsConsumed {
		t.Fatalf("sale 模式 cost_credits 应与 credits 不同，got both=%v", rec.CreditsConsumed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestRecord_InsertUniqueViolation_ReturnsCompleted(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	usage := Usage{Input: 1234, Output: 567}
	rates := fullRates()

	// 快速路径未命中历史记录。
	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).
		WillReturnError(sql.ErrNoRows)

	// Consume 成功后 Insert 撞唯一索引（并发竞态，另一请求已写库成功）。
	mock.ExpectQuery(insertBill.String()).
		WillReturnError(errors.New(`ERROR: duplicate key value violates unique constraint "billing_records_billing_id_key" (SQLSTATE 23505)`))

	// 重查该 billing_id 的 completed 记录并返回成功。
	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).
		WillReturnRows(completedRow(t))

	credit := &fakeCredit{before: 100, after: 90}
	svc := newTestService(db, credit, newFakeConfig(), time.Now())

	rec, err := svc.Record(context.Background(), RecordReq{
		BillingID: billingID, UserID: testUserID, PricingMode: "sale",
		ExternalModel: "ext-model", InternalModelID: "int-model", ChannelKeyID: 1, SessionID: "sess-u",
		Tokens: usage, Rates: rates,
	})
	if err != nil {
		t.Fatalf("唯一索引冲突应重查并返回成功, got err=%v", err)
	}
	if rec == nil || rec.Status != StatusCompleted || rec.BillingID != billingID {
		t.Fatalf("应返回已落库的 completed 记录, got %+v", rec)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// 落库失败（DB 抖动）时账单进入本地重试队列，不丢；DB 恢复后可重投落库。
func TestRecord_InsertFailure_QueuesRetry(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(insertBill.String()).WillReturnError(errors.New("db down"))

	q := NewRetryQueue(filepath.Join(t.TempDir(), "retry.jsonl"), nil)
	credit := &fakeCredit{before: 10, after: 9.7632}
	svc := newTestService(db, credit, newFakeConfig(), time.Now())
	svc.SetRetryQueue(q)

	_, err = svc.Record(context.Background(), RecordReq{
		BillingID: billingID, UserID: testUserID, PricingMode: "sale",
		ExternalModel: "ext-model", InternalModelID: "int-model", ChannelKeyID: 1,
		Tokens: Usage{Input: 1234, Output: 567}, Rates: fullRates(),
	})
	if err == nil {
		t.Fatal("落库失败必须向调用方返回错误（网关据此判定计费失败）")
	}
	if n := q.Pending(); n != 1 {
		t.Fatalf("落库失败的账单应进入重试队列, pending=%d", n)
	}

	// 重投：记录字段完整，可正常落库。
	store := &fakeRetryStore{}
	ok, pending, ferr := q.Flush(context.Background(), store)
	if ferr != nil || ok != 1 || pending != 0 {
		t.Fatalf("flush: ok=%d pending=%d err=%v", ok, pending, ferr)
	}
	if len(store.inserted) != 1 || store.inserted[0] != billingID {
		t.Fatalf("重投记录有误: %+v", store.inserted)
	}
}

// JSONB 列为 NOT NULL：序列化失败（如 NaN 费率）必须返回错误，而不是静默写入 SQL NULL。
func TestSqlStore_Insert_MarshalError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := NewSqlStore(db)
	_, err = store.Insert(context.Background(), &Record{
		BillingID: "bill-nan", UserID: testUserID, Status: StatusCompleted,
		Rates: Rates{"input": math.NaN()},
	})
	if err == nil {
		t.Fatal("NaN 费率应返回序列化错误")
	}
	if merr := mock.ExpectationsWereMet(); merr != nil {
		t.Fatalf("序列化失败时不应发起 DB 调用: %v", merr)
	}
}

// ---- 事务路径：扣款 + 积分流水 + 账单同事务 ----

// txInsertArgs 组装 insertBill 的完整 24 参数匹配（事务路径用例复用；首参为应用层雪花 ID）。
func txInsertArgs(credits, before, after float64, status string) []driver.Value {
	return []driver.Value{
		sqlmock.AnyArg(),                 // id（应用层雪花 ID）
		billingID, testUserID, "sale", sqlmock.AnyArg(), "ext-model", "int-model", int64(1),
		sqlmock.AnyArg(),                 // session_id
		sqlmock.AnyArg(),                 // session_name
		sqlmock.AnyArg(),                 // call_time
		sqlmock.AnyArg(),                 // tokens
		sqlmock.AnyArg(),                 // rates
		[]byte(`{"time":1,"context":1}`), // coefficients
		int64(10000),                     // r_value
		sqlmock.AnyArg(),                 // raw_total
		credits,                          // credits_consumed
		sqlmock.AnyArg(),                 // cost_credits
		before,                           // balance_before
		after,                            // balance_after
		status,                           // status
		sqlmock.AnyArg(),                 // error_message
		sqlmock.AnyArg(),                 // duration_ms
		sqlmock.AnyArg(),                 // first_token_ms
	}
}

// 事务路径成功：Begin → 扣款(Tx) → 账单 Insert(Tx) → Commit。
func TestRecord_TxPath_Success(t *testing.T) {
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
	mock.ExpectBegin()
	mock.ExpectQuery(insertBill.String()).WithArgs(txInsertArgs(credits, before, after, "completed")...).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))
	mock.ExpectCommit()

	credit := &fakeCredit{before: before, after: after}
	svc := newTxTestService(db, credit, newFakeConfig(), time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t)))

	rec, err := svc.Record(context.Background(), RecordReq{
		BillingID: billingID, UserID: testUserID, PricingMode: "sale",
		ExternalModel: "ext-model", InternalModelID: "int-model", ChannelKeyID: 1, SessionID: "sess-1",
		Tokens: usage, Rates: rates,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.Status != StatusCompleted {
		t.Fatalf("status=%s, want completed", rec.Status)
	}
	if credit.txCalls != 1 || credit.calls != 0 {
		t.Fatalf("事务路径应走 ConsumeTx 一次且不走非事务 Consume, tx=%d plain=%d", credit.txCalls, credit.calls)
	}
	if credit.amount != credits {
		t.Fatalf("ConsumeTx amount=%v, want %v", credit.amount, credits)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// 事务路径扣款失败：Begin → 扣款(Tx) 失败 → Rollback → 事务外落 failed 账单，透传原始错误。
func TestRecord_TxPath_ConsumeError_RollsBackAndRecordsFailed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	consumeErr := errors.New("40201: 积分余额不足，本次消耗需 0.2368 积分")

	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectRollback() // 扣款失败先回滚
	mock.ExpectQuery(insertBill.String()).WithArgs(
		txInsertArgs(0, 0, 0, "failed")...,
	).WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(2), time.Now()))

	credit := &fakeCredit{err: consumeErr}
	svc := newTxTestService(db, credit, newFakeConfig(), time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t)))

	rec, err := svc.Record(context.Background(), RecordReq{
		BillingID: billingID, UserID: testUserID, PricingMode: "sale",
		ExternalModel: "ext-model", InternalModelID: "int-model", ChannelKeyID: 1, SessionID: "sess-1",
		Tokens: Usage{Input: 1234, Output: 567}, Rates: fullRates(),
	})
	if err == nil || err.Error() != consumeErr.Error() {
		t.Fatalf("应透传原始扣款错误, got rec=%+v err=%v", rec, err)
	}
	if rec != nil {
		t.Fatalf("失败路径应返回 nil 记录, got %+v", rec)
	}
	if credit.txCalls != 1 || credit.calls != 0 {
		t.Fatalf("不应回退到非事务路径, tx=%d plain=%d", credit.txCalls, credit.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// 事务路径账单写入失败：Rollback（扣款一并回滚，余额不变）后回退非事务路径，
// 由重试队列兜底；本次只应扣款一次（事务内的那次已回滚）。
func TestRecord_TxPath_InsertFailure_FallsBackToPlain(t *testing.T) {
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
	// 事务内账单写入失败 → 回滚。
	mock.ExpectBegin()
	mock.ExpectQuery(insertBill.String()).WillReturnError(errors.New("tx insert boom"))
	mock.ExpectRollback()
	// 回退非事务路径：扣款 + 账单 Insert 成功。
	mock.ExpectQuery(insertBill.String()).WithArgs(txInsertArgs(credits, before, after, "completed")...).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(3), time.Now()))

	credit := &fakeCredit{before: before, after: after}
	svc := newTxTestService(db, credit, newFakeConfig(), time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t)))

	rec, err := svc.Record(context.Background(), RecordReq{
		BillingID: billingID, UserID: testUserID, PricingMode: "sale",
		ExternalModel: "ext-model", InternalModelID: "int-model", ChannelKeyID: 1, SessionID: "sess-1",
		Tokens: usage, Rates: rates,
	})
	if err != nil {
		t.Fatalf("回退非事务路径后应成功, got %v", err)
	}
	if rec.Status != StatusCompleted {
		t.Fatalf("status=%s, want completed", rec.Status)
	}
	if credit.txCalls != 1 || credit.calls != 1 {
		t.Fatalf("应事务一次 + 回退一次, tx=%d plain=%d", credit.txCalls, credit.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// 事务路径撞唯一索引：Rollback（本次扣款不落库）后重查 completed 返回，不重复扣款。
func TestRecord_TxPath_UniqueViolation_ReturnsCompleted(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery(insertBill.String()).
		WillReturnError(errors.New(`ERROR: duplicate key value violates unique constraint "billing_records_billing_id_key" (SQLSTATE 23505)`))
	mock.ExpectRollback()
	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).WillReturnRows(completedRow(t))

	credit := &fakeCredit{before: 100, after: 90}
	svc := newTxTestService(db, credit, newFakeConfig(), time.Now())

	rec, err := svc.Record(context.Background(), RecordReq{
		BillingID: billingID, UserID: testUserID, PricingMode: "sale",
		ExternalModel: "ext-model", InternalModelID: "int-model", ChannelKeyID: 1, SessionID: "sess-u",
		Tokens: Usage{Input: 1234, Output: 567}, Rates: fullRates(),
	})
	if err != nil {
		t.Fatalf("唯一索引冲突应重查并返回成功, got err=%v", err)
	}
	if rec == nil || rec.Status != StatusCompleted || rec.BillingID != billingID {
		t.Fatalf("应返回已落库的 completed 记录, got %+v", rec)
	}
	if credit.txCalls != 1 || credit.calls != 0 {
		t.Fatalf("撞库不应重复扣款, tx=%d plain=%d", credit.txCalls, credit.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// staticSource 是 configSource 的最小 fake，按 key 返回预设的 sys_configs 值。
type staticSource map[string][]byte

func (s staticSource) GetConfig(_ context.Context, key string) ([]byte, error) {
	v, ok := s[key]
	if !ok {
		return nil, errors.New("系统配置不存在")
	}
	return v, nil
}

// TestConfigProvider_CnyRate 验证 billing.cny_rate 的解析：兼容数字与字符串两种 JSON 存储。
func TestConfigProvider_CnyRate(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want float64
	}{
		{name: "numeric", raw: []byte(`6.8`), want: 6.8},
		{name: "string", raw: []byte(`"7.35"`), want: 7.35},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &defaultConfigProvider{src: staticSource{cfgCnyRate: c.raw}}
			got, err := p.CnyRate()
			if err != nil {
				t.Fatalf("CnyRate err=%v, want %v", err, c.want)
			}
			if got != c.want {
				t.Fatalf("CnyRate=%v, want %v", got, c.want)
			}
		})
	}
}

func shLocForTest(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return loc
}

// TestRecord_ModelLevelOverride 验证模型级 time_config/context_tiers 优先于全局 sys_configs。
func TestRecord_ModelLevelOverride(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	var noLimit *int64 = nil
	usage := Usage{Input: 1000}
	rates := fullRates()
	// 模型级覆盖：ctx 系数 2.0、time 系数 1.5（全局为 1.0）。sum=1000 ⇒ 1000*1.5*2/10000=0.3。
	modelTime := TimeCoeffConfig{
		Timezone: "Asia/Shanghai", Default: 1.5,
		Periodic: []Segment{{Name: "全天", Start: "00:00", End: "24:00", Coeff: 1.5}},
	}
	modelTiers := []TierRule{{Min: 0, Max: noLimit, Coeff: 2.0}}
	credits, err := ComputeCredits(usage, rates, 1.5, 2.0, 10000)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if credits != 0.3 {
		t.Fatalf("credits want 0.3, got %v", credits)
	}

	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(insertBill.String()).WithArgs(
		sqlmock.AnyArg(), // id
		billingID, testUserID, "sale", sqlmock.AnyArg(), "ext-model", "int-model", int64(1),
		sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), []byte(`{"time":1.5,"context":2}`),
		int64(10000), sqlmock.AnyArg(), credits, sqlmock.AnyArg(),
		sqlmock.AnyArg(), sqlmock.AnyArg(), "completed", sqlmock.AnyArg(),
		sqlmock.AnyArg(), sqlmock.AnyArg(),
	).WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(7), time.Now()))

	credit := &fakeCredit{before: 10, after: 9.7}
	svc := newTestService(db, credit, newFakeConfig(), time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t)))

	rec, err := svc.Record(context.Background(), RecordReq{
		BillingID: billingID, UserID: testUserID, PricingMode: "sale",
		ExternalModel: "ext-model", InternalModelID: "int-model", ChannelKeyID: 1,
		Tokens: usage, Rates: rates,
		TimeConfig:   &modelTime,
		ContextTiers: modelTiers,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.Coefficients.Time != 1.5 || rec.Coefficients.Context != 2.0 {
		t.Fatalf("模型级系数未生效: time=%v context=%v", rec.Coefficients.Time, rec.Coefficients.Context)
	}
	if rec.CreditsConsumed != credits {
		t.Fatalf("credits=%v, want %v", rec.CreditsConsumed, credits)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestRecord_Dimension 断言落库账单携带渠道密钥与会话维度（channel_key_id/session_id）。
func TestRecord_Dimension(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	usage := Usage{Input: 100}
	rates := fullRates()
	credits, err := ComputeCredits(usage, rates, 1.0, 1.0, 10000)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	mock.ExpectQuery(selectByBillingID.String()).WithArgs(billingID).
		WillReturnError(sql.ErrNoRows)

	// 关键断言：第 8 参数为 channel_key_id=42，第 9 参数为 session_id="sess-dim"。
	mock.ExpectQuery(insertBill.String()).WithArgs(
		sqlmock.AnyArg(), // id
		billingID, testUserID, "sale", sqlmock.AnyArg(), "ext-model", "int-model", int64(42),
		"sess-dim",
		sqlmock.AnyArg(),                 // session_name
		sqlmock.AnyArg(),                 // call_time
		sqlmock.AnyArg(),                 // tokens
		sqlmock.AnyArg(),                 // rates
		[]byte(`{"time":1,"context":1}`), // coefficients
		int64(10000),                     // r_value
		sqlmock.AnyArg(),                 // raw_total
		credits,                          // credits_consumed
		sqlmock.AnyArg(),                 // cost_credits
		sqlmock.AnyArg(),                 // balance_before
		sqlmock.AnyArg(),                 // balance_after
		"completed",                      // status
		sqlmock.AnyArg(),                 // error_message
		sqlmock.AnyArg(),                 // duration_ms
		sqlmock.AnyArg(),                 // first_token_ms
	).WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(8), time.Now()))

	credit := &fakeCredit{before: 10, after: 10 - credits}
	svc := newTestService(db, credit, newFakeConfig(), time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t)))

	rec, err := svc.Record(context.Background(), RecordReq{
		BillingID: billingID, UserID: testUserID, PricingMode: "sale",
		ExternalModel: "ext-model", InternalModelID: "int-model",
		ChannelKeyID: 42, SessionID: "sess-dim",
		CallTime: time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t)),
		Tokens:   usage, Rates: rates,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.ChannelKeyID != 42 || rec.SessionID != "sess-dim" {
		t.Fatalf("维度未落库: channel_key_id=%d session_id=%q", rec.ChannelKeyID, rec.SessionID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestService_EstimateCredits_MatchesRecordFormula 预扣估算与结算 Record 同一口径：
// 输入 token 代入本地公式（时段×分档×倍率÷R）；输出不参与预扣；不落库、不扣费。
func TestService_EstimateCredits_MatchesRecordFormula(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 1, 2, 12, 0, 0, 0, shLocForTest(t))
	svc := newTestService(db, &fakeCredit{before: 10, after: 10}, newFakeConfig(), now)

	inputOnly := Usage{Input: 1234, Output: 0}
	est, err := svc.EstimateCredits(context.Background(), RecordReq{
		ExternalModel: "ext-model",
		Tokens:        inputOnly,
		Rates:         fullRates(),
		CallTime:      now,
	})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	want, err := ComputeCredits(inputOnly, fullRates(), 1.0, 1.0, 10000)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if math.Abs(est-want) > 0.00001 {
		t.Fatalf("estimate=%v, want %v", est, want)
	}
	if est <= 0 {
		t.Fatalf("estimate must be positive, got %v", est)
	}
}
