// Package ledgersettle 把「上游账本条目」匹配到「灵枢账单行」，并把差额结算落库（阶段二 A3）。
//
// 为什么需要它：预扣与结算用的是**灵枢自己的 usage 估算/读取**（请求级事实），
// 而供应商真正收的钱在账本里（账本条目按 (provider, account, model) 归集，**没有 request_id**）。
// 两件事必须挂上：
//
//	① 成本：billing_records.cost_credits + cost_source='ledger' + ledger_entry_id
//	   —— 盈亏页的成本口径只有这一个真源（估算不算成本）；
//	② 预扣登记：credit_holds.ledger_entry_id（一请求一预扣「已匹配到账本」的证据）。
//
// 匹配口径（刻意的严格）：
//   - 键 = provider + account + model **三列全等**。缺一列就不匹配（宁可留 unmatched 让人看见，
//     也不按 model 单键猜——猜错等于把成本记到别人账上，盈亏页直接失真）；
//   - 一对一：一条账本条目只对应一条账单；
//   - 时间窗内取最近：|账单 call_time − 账本 occurred_at| ≤ 容差（默认 2 个对账窗口 = 120s，
//     覆盖上游时钟偏差 30s + 排队/重试延迟）；
//   - 只匹配「还没有账本条目」的账单（ledger_entry_id IS NULL），重复跑幂等。
//
// 不在这层做的事：改用户侧的扣费（credits_consumed 属售价口径，账本成本不动它）、
// 改账单状态、重算对账窗口计数（那是 reconcile 包的事）。
package ledgersettle

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/upstream"
	"github.com/team/llmgateway/internal/pkg/decimalx"
)

// 默认参数。
const (
	// DefaultMatchTolerance 是账单与账本条目的时间容差（2 个对账窗口）。
	DefaultMatchTolerance = 2 * time.Minute
	// DefaultLookback 是每轮回补的窗口数（迟到的账本条目是常态）。
	DefaultLookback = 10
)

// BillingRow 是一条待匹配的账单（只取匹配需要的列）。
type BillingRow struct {
	BillingID string
	RequestID string
	Provider  string
	Account   string
	Model     string
	CallTime  time.Time
	// Tokens 是灵枢口径的三段合计（input + cache_read + output），用于观察差值。
	Tokens int64
}

// LedgerRow 是一条账本条目。
//
// TokensIn 是**提示词侧总量**（代理侧 `in` + cache_read + cache_write，见 upstream.LedgerEntry），
// 所以 TokensIn+TokensOut 与账单侧的三段合计同口径；TokenDelta 才有意义。
type LedgerRow struct {
	LedgerID   string
	Provider   string
	Account    string
	Model      string
	TokensIn   int64
	TokensOut  int64
	Cost       float64
	OccurredAt time.Time
}

// Match 是一对已匹配的（账单, 账本条目）。
type Match struct {
	BillingID string
	RequestID string
	LedgerID  string
	Cost      float64
	// Skew 是两侧时间差（账本 − 账单），排障用。
	Skew time.Duration
	// TokenDelta 是「灵枢 tokens − 账本 tokens」，对账口径的请求级观察值。
	TokenDelta int64
}

// MatchResult 是一次纯匹配的结果。
type MatchResult struct {
	Matched []Match
	// UnmatchedLedger / UnmatchedBilling 是没配上的条数（必须可观测，静默丢弃等于漏账）。
	UnmatchedLedger  int
	UnmatchedBilling int
}

// matchKey 是匹配键：三列全等，缺失即空串（空串也只能和空串相等 —— 但空 provider/model 的条目
// 在一开始就被剔掉，见 matchable）。
type matchKey struct{ provider, account, model string }

func keyOf(provider, account, model string) matchKey {
	return matchKey{
		provider: strings.TrimSpace(provider),
		account:  strings.TrimSpace(account),
		model:    strings.TrimSpace(model),
	}
}

