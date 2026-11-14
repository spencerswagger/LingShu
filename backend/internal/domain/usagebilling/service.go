// service.go 是「旁路 usage 计费」闭环：预扣 → 转发 → 按 usage 结算（差额多退少补）。
//
// 口径（契约 §3.5，含本期的行为反转）：
//
//	上游 2xx + 有 usage            → 按 usage 计费，completed，estimated=false
//	上游 2xx + 无 usage            → 按请求体估算，completed，estimated=true
//	上游 4xx/5xx/连接失败/超时      → 退预扣、不收钱，failed，credits=0
//	客户端取消 + 有 usage          → 按 usage 计费，completed
//	客户端取消 + 无 usage          → **退预扣、不估算**，failed（旧口径是估算计费）
//	上游 204/304                   → 退预扣、无内容可计费，failed
//
// 这里的实现只做「钱」，不做转发、不写请求日志（call_logs 停写）。
package usagebilling

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/magpie"
)

// Billing 是计费服务的最小依赖（billing.Service 实现该接口）。
type Billing interface {
	EstimateCredits(ctx context.Context, req billing.RecordReq) (float64, error)
	Record(ctx context.Context, req billing.RecordReq) (*billing.Record, error)
}

// Credit 是钱包扣减/差额/退款的最小依赖（identity.CreditService 实现该接口）。
type Credit interface {
	Consume(ctx context.Context, userID int64, amount float64, billingID, sessionID, remark string) (before, after float64, err error)
	Settle(ctx context.Context, userID int64, delta float64, sessionID, remark string) (before, after float64, err error)
}

// UserLookup 取用户定价模式（sale/cost）。nil 时按 sale 计。
type UserLookup interface {
	GetByID(ctx context.Context, id int64) (*identity.User, error)
}

// Reservation 是一次请求的预扣快照（作为不透明 handle 交给 magpie，结算时取回）。
type Reservation struct {
	BillingID     string
	UserID        int64
	TokenID       int64
	PricingMode   string
	ExternalModel string
	MagpieModel   string
	CallerKeyName string
	SessionID     string

	Rates        billing.Rates
	TimeConfig   *billing.TimeCoeffConfig
	ContextTiers []billing.TierRule
	// PreConsumed 是已冻结（已扣）的积分；0 表示未预扣（例如估算为 0）。
	PreConsumed float64
	// EstInputTokens 是预扣时估算的输入 token（用于日志与审计）。
	EstInputTokens int64
}

// AnomalyAuditor 写「资金异常」审计（结算失败、退回预扣失败）。
//
// 为什么必须落审计：这类故障只打日志时，运维与对账都看不见（对账只统计 completed），
// 而它们的后果是「用户被扣钱但账单显示 0」，必须留下可检索的痕迹。
type AnomalyAuditor interface {
	InsertBillingAnomaly(ctx context.Context, detail map[string]any) error
}

// Service 把 magpie 的转发事实翻成账单。
type Service struct {
	billing Billing
	credit  Credit
	users   UserLookup
	clock   func() time.Time
	logger  *slog.Logger
	audit   AnomalyAuditor
}

// SetAuditor 注入资金异常审计（nil = 只打日志，单测默认）。
func (s *Service) SetAuditor(a AnomalyAuditor) { s.audit = a }

// 退回预扣的重试节奏（结算失败/账单落不了库时，钱必须退回去）。
const (
	refundAttempts   = 3
	refundRetryDelay = 50 * time.Millisecond
)

// NewService 装配计费闭环。users 可为 nil（全部按 sale 计）。
func NewService(b Billing, c Credit, users UserLookup, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{billing: b, credit: c, users: users, clock: time.Now, logger: logger}
}

// SetClock 注入时钟（测试用）。
func (s *Service) SetClock(fn func() time.Time) { s.clock = fn }

// 拒绝口径：预扣失败等价于「没额度可用」，按 402 拒绝且不发上游。
const (
	codeInsufficientCredit = "insufficient_credit"
)

