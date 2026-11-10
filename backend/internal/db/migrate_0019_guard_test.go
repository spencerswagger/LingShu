package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedOwnershipFixture 造一份覆盖「下移 / 漂移边界」的固定数据集（迁移路径与回填路径共用，
// 保证两条路径的输入完全一致，从而「逐行一致」这个断言才有意义）。
//
// 覆盖的边界（契约 §6.1(c)(d) 五条断言 + 一条范围外）：
//
//	101 m_null_protocols     种子有 protocols、实例 NULL      → 下移写入、不标 edited
//	102 m_conflict_protocols 种子 {a}、实例 {b}                → 不下移、标 edited
//	103 m_cost_1_vs_1_0      种子 Input=1、实例 input=1.0      → 不标 edited（数值相等）
//	104 m_seed_no_cost       种子无 costRates、实例成本不同     → 不标 edited（种子没定义的键不判定）
//	105 m_timecfg_null       种子有 timeConfig、实例 NULL       → 不标 edited、值保持 NULL
//	106 m_seed_no_entry      种子根本没有该模型                 → 不标 edited、不改动
func seedOwnershipFixture(t *testing.T, tempDB *sql.DB) {
	t.Helper()
	exec(t, tempDB, `INSERT INTO external_models(id, external_name) VALUES (2,'ext-a'), (5,'ext-b')`)
	exec(t, tempDB, `
INSERT INTO channel_protocols(id, name, base_url, protocols, request_headers, models)
VALUES (7, 'p7', 'https://proto.example.com', '{"openai-chat":{}}'::jsonb, '{"passthroughMode":"none"}'::jsonb,
 '{
   "m_null_protocols":     {"externalModelID":"2","protocols":{"anthropic-messages":{}}},
   "m_conflict_protocols": {"externalModelID":"2","protocols":{"anthropic-messages":{}}},
   "m_cost_1_vs_1_0":      {"externalModelID":"2","costRates":{"Input":1,"Output":2,"CacheRead":0.1}},
   "m_seed_no_cost":       {"externalModelID":"5"},
   "m_timecfg_null":       {"externalModelID":"2","timeConfig":{"Default":1.5}},
   "m_timecfg_empty":      {"externalModelID":"2","timeConfig":{"Default":2}},
   "m_timecfg_diff":       {"externalModelID":"2","timeConfig":{"Default":3}}
 }'::jsonb)`)
	exec(t, tempDB, `INSERT INTO channels(id, name, base_url, protocol, channel_protocol_id)
		VALUES (1, 'ref', 'https://legacy.example.com', 'openai-compat', 7)`)
	exec(t, tempDB, `INSERT INTO channels(id, name, base_url, protocol)
		VALUES (2, 'inline', 'https://inline.example.com', 'openai-chat')`)

	exec(t, tempDB, `INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates, protocols) VALUES
		(101, 1, 'm_null_protocols',     2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb, NULL),
		(102, 1, 'm_conflict_protocols', 2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb, '{"openai-responses":{}}'::jsonb),
		(103, 1, 'm_cost_1_vs_1_0',      2, '{"input":1.0,"output":2.0,"cache_read":0.1}'::jsonb, NULL),
		(104, 1, 'm_seed_no_cost',       5, '{"input":9,"output":9,"cache_read":9}'::jsonb, NULL),
		(105, 1, 'm_timecfg_null',       2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb, NULL),
		(106, 1, 'm_seed_no_entry',      2, '{"input":5,"output":5,"cache_read":5}'::jsonb, NULL),
		-- P1-① 回归：time_config 为空对象 = 未设置，不得标 edited（否则该行永久冻结）。
		(107, 1, 'm_timecfg_empty',      2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb, NULL),
		-- 对照：双方都有值且不同 → 标 edited。
		(108, 1, 'm_timecfg_diff',       2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb, NULL),
		(201, 2, 'inline-1',             2, '{"input":7,"output":7,"cache_read":7}'::jsonb, NULL)`)
	exec(t, tempDB, `UPDATE channel_models SET time_config = '{}'::jsonb WHERE id = 107`)
	exec(t, tempDB, `UPDATE channel_models SET time_config = '{"Default":9}'::jsonb WHERE id = 108`)
}

