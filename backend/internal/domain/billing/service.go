package billing

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/team/llmgateway/internal/pkg/dbx"
	"github.com/team/llmgateway/internal/pkg/idgen"
	"github.com/team/llmgateway/internal/pkg/money"
)

// 记账状态（值域与 0025 的 ck_billing_status 逐字一致）。
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	// StatusUpstreamError 表示上游 4xx/5xx / 传输失败 / 超时。
	// 与 StatusFailed 的区别只在「为什么没计费」：两者都可以是金额 0，
	// 但 upstream_error 要在账单上保留上游状态码与错误体摘要，供运营分派。
	StatusUpstreamError = "upstream_error"
)

// ValidStatus 判断状态是否在 0025 的值域内。
func ValidStatus(status string) bool {
	switch status {
	case StatusCompleted, StatusFailed, StatusUpstreamError:
		return true
	default:
		return false
	}
}

// upstreamErrorSummaryMaxBytes 是上游错误体摘要的落库上限（与网关侧 capture 上限同量级）。
const upstreamErrorSummaryMaxBytes = 512

// statusOrCompleted 把空状态归一成 completed（非空且非法由调用方先校验）。
func statusOrCompleted(status string) string {
	if status == "" {
		return StatusCompleted
	}
	return status
}

// truncateUpstreamSummary 做写入侧最后一道兜底：非法 UTF-8 → U+FFFD，按 UTF-8 边界截到 512B。
// 调用方（网关）已经遮蔽过凭据；这里不重复遮蔽，避免把正常文本误伤成 ***。
func truncateUpstreamSummary(s string) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "�")
	if len(s) <= upstreamErrorSummaryMaxBytes {
		return s
	}
	cut := upstreamErrorSummaryMaxBytes
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}

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
	cfgCostSample = "routing.cost_sample"
	// cfgSampleProfile 是路由样值补齐比例（缓存读/纯输入、输出/纯输入）。
	cfgSampleProfile = "routing.sample_profile"
)

// SampleProfile 是 routing.sample_profile 的解析结果：两个「相对纯输入」的比例，
// 用于把「只有输入段（缓存读/输出未知）」的样值补齐为三段（供会话成本路由在路由时刻
// 无缓存读/输出实测值时估算真实规模）。口径：
//   - CacheReadRatio = 缓存读 token / 纯输入 token（默认 0.9，与 routing.cost_sample 默认样值同源）；
//   - OutputRatio    = 输出 token / 纯输入 token（默认 0.1，同上）。
//
// 三段口径：纯输入 + 缓存读 = 总输入（prompt）。故给定总输入 P 时，纯输入 = P / (1 + CacheReadRatio)。
type SampleProfile struct {
	CacheReadRatio float64 `json:"cache_read_ratio"`
	OutputRatio    float64 `json:"output_ratio"`
}

// defaultSampleProfile 缺省比例（与 routing.cost_sample 默认样值 10000/9000/1000 同源）。
func defaultSampleProfile() SampleProfile {
	return SampleProfile{CacheReadRatio: 0.9, OutputRatio: 0.1}
}

