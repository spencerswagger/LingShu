package usagebilling

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/magpie"
)

// ---- fakes ----

type estCall struct {
	tokens billing.Usage
	rates  billing.Rates
}

type fakeBilling struct {
	estCredits float64
	estErr     error
	estCalls   []estCall
	recs       []billing.RecordReq
	recErr     error
}

func (f *fakeBilling) EstimateCredits(_ context.Context, req billing.RecordReq) (float64, error) {
	f.estCalls = append(f.estCalls, estCall{tokens: req.Tokens, rates: req.Rates})
	return f.estCredits, f.estErr
}

func (f *fakeBilling) Record(_ context.Context, req billing.RecordReq) (*billing.Record, error) {
	f.recs = append(f.recs, req)
	if f.recErr != nil {
		return nil, f.recErr
	}
	return &billing.Record{BillingID: req.BillingID}, nil
}

type fakeCredit struct {
	consumeErr error
	consumed   []float64
	settled    []float64
	// settleErrs 按调用序号返回错误（用尽后返回 nil），用于「退款先失败后成功」。
	settleErrs []error
}

// settleErrAt 取第 i 次 Settle 的错误（超出列表即成功）。
func (f *fakeCredit) settleErrAt(i int) error {
	if i < len(f.settleErrs) {
		return f.settleErrs[i]
	}
	return nil
}

func (f *fakeCredit) Consume(_ context.Context, _ int64, amount float64, _, _, _ string) (float64, float64, error) {
	if f.consumeErr != nil {
		return 0, 0, f.consumeErr
	}
	f.consumed = append(f.consumed, amount)
	return 100, 100 - amount, nil
}

func (f *fakeCredit) Settle(_ context.Context, _ int64, delta float64, _, _ string) (float64, float64, error) {
	n := len(f.settled)
	f.settled = append(f.settled, delta)
	if err := f.settleErrAt(n); err != nil {
		return 0, 0, err
	}
	return 0, 0, nil
}

var testRates = billing.Rates{"input": 1000, "output": 2000, "cache_read": 100}

func newSvc(b *fakeBilling, c *fakeCredit) *Service {
	s := NewService(b, c, nil, nil)
	s.SetClock(func() time.Time { return time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC) })
	return s
}

func reserveInput(body string) magpie.ReserveInput {
	return magpie.ReserveInput{
		Endpoint: "/chat/completions", Protocol: magpie.ProtocolOpenAIChat,
		TokenID: 3, UserID: 7, ExternalModel: "gpt-x", MagpieModel: "openai/gpt-4o",
		CallerKeyName: "key-a", SessionID: "sess-1", Body: []byte(body),
		Rates: testRates,
	}
}

// ---- 预扣 ----

func TestReserve_DeductsEstimatedInputOnly(t *testing.T) {
	b := &fakeBilling{estCredits: 1.5}
	c := &fakeCredit{}
	s := newSvc(b, c)

	handle, deny := s.Reserve(context.Background(), reserveInput(`{"messages":[{"role":"user","content":"hello world"}]}`))
	if deny != nil {
		t.Fatalf("不应拒绝: %+v", deny)
	}
	rv, ok := handle.(*Reservation)
	if !ok || rv.PreConsumed != 1.5 {
		t.Fatalf("预扣未生效: %+v", handle)
	}
	if len(b.estCalls) != 1 {
		t.Fatalf("预估调用次数=%d", len(b.estCalls))
	}
	// 预扣只估算输入（输出留待结算多退少补）。
	if b.estCalls[0].tokens.Output != 0 || b.estCalls[0].tokens.Input <= 0 {
		t.Fatalf("预扣 tokens=%+v（应只含输入）", b.estCalls[0].tokens)
	}
	if len(c.consumed) != 1 || c.consumed[0] != 1.5 {
		t.Fatalf("扣款=%v", c.consumed)
	}
	if rv.BillingID == "" || rv.PricingMode != "sale" || rv.MagpieModel != "openai/gpt-4o" {
		t.Fatalf("预扣快照不完整: %+v", rv)
	}
}

