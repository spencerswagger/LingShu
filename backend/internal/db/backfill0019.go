package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// 0019 回填（裁定①的「基址随协议」改造 + 物化守卫 initial 标记）。
//
// 为什么有两个入口：
//   - 迁移 0019 里已经用**单语句幂等 UPDATE** 做了同一件事，适用于 ≤5 万行（契约 §10）；
//   - 本文件是 >5 万行时的替代路径：按主键游标分批、每批独立事务、进度写 backfill_checkpoints，
//     失败可断点续跑。之所以不能写进 .sql：db.Migrate 把**每个迁移文件包在一个事务里**
//     （`internal/db/migrate.go`），分批与断点在单事务里没有意义。
//
// 幂等性来源（重复执行不重复写、可中断重启）：
//   - 归档语句带 `ON CONFLICT (channel_id) DO NOTHING`，同一渠道不会写第二份留档；
//   - 清空语句带 `btrim(base_url) <> '' OR btrim(protocol) <> ''` 条件，已清空的行不再命中；
//   - 漂移标记只把 `edited=true` 写成 true，重复执行结果一致；
//   - 游标只前进不回退，checkpoint 里的 last_id 是「已提交的最大主键」。

// 回填阶段名（写入 backfill_checkpoints.name；改名会让存量部署从头再跑一遍，别改）。
const (
	Backfill0019PhaseChannels = "0019_channels"
	Backfill0019PhaseModels   = "0019_channel_models"
)

// defaultBackfillBatchSize 单批行数：单批 < 1s，10 万行时往返次数可控（契约 §10 的取舍）。
const defaultBackfillBatchSize = 5000

// Backfill0019Options 回填参数。
type Backfill0019Options struct {
	// BatchSize 单批行数；<=0 取默认 5000。
	BatchSize int
	// MaxBatchesPerPhase 每个阶段最多跑几批（0=一直跑到没有数据）。用于限速/演练断点续跑。
	MaxBatchesPerPhase int
	// Logf 进度输出；nil 时静默。
	Logf func(format string, args ...any)
}

// Backfill0019Result 回填结果（计数口径：本阶段实际处理的行数，含幂等空跑）。
type Backfill0019Result struct {
	ChannelsScanned  int64 // 扫过的渠道行数
	ChannelsArchived int64 // 本次新写入留档的渠道数（ON CONFLICT 命中不计）
	ModelsScanned    int64 // 扫过的模型行数
	ModelsMarked     int64 // 本次被标 edited=true 的行数
	Batches          int   // 总批次数
}