// ownershipSnapshot 取「迁移/回填结果」的可比快照：渠道基址、留档、模型行的 protocols/edited/成本/绑定。
func ownershipSnapshot(t *testing.T, tempDB *sql.DB) string {
	t.Helper()
	var b strings.Builder
	rows, err := tempDB.Query(`SELECT id, base_url, protocol FROM channels ORDER BY id`)
	if err != nil {
		t.Fatalf("快照 channels 失败: %v", err)
	}
	for rows.Next() {
		var id int64
		var base, proto string
		if err := rows.Scan(&id, &base, &proto); err != nil {
			t.Fatalf("扫描 channels 失败: %v", err)
		}
		fmt.Fprintf(&b, "ch:%d=%s|%s\n", id, base, proto)
	}
	rows.Close()

	rows, err = tempDB.Query(`SELECT channel_id, base_url, protocol FROM channel_base_url_archive ORDER BY channel_id`)
	if err != nil {
		t.Fatalf("快照 archive 失败: %v", err)
	}
	for rows.Next() {
		var id int64
		var base, proto string
		if err := rows.Scan(&id, &base, &proto); err != nil {
			t.Fatalf("扫描 archive 失败: %v", err)
		}
		fmt.Fprintf(&b, "ar:%d=%s|%s\n", id, base, proto)
	}
	rows.Close()

	rows, err = tempDB.Query(`SELECT id, coalesce(protocols::text,'-'), edited,
		coalesce(cost_rates::text,'-'), external_model_id, state
		FROM channel_models ORDER BY id`)
	if err != nil {
		t.Fatalf("快照 channel_models 失败: %v", err)
	}
	for rows.Next() {
		var id, extID int64
		var protocols, costs, state string
		var edited bool
		if err := rows.Scan(&id, &protocols, &edited, &costs, &extID, &state); err != nil {
			t.Fatalf("扫描 channel_models 失败: %v", err)
		}
		fmt.Fprintf(&b, "cm:%d=%s|edited=%v|%s|%d|%s\n", id, protocols, edited, costs, extID, state)
	}
	rows.Close()
	return b.String()
}

// 回填脚本（迁移被阈值守卫跳过时的唯一数据段执行者）必须与迁移得到**逐行一致**的结果。
func TestBackfill0019_MatchesMigrationResult(t *testing.T) {
	ctx := context.Background()
	migDir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")

	// A：迁移内联回填路径。
	migDB := openTempDB(t)
	applyMigrationsBelow(t, migDB, migDir, "0019")
	seedOwnershipFixture(t, migDB)
	runMigration(t, migDB, migDir, migrationFile(t, migDir, "0019"))
	want := ownershipSnapshot(t, migDB)

	// B：只做 DDL（模拟阈值守卫跳过数据段）→ 全部交给回填脚本。
	bfDB := openTempDB(t)
	applyMigrationsBelow(t, bfDB, migDir, "0019")
	seedOwnershipFixture(t, bfDB)
	exec(t, bfDB, `ALTER TABLE channel_models ADD COLUMN IF NOT EXISTS edited BOOLEAN NOT NULL DEFAULT false`)
	exec(t, bfDB, `CREATE TABLE IF NOT EXISTS channel_base_url_archive (
		channel_id BIGINT PRIMARY KEY, base_url TEXT NOT NULL DEFAULT '',
		protocol TEXT NOT NULL DEFAULT '', archived_at TIMESTAMPTZ NOT NULL DEFAULT now())`)
	if _, err := Backfill0019(ctx, bfDB, Backfill0019Options{BatchSize: 2}); err != nil {
		t.Fatalf("回填失败: %v", err)
	}
	got := ownershipSnapshot(t, bfDB)

	if got != want {
		t.Fatalf("回填结果与迁移结果不一致：\n--- migration ---\n%s\n--- backfill ---\n%s", want, got)
	}
}

