package subscription

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/team/llmgateway/internal/domain/audit"
	"github.com/team/llmgateway/internal/pkg/decimalx"
	"github.com/team/llmgateway/internal/pkg/idgen"
)

// ConfigSource 提供折算所需的两个全局参数（billing.ConfigProvider 满足）。
type ConfigSource interface {
	// R 是积分换算系数（billing.r）：积分价值 = R ÷ 1,000,000 元/积分。
	R() (int64, error)
	// CnyRate 是 USD/CNY 汇率（billing.cny_rate）。
	CnyRate() (float64, error)
}

// AuditSink 是审计写入端口（audit.Store 满足）。
type AuditSink interface {
	Insert(ctx context.Context, e audit.Entry) error
}

// Options 是 NewService 的入参。
type Options struct {
	Store  Store
	Config ConfigSource
	Audit  AuditSink
	Logger *slog.Logger
	Now    func() time.Time
	// NewID 是主键生成器（默认雪花）；单测注入定值便于断言。
	NewID func() int64
}

// Service 是订阅录入服务。
type Service struct {
	store  Store
	cfg    ConfigSource
	audit  AuditSink
	log    *slog.Logger
	now    func() time.Time
	newID  func() int64
	userID func(ctx context.Context) (int64, bool)
}

// NewService 创建订阅服务。
func NewService(opts Options) *Service {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	newID := opts.NewID
	if newID == nil {
		newID = idgen.New
	}
	return &Service{store: opts.Store, cfg: opts.Config, audit: opts.Audit, log: logger, now: now, newID: newID}
}

// Create 录入一条订阅（S2）。
//
// 校验顺序刻意是「语法 → 全局参数 → 拓扑存在性 → 落库」：
// 反过来会让一个「价格为负 + 拓扑为空」的请求先报 topology_not_synced，运营照着去同步，
// 回来还是 400 —— Reason 必须指向真正要改的那一处。
func (s *Service) Create(ctx context.Context, in Input) (*View, *Error) {
	if s == nil || s.store == nil {
		return nil, &Error{Status: 500, Code: 50001, Reason: ReasonInternal, Message: "订阅服务未装配存储"}
	}
	norm, verr := normalizeAndValidate(in)
	if verr != nil {
		return nil, verr
	}

	// 折算参数先取：缺汇率时连库都不用碰（USD 录入的 422 要在所有写动作之前）。
	r, rerr := s.r()
	if rerr != nil {
		return nil, &Error{Status: 500, Code: 50001, Reason: ReasonRNotConfigured,
			Message: "积分换算系数 R 未配置或非法，无法折算：" + rerr.Error()}
	}
	cnyRate := 0.0
	if norm.Currency == CurrencyUSD {
		rate, err := s.cnyRate()
		if err != nil || rate <= 0 {
			return nil, &Error{Status: 422, Code: 42201, Reason: ReasonRateNotConfigured,
				Message: "USD 汇率未配置，暂不能按美元录入（去「系统设置 → 计费参数」配 billing.cny_rate）"}
		}
		cnyRate = rate
	}
	credits, cerr := creditsFor(norm.Price, norm.Currency, cnyRate, r)
	if cerr != nil {
		return nil, &Error{Status: 422, Code: 42201, Reason: ReasonRateNotConfigured, Message: cerr.Error()}
	}

	// 拓扑存在性：镜像为空 → 409 topology_not_synced（页面阻断并指路去同步）；
	// 有镜像但没这个供应商 → 400 provider_not_found（改字段即可，不用同步）。
	providers, err := s.store.Providers(ctx)
	if err != nil {
		return nil, &Error{Status: 500, Code: 50001, Reason: ReasonInternal, Message: err.Error()}
	}
	if len(providers) == 0 {
		return nil, &Error{Status: 409, Code: 40901, Reason: ReasonTopologyNotSynced,
			Message: "尚未同步网关拓扑，请先在「上游拓扑」页执行一次同步"}
	}
	if !containsString(providers, norm.Provider) {
		return nil, apiError(ReasonProviderNotFound, "Provider", "订阅方不在网关镜像中（可先去「上游拓扑」页同步）")
	}

	// 账号只能软校验：V2 没有网关账号镜像表，硬校验等于录入时必须实时调网关。
	warnings := make([]Warning, 0, 1)
	known, err := s.store.KnownAccounts(ctx, s.now().AddDate(0, 0, -30))
	if err != nil {
		// 软校验失败不影响落库，但要让运营知道「这次没校验」。
		s.log.Warn("读取已知账号失败（账号软校验跳过）", "err", err)
	} else if _, ok := known[norm.Account]; !ok {
		warnings = append(warnings, Warning{
			Code:    WarningAccountUnverified,
			Message: "账号未在近 30 天账单/账本中出现，请确认拼写，否则永远摊 0 笔",
		})
	}

	row := &Row{
		ID:           s.newID(),
		Provider:     norm.Provider,
		Account:      norm.Account,
		PriceCredits: credits,
		Currency:     norm.Currency,
		Cycle:        norm.Cycle,
		PayDay:       norm.PayDay,
		ShareCost:    norm.ShareCost,
		CycleStart:   norm.CycleStart,
		Note:         norm.Note,
	}
	if err := s.store.Create(ctx, row); err != nil {
		if errors.Is(err, ErrDuplicate) {
			return nil, s.duplicate(ctx, norm.Provider, norm.Account)
		}
		return nil, &Error{Status: 500, Code: 50001, Reason: ReasonInternal, Message: err.Error()}
	}

	stored, err := s.store.GetByNaturalKey(ctx, norm.Provider, norm.Account)
	if err != nil {
		// 刚插进去却查不出来：宁可报错也不要用内存里的半成品糊过去。
		return nil, &Error{Status: 500, Code: 50001, Reason: ReasonInternal,
			Message: "订阅已写入但回读失败，请刷新列表确认：" + err.Error()}
	}

	s.auditCreate(ctx, stored)
	return s.render(stored, s.allocatedFor(ctx, stored), r, cnyRate, warnings), nil
}

