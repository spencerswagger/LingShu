// backfill_stats.go 统计改造（0020）的一次性历史回填：三段 token 实体列 + 日级预聚合。
//
// 为什么不在迁移里做：迁移每个文件一个事务，对大表做全量 UPDATE / GROUP BY 会把
// 启动卡在单个事务上（锁、回滚段、超时都不可控）。这里改成「分批 + 断点续跑 + 幂等」：
//   - 每批独立事务，失败只丢当前批，重启从断点继续；
//   - token 列按主键游标推进（keyset，走 PK 索引）；
//   - 日聚合按自然日分批推进，且每个批次对聚合表是**覆盖写**（ON CONFLICT DO UPDATE 赋新值而不是累加），
//     所以同一批重跑多少次结果都一样；
//   - 只回填「已经过去的日子」（< 今天），今天的格子永远由结算路径实时累加，回填不会覆盖实时数。
package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// 回填断点名（stats_backfill_state.name）。
const (
	BackfillPhaseTokens = "billing_token_columns"
	BackfillPhaseDaily  = "user_daily_stats"
)

// 回填默认批量。
const (
	defaultTokenBatchRows = 20000
	defaultDailyBatchDays = 7
)

// StatsBackfillOptions 回填参数（零值即默认）。
type StatsBackfillOptions struct {
	TokenBatchRows int          // 每批 token 列回填行数（默认 5000）
	DailyBatchDays int          // 每批聚合天数（默认 7）
	Reset          bool         // 清空断点从最早数据重跑（幂等，手动修复用）
	Logger         *slog.Logger // 进度日志（nil 用 slog.Default）
	Today          time.Time    // 「今天」的判定基准（单测注入；零值取 time.Now）
}

// StatsBackfillReport 回填结果（压测与运维观测用）。
type StatsBackfillReport struct {
	TokenRows    int64         // token 列回填行数
	Days         int           // 聚合回填天数
	DailyRows    int64         // 聚合表写入（含更新）行数
	Duration     time.Duration // 总耗时
	PeakRSSBytes uint64        // 进程峰值常驻内存（VmHWM）
	Skipped      bool          // 断点已完成，本次未做任何事
}

