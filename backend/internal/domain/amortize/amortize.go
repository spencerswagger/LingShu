// Package amortize 是「订阅月费分摊」：把每个上游账号的固定订阅费按用量摊到当期账单行上（阶段二 A5）。
//
// 为什么需要它：账本给的是**变动成本**（每笔请求实付），订阅费是**固定成本**（按月付一笔）。
// 不摊，盈亏页的「成本」就只剩变动部分，月度毛利会虚高一个订阅费的量级。
//
// 口径（写进代码而不是只写文档）：
//   - 账期：[cycle_start, cycle_start + period)，period 由 billing_cycle 决定（month=+1 月，year=+1 年）；
//     cycle_start 为 NULL 或 billing_cycle='once' → 本期不摊（一次性费用不是「月费」，
//     硬摊到某个月会让那个月的毛利莫名变差）；
//   - 分摊基数：当期账单行的三段 token 合计（与对账/统计同一口径），按比例分配；
//     全部行 token 都为 0 时退化为按行均分（有账单但没用量，仍然要把固定成本摊出去）；
//   - 只摊到「还没有账本成本」的行（ledger_entry_id IS NULL 且 cost_source 为空/none/amortized）：
//     账本成本是变动成本的真源，两者同时写一列会互相覆盖；账本条目迟到时由 ledgersettle 覆盖，
//     下一轮本服务在剩下的行上重新摊（写的是 SET 不是 ADD，所以幂等且能收敛）；
//   - 金额精确：分配后合计**必须**等于订阅价（分币级误差按最大余额法补在 token 最多的那行）。
//
// 不在这层做的事：改用户扣费（credits_consumed 是售价口径）、改订阅记录本身、写 credit_flows。
package amortize

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/decimalx"
)

// 账期种类（与 0019 的 CHECK 同源）。
const (
	CycleMonth = "month"
	CycleYear  = "year"
	CycleOnce  = "once"
)

// Subscription 是一条参与分摊的订阅（0019 的 subscriptions 行）。
type Subscription struct {
	Provider   string
	Account    string
	Price      float64
	Cycle      string
	CycleStart time.Time
}

// Row 是当期一条待摊账单。
type Row struct {
	BillingID string
	Tokens    int64
}

// Share 是一条账单分到的成本。
type Share struct {
	BillingID string
	Cost      float64
}

// AllocateShares 把 price 按 rows 的 tokens 比例分配（纯函数，可单测）。
//
// 三条不变量：
//  1. 返回值的 Cost 合计 == price（分币级精确，误差补在 token 最多的那行）；
//  2. rows 为空时返回 nil（调用方据此记「未摊出去」）；
//  3. 分配顺序稳定（按 billing_id 排序后再算，避免 map 迭代顺序导致数值抖动）。
func AllocateShares(price float64, rows []Row) []Share {
	if len(rows) == 0 || price <= 0 {
		return nil
	}
	sorted := make([]Row, len(rows))
	copy(sorted, rows)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].BillingID < sorted[j].BillingID })

	var total int64
	for _, r := range sorted {
		if r.Tokens > 0 {
			total += r.Tokens
		}
	}
	out := make([]Share, 0, len(sorted))
	allocated := 0.0
	for _, r := range sorted {
		var share float64
		if total > 0 {
			if r.Tokens <= 0 {
				continue // 没用量不分摊（否则会出现「0 用量的行也背成本」）
			}
			share = decimalx.Round5(price * float64(r.Tokens) / float64(total))
		} else {
			share = decimalx.Round5(price / float64(len(sorted)))
		}
		out = append(out, Share{BillingID: r.BillingID, Cost: share})
		allocated = decimalx.Round5(allocated + share)
	}
	if len(out) == 0 {
		return nil
	}
	// 最大余额法：把四舍五入的差额补到 token 最多的那行（合计必须等于 price）。
	if delta := decimalx.Round5(price - allocated); delta != 0 {
		idx := 0
		for i := range out {
			if sorted[i].Tokens > sorted[idx].Tokens {
				idx = i
			}
		}
		out[idx].Cost = decimalx.Round5(out[idx].Cost + delta)
	}
	return out
}

// Store 是分摊的读写端口。
type Store interface {
	// Subscriptions 返回参与分摊的订阅（share_cost=true 且价格>0）。
	Subscriptions(ctx context.Context) ([]Subscription, error)
	// CycleRows 返回该账号在账期内「还没有账本成本」的账单（连 token 合计）。
	CycleRows(ctx context.Context, provider, account string, from, to time.Time) ([]Row, error)
	// Allocate 写入一条账单的分摊成本（SET，幂等）。
	Allocate(ctx context.Context, billingID string, cost float64) (bool, error)
}

// Result 是一轮分摊的汇总（日志/排障用）。
type Result struct {
	// Subscriptions 是参与分摊的订阅数；Skipped 是「本期不摊」的条数（无账期/一次性/价格为 0）。
	Subscriptions int
	Skipped       int
	// Updated 是被写入的账单行数，Allocated 是摊出去的成本合计。
	Updated   int
	Allocated float64
	// Unallocated 是想摊但摊不出去的钱（当期没有可摊账单）——必须告警，不许静默。
	Unallocated float64
}

// Allocator 是分摊服务。
type Allocator struct {
	store Store
	now   func() time.Time
	log   *slog.Logger
}

// Options 是 NewAllocator 的入参。
type Options struct {
	Store  Store
	Now    func() time.Time
	Logger *slog.Logger
}