// Reserve 在转发前按估算输入 token 预扣（契约：请求前预扣，拿不到钱就不发请求）。
//
// 只按输入 token 估算（与 V1 `estimatePreConsumeCost` 同口径）：输出留待结算多退少补。
// 估算为 0（例如免费模型/单价缺失）时跳过扣款，由结算按实际兜底。
func (s *Service) Reserve(ctx context.Context, in magpie.ReserveInput) (any, *magpie.Deny) {
	rates, timeCfg, tiers := in.Rates, in.TimeConfig, in.ContextTiers
	if len(rates) == 0 {
		return &Reservation{
			UserID: in.UserID, TokenID: in.TokenID, ExternalModel: in.ExternalModel,
			MagpieModel: in.MagpieModel, CallerKeyName: in.CallerKeyName, SessionID: in.SessionID,
			PricingMode: s.pricingMode(ctx, in.UserID),
			BillingID:   billing.GenBillingID(),
		}, nil
	}

	mode := s.pricingMode(ctx, in.UserID)
	billingID := billing.GenBillingID()
	estIn := int64(EstimateInputTokens(in.Body, in.MagpieModel))
	rv := &Reservation{
		BillingID:      billingID,
		UserID:         in.UserID,
		TokenID:        in.TokenID,
		PricingMode:    mode,
		ExternalModel:  in.ExternalModel,
		MagpieModel:    in.MagpieModel,
		CallerKeyName:  in.CallerKeyName,
		SessionID:      in.SessionID,
		Rates:          rates,
		TimeConfig:     timeCfg,
		ContextTiers:   tiers,
		EstInputTokens: estIn,
	}

	credits, err := s.billing.EstimateCredits(ctx, billing.RecordReq{
		BillingID:       billingID,
		UserID:          in.UserID,
		PricingMode:     mode,
		ExternalModel:   in.ExternalModel,
		InternalModelID: in.MagpieModel,
		Tokens:          billing.Usage{Input: estIn},
		Rates:           rates,
		TimeConfig:      timeCfg,
		ContextTiers:    tiers,
		CallTime:        s.clock(),
		MagpieKeyName:   in.CallerKeyName,
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "预扣估算失败", "user_id", in.UserID, "model", in.ExternalModel, "err", err.Error())
		d := magpie.Deny{Status: http.StatusInternalServerError, Code: "internal_error", Message: "计费估算失败，请稍后重试"}
		return nil, &d
	}
	if credits <= 0 {
		return rv, nil
	}
	if _, _, err := s.credit.Consume(ctx, in.UserID, credits, billingID, in.SessionID, billing.FlowRemarkReserve); err != nil {
		// 余额不足 / 钱包缺失：契约口径是「拿不到钱就不发请求」。
		s.logger.WarnContext(ctx, "预扣被拒", "user_id", in.UserID, "model", in.ExternalModel,
			"credits", credits, "err", err.Error())
		d := magpie.Deny{Status: http.StatusPaymentRequired, Code: codeInsufficientCredit, Message: "账户可用积分不足，请先充值"}
		return nil, &d
	}
	rv.PreConsumed = credits
	return rv, nil
}