// BackfillStats 执行（或续跑）统计回填。
func BackfillStats(ctx context.Context, d *sql.DB, opt StatsBackfillOptions) (StatsBackfillReport, error) {
	started := time.Now()
	var rep StatsBackfillReport
	logr := opt.Logger
	if logr == nil {
		logr = slog.Default()
	}
	if opt.TokenBatchRows <= 0 {
		opt.TokenBatchRows = defaultTokenBatchRows
	}
	if opt.DailyBatchDays <= 0 {
		opt.DailyBatchDays = defaultDailyBatchDays
	}
	today := opt.Today
	if today.IsZero() {
		today = time.Now()
	}

	if opt.Reset {
		if _, err := d.ExecContext(ctx, `DELETE FROM stats_backfill_state WHERE name = ANY($1)`,
			[]string{BackfillPhaseTokens, BackfillPhaseDaily}); err != nil {
			return rep, fmt.Errorf("reset stats backfill state: %w", err)
		}
	}

	// 阶段一：三段 token 实体列（幂等：只补「列为 0 但 JSONB 有值」的行）。
	lastID, done, err := loadBackfillCursor(ctx, d, BackfillPhaseTokens)
	if err != nil {
		return rep, err
	}
	if !done {
		rows, newID, derr := backfillTokenColumns(ctx, d, lastID, opt.TokenBatchRows, func(n int64, maxID int64) {
			logr.Info("stats backfill: token columns batch", "rows", n, "cursor", maxID)
		})
		rep.TokenRows += rows
		if derr != nil {
			return rep, derr
		}
		if err := saveBackfillCursor(ctx, d, BackfillPhaseTokens, newID, true); err != nil {
			return rep, err
		}
	} else {
		logr.Info("stats backfill: token columns already done, skipped")
	}

	// 阶段二：日级预聚合（覆盖写，只到「昨天」）。
	dayCursor, dailyDone, err := loadBackfillCursor(ctx, d, BackfillPhaseDaily)
	if err != nil {
		return rep, err
	}
	if dailyDone {
		logr.Info("stats backfill: daily rollup already done, skipped")
		rep.Skipped = true
	} else {
		firstDay, err := firstBillDay(ctx, d)
		if err != nil {
			return rep, err
		}
		// 回填到「今天」为止（含今天）：回填在开始对外服务之前执行，
		// 所以这一格由回填覆盖写后、由结算路径继续增量累加，两者不会同时写。
		// （多副本滚动重启时理论上会与另一副本的今日增量竞争，见 notes/stats-billing-perf.md 的已知边界。）
		lastDay := dayOf(today)
		cur := firstDay
		if dayCursor > 0 {
			cur = fromDayNumber(dayCursor).AddDate(0, 0, 1)
		}
		for !cur.After(lastDay) {
			end := cur.AddDate(0, 0, opt.DailyBatchDays)
			if end.After(lastDay.AddDate(0, 0, 1)) {
				end = lastDay.AddDate(0, 0, 1)
			}
			n, err := upsertDailyRollup(ctx, d, cur, end)
			if err != nil {
				return rep, err
			}
			rep.DailyRows += n
			rep.Days += int(end.Sub(cur).Hours() / 24)
			if err := saveBackfillCursor(ctx, d, BackfillPhaseDaily, toDayNumber(end.AddDate(0, 0, -1)), false); err != nil {
				return rep, err
			}
			logr.Info("stats backfill: daily rollup batch",
				"from", cur.Format("2006-01-02"), "to", end.AddDate(0, 0, -1).Format("2006-01-02"), "rows", n)
			cur = end
		}
		if err := saveBackfillCursor(ctx, d, BackfillPhaseDaily, 0, true); err != nil {
			return rep, err
		}
	}

	rep.Duration = time.Since(started)
	rep.PeakRSSBytes = peakRSS()
	logr.Info("stats backfill finished",
		"token_rows", rep.TokenRows, "days", rep.Days, "daily_rows", rep.DailyRows,
		"elapsed_ms", rep.Duration.Milliseconds(), "peak_rss_mb", rep.PeakRSSBytes/1024/1024)
	return rep, nil
}