// MatchRows 做纯匹配（不碰库，可单测）。
//
// 同一键内两侧各自按时间升序，然后对每条账本条目取「时间最近且未被占用」的账单：
// 账单侧是稀缺侧（一个请求一条账），账本条目可能重复/迟到，所以从账本侧遍历。
func MatchRows(bills []BillingRow, ledgers []LedgerRow, tolerance time.Duration) MatchResult {
	if tolerance <= 0 {
		tolerance = DefaultMatchTolerance
	}
	var out MatchResult

	type bucket struct {
		bills   []int
		ledgers []int
	}
	groups := map[matchKey]*bucket{}
	order := make([]matchKey, 0, 8)
	touch := func(k matchKey) *bucket {
		b, ok := groups[k]
		if !ok {
			b = &bucket{}
			groups[k] = b
			order = append(order, k)
		}
		return b
	}

	for i := range bills {
		k := keyOf(bills[i].Provider, bills[i].Account, bills[i].Model)
		if k.provider == "" || k.model == "" {
			// 归因缺失的账单不参与匹配：没有供应商/模型就没有可信的成本归属。
			out.UnmatchedBilling++
			continue
		}
		touch(k).bills = append(touch(k).bills, i)
	}
	for i := range ledgers {
		k := keyOf(ledgers[i].Provider, ledgers[i].Account, ledgers[i].Model)
		if k.provider == "" || k.model == "" {
			out.UnmatchedLedger++
			continue
		}
		touch(k).ledgers = append(touch(k).ledgers, i)
	}

	used := make([]bool, len(bills))
	for _, k := range order {
		g := groups[k]
		sort.SliceStable(g.bills, func(a, b int) bool {
			return bills[g.bills[a]].CallTime.Before(bills[g.bills[b]].CallTime)
		})
		sort.SliceStable(g.ledgers, func(a, b int) bool {
			return ledgers[g.ledgers[a]].OccurredAt.Before(ledgers[g.ledgers[b]].OccurredAt)
		})
		for _, li := range g.ledgers {
			l := ledgers[li]
			bestIdx, bestSkew := -1, time.Duration(math.MaxInt64)
			for _, bi := range g.bills {
				if used[bi] {
					continue
				}
				b := bills[bi]
				skew := l.OccurredAt.Sub(b.CallTime)
				abs := skew
				if abs < 0 {
					abs = -abs
				}
				if abs > tolerance {
					continue
				}
				if abs < bestSkew {
					bestIdx, bestSkew = bi, abs
				}
			}
			if bestIdx < 0 {
				out.UnmatchedLedger++
				continue
			}
			used[bestIdx] = true
			b := bills[bestIdx]
			out.Matched = append(out.Matched, Match{
				BillingID:  b.BillingID,
				RequestID:  b.RequestID,
				LedgerID:   l.LedgerID,
				Cost:       decimalx.Round5(l.Cost),
				Skew:       l.OccurredAt.Sub(b.CallTime),
				TokenDelta: b.Tokens - (l.TokensIn + l.TokensOut),
			})
		}
	}
	for i := range bills {
		if !used[i] && keyOf(bills[i].Provider, bills[i].Account, bills[i].Model).provider != "" &&
			keyOf(bills[i].Provider, bills[i].Account, bills[i].Model).model != "" {
			out.UnmatchedBilling++
		}
	}
	return out
}

// Store 是账本结算的读写端口（SQLStore 的 SQL 实现；用例注入 fake）。
type Store interface {
	// UnmatchedBilling 返回窗口内「还没挂账本条目」且带归因的账单。
	UnmatchedBilling(ctx context.Context, from, to time.Time) ([]BillingRow, error)
	// LedgerRows 返回窗口内的账本条目。
	LedgerRows(ctx context.Context, from, to time.Time) ([]LedgerRow, error)
	// Link 把一对匹配落库（账单成本列 + 预扣登记），返回是否真的写到了账单行。
	Link(ctx context.Context, m Match) (bool, error)
}

// Result 是一轮结算的汇总。
type Result struct {
	From, To time.Time
	// Linked 是成功落库的条数；Skipped 是账单行已被别的条目占用（并发/重跑）。
	Linked  int
	Skipped int
	// UnmatchedLedger / UnmatchedBilling 见 MatchResult。
	UnmatchedLedger  int
	UnmatchedBilling int
	// Cost 是本轮挂上的成本合计（积分）。
	Cost float64
}

// Matcher 是账本结算服务。
type Matcher struct {
	store    Store
	lookback int
	tol      time.Duration
	now      func() time.Time
	log      *slog.Logger
}

// Options 是 NewMatcher 的入参。
type Options struct {
	Store     Store
	Lookback  int
	Tolerance time.Duration
	Now       func() time.Time
	Logger    *slog.Logger
}

