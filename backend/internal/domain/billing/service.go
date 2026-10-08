package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/pkg/dbx"
	"github.com/team/llmgateway/internal/pkg/idgen"
	"github.com/team/llmgateway/internal/pkg/money"
)

// 记账状态。
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// validateStoredMoney 校验金额在可存储范围内且非 NaN/Inf（实现迁移至 internal/pkg/money）。
func validateStoredMoney(v float64, what string) error {
	return money.Validate(v, what)
}

// Prices 常量（计费三配置在 sys_configs 中的 key）。
const (
	cfgR          = "billing.r"
	cfgTiers      = "billing.context_tiers"
	cfgTimeConfig = "billing.time_config"
	cfgCnyRate    = "billing.cny_rate"
)

// Store 提供 billing_records 的数据访问。由基于 *sql.DB 的 SqlStore 实现。
type Store interface {
	GetByBillingID(ctx context.Context, billingID string) (*Record, error)
	Insert(ctx context.Context, rec *Record) (*Record, error)
	// InsertTx 在给定执行器（事务）上插入账单，供 Record 把「扣款 + 积分流水 + 账单」放进同一事务。
	InsertTx(ctx context.Context, ex dbx.Execer, rec *Record) (*Record, error)
}

// Record 对应 billing_records 一行，携带一次调用的完整计费信息。
type Record struct {
	ID              int64
	BillingID       string
	UserID          int64
	PricingMode     string // "sale"/"cost"
	TokenID         *int64
	ExternalModel   string
	InternalModelID string
	ChannelKeyID    int64  // 落 channel_key_id：渠道密钥维度（运行时实体）
	SessionID       string // 落 session_id：请求携带 x-session-id 时的会话维度；失败记录同样携带
	SessionName     string // 落 session_name：写入时的会话可读名快照（sessions 投影过期清理后仍可展示）
	CallTime        time.Time
	Tokens          Usage
	Rates           Rates
	Coefficients    struct {
		Time    float64 `json:"time"`
		Context float64 `json:"context"`
	}
	RValue          int64
	RawTotal        float64 // 未除 R 的原始价值（对账展示）
	CreditsConsumed float64
	CostCredits     float64 // 成本模式下的成本积分（路由/差异展示用）
	BalanceBefore   float64
	BalanceAfter    float64
	Status          string
	ErrorMessage    string
	DurationMs      *int64 // 总耗时（毫秒）
	FirstTokenMs    *int64 // 首字耗时（毫秒）
}

// RecordReq 是 Record 的入参（由路由/网关在调用完成后传入）。
type RecordReq struct {
	BillingID       string
	UserID          int64
	PricingMode     string
	TokenID         *int64
	ExternalModel   string
	InternalModelID string
	ChannelKeyID    int64
	SessionID       string
	SessionName     string // 会话可读名快照（写入账单时固化，历史可查）
	CallTime        time.Time
	Tokens          Usage
	Rates           Rates // 计费单价（sale 或 cost，由调用方选好模式传入）
	CostRates       Rates // cost 模式下用于记录成本积分，可为 nil
	ErrorMessage    string
	DurationMs      *int64
	FirstTokenMs    *int64
	// Fail 为 true 时不扣积分、不计算公式，直接落一条 failed 记录（记录上游/下游失败原因）。
	Fail bool
	// 模型级覆盖（可空）。非空时本单次优先使用模型级时段/分档，
	// 否则回落全局 sys_configs（billing.time_config / billing.context_tiers）。
	TimeConfig   *TimeCoeffConfig
	ContextTiers []TierRule
	// PreConsumed 为本次请求已预扣冻结的积分：>0 时实际只增减差额（多退少补），
	// =0 时（健康探测/直接计费等无预扣场景）按全额扣减。
	PreConsumed float64
	Remark      string
}