// NewAllocator 创建分摊服务。
func NewAllocator(opts Options) *Allocator {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Allocator{store: opts.Store, now: now, log: logger}
}

// RunOnce 重算所有订阅当前账期的分摊（幂等：写的是 SET，不是累加）。
func (a *Allocator) RunOnce(ctx context.Context) (Result, error) {
	if a == nil || a.store == nil {
		return Result{}, fmt.Errorf("月费分摊服务未装配存储")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	subs, err := a.store.Subscriptions(ctx)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, s := range subs {
		if s.Price <= 0 || s.CycleStart.IsZero() {
			res.Skipped++
			continue
		}
		if s.Cycle == CycleOnce {
			// 一次性费用不属于按月分摊：摊了等于把它摊成 12 份，毛利会被摊平。
			res.Skipped++
			continue
		}
		from := s.CycleStart
		to := cycleEnd(from, s.Cycle)
		rows, err := a.store.CycleRows(ctx, s.Provider, s.Account, from, to)
		if err != nil {
			return res, err
		}
		shares := AllocateShares(s.Price, rows)
		if len(shares) == 0 {
			res.Unallocated = decimalx.Round5(res.Unallocated + s.Price)
			a.log.Warn("订阅费本期摊不出去（账期内没有可摊账单）",
				"provider", s.Provider, "account", s.Account,
				"price", s.Price, "cycle_start", from.Format(time.RFC3339))
			res.Subscriptions++
			continue
		}
		for _, sh := range shares {
			ok, err := a.store.Allocate(ctx, sh.BillingID, sh.Cost)
			if err != nil {
				return res, err
			}
			if !ok {
				// 该行刚被账本成本占用（ledger_entry_id 非空）→ 本轮跳过，下一轮重算会把它剔出分母。
				continue
			}
			res.Updated++
			res.Allocated = decimalx.Round5(res.Allocated + sh.Cost)
		}
		res.Subscriptions++
	}
	return res, nil
}

// cycleEnd 算账期右界（左闭右开）。未知账期按 1 个月处理（宁可摊窄，也不摊到一年）。
func cycleEnd(start time.Time, cycle string) time.Time {
	switch cycle {
	case CycleYear:
		return start.AddDate(1, 0, 0)
	default:
		return start.AddDate(0, 1, 0)
	}
}

// SQLStore 是 Store 的 PostgreSQL 实现。
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore 创建 SQL 存储。
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

var _ Store = (*SQLStore)(nil)

// Subscriptions 读参与分摊的订阅。
func (s *SQLStore) Subscriptions(ctx context.Context) ([]Subscription, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("月费分摊存储未装配数据库连接")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT provider_ref, account_ref, price_amount, billing_cycle, cycle_start
		   FROM subscriptions
		  WHERE share_cost = true AND price_amount > 0
		  ORDER BY provider_ref, account_ref`)
	if err != nil {
		return nil, fmt.Errorf("读取订阅失败：%w", err)
	}
	defer rows.Close()

	out := make([]Subscription, 0, 8)
	for rows.Next() {
		var (
			sub   Subscription
			start sql.NullTime
		)
		if err := rows.Scan(&sub.Provider, &sub.Account, &sub.Price, &sub.Cycle, &start); err != nil {
			return nil, fmt.Errorf("扫描订阅失败：%w", err)
		}
		if start.Valid {
			sub.CycleStart = start.Time
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历订阅失败：%w", err)
	}
	return out, nil
}

// CycleRows 读账期内「还没有账本成本」的账单。
//
// cost_source 允许 amortized：本服务重算时要能看到上一轮自己写过的行（否则第二轮分母缺行，
// 摊出来的数会跳）。ledger_entry_id IS NOT NULL 的行属于账本口径，绝不参与。
func (s *SQLStore) CycleRows(ctx context.Context, provider, account string, from, to time.Time) ([]Row, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("月费分摊存储未装配数据库连接")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT billing_id, COALESCE(`+billing.TokenSumExpr("")+`, 0)
		   FROM billing_records
		  WHERE upstream_provider = $1 AND upstream_account = $2
		    AND call_time >= $3 AND call_time < $4
		    AND ledger_entry_id IS NULL
		    AND (cost_source IS NULL OR cost_source IN ('none','amortized'))
		  ORDER BY billing_id`, provider, account, from, to)
	if err != nil {
		return nil, fmt.Errorf("读取账期账单失败：%w", err)
	}
	defer rows.Close()

	out := make([]Row, 0, 64)
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.BillingID, &r.Tokens); err != nil {
			return nil, fmt.Errorf("扫描账期账单失败：%w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历账期账单失败：%w", err)
	}
	return out, nil
}

// Allocate 写入一条账单的分摊成本。
//
// 条件里的 `ledger_entry_id IS NULL` 是并发保护：账本成本一旦落地就不再摊（错开就是双记成本）。
// 写回 false 表示该行已被账本占用。
func (s *SQLStore) Allocate(ctx context.Context, billingID string, cost float64) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("月费分摊存储未装配数据库连接")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE billing_records
		    SET cost_credits = $2,
		        cost_source  = 'amortized'
		  WHERE billing_id = $1
		    AND ledger_entry_id IS NULL
		    AND (cost_source IS NULL OR cost_source IN ('none','amortized'))`,
		billingID, decimalx.Round5(cost))
	if err != nil {
		return false, fmt.Errorf("写入分摊成本失败（%s）：%w", billingID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取分摊写入行数失败：%w", err)
	}
	return n > 0, nil
}
