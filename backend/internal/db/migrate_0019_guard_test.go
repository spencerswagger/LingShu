package db

import (
	"context"
	"database/sql"
	"fmt"
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
   "m_timecfg_null":       {"externalModelID":"2","timeConfig":{"Default":1.5}}
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
		(201, 2, 'inline-1',             2, '{"input":7,"output":7,"cache_read":7}'::jsonb, NULL)`)
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