// CreditService 抽象钱包扣减/差额结算，由 identity 包实现。
type CreditService interface {
	Consume(ctx context.Context, userID int64, amount float64, billingID, sessionID, remark string) (before, after float64, err error)
	Settle(ctx context.Context, userID int64, delta float64, sessionID, remark string) (before, after float64, err error)
	// Tx 变体：在调用方事务内完成扣减/差额结算与积分流水写入，供 Record 同事务提交。
	ConsumeTx(ctx context.Context, ex dbx.Execer, userID int64, amount float64, billingID, sessionID, remark string) (before, after float64, err error)
	SettleTx(ctx context.Context, ex dbx.Execer, userID int64, delta float64, sessionID, remark string) (before, after float64, err error)
}

// Service 计费服务：记账 + 钱包扣减 + 幂等。
type Service struct {
	store  Store
	cfg    ConfigProvider
	credit CreditService
	clock  func() time.Time
	retry  *RetryQueue    // 可空：落库失败时的本地持久重试队列
	txdb   dbx.TxBeginner // 可空：配置后「扣款 + 积分流水 + 账单」走单事务提交
}

// NewService 创建计费服务。
func NewService(store Store, cfg ConfigProvider, credit CreditService) *Service {
	return &Service{store: store, cfg: cfg, credit: credit, clock: time.Now}
}

// SetRetryQueue 注入落库失败时的本地持久重试队列；未注入时落库失败即丢失（仅告警）。
func (s *Service) SetRetryQueue(q *RetryQueue) { s.retry = q }

// SetTxBeginner 注入事务开启者（通常为 *sql.DB）：注入后「扣款 + 积分流水 + 账单」在同一 PostgreSQL
// 事务内提交，消灭「扣了钱没账单」；未注入（如单测）时退回非事务路径。
func (s *Service) SetTxBeginner(b dbx.TxBeginner) { s.txdb = b }