// 漂移判定的五条边界（逐条断言，别看快照差异，失败时能直接指出是哪条口径错了）。
func TestMigrate_0019_DriftBoundaries(t *testing.T) {
	tempDB := openTempDB(t)
	migDir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
	applyMigrationsBelow(t, tempDB, migDir, "0019")
	seedOwnershipFixture(t, tempDB)
	runMigration(t, tempDB, migDir, migrationFile(t, migDir, "0019"))

	type row struct {
		protocols string
		edited    bool
	}
	got := map[int64]row{}
	rows, err := tempDB.Query(`SELECT id, coalesce(protocols::text,'-'), edited FROM channel_models ORDER BY id`)
	if err != nil {
		t.Fatalf("读取模型行失败: %v", err)
	}
	for rows.Next() {
		var id int64
		var r row
		if err := rows.Scan(&id, &r.protocols, &r.edited); err != nil {
			t.Fatalf("扫描模型行失败: %v", err)
		}
		got[id] = r
	}
	rows.Close()

	// 1) 实例 NULL + 种子有 → 下移写入且不标 edited（否则子集会永久丢失）。
	if got[101].protocols != `{"anthropic-messages": {}}` || got[101].edited {
		t.Fatalf("101 实例 protocols 为 NULL 时应下移种子值且不标 edited, got %+v", got[101])
	}
	// 2) 实例有值且不同 → 不下移、标 edited（本地显式配置优先）。
	if got[102].protocols != `{"openai-responses": {}}` || !got[102].edited {
		t.Fatalf("102 实例已有不同 protocols 时不应覆盖且应标 edited, got %+v", got[102])
	}
	// 3) 成本 1 vs 1.0 → 数值相等，不标 edited。
	if got[103].edited {
		t.Fatalf("103 成本 1 vs 1.0 属数值相等，不应标 edited, got %+v", got[103])
	}
	// 4) 种子没定义 costRates → 该键与种子无关，不标 edited。
	if got[104].edited {
		t.Fatalf("104 种子无 costRates，不应标 edited, got %+v", got[104])
	}
	// 5) 种子有 timeConfig、实例 NULL → 未设置不算漂移，不标 edited。
	if got[105].edited {
		t.Fatalf("105 实例 time_config 为 NULL 属未设置，不应标 edited, got %+v", got[105])
	}
	var timeRaw []byte
	if err := tempDB.QueryRow(`SELECT time_config FROM channel_models WHERE id=105`).Scan(&timeRaw); err != nil {
		t.Fatalf("读取 time_config 失败: %v", err)
	}
	if timeRaw != nil {
		t.Fatalf("105 下移/漂移都不该碰 time_config，got %s", timeRaw)
	}
	// 5b) P1-① 回归：实例 time_config = '{}' 属未设置，与 cost_rates='{}' 同口径，不标 edited。
	if got[107].edited {
		t.Fatalf("107 实例 time_config='{}' 属未设置，不应标 edited（否则该行永久冻结）, got %+v", got[107])
	}
	// 5c) 对照：实例 time_config 有值与种子不同 → 标 edited。
	if !got[108].edited {
		t.Fatalf("108 实例 time_config 与种子不同，应标 edited, got %+v", got[108])
	}
	// 6) 种子根本没有该模型 → 完全不动。
	if got[106].edited {
		t.Fatalf("106 不在协议种子内，不应标 edited, got %+v", got[106])
	}
	// 7) 内联渠道的模型行不受影响。
	if got[201].edited {
		t.Fatalf("201 内联渠道模型行不应受影响, got %+v", got[201])
	}
}