// Settle 是转发结束后的结算（magpie.Hook）。
func (s *Service) Settle(ctx context.Context, res magpie.Result) {
	if !res.Billable {
		return
	}
	rv, _ := res.Reservation.(*Reservation)
	if rv == nil {
		// 未经预扣（Reserve 未装配）：按全额扣，仍按 usage 计。
		rv = &Reservation{
			BillingID:     billing.GenBillingID(),
			UserID:        res.UserID,
			TokenID:       res.TokenID,
			PricingMode:   s.pricingMode(ctx, res.UserID),
			ExternalModel: res.ExternalModel,
			MagpieModel:   res.MagpieModel,
			CallerKeyName: res.CallerKeyName,
			SessionID:     res.SessionID,
		}
	}

	req := billing.RecordReq{
		BillingID:       rv.BillingID,
		UserID:          rv.UserID,
		PricingMode:     rv.PricingMode,
		ExternalModel:   rv.ExternalModel,
		InternalModelID: rv.MagpieModel,
		SessionID:       rv.SessionID,
		CallTime:        s.clock(),
		Rates:           rv.Rates,
		TimeConfig:      rv.TimeConfig,
		ContextTiers:    rv.ContextTiers,
		PreConsumed:     rv.PreConsumed,
		MagpieKeyName:   rv.CallerKeyName,
	}
	tokenID := rv.TokenID
	req.TokenID = &tokenID
	if res.Duration > 0 {
		ms := res.Duration.Milliseconds()
		req.DurationMs = &ms
	}

	switch {
	case res.NoContent:
		// 204/304：无内容可计费，不退不让、直接退预扣并记失败（契约同 5xx 口径）。
		s.refundAndFail(ctx, rv, req, "上游无内容（204/304）")
		return
	case res.Status == 0 || res.Status >= http.StatusBadRequest:
		msg := res.Err
		if msg == nil {
			msg = fmt.Errorf("上游返回 %d", res.Status)
		}
		s.refundAndFail(ctx, rv, req, failureNote(res.Status, msg))
		return
	case res.Canceled && !res.HasUsage:
		// 行为反转：取消且无 usage 不再估算计费，退预扣。
		s.refundAndFail(ctx, rv, req, "client_cancelled")
		return
	}

	// 2xx（含客户端取消但已拿到 usage）：按 usage 结算。
	if res.HasUsage {
		req.Tokens = billing.Usage{Input: res.Usage.Input, CacheRead: res.Usage.CacheRead, Output: res.Usage.Output}
		req.Estimated = false
		req.UpstreamUsage = res.UsageRaw
	} else {
		req.Tokens = billing.Usage{
			Input:  int64(EstimateInputTokens(res.RequestBody, rv.MagpieModel)),
			Output: EstimateOutputTokens(res.ObservedTextRunes),
		}
		req.Estimated = true
	}
	if _, err := s.billing.Record(ctx, req); err != nil {
		s.logger.ErrorContext(ctx, "账单结算失败", "billing_id", req.BillingID, "user_id", req.UserID,
			"model", req.ExternalModel, "estimated", req.Estimated, "err", err.Error())
		// 结算失败 = 差额没扣成/账单没落成，但预扣在 Reserve 就已经真扣了：
		// 不退回去就是「用户被扣钱、账单显示 0、对账只统计 completed 看不到这笔钱」。
		s.rollbackSettlement(ctx, rv, req, err)
	}
}

// rollbackSettlement 是「结算失败」的收尾：退预扣 → 落一条 failed 账单（金额 0 + 原因）→ 落审计。
//
// 口径：钱优先退回（用户不该为一次失败的结算付钱）；账单要能自解释（failed + credits=0 +
// error_message 说明是结算失败且已退回）。退款最终失败时写审计 + ERROR 日志，必须可被运维发现。
func (s *Service) rollbackSettlement(ctx context.Context, rv *Reservation, req billing.RecordReq, cause error) {
	refundErr := s.refundPreConsumed(ctx, rv, "结算失败")
	if refundErr != nil {
		s.auditAnomaly(ctx, map[string]any{
			"kind": "settle_failed_refund_failed", "billing_id": req.BillingID, "user_id": rv.UserID,
			"pre_consumed": rv.PreConsumed, "settle_error": cause.Error(), "refund_error": refundErr.Error(),
		})
	}
	note := "结算失败（预扣已退回）"
	if refundErr != nil {
		note = "结算失败且退回预扣失败（需人工核对）"
	}
	failReq := req
	failReq.Fail = true
	failReq.PreConsumed = 0
	failReq.Tokens = billing.Usage{}
	failReq.Estimated = false
	failReq.UpstreamUsage = nil
	failReq.ErrorMessage = note
	if _, ferr := s.billing.Record(ctx, failReq); ferr != nil {
		s.logger.ErrorContext(ctx, "结算失败的 failed 账单落库失败",
			"billing_id", req.BillingID, "err", ferr.Error())
		s.auditAnomaly(ctx, map[string]any{
			"kind": "settle_failed_bill_lost", "billing_id": req.BillingID, "user_id": rv.UserID,
			"settle_error": cause.Error(), "record_error": ferr.Error(),
		})
	}
}