// Record 完成一次计费。
//
// 幂等语义：按 billing_id 单查（store.GetByBillingID），若命中已完成的记录直接返回
// （网关重试安全，不会重复扣积分）。并发安全说明：GetByBillingID 是单查询做快速路径，
// billing_records.billing_id 上的唯一索引作为最终兜底——即便两个并发都未命中历史记录，
// 也只会有一个把记录写库成功；另一个 Insert 撞唯一约束（23505）时重新查询该 billing_id
// 的 completed 记录并作为成功返回（见下方 isUniqueViolation 分支），积分侧由
// identity.Consume 的原子扣减保证不重复。
//
// 流程：读配置 → 时段系数 + 上下文分档 → ComputeCredits → credit.Consume →
// 成功插 completed、失败（如 40201 余额不足）插 failed 并透传原始错误。
func (s *Service) Record(ctx context.Context, req RecordReq) (*Record, error) {
	if req.BillingID == "" {
		req.BillingID = GenBillingID()
	}
	// call_time 兜底：未提供（如健康探测账单）时取当前时刻，禁止零值写库（页面显示 0001-01-01）。
	if req.CallTime.IsZero() {
		req.CallTime = s.clock()
	}

	// 失败记录快速通道（上游/下游失败不扣积分）：直接落 failed 并携带原因。
	if req.Fail {
		rec := &Record{
			BillingID:       req.BillingID,
			UserID:          req.UserID,
			PricingMode:     req.PricingMode,
			TokenID:         req.TokenID,
			ExternalModel:   req.ExternalModel,
			InternalModelID: req.InternalModelID,
			ChannelKeyID:    req.ChannelKeyID,
			SessionID:       req.SessionID,
			SessionName:     req.SessionName,
			CallTime:        req.CallTime,
			Tokens:          req.Tokens,
			Status:          StatusFailed,
			ErrorMessage:    req.ErrorMessage,
			DurationMs:      req.DurationMs,
			FirstTokenMs:    req.FirstTokenMs,
		}
		if _, err := s.store.Insert(ctx, rec); err != nil {
			if isUniqueViolation(err) {
				return rec, nil
			}
			s.enqueueRetry(rec, err)
			return nil, err
		}
		return rec, nil
	}

	// 1) 幂等快速路径。
	existing, err := s.store.GetByBillingID(ctx, req.BillingID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if existing != nil {
		if existing.Status == StatusCompleted {
			return existing, nil
		}
		// 历史 failed 记录：不重复扣减/插入（避免撞唯一索引），透传原失败原因。
		msg := existing.ErrorMessage
		if msg == "" {
			msg = "历史计费记录为失败状态"
		}
		return nil, errors.New(msg)
	}

	// 2) 读取配置与系数（模型级 time_config/context_tiers 非空则优先，否则回落全局）。
	credits, timeCoeff, ctxCoeff, r, err := s.requestCredits(req)
	if err != nil {
		return nil, err
	}
	rawTotal, err := RawTotal(req.Tokens, req.Rates, timeCoeff, ctxCoeff)
	if err != nil {
		// rates 已在上一步校验过，此处不会触发；防御性返回。
		return nil, err
	}
	if err := validateStoredMoney(rawTotal, "原始价值"); err != nil {
		return nil, err
	}

	rec := &Record{
		BillingID:       req.BillingID,
		UserID:          req.UserID,
		PricingMode:     req.PricingMode,
		TokenID:         req.TokenID,
		ExternalModel:   req.ExternalModel,
		InternalModelID: req.InternalModelID,
		ChannelKeyID:    req.ChannelKeyID,
		SessionID:       req.SessionID,
		SessionName:     req.SessionName,
		CallTime:        req.CallTime,
		Tokens:          req.Tokens,
		Rates:           req.Rates,
		Coefficients: struct {
			Time    float64 `json:"time"`
			Context float64 `json:"context"`
		}{Time: timeCoeff, Context: ctxCoeff},
		RValue:       r,
		RawTotal:     rawTotal,
		DurationMs:   req.DurationMs,
		FirstTokenMs: req.FirstTokenMs,
		ErrorMessage: req.ErrorMessage,
	}
	// 只要提供了成本单价就记录成本积分（仅展示/毛利差异，不参与扣减）：
	// sale 模式 cost_credits 反映真实成本（区别于售价积分的毛利段），
	// cost 模式与扣减积分一致（按成本计费）。
	if req.CostRates != nil {
		if c, cerr := ComputeCredits(req.Tokens, req.CostRates, timeCoeff, ctxCoeff, r); cerr == nil {
			rec.CostCredits = c
		}
	}

	// 3) 钱包扣减 + 账单落库（同一数据库事务：扣款、积分流水、账单要么全部成功要么全部回滚）。
	// 已预扣 → 只增减实际应付与预扣的差额（多退少补，不产生「结算退回」全额退款）；
	// 无预扣 → 按全额扣减。
	// 预扣（PreConsume/Refund）在候选循环之前执行、必须立即生效，不在此事务内。
	rec, err = s.settleLedger(ctx, rec, req, credits)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// settleLedger 完成「钱包扣减 + 积分流水 + 账单落库」：
//   - txdb 已注入：三者放进同一事务提交，扣款成功但账单写入失败时整体回滚（余额不变），
//     从根上消灭「扣了钱没账单」；
//   - txdb 为 nil（单测/未装配）：按原非事务路径逐步执行，扣款与落库独立。
func (s *Service) settleLedger(ctx context.Context, rec *Record, req RecordReq, credits float64) (*Record, error) {
	if s.txdb == nil {
		return s.settlePlain(ctx, rec, req, credits)
	}
	out, err, done := s.settleTx(ctx, rec, req, credits)
	if done {
		return out, err
	}
	// 事务在提交前失败（余额已随事务回滚、未扣款）：退回非事务路径，
	// 保留「账单不丢」的本地重试队列兜底，行为与引入事务前一致。
	return s.settlePlain(ctx, rec, req, credits)
}

// settleTx 事务路径：Begin → 扣款/结算（含积分流水）+ 账单 Insert → Commit。
// done=false 表示事务在提交前失败且非业务错误（账单写入失败/BeginTx 失败），调用方应退回非事务路径；
// done=true 表示结果已确定（成功、钱包业务失败已落 failed 账单、并发撞库、提交结果不确定）。
func (s *Service) settleTx(ctx context.Context, rec *Record, req RecordReq, credits float64) (out *Record, err error, done bool) {
	tx, berr := s.txdb.BeginTx(ctx, nil)
	if berr != nil {
		return nil, berr, false
	}
	defer tx.Rollback() //nolint:errcheck // Commit 成功后 Rollback 是 no-op

	before, after, err := s.consumeInTx(ctx, tx, rec, req, credits)
	if err != nil {
		// 扣款失败（40201 余额不足 / 40401 钱包缺失等）：先回滚，再在事务外落 failed 账单
		// （否则 failed 记录会随回滚一起丢掉），并透传原始错误（网关据此映射 402）。
		_ = tx.Rollback()
		out, err = s.insertFailedOutOfTx(ctx, rec, err)
		return out, err, true
	}
	rec.BalanceBefore = before
	rec.BalanceAfter = after
	rec.Status = StatusCompleted
	rec.CreditsConsumed = credits

	if _, ierr := s.store.InsertTx(ctx, tx, rec); ierr != nil {
		_ = tx.Rollback()
		// 并发竞态兜底：两个请求可能都通过幂等快速路径，billing_id 唯一索引保证其一写库成功、
		// 另一侧撞唯一约束（23505）。撞库方本次扣款已随事务回滚（未重复扣），重查已落库记录返回。
		if isUniqueViolation(ierr) {
			existing, gerr := s.store.GetByBillingID(ctx, rec.BillingID)
			if gerr == nil && existing != nil && existing.Status == StatusCompleted {
				return existing, nil, true
			}
			return nil, ierr, true
		}
		// 真实写入失败：本次扣款已回滚（余额不变），退回非事务路径重试；其失败由重试队列兜底。
		return nil, ierr, false
	}
	if cerr := tx.Commit(); cerr != nil {
		// 提交结果不确定（可能已提交）：不重投，避免重复扣款，直接报错。
		return nil, fmt.Errorf("commit billing tx: %w", cerr), true
	}
	return rec, nil, true
}

// settlePlain 非事务路径（txdb 未注入）：扣款与落库独立执行，语义与历史行为一致。
func (s *Service) settlePlain(ctx context.Context, rec *Record, req RecordReq, credits float64) (*Record, error) {
	before, after, err := s.consumeInTx(ctx, nil, rec, req, credits)
	if err != nil {
		return s.insertFailedOutOfTx(ctx, rec, err)
	}
	rec.Status = StatusCompleted
	rec.CreditsConsumed = credits
	rec.BalanceBefore = before
	rec.BalanceAfter = after
	if _, err := s.store.Insert(ctx, rec); err != nil {
		if isUniqueViolation(err) {
			if existing, gerr := s.store.GetByBillingID(ctx, rec.BillingID); gerr == nil &&
				existing != nil && existing.Status == StatusCompleted {
				return existing, nil
			}
			return nil, err
		}
		s.enqueueRetry(rec, err)
		return nil, err
	}
	return rec, nil
}

// consumeInTx 执行钱包扣减/差额结算：ex 为 nil 时走非事务版本（无 Tx 能力的 credit 桩）。
// 返回扣减前后的余额。
func (s *Service) consumeInTx(ctx context.Context, ex dbx.Execer, rec *Record, req RecordReq, credits float64) (before, after float64, err error) {
	if req.PreConsumed > 0 {
		delta := credits - req.PreConsumed
		if ex != nil {
			return s.credit.SettleTx(ctx, ex, req.UserID, delta, req.SessionID, "实际调用差额")
		}
		return s.credit.Settle(ctx, req.UserID, delta, req.SessionID, "实际调用差额")
	}
	if ex != nil {
		return s.credit.ConsumeTx(ctx, ex, req.UserID, credits, rec.BillingID, req.SessionID, "实际调用差额")
	}
	return s.credit.Consume(ctx, req.UserID, credits, rec.BillingID, req.SessionID, "实际调用差额")
}

// userMessageError 由业务错误实现：返回可直接落地展示的中文文案。
// 内部/基础设施错误不实现该接口，其细节只进日志，落库统一归一为通用中文文案。
type userMessageError interface{ UserMessage() string }

// failureMessage 把结算失败原因转为落库文案：业务错误用其中文文案，其余归一为通用提示。
func failureMessage(cause error) string {
	var um userMessageError
	if errors.As(cause, &um) {
		if s := strings.TrimSpace(um.UserMessage()); s != "" {
			return s
		}
	}
	return "计费结算失败"
}

// insertFailedOutOfTx 扣款失败路径：在事务之外落 failed 账单（避免被回滚丢弃），失败仍入重试队列。
// 内部错误细节只进日志，error_message 落库统一为中文业务文案。
func (s *Service) insertFailedOutOfTx(ctx context.Context, rec *Record, cause error) (*Record, error) {
	rec.Status = StatusFailed
	rec.ErrorMessage = failureMessage(cause)
	slog.ErrorContext(ctx, "billing settle failed", "billing_id", rec.BillingID, "user_id", rec.UserID, "err", cause)
	if _, ierr := s.store.Insert(ctx, rec); ierr != nil && !isUniqueViolation(ierr) {
		s.enqueueRetry(rec, ierr)
	}
	return nil, cause
}

// enqueueRetry 落库失败时把整条记录写入本地重试队列，避免丢失（后台周期重投）。
func (s *Service) enqueueRetry(rec *Record, cause error) {
	if s.retry == nil {
		slog.Error("billing insert failed and no retry queue configured, record lost",
			"billing_id", rec.BillingID, "err", cause)
		return
	}
	if err := s.retry.Enqueue(rec); err != nil {
		slog.Error("billing insert failed and retry enqueue failed, record lost",
			"billing_id", rec.BillingID, "cause", cause, "err", err)
		return
	}
	slog.Warn("billing insert failed, record queued for retry",
		"billing_id", rec.BillingID, "cause", cause)
}

// PendingRetry 返回待重投条数（启动日志/监控用）。
func (s *Service) PendingRetry() int { return s.retry.Pending() }

// FlushRetryQueue 重投本地队列中全部记录（启动时与后台周期调用）。
func (s *Service) FlushRetryQueue(ctx context.Context) (int, int, error) {
	if s.retry == nil {
		return 0, 0, nil
	}
	return s.retry.Flush(ctx, s.store)
}

// RunRetryQueue 后台周期重投本地账单队列，直到 ctx 结束。
func (s *Service) RunRetryQueue(ctx context.Context, interval time.Duration) {
	if s.retry == nil || interval <= 0 {
		return
	}
	// 启动先投一次：把上次进程退出前积压的记录尽快补落。
	s.flushRetryOnce(ctx)
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			s.flushRetryOnce(ctx)
		}
	}
}