// 方案 A 的阈值守卫：channel_models 超阈值时迁移只做 DDL，不跑数据段，且启动检查能报警；
// 跑完 scripts/backfill-0019 那条路径后告警消失、数据补齐。
func TestMigrate_0019_ThresholdGuard(t *testing.T) {
	ctx := context.Background()
	tempDB := openTempDB(t)
	migDir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
	applyMigrationsBelow(t, tempDB, migDir, "0019")
	seedOwnershipFixture(t, tempDB)

	// 撑过阈值：批量插入（只填非空列），阈值 50000 → 造 50001 行。
	exec(t, tempDB, `INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates)
		SELECT 1000000 + i, 1, 'bulk-' || i, 2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb
		FROM generate_series(1, 50001) AS i`)
	var live int64
	if err := tempDB.QueryRow(`SELECT count(*) FROM channel_models WHERE deleted_at IS NULL`).Scan(&live); err != nil {
		t.Fatalf("统计行数失败: %v", err)
	}
	if live <= Backfill0019InlineThreshold {
		t.Fatalf("前置条件不成立：构造行数 %d 未超过阈值 %d", live, Backfill0019InlineThreshold)
	}

	runMigration(t, tempDB, migDir, migrationFile(t, migDir, "0019"))

	// DDL 生效（否则独立回填没有执行窗口）。
	if !columnExists(t, tempDB, "channel_models", "edited") {
		t.Fatalf("超阈值时也必须建好 edited 列（DDL 与 DML 分离）")
	}
	if !tableExists(t, tempDB, "channel_base_url_archive") {
		t.Fatalf("超阈值时也必须建好留档表")
	}
	// 数据段被跳过：下移没发生、基址没被清空、留档为空。
	var protocols []byte
	if err := tempDB.QueryRow(`SELECT protocols FROM channel_models WHERE id=101`).Scan(&protocols); err != nil {
		t.Fatalf("读取 101 protocols 失败: %v", err)
	}
	if protocols != nil {
		t.Fatalf("超阈值时迁移内联回填应被跳过（101 仍应 NULL），got %s", protocols)
	}
	var refURL string
	if err := tempDB.QueryRow(`SELECT base_url FROM channels WHERE id=1`).Scan(&refURL); err != nil {
		t.Fatalf("读取渠道基址失败: %v", err)
	}
	if refURL != "https://legacy.example.com" {
		t.Fatalf("超阈值时渠道基址应保持原值待回填, got %q", refURL)
	}
	var archived int
	if err := tempDB.QueryRow(`SELECT count(*) FROM channel_base_url_archive`).Scan(&archived); err != nil {
		t.Fatalf("统计留档失败: %v", err)
	}
	if archived != 0 {
		t.Fatalf("超阈值时不应有留档行, got %d", archived)
	}

	// 启动检查必须报警，并说清下一步命令。
	msgs, err := PendingLargeBackfills(ctx, tempDB)
	if err != nil {
		t.Fatalf("PendingLargeBackfills 失败: %v", err)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0], "backfill-0019") {
		t.Fatalf("超阈值且未回填时应给出告警与命令指引, got %v", msgs)
	}

	// 走独立回填：数据补齐、告警消失。
	if _, err := Backfill0019(ctx, tempDB, Backfill0019Options{BatchSize: 25000}); err != nil {
		t.Fatalf("独立回填失败: %v", err)
	}
	if err := tempDB.QueryRow(`SELECT protocols FROM channel_models WHERE id=101`).Scan(&protocols); err != nil {
		t.Fatalf("读取 101 protocols 失败: %v", err)
	}
	if string(protocols) != `{"anthropic-messages": {}}` {
		t.Fatalf("独立回填后 101 应已下移种子值, got %s", protocols)
	}
	if err := tempDB.QueryRow(`SELECT base_url FROM channels WHERE id=1`).Scan(&refURL); err != nil {
		t.Fatalf("读取渠道基址失败: %v", err)
	}
	if refURL != "" {
		t.Fatalf("独立回填后渠道基址应被清空（原值已留档）, got %q", refURL)
	}
	msgs, err = PendingLargeBackfills(ctx, tempDB)
	if err != nil {
		t.Fatalf("PendingLargeBackfills 失败: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("回填完成后不应再有告警, got %v", msgs)
	}
}

