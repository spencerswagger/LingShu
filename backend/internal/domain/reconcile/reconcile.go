// Package reconcile 是 60 秒对账服务：把「灵枢账单」与「上游账本」按窗口比对，
// 并把结果写进 reconcile_windows 的四个计数列（D4 裁定：计数列的唯一写入方）。
//
// 为什么单独一个包（不塞进 upstream 或 billing）：它同时要读两张表的聚合，
// 而 billing 已经 import upstream（归因枚举），把对账放进 upstream 会形成循环依赖；
// 放进 billing 又会让「账单域」知道窗口表。单独一层只依赖两者的**导出 SQL 片段与结构**。
//
// 口径（与 0023 文件头一致）：
//   - 只比 token 合计与请求笔数，**不比金额**（账本币种/牌价与灵枢积分不等价）；
//   - 灵枢侧 tokens = input + cache_read + output（三段互斥，唯一来源是 billing.TokenSumExpr）；
//   - 账本侧 tokens = tokens_in + tokens_out（求和不受「含不含缓存读」的口径影响）；
//   - 偏差率 = |local - upstream| / max(upstream, 1)，超过阈值 → alarm；
//   - 连续 alarm 累加 alarm_streak（>=3 升级审计），ok 归 0；
//   - 账本关闭/降级 → skipped（本窗不参与比对），**不是 blocked**：
//     skipped 是「按口径不比」，blocked 是「该比但没拉完」，混用会让漏账看起来像正常；
//   - 计数写进列，detail 只兜底（偏差率、阈值、两侧合计，方便排障），判定不解析 detail。
package reconcile

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/upstream"
)

// 默认参数。
const (
	// DefaultLookback 是每轮重算的窗口数：窗口边界处迟到的账单/账本条目靠它吸收
	// （迟到一个窗口是常态，重算 10 窗 = 10 分钟容忍度）。
	DefaultLookback = 10
	// DefaultTolerance 是 token 偏差率阈值（1%）。
	DefaultTolerance = 0.01
	// StreakUpgrade 是连击升级阈值（连续 3 窗告警）。
	StreakUpgrade = 3
)

// Agg 是一个窗口的聚合（笔数 + token 合计）。
type Agg struct {
	Count  int64
	Tokens int64
}

// WindowAggSource 按窗口取聚合（本地账单 / 账本各一个实现）。
type WindowAggSource interface {
	WindowAgg(ctx context.Context, start, end time.Time) (Agg, error)
}

// StateReader 读取窗口当前状态（避免覆盖同步轮次持有的 blocked/error）。
type StateReader interface {
	WindowStates(ctx context.Context, from, to time.Time) (map[time.Time]upstream.WindowStatus, error)
}

// StreakReader 读取上一窗的 alert 连击（跨窗累加）。
type StreakReader interface {
	LastAlarmStreak(ctx context.Context, before time.Time) (int16, error)
}

// Result 是一轮对账的汇总（日志/排障用）。
type Result struct {
	From, To time.Time
	Examined int
	OK       int
	Alarm    int
	Skipped  int
	// Deferred 是「本轮没算成」的窗数（同步轮次仍持有 blocked/error）。
	Deferred int
}

// Options 是 NewReconciler 的入参。
type Options struct {
	// Local 是灵枢账单聚合源（billing_records）。
	Local WindowAggSource
	// Upstream 是账本聚合源（ledger_entries）。
	Upstream WindowAggSource
	// Counts 是对账结果写入方（upstream.SQLWindowStore 满足）。
	Counts upstream.CountWriter
	// States 读窗口现状（nil = 不检查，直接写；生产必须给，否则会覆盖 blocked）。
	States StateReader
	// Streaks 读上一窗连击（nil = 每轮从 0 起算，仅测试用）。
	Streaks StreakReader
	// LedgerEnabled 返回账本侧当前是否可用（false → 本窗 skipped）。
	LedgerEnabled func() bool
	// LookbackWindowCount 为 0 时用 DefaultLookback。
	LookbackWindowCount int
	// Tolerance 为 0 时用 DefaultTolerance。
	Tolerance float64
	// Now 为 nil 时用 time.Now（用例注入固定时钟）。
	Now    func() time.Time
	Logger *slog.Logger
}

