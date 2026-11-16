// Package facadebill 是 /v1 facade 转发链的计费钩子（预扣 + 结算 + 退预扣）。
//
// # 口径（2026-11-16 终裁，覆盖此前所有版本）
//
//  1. **无 usage 一律不计费**：上游没给真实 usage（或给的是全零）→ 整笔退预扣，
//     账单记 `failed` + 原因 `no-usage`，**不做任何估算计费**。
//     估算只允许出现在「预扣」这一侧（按请求体规模冻结一笔押金），
//     绝不允许拿它当最终出账金额。此前 `estimated=true` 的账单列已随该口径下线。
//  2. 上游明确失败（5xx / 传输失败 / 超时 / 客户端断开且无 usage）→ 同样整笔退预扣 + failed 账单。
//  3. 只有「上游 2xx 且拿到真实 usage」才结算真实金额；预扣只作差额依据（多退少补）。
//  4. 结算与退预扣必须用 `context.WithoutCancel`：客户端断开时 r.Context() 已取消，
//     直接用它写库会导致「钱没退、账单没落」。
//
// # 为什么单独成包
//
// magpie 包刻意不碰钱（只产出 Result 事实），本包把「事实 → 账单/钱包」的映射收在一处，
// 换成任何一套资金实现（当前是 billing.Service + identity.CreditService 的预扣登记）都只改这里。
package facadebill

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/magpie"
	"github.com/team/llmgateway/internal/pkg/dbx"
	"github.com/team/llmgateway/internal/pkg/idgen"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// 计费失败原因（落 billing_records.error_message，也是查询口径的唯一来源）。
const (
	// ReasonNoUsage：上游 2xx 但没有可用 usage。
	ReasonNoUsage = "no-usage"
	// ReasonUpstreamError：上游失败（5xx / 传输失败 / 超时）。
	ReasonUpstreamError = "upstream-error"
	// ReasonCanceled：客户端中途断开且没拿到 usage。
	ReasonCanceled = "client-canceled"
)

// Ledger 是账单写入侧（billing.Service 的子集）。
type Ledger interface {
	// EstimateCredits 估算预扣额（预扣专用，不参与最终出账）。
	EstimateCredits(ctx context.Context, req billing.RecordReq) (float64, error)
	// Record 落一条账单（PreConsumed>0 时只结算差额）。
	Record(ctx context.Context, req billing.RecordReq) (*billing.Record, error)
}

// Wallet 是资金侧（identity.CreditService 的子集）。
//
// ⚠️ 这里**没有**「单独退钱」和「单独终结」两个入口，只有两种原子终结方式（Owl 复审 P1）：
//   - SettleHold：终结登记 + 改余额 + 写流水，**一个事务**（失败/取消路径用）；
//   - CloseHoldTx：把终结塞进 billing 的结算事务（成功路径用，钱与登记同生共死）。
//
// 旧的 `Settle` + `CloseHold` 组合会让「钱动了、登记没落」成为可能，过期扫描随后再退一次。
type Wallet interface {
	PreConsumeHold(ctx context.Context, req identity.HoldRequest) (identity.Hold, error)
	// SettleHold 同一事务内终结登记并按 Delta 多退少补；false = 已被别的路径终结（不动钱）。
	SettleHold(ctx context.Context, req identity.SettleHoldRequest) (bool, error)
	// CloseHoldTx 在调用方事务内终结登记（billing.Record 的结算事务会调用它）。
	CloseHoldTx(ctx context.Context, ex dbx.Execer, billingID, status, ledgerEntryID string) (bool, error)
}

// Pricer 提供命中模型的售价（三段单价 + 模型级时段/分档）。
type Pricer interface {
	SaleRatesFor(ctx context.Context, externalModel string) (billing.Rates, *billing.TimeCoeffConfig, []billing.TierRule, error)
}

// Options 是装配参数。
type Options struct {
	Ledger Ledger
	Wallet Wallet
	Pricer Pricer
	// Now 可注入（测试用）；nil 用 time.Now。
	Now    func() time.Time
	Logger *slog.Logger
	// ReserveRatio 是预扣比例（0 或 >1 时按 1）；默认 1，即按估算全额冻结。
	ReserveRatio float64
}

// Service 同时实现 magpie.ReserveHook 与 magpie.Hook。
type Service struct {
	ledger Ledger
	wallet Wallet
	pricer Pricer
	now    func() time.Time
	logger *slog.Logger
	ratio  float64
	// billingID 生成器可注入（测试用）。
	newID func() string
}