// flushRetryOnce 重投一次：脱离取消信号并带超时，避免服务退出时中断重投。
func (s *Service) flushRetryOnce(ctx context.Context) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	ok, pending, err := s.FlushRetryQueue(fctx)
	switch {
	case err != nil:
		slog.Error("billing retry queue flush incomplete", "recovered", ok, "pending", pending, "err", err)
	case ok > 0:
		slog.Info("billing retry queue flushed", "recovered", ok)
	}
}

// requestCredits 解析一次请求的应付积分与系数（Record 结算与 EstimateCredits 预扣共用同一口径）：
// 模型级 time_config/context_tiers 非空则优先，否则回落全局；ctxCoeff 仅按输入 token 分档。
func (s *Service) requestCredits(req RecordReq) (credits, timeCoeff, ctxCoeff float64, r int64, err error) {
	r, err = s.cfg.R()
	if err != nil {
		return 0, 0, 0, 0, err
	}
	tiers := req.ContextTiers
	if len(tiers) == 0 {
		tiers, err = s.cfg.ContextTiers()
		if err != nil {
			return 0, 0, 0, 0, err
		}
	}
	timeCfg := TimeCoeffConfig{}
	if req.TimeConfig != nil {
		timeCfg = *req.TimeConfig
	} else {
		timeCfg, err = s.cfg.TimeConfig()
		if err != nil {
			return 0, 0, 0, 0, err
		}
	}
	timeCoeff, err = timeCfg.CoeffFor(s.clock())
	if err != nil {
		return 0, 0, 0, 0, err
	}
	ctxCoeff, err = ContextTierCoeff(req.Tokens.Input, tiers)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	credits, err = ComputeCredits(req.Tokens, req.Rates, timeCoeff, ctxCoeff, r)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if err := validateStoredMoney(credits, "计费积分"); err != nil {
		return 0, 0, 0, 0, err
	}
	return credits, timeCoeff, ctxCoeff, r, nil
}