// backfillTokenColumns 按主键游标分批把 tokens JSONB 的三段写到实体列。
// 返回本批行数与新的游标（已处理的最大 id）。
func backfillTokenColumns(ctx context.Context, d *sql.DB, lastID int64, batch int,
	progress func(rows int64, maxID int64)) (int64, int64, error) {
	var total int64
	cur := lastID
	for {
		if err := ctx.Err(); err != nil {
			return total, cur, err
		}
		rows, err := d.QueryContext(ctx, `
WITH batch AS (
    SELECT id FROM billing_records
    WHERE id > $1
      AND input_tokens = 0 AND cache_read_tokens = 0 AND output_tokens = 0
      AND tokens <> '{}'::jsonb
    ORDER BY id
    LIMIT $2
), upd AS (
    UPDATE billing_records b SET
        input_tokens = CASE WHEN (b.tokens->>'input') ~ '^-?[0-9]{1,18}$'
                            THEN (b.tokens->>'input')::bigint ELSE 0 END,
        cache_read_tokens = CASE WHEN (b.tokens->>'cache_read') ~ '^-?[0-9]{1,18}$'
                            THEN (b.tokens->>'cache_read')::bigint ELSE 0 END,
        output_tokens = CASE WHEN (b.tokens->>'output') ~ '^-?[0-9]{1,18}$'
                            THEN (b.tokens->>'output')::bigint ELSE 0 END
    WHERE b.id IN (SELECT id FROM batch)
    RETURNING b.id
)
SELECT count(*), COALESCE(max(id), 0) FROM upd`, cur, batch)
		if err != nil {
			return total, cur, fmt.Errorf("backfill token columns: %w", err)
		}
		var n, maxID int64
		if rows.Next() {
			if err := rows.Scan(&n, &maxID); err != nil {
				rows.Close()
				return total, cur, err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return total, cur, err
		}
		rows.Close()
		if n == 0 {
			return total, cur, nil
		}
		total += n
		cur = maxID
		if err := saveBackfillCursor(ctx, d, BackfillPhaseTokens, cur, false); err != nil {
			return total, cur, err
		}
		if progress != nil {
			progress(n, cur)
		}
	}
}

// upsertDailyRollup 覆盖写 [from, to) 区间内每天的聚合（幂等）。
func upsertDailyRollup(ctx context.Context, d *sql.DB, from, to time.Time) (int64, error) {
	res, err := d.ExecContext(ctx, `
INSERT INTO user_daily_stats
    (user_id, day, requests, input_tokens, cache_read_tokens, output_tokens, credits, cost_credits, updated_at)
SELECT b.user_id,
       (b.call_time AT TIME ZONE 'Asia/Shanghai')::date AS day,
       count(*),
       COALESCE(SUM(b.input_tokens), 0),
       COALESCE(SUM(b.cache_read_tokens), 0),
       COALESCE(SUM(b.output_tokens), 0),
       COALESCE(SUM(b.credits_consumed), 0),
       COALESCE(SUM(b.cost_credits), 0),
       now()
FROM billing_records b
WHERE b.status = 'completed' AND b.call_time >= $1 AND b.call_time < $2
GROUP BY 1, 2
ON CONFLICT (user_id, day) DO UPDATE SET
    requests          = EXCLUDED.requests,
    input_tokens      = EXCLUDED.input_tokens,
    cache_read_tokens = EXCLUDED.cache_read_tokens,
    output_tokens     = EXCLUDED.output_tokens,
    credits           = EXCLUDED.credits,
    cost_credits      = EXCLUDED.cost_credits,
    updated_at        = now()`, from, to)
	if err != nil {
		return 0, fmt.Errorf("upsert daily rollup [%s, %s): %w",
			from.Format("2006-01-02"), to.Format("2006-01-02"), err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// loadBackfillCursor 读断点：返回 last_id 与「是否已完成」。
func loadBackfillCursor(ctx context.Context, d *sql.DB, name string) (int64, bool, error) {
	var lastID int64
	var doneAt sql.NullTime
	err := d.QueryRowContext(ctx,
		`SELECT last_id, done_at FROM stats_backfill_state WHERE name = $1`, name).Scan(&lastID, &doneAt)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("load backfill cursor %s: %w", name, err)
	}
	return lastID, doneAt.Valid, nil
}

// saveBackfillCursor 写断点（done=true 时盖章完成时间）。
func saveBackfillCursor(ctx context.Context, d *sql.DB, name string, lastID int64, done bool) error {
	_, err := d.ExecContext(ctx, `
INSERT INTO stats_backfill_state (name, last_id, done_at, updated_at)
VALUES ($1, $2, CASE WHEN $3 THEN now() ELSE NULL END, now())
ON CONFLICT (name) DO UPDATE SET
    last_id    = EXCLUDED.last_id,
    done_at    = CASE WHEN $3 THEN now() ELSE stats_backfill_state.done_at END,
    updated_at = now()`, name, lastID, done)
	if err != nil {
		return fmt.Errorf("save backfill cursor %s: %w", name, err)
	}
	return nil
}

// firstBillDay 最早一笔账单的自然日（北京时间）；表为空时返回今天（循环自然不执行）。
func firstBillDay(ctx context.Context, d *sql.DB) (time.Time, error) {
	var t sql.NullTime
	if err := d.QueryRowContext(ctx, `SELECT MIN(call_time) FROM billing_records`).Scan(&t); err != nil {
		return time.Time{}, fmt.Errorf("first billing day: %w", err)
	}
	if !t.Valid {
		return dayOf(time.Now()), nil
	}
	return dayOf(t.Time), nil
}

// dayOf 取北京时间自然日 00:00:00。
func dayOf(t time.Time) time.Time {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.Local
	}
	n := t.In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
}

// toDayNumber / fromDayNumber：把自然日压成「自 1970-01-01 起的天数」，断点表只存 BIGINT。
func toDayNumber(day time.Time) int64 { return day.Unix() / 86400 }
func fromDayNumber(n int64) time.Time {
	return time.Unix(n*86400, 0).In(dayOf(time.Now()).Location())
}

// peakRSS 读进程峰值常驻内存（VmHWM）；读不到返回 0（不影响回填）。
func peakRSS() uint64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "VmHWM:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