// Reconciler 执行一轮对账。
type Reconciler struct {
	local     WindowAggSource
	upstream  WindowAggSource
	counts    upstream.CountWriter
	states    StateReader
	streaks   StreakReader
	ledgerOn  func() bool
	lookback  int
	tolerance float64
	now       func() time.Time
	log       *slog.Logger
}

// NewReconciler 创建对账服务（零值不安全：Local/Upstream/Counts 必须给）。
func NewReconciler(opts Options) *Reconciler {
	lookback := opts.LookbackWindowCount
	if lookback <= 0 {
		lookback = DefaultLookback
	}
	tol := opts.Tolerance
	if tol <= 0 {
		tol = DefaultTolerance
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ledgerOn := opts.LedgerEnabled
	if ledgerOn == nil {
		ledgerOn = func() bool { return true }
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{
		local: opts.Local, upstream: opts.Upstream, counts: opts.Counts,
		states: opts.States, streaks: opts.Streaks, ledgerOn: ledgerOn,
		lookback: lookback, tolerance: tol, now: now, log: logger,
	}
}

// RunOnce 重算最近 lookback 个**已结束**窗口的计数。
//
// 为什么每轮重算而不是「只算一次」：账单是异步写（call_logs/billing_records 有毫秒级窗口）、
// 账本条目也可能迟到一两窗。只算一次会把迟到数据记成永久偏差；重算 N 窗是幂等的
// （结果只由两侧聚合决定），成本是 N 次两条聚合查询。
func (r *Reconciler) RunOnce(ctx context.Context) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil || r.local == nil || r.upstream == nil || r.counts == nil {
		return Result{}, fmt.Errorf("对账服务未装配聚合源或写入方")
	}

	// 只算「已结束」的窗口：window_end <= now 才算齐（in-progress 窗还在进数据）。
	end := r.now().Truncate(upstream.WindowSize)
	from := end.Add(-time.Duration(r.lookback) * upstream.WindowSize)
	res := Result{From: from, To: end}

	var states map[time.Time]upstream.WindowStatus
	if r.states != nil {
		var err error
		states, err = r.states.WindowStates(ctx, from, end)
		if err != nil {
			return res, err
		}
	}

	ledgerOn := r.ledgerOn()
	for start := from; start.Before(end); start = start.Add(upstream.WindowSize) {
		winStart := start
		winEnd := start.Add(upstream.WindowSize)
		res.Examined++

		// blocked/error 由同步轮次持有：这一窗还没拉完，比出来的差值没有意义。
		if st, ok := states[winStart]; ok && (st == upstream.WindowBlocked || st == upstream.WindowError) {
			res.Deferred++
			continue
		}

		localAgg, err := r.local.WindowAgg(ctx, winStart, winEnd)
		if err != nil {
			return res, fmt.Errorf("读取灵枢侧窗口 %s 聚合失败：%w", winStart.Format(time.RFC3339), err)
		}
		var upAgg Agg
		if ledgerOn {
			upAgg, err = r.upstream.WindowAgg(ctx, winStart, winEnd)
			if err != nil {
				return res, fmt.Errorf("读取账本侧窗口 %s 聚合失败：%w", winStart.Format(time.RFC3339), err)
			}
		}

		prevStreak := int16(0)
		if r.streaks != nil {
			s, serr := r.streaks.LastAlarmStreak(ctx, winStart)
			if serr != nil {
				return res, serr
			}
			prevStreak = s
		}

		status, streak, detail := decide(localAgg, upAgg, prevStreak, ledgerOn, r.tolerance)
		counts := upstream.WindowCounts{
			Start: winStart, End: winEnd,
			LocalCount: localAgg.Count, LocalTokens: localAgg.Tokens,
			UpstreamCount: upAgg.Count, UpstreamTokens: upAgg.Tokens,
			Status: status, AlarmStreak: streak, Detail: detail, CheckedAt: r.now(),
		}
		if err := r.counts.SaveCounts(ctx, counts); err != nil {
			if err == upstream.ErrWindowNotReconcilable {
				res.Deferred++
				continue
			}
			return res, err
		}
		switch status {
		case upstream.WindowOK:
			res.OK++
		case upstream.WindowAlarm:
			res.Alarm++
			r.log.Warn("对账窗口告警",
				"window", winStart.Format(time.RFC3339),
				"local_tokens", localAgg.Tokens, "upstream_tokens", upAgg.Tokens,
				"streak", streak)
			if streak >= StreakUpgrade {
				r.log.Error("对账连续告警达升级阈值（需要人看）",
					"window", winStart.Format(time.RFC3339), "streak", streak)
			}
		case upstream.WindowSkipped:
			res.Skipped++
		}
	}
	return res, nil
}

// decide 是纯判定（可单测，不碰库）：输入两侧聚合与上一窗连击，输出状态/连击/兜底明细。
func decide(local, up Agg, prevStreak int16, ledgerOn bool, tolerance float64) (
	upstream.WindowStatus, int16, map[string]any) {

	detail := map[string]any{
		"local_count":     local.Count,
		"local_tokens":    local.Tokens,
		"upstream_count":  up.Count,
		"upstream_tokens": up.Tokens,
	}
	if !ledgerOn {
		// 账本侧不可用：本窗不参与比对。**计数列照样写灵枢侧**（不许恒 0），
		// 账本侧留 0 并在 detail 里写原因 —— 「0」在计数列里含义是「账本侧没数据」，
		// 而 skipped 状态说明「这不是偏差」。
		detail["reason"] = "ledger_disabled"
		return upstream.WindowSkipped, 0, detail
	}
	denom := up.Tokens
	if denom <= 0 {
		denom = 1
	}
	diff := local.Tokens - up.Tokens
	if diff < 0 {
		diff = -diff
	}
	rate := float64(diff) / float64(denom)
	detail["deviation_rate"] = rate
	detail["tolerance"] = tolerance
	if rate > tolerance {
		return upstream.WindowAlarm, prevStreak + 1, detail
	}
	return upstream.WindowOK, 0, detail
}

// SQLWindowAgg 用一条聚合 SQL 读某张表在某窗口的笔数与 token 合计。
//
// 两张表的时间列与 token 表达式不同，所以由调用方给出（见 NewLocalSource / NewUpstreamSource）。
type SQLWindowAgg struct {
	db        *sql.DB
	table     string
	timeCol   string
	countExpr string
	tokenExpr string
}

// NewLocalSource 是灵枢侧（billing_records）：笔数 + 三段 token 合计。
//
// token 表达式复用 billing.TokenSumExpr（唯一来源）：三段互斥口径（input 不含缓存读）只在一处定义，
// 这里另写一份 SQL，迟早与账单页/统计页读出两种数。
func NewLocalSource(db *sql.DB) *SQLWindowAgg {
	return &SQLWindowAgg{
		db:        db,
		table:     "billing_records",
		timeCol:   "call_time",
		countExpr: "count(*)",
		tokenExpr: "COALESCE(sum(" + billing.TokenSumExpr("") + "),0)",
	}
}

// NewUpstreamSource 是账本侧（ledger_entries）：笔数 + in+out 合计。
func NewUpstreamSource(db *sql.DB) *SQLWindowAgg {
	return &SQLWindowAgg{
		db:        db,
		table:     upstream.LedgerTable,
		timeCol:   "occurred_at",
		countExpr: "count(*)",
		tokenExpr: "COALESCE(sum(tokens_in + tokens_out),0)",
	}
}

// WindowAgg 实现 WindowAggSource：左闭右开 [start, end)。
func (s *SQLWindowAgg) WindowAgg(ctx context.Context, start, end time.Time) (Agg, error) {
	if s == nil || s.db == nil {
		return Agg{}, fmt.Errorf("对账聚合源未装配数据库连接")
	}
	var out Agg
	err := s.db.QueryRowContext(ctx,
		`SELECT `+s.countExpr+`, `+s.tokenExpr+`
		   FROM `+s.table+`
		  WHERE `+s.timeCol+` >= $1 AND `+s.timeCol+` < $2`, start, end).
		Scan(&out.Count, &out.Tokens)
	if err != nil {
		return Agg{}, fmt.Errorf("聚合 %s 窗口 [%s, %s) 失败：%w",
			s.table, start.Format(time.RFC3339), end.Format(time.RFC3339), err)
	}
	return out, nil
}