// NewMatcher 创建结算服务。
func NewMatcher(opts Options) *Matcher {
	lb := opts.Lookback
	if lb <= 0 {
		lb = DefaultLookback
	}
	tol := opts.Tolerance
	if tol <= 0 {
		tol = DefaultMatchTolerance
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Matcher{store: opts.Store, lookback: lb, tol: tol, now: now, log: logger}
}

// RunOnce 回补最近 lookback 个已结束窗口的账本结算。
func (m *Matcher) RunOnce(ctx context.Context) (Result, error) {
	if m == nil || m.store == nil {
		return Result{}, fmt.Errorf("账本结算服务未装配存储")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	end := m.now().Truncate(upstream.WindowSize)
	from := end.Add(-time.Duration(m.lookback) * upstream.WindowSize)
	res := Result{From: from, To: end}

	bills, err := m.store.UnmatchedBilling(ctx, from, end)
	if err != nil {
		return res, err
	}
	ledgers, err := m.store.LedgerRows(ctx, from, end)
	if err != nil {
		return res, err
	}
	mr := MatchRows(bills, ledgers, m.tol)
	res.UnmatchedLedger, res.UnmatchedBilling = mr.UnmatchedLedger, mr.UnmatchedBilling

	for _, one := range mr.Matched {
		ok, err := m.store.Link(ctx, one)
		if err != nil {
			return res, err
		}
		if !ok {
			res.Skipped++
			continue
		}
		res.Linked++
		res.Cost = decimalx.Round5(res.Cost + one.Cost)
	}
	return res, nil
}

// SQLStore 是 Store 的 PostgreSQL 实现。
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore 创建 SQL 存储。
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

var _ Store = (*SQLStore)(nil)

// UnmatchedBilling 读窗口内待挂账的账单。
//
// 过滤条件三条：
//   - ledger_entry_id IS NULL：已挂过的不再匹配（幂等）；
//   - upstream_provider IS NOT NULL：没有归因的账单匹配不了（历史行）；
//   - cost_source 只允许空/none/amortized：账本成本是变动成本的真源，**可以覆盖**订阅摊销
//     （amortize 会在下一轮把被覆盖的行剔出分母重算，两边都是 SET 不是 ADD，能收敛）。
func (s *SQLStore) UnmatchedBilling(ctx context.Context, from, to time.Time) ([]BillingRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("账本结算存储未装配数据库连接")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT billing_id,
		        COALESCE(request_id, ''),
		        COALESCE(upstream_provider, ''),
		        COALESCE(upstream_account, ''),
		        COALESCE(upstream_model, ''),
		        call_time,
		        COALESCE(`+billing.TokenSumExpr("")+`, 0)
		   FROM billing_records
		  WHERE call_time >= $1 AND call_time < $2
		    AND ledger_entry_id IS NULL
		    AND upstream_provider IS NOT NULL
		    AND (cost_source IS NULL OR cost_source IN ('none','amortized'))
		  ORDER BY call_time, billing_id`, from, to)
	if err != nil {
		return nil, fmt.Errorf("读取待结算账单失败：%w", err)
	}
	defer rows.Close()

	out := make([]BillingRow, 0, 64)
	for rows.Next() {
		var b BillingRow
		if err := rows.Scan(&b.BillingID, &b.RequestID, &b.Provider, &b.Account, &b.Model, &b.CallTime, &b.Tokens); err != nil {
			return nil, fmt.Errorf("扫描待结算账单失败：%w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历待结算账单失败：%w", err)
	}
	return out, nil
}

// LedgerRows 读窗口内的账本条目。
func (s *SQLStore) LedgerRows(ctx context.Context, from, to time.Time) ([]LedgerRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("账本结算存储未装配数据库连接")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ledger_id, provider, provider_account, model,
		        tokens_in, tokens_out, cost_credits, occurred_at
		   FROM `+upstream.LedgerTable+`
		  WHERE occurred_at >= $1 AND occurred_at < $2
		  ORDER BY occurred_at, ledger_id`, from, to)
	if err != nil {
		return nil, fmt.Errorf("读取账本条目失败：%w", err)
	}
	defer rows.Close()

	out := make([]LedgerRow, 0, 64)
	for rows.Next() {
		var e LedgerRow
		if err := rows.Scan(&e.LedgerID, &e.Provider, &e.Account, &e.Model,
			&e.TokensIn, &e.TokensOut, &e.Cost, &e.OccurredAt); err != nil {
			return nil, fmt.Errorf("扫描账本条目失败：%w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历账本条目失败：%w", err)
	}
	return out, nil
}

// Link 落一对匹配：账单成本列 + 预扣登记的账本条目。
//
// 两条语句在**同一事务**里：账单挂了成本但登记没挂上，排障时看不出这笔预扣是否真的对上了账。
// 账单侧的 `AND ledger_entry_id IS NULL` 是并发保护（多副本同时跑时只有一方写成功）。
func (s *SQLStore) Link(ctx context.Context, m Match) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("账本结算存储未装配数据库连接")
	}
	if strings.TrimSpace(m.BillingID) == "" || strings.TrimSpace(m.LedgerID) == "" {
		return false, fmt.Errorf("账本结算缺少账单或账本条目 id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("开启账本结算事务失败：%w", err)
	}
	defer tx.Rollback()

	// F10 跨列 CHECK：cost_credits 与 cost_source 必须同时写（只写一个会被 23514 拒）。
	res, err := tx.ExecContext(ctx,
		`UPDATE billing_records
		    SET ledger_entry_id = $2,
		        cost_credits    = $3,
		        cost_source     = 'ledger'
		  WHERE billing_id = $1
		    AND ledger_entry_id IS NULL`, m.BillingID, m.LedgerID, m.Cost)
	if err != nil {
		return false, fmt.Errorf("写入账单成本失败（%s）：%w", m.BillingID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取账单写入行数失败：%w", err)
	}
	if n == 0 {
		return false, nil
	}
	if m.RequestID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE credit_holds SET ledger_entry_id = $2
			  WHERE billing_id = $1 AND status = 'settled'`, m.RequestID, m.LedgerID); err != nil {
			return false, fmt.Errorf("写入预扣登记的账本条目失败（%s）：%w", m.RequestID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("提交账本结算事务失败：%w", err)
	}
	return true, nil
}
