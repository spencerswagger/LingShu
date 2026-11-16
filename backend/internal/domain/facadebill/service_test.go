package facadebill

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/magpie"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// fakeLedger 记录调用参数（断言「有没有出估算账」全靠它）。
type fakeLedger struct {
	estCredits float64
	estErr     error
	recordErr  error

	estimateCalls int
	records       []billing.RecordReq
}

func (f *fakeLedger) EstimateCredits(_ context.Context, req billing.RecordReq) (float64, error) {
	f.estimateCalls++
	if f.estErr != nil {
		return 0, f.estErr
	}
	return f.estCredits, nil
}

func (f *fakeLedger) Record(_ context.Context, req billing.RecordReq) (*billing.Record, error) {
	f.records = append(f.records, req)
	if f.recordErr != nil {
		return nil, f.recordErr
	}
	return &billing.Record{BillingID: req.BillingID, CreditsConsumed: 0}, nil
}

// fakeWallet 记录预扣 / 退回 / 终结。
type fakeWallet struct {
	preErr    error
	settleErr error

	holds   []identity.HoldRequest
	settles []float64
	closes  []string
}

func (f *fakeWallet) PreConsumeHold(_ context.Context, req identity.HoldRequest) (identity.Hold, error) {
	f.holds = append(f.holds, req)
	if f.preErr != nil {
		return identity.Hold{}, f.preErr
	}
	return identity.Hold{BillingID: req.BillingID, Amount: req.Amount, Status: identity.HoldHeld}, nil
}

func (f *fakeWallet) CloseHold(_ context.Context, billingID, status, _ string) (bool, error) {
	f.closes = append(f.closes, status)
	return true, nil
}

func (f *fakeWallet) Settle(_ context.Context, _ int64, delta float64, _, _ string) (float64, float64, error) {
	f.settles = append(f.settles, delta)
	if f.settleErr != nil {
		return 0, 0, f.settleErr
	}
	return 0, 0, nil
}

// fakePricer 返回固定售价；err 非空时模拟「模型没配价」。
type fakePricer struct {
	rates billing.Rates
	err   error
}

func (f fakePricer) SaleRatesFor(context.Context, string) (billing.Rates, *billing.TimeCoeffConfig, []billing.TierRule, error) {
	if f.err != nil {
		return nil, nil, nil, f.err
	}
	return f.rates, nil, nil, nil
}

func newTestService(t *testing.T, ledger *fakeLedger, wallet *fakeWallet) *Service {
	t.Helper()
	s := New(Options{
		Ledger: ledger,
		Wallet: wallet,
		Pricer: fakePricer{rates: billing.Rates{"input": 1, "output": 2, "cache_read": 0.1}},
		Now:    func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})
	if s == nil {
		t.Fatalf("服务装配失败（依赖齐全时应返回非 nil）")
	}
	s.newID = func() string { return "bill-test-1" }
	return s
}

func reserveInput() magpie.ReserveInput {
	return magpie.ReserveInput{
		UserID: 7, TokenID: 11, ExternalModel: "tenant-gpt",
		Protocol: "openai-chat", Endpoint: "chat.completions",
		SessionID: "sess-1", RequestBody: []byte(`{"model":"tenant-gpt","messages":[{"role":"user","content":"hi"}]}`),
	}
}

// TestReserve_FreezesEstimatedAmount 预扣按估算金额冻结（估算只用于预扣，不是出账）。
func TestReserve_FreezesEstimatedAmount(t *testing.T) {
	ledger := &fakeLedger{estCredits: 3.5}
	wallet := &fakeWallet{}
	s := newTestService(t, ledger, wallet)

	handle, deny := s.Reserve(context.Background(), reserveInput())
	if deny != nil {
		t.Fatalf("预扣不应被拒绝: %+v", deny)
	}
	if handle == nil {
		t.Fatalf("预扣应返回句柄")
	}
	if len(wallet.holds) != 1 || wallet.holds[0].Amount != 3.5 {
		t.Fatalf("应冻结 3.5，实际 %+v", wallet.holds)
	}
	if wallet.holds[0].BillingID != "bill-test-1" {
		t.Fatalf("预扣登记的 billing_id 应与会话句柄一致，实际 %q", wallet.holds[0].BillingID)
	}
}

// TestReserve_InsufficientBalance_Denies 余额不足 → 402，且不透传上游。
func TestReserve_InsufficientBalance_Denies(t *testing.T) {
	ledger := &fakeLedger{estCredits: 10}
	wallet := &fakeWallet{preErr: &identity.APIError{HTTPStatus: 402, Code: resp.CodeInsufficient, Message: "积分余额不足"}}
	s := newTestService(t, ledger, wallet)

	if _, deny := s.Reserve(context.Background(), reserveInput()); deny == nil {
		t.Fatalf("余额不足必须拒绝")
	} else if deny.Status != 402 || deny.Code != magpie.CodeInsufficientQuota {
		t.Fatalf("拒绝口径应为 402/insufficient_quota，实际 %d/%s", deny.Status, deny.Code)
	}
}