// EstimateCredits 预扣金额估算：与 Record 使用完全相同的计费公式与系数
// （时段系数 × 上下文分档 × 全部倍率 ÷ R），以请求预估用量（输入 token 估算 + 输出按需传值）算出冻结额。
// 不落库、不扣费，供网关三层计费的 pre-consume 阶段使用。
func (s *Service) EstimateCredits(ctx context.Context, req RecordReq) (float64, error) {
	credits, _, _, _, err := s.EstimateBreakdown(ctx, req)
	return credits, err
}

// EstimateBreakdown 预扣金额估算与系数明细：与 Record/EstimateCredits 同一口径
// （时段 × 分档 × 全部倍率 ÷ R），额外返回本次估算实际采用的时段系数、分档系数与 R，
// 供网关在预扣不足落 failed 账单时渲染与正常结算账单一致的多行公式。不落库、不扣费。
func (s *Service) EstimateBreakdown(ctx context.Context, req RecordReq) (credits, timeCoeff, ctxCoeff float64, r int64, err error) {
	return s.requestCredits(req)
}

// isUniqueViolation 判断数据库错误是否为 billing_id 唯一索引冲突。
// 优先识别 Postgres 原生错误码 23505，同时兜底常见的驱动报错文案，保证在
// sqlmock / 其它驱动下也可稳定判定。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "23505") ||
		strings.Contains(s, "duplicate key") ||
		strings.Contains(s, "already exists")
}

