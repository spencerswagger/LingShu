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
//
// 顺序即契约 §6.1 的顺序：先渠道基址归档清空 (b)，再模型级协议子集下移 (c)，最后漂移标记 (d)。
// (c) 必须在 (d) 之前——否则「实例 protocols 为空」的行会先被判成漂移标 edited，
// 下移再跳过，模型级协议子集永久丢失且该行被冻结。
const (
	Backfill0019PhaseChannels  = "0019_channels"
	Backfill0019PhaseDownshift = "0019_protocols_downshift"
	Backfill0019PhaseModels    = "0019_channel_models"
)

// Backfill0019InlineThreshold 0019 **迁移内联**回填的行数阈值（变量而非常量，
// 便于测试与校验脚本在同进程内临时调高以强制走内联路径；生产部署不要改这个默认值）：
// channel_models 未删除行数 > 该值时，迁移只做 DDL（加列 + 建留档表）并显式告警，
// 数据段（归档清空 / 下移 / 漂移标记）交给 scripts/backfill-0019 分批完成。
//
// 这是**唯一开关**：SQL 侧不写死数字，迁移文件里是占位符 {{backfill_threshold}}，
// 由 renderMigration() 用本常量渲染（见 migrate.go）。改这里即可同时改两条路径。
var Backfill0019InlineThreshold int64 = 50000

// backfillThresholdToken 迁移 SQL 里的阈值占位符。
const backfillThresholdToken = "{{backfill_threshold}}"

// defaultBackfillBatchSize 单批行数：单批 < 1s，10 万行时往返次数可控（契约 §10.2 的取舍）。
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
	ChannelsScanned   int64 // 扫过的渠道行数
	ChannelsArchived  int64 // 本次新写入留档的渠道数（ON CONFLICT 命中不计）
	ModelsScanned     int64 // 扫过的模型行数（下移阶段 + 漂移阶段各自累加）
	ModelsDownshifted int64 // 下移后仍有 protocols 的行数（下移生效面）
	ModelsMarked      int64 // 本次被标 edited=true 的行数
	Batches           int   // 总批次数
}

// Backfill0019 执行 0019 的三段数据回填，顺序与迁移 0019 严格一致、可重复执行：
//
//	(b) 渠道内联基址归档并清空 → (c) 模型级协议子集下移 → (d) 漂移标记 edited。
//
// 与迁移共用同一套 SQL 常量与幂等条件；迁移因超阈值跳过 DML 时，本函数就是唯一的数据段执行者。
func Backfill0019(ctx context.Context, d *sql.DB, opts Backfill0019Options) (Backfill0019Result, error) {
	batch := opts.BatchSize
	if batch <= 0 {
		batch = defaultBackfillBatchSize
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := EnsureBackfill0019Schema(ctx, d); err != nil {
		return Backfill0019Result{}, err
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
		apply:       []string{archiveChannelsSQL, clearChannelsSQL},
		countWrites: true,
	}, batch, opts.MaxBatchesPerPhase, logf)
	if err != nil {
		return res, err
	}
	res.ChannelsScanned = chanRes.scanned
	res.ChannelsArchived = chanRes.measured
	res.Batches += chanRes.batches

	// (c) 模型级协议子集下移：必须在漂移标记之前（契约 §6.1 顺序条款）。
	downRes, err := runBackfillPhase(ctx, d, backfillPhase{
		name:  Backfill0019PhaseDownshift,
		label: "模型级协议子集下移（种子 → 实例）",
		pick: `SELECT id FROM channel_models
		       WHERE id > $1 AND deleted_at IS NULL
		       ORDER BY id ASC LIMIT $2`,
		apply:       []string{downshiftProtocolsSQL},
		countWrites: true,
	}, batch, opts.MaxBatchesPerPhase, logf)
	if err != nil {
		return res, err
	}
	res.ModelsScanned += downRes.scanned
	res.ModelsDownshifted = downRes.measured
	res.Batches += downRes.batches

	modelRes, err := runBackfillPhase(ctx, d, backfillPhase{
		name:  Backfill0019PhaseModels,
		label: "模型行漂移标记 edited",
		pick: `SELECT id FROM channel_models
		       WHERE id > $1 AND deleted_at IS NULL
		       ORDER BY id ASC LIMIT $2`,
		// 首条语句的 RowsAffected 用于计数（本次新标记行）；脏种子记录挂在其后，不影响计数。
		apply:       []string{markDriftSQL, recordSkippedSeedsSQL},
		countWrites: true,
	}, batch, opts.MaxBatchesPerPhase, logf)
	if err != nil {
		return res, err
	}
	res.ModelsScanned += modelRes.scanned
	res.ModelsMarked = modelRes.measured
	res.Batches += modelRes.batches
	return res, nil
}