// refundPreConsumed 退回预扣（带重试）；返回非 nil 表示最终失败。
func (s *Service) refundPreConsumed(ctx context.Context, rv *Reservation, reason string) error {
	if rv.PreConsumed <= 0 {
		return nil
	}
	var lastErr error
	for i := 0; i < refundAttempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return lastErr
			case <-time.After(refundRetryDelay):
			}
		}
		_, _, err := s.credit.Settle(ctx, rv.UserID, -rv.PreConsumed, rv.SessionID, billing.FlowRemarkRefund)
		if err == nil {
			return nil
		}
		lastErr = err
		s.logger.WarnContext(ctx, "退回预扣失败，准备重试", "billing_id", rv.BillingID,
			"user_id", rv.UserID, "pre_consumed", rv.PreConsumed, "attempt", i+1, "reason", reason, "err", err.Error())
	}
	s.logger.ErrorContext(ctx, "退回预扣最终失败（需人工核对）", "billing_id", rv.BillingID,
		"user_id", rv.UserID, "pre_consumed", rv.PreConsumed, "reason", reason, "err", lastErr.Error())
	return lastErr
}

// auditAnomaly 写资金异常审计（未注入时只打日志，但仍带 ERROR 级别与结构化字段）。
func (s *Service) auditAnomaly(ctx context.Context, detail map[string]any) {
	if s.audit == nil {
		s.logger.ErrorContext(ctx, "资金异常（未装配审计）", "detail", fmt.Sprintf("%v", detail))
		return
	}
	if err := s.audit.InsertBillingAnomaly(ctx, detail); err != nil {
		s.logger.ErrorContext(ctx, "资金异常审计写入失败", "detail", fmt.Sprintf("%v", detail), "err", err.Error())
	}
}

// refundAndFail 退回预扣并落一条 failed 账单（credits=0，无 usage）。
func (s *Service) refundAndFail(ctx context.Context, rv *Reservation, req billing.RecordReq, reason string) {
	if err := s.refundPreConsumed(ctx, rv, reason); err != nil {
		// 退款最终失败：钱已经扣了但没退回去，必须留下可检索的审计（对账看不到这笔）。
		s.auditAnomaly(ctx, map[string]any{
			"kind": "refund_failed", "billing_id": rv.BillingID, "user_id": rv.UserID,
			"pre_consumed": rv.PreConsumed, "reason": reason, "refund_error": err.Error(),
		})
	}
	req.Fail = true
	req.PreConsumed = 0
	req.Tokens = billing.Usage{}
	req.Estimated = false
	req.UpstreamUsage = nil
	req.ErrorMessage = reason
	if _, err := s.billing.Record(ctx, req); err != nil {
		s.logger.ErrorContext(ctx, "失败账单落库失败", "billing_id", req.BillingID, "err", err.Error())
	}
}

// failureNote 把上游失败归一成可落库文案（不暴露内部地址/凭据）。
func failureNote(status int, cause error) string {
	if status >= http.StatusInternalServerError {
		return fmt.Sprintf("上游服务异常（%d）", status)
	}
	if status >= http.StatusBadRequest {
		return fmt.Sprintf("上游拒绝请求（%d）", status)
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return "上游响应超时"
	}
	return "上游调用失败"
}

// pricingMode 取用户定价模式（缺省 sale）。cost 模式本期没有成本价源，仍按售价计费。
func (s *Service) pricingMode(ctx context.Context, userID int64) string {
	if s.users != nil {
		if u, err := s.users.GetByID(ctx, userID); err == nil && u.PricingMode != "" {
			return u.PricingMode
		}
	}
	return identity.PricingModeSale
}