// 余额不足 → 402 拒绝且不发上游、不产生账单。
func TestReserve_InsufficientCreditDenies(t *testing.T) {
	b := &fakeBilling{estCredits: 9}
	c := &fakeCredit{consumeErr: errors.New("insufficient balance")}
	s := newSvc(b, c)

	handle, deny := s.Reserve(context.Background(), reserveInput(`{"messages":[]}`))
	if deny == nil {
		t.Fatalf("余额不足应拒绝，handle=%+v", handle)
	}
	if deny.Status != 402 || deny.Code != codeInsufficientCredit {
		t.Fatalf("拒绝口径=%+v", deny)
	}
	if len(b.recs) != 0 {
		t.Fatalf("被拒请求不应落账单: %+v", b.recs)
	}
}

// 估算为 0（免费模型/无单价）→ 不扣款但仍返回快照（结算按全额兜底）。
func TestReserve_ZeroCreditsSkipsDeduction(t *testing.T) {
	b := &fakeBilling{estCredits: 0}
	c := &fakeCredit{}
	s := newSvc(b, c)

	handle, deny := s.Reserve(context.Background(), reserveInput(`{"messages":[]}`))
	if deny != nil {
		t.Fatalf("不应拒绝: %+v", deny)
	}
	if len(c.consumed) != 0 {
		t.Fatalf("0 积分不应调用扣款: %v", c.consumed)
	}
	if rv := handle.(*Reservation); rv.PreConsumed != 0 {
		t.Fatalf("PreConsumed 应为 0: %+v", rv)
	}
}

// ---- 结算 ----

func settlementFixture(t *testing.T, res magpie.Result) (*Service, *fakeBilling, *fakeCredit, magpie.Result) {
	t.Helper()
	b := &fakeBilling{estCredits: 2}
	c := &fakeCredit{}
	s := newSvc(b, c)
	handle, deny := s.Reserve(context.Background(), reserveInput(`{"messages":[{"role":"user","content":"hi there"}]}`))
	if deny != nil {
		t.Fatalf("预扣被拒: %+v", deny)
	}
	res.Reservation = handle
	res.Billable = true
	res.UserID, res.TokenID = 7, 3
	res.ExternalModel, res.MagpieModel, res.CallerKeyName = "gpt-x", "openai/gpt-4o", "key-a"
	return s, b, c, res
}

// 非流式：拿到 usage 即按 usage 结算、estimated=false、留原文。
func TestSettle_NonStreamWithUsage(t *testing.T) {
	res := magpie.Result{
		Status: 200, Streamed: false,
		Usage:    magpie.Usage{Input: 100, CacheRead: 20, Output: 50},
		HasUsage: true, UsageRaw: []byte(`{"prompt_tokens":120,"completion_tokens":50}`),
	}
	s, b, c, res := settlementFixture(t, res)
	s.Settle(context.Background(), res)

	if len(c.settled) != 0 {
		t.Fatalf("成功请求不应退款: %v", c.settled)
	}
	if len(b.recs) != 1 {
		t.Fatalf("账单条数=%d", len(b.recs))
	}
	rec := b.recs[0]
	if rec.Estimated || rec.Fail {
		t.Fatalf("estimated/fail=%v/%v", rec.Estimated, rec.Fail)
	}
	if rec.Tokens != (billing.Usage{Input: 100, CacheRead: 20, Output: 50}) {
		t.Fatalf("tokens=%+v", rec.Tokens)
	}
	if string(rec.UpstreamUsage) == "" || rec.MagpieKeyName != "key-a" {
		t.Fatalf("upstream_usage/key=%q/%q", rec.UpstreamUsage, rec.MagpieKeyName)
	}
	if rec.PreConsumed != 2 {
		t.Fatalf("差额结算需要 PreConsumed=2, got %v", rec.PreConsumed)
	}
}

