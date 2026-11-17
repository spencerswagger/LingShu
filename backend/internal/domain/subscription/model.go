// Package subscription 是订阅（上游账号固定月费）的管理端录入与手工重算端点（阶段二 A5 的唯一入口）。
//
// 为什么需要它：A5 分摊的取数 SQL 是 `share_cost = true AND price_amount > 0`，
// 而 subscriptions 从来没有录入入口 → 表空 → 每轮摊 0 笔 → 盈亏页成本永远少一笔固定支出。
//
// 口径唯一来源：《2026-11-16-subscription-entry-standard》（Spec 生效版）。本包逐条落地：
//   - 唯一自然键 (provider_ref, account_ref)：重复录入 409，不 upsert、不合并；
//   - `price_amount` 落库单位是**积分**（录入收「元/美元」，× 1,000,000 ÷ R，USD 先 × cny_rate）；
//   - 状态不落列（由 share_cost + cycle_start + billing_cycle 现算），本期不加任何列；
//   - 只给 ADMIN（路由挂在 /api/v1/admin 下），写操作全部留审计（谁、何时、改了什么）。
package subscription

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/pkg/decimalx"
)

// 计费周期（与 0019 的 CHECK 同源）。
const (
	CycleMonth = "month"
	CycleYear  = "year"
	CycleOnce  = "once"
)

// 币种。
const (
	CurrencyCNY = "CNY"
	CurrencyUSD = "USD"
)

// 派生状态（不落列，优先级见标准 §2.3，从上到下）。
const (
	StatusStopped      = "stopped"
	StatusOnce         = "once"
	StatusMissingCycle = "missing_cycle"
	StatusPending      = "pending"
	StatusExpired      = "expired"
	StatusActive       = "active"
)

// 「摊 0 笔」原因码（标准 §5.2）。静态条件（C1–C4）由本包判定；
// no_billing_rows / no_attribution / account_mismatch / ledger_taken 是运行时事实，属 S4。
const (
	ReasonShareOff     = "share_off"
	ReasonPriceZero    = "price_zero"
	ReasonCycleOnce    = "cycle_once"
	ReasonCycleMissing = "cycle_missing"
	ReasonCyclePending = "cycle_pending"
	ReasonCycleExpired = "cycle_expired"
)

// 录入响应里的机器可读原因键（标准 §6.2，落在响应体 Reason 字段）。
const (
	ReasonInvalidField        = "invalid_field"
	ReasonProviderNotFound    = "provider_not_found"
	ReasonTopologyNotSynced   = "topology_not_synced"
	ReasonDuplicate           = "duplicate_subscription"
	ReasonRateNotConfigured   = "rate_not_configured"
	ReasonRNotConfigured      = "r_not_configured"
	ReasonRateLimited         = "rate_limited"
	ReasonInternal            = "internal_error"
	WarningAccountUnverified  = "account_unverified"
	nextRunHint               = "下一轮分摊 ≤5 分钟；无账单会进「未摊出」告警"
	priceUpperBound           = 1e9 // 元；再大必然是录入错误（标准 §3.1）
	noteMaxRunes              = 500
	nameMaxLen                = 128
	minCycleStartYear         = 2000
	creditsPerYuanCoefficient = 1_000_000 // 积分 = 元 × 1e6 ÷ R（积分价值 V = R ÷ 1e6 元/积分）
)

// ErrDuplicate 表示自然键冲突（PG 23505 on uq_subscriptions_account）。
var ErrDuplicate = errors.New("订阅账号已存在")

// ErrNotFound 表示按 id/自然键查不到。
var ErrNotFound = errors.New("订阅不存在")

// Input 是录入入参（字段名与标准 §2.1 的「API 字段」一致，View 的 SubscriptionInput 逐字对齐）。
type Input struct {
	Provider string  `json:"Provider"`
	Account  string  `json:"Account"`
	Price    float64 `json:"Price"`
	Currency string  `json:"Currency"`
	Cycle    string  `json:"Cycle"`
	// PayDay / ShareCost 用指针区分「没传」与「传了零值」：缺省按 1 / true（标准 §2.1 的默认值）。
	PayDay     *int   `json:"PayDay"`
	ShareCost  *bool  `json:"ShareCost"`
	CycleStart string `json:"CycleStart"`
	Note       string `json:"Note"`
}