// GenBillingID 生成计费流水号 bill-YYYYMMDD-xxxxxx。
func GenBillingID() string {
	return fmt.Sprintf("bill-%s-%06d", time.Now().Format("20060102"), rand.Intn(1000000))
}

// ===== ConfigProvider =====

// configSource 提供 sys_configs 单键读取。
type configSource interface {
	GetConfig(ctx context.Context, key string) ([]byte, error)
}

// ConfigProvider 读取计费系统配置。
type ConfigProvider interface {
	R() (int64, error)
	CnyRate() (float64, error)
	ContextTiers() ([]TierRule, error)
	TimeConfig() (TimeCoeffConfig, error)
}

// defaultConfigProvider 基于 sys_configs 的配置提供方。
// Task 8 控制台改动后经 Store 更新即可（本阶段直查，无内存缓存）。
type defaultConfigProvider struct {
	src configSource
}

// NewConfigProvider 创建基于 sys_configs 的配置提供方。
func NewConfigProvider(src configSource) ConfigProvider {
	return &defaultConfigProvider{src: src}
}

// R 读取 billing.r（积分换算系数）。兼容数字（10000）与字符串（"10000"）两种 JSON 存储。
// 采用无 ctx 签名，配置读少且只读，暂用 Background ctx（保留取消传播可后续扩展）。
func (p *defaultConfigProvider) R() (int64, error) {
	raw, err := p.src.GetConfig(context.Background(), cfgR)
	if err != nil {
		return 0, err
	}
	return parseR(raw)
}

func parseR(raw []byte) (int64, error) {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		v, perr := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if perr != nil {
			return 0, fmt.Errorf("billing.r 非法: %w", perr)
		}
		return v, nil
	}
	return 0, errors.New("billing.r 配置无法解析为整数")
}

// CnyRate 读取 billing.cny_rate（USD/CNY 汇率，1 USD = cny_rate CNY，仅用于 models.dev 美元参考价 → 人民币倍率折算）。兼容数字（6.8）与字符串（"6.8"）两种 JSON 存储。
func (p *defaultConfigProvider) CnyRate() (float64, error) {
	raw, err := p.src.GetConfig(context.Background(), cfgCnyRate)
	if err != nil {
		return 0, err
	}
	return parseFloatConfig(raw)
}

// parseFloatConfig 将 sys_configs 值解析为浮点数，兼容 JSON 数字与 JSON 字符串两种存储格式。
func parseFloatConfig(raw []byte) (float64, error) {
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		v, perr := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if perr != nil {
			return 0, fmt.Errorf("billing 浮点配置非法: %w", perr)
		}
		return v, nil
	}
	return 0, errors.New("billing 浮点配置无法解析")
}