// 流式：结算口径与非流式一致（usage 已由旁路累计）。
func TestSettle_StreamWithUsage(t *testing.T) {
	res := magpie.Result{
		Status: 200, Streamed: true,
		Usage:    magpie.Usage{Input: 10, Output: 5},
		HasUsage: true, UsageRaw: []byte(`{"input_tokens":10,"output_tokens":5}`),
	}
	s, b, _, res := settlementFixture(t, res)
	s.Settle(context.Background(), res)
	if len(b.recs) != 1 || b.recs[0].Estimated {
		t.Fatalf("流式应按 usage 结算: %+v", b.recs)
	}
	if b.recs[0].Tokens != (billing.Usage{Input: 10, Output: 5}) {
		t.Fatalf("tokens=%+v", b.recs[0].Tokens)
	}
}

// 2xx 无 usage → 估算出账且 estimated=true、upstream_usage 为空。
func TestSettle_NoUsageMarksEstimated(t *testing.T) {
	res := magpie.Result{
		Status: 200, ObservedTextRunes: 400,
		RequestBody: []byte(`{"messages":[{"role":"user","content":"estimate me please"}]}`),
	}
	s, b, c, res := settlementFixture(t, res)
	s.Settle(context.Background(), res)

	if len(c.settled) != 0 {
		t.Fatalf("2xx 不应退款: %v", c.settled)
	}
	rec := b.recs[0]
	if !rec.Estimated {
		t.Fatalf("应标 estimated=true")
	}
	if len(rec.UpstreamUsage) != 0 {
		t.Fatalf("estimated 单不应写 upstream_usage: %q", rec.UpstreamUsage)
	}
	if rec.Tokens.Input <= 0 || rec.Tokens.Output != 200 {
		t.Fatalf("估算 tokens=%+v", rec.Tokens)
	}
}

// 上游 5xx → 退预扣、不收钱。
func TestSettle_Upstream5xxRefunds(t *testing.T) {
	for _, status := range []int{500, 502, 503, 0} {
		res := magpie.Result{Status: status, Err: errors.New("upstream boom")}
		s, b, c, res := settlementFixture(t, res)
		s.Settle(context.Background(), res)

		if len(c.settled) != 1 || c.settled[0] != -2 {
			t.Fatalf("status=%d 退款=%v（应为 -2）", status, c.settled)
		}
		rec := b.recs[0]
		if !rec.Fail || rec.PreConsumed != 0 || rec.Estimated {
			t.Fatalf("status=%d 失败账单=%+v", status, rec)
		}
		if rec.ErrorMessage == "" {
			t.Fatalf("status=%d 失败账单缺原因", status)
		}
	}
}

// 超时（无响应）也退预扣：状态码 0 走同一分支。
func TestSettle_TimeoutRefunds(t *testing.T) {
	res := magpie.Result{Status: 0, TimedOut: true, Err: context.DeadlineExceeded}
	s, b, c, res := settlementFixture(t, res)
	s.Settle(context.Background(), res)
	if len(c.settled) != 1 || c.settled[0] != -2 {
		t.Fatalf("超时退款=%v", c.settled)
	}
	if !b.recs[0].Fail || b.recs[0].ErrorMessage != "上游响应超时" {
		t.Fatalf("超时失败口径=%+v", b.recs[0])
	}
}

// 取消 + 有 usage → 仍按 usage 计费（不退款）。
func TestSettle_CanceledWithUsageChargesActual(t *testing.T) {
	res := magpie.Result{
		Status: 200, Canceled: true,
		Usage: magpie.Usage{Input: 9, Output: 4}, HasUsage: true,
	}
	s, b, c, res := settlementFixture(t, res)
	s.Settle(context.Background(), res)
	if len(c.settled) != 0 {
		t.Fatalf("有 usage 的取消不应退款: %v", c.settled)
	}
	if len(b.recs) != 1 || b.recs[0].Fail || b.recs[0].Estimated {
		t.Fatalf("取消计费口径=%+v", b.recs)
	}
}