// TestReserve_PriceMissing_Denies 模型没配价 → 拒绝（不放过去白送一次）。
func TestReserve_PriceMissing_Denies(t *testing.T) {
	ledger := &fakeLedger{estCredits: 1}
	s := New(Options{Ledger: ledger, Wallet: &fakeWallet{}, Pricer: fakePricer{err: errors.New("boom")}})
	if s == nil {
		t.Fatalf("装配应成功（pricer 有 err 但非 nil）")
	}
	if _, deny := s.Reserve(context.Background(), reserveInput()); deny == nil || deny.Status != 402 {
		t.Fatalf("取不到售价必须 402 拒绝，实际 %+v", deny)
	}
}

// TestOnComplete_NoUsage_RefundsEverythingAndNeverEstimates 是**口径①**的守门用例：
// 无 usage → 整笔退预扣 + failed 账单（credits 恒 0）+ 预扣登记终结为 refunded；
// **绝不**把预扣额或估算额当出账金额。
func TestOnComplete_NoUsage_RefundsEverythingAndNeverEstimates(t *testing.T) {
	ledger := &fakeLedger{estCredits: 4}
	wallet := &fakeWallet{}
	s := newTestService(t, ledger, wallet)

	handle, deny := s.Reserve(context.Background(), reserveInput())
	if deny != nil {
		t.Fatalf("预扣被拒: %+v", deny)
	}

	// 上游 2xx 但没有 usage（HasUsage=false）。
	s.OnComplete(context.Background(), magpie.Result{
		UserID: 7, TokenID: 11, ExternalModel: "tenant-gpt", MagpieModel: "gpt-x",
		Billable: true, Status: 200, Reservation: handle,
	})

	if len(wallet.settles) != 1 || wallet.settles[0] != -4 {
		t.Fatalf("应整笔退回 -4，实际 %v", wallet.settles)
	}
	if len(ledger.records) != 1 {
		t.Fatalf("应落 1 条 failed 账单，实际 %d", len(ledger.records))
	}
	rec := ledger.records[0]
	if !rec.Fail {
		t.Fatalf("无 usage 的账单必须是 failed")
	}
	if rec.ErrorMessage != ReasonNoUsage {
		t.Fatalf("失败原因应为 %q，实际 %q", ReasonNoUsage, rec.ErrorMessage)
	}
	if rec.Tokens.Input != 0 || rec.Tokens.Output != 0 || rec.Tokens.CacheRead != 0 {
		t.Fatalf("failed 账单不许带任何 token（否则等于按估算出账）：%+v", rec.Tokens)
	}
	if rec.PreConsumed != 0 {
		t.Fatalf("failed 账单不应带预扣差额结算，实际 %v", rec.PreConsumed)
	}
	if len(wallet.closes) != 1 || wallet.closes[0] != identity.HoldRefunded {
		t.Fatalf("预扣登记应终结为 refunded，实际 %v", wallet.closes)
	}
}

// TestOnComplete_ZeroUsageTreatedAsMissing `"usage":{}`（三段全零但 HasUsage=true）同样不计费。
func TestOnComplete_ZeroUsageTreatedAsMissing(t *testing.T) {
	ledger := &fakeLedger{estCredits: 2}
	wallet := &fakeWallet{}
	s := newTestService(t, ledger, wallet)
	handle, _ := s.Reserve(context.Background(), reserveInput())

	s.OnComplete(context.Background(), magpie.Result{
		UserID: 7, ExternalModel: "tenant-gpt", Billable: true, Status: 200,
		HasUsage: true, Usage: magpie.Usage{}, Reservation: handle,
	})

	if len(wallet.settles) != 1 || wallet.settles[0] != -2 {
		t.Fatalf("全零 usage 应整笔退回，实际 %v", wallet.settles)
	}
	if len(ledger.records) != 1 || !ledger.records[0].Fail {
		t.Fatalf("全零 usage 应落 failed 账单")
	}
}