// 归档范围边界：软删渠道不归档、已空渠道不产生留档行、引用「已软删协议」的渠道不归档（协议谓词带 deleted_at）。
func TestMigrate_0019_ArchiveScope(t *testing.T) {
	tempDB := openTempDB(t)
	migDir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
	applyMigrationsBelow(t, tempDB, migDir, "0019")

	exec(t, tempDB, `INSERT INTO channel_protocols(id, name, base_url, models) VALUES (7,'live','https://live.example.com','{}'::jsonb)`)
	exec(t, tempDB, `INSERT INTO channel_protocols(id, name, base_url, models, deleted_at)
		VALUES (8,'gone','https://gone.example.com','{}'::jsonb, now())`)
	// 1 正常（应归档） / 2 已空（不产生留档） / 3 软删（不归档） / 4 引用软删协议（不归档） / 5 内联（不归档）
	exec(t, tempDB, `INSERT INTO channels(id, name, base_url, protocol, channel_protocol_id) VALUES
		(1, 'live-with-url',  'https://legacy-1.example.com', 'openai-compat', 7),
		(2, 'live-empty-url', '', '', 7), -- base_url 与 protocol 都空：真正的「无需留档」
		(3, 'soft-deleted',   'https://legacy-3.example.com', 'openai-compat', 7),
		(4, 'refs-deleted',   'https://legacy-4.example.com', 'openai-compat', 8),
		(5, 'inline',         'https://inline.example.com',   'openai-chat', NULL)`)
	exec(t, tempDB, `UPDATE channels SET deleted_at = now() WHERE id = 3`)

	runMigration(t, tempDB, migDir, migrationFile(t, migDir, "0019"))

	archived := map[int64]bool{}
	rows, err := tempDB.Query(`SELECT channel_id FROM channel_base_url_archive`)
	if err != nil {
		t.Fatalf("读取留档失败: %v", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("扫描留档失败: %v", err)
		}
		archived[id] = true
	}
	rows.Close()

	if !archived[1] {
		t.Fatalf("正常引用协议渠道应被归档")
	}
	if archived[2] {
		t.Fatalf("基址/协议已空的渠道不应产生留档行")
	}
	if archived[3] {
		t.Fatalf("软删渠道不应归档")
	}
	if archived[4] {
		t.Fatalf("引用已软删协议的渠道不应归档（协议谓词须带 deleted_at IS NULL）")
	}
	if archived[5] {
		t.Fatalf("内联渠道不应归档")
	}

	// 软删渠道的 base_url 也不能被清空（它可能被恢复）。
	var url3 string
	if err := tempDB.QueryRow(`SELECT base_url FROM channels WHERE id=3`).Scan(&url3); err != nil {
		t.Fatalf("读取软删渠道失败: %v", err)
	}
	if url3 != "https://legacy-3.example.com" {
		t.Fatalf("软删渠道的 base_url 不应被清空, got %q", url3)
	}
	// 引用已软删协议的渠道保持原值（迁移不碰它；rows_live 仍存在但协议谓词挡住）。
	var url4 string
	if err := tempDB.QueryRow(`SELECT base_url FROM channels WHERE id=4`).Scan(&url4); err != nil {
		t.Fatalf("读取渠道 4 失败: %v", err)
	}
	if url4 != "https://legacy-4.example.com" {
		t.Fatalf("引用软删协议的渠道不应被清空, got %q", url4)
	}
}

// 迁移前跑回填：必须给出可读错误（先说清「先跑迁移」），而不是抛 column does not exist。
func TestBackfill0019_BeforeMigrationFailsReadably(t *testing.T) {
	tempDB := openTempDB(t)
	migDir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
	applyMigrationsBelow(t, tempDB, migDir, "0019") // 只到 0018

	_, err := Backfill0019(context.Background(), tempDB, Backfill0019Options{BatchSize: 10})
	if err == nil {
		t.Fatalf("迁移前跑回填应失败")
	}
	if !strings.Contains(err.Error(), "请先执行迁移") || !strings.Contains(err.Error(), "0019") {
		t.Fatalf("错误信息应指导先跑迁移, got: %v", err)
	}
}

// 回填跑完后再执行迁移 0019：幂等，结果不变（>5 万行的真实顺序）。
func TestMigrate_0019_AfterBackfillIsIdempotent(t *testing.T) {
	ctx := context.Background()
	tempDB := openTempDB(t)
	migDir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
	applyMigrationsBelow(t, tempDB, migDir, "0019")
	seedOwnershipFixture(t, tempDB)
	exec(t, tempDB, `INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates)
		SELECT 2000000 + i, 1, 'bulk2-' || i, 2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb
		FROM generate_series(1, 50001) AS i`)

	// 1) 迁移（超阈值 → 只做 DDL）。
	runMigration(t, tempDB, migDir, migrationFile(t, migDir, "0019"))
	// 2) 独立回填补齐数据段。
	if _, err := Backfill0019(ctx, tempDB, Backfill0019Options{BatchSize: 25000}); err != nil {
		t.Fatalf("回填失败: %v", err)
	}
	afterBackfill := ownershipSnapshot(t, tempDB)

	// 3) 再跑一次迁移：内联回填此时仍会因超阈值跳过，但即便执行也必须是幂等空跑。
	runMigration(t, tempDB, migDir, migrationFile(t, migDir, "0019"))
	if got := ownershipSnapshot(t, tempDB); got != afterBackfill {
		t.Fatalf("回填后再跑迁移改变了结果（应幂等）：\n--- before ---\n%s\n--- after ---\n%s", afterBackfill, got)
	}
	if msgs, err := PendingLargeBackfills(ctx, tempDB); err != nil || len(msgs) != 0 {
		t.Fatalf("回填完成且迁移重跑后不应再有告警: %v / %v", msgs, err)
	}
}