// parseSampleProfile 解析 routing.sample_profile：字段可缺省（用默认比例补齐）；JSON 非法返回错误。
func parseSampleProfile(raw []byte) (SampleProfile, error) {
	var in struct {
		CacheReadRatio *float64 `json:"cache_read_ratio"`
		OutputRatio    *float64 `json:"output_ratio"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return SampleProfile{}, fmt.Errorf("解析 routing.sample_profile: %w", err)
	}
	prof := defaultSampleProfile()
	if in.CacheReadRatio != nil {
		prof.CacheReadRatio = *in.CacheReadRatio
	}
	if in.OutputRatio != nil {
		prof.OutputRatio = *in.OutputRatio
	}
	return prof, nil
}

// DefaultSampleProfile 返回缺省补齐比例（导出给控制台面板，保证与路由口径同源）。
func DefaultSampleProfile() SampleProfile { return defaultSampleProfile() }

// ParseSampleProfile 解析 routing.sample_profile 原始 JSON（导出给控制台面板，缺省字段用默认比例补齐）。
func ParseSampleProfile(raw []byte) (SampleProfile, error) { return parseSampleProfile(raw) }

// Store 提供 billing_records 的数据访问。由基于 *sql.DB 的 SqlStore 实现。
type Store interface {
	GetByBillingID(ctx context.Context, billingID string) (*Record, error)
	Insert(ctx context.Context, rec *Record) (*Record, error)
	// InsertTx 在给定执行器（事务）上插入账单，供 Record 把「扣款 + 积分流水 + 账单」放进同一事务。
	InsertTx(ctx context.Context, ex dbx.Execer, rec *Record) (*Record, error)
}

// idempotencyReader 是 SqlStore 提供的「只读本表、不回填展示维度」的读取能力。
// 写路径的幂等预检查只关心「是否已存在、状态如何」，不该为展示维度多付一次 call_logs 查询。
type idempotencyReader interface {
	scanByBillingID(ctx context.Context, billingID string) (*Record, error)
}

// recordByBillingID 幂等预检查用：优先走不带展示维度的读取，其他 Store 实现回落 GetByBillingID。
func (s *Service) recordByBillingID(ctx context.Context, billingID string) (*Record, error) {
	if reader, ok := s.store.(idempotencyReader); ok {
		return reader.scanByBillingID(ctx, billingID)
	}
	return s.store.GetByBillingID(ctx, billingID)
}

// Record 对应 billing_records 一行，携带一次调用的完整计费信息。
type Record struct {
	ID              int64
	BillingID       string
	UserID          int64
	PricingMode     string // "sale"/"cost"
	TokenID         *int64
	ExternalModel   string
	AutoModelName   string // 请求的 Auto 模型名（非 Auto 请求为空，落库为 NULL）
	InternalModelID string
	ChannelKeyID    int64  // 渠道密钥维度（运行时实体）：不再落 billing_records，读取时由 call_logs 快照回填
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
	// UpstreamStatus 是上游 HTTP 状态码（NULL/0 = 没收到响应）；仅 upstream_error 行有值。
	UpstreamStatus *int
	// UpstreamErrorSummary 是上游错误体摘要（已遮蔽凭据、折叠空白、截断 512B）。
	UpstreamErrorSummary string
	DurationMs           *int64 // 总耗时（毫秒）
	FirstTokenMs         *int64 // 首字耗时（毫秒）
	// RequestID 网关请求链路 ID：写入侧落 billing_records.request_id（V2 归因列，空写 NULL），
	// 列表查询侧仍按既有口径从 call_logs 归并（两者同值；统一读取口径属后续切片）。
	// 让一次请求在账单可被直接定位（多候选尝试共享同一 request_id）。
	RequestID string
	// ===== V2 归因列（迁移由架构侧 ADD COLUMN；缺失留 NULL，不猜）=====
	// UpstreamProvider / UpstreamAccount 来自上游回程头里的供应商与账号标识。
	UpstreamProvider string
	UpstreamAccount  string
	// UpstreamProtocol 上游协议，取值域见 upstream.AttributionProtocols()。
	UpstreamProtocol string
	// UpstreamModel 上游真实模型名。
	UpstreamModel string
	// LedgerEntryID 匹配到的账本条目 id（阶段一没有匹配逻辑 → 恒空 → NULL）。
	LedgerEntryID string
	// CostSource 成本来源：ledger / amortized / none（空按 none 落库）。
	CostSource string
	// Attempts 该请求共产生的账单条数（仅归并列表查询时填充；1=单次尝试，>1=发生过候选/上游重试）。
	Attempts int
}

// RecordReq 是 Record 的入参（由路由/网关在调用完成后传入）。
type RecordReq struct {
	BillingID       string
	UserID          int64
	PricingMode     string
	TokenID         *int64
	ExternalModel   string
	AutoModelName   string // 请求的 Auto 模型名（非 Auto 请求为空）；仅留痕，不参与定价
	InternalModelID string
	ChannelKeyID    int64
	SessionID       string
	SessionName     string // 会话可读名快照（写入账单时固化，历史可查）
	CallTime        time.Time
	Tokens          Usage
	Rates           Rates // 计费单价（sale 或 cost，由调用方选好模式传入）
	CostRates       Rates // cost 模式下用于记录成本积分，可为 nil
	ErrorMessage    string
	// Status 为空时按 Fail 推导（Fail=true → failed，否则 completed）。
	// upstream_error 由调用方显式传入（上游 4xx/5xx / 传输失败）。
	Status string
	// UpstreamStatus 是上游 HTTP 状态码（0 表示没收到响应，落 NULL）。
	UpstreamStatus *int
	// UpstreamErrorSummary 是上游错误体摘要（调用方负责遮蔽/截断；写入侧再截一次兜底）。
	UpstreamErrorSummary string
	DurationMs           *int64
	FirstTokenMs         *int64
	// Fail 为 true 时不扣积分、不计算公式，直接落一条失败记录（记录上游/下游失败原因）。
	Fail bool
	// 模型级覆盖（可空）。非空时本单次优先使用模型级时段/分档，
	// 否则回落全局 sys_configs（billing.time_config / billing.context_tiers）。
	TimeConfig   *TimeCoeffConfig
	ContextTiers []TierRule
	Remark       string
	// ===== V2 归因（由网关在调用完成后传入；未接线时全空 → 落 NULL / none）=====
	RequestID        string
	UpstreamProvider string
	UpstreamAccount  string
	UpstreamProtocol string
	UpstreamModel    string
	LedgerEntryID    string
	CostSource       string
}

// CreditService 抽象钱包扣减/差额结算，由 identity 包实现。
type CreditService interface {
	Consume(ctx context.Context, userID int64, amount float64, billingID, sessionID, remark string) (before, after float64, err error)
	// Tx 变体：在调用方事务内完成扣减与积分流水写入，供 Record 同事务提交。
	ConsumeTx(ctx context.Context, ex dbx.Execer, userID int64, amount float64, billingID, sessionID, remark string) (before, after float64, err error)
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
	if req.Status != "" && !ValidStatus(req.Status) {
		return nil, fmt.Errorf("非法账单状态 %q（只允许 completed/failed/upstream_error）", req.Status)
	}

	// 失败记录快速通道（上游/下游失败不扣积分）：直接落失败状态并携带原因。
	if req.Fail {
		status := req.Status
		if status == "" {
			status = StatusFailed
		}
		if !ValidStatus(status) {
			return nil, fmt.Errorf("非法账单状态 %q（只允许 completed/failed/upstream_error）", status)
		}
		rec := &Record{
			BillingID:            req.BillingID,
			UserID:               req.UserID,
			PricingMode:          req.PricingMode,
			TokenID:              req.TokenID,
			ExternalModel:        req.ExternalModel,
			AutoModelName:        req.AutoModelName,
			InternalModelID:      req.InternalModelID,
			ChannelKeyID:         req.ChannelKeyID,
			SessionID:            req.SessionID,
			SessionName:          req.SessionName,
			CallTime:             req.CallTime,
			Tokens:               req.Tokens,
			Status:               status,
			ErrorMessage:         req.ErrorMessage,
			UpstreamStatus:       req.UpstreamStatus,
			UpstreamErrorSummary: truncateUpstreamSummary(req.UpstreamErrorSummary),
			DurationMs:           req.DurationMs,
			FirstTokenMs:         req.FirstTokenMs,
			RequestID:            req.RequestID,
			UpstreamProvider:     req.UpstreamProvider,
			UpstreamAccount:      req.UpstreamAccount,
			UpstreamProtocol:     req.UpstreamProtocol,
			UpstreamModel:        req.UpstreamModel,
			LedgerEntryID:        req.LedgerEntryID,
			CostSource:           req.CostSource,
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
	existing, err := s.recordByBillingID(ctx, req.BillingID)
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
		AutoModelName:   req.AutoModelName,
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
		RValue:               r,
		RawTotal:             rawTotal,
		DurationMs:           req.DurationMs,
		FirstTokenMs:         req.FirstTokenMs,
		ErrorMessage:         req.ErrorMessage,
		Status:               statusOrCompleted(req.Status),
		UpstreamStatus:       req.UpstreamStatus,
		UpstreamErrorSummary: truncateUpstreamSummary(req.UpstreamErrorSummary),

		RequestID:        req.RequestID,
		UpstreamProvider: req.UpstreamProvider,
		UpstreamAccount:  req.UpstreamAccount,
		UpstreamProtocol: req.UpstreamProtocol,
		UpstreamModel:    req.UpstreamModel,
		LedgerEntryID:    req.LedgerEntryID,
		CostSource:       req.CostSource,
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
	// V2 的预扣差额结算已下线，现存调用方一律按全额扣减。
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
		// ⚠️ 资金底线（2026-11-15 复审 R3）：事务开不起来时**绝不退回非事务路径**——
		// 那条路是「先 Consume 真扣钱、再 Insert 落账」，事务不可用往往伴随库不可用，
		// 于是钱扣了、账单写不进去（重试队列对确定性错误永远重试不成功）。
		// 取舍：宁可不成交（本次不扣款、不落账，请求报错），也不出现「扣了钱没账」。
		// 这里不重试 BeginTx：连接层问题由上层请求重试/连接池负责，账单侧重试没有意义。
		slog.ErrorContext(ctx, "billing tx begin failed, abort without charging",
			"billing_id", rec.BillingID, "user_id", rec.UserID, "err", berr)
		return nil, fmt.Errorf("begin billing tx: %w", berr), true
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
			existing, gerr := s.recordByBillingID(ctx, rec.BillingID)
			if gerr == nil && existing != nil && existing.Status == StatusCompleted {
				return existing, nil, true
			}
			return nil, ierr, true
		}
		// 非「可重投」类写入失败（约束/非法值等确定性错误）：事务已回滚、余额不变，
		// **不许**退回非事务路径——那条路会先真扣钱再落库，一失败就变成「钱扣了账没落」。
		// 交回调用方按失败处理（含重试队列），不再二次扣款。
		if isDeterministicWriteError(ierr) {
			slog.ErrorContext(ctx, "billing insert rejected, no retry on non-transactional path",
				"billing_id", rec.BillingID, "user_id", rec.UserID, "err", ierr)
			return nil, ierr, true
		}
		// 其它写入失败（连接/超时等）：本次扣款已回滚（余额不变），退回非事务路径重试；
		// 其失败由重试队列兜底。
		return nil, ierr, false
	}
	if cerr := tx.Commit(); cerr != nil {
		// 提交结果不确定（可能已提交）：不重投，避免重复扣款，直接报错。
		return nil, fmt.Errorf("commit billing tx: %w", cerr), true
	}
	return rec, nil, true
}

// isDeterministicWriteError 判断账单写入失败是否属于「重投也没用」的确定性错误：
// 约束冲突（23514 CHECK / 23503 外键 / 23502 NOT NULL / 22xxx 数据类型）与一切非连接类错误。
//
// 为什么单独判：事务路径回滚后有一条「退回非事务路径」的保底，那条路会**重新扣一次钱**。
// 对确定性错误重投必然再失败，等于把「扣了钱没账单」做实（预审 P1 资金事故）。
func isDeterministicWriteError(err error) bool {
	if err == nil {
		return false
	}
	// 连接类（可重投）：坏连接 / 连接已关 / 上下文取消超时 / PG 的 08 连接异常、57 运维中断、53 资源不足。
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case strings.HasPrefix(pgErr.Code, "08"),
			strings.HasPrefix(pgErr.Code, "57"),
			strings.HasPrefix(pgErr.Code, "53"):
			return false
		}
	}
	// 其余（约束冲突 23xxx、数据异常 22xxx、编码/参数/校验类）：确定性错误，重投必然再失败。
	return true
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
			if existing, gerr := s.recordByBillingID(ctx, rec.BillingID); gerr == nil &&
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

// consumeInTx 执行钱包扣减：ex 为 nil 时走非事务版本（无 Tx 能力的 credit 桩）。
// 返回扣减前后的余额。
//
// V2 的预扣（preauth）差额结算（PreConsumed/Hold 管线）已随逐请求代理链下线：
// 现存所有调用方（V3 账本轮询、对账补偿、探测计费）都无预扣，一律按全额扣减。
func (s *Service) consumeInTx(ctx context.Context, ex dbx.Execer, rec *Record, req RecordReq, credits float64) (before, after float64, err error) {
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

// requestCredits 解析一次请求的应付积分与系数（Record 结算的唯一计价口径）：
// 模型级 time_config/context_tiers 非空则优先，否则回落全局；
// ctxCoeff 按总输入 token（Input + CacheRead）分档——分档语义是「上下文长度」，
// 三段口径下 Input 只是纯输入，必须加上缓存读才是本次请求实际处理的上下文规模。
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
	ctxCoeff, err = ContextTierCoeff(req.Tokens.Input+req.Tokens.CacheRead, tiers)
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
	// SampleUsage 读取路由成本比较样值用量（routing.cost_sample）。
	SampleUsage() (Usage, error)
	// SampleProfile 读取样值补齐比例（routing.sample_profile）：路由时刻只有输入段时，
	// 按该比例估算缓存读/输出，补齐为三段（会话成本路由用）。缺失/解析失败返回错误。
	SampleProfile() (SampleProfile, error)
	// InvalidateCostConfig 清空配置相关的内存缓存（样值 + 全局时段 + R + 上下文分档）。
	// 由装配层在 sys_configs 写入后调用（见 SqlStore.SetConfigWriteHook），保证配置修改即时生效。
	InvalidateCostConfig()
	// SetCacheEnabled 开关配置读缓存（默认开启）；关闭后退回直查 DB（直查 fallback 安全开关）。
	SetCacheEnabled(enabled bool)
}

// defaultConfigProvider 基于 sys_configs 的配置提供方。
//
// 缓存：TimeConfig（全局时段）、SampleUsage（路由成本样值）、R（积分换算系数）与
// ContextTiers（上下文分档）会缓存在内存中，避免网关/路由热路径每次直查 sys_configs
// （F-R3-1，NFR-1）。失效仅由装配层在 sys_configs 写入后经 InvalidateCostConfig 触发；
// 其余配置（汇率）不缓存，保持原语义。读取失败不写入缓存（瞬时故障可自愈）。
//
// 失效代际（对齐 pkg/gencache 的 Generation/PutAtGen 语义）：InvalidateCostConfig 在清空
// 四个缓存前先递增 gen；读路径回源前捕获 gen，回填时仅当 gen 未变才写入。这样「回源期间
// 发生失效」的并发场景下陈旧回填被丢弃，避免旧配置（如 R，影响计费分母）无限期滞留（无 TTL）。
type defaultConfigProvider struct {
	src configSource

	mu      sync.RWMutex
	sample  *Usage           // 缓存的路由成本样值（nil=未缓存）
	profile *SampleProfile   // 缓存的样值补齐比例（nil=未缓存）
	timeCfg *TimeCoeffConfig // 缓存的全局时段配置（nil=未缓存）
	r       *int64           // 缓存的 billing.r（nil=未缓存）
	tiers   *[]TierRule      // 缓存的 billing.context_tiers（nil=未缓存）
	// gen 为失效代际：InvalidateCostConfig 递增；读路径回源前捕获、回填前校验（见上方说明）。
	gen uint64
	// cacheOff 为 true 时关闭全部配置缓存（安全态：直查 DB）。仅启动装配阶段设置，运行期不变。
	cacheOff bool
}

// NewConfigProvider 创建基于 sys_configs 的配置提供方。
func NewConfigProvider(src configSource) ConfigProvider {
	return &defaultConfigProvider{src: src}
}

// SetCacheEnabled 开关配置读缓存（默认开启）；关闭后各配置读取直查 DB（直查 fallback 安全开关）。
func (p *defaultConfigProvider) SetCacheEnabled(on bool) { p.cacheOff = !on }

// InvalidateCostConfig 清空全部配置缓存（路由成本样值、全局时段、R、上下文分档）。
// 由装配层在 sys_configs 写入后触发（见 SqlStore.SetConfigWriteHook），保证配置修改即时生效（无 TTL）。
// 先递增失效代际再清空：并发的读路径可能正要回填同一配置，代际校验会拒绝其陈旧回填（见 defaultConfigProvider 说明）。
func (p *defaultConfigProvider) InvalidateCostConfig() {
	p.mu.Lock()
	p.gen++
	p.sample = nil
	p.profile = nil
	p.timeCfg = nil
	p.r = nil
	p.tiers = nil
	p.mu.Unlock()
}

// R 读取 billing.r（积分换算系数）。兼容数字（10000）与字符串（"10000"）两种 JSON 存储。
// 带内存缓存（见 defaultConfigProvider 缓存说明）：命中直接返回，零 DB 查询；失败不写缓存。
// 采用无 ctx 签名，配置读少且只读，暂用 Background ctx（保留取消传播可后续扩展）。
func (p *defaultConfigProvider) R() (int64, error) {
	var gen uint64
	if !p.cacheOff {
		p.mu.RLock()
		cached := p.r
		gen = p.gen
		p.mu.RUnlock()
		if cached != nil {
			return *cached, nil
		}
	}
	raw, err := p.src.GetConfig(context.Background(), cfgR)
	if err != nil {
		return 0, err
	}
	v, err := parseR(raw)
	if err != nil {
		return 0, err
	}
	if !p.cacheOff {
		p.mu.Lock()
		// 仅当回源期间未发生失效（代际未变）才回填：避免失效前读到的旧值滞留（无 TTL）。
		if p.gen == gen {
			p.r = &v
		}
		p.mu.Unlock()
	}
	return v, nil
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

// ContextTiers 读取 billing.context_tiers（带内存缓存，见 defaultConfigProvider 缓存说明）。
// 命中直接返回，零 DB 查询；失败不写缓存。
func (p *defaultConfigProvider) ContextTiers() ([]TierRule, error) {
	var gen uint64
	if !p.cacheOff {
		p.mu.RLock()
		cached := p.tiers
		gen = p.gen
		p.mu.RUnlock()
		if cached != nil {
			return *cached, nil
		}
	}
	raw, err := p.src.GetConfig(context.Background(), cfgTiers)
	if err != nil {
		return nil, err
	}
	var tiers []TierRule
	if err := json.Unmarshal(raw, &tiers); err != nil {
		return nil, fmt.Errorf("解析 context_tiers: %w", err)
	}
	if !p.cacheOff {
		p.mu.Lock()
		// 仅当回源期间未发生失效（代际未变）才回填（见 defaultConfigProvider 说明）。
		if p.gen == gen {
			p.tiers = &tiers
		}
		p.mu.Unlock()
	}
	return tiers, nil
}

// TimeConfig 读取 billing.time_config（带内存缓存，见 defaultConfigProvider 缓存说明）。
// cacheOff 时读写缓存均跳过（与 R/ContextTiers 一致），退回直查 DB。
func (p *defaultConfigProvider) TimeConfig() (TimeCoeffConfig, error) {
	var gen uint64
	if !p.cacheOff {
		p.mu.RLock()
		cached := p.timeCfg
		gen = p.gen
		p.mu.RUnlock()
		if cached != nil {
			return *cached, nil
		}
	}

	raw, err := p.src.GetConfig(context.Background(), cfgTimeConfig)
	if err != nil {
		return TimeCoeffConfig{}, err
	}
	var tc TimeCoeffConfig
	tc, err = ParseTimeConfig(raw)
	if err != nil {
		return TimeCoeffConfig{}, fmt.Errorf("解析 time_config: %w", err)
	}

	if !p.cacheOff {
		p.mu.Lock()
		// 仅当回源期间未发生失效（代际未变）才回填（见 defaultConfigProvider 说明）。
		if p.gen == gen {
			p.timeCfg = &tc
		}
		p.mu.Unlock()
	}
	return tc, nil
}

// SampleUsage 读取 routing.cost_sample（路由成本比较样值用量，带内存缓存）。
// JSON 键与 Usage 一致（input/output/cache_read，三段口径）；未提供的段按 0 处理。
// 该值仅用于新会话创建时的成本比较，不参与实际计费。
// cacheOff 时读写缓存均跳过（与 R/ContextTiers 一致），退回直查 DB。
func (p *defaultConfigProvider) SampleUsage() (Usage, error) {
	var gen uint64
	if !p.cacheOff {
		p.mu.RLock()
		cached := p.sample
		gen = p.gen
		p.mu.RUnlock()
		if cached != nil {
			return *cached, nil
		}
	}

	raw, err := p.src.GetConfig(context.Background(), cfgCostSample)
	if err != nil {
		return Usage{}, err
	}
	var u Usage
	if err := json.Unmarshal(raw, &u); err != nil {
		return Usage{}, fmt.Errorf("解析 routing.cost_sample: %w", err)
	}

	if !p.cacheOff {
		p.mu.Lock()
		// 仅当回源期间未发生失效（代际未变）才回填（见 defaultConfigProvider 说明）。
		if p.gen == gen {
			p.sample = &u
		}
		p.mu.Unlock()
	}
	return u, nil
}

// SampleProfile 读取 routing.sample_profile（样值补齐比例，带内存缓存，见 defaultConfigProvider 缓存说明）。
// 缺失/解析失败返回错误（调用方回落全局 routing.cost_sample）；字段缺省用默认比例（0.9 / 0.1）补齐。
// cacheOff 时读写缓存均跳过（与 SampleUsage/ContextTiers 一致），退回直查 DB。
func (p *defaultConfigProvider) SampleProfile() (SampleProfile, error) {
	var gen uint64
	if !p.cacheOff {
		p.mu.RLock()
		cached := p.profile
		gen = p.gen
		p.mu.RUnlock()
		if cached != nil {
			return *cached, nil
		}
	}

	raw, err := p.src.GetConfig(context.Background(), cfgSampleProfile)
	if err != nil {
		return SampleProfile{}, err
	}
	prof, err := parseSampleProfile(raw)
	if err != nil {
		return SampleProfile{}, err
	}

	if !p.cacheOff {
		p.mu.Lock()
		// 仅当回源期间未发生失效（代际未变）才回填（见 defaultConfigProvider 说明）。
		if p.gen == gen {
			p.profile = &prof
		}
		p.mu.Unlock()
	}
	return prof, nil
}

// ===== SqlStore（billing_records + sys_configs）=====

// SqlStore 是存储层实现，同时满足 Store（billing_records）与 configSource（sys_configs）。
type SqlStore struct {
	db *sql.DB

	// onConfigWrite 在 sys_configs 写入成功后触发（用于失效 ConfigProvider 缓存）。
	// 由装配层经 SetConfigWriteHook 注入；为空时表示无缓存需要失效。
	onConfigWrite func()
}

// NewSqlStore 创建基于 *sql.DB 的存储实现。
func NewSqlStore(db *sql.DB) *SqlStore {
	return &SqlStore{db: db}
}

// SetConfigWriteHook 注册 sys_configs 写入后的回调（如失效配置缓存）。
// 非并发安全：仅应在启动装配阶段调用一次。
func (s *SqlStore) SetConfigWriteHook(fn func()) {
	s.onConfigWrite = fn
}

// recordCols 查询 billing_records 使用的全部列。
//
// ⚠️ 这里**不再有 channel_key_id**：0024 已删该列；渠道维度只剩 call_logs 的历史快照
// （call_logs 与账单按 billing_id 1:1），见 fillChannelKeys。写路径同样不再写该列。
const recordCols = `id, billing_id, user_id, pricing_mode, token_id, external_model_name,
	auto_model_name, internal_model_id, session_id, session_name, call_time, tokens, rates, coefficients, r_value,
	raw_total, credits_consumed, cost_credits, balance_before, balance_after, status, error_message, duration_ms, first_token_ms,
	upstream_status, upstream_error_summary`

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
// 展示用的渠道密钥维度由 call_logs 快照回填（billing_records 已无该列）。
func (s *SqlStore) GetByBillingID(ctx context.Context, billingID string) (*Record, error) {
	rec, err := s.scanByBillingID(ctx, billingID)
	if err != nil {
		return nil, err
	}
	if err := fillChannelKeys(ctx, s.db, []*Record{rec}); err != nil {
		return nil, err
	}
	return rec, nil
}

// scanByBillingID 只读 billing_records 本表（不回填展示维度）。
// 写路径的幂等预检查用它：那里只关心「是否已存在、状态如何」，不需要为了展示多查一次 call_logs。
func (s *SqlStore) scanByBillingID(ctx context.Context, billingID string) (*Record, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+recordCols+` FROM billing_records WHERE billing_id=$1`, billingID)
	return sqlScan(row)
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
	var sessionID any
	if rec.SessionID != "" {
		sessionID = rec.SessionID
	}
	// auto_model_name 可空：非 Auto 请求写 NULL（与列语义一致）。
	var autoModelName any
	if rec.AutoModelName != "" {
		autoModelName = rec.AutoModelName
	}
	// session_name 为 NOT NULL：空会话名写空串而非 NULL（与列默认值语义一致）。
	sessionName := any(rec.SessionName)
	var errMsg any
	if rec.ErrorMessage != "" {
		errMsg = rec.ErrorMessage
	}
	durationMs := nullableInt64For(rec.DurationMs)
	firstTokenMs := nullableInt64For(rec.FirstTokenMs)
	// ⚠️ 0024_v2_drop_legacy 已删 billing_records.channel_key_id：这里**不再写**该列。
	// 渠道维度只剩 call_logs.channel_key_id 的历史快照（读路径见 fillChannelKeys），
	// 新账单的渠道维度恒为空 —— 这是「渠道体系下线」的必然结果，不是丢数据。

	// V2 归因七列：写库前归一（非法协议 → NULL、非法 cost_source → none + WARN）。
	// 归因是解释性字段，**不许**因为它的取值问题让账单写不进去（会让扣款与落账不一致）。
	attr := attributionArgs(ctx, rec)

	var created time.Time
	if rec.ID == 0 {
		rec.ID = idgen.New()
	}
	// 两个新列放在最后，避免打乱既有 31 个参数的位置（重试队列/单测都按位置构造）。
	// upstream_status 为 0 时写 NULL（0 不是合法的 HTTP 码，写 0 会让统计误以为有响应）。
	args := append([]any{
		rec.ID, rec.BillingID, rec.UserID, rec.PricingMode, tokenID, rec.ExternalModel, autoModelName,
		rec.InternalModelID, sessionID, sessionName, rec.CallTime, tokensRaw, ratesRaw, coeffRaw,
		rec.RValue, rec.RawTotal, rec.CreditsConsumed, rec.CostCredits,
		rec.BalanceBefore, rec.BalanceAfter, rec.Status, errMsg, durationMs, firstTokenMs,
	}, attr...)
	args = append(args, nullableUpstreamStatus(rec.UpstreamStatus), nullableText(rec.UpstreamErrorSummary))

	err = ex.QueryRowContext(ctx,
		`INSERT INTO billing_records(id, billing_id, user_id, pricing_mode, token_id, external_model_name,
			auto_model_name, internal_model_id, session_id, session_name, call_time, tokens, rates, coefficients, r_value,
			raw_total, credits_consumed, cost_credits, balance_before, balance_after, status, error_message, duration_ms, first_token_ms,
			request_id, upstream_provider, upstream_account, upstream_protocol, upstream_model, ledger_entry_id, cost_source,
			upstream_status, upstream_error_summary)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,
			$25,$26,$27,$28,$29,$30,$31,$32,$33)
		 RETURNING id, created_at`, args...).
		Scan(&rec.ID, &created)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// fillChannelKeys 用 call_logs 的 1:1 历史快照回填渠道密钥维度（0024 之后**唯一**来源）。
//
// 为什么不写成 recordCols 里的标量子查询：recordCols 会被多表 JOIN/归并查询复用，加子查询要么
// 需要给外层表起别名（三处查询各一套），要么在归并 CTE 里落到错误的表上；批量回填只有一次查询，
// 且「值不变」这件事只在一个函数里说得清（迁移前 billing_records 与该快照同值）。
// 缺 call_logs 行（拒绝类账单没有日志）时留 0，与历史「未进入渠道」的语义一致。
func fillChannelKeys(ctx context.Context, ex dbx.Execer, recs []*Record) error {
	if len(recs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(recs))
	pos := make(map[string][]int, len(recs))
	for i, rec := range recs {
		if rec == nil || rec.BillingID == "" {
			continue
		}
		bid := rec.BillingID
		if _, ok := pos[bid]; !ok {
			ids = append(ids, bid)
		}
		pos[bid] = append(pos[bid], i)
	}
	if len(ids) == 0 {
		return nil
	}
	// 用 IN($1..$n) 而不是 ANY($1::text[])：不依赖驱动对切片参数的编码，
	// 也让用例（sqlmock）与生产（pgx）走同一条 SQL 形状。
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		marks[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	// 0024 之后 billing_records 已无 channel_key_id，快照（call_logs）是唯一来源：
	// 新账单（call_logs 停写之后）恒查不到 → 渠道维度留 0，符合「渠道体系下线」的口径。
	rows, err := ex.QueryContext(ctx,
		`SELECT cl.billing_id, cl.channel_key_id
		   FROM call_logs cl
		  WHERE cl.billing_id IN (`+strings.Join(marks, ", ")+`)
		    AND cl.channel_key_id IS NOT NULL`, args...)
	if err != nil {
		return fmt.Errorf("读取调用日志的渠道密钥维度失败：%w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var bid string
		var keyID int64
		if err := rows.Scan(&bid, &keyID); err != nil {
			return fmt.Errorf("扫描调用日志的渠道密钥维度失败：%w", err)
		}
		for _, i := range pos[bid] {
			recs[i].ChannelKeyID = keyID
		}
	}
	return rows.Err()
}

// nullableUpstreamStatus 把上游状态码转为可入库的 any：nil/0 → NULL（0 = 没收到响应）。
func nullableUpstreamStatus(v *int) any {
	if v == nil || *v <= 0 {
		return nil
	}
	return *v
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
	var sessionID sql.NullString
	var sessionName sql.NullString
	var autoModelName sql.NullString
	var rawTotal, consCred, costCred, balBefore, balAfter sql.NullFloat64
	var errMsg sql.NullString
	var durationMs, firstTokenMs sql.NullInt64
	var upstreamStatus sql.NullInt64
	var upstreamSummary sql.NullString
	var tokensRaw, ratesRaw, coeffRaw []byte
	var callTime time.Time

	err := row.Scan(&rec.ID, &rec.BillingID, &rec.UserID, &rec.PricingMode, &tokenID,
		&rec.ExternalModel, &autoModelName, &rec.InternalModelID, &sessionID, &sessionName, &callTime,
		&tokensRaw, &ratesRaw, &coeffRaw, &rec.RValue,
		&rawTotal, &consCred, &costCred, &balBefore, &balAfter, &rec.Status, &errMsg,
		&durationMs, &firstTokenMs, &upstreamStatus, &upstreamSummary)
	if err != nil {
		return nil, err
	}
	if tokenID.Valid {
		v := tokenID.Int64
		rec.TokenID = &v
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
	if upstreamStatus.Valid {
		v := int(upstreamStatus.Int64)
		rec.UpstreamStatus = &v
	}
	rec.UpstreamErrorSummary = upstreamSummary.String
	rec.ErrorMessage = errMsg.String
	rec.SessionID = sessionID.String
	rec.SessionName = sessionName.String
	rec.AutoModelName = autoModelName.String
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