// PendingLargeBackfills 返回「迁移因超阈值跳过了内联回填、且**数据确实还没就绪**」的告警文案（无待办返回空）。
//
// 判定口径：先看行数是否超过内联阈值，再用与 worker **完全相同**的条件逐阶段 EXISTS 探测
// 「还有没有待处理的行」。
//
// 为什么不能只看断点表：迁移内联跑完、之后行数才涨过阈值的库，断点表压根不存在（或停在旧主键），
// 只看断点会永久误报「回填未完成」，而实际每次跑都是空跑。用 pending-work 探测可以识别
// 「数据已就绪」状态，避免无谓告警。
//
// 供应用启动时打 ERROR 级日志（契约 §10.2 方案 A 的配套要求）：说清「还差什么 + 跑什么命令」，
// 不阻止启动——回填完成前网关仍可服务；实例行缺 protocols 会退化成协议级全集，属行为放大、不是崩溃。
func PendingLargeBackfills(ctx context.Context, d *sql.DB) ([]string, error) {
	var live int64
	if err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM channel_models WHERE deleted_at IS NULL`).Scan(&live); err != nil {
		return nil, fmt.Errorf("count channel_models: %w", err)
	}
	if live <= Backfill0019InlineThreshold {
		return nil, nil
	}
	// 结构未就绪（迁移还没跑）时不在这里报——那是迁移自身的事，别混进回填告警。
	if err := EnsureBackfill0019Schema(ctx, d); err != nil {
		return nil, nil
	}

	chanWork, downWork, driftWork, err := backfillPendingWork(ctx, d)
	if err != nil {
		return nil, err
	}
	var pending []string
	if chanWork {
		pending = append(pending, "渠道内联基址归档并清空")
	}
	if downWork {
		pending = append(pending, "模型级协议子集下移")
	}
	if driftWork {
		pending = append(pending, "模型行漂移标记 edited")
	}
	if len(pending) == 0 {
		return nil, nil
	}
	return []string{fmt.Sprintf(
		"channel_models 现有 %d 行（内联回填阈值 %d），仍有未回填的数据：%s。"+
			"请执行 `cd backend && go run ./scripts/backfill-0019 -dsn \"$DSN\"`（可中断重跑、幂等）；"+
			"完成前实例行缺少模型级协议子集，可达协议会退化为协议级全集。",
		live, Backfill0019InlineThreshold, strings.Join(pending, "、"))}, nil
}