// TestOnComplete_WithUsage_SettlesActual 有真实 usage → 按实际结算（预扣作差额依据）。
func TestOnComplete_WithUsage_SettlesActual(t *testing.T) {
	ledger := &fakeLedger{estCredits: 5}
	wallet := &fakeWallet{}
	s := newTestService(t, ledger, wallet)
	handle, _ := s.Reserve(context.Background(), reserveInput())

	s.OnComplete(context.Background(), magpie.Result{
		UserID: 7, TokenID: 11, ExternalModel: "tenant-gpt", MagpieModel: "gpt-x",
		Billable: true, Status: 200, HasUsage: true,
		Usage:       magpie.Usage{Input: 100, Output: 50, CacheRead: 20},
		Reservation: handle,
	})

	if len(wallet.settles) != 0 {
		t.Fatalf("有 usage 时不许退预扣，实际 %v", wallet.settles)
	}
	if len(ledger.records) != 1 {
		t.Fatalf("应落 1 条账单，实际 %d", len(ledger.records))
	}
	rec := ledger.records[0]
	if rec.Fail {
		t.Fatalf("成功请求的账单不许是 failed")
	}
	if rec.Tokens.Input != 100 || rec.Tokens.Output != 50 || rec.Tokens.CacheRead != 20 {
		t.Fatalf("账单必须用上游真实 usage，实际 %+v", rec.Tokens)
	}
	if rec.PreConsumed != 5 {
		t.Fatalf("结算应带上预扣额（只结算差额），实际 %v", rec.PreConsumed)
	}
	if len(wallet.closes) != 1 || wallet.closes[0] != identity.HoldSettled {
		t.Fatalf("预扣登记应终结为 settled，实际 %v", wallet.closes)
	}
}

// TestOnComplete_UpstreamError_Refunds 上游 5xx（即使带了 usage）→ 退预扣 + failed。
func TestOnComplete_UpstreamError_Refunds(t *testing.T) {
	ledger := &fakeLedger{estCredits: 3}
	wallet := &fakeWallet{}
	s := newTestService(t, ledger, wallet)
	handle, _ := s.Reserve(context.Background(), reserveInput())

	s.OnComplete(context.Background(), magpie.Result{
		UserID: 7, ExternalModel: "tenant-gpt", Billable: true, Status: 502,
		Err: errors.New("upstream unreachable"), Reservation: handle,
	})

	if len(wallet.settles) != 1 || wallet.settles[0] != -3 {
		t.Fatalf("上游失败应整笔退回，实际 %v", wallet.settles)
	}
	if len(ledger.records) != 1 || ledger.records[0].ErrorMessage != ReasonUpstreamError {
		t.Fatalf("失败原因应为 %q，实际 %+v", ReasonUpstreamError, ledger.records)
	}
}

// TestOnComplete_SettleFailure_RefundsInsteadOfLosing 结算写库失败 → 退预扣 + failed（不许既不退钱也不落账）。
func TestOnComplete_SettleFailure_RefundsInsteadOfLosing(t *testing.T) {
	ledger := &fakeLedger{estCredits: 3, recordErr: errors.New("db down")}
	wallet := &fakeWallet{}
	s := newTestService(t, ledger, wallet)
	handle, _ := s.Reserve(context.Background(), reserveInput())

	s.OnComplete(context.Background(), magpie.Result{
		UserID: 7, ExternalModel: "tenant-gpt", Billable: true, Status: 200,
		HasUsage: true, Usage: magpie.Usage{Input: 10, Output: 5}, Reservation: handle,
	})

	if len(wallet.settles) != 1 || wallet.settles[0] != -3 {
		t.Fatalf("结算失败必须退回预扣，实际 %v", wallet.settles)
	}
	if len(wallet.closes) != 1 || wallet.closes[0] != identity.HoldRefunded {
		t.Fatalf("预扣登记应终结为 refunded，实际 %v", wallet.closes)
	}
}

// TestOnComplete_NonBillableIgnored count_tokens 类端点不产生任何计费动作。
func TestOnComplete_NonBillableIgnored(t *testing.T) {
	ledger := &fakeLedger{estCredits: 3}
	wallet := &fakeWallet{}
	s := newTestService(t, ledger, wallet)

	s.OnComplete(context.Background(), magpie.Result{Billable: false, Status: 200})
	if len(ledger.records) != 0 || len(wallet.settles) != 0 {
		t.Fatalf("非计费端点不许有任何账单/资金动作")
	}
}

// TestNew_MissingDepsReturnsNil 依赖不全时返回 nil（装配层据此保持「不计费」安全态）。
func TestNew_MissingDepsReturnsNil(t *testing.T) {
	if New(Options{}) != nil {
		t.Fatalf("缺依赖时应返回 nil")
	}
}

// TestStatusFor 归因词（供日志/审计与账单口径对齐）。
func TestStatusFor(t *testing.T) {
	if got := StatusFor(magpie.Result{HasUsage: true, Usage: magpie.Usage{Input: 1}, Status: 200}); got != "ok" {
		t.Fatalf("有 usage 应为 ok，实际 %q", got)
	}
	if got := StatusFor(magpie.Result{Status: 200}); got != "failed:no-usage" {
		t.Fatalf("无 usage 应为 failed:no-usage，实际 %q", got)
	}
	if got := StatusFor(magpie.Result{Status: 503}); got != "failed:upstream-error" {
		t.Fatalf("5xx 应为 failed:upstream-error，实际 %q", got)
	}
}