// 行为反转：取消且无 usage → 退预扣、不估算计费，失败原因固定 client_cancelled。
func TestSettle_CanceledWithoutUsageRefundsAndDoesNotEstimate(t *testing.T) {
	res := magpie.Result{Status: 200, Canceled: true, ObservedTextRunes: 10000}
	s, b, c, res := settlementFixture(t, res)
	s.Settle(context.Background(), res)

	if len(c.settled) != 1 || c.settled[0] != -2 {
		t.Fatalf("取消无 usage 应退预扣: %v", c.settled)
	}
	rec := b.recs[0]
	if !rec.Fail || rec.Estimated {
		t.Fatalf("取消无 usage 不应估算计费: %+v", rec)
	}
	if rec.ErrorMessage != "client_cancelled" {
		t.Fatalf("失败原因=%q", rec.ErrorMessage)
	}
	if rec.Tokens != (billing.Usage{}) {
		t.Fatalf("失败单 tokens 应为空: %+v", rec.Tokens)
	}
}

// 204/304：无内容可计费 → 退预扣 + failed。
func TestSettle_NoContentRefunds(t *testing.T) {
	res := magpie.Result{Status: 204, NoContent: true}
	s, b, c, res := settlementFixture(t, res)
	s.Settle(context.Background(), res)
	if len(c.settled) != 1 || c.settled[0] != -2 {
		t.Fatalf("204 退款=%v", c.settled)
	}
	if !b.recs[0].Fail {
		t.Fatalf("204 应为 failed: %+v", b.recs[0])
	}
}

// 不计费端点（count_tokens）不产生任何账务动作。
func TestSettle_NonBillableIsNoop(t *testing.T) {
	b := &fakeBilling{}
	c := &fakeCredit{}
	s := newSvc(b, c)
	s.Settle(context.Background(), magpie.Result{Status: 200, HasUsage: true, Billable: false})
	if len(b.recs) != 0 || len(c.settled) != 0 || len(c.consumed) != 0 {
		t.Fatalf("不计费端点产生了账务动作: recs=%d settled=%v", len(b.recs), c.settled)
	}
}

// 未预扣（Reserve 未装配）也要按 usage 全额出账。
func TestSettle_WithoutReservationChargesFull(t *testing.T) {
	b := &fakeBilling{}
	c := &fakeCredit{}
	s := newSvc(b, c)
	s.Settle(context.Background(), magpie.Result{
		Billable: true, Status: 200, UserID: 7, TokenID: 3,
		ExternalModel: "gpt-x", MagpieModel: "openai/gpt-4o", CallerKeyName: "key-a",
		HasUsage: true, Usage: magpie.Usage{Input: 1, Output: 2},
	})
	if len(b.recs) != 1 {
		t.Fatalf("账单条数=%d", len(b.recs))
	}
	if b.recs[0].PreConsumed != 0 || b.recs[0].MagpieKeyName != "key-a" {
		t.Fatalf("无预扣账单=%+v", b.recs[0])
	}
}

// ---- 结算失败（P1：钱必须退回去，账单必须自解释）----

// fakeAnomalyAudit 收集资金异常审计（reconcile_test.go 已占用 fakeAudit 名）。
type fakeAnomalyAudit struct {
	details []map[string]any
	err     error
}

func (f *fakeAnomalyAudit) InsertBillingAnomaly(_ context.Context, detail map[string]any) error {
	f.details = append(f.details, detail)
	return f.err
}

// 结算失败（差额扣不动 / 账单没落）→ 预扣整笔退回 + 落 failed 账单（credits=0 + 原因）。
func TestSettle_RecordFailureRefundsPreConsumption(t *testing.T) {
	b := &fakeBilling{estCredits: 2.0, recErr: errors.New("db down")}
	c := &fakeCredit{}
	svc := newSvc(b, c)

	handle, deny := svc.Reserve(context.Background(), reserveInput(`{"model":"gpt-x","messages":[]}`))
	if deny != nil || handle == nil {
		t.Fatalf("预扣应成功: deny=%+v", deny)
	}
	rv := handle.(*Reservation)
	if rv.PreConsumed != 2.0 {
		t.Fatalf("预扣=%v", rv.PreConsumed)
	}

	svc.Settle(context.Background(), magpie.Result{
		Billable: true, Status: 200, HasUsage: true, Usage: magpie.Usage{Input: 10, Output: 5},
		Reservation: rv, UserID: 7, TokenID: 3, ExternalModel: "gpt-x",
	})

	// 预扣 2.0 必须被退回（负数差额）。
	var refunded bool
	for _, d := range c.settled {
		if d == -2.0 {
			refunded = true
		}
	}
	if !refunded {
		t.Fatalf("结算失败必须退回预扣，settled=%v", c.settled)
	}
	// 最后一次账单必须是 failed + 0 积分 + 自解释原因。
	last := b.recs[len(b.recs)-1]
	if !last.Fail || last.PreConsumed != 0 || last.Tokens.Input != 0 {
		t.Fatalf("失败账单口径: %+v", last)
	}
	if !strings.Contains(last.ErrorMessage, "结算失败") || !strings.Contains(last.ErrorMessage, "退回") {
		t.Fatalf("失败账单要能自解释（状态+金额+已退回）: %q", last.ErrorMessage)
	}
}