// ContextTiers 读取 billing.context_tiers。
func (p *defaultConfigProvider) ContextTiers() ([]TierRule, error) {
	raw, err := p.src.GetConfig(context.Background(), cfgTiers)
	if err != nil {
		return nil, err
	}
	var tiers []TierRule
	if err := json.Unmarshal(raw, &tiers); err != nil {
		return nil, fmt.Errorf("解析 context_tiers: %w", err)
	}
	return tiers, nil
}

// TimeConfig 读取 billing.time_config。
func (p *defaultConfigProvider) TimeConfig() (TimeCoeffConfig, error) {
	raw, err := p.src.GetConfig(context.Background(), cfgTimeConfig)
	if err != nil {
		return TimeCoeffConfig{}, err
	}
	var tc TimeCoeffConfig
	if err := json.Unmarshal(raw, &tc); err != nil {
		return TimeCoeffConfig{}, fmt.Errorf("解析 time_config: %w", err)
	}
	return tc, nil
}

// ===== SqlStore（billing_records + sys_configs）=====

// SqlStore 是存储层实现，同时满足 Store（billing_records）与 configSource（sys_configs）。
type SqlStore struct {
	db *sql.DB
}

// NewSqlStore 创建基于 *sql.DB 的存储实现。
func NewSqlStore(db *sql.DB) *SqlStore {
	return &SqlStore{db: db}
}

// recordCols 查询 billing_records 使用的全部列。
const recordCols = `id, billing_id, user_id, pricing_mode, token_id, external_model_name,
	internal_model_id, channel_key_id, session_id, session_name, call_time, tokens, rates, coefficients, r_value,
	raw_total, credits_consumed, cost_credits, balance_before, balance_after, status, error_message, duration_ms, first_token_ms`