// Backfill0019 执行 0019 的两段回填（渠道基址归档+清空、模型行漂移标记），可重复执行。
func Backfill0019(ctx context.Context, d *sql.DB, opts Backfill0019Options) (Backfill0019Result, error) {
	batch := opts.BatchSize
	if batch <= 0 {
		batch = defaultBackfillBatchSize
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := EnsureBackfillCheckpoints(ctx, d); err != nil {
		return Backfill0019Result{}, err
	}

	var res Backfill0019Result

	chanRes, err := runBackfillPhase(ctx, d, backfillPhase{
		name:  Backfill0019PhaseChannels,
		label: "渠道内联基址归档并清空",
		pick: `SELECT id FROM channels
		       WHERE id > $1 AND deleted_at IS NULL AND channel_protocol_id IS NOT NULL
		       ORDER BY id ASC LIMIT $2`,
		before:  append([]string{archiveChannelsSQL}, nil...),
		apply:   []string{archiveChannelsSQL, clearChannelsSQL},
		measure: `SELECT count(*) FROM channel_base_url_archive WHERE channel_id = ANY($1::bigint[])`,
	}, batch, opts.MaxBatchesPerPhase, logf)
	if err != nil {
		return res, err
	}
	res.ChannelsScanned = chanRes.scanned
	res.ChannelsArchived = chanRes.measured
	res.Batches += chanRes.batches

	modelRes, err := runBackfillPhase(ctx, d, backfillPhase{
		name:  Backfill0019PhaseModels,
		label: "模型行漂移标记 edited",
		pick: `SELECT id FROM channel_models
		       WHERE id > $1 AND deleted_at IS NULL
		       ORDER BY id ASC LIMIT $2`,
		apply:   []string{markDriftSQL},
		measure: `SELECT count(*) FROM channel_models WHERE id = ANY($1::bigint[]) AND edited`,
	}, batch, opts.MaxBatchesPerPhase, logf)
	if err != nil {
		return res, err
	}
	res.ModelsScanned = modelRes.scanned
	res.ModelsMarked = modelRes.measured
	res.Batches += modelRes.batches
	return res, nil
}

// EnsureBackfillCheckpoints 建断点表（幂等）。表结构刻意最小：一行一个阶段的已提交最大主键。
func EnsureBackfillCheckpoints(ctx context.Context, d *sql.DB) error {
	_, err := d.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS backfill_checkpoints (
    name       TEXT PRIMARY KEY,
    last_id    BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`)
	if err != nil {
		return fmt.Errorf("create backfill_checkpoints: %w", err)
	}
	return nil
}

// BackfillCheckpoint 读取某阶段已处理到的最大主键（无记录返回 0）。
func BackfillCheckpoint(ctx context.Context, d *sql.DB, phase string) (int64, error) {
	var last int64
	err := d.QueryRowContext(ctx, `SELECT last_id FROM backfill_checkpoints WHERE name = $1`, phase).Scan(&last)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read checkpoint %s: %w", phase, err)
	}
	return last, nil
}

// ResetBackfillCheckpoint 清掉某阶段的断点（仅用于演练/重跑；生产不需要）。
func ResetBackfillCheckpoint(ctx context.Context, d *sql.DB, phase string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM backfill_checkpoints WHERE name = $1`, phase)
	if err != nil {
		return fmt.Errorf("reset checkpoint %s: %w", phase, err)
	}
	return nil
}

type backfillPhase struct {
	name    string
	label   string
	pick    string   // 取下一批主键（$1=lastID, $2=limit）
	before  []string // 在 apply 之前、测量之后执行的语句（当前仅用于可读性，允许为空）
	apply   []string // 批次内执行的语句（$1=ids 数组字面量）
	measure string   // 批后测量（$1=ids 数组字面量），用于统计「实际生效」行数
}

type phaseResult struct {
	scanned  int64
	measured int64
	batches  int
}

// runBackfillPhase 按 id 游标循环处理：每批一个事务（语句 + checkpoint 同事务提交）。
func runBackfillPhase(ctx context.Context, d *sql.DB, ph backfillPhase, batch, maxBatches int, logf func(string, ...any)) (phaseResult, error) {
	var out phaseResult
	lastID, err := BackfillCheckpoint(ctx, d, ph.name)
	if err != nil {
		return out, err
	}
	logf("回填[%s] 从断点 last_id=%d 开始（单批 %d）", ph.label, lastID, batch)

	for {
		if maxBatches > 0 && out.batches >= maxBatches {
			logf("回填[%s] 达到本次批数上限 %d，停在 last_id=%d（可重跑续做）", ph.label, maxBatches, lastID)
			return out, nil
		}
		ids, err := pickBackfillIDs(ctx, d, ph.pick, lastID, batch)
		if err != nil {
			return out, err
		}
		if len(ids) == 0 {
			logf("回填[%s] 完成：本阶段共扫描 %d 行 / 生效 %d 行 / %d 批", ph.label, out.scanned, out.measured, out.batches)
			return out, nil
		}
		anyArg := idArrayLiteral(ids)
		maxID := ids[len(ids)-1]

		measured, err := applyBackfillBatch(ctx, d, ph, anyArg, maxID)
		if err != nil {
			return out, err
		}
		out.scanned += int64(len(ids))
		out.measured += measured
		out.batches++
		lastID = maxID
		if out.batches%10 == 0 || len(ids) < batch {
			logf("回填[%s] 进度 last_id=%d 已扫描 %d 行 / 生效 %d 行", ph.label, lastID, out.scanned, out.measured)
		}
	}
}

// applyBackfillBatch 在一个事务里执行批内语句 + 测量 + 推进 checkpoint。
// 事务边界就是「断点粒度」：崩在事务中则该批整体回滚，重启后从 last_id 之后重做（语句幂等）。
func applyBackfillBatch(ctx context.Context, d *sql.DB, ph backfillPhase, anyArg string, maxID int64) (int64, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("%s: begin tx: %w", ph.name, err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range ph.apply {
		if _, err := tx.ExecContext(ctx, stmt, anyArg); err != nil {
			return 0, fmt.Errorf("%s: exec batch: %w", ph.name, err)
		}
	}
	var measured int64
	if ph.measure != "" {
		if err := tx.QueryRowContext(ctx, ph.measure, anyArg).Scan(&measured); err != nil {
			return 0, fmt.Errorf("%s: measure batch: %w", ph.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO backfill_checkpoints(name, last_id, updated_at) VALUES($1, $2, now())
		 ON CONFLICT (name) DO UPDATE SET last_id = EXCLUDED.last_id, updated_at = now()`,
		ph.name, maxID); err != nil {
		return 0, fmt.Errorf("%s: advance checkpoint: %w", ph.name, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("%s: commit batch: %w", ph.name, err)
	}
	return measured, nil
}

func pickBackfillIDs(ctx context.Context, d *sql.DB, query string, lastID int64, limit int) ([]int64, error) {
	rows, err := d.QueryContext(ctx, query, lastID, limit)
	if err != nil {
		return nil, fmt.Errorf("pick backfill ids: %w", err)
	}
	defer rows.Close()
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan backfill id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// idArrayLiteral 把一批主键拼成 PG 数组字面量（{1,2,3}），与仓库既有 bigint[] 传参口径一致。
func idArrayLiteral(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// archiveChannelsSQL 留档被清空的内联基址/协议类型（裁定①：清空前先存一份，保证可无损回滚）。
const archiveChannelsSQL = `
INSERT INTO channel_base_url_archive(channel_id, base_url, protocol)
SELECT id, btrim(base_url), btrim(protocol)
FROM channels
WHERE id = ANY($1::bigint[])
  AND deleted_at IS NULL
  AND channel_protocol_id IS NOT NULL
  AND (btrim(base_url) <> '' OR btrim(protocol) <> '')
ON CONFLICT (channel_id) DO NOTHING`

// clearChannelsSQL 清空「引用协议」渠道的内联基址与协议类型（该值不再参与解析）。
const clearChannelsSQL = `
UPDATE channels
SET base_url = '', protocol = '', updated_at = now()
WHERE id = ANY($1::bigint[])
  AND deleted_at IS NULL
  AND channel_protocol_id IS NOT NULL
  AND (btrim(base_url) <> '' OR btrim(protocol) <> '')`

// markDriftSQL 与迁移 (d) 段同口径：种子定义了的键 + 双方都有值 + 值不同 → edited=true。
const markDriftSQL = `
UPDATE channel_models cm
SET edited = true
FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE cm.id = ANY($1::bigint[])
  AND cm.channel_id = c.id
  AND cm.deleted_at IS NULL
  AND cp.models ? cm.internal_model_id
  AND cp.models -> cm.internal_model_id ?| ARRAY[
        'externalModelID','protocols','costRates','timeConfig','contextTiers']
  AND (
    (cp.models -> cm.internal_model_id ? 'externalModelID'
     AND NULLIF(btrim(cp.models -> cm.internal_model_id ->> 'externalModelID'), '') IS NOT NULL
     AND cm.external_model_id IS DISTINCT FROM
         NULLIF(btrim(cp.models -> cm.internal_model_id ->> 'externalModelID'), '')::bigint)
    OR (cp.models -> cm.internal_model_id ? 'protocols'
        AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'protocols') = 'object'
        AND (cp.models -> cm.internal_model_id -> 'protocols') <> '{}'::jsonb
        AND cm.protocols IS NOT NULL
        AND jsonb_typeof(cm.protocols) = 'object'
        AND cm.protocols <> '{}'::jsonb
        AND cm.protocols IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'protocols'))
    OR (cp.models -> cm.internal_model_id ? 'costRates'
        AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'costRates') = 'object'
        AND cm.cost_rates IS NOT NULL
        AND jsonb_typeof(cm.cost_rates) = 'object'
        AND cm.cost_rates <> '{}'::jsonb
        AND (
             (cp.models -> cm.internal_model_id -> 'costRates' ? 'Input'
              AND (cp.models -> cm.internal_model_id -> 'costRates' ->> 'Input') ~ '^-?[0-9]+(\.[0-9]+)?$'
              AND COALESCE((cm.cost_rates ->> 'input') ~ '^-?[0-9]+(\.[0-9]+)?$', false)
              AND (cm.cost_rates ->> 'input')::numeric
                  IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'costRates' ->> 'Input')::numeric)
          OR (cp.models -> cm.internal_model_id -> 'costRates' ? 'Output'
              AND (cp.models -> cm.internal_model_id -> 'costRates' ->> 'Output') ~ '^-?[0-9]+(\.[0-9]+)?$'
              AND COALESCE((cm.cost_rates ->> 'output') ~ '^-?[0-9]+(\.[0-9]+)?$', false)
              AND (cm.cost_rates ->> 'output')::numeric
                  IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'costRates' ->> 'Output')::numeric)
          OR (cp.models -> cm.internal_model_id -> 'costRates' ? 'CacheRead'
              AND (cp.models -> cm.internal_model_id -> 'costRates' ->> 'CacheRead') ~ '^-?[0-9]+(\.[0-9]+)?$'
              AND COALESCE((cm.cost_rates ->> 'cache_read') ~ '^-?[0-9]+(\.[0-9]+)?$', false)
              AND (cm.cost_rates ->> 'cache_read')::numeric
                  IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'costRates' ->> 'CacheRead')::numeric)
        ))
    OR (cp.models -> cm.internal_model_id ? 'timeConfig'
        AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'timeConfig') = 'object'
        AND cm.time_config IS NOT NULL
        AND jsonb_typeof(cm.time_config) = 'object'
        AND cm.time_config IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'timeConfig'))
    OR (cp.models -> cm.internal_model_id ? 'contextTiers'
        AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'contextTiers') = 'array'
        AND jsonb_array_length(cp.models -> cm.internal_model_id -> 'contextTiers') > 0
        AND cm.context_tiers IS NOT NULL
        AND jsonb_typeof(cm.context_tiers) = 'array'
        AND jsonb_array_length(cm.context_tiers) > 0
        AND cm.context_tiers IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'contextTiers'))
  )`