// 退款最终失败：必须落审计（对账看不到这笔钱，只能靠审计与告警发现）+ 账单标注需人工核对。
func TestSettle_RefundFailureLeavesAudit(t *testing.T) {
	b := &fakeBilling{estCredits: 2.0, recErr: errors.New("db down")}
	c := &fakeCredit{settleErrs: []error{errors.New("wallet locked"), errors.New("wallet locked"), errors.New("wallet locked")}}
	audit := &fakeAnomalyAudit{}
	svc := newSvc(b, c)
	svc.SetAuditor(audit)

	handle, _ := svc.Reserve(context.Background(), reserveInput(`{"model":"gpt-x","messages":[]}`))
	rv := handle.(*Reservation)
	svc.Settle(context.Background(), magpie.Result{
		Billable: true, Status: 200, HasUsage: true, Usage: magpie.Usage{Input: 1, Output: 1},
		Reservation: rv, UserID: 7, TokenID: 3, ExternalModel: "gpt-x",
	})

	if len(c.settled) != refundAttempts {
		t.Fatalf("退款应重试 %d 次，实际 %d", refundAttempts, len(c.settled))
	}
	if len(audit.details) == 0 {
		t.Fatal("退款最终失败必须落审计")
	}
	kind, _ := audit.details[0]["kind"].(string)
	if kind != "settle_failed_refund_failed" {
		t.Fatalf("审计类别=%q detail=%v", kind, audit.details[0])
	}
	last := b.recs[len(b.recs)-1]
	if !strings.Contains(last.ErrorMessage, "人工") {
		t.Fatalf("退款失败的账单要标注需人工核对: %q", last.ErrorMessage)
	}
}

// 退款先失败后成功：重试生效（不许一次失败就放弃）。
func TestRefundPreConsumed_RetriesUntilSuccess(t *testing.T) {
	c := &fakeCredit{settleErrs: []error{errors.New("transient")}}
	svc := newSvc(&fakeBilling{}, c)
	rv := &Reservation{BillingID: "bill-x", UserID: 7, PreConsumed: 1.5}

	if err := svc.refundPreConsumed(context.Background(), rv, "测试"); err != nil {
		t.Fatalf("第二次应成功: %v", err)
	}
	if len(c.settled) != 2 || c.settled[0] != -1.5 || c.settled[1] != -1.5 {
		t.Fatalf("重试与金额: %v", c.settled)
	}
}

// 失败路径（上游 5xx）退款失败也要审计，不能只打日志。
func TestRefundAndFail_AuditsRefundFailure(t *testing.T) {
	b := &fakeBilling{estCredits: 1.0}
	c := &fakeCredit{settleErrs: []error{errors.New("x"), errors.New("x"), errors.New("x")}}
	audit := &fakeAnomalyAudit{}
	svc := newSvc(b, c)
	svc.SetAuditor(audit)

	handle, _ := svc.Reserve(context.Background(), reserveInput(`{"model":"gpt-x","messages":[]}`))
	rv := handle.(*Reservation)
	svc.Settle(context.Background(), magpie.Result{
		Billable: true, Status: 500, Reservation: rv, UserID: 7, TokenID: 3, ExternalModel: "gpt-x",
	})

	if len(audit.details) == 0 || audit.details[0]["kind"] != "refund_failed" {
		t.Fatalf("上游失败时退款失败必须审计: %v", audit.details)
	}
}