// GetConfig 读取 sys_configs 单键值；key 不存在返回错误。
func (s *SqlStore) GetConfig(ctx context.Context, key string) ([]byte, error) {
	var v []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM sys_configs WHERE key=$1`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("系统配置 %q 不存在", key)
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

// GetByBillingID 按 billing_id 查询；无记录返回 sql.ErrNoRows。
func (s *SqlStore) GetByBillingID(ctx context.Context, billingID string) (*Record, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+recordCols+` FROM billing_records WHERE billing_id=$1`, billingID)
	rec, err := sqlScan(row)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// Insert 插入一条计费记录（billing_id 唯一约束冲突返回错误）。
func (s *SqlStore) Insert(ctx context.Context, rec *Record) (*Record, error) {
	return s.insertOn(ctx, s.db, rec)
}

// InsertTx 在给定执行器（事务）上插入一条计费记录，供 Record 与钱包扣减同事务提交。
func (s *SqlStore) InsertTx(ctx context.Context, ex dbx.Execer, rec *Record) (*Record, error) {
	return s.insertOn(ctx, ex, rec)
}

func (s *SqlStore) insertOn(ctx context.Context, ex dbx.Execer, rec *Record) (*Record, error) {
	// 不吞序列化错误：tokens/rates/coefficients 为 JSONB NOT NULL 列，
	// 静默写入 nil 会变成 SQL NULL 而触发约束失败（例如 Rates 含 NaN/Inf 时）。
	coeffRaw, err := json.Marshal(rec.Coefficients)
	if err != nil {
		return nil, fmt.Errorf("marshal coefficients: %w", err)
	}
	tokensRaw, err := json.Marshal(rec.Tokens)
	if err != nil {
		return nil, fmt.Errorf("marshal tokens: %w", err)
	}
	ratesRaw, err := json.Marshal(rec.Rates)
	if err != nil {
		return nil, fmt.Errorf("marshal rates: %w", err)
	}

	var tokenID any
	if rec.TokenID != nil {
		tokenID = *rec.TokenID
	}
	// 未进入渠道的拒绝类账单 channel_key_id 为 0 → 写 NULL（列可空，0 不满足外键）。
	var channelKeyID any
	if rec.ChannelKeyID > 0 {
		channelKeyID = rec.ChannelKeyID
	}
	var sessionID any
	if rec.SessionID != "" {
		sessionID = rec.SessionID
	}
	// session_name 为 NOT NULL：空会话名写空串而非 NULL（与列默认值语义一致）。
	sessionName := any(rec.SessionName)
	var errMsg any
	if rec.ErrorMessage != "" {
		errMsg = rec.ErrorMessage
	}
	durationMs := nullableInt64For(rec.DurationMs)
	firstTokenMs := nullableInt64For(rec.FirstTokenMs)

	var created time.Time
	if rec.ID == 0 {
		rec.ID = idgen.New()
	}
	err = ex.QueryRowContext(ctx,
		`INSERT INTO billing_records(id, billing_id, user_id, pricing_mode, token_id, external_model_name,
			internal_model_id, channel_key_id, session_id, session_name, call_time, tokens, rates, coefficients, r_value,
			raw_total, credits_consumed, cost_credits, balance_before, balance_after, status, error_message, duration_ms, first_token_ms)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)
		 RETURNING id, created_at`,
		rec.ID, rec.BillingID, rec.UserID, rec.PricingMode, tokenID, rec.ExternalModel,
		rec.InternalModelID, channelKeyID, sessionID, sessionName, rec.CallTime, tokensRaw, ratesRaw, coeffRaw,
		rec.RValue, rec.RawTotal, rec.CreditsConsumed, rec.CostCredits,
		rec.BalanceBefore, rec.BalanceAfter, rec.Status, errMsg, durationMs, firstTokenMs).
		Scan(&rec.ID, &created)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// nullableInt64For 把 *int64 转为可入库的 any（nil → nil）。
func nullableInt64For(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// sqlScan 扫描 billing_records 行到 *Record。
func sqlScan(row interface{ Scan(...any) error }) (*Record, error) {
	var rec Record
	var tokenID sql.NullInt64
	var channelKeyID sql.NullInt64
	var sessionID sql.NullString
	var sessionName sql.NullString
	var rawTotal, consCred, costCred, balBefore, balAfter sql.NullFloat64
	var errMsg sql.NullString
	var durationMs, firstTokenMs sql.NullInt64
	var tokensRaw, ratesRaw, coeffRaw []byte
	var callTime time.Time

	err := row.Scan(&rec.ID, &rec.BillingID, &rec.UserID, &rec.PricingMode, &tokenID,
		&rec.ExternalModel, &rec.InternalModelID, &channelKeyID, &sessionID, &sessionName, &callTime,
		&tokensRaw, &ratesRaw, &coeffRaw, &rec.RValue,
		&rawTotal, &consCred, &costCred, &balBefore, &balAfter, &rec.Status, &errMsg,
		&durationMs, &firstTokenMs)
	if err != nil {
		return nil, err
	}
	if tokenID.Valid {
		v := tokenID.Int64
		rec.TokenID = &v
	}
	if channelKeyID.Valid {
		rec.ChannelKeyID = channelKeyID.Int64
	}
	if rawTotal.Valid {
		rec.RawTotal = rawTotal.Float64
	}
	if consCred.Valid {
		rec.CreditsConsumed = consCred.Float64
	}
	if costCred.Valid {
		rec.CostCredits = costCred.Float64
	}
	if balBefore.Valid {
		rec.BalanceBefore = balBefore.Float64
	}
	if balAfter.Valid {
		rec.BalanceAfter = balAfter.Float64
	}
	if durationMs.Valid {
		v := durationMs.Int64
		rec.DurationMs = &v
	}
	if firstTokenMs.Valid {
		v := firstTokenMs.Int64
		rec.FirstTokenMs = &v
	}
	rec.ErrorMessage = errMsg.String
	rec.SessionID = sessionID.String
	rec.SessionName = sessionName.String
	rec.CallTime = callTime
	if len(tokensRaw) > 0 {
		_ = json.Unmarshal(tokensRaw, &rec.Tokens)
	}
	if len(ratesRaw) > 0 {
		_ = json.Unmarshal(ratesRaw, &rec.Rates)
	}
	if len(coeffRaw) > 0 {
		_ = json.Unmarshal(coeffRaw, &rec.Coefficients)
	}
	return &rec, nil
}