// New 装配计费钩子；缺依赖时返回 nil（调用方保持「不产生计费动作」的安全态）。
func New(opt Options) *Service {
	if opt.Ledger == nil || opt.Wallet == nil || opt.Pricer == nil {
		return nil
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	lg := opt.Logger
	if lg == nil {
		lg = slog.Default()
	}
	ratio := opt.ReserveRatio
	if ratio <= 0 || ratio > 1 {
		ratio = 1
	}
	return &Service{
		ledger: opt.Ledger, wallet: opt.Wallet, pricer: opt.Pricer,
		now: now, logger: lg, ratio: ratio,
		newID: newBillingID,
	}
}

// reservation 是预扣句柄（交给 magpie，结算时原样带回）。
type reservation struct {
	billingID  string
	userID     int64
	tokenID    int64
	extModel   string
	protocol   string
	endpoint   string
	sessionID  string
	pre        float64
	rates      billing.Rates
	timeConfig *billing.TimeCoeffConfig
	tiers      []billing.TierRule
	reservedAt time.Time
}

// Reserve 是预扣钩子：估算 → 冻结 → 返回句柄。余额不足返回 402（不透传上游）。
func (s *Service) Reserve(ctx context.Context, in magpie.ReserveInput) (any, *magpie.Deny) {
	rates, tc, tiers, err := s.pricer.SaleRatesFor(ctx, in.ExternalModel)
	if err != nil {
		// 价格缺失属装配/配置问题：不给假的预扣额，直接拒绝（宁可 402 也别放过去白送一次）。
		s.logger.Warn("预扣失败：取不到模型售价", "model", in.ExternalModel, "err", err.Error())
		return nil, &magpie.Deny{Status: 402, Code: magpie.CodeInsufficientQuota, Message: "模型未配置售价，暂不可调用"}
	}
	est := estimateInputTokens(in.RequestBody)
	req := billing.RecordReq{
		UserID:        in.UserID,
		ExternalModel: in.ExternalModel,
		Tokens:        billing.Usage{Input: est},
		Rates:         rates,
		TimeConfig:    tc,
		ContextTiers:  tiers,
	}
	pre, err := s.ledger.EstimateCredits(ctx, req)
	if err != nil {
		s.logger.Warn("预扣估算失败", "model", in.ExternalModel, "err", err.Error())
		return nil, &magpie.Deny{Status: 402, Code: magpie.CodeInsufficientQuota, Message: "无法计算预扣金额"}
	}
	pre *= s.ratio
	handle := &reservation{
		billingID: s.newID(), userID: in.UserID, tokenID: in.TokenID,
		extModel: in.ExternalModel, protocol: in.Protocol, endpoint: in.Endpoint,
		sessionID: in.SessionID, pre: pre, rates: rates, timeConfig: tc, tiers: tiers,
		reservedAt: s.now(),
	}
	if pre <= 0 {
		// 免费模型（售价 0）：不冻结，但仍要落账单（否则调用不留痕）。
		return handle, nil
	}
	if _, err := s.wallet.PreConsumeHold(ctx, identity.HoldRequest{
		BillingID: handle.billingID,
		UserID:    in.UserID,
		TokenID:   tokenPtr(in.TokenID),
		Amount:    pre,
		SessionID: in.SessionID,
		Remark:    "facade 转发预扣",
	}); err != nil {
		if isInsufficient(err) {
			return nil, &magpie.Deny{Status: 402, Code: magpie.CodeInsufficientQuota, Message: "积分余额不足"}
		}
		s.logger.Warn("预扣失败", "model", in.ExternalModel, "err", err.Error())
		return nil, &magpie.Deny{Status: 402, Code: magpie.CodeInsufficientQuota, Message: "预扣失败，请稍后重试"}
	}
	return handle, nil
}

// OnComplete 是结算钩子（magpie.Hook）。**必须**用 WithoutCancel 的 ctx 调用。
func (s *Service) OnComplete(ctx context.Context, res magpie.Result) {
	if !res.Billable {
		return
	}
	// 客户端断开时 r.Context() 已取消：钱与账单都要落库，所以剥掉取消。
	ctx = context.WithoutCancel(ctx)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	handle, _ := res.Reservation.(*reservation)
	if handle == nil {
		// 没预扣（未装配 OnReserve / 免费路径没句柄）：没有可退的钱，只补一条失败账单留痕。
		if !hasUsage(res) {
			reason := reasonOf(res)
			s.recordFailure(ctx, nil, res, reason)
			s.warnNoUsage(res, "", reason)
		}
		return
	}

	if !hasUsage(res) {
		// 口径①：无 usage 一律不计费 —— 整笔退预扣，落 failed/no-usage，绝不出估算金额。
		reason := reasonOf(res)
		s.refundAndClose(ctx, handle, reason)
		s.recordFailure(ctx, handle, res, reason)
		s.warnNoUsage(res, handle.billingID, reason)
		return
	}

	// 有真实 usage：按实际用量结算，预扣只作差额依据。
	credits, err := s.settle(ctx, handle, res)
	if err != nil {
		// 结算失败不能装作成功：退预扣 + failed 账单（原因里带真实错误，运维可查）。
		// 结算事务已整体回滚（含 hold 终结），所以这里 hold 仍是 held，退款路径能抢到终结权。
		s.logger.Error("结算失败，改为退预扣", "billing_id", handle.billingID, "err", err.Error())
		s.refundAndClose(ctx, handle, ReasonUpstreamError)
		s.recordFailureWith(ctx, handle, res, "settle-failed: "+err.Error())
		return
	}
	// 登记已由结算事务一并终结（billing.Record 的 Hold 参数），这里不再单独 close。
	s.logger.Info("facade 转发已结算",
		"billing_id", handle.billingID, "model", handle.extModel,
		"credits", credits, "pre", handle.pre, "endpoint", handle.endpoint)
}

// warnNoUsage 把「不给 usage 就不计费」这条口径的**代价**打成可观测事实（Owl 复审 P1-4）。
//
// 风险原样说清：流式请求若上游不回 usage，本次内容对租户是**白送**（预扣整笔退回、账单元）。
// 现在只在 `billing_records.error_message='no-usage'` 里留一条事后线索，量不出规模，
// 所以这里补三样东西：下行字节数（真的送出多少）、是否流式、上游状态码。
// 要不要再加「占比告警 / 注入 include_usage / 按 max_tokens 保留预扣」属计费口径变更，
// 需产品拍板，本批不做（登记在交付说明的未修项里）。
func (s *Service) warnNoUsage(res magpie.Result, billingID, reason string) {
	if s.logger == nil || reason != ReasonNoUsage {
		return
	}
	s.logger.Warn("上游未返回 usage：本次不计费（预扣已整笔退回）",
		"billing_id", billingID,
		"model", res.ExternalModel,
		"endpoint", res.Endpoint,
		"stream", res.Streamed,
		"status", res.Status,
		"downstream_bytes", res.DownstreamBytes,
		"duration_ms", res.Duration.Milliseconds())
}

// settle 按真实 usage 落账单（PreConsumed = 预扣额，只结算差额），返回本次实际积分。
func (s *Service) settle(ctx context.Context, h *reservation, res magpie.Result) (float64, error) {
	rec, err := s.ledger.Record(ctx, billing.RecordReq{
		BillingID:        h.billingID,
		UserID:           h.userID,
		TokenID:          tokenPtr(h.tokenID),
		ExternalModel:    h.extModel,
		SessionID:        h.sessionID,
		CallTime:         h.reservedAt,
		Tokens:           billing.Usage{Input: res.Usage.Input, Output: res.Usage.Output, CacheRead: res.Usage.CacheRead},
		Rates:            h.rates,
		TimeConfig:       h.timeConfig,
		ContextTiers:     h.tiers,
		PreConsumed:      h.pre,
		Hold:             s.holdClose(h, identity.HoldSettled),
		Remark:           "facade 转发结算",
		UpstreamProtocol: h.protocol,
		UpstreamModel:    res.MagpieModel,
	})
	if err != nil {
		return 0, err
	}
	if rec == nil {
		return 0, nil
	}
	return rec.CreditsConsumed, nil
}

// recordFailure 落一条 failed 账单：**credits 恒 0**（无 usage 不计费）。
func (s *Service) recordFailure(ctx context.Context, h *reservation, res magpie.Result, reason string) {
	s.recordFailureWith(ctx, h, res, reason)
}

func (s *Service) recordFailureWith(ctx context.Context, h *reservation, res magpie.Result, reason string) {
	req := billing.RecordReq{
		UserID:           res.UserID,
		Fail:             true,
		CallTime:         s.now(),
		ErrorMessage:     reason,
		ExternalModel:    res.ExternalModel,
		UpstreamProtocol: string(res.Protocol),
		UpstreamModel:    res.MagpieModel,
	}
	if h != nil {
		req.BillingID = h.billingID
		req.UserID = h.userID
		req.TokenID = tokenPtr(h.tokenID)
		req.SessionID = h.sessionID
		req.CallTime = h.reservedAt
		req.ExternalModel = h.extModel
		req.UpstreamProtocol = h.protocol
		req.Remark = "facade 转发失败（不计费）"
	} else {
		req.BillingID = s.newID()
		req.Remark = "facade 转发失败（未预扣）"
	}
	if _, err := s.ledger.Record(ctx, req); err != nil {
		s.logger.Error("失败账单落库失败", "billing_id", req.BillingID, "reason", reason, "err", err.Error())
	}
}

// refundAndClose 整笔退回预扣，并在**同一个事务**里把登记终结为 refunded（失败重试 3 次）。
//
// 为什么不能拆成「Settle + CloseHold」：Owl 复审 P1 实测过，两步之间失败或进程中断
// 会让 hold 停在 held，15 分钟后过期扫描再全额退一次（双份预扣）。
// 重试耗尽也不是灾难：hold 仍是 held，过期扫描会退（那次也是原子的），不会重复。
func (s *Service) refundAndClose(ctx context.Context, h *reservation, reason string) {
	if h.pre <= 0 {
		return
	}
	var last error
	for i := 0; i < 3; i++ {
		owned, err := s.wallet.SettleHold(ctx, identity.SettleHoldRequest{
			BillingID: h.billingID,
			UserID:    h.userID,
			Delta:     -h.pre,
			SessionID: h.sessionID,
			Remark:    "请求失败退回预扣（" + reason + "）",
			Status:    identity.HoldRefunded,
		})
		if err == nil {
			if owned {
				s.logger.Info("已退回预扣", "billing_id", h.billingID, "credits", h.pre, "reason", reason)
			} else {
				// 已被别的路径终结（例如过期扫描先到）：那笔钱不该再退，这里什么都不做。
				s.logger.Warn("预扣登记已被终结，跳过退款", "billing_id", h.billingID, "reason", reason)
			}
			return
		}
		last = err
		select {
		case <-ctx.Done():
			s.logger.Error("退回预扣未完成（上下文结束）", "billing_id", h.billingID, "credits", h.pre, "err", last.Error())
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	s.logger.Error("退回预扣失败（重试耗尽，靠过期扫描兜底）",
		"billing_id", h.billingID, "credits", h.pre, "err", last.Error())
}

// holdClose 生成「本单要在结算事务内终结的预扣登记」；免费模型（pre<=0）没有登记，返回 nil。
func (s *Service) holdClose(h *reservation, status string) *billing.HoldCloseReq {
	if h == nil || h.pre <= 0 {
		return nil
	}
	return &billing.HoldCloseReq{BillingID: h.billingID, Status: status, Closer: s.wallet}
}

// hasUsage 判定「有真实 usage」：HasUsage 为真且三段非全零。
// 全零与「没取到」同口径（`"usage":{}` 不许当成 0 token 出账）。
func hasUsage(res magpie.Result) bool {
	if !res.HasUsage {
		return false
	}
	return !(res.Usage.Input == 0 && res.Usage.Output == 0 && res.Usage.CacheRead == 0)
}

// reasonOf 把转发结果归因成失败原因（只在「不计费」分支使用）。
func reasonOf(res magpie.Result) string {
	switch {
	case res.Status >= 500:
		return ReasonUpstreamError
	case res.Status == 0 || res.Err != nil:
		if res.Canceled {
			return ReasonCanceled
		}
		return ReasonUpstreamError
	case res.Canceled:
		return ReasonCanceled
	default:
		return ReasonNoUsage
	}
}

// estimateInputTokens 按请求体字节数粗估输入 token（**只用于预扣**，4 字节 ≈ 1 token）。
//
// 明确不是出账口径：最终金额只认上游 usage；无 usage 时整笔退回（本包不提供估算出账路径）。
func estimateInputTokens(body []byte) int64 {
	if len(body) == 0 {
		return 0
	}
	n := int64(len(body) / 4)
	if n < 1 {
		n = 1
	}
	return n
}

// isInsufficient 判定「余额不足」：identity 侧用 *APIError + resp.CodeInsufficient 表达。
func isInsufficient(err error) bool {
	var apiErr *identity.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == resp.CodeInsufficient
	}
	return false
}

// newBillingID 生成账单号：bill-YYYYMMDD-<8 hex>。与 billing 包的格式同族，
// 唯一性由 billing_records 的主键/唯一索引兜底（撞号会重试换号，见 billing.Service）。
func newBillingID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("bill-%s-%08d", time.Now().Format("20060102"), idgen.New()%100000000)
	}
	return fmt.Sprintf("bill-%s-%x", time.Now().Format("20060102"), b)
}

func tokenPtr(id int64) *int64 {
	if id == 0 {
		return nil
	}
	v := id
	return &v
}

// StatusFor 把结果映射成账单状态词（供日志/审计对齐口径）：ok / failed:no-usage 等。
func StatusFor(res magpie.Result) string {
	if hasUsage(res) {
		return "ok"
	}
	return "failed:" + reasonOf(res)
}

// String 便于日志。
func (s *Service) String() string { return fmt.Sprintf("facadebill(ratio=%.2f)", s.ratio) }

var _ = strings.TrimSpace
