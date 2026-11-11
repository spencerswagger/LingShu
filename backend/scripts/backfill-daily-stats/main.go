// Command backfill-daily-stats 手工执行统计回填（0020：三段 token 实体列 + 日级预聚合）。
//
// 服务启动时会自动跑一次（幂等 + 断点续跑），本命令用于：
//   - 回填失败后补跑（服务不用重启）；
//   - 明确重跑（-reset：清断点从最早数据重算，结果与分批推进一致，覆盖写幂等）；
//   - 单独观测耗时/峰值内存（不进服务启动路径）。
//
// 用法：
//
//	DATABASE_DSN=postgres://... go run ./scripts/backfill-daily-stats
//	DATABASE_DSN=postgres://... go run ./scripts/backfill-daily-stats -reset -days 3
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/team/llmgateway/internal/db"
)

func main() {
	dsn := flag.String("dsn", "", "PostgreSQL DSN（为空取 DATABASE_DSN 环境变量）")
	reset := flag.Bool("reset", false, "清空断点，从最早数据重算（覆盖写，幂等）")
	batchRows := flag.Int("rows", 5000, "token 列回填每批行数")
	days := flag.Int("days", 7, "日聚合每批天数")
	flag.Parse()

	if *dsn == "" {
		*dsn = os.Getenv("DATABASE_DSN")
	}
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "缺少 DSN：用 -dsn 或 DATABASE_DSN 指定")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d, err := db.Open(*dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据库失败: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = d.Close() }()

	if _, err := db.Migrate(ctx, d); err != nil {
		fmt.Fprintf(os.Stderr, "迁移失败: %v\n", err)
		os.Exit(1)
	}
	if err := ensureSchema(ctx, d); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	started := time.Now()
	rep, err := db.BackfillStats(ctx, d, db.StatsBackfillOptions{
		TokenBatchRows: *batchRows,
		DailyBatchDays: *days,
		Reset:          *reset,
		Logger:         slog.Default(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "回填失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("回填完成：token 列 %d 行 / 聚合 %d 天（写入 %d 行）/ 耗时 %.2fs / 峰值内存 %.1f MB (skipped=%v)\n",
		rep.TokenRows, rep.Days, rep.DailyRows,
		time.Since(started).Seconds(), float64(rep.PeakRSSBytes)/1024/1024, rep.Skipped)
}

// ensureSchema 给出比 SQL 报错更可读的前置提示（迁移没跑时最常见的误用）。
func ensureSchema(ctx context.Context, d *sql.DB) error {
	var ok bool
	if err := d.QueryRowContext(ctx,
		`SELECT to_regclass('public.stats_backfill_state') IS NOT NULL`).Scan(&ok); err != nil {
		return fmt.Errorf("检查回填表: %w", err)
	}
	if !ok {
		return fmt.Errorf("stats_backfill_state 不存在：请先执行迁移 0020（本命令会自动跑迁移，若仍失败请检查权限）")
	}
	return nil
}