// Row 是 subscriptions 的一行（price_amount 已是积分口径）。
type Row struct {
	ID           int64
	Provider     string
	Account      string
	PriceCredits float64
	Currency     string
	Cycle        string
	PayDay       int
	ShareCost    bool
	CycleStart   *time.Time
	Note         string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Warning 是「不阻断但不该忽略」的提示（目前只有账号软校验一种）。
type Warning struct {
	Code    string `json:"Code"`
	Message string `json:"Message"`
}

// View 是订阅的对外表示（View 的 Subscription 类型逐字对齐）。
type View struct {
	ID           string    `json:"ID"`
	Provider     string    `json:"Provider"`
	Account      string    `json:"Account"`
	Price        float64   `json:"Price"` // 原币金额（元/美元），仅展示用
	Currency     string    `json:"Currency"`
	PriceCredits *float64  `json:"PriceCredits"` // 折算后的积分；缺 R 时为 null（不许用 0 冒充）
	Cycle        string    `json:"Cycle"`
	PayDay       int       `json:"PayDay"`
	ShareCost    bool      `json:"ShareCost"`
	CycleStart   string    `json:"CycleStart"`
	Note         string    `json:"Note"`
	Status       string    `json:"Status"`
	WillAmortize bool      `json:"WillAmortize"`
	BlockReasons []string  `json:"BlockReasons"`
	Warnings     []Warning `json:"Warnings"`
	// CycleAllocated / CycleUnallocated 是本期已摊 / 未摊出，**现算不落库**（标准 §0 第 6 条）。
	CycleAllocated   *float64 `json:"CycleAllocated"`
	CycleUnallocated *float64 `json:"CycleUnallocated"`
	CreatedAt        string   `json:"CreatedAt"`
	UpdatedAt        string   `json:"UpdatedAt"`
	NextRunHint      string   `json:"NextRunHint"`
}

// Error 是可直接映射成 HTTP 响应的业务错误。
type Error struct {
	Status  int
	Code    int
	Reason  string
	Field   string
	Message string
	// Data 是可选的附加信息（如 duplicate 的 ExistingID），由 handler 原样放进响应体。
	Data any
}

func (e *Error) Error() string { return e.Message }

// apiError 造一个带字段定位的业务错误（400 类）。
func apiError(reason, field, msg string) *Error {
	return &Error{Status: 400, Code: 40001, Reason: reason, Field: field, Message: msg}
}

// normalized 是校验通过后的规范化值（trim 过的字符串 + 显式默认值）。
type normalized struct {
	Provider   string
	Account    string
	Price      float64
	Currency   string
	Cycle      string
	PayDay     int
	ShareCost  bool
	CycleStart *time.Time
	Note       string
}

// normalizeAndValidate 落地标准 §3.1（L1 语法与范围）。
//
// 只做「不依赖外部数据」的校验：R/汇率（依赖 sys_configs）与 provider 存在性（依赖拓扑镜像）
// 在 Service 里按依赖顺序判 —— 顺序错了会给出误导性的错误码（标准 §6.2 要求 Reason 可分支）。
func normalizeAndValidate(in Input) (normalized, *Error) {
	out := normalized{
		Provider:  strings.TrimSpace(in.Provider),
		Account:   strings.TrimSpace(in.Account),
		Price:     in.Price,
		Currency:  strings.ToUpper(strings.TrimSpace(in.Currency)),
		Cycle:     strings.ToLower(strings.TrimSpace(in.Cycle)),
		PayDay:    1,
		ShareCost: true,
		Note:      strings.TrimSpace(in.Note),
	}
	if out.Currency == "" {
		out.Currency = CurrencyCNY
	}
	if out.Cycle == "" {
		out.Cycle = CycleMonth
	}
	if in.PayDay != nil {
		out.PayDay = *in.PayDay
	}
	if in.ShareCost != nil {
		out.ShareCost = *in.ShareCost
	}

	switch {
	case out.Provider == "":
		return out, apiError(ReasonInvalidField, "Provider", "订阅方不能为空")
	case len([]rune(out.Provider)) > nameMaxLen:
		return out, apiError(ReasonInvalidField, "Provider", "订阅方名称过长（最多 128 字）")
	case strings.ContainsAny(out.Provider, "\r\n"):
		return out, apiError(ReasonInvalidField, "Provider", "订阅方名称不能包含换行")
	case out.Account == "":
		return out, apiError(ReasonInvalidField, "Account", "账号不能为空")
	case len([]rune(out.Account)) > nameMaxLen:
		return out, apiError(ReasonInvalidField, "Account", "账号过长（最多 128 字）")
	case strings.ContainsAny(out.Account, "\r\n"):
		return out, apiError(ReasonInvalidField, "Account", "账号不能包含换行")
	}

	if out.Price < 0 {
		return out, apiError(ReasonInvalidField, "Price", "整期费用不能为负数")
	}
	if out.Price > priceUpperBound {
		return out, apiError(ReasonInvalidField, "Price", "整期费用过大（上限 1e9）")
	}
	if tooManyDecimals(out.Price, 5) {
		return out, apiError(ReasonInvalidField, "Price", "整期费用最多 5 位小数")
	}
	// 0 元只允许作为「停摊占位」：否则会存进一条「会被扫到但永远摊 0」的记录。
	if out.Price == 0 && out.ShareCost {
		return out, apiError(ReasonInvalidField, "Price", "0 元只能作为停摊占位（先关闭「参与分摊」）")
	}

	switch out.Currency {
	case CurrencyCNY, CurrencyUSD:
	default:
		return out, apiError(ReasonInvalidField, "Currency", "币种只支持 CNY / USD")
	}
	switch out.Cycle {
	case CycleMonth, CycleYear, CycleOnce:
	default:
		return out, apiError(ReasonInvalidField, "Cycle", "计费周期只支持 month / year / once")
	}
	if out.PayDay < 1 || out.PayDay > 28 {
		return out, apiError(ReasonInvalidField, "PayDay", "付款日必须是 1–28（29–31 会跨月错位）")
	}
	if len([]rune(out.Note)) > noteMaxRunes {
		return out, apiError(ReasonInvalidField, "Note", "备注最多 500 字")
	}

	// 账期起点：允许留空（= 不摊，状态 missing_cycle，可在列表里补齐），
	// 但一旦给了就必须是合法日期，且不早于 2000-01-01。
	if raw := strings.TrimSpace(in.CycleStart); raw != "" {
		d, err := time.Parse("2006-01-02", raw)
		if err != nil {
			return out, apiError(ReasonInvalidField, "CycleStart", "账期起点格式必须是 YYYY-MM-DD")
		}
		if d.Year() < minCycleStartYear {
			return out, apiError(ReasonInvalidField, "CycleStart", "账期起点不能早于 2000-01-01")
		}
		out.CycleStart = &d
	}
	return out, nil
}

// tooManyDecimals 判断 v 超过 n 位小数（用十进制字符串判，避免浮点误差把 100.005 判成合规/不合规）。
func tooManyDecimals(v float64, n int) bool {
	s := fmt.Sprintf("%.*f", n+6, v)
	i := strings.IndexByte(s, '.')
	if i < 0 {
		return false
	}
	frac := strings.TrimRight(s[i+1:], "0")
	return len(frac) > n
}

// creditsFor 把录入金额折算成积分（落库单位）。
//
// 公式与 billing/engine.go 同源：积分价值 V = R ÷ 1,000,000 元/积分 ⇒ 积分 = 元 × 1e6 ÷ R。
// USD 先按全局 billing.cny_rate 折成人民币（汇率不进表：录入即快照，币种只作审计）。
func creditsFor(price float64, currency string, cnyRate float64, r int64) (float64, error) {
	if r <= 0 {
		return 0, errors.New("R(积分换算系数) 必须大于 0")
	}
	cny := price
	if currency == CurrencyUSD {
		if cnyRate <= 0 {
			return 0, errors.New("USD 汇率未配置")
		}
		cny = price * cnyRate
	}
	return decimalx.Round5(cny * creditsPerYuanCoefficient / float64(r)), nil
}

// priceFor 把积分折回原币金额（仅展示/回读用；因为汇率与 R 是全局量，回读值是近似值）。
func priceFor(credits float64, currency string, cnyRate float64, r int64) float64 {
	if r <= 0 {
		return 0
	}
	cny := credits * float64(r) / creditsPerYuanCoefficient
	if currency == CurrencyUSD {
		if cnyRate <= 0 {
			return 0
		}
		cny = cny / cnyRate
	}
	return decimalx.Round2(cny)
}

// cycleWindow 返回账期窗口 [start, end)。month = +1 月，year = +1 年，once = 当天结束。
func cycleWindow(start time.Time, cycle string) time.Time {
	switch cycle {
	case CycleYear:
		return start.AddDate(1, 0, 0)
	case CycleOnce:
		return start.AddDate(0, 0, 1)
	default:
		return start.AddDate(0, 1, 0)
	}
}

// dayStart 抹掉时分秒（账期是「日」粒度，比较必须按自然日）。
func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// statusOf 现算派生状态（标准 §2.3，优先级从上到下）。
func statusOf(shareCost bool, cycle string, cycleStart *time.Time, today time.Time) string {
	if !shareCost {
		return StatusStopped
	}
	if cycle == CycleOnce {
		return StatusOnce
	}
	if cycleStart == nil {
		return StatusMissingCycle
	}
	d := dayStart(today)
	st := dayStart(*cycleStart)
	if st.After(d) {
		return StatusPending
	}
	if !d.Before(cycleWindow(st, cycle)) {
		return StatusExpired
	}
	return StatusActive
}

// staticVerdict 判定「会不会被摊到」（只判 C1–C4 静态条件，标准 §5.1/§5.3）。
//
// C5/C6（当期有没有可摊账单、是否已被账本占用）是运行时事实，由 S4 回显，
// 这里**不许**承诺 —— 承诺了就会变成「录完说会摊、结果摊 0」。
func staticVerdict(shareCost bool, credits float64, cycle string, cycleStart *time.Time, today time.Time) (bool, []string) {
	reasons := make([]string, 0, 3)
	if !shareCost {
		reasons = append(reasons, ReasonShareOff)
	}
	if !(credits > 0) {
		reasons = append(reasons, ReasonPriceZero)
	}
	if cycle == CycleOnce {
		reasons = append(reasons, ReasonCycleOnce)
	}
	if shareCost && cycle != CycleOnce {
		switch statusOf(shareCost, cycle, cycleStart, today) {
		case StatusMissingCycle:
			reasons = append(reasons, ReasonCycleMissing)
		case StatusExpired:
			reasons = append(reasons, ReasonCycleExpired)
		case StatusPending:
			// 待生效不是异常（标准 §5.2 的处置就是「无需处理」），但仍不算会摊。
			reasons = append(reasons, ReasonCyclePending)
		}
	}
	return len(reasons) == 0, reasons
}