// 契约 §6.1 不变量（回滚正确性的唯一依赖）：**归档集合 ≡ 清空集合**，
// 因而「未归档 ⇒ 未被改动」。这里同时守两层：
//  1. 源码级：迁移 (b) 的归档与清空必须用逐字相同的谓词（防止只改一条语句）；
//  2. 行为级：造 C1~C5 五类渠道，断言 归档集合 = 被清空集合，且未归档渠道**原值逐字未变**。
func TestMigrate_0019_ArchiveEqualsClearSet(t *testing.T) {
	// ---------- 1. 源码级：谓词必须逐字相同 ----------
	migPath := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations", "0019_channel_field_ownership.sql")
	raw, err := os.ReadFile(migPath)
	if err != nil {
		t.Fatalf("读取迁移失败: %v", err)
	}
	predicate := `cp.id = c.channel_protocol_id
      AND cp.deleted_at IS NULL
      AND c.deleted_at IS NULL
      AND (btrim(c.base_url) <> '' OR btrim(c.protocol) <> '')`
	if n := strings.Count(string(raw), predicate); n != 2 {
		t.Fatalf("归档与清空的谓词必须逐字相同且各出现一次（实际命中 %d 次）；"+
			"改了一条语句忘了另一条会破坏「未归档即未改动」", n)
	}

	// ---------- 2. 行为级：五类渠道 ----------
	tempDB := openTempDB(t)
	migDir := filepath.Dir(migPath)
	applyMigrationsBelow(t, tempDB, migDir, "0019")
	exec(t, tempDB, `INSERT INTO channel_protocols(id, name, base_url, models) VALUES (7,'live','https://live.example.com','{}'::jsonb)`)
	exec(t, tempDB, `INSERT INTO channel_protocols(id, name, base_url, models, deleted_at)
		VALUES (8,'gone','https://gone.example.com','{}'::jsonb, now())`)
	exec(t, tempDB, `INSERT INTO channels(id, name, base_url, protocol, channel_protocol_id) VALUES
		(1, 'C1-live-with-url', 'https://c1.example.com', 'openai-compat', 7),
		(2, 'C2-live-empty',    '',                     '',              7),
		(3, 'C3-soft-deleted',  'https://c3.example.com','openai-compat', 7),
		(4, 'C4-refs-gone',     'https://c4.example.com','openai-compat', 8),
		(5, 'C5-inline',        'https://c5.example.com','openai-chat',   NULL)`)
	exec(t, tempDB, `UPDATE channels SET deleted_at = now() WHERE id = 3`)

	// 迁移前的原值（用于断言「未归档即未改动」）。
	type val struct{ base, proto string }
	before := map[int64]val{}
	rows, err := tempDB.Query(`SELECT id, base_url, protocol FROM channels ORDER BY id`)
	if err != nil {
		t.Fatalf("读取迁移前数据失败: %v", err)
	}
	for rows.Next() {
		var id int64
		var v val
		if err := rows.Scan(&id, &v.base, &v.proto); err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		before[id] = v
	}
	rows.Close()

	runMigration(t, tempDB, migDir, migrationFile(t, migDir, "0019"))

	// 迁移后：哪些被归档、哪些被清空。
	archived := map[int64]bool{}
	rows, err = tempDB.Query(`SELECT channel_id FROM channel_base_url_archive`)
	if err != nil {
		t.Fatalf("读取留档失败: %v", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("扫描留档失败: %v", err)
		}
		archived[id] = true
	}
	rows.Close()

	after := map[int64]val{}
	rows, err = tempDB.Query(`SELECT id, base_url, protocol FROM channels ORDER BY id`)
	if err != nil {
		t.Fatalf("读取迁移后数据失败: %v", err)
	}
	for rows.Next() {
		var id int64
		var v val
		if err := rows.Scan(&id, &v.base, &v.proto); err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		after[id] = v
	}
	rows.Close()

	for id, b := range before {
		cleared := after[id].base == "" && after[id].proto == ""
		hadValue := b.base != "" || b.proto != ""
		switch {
		case archived[id]:
			// 归档 ⇒ 必须被清空（否则留档表与库不一致）。
			if hadValue && !cleared {
				t.Fatalf("渠道 %d 已归档但未被清空（%q/%q）", id, after[id].base, after[id].proto)
			}
		default:
			// 未归档 ⇒ 必须原值逐字未变（S1/S2/S3 + 内联渠道）。
			if after[id] != b {
				t.Fatalf("渠道 %d 未归档却被改动：before=%+v after=%+v（违反「未归档 ⇒ 未被改动」）", id, b, after[id])
			}
			if hadValue && cleared {
				t.Fatalf("渠道 %d 出现「被清空但未归档」——回滚会丢地址", id)
			}
		}
	}

	// 具体集合：只有 C1 该被归档；C4（引用软删协议）必须原样保留。
	if !archived[1] {
		t.Fatalf("C1（引用生效协议 + 非空值）应被归档")
	}
	for _, id := range []int64{2, 3, 4, 5} {
		if archived[id] {
			t.Fatalf("渠道 %d 不应被归档（C2 空值 / C3 软删 / C4 引用软删协议 / C5 内联）", id)
		}
	}
	if after[4].base != "https://c4.example.com" {
		t.Fatalf("C4 引用已软删协议，内联值此刻就是生效配置，必须原样保留，got %q", after[4].base)
	}
}

