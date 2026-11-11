// dailystats.go 日级预聚合 user_daily_stats 的读写与口径。
//
// 为什么存在：看板查询（本人摘要 / 成本统计）直接扫 billing_records 会随行数线性变慢；
// 实测 3000 万行下按月分区更慢（过滤列 call_time 与分区键不一致，零裁剪），
// 而按 (user_id, day) 的日级预聚合把「几十万行 -> 几十行」，1 亿行仍是常数级。
//
// 写入分两路，互不重复：
//   - 实时：结算事务内 upsert（增量累加），见 UpsertDailyStatsTx；
//   - 历史：启动时一次性回填（按天覆盖），见 internal/db.BackfillStats。
//
// 覆盖只对「已过去的日子」执行，今天的格子永远由实时路径维护，两者不会打架。
package billing

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// statsLocation 聚合切日时区：与前端/账单页一致，按北京时间切日。
func statsLocation() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.Local
	}
	return loc
}

// StatsDay 把时间点归到聚合用的自然日（Asia/Shanghai）：返回当日 00:00:00。
func StatsDay(t time.Time) time.Time {
	n := t.In(statsLocation())
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, statsLocation())
}

// upsertDailyStatsSQL 结算时对聚合表的增量累加。
//
// 同一 billing_id 只会结算一次（settle 前有幂等快速路径 + billing_id 唯一索引），
// 因此这里不需要去重表：重复投递在 insert 阶段就会被唯一约束挡掉，走不到这一步。
const upsertDailyStatsSQL = `
INSERT INTO user_daily_stats
    (user_id, day, requests, input_tokens, cache_read_tokens, output_tokens, credits, cost_credits, updated_at)
VALUES ($1, $2, 1, $3, $4, $5, $6, $7, now())
ON CONFLICT (user_id, day) DO UPDATE SET
    requests          = user_daily_stats.requests + 1,
    input_tokens      = user_daily_stats.input_tokens + EXCLUDED.input_tokens,
    cache_read_tokens = user_daily_stats.cache_read_tokens + EXCLUDED.cache_read_tokens,
    output_tokens     = user_daily_stats.output_tokens + EXCLUDED.output_tokens,
    credits           = user_daily_stats.credits + EXCLUDED.credits,
    cost_credits      = user_daily_stats.cost_credits + EXCLUDED.cost_credits,
    updated_at        = now()`