// EnsureBackfill0019Schema 校验 0019 的结构是否就绪（`channel_models.edited` 列 + 留档表）。
//
// 迁移**之前**跑回填会缺列，SQL 层面报的是 "column edited does not exist"，
// 对运维不可读；这里提前拦住并说清「先跑迁移」。
func EnsureBackfill0019Schema(ctx context.Context, d *sql.DB) error {
	// 脏种子留档表属于 0019 结构的一部分；补齐建表（幂等），使回填在任何一次 0019 之后都能记录。
	if _, err := d.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS channel_seed_skipped (
    channel_id        BIGINT NOT NULL,
    internal_model_id TEXT   NOT NULL,
    reason            TEXT   NOT NULL,
    raw_value         TEXT   NOT NULL,
    skipped_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (channel_id, internal_model_id)
)`); err != nil {
		return fmt.Errorf("ensure channel_seed_skipped: %w", err)
	}
	var hasColumn, hasArchive bool
	if err := d.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                WHERE table_name='channel_models' AND column_name='edited')`).Scan(&hasColumn); err != nil {
		return fmt.Errorf("check channel_models.edited: %w", err)
	}
	if err := d.QueryRowContext(ctx,
		`SELECT to_regclass('public.channel_base_url_archive') IS NOT NULL`).Scan(&hasArchive); err != nil {
		return fmt.Errorf("check channel_base_url_archive: %w", err)
	}
	if !hasColumn || !hasArchive {
		missing := make([]string, 0, 2)
		if !hasColumn {
			missing = append(missing, "channel_models.edited 列")
		}
		if !hasArchive {
			missing = append(missing, "channel_base_url_archive 留档表")
		}
		return fmt.Errorf("0019 结构未就绪（缺少：%s）：请先执行迁移 0019（启动后端或 make migrate），再跑本回填",
			strings.Join(missing, "、"))
	}
	return nil
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
	name  string
	label string
	pick  string   // 取下一批主键（$1=lastID, $2=limit）
	apply []string // 批次内执行的语句（$1=ids 数组字面量）
	// 「本批生效行数」= 第一条 apply 语句的 RowsAffected（本次真正写入/标记的行数）。
	// 不用「批内某表 count(*)」——那会把历史行也数进来，-reset 重跑会虚报。
	// 前提：各阶段的第一条语句都只命中「尚未处理」的行（SQL 里带幂等条件）。
	countWrites bool
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

	var measured int64
	for i, stmt := range ph.apply {
		res, err := tx.ExecContext(ctx, stmt, anyArg)
		if err != nil {
			return 0, fmt.Errorf("%s: exec batch: %w", ph.name, err)
		}
		if ph.countWrites && i == 0 {
			// 只统计本次真正写入的行（幂等重放时 ON CONFLICT 命中的不算）。
			measured, _ = res.RowsAffected()
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
// archivedChannelPredicate 是「归档」与「清空」必须**逐字相同**的谓词（契约 §6.1 不变量）。
//
// 为什么必须是同一个字符串：归档集合 ≡ 清空集合 ⇒ 「未归档 ⇒ 未被改动」，回滚才可能无损、
// 且不依赖任何外部备份。若有人只给其中一条加条件（例如忘了 join 协议 deleted_at），
// 回滚就会静默丢地址——所以这里用同一个常量拼两条语句，不给「改一处漏一处」留缝。
//
// 口径（§6.1 S1~S3）：只处理「引用了**仍在生效**协议」的渠道。协议已软删时运行时会
// 回落读 channels.base_url/protocol（lookupChannelProtocol 过滤 deleted_at），
// 这两个内联值此刻就是生效配置，清空等于把渠道打向空地址。
const archivedChannelPredicate = `cp.id = c.channel_protocol_id
  AND cp.deleted_at IS NULL
  AND c.deleted_at IS NULL
  AND (btrim(c.base_url) <> '' OR btrim(c.protocol) <> '')`

// archivedChannelBatchFilter 批内主键过滤：两条语句共用同一份，保证批内集合也相等。
const archivedChannelBatchFilter = `AND c.id = ANY($1::bigint[])`

// archiveChannelsSQL 留档被清空的内联基址/协议类型（清空前先存一份，保证可无损回滚）。
const archiveChannelsSQL = `
INSERT INTO channel_base_url_archive(channel_id, base_url, protocol)
SELECT c.id, btrim(c.base_url), btrim(c.protocol)
FROM channels c
CROSS JOIN channel_protocols cp
WHERE ` + archivedChannelPredicate + `
  ` + archivedChannelBatchFilter + `
ON CONFLICT (channel_id) DO NOTHING`

// clearChannelsSQL 清空「引用生效协议」渠道的内联基址与协议类型（该值不再参与解析）。
// 谓词与 archiveChannelsSQL 完全相同（见 archivedChannelPredicate）。
const clearChannelsSQL = `
UPDATE channels c
SET base_url = '', protocol = '', updated_at = now()
FROM channel_protocols cp
WHERE ` + archivedChannelPredicate + `
  ` + archivedChannelBatchFilter

// ---- 模型行两个阶段（下移 / 漂移）----
//
// worker 语句与「是否还有待处理数据」的判定共用同一份 join 与条件常量：
// 这样「启动告警说还差什么」与「脚本实际做什么」永远同口径，不会出现「判定说没做完、跑了却无事可做」。

// seedModelJoin 实例行 × 所属渠道 × 生效协议的公共连接（下移与漂移共用）。
const seedModelJoin = `FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL`

// seedModelProbeFrom 同 seedModelJoin，但把 channel_models 也放进 FROM——
// 用于「还有没有待处理行」的 SELECT 探测（UPDATE 语句里 cm 已是目标表，不能重复出现在 FROM）。
const seedModelProbeFrom = `FROM channel_models cm CROSS JOIN channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL`

// downshiftCondition 下移阶段的判定条件（不含批内主键过滤）：种子有非空 object 且实例为空。
const downshiftCondition = `cm.channel_id = c.id
  AND cm.deleted_at IS NULL
  AND cp.models ? cm.internal_model_id
  AND cp.models -> cm.internal_model_id ? 'protocols'
  AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'protocols') = 'object'
  AND (cp.models -> cm.internal_model_id -> 'protocols') <> '{}'::jsonb
  AND (cm.protocols IS NULL OR cm.protocols = '{}'::jsonb OR jsonb_typeof(cm.protocols) <> 'object')`

// driftCondition 漂移标记的判定条件（不含批内主键过滤）。
// 口径：种子定义了的键 + 双方都有值且都非空 + 值不等（成本逐键 numeric 比较）。
// `cm.edited = false` 让 RowsAffected = 本次新标记数，重复执行即空跑。
const driftCondition = `cm.channel_id = c.id
  AND cm.deleted_at IS NULL
  AND cm.edited = false
  AND cp.models ? cm.internal_model_id
  AND cp.models -> cm.internal_model_id ?| ARRAY['externalModelID','protocols','costRates','timeConfig','contextTiers']
  AND (
    -- 对外模型绑定：种子有值、实例有值且不同。
    -- 脏种子守卫（与迁移同口径）：CASE 惰性求值，只有形如整数的值才会进 ::numeric/::bigint，
    -- 非整数或超出 int64 的值不会抛错，而是「该模型跳过」（迁移侧会记入 channel_seed_skipped）。
    (cp.models -> cm.internal_model_id ? 'externalModelID'
     AND (CASE WHEN btrim(cp.models -> cm.internal_model_id ->> 'externalModelID') ~ '^-?[0-9]+$'
               THEN btrim(cp.models -> cm.internal_model_id ->> 'externalModelID')::numeric
                    BETWEEN -9223372036854775808 AND 9223372036854775807
               ELSE false END)
     AND cm.external_model_id IS DISTINCT FROM
         (CASE WHEN btrim(cp.models -> cm.internal_model_id ->> 'externalModelID') ~ '^-?[0-9]+$'
                    AND btrim(cp.models -> cm.internal_model_id ->> 'externalModelID')::numeric
                        BETWEEN -9223372036854775808 AND 9223372036854775807
               THEN btrim(cp.models -> cm.internal_model_id ->> 'externalModelID')::bigint
          END))
    -- 模型级协议子集：双方都有内容（空对象=未设置）且不同（键序无关）。
    OR (cp.models -> cm.internal_model_id ? 'protocols'
        AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'protocols') = 'object'
        AND (cp.models -> cm.internal_model_id -> 'protocols') <> '{}'::jsonb
        AND cm.protocols IS NOT NULL
        AND jsonb_typeof(cm.protocols) = 'object'
        AND cm.protocols <> '{}'::jsonb
        AND cm.protocols IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'protocols'))
    -- 成本三段：逐键按 numeric 比较（仅比较种子定义了的键）。
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
    -- 时段系数：双方都有内容（空对象=未设置）且不同。
    OR (cp.models -> cm.internal_model_id ? 'timeConfig'
        AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'timeConfig') = 'object'
        AND (cp.models -> cm.internal_model_id -> 'timeConfig') <> '{}'::jsonb
        AND cm.time_config IS NOT NULL
        AND jsonb_typeof(cm.time_config) = 'object'
        AND cm.time_config <> '{}'::jsonb
        AND cm.time_config IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'timeConfig'))
    -- 上下文分档：双方都有内容（空数组=未设置）且不同。
    OR (cp.models -> cm.internal_model_id ? 'contextTiers'
        AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'contextTiers') = 'array'
        AND jsonb_array_length(cp.models -> cm.internal_model_id -> 'contextTiers') > 0
        AND cm.context_tiers IS NOT NULL
        AND jsonb_typeof(cm.context_tiers) = 'array'
        AND jsonb_array_length(cm.context_tiers) > 0
        AND cm.context_tiers IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'contextTiers'))
  )`

// markDriftSQL 与迁移 (d) 段同口径（条件见 driftCondition），按批处理。
const markDriftSQL = `
UPDATE channel_models cm
SET edited = true
` + seedModelJoin + `
WHERE cm.id = ANY($1::bigint[])
  AND ` + driftCondition

// downshiftProtocolsSQL 与迁移 (c) 段同口径（条件见 downshiftCondition），按批处理。
const downshiftProtocolsSQL = `
UPDATE channel_models cm
SET protocols = cp.models -> cm.internal_model_id -> 'protocols',
    updated_at = now()
` + seedModelJoin + `
WHERE cm.id = ANY($1::bigint[])
  AND ` + downshiftCondition

// backfillPendingWork 判断三阶段是否**还有待处理的数据**（数据已就绪则都没有）。
//
// 为什么不能只看断点表：迁移内联跑完之后行数才涨过阈值的库，断点表压根不存在，
// 会永久误报「回填未完成」。这里直接用与 worker 相同的条件做 EXISTS 探测（命中 1 行即停），
// 判定与「真跑一遍会不会改数据」严格一致。
func backfillPendingWork(ctx context.Context, d *sql.DB) (channels, downshift, drift bool, err error) {
	probe := func(query string) (bool, error) {
		var hit bool
		if err := d.QueryRowContext(ctx, query).Scan(&hit); err != nil {
			return false, err
		}
		return hit, nil
	}

	channels, err = probe(`SELECT EXISTS (SELECT 1 FROM channels c CROSS JOIN channel_protocols cp
		WHERE ` + archivedChannelPredicate + `)`)
	if err != nil {
		return false, false, false, fmt.Errorf("probe channels archive: %w", err)
	}
	downshift, err = probe(`SELECT EXISTS (SELECT 1 ` + seedModelProbeFrom + `
		WHERE ` + downshiftCondition + `)`)
	if err != nil {
		return false, false, false, fmt.Errorf("probe protocols downshift: %w", err)
	}
	drift, err = probe(`SELECT EXISTS (SELECT 1 ` + seedModelProbeFrom + `
		WHERE ` + driftCondition + `)`)
	if err != nil {
		return false, false, false, fmt.Errorf("probe drift marking: %w", err)
	}
	return channels, downshift, drift, nil
}

// Backfill0019Phases 返回三阶段的稳定顺序；脚本的 -reset 与调用方都从这里取，避免漏掉某个阶段。
func Backfill0019Phases() []string {
	return []string{Backfill0019PhaseChannels, Backfill0019PhaseDownshift, Backfill0019PhaseModels}
}

// recordSkippedSeedsSQL 与迁移同口径：把 externalModelID 不可安全绑定（非整数/空串/超 int64）
// 的种子行记入 channel_seed_skipped，供排障。批量执行也写，保证 >5 万行走独立回填时同样可见。
const recordSkippedSeedsSQL = `
INSERT INTO channel_seed_skipped(channel_id, internal_model_id, reason, raw_value)
SELECT cm.channel_id,
       cm.internal_model_id,
       CASE WHEN btrim(cp.models -> cm.internal_model_id ->> 'externalModelID') = ''
                 THEN 'externalModelID 为空串（视为未绑定，跳过）'
            WHEN btrim(cp.models -> cm.internal_model_id ->> 'externalModelID') ~ '^-?[0-9]+$'
                 THEN 'externalModelID 超出 int64 范围'
            ELSE 'externalModelID 非整数字面量' END,
       COALESCE(cp.models -> cm.internal_model_id ->> 'externalModelID', '')
FROM channel_models cm
JOIN channels c ON c.id = cm.channel_id AND c.deleted_at IS NULL
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE cm.id = ANY($1::bigint[])
  AND cm.deleted_at IS NULL
  AND cp.models ? cm.internal_model_id
  AND cp.models -> cm.internal_model_id ? 'externalModelID'
  AND NOT (CASE WHEN btrim(cp.models -> cm.internal_model_id ->> 'externalModelID') ~ '^-?[0-9]+$'
                THEN btrim(cp.models -> cm.internal_model_id ->> 'externalModelID')::numeric
                     BETWEEN -9223372036854775808 AND 9223372036854775807
                ELSE false END)
ON CONFLICT (channel_id, internal_model_id) DO NOTHING`