// Gate 非阻断项 2：迁移**内联已完成**、之后行数才涨过阈值的库，不应永久误报「回填未完成」。
// 判定从「看断点表」改为「用与 worker 相同的条件探测还有没有待处理行」，数据已就绪 → PENDING=0。
func TestPendingLargeBackfills_InlineDoneThenGrows(t *testing.T) {
	ctx := context.Background()
	tempDB := openTempDB(t)
	migDir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
	applyMigrationsBelow(t, tempDB, migDir, "0019")
	seedOwnershipFixture(t, tempDB) // 少量数据：迁移会走内联路径

	runMigration(t, tempDB, migDir, migrationFile(t, migDir, "0019"))
	if msgs, err := PendingLargeBackfills(ctx, tempDB); err != nil || len(msgs) != 0 {
		t.Fatalf("内联完成后行数还在阈值内，不应告警: %v / %v", msgs, err)
	}

	// 之后行数涨过阈值（模拟业务增长）：这些新行没有待回填的内容（无种子定义漂移），
	// 断点表也不存在 —— 旧实现会永久误报。
	exec(t, tempDB, `INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates, protocols)
		SELECT 3000000 + i, 1, 'grown-' || i, 2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb,
		       '{"openai-chat":{}}'::jsonb
		FROM generate_series(1, 50001) AS i`)
	var live int64
	if err := tempDB.QueryRow(`SELECT count(*) FROM channel_models WHERE deleted_at IS NULL`).Scan(&live); err != nil {
		t.Fatalf("统计行数失败: %v", err)
	}
	if live <= Backfill0019InlineThreshold {
		t.Fatalf("前置条件：行数 %d 未超过阈值", live)
	}

	msgs, err := PendingLargeBackfills(ctx, tempDB)
	if err != nil {
		t.Fatalf("PendingLargeBackfills 失败: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("内联已完成的大库不应误报（数据已就绪），got %v", msgs)
	}

	// 反向验证：真造一条待回填的数据 → 必须报出来，且跑一次回填后消失。
	exec(t, tempDB, `UPDATE channel_models SET protocols = NULL WHERE id = 101`)
	msgs, err = PendingLargeBackfills(ctx, tempDB)
	if err != nil {
		t.Fatalf("PendingLargeBackfills 失败: %v", err)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0], "下移") {
		t.Fatalf("存在待下移行时应报「协议子集下移」，got %v", msgs)
	}
	if _, err := Backfill0019(ctx, tempDB, Backfill0019Options{BatchSize: 25000}); err != nil {
		t.Fatalf("回填失败: %v", err)
	}
	if msgs, err = PendingLargeBackfills(ctx, tempDB); err != nil || len(msgs) != 0 {
		t.Fatalf("回填后不应再告警: %v / %v", msgs, err)
	}
}