// upsertDailyStatsTx 在结算事务内累加当日聚合。
//
// 统计是「派生数据」，不能反过来让一笔已扣款成功的账单因为聚合表写失败而回滚：
// 因此用 SAVEPOINT 把这条语句包起来——失败只回滚到保存点，事务与账单照常提交，
// 同时打 WARN（下一轮启动回填会按天重算覆盖，最终一致）。
func upsertDailyStatsTx(ctx context.Context, tx *sql.Tx, rec *Record) error {
	if tx == nil || rec == nil || rec.Status != StatusCompleted {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT daily_stats`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, upsertDailyStatsSQL,
		rec.UserID, StatsDay(rec.CallTime),
		rec.Tokens.Input, rec.Tokens.CacheRead, rec.Tokens.Output,
		rec.CreditsConsumed, rec.CostCredits)
	if err != nil {
		if _, rerr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT daily_stats`); rerr != nil {
			slog.Warn("daily stats savepoint rollback failed",
				"user_id", rec.UserID, "billing_id", rec.BillingID, "err", rerr)
		}
		slog.Warn("daily stats upsert failed, billing kept",
			"user_id", rec.UserID, "billing_id", rec.BillingID, "err", err)
		return err
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT daily_stats`); err != nil {
		return err
	}
	return nil
}

// dailyStatsUpserter 可选能力：Store 实现即表示支持日级预聚合增量累加。
// 不做成 Store 接口的必选方法——既有测试桩（sqlmock/内存桩）无需跟着实现。
type dailyStatsUpserter interface {
	UpsertDailyStats(ctx context.Context, rec *Record)
}

// UpsertDailyStats 非事务累加当日聚合（SqlStore 实现 dailyStatsUpserter）。
func (s *SqlStore) UpsertDailyStats(ctx context.Context, rec *Record) {
	if s == nil {
		return
	}
	upsertDailyStatsOn(ctx, s.db, rec)
}

// upsertDailyStatsOn 非事务路径的增量累加（未装配 txdb 的单测/降级路径）。
// 失败只记日志，不影响账单落库。
func upsertDailyStatsOn(ctx context.Context, ex interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, rec *Record) {
	if ex == nil || rec == nil || rec.Status != StatusCompleted {
		return
	}
	if _, err := ex.ExecContext(ctx, upsertDailyStatsSQL,
		rec.UserID, StatsDay(rec.CallTime),
		rec.Tokens.Input, rec.Tokens.CacheRead, rec.Tokens.Output,
		rec.CreditsConsumed, rec.CostCredits); err != nil {
		slog.Warn("daily stats upsert failed, billing kept",
			"user_id", rec.UserID, "billing_id", rec.BillingID, "err", err)
	}
}

// DailyTotals 某人某日区间（闭区间，按自然日）的聚合结果。口径与 billing.Summarize 一致：
// 只统计 status='completed'（聚合表本身只记 completed 行）。
type DailyTotals struct {
	Requests        int64   `json:"Requests"`
	InputTokens     int64   `json:"InputTokens"`
	CacheReadTokens int64   `json:"CacheReadTokens"`
	OutputTokens    int64   `json:"OutputTokens"`
	CreditsTotal    float64 `json:"CreditsTotal"`
	CostCredits     float64 `json:"CostCredits"`
}

// TokensTotal 三段 token 之和（= Summarize 的 TokensTotal 口径）。
func (d DailyTotals) TokensTotal() int64 {
	return d.InputTokens + d.CacheReadTokens + d.OutputTokens
}

// UserDayTotals 本人看板需要的三档口径：今日 / 昨日 / 本月（至今）。
type UserDayTotals struct {
	TodayCredits     float64
	YesterdayCredits float64
	MonthCredits     float64
	MonthInput       int64
	MonthCacheRead   int64
}

// UserDayTotals 一次查询取今日 / 昨日 / 本月三段聚合（聚合表行数 = 本月天数，几条到几十条）。
// 三个参数都是「自然日边界」：今天、昨天、本月 1 日（均按 Asia/Shanghai）。
func (s *SqlStore) UserDayTotals(ctx context.Context, userID int64, today, yesterday, monthStart time.Time) (UserDayTotals, error) {
	var out UserDayTotals
	err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(credits) FILTER (WHERE day = $2), 0),
       COALESCE(SUM(credits) FILTER (WHERE day = $3), 0),
       COALESCE(SUM(credits) FILTER (WHERE day >= $4), 0),
       COALESCE(SUM(input_tokens) FILTER (WHERE day >= $4), 0),
       COALESCE(SUM(cache_read_tokens) FILTER (WHERE day >= $4), 0)
FROM user_daily_stats
WHERE user_id = $1 AND day BETWEEN LEAST($3, $4) AND $2`,
		userID, StatsDay(today), StatsDay(yesterday), StatsDay(monthStart)).
		Scan(&out.TodayCredits, &out.YesterdayCredits, &out.MonthCredits,
			&out.MonthInput, &out.MonthCacheRead)
	if err != nil {
		return out, fmt.Errorf("user day totals: %w", err)
	}
	return out, nil
}

// DailyTotalsByUser 从聚合表读某人 [from, to] 的合计（按自然日闭区间）。
// 走主键 (user_id, day)：命中行数 = 区间天数，与明细表行数无关。
func (s *SqlStore) DailyTotalsByUser(ctx context.Context, userID int64, from, to time.Time) (DailyTotals, error) {
	var out DailyTotals
	err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(requests), 0),
       COALESCE(SUM(input_tokens), 0),
       COALESCE(SUM(cache_read_tokens), 0),
       COALESCE(SUM(output_tokens), 0),
       COALESCE(SUM(credits), 0),
       COALESCE(SUM(cost_credits), 0)
FROM user_daily_stats
WHERE user_id = $1 AND day >= $2 AND day <= $3`,
		userID, StatsDay(from), StatsDay(to)).
		Scan(&out.Requests, &out.InputTokens, &out.CacheReadTokens, &out.OutputTokens,
			&out.CreditsTotal, &out.CostCredits)
	if err != nil {
		return out, fmt.Errorf("daily totals by user: %w", err)
	}
	return out, nil
}