// duplicate 组装 409：带 ExistingID 与现有行，前端据此弹「改为编辑」。
func (s *Service) duplicate(ctx context.Context, provider, account string) *Error {
	e := &Error{Status: 409, Code: 40901, Reason: ReasonDuplicate,
		Message: "该账号已有订阅（自然键 provider + account 不允许重复录入）"}
	if existing, err := s.store.GetByNaturalKey(ctx, provider, account); err == nil && existing != nil {
		e.Message = fmt.Sprintf("该账号已有订阅（ID %d），唯一键不允许重复录入，是否改为编辑？", existing.ID)
		// ExistingID 让前端直接跳详情；没有它只能拿 (Provider, Account) 在本地列表里反查。
		e.Data = map[string]any{"ExistingID": strconv.FormatInt(existing.ID, 10)}
	}
	return e
}

// auditCreate 记录一条结构化审计（写操作必须可追责：谁、何时、改了什么）。
//
// 审计失败不回滚业务（数据已落库），但必须打 ERROR：
// 静默丢审计等于安全控制在被悄悄关掉（与 audit 包的口径一致）。
func (s *Service) auditCreate(ctx context.Context, row *Row) {
	if s.audit == nil {
		return
	}
	detail := map[string]any{
		"provider":           row.Provider,
		"account":            row.Account,
		"price_amount_after": row.PriceCredits, // 积分口径（落库值）
		"price_currency":     row.Currency,
		"billing_cycle":      row.Cycle,
		"pay_day":            row.PayDay,
		"share_cost_after":   row.ShareCost,
		"cycle_start_after":  cycleStartText(row.CycleStart),
	}
	if err := s.audit.Insert(ctx, audit.Entry{
		Action:     "subscription.create",
		TargetType: "subscription",
		TargetID:   strconv.FormatInt(row.ID, 10),
		Detail:     detail,
	}); err != nil {
		s.log.Error("订阅录入审计写入失败", "err", err, "subscription_id", row.ID)
	}
}

// render 把库行渲染成对外视图（状态/判定全部现算；allocated 由调用方提供）。
//
// allocated 用指针是为了区分「算过且是 0」与「没算过」：前端对 null 显示「—」，
// 用 0 冒充会让运营以为「本期已摊 0，是不是分摊没跑」。
func (s *Service) render(row *Row, allocated *float64, r int64, cnyRate float64, warnings []Warning) *View {
	today := s.now()
	status := statusOf(row.ShareCost, row.Cycle, row.CycleStart, today)
	will, reasons := staticVerdict(row.ShareCost, row.PriceCredits, row.Cycle, row.CycleStart, today)

	credits := row.PriceCredits
	out := &View{
		ID:           strconv.FormatInt(row.ID, 10),
		Provider:     row.Provider,
		Account:      row.Account,
		Price:        priceFor(row.PriceCredits, row.Currency, cnyRate, r),
		Currency:     row.Currency,
		PriceCredits: &credits,
		Cycle:        row.Cycle,
		PayDay:       row.PayDay,
		ShareCost:    row.ShareCost,
		CycleStart:   cycleStartText(row.CycleStart),
		Note:         row.Note,
		Status:       status,
		WillAmortize: will,
		BlockReasons: reasons, // 静态条件全过时是空数组，不是 null
		Warnings:     warnings,
		CreatedAt:    row.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    row.UpdatedAt.UTC().Format(time.RFC3339),
		NextRunHint:  nextRunHint,
	}
	if allocated != nil {
		a := decimalx.Round5(*allocated)
		un := decimalx.Round5(row.PriceCredits - a)
		if un < 0 {
			un = 0 // 价格调低后已摊可能大于当期价：未摊出按 0 展示，不显示负数
		}
		out.CycleAllocated = &a
		out.CycleUnallocated = &un
	}
	return out
}

// allocatedFor 现查本期已摊（只对「当前账期内」有意义：过往账期已结转，不再回算）。
func (s *Service) allocatedFor(ctx context.Context, row *Row) *float64 {
	if row.CycleStart == nil {
		return nil
	}
	switch statusOf(row.ShareCost, row.Cycle, row.CycleStart, s.now()) {
	case StatusPending, StatusMissingCycle:
		return nil
	}
	from := dayStart(*row.CycleStart)
	allocated, err := s.store.Allocated(ctx, row.Provider, row.Account, from, cycleWindow(from, row.Cycle))
	if err != nil {
		// 读失败回 null（不冒充 0），并留下日志：这是「页面显示 —」的真实原因。
		s.log.Warn("读取本期已摊失败（回空值，不冒充 0）", "err", err, "subscription_id", row.ID)
		return nil
	}
	return &allocated
}

func (s *Service) r() (int64, error) {
	if s.cfg == nil {
		return 0, errors.New("未装配计费配置源")
	}
	r, err := s.cfg.R()
	if err != nil {
		return 0, err
	}
	if r <= 0 {
		return 0, fmt.Errorf("billing.r = %d 非法", r)
	}
	return r, nil
}

func (s *Service) cnyRate() (float64, error) {
	if s.cfg == nil {
		return 0, errors.New("未装配计费配置源")
	}
	return s.cfg.CnyRate()
}

func containsString(list []string, v string) bool {
	for _, it := range list {
		if it == v {
			return true
		}
	}
	return false
}

func cycleStartText(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02")
}
