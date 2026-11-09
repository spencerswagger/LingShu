// backfill-0019：迁移 0019 的「大表回填」独立执行端（>5 万行时用；≤5 万行直接跑迁移即可）。
//
// 为什么单独一个程序：db.Migrate 把每个迁移文件包在一个事务里，分批 + 断点写不进去
// （契约 §10 的限制说明）。本程序用按主键游标、单批 5000 行、每批独立事务，
// 进度写在 backfill_checkpoints(name, last_id)，中断后重跑自动续做。
//
// 用法：
//
//	cd backend
//	go run ./scripts/backfill-0019 -dsn "$PG_DSN"
//	go run ./scripts/backfill-0019 -dsn "$PG_DSN" -batch 5000 -max-batches 5   # 限速/演练
//
// 幂等：重复执行不会重复写留档、不会重复清空、不会把 edited 反复置位；
// 想从头重跑（演练）加 -reset。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/team/llmgateway/internal/db"
)

func main() {
	var (
		dsn        = flag.String("dsn", "", "数据库 DSN（为空则读 DATABASE_URL / LLMGATEWAY_DSN）")
		batch      = flag.Int("batch", 5000, "单批行数")
		maxBatches = flag.Int("max-batches", 0, "每阶段最多跑几批（0=跑完）")
		reset      = flag.Bool("reset", false, "清掉断点后从头跑（仅演练用）")
		timeout    = flag.Duration("timeout", 30*time.Minute, "总超时")
	)
	flag.Parse()

	target := *dsn
	if target == "" {
		target = os.Getenv("DATABASE_URL")
	}
	if target == "" {
		target = os.Getenv("LLMGATEWAY_DSN")
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "缺少 DSN：用 -dsn 指定，或设置 DATABASE_URL / LLMGATEWAY_DSN")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	conn, err := db.Open(target)
	if err != nil {
		log.Fatalf("连接数据库失败: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if *reset {
		for _, phase := range []string{db.Backfill0019PhaseChannels, db.Backfill0019PhaseModels} {
			if err := db.ResetBackfillCheckpoint(ctx, conn, phase); err != nil {
				log.Fatalf("重置断点失败: %v", err)
			}
		}
		log.Printf("已清空 0019 回填断点（-reset）")
	}

	res, err := db.Backfill0019(ctx, conn, db.Backfill0019Options{
		BatchSize:          *batch,
		MaxBatchesPerPhase: *maxBatches,
		Logf:               log.Printf,
	})
	if err != nil {
		log.Fatalf("回填失败（可原命令重跑续做）: %v", err)
	}
	log.Printf("回填完成：渠道扫描 %d / 新留档 %d；模型扫描 %d / 标记 edited %d；共 %d 批",
		res.ChannelsScanned, res.ChannelsArchived, res.ModelsScanned, res.ModelsMarked, res.Batches)
}