// Gate 非阻断项 1：-reset 必须能重置全部三个阶段（阶段列表单一来源）。
func TestBackfill0019Phases_CoversAll(t *testing.T) {
	phases := Backfill0019Phases()
	want := []string{Backfill0019PhaseChannels, Backfill0019PhaseDownshift, Backfill0019PhaseModels}
	if len(phases) != len(want) {
		t.Fatalf("阶段数应为 %d（含下移），got %d: %v", len(want), len(phases), phases)
	}
	for i := range want {
		if phases[i] != want[i] {
			t.Fatalf("阶段顺序应与迁移一致（归档→下移→漂移），got %v", phases)
		}
	}
}

// 脏种子守卫：externalModelID 出现非整数/空串/科学计数/超 int64 时，
// 迁移必须走完（不回滚、不抛错），并在 channel_seed_skipped 里留下记录（该模型跳过）。
func TestMigrate_0019_DirtySeedExternalModelID(t *testing.T) {
	tempDB := openTempDB(t)
	migDir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
	applyMigrationsBelow(t, tempDB, migDir, "0019")

	exec(t, tempDB, `INSERT INTO external_models(id, external_name) VALUES (2,'ext-a')`)
	exec(t, tempDB, `INSERT INTO channel_protocols(id, name, base_url, models) VALUES (7,'p7','https://proto.example.com','{
		"m-nonnumeric":{"externalModelID":"abc"},
		"m-empty":{"externalModelID":""},
		"m-scientific":{"externalModelID":"1e5"},
		"m-overflow":{"externalModelID":"9223372036854775808"},
		"m-ok":{"externalModelID":"2","costRates":{"Input":1,"Output":2,"CacheRead":0.1}}
	}'::jsonb)`)
	exec(t, tempDB, `INSERT INTO channels(id, name, base_url, protocol, channel_protocol_id)
		VALUES (1,'ref','','',7)`)
	exec(t, tempDB, `INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates) VALUES
		(1, 1, 'm-nonnumeric', 2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb),
		(2, 1, 'm-empty',      2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb),
		(3, 1, 'm-scientific', 2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb),
		(4, 1, 'm-overflow',   2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb),
		(5, 1, 'm-ok',         2, '{"input":9,"output":9,"cache_read":9}'::jsonb)`)

	// 迁移必须整体成功（单个脏值不得回滚事务）。
	runMigration(t, tempDB, migDir, migrationFile(t, migDir, "0019"))

	rows, err := tempDB.Query(`SELECT internal_model_id, reason FROM channel_seed_skipped ORDER BY internal_model_id`)
	if err != nil {
		t.Fatalf("读取跳过记录失败: %v", err)
	}
	defer rows.Close()
	skipped := map[string]string{}
	for rows.Next() {
		var id, reason string
		if err := rows.Scan(&id, &reason); err != nil {
			t.Fatalf("扫描跳过记录失败: %v", err)
		}
		skipped[id] = reason
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	for _, id := range []string{"m-nonnumeric", "m-empty", "m-scientific", "m-overflow"} {
		if _, ok := skipped[id]; !ok {
			t.Fatalf("脏种子 %s 应被记录为跳过，实际记录=%v", id, skipped)
		}
	}
	if _, ok := skipped["m-ok"]; ok {
		t.Fatalf("合法种子 m-ok 不应被跳过")
	}
	if !strings.Contains(skipped["m-overflow"], "超出 int64") {
		t.Fatalf("超范围的原因应可区分, got %q", skipped["m-overflow"])
	}
	if !strings.Contains(skipped["m-empty"], "空串") {
		t.Fatalf("空串的原因应可区分, got %q", skipped["m-empty"])
	}
	if !strings.Contains(skipped["m-nonnumeric"], "非整数") {
		t.Fatalf("非整数的原因应可区分, got %q", skipped["m-nonnumeric"])
	}

	// 合法行照常参与漂移判定（成本偏离 → edited=true）；脏值行不因判定失败而中断。
	var okEdited bool
	if err := tempDB.QueryRow(`SELECT edited FROM channel_models WHERE internal_model_id='m-ok'`).Scan(&okEdited); err != nil {
		t.Fatalf("读取 m-ok edited 失败: %v", err)
	}
	if !okEdited {
		t.Fatalf("m-ok 成本偏离种子，应被标 edited")
	}
}
