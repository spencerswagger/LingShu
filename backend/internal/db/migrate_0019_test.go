package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMigrate_0019_FieldOwnership 验证迁移 0019（RULED-v2：基址随协议 + 物化守卫）的断言场景，
// 以及回滚脚本能把库恢复到「老代码可跑且数据无损」的状态。
//
// 运行方式（本地 test PG）：
//
//	cd backend
//	. ../../.keys/env.sh 2>/dev/null; LLMGATEWAY_TEST_PG_DSN="$PG_TEST_DSN" \
//	  go test ./internal/db -run TestMigrate_0019 -v
//
// 未设置 LLMGATEWAY_TEST_PG_DSN 时 t.Skip。
//
// 断言清单（对应契约 RULED-v2 §6.1 / §9 裁定）：
//  1. 新增业务列只有 `channel_models.edited`；**没有** convert_enabled / convert_targets（裁定②）；
//  2. 引用协议的渠道：内联 base_url/protocol 先归档进 channel_base_url_archive 再清空（裁定①）；
//  3. 内联渠道的 base_url/protocol 完全不动；
//  4. 种子里有、实例里空的模型级 protocols → 迁移后落进实例列（运行时不再回落种子）；
//  5. 漂移标记 edited：种子定义了且双方都有值且不等 → true；种子没定义的键 → 不标；
//  6. B1 回归：种子缺 costRates 的行，迁移不得把 cost_rates 写成 null；
//  7. 幂等：同一迁移连跑两次结果一致；
//  8. 回滚：base_url/protocol 从留档表还原、edited 列与留档表被删、模型行数据保真。
func TestMigrate_0019_FieldOwnership(t *testing.T) {
	tempDB := openTempDB(t)
	migDir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
	applyMigrationsBelow(t, tempDB, migDir, "0019")

	// ---------- 造数：对外模型 / 协议（含种子）/ 引用协议渠道 / 内联渠道 / 四种模型行 ----------
	exec(t, tempDB, `INSERT INTO external_models(id, external_name) VALUES (2, 'ext-a'), (5, 'ext-b')`)
	exec(t, tempDB, `
INSERT INTO channel_protocols(id, name, base_url, protocols, request_headers, models)
VALUES (7, 'p7', 'https://proto.example.com',
        '{"openai-chat":{"baseUrl":"https://proto-chat.example.com"}}'::jsonb,
        '{"passthroughMode":"none"}'::jsonb,
        '{
          "qwen-max": {
            "externalModelID": "2",
            "costRates": {"Input":1,"Output":2,"CacheRead":0.1},
            "protocols": {"anthropic-messages":{}},
            "convertEnabled": false
          },
          "qwen-plus": {"externalModelID": "5"},
          "qwen-drift": {
            "externalModelID": "2",
            "costRates": {"Input":3,"Output":4,"CacheRead":0.3}
          }
        }'::jsonb)`)
	// 渠道 1：引用协议 + 遗留内联基址（应被归档并清空）；渠道 2：内联（完全不动）。
	exec(t, tempDB, `INSERT INTO channels(id, name, base_url, protocol, channel_protocol_id)
		VALUES (1, 'ref', 'https://legacy.example.com', 'openai-compat', 7)`)
	exec(t, tempDB, `INSERT INTO channels(id, name, base_url, protocol)
		VALUES (2, 'inline', 'https://inline.example.com', 'openai-chat')`)

	// 渠道1/qwen-max：成本与种子一致、protocols 为空 → 不标 edited，protocols 由种子补齐。
	exec(t, tempDB, `INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates, protocols)
		VALUES (101, 1, 'qwen-max', 2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb, NULL)`)
	// 渠道1/qwen-plus：种子没定义 costRates → 成本不参与漂移判定（即使数值不同也不标）。
	exec(t, tempDB, `INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates)
		VALUES (102, 1, 'qwen-plus', 5, '{"input":9,"output":9,"cache_read":9}'::jsonb)`)
	// 渠道1/qwen-drift：种子定义了 costRates 且与实例不同 → 标 edited。
	exec(t, tempDB, `INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates)
		VALUES (103, 1, 'qwen-drift', 2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb)`)
	// 渠道2/inline-1：内联渠道的模型行 → 完全不动。
	exec(t, tempDB, `INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates)
		VALUES (104, 2, 'inline-1', 2, '{"input":7,"output":7,"cache_read":7}'::jsonb)`)

	before := snapshotModelRows(t, tempDB)

	// ---------- 执行 0019 ----------
	runMigration(t, tempDB, migDir, migrationFile(t, migDir, "0019"))

	// ---------- 1) 列与表：只有 edited，没有 convert_*；留档表已建 ----------
	if !columnExists(t, tempDB, "channel_models", "edited") {
		t.Fatalf("channel_models 缺少列 edited")
	}
	for _, col := range []string{"convert_enabled", "convert_targets"} {
		if columnExists(t, tempDB, "channel_models", col) {
			t.Fatalf("裁定②：不应新增 channel_models.%s（转换保持协议独占）", col)
		}
	}
	if !tableExists(t, tempDB, "channel_base_url_archive") {
		t.Fatalf("缺少留档表 channel_base_url_archive")
	}

	// ---------- 2) 引用协议渠道：归档 + 清空；内联渠道不动 ----------
	var archivedURL, archivedProto string
	if err := tempDB.QueryRow(
		`SELECT base_url, protocol FROM channel_base_url_archive WHERE channel_id = 1`).
		Scan(&archivedURL, &archivedProto); err != nil {
		t.Fatalf("留档行缺失（引用协议的渠道应被归档）: %v", err)
	}
	if archivedURL != "https://legacy.example.com" || archivedProto != "openai-compat" {
		t.Fatalf("留档内容不符: base_url=%q protocol=%q", archivedURL, archivedProto)
	}
	var refURL, refProto string
	if err := tempDB.QueryRow(`SELECT base_url, protocol FROM channels WHERE id=1`).Scan(&refURL, &refProto); err != nil {
		t.Fatalf("读取渠道 1 失败: %v", err)
	}
	if refURL != "" || refProto != "" {
		t.Fatalf("引用协议渠道的 base_url/protocol 应被清空, got %q/%q", refURL, refProto)
	}
	var inlineURL, inlineProto string
	if err := tempDB.QueryRow(`SELECT base_url, protocol FROM channels WHERE id=2`).Scan(&inlineURL, &inlineProto); err != nil {
		t.Fatalf("读取渠道 2 失败: %v", err)
	}
	if inlineURL != "https://inline.example.com" || inlineProto != "openai-chat" {
		t.Fatalf("内联渠道不应被改动, got %q/%q", inlineURL, inlineProto)
	}
	var archivedInline int
	if err := tempDB.QueryRow(`SELECT count(*) FROM channel_base_url_archive WHERE channel_id = 2`).Scan(&archivedInline); err != nil {
		t.Fatalf("统计内联渠道留档失败: %v", err)
	}
	if archivedInline != 0 {
		t.Fatalf("内联渠道不应被归档")
	}

	// ---------- 3) 种子的模型级 protocols 已下沉（qwen-max）----------
	if got := jsonCol(t, tempDB, `SELECT protocols FROM channel_models WHERE id=101`); got != `{"anthropic-messages": {}}` {
		t.Fatalf("qwen-max protocols 未从种子下沉, got %s", got)
	}

	// ---------- 4) 漂移标记 ----------
	edited := map[int64]bool{}
	for id := int64(101); id <= 104; id++ {
		var v bool
		if err := tempDB.QueryRow(`SELECT edited FROM channel_models WHERE id=$1`, id).Scan(&v); err != nil {
			t.Fatalf("读取 edited(id=%d) 失败: %v", id, err)
		}
		edited[id] = v
	}
	if edited[101] {
		t.Fatalf("qwen-max 与种子一致，不应被标 edited")
	}
	if edited[102] {
		t.Fatalf("qwen-plus 的种子没定义 costRates，成本不参与漂移判定，不应被标 edited")
	}
	if !edited[103] {
		t.Fatalf("qwen-drift 成本偏离种子，应被标 edited")
	}
	if edited[104] {
		t.Fatalf("内联渠道的模型行不在协议范围内，不应被标 edited")
	}

	// ---------- 5) B1 回归：没有任何 cost_rates 被写成 JSON null ----------
	var nullCosts int
	if err := tempDB.QueryRow(`SELECT count(*) FROM channel_models WHERE cost_rates IS NULL OR jsonb_typeof(cost_rates) <> 'object'`).Scan(&nullCosts); err != nil {
		t.Fatalf("统计 cost_rates 失败: %v", err)
	}
	if nullCosts != 0 {
		t.Fatalf("迁移后出现 %d 行 cost_rates 非对象（null），B1 回归失败", nullCosts)
	}

	// ---------- 6) 幂等：再跑一次不报错、结果不变（留档不重复、清空不反复）----------
	runMigration(t, tempDB, migDir, migrationFile(t, migDir, "0019"))
	var archivedRows int
	if err := tempDB.QueryRow(`SELECT count(*) FROM channel_base_url_archive`).Scan(&archivedRows); err != nil {
		t.Fatalf("统计留档行失败: %v", err)
	}
	if archivedRows != 1 {
		t.Fatalf("二次执行后留档行数应为 1（幂等），got %d", archivedRows)
	}
	if got := jsonCol(t, tempDB, `SELECT protocols FROM channel_models WHERE id=101`); got != `{"anthropic-messages": {}}` {
		t.Fatalf("二次执行后 qwen-max protocols 变了: %s", got)
	}

	// ---------- 7) 回滚脚本：还原基址 + 删列删表 + 数据保真 ----------
	// backend/internal/db → backend/scripts
	backendRoot := filepath.Dir(filepath.Dir(filepath.Dir(mustCallerFile(t))))
	body, err := os.ReadFile(filepath.Join(backendRoot, "scripts", "rollback-0019.sql"))
	if err != nil {
		t.Fatalf("读取回滚脚本失败: %v", err)
	}
	if _, err := tempDB.Exec(string(body)); err != nil {
		t.Fatalf("执行回滚脚本失败: %v", err)
	}
	if columnExists(t, tempDB, "channel_models", "edited") {
		t.Fatalf("回滚后 channel_models 仍存在列 edited")
	}
	if tableExists(t, tempDB, "channel_base_url_archive") {
		t.Fatalf("回滚后留档表仍存在")
	}
	var restoredURL, restoredProto string
	if err := tempDB.QueryRow(`SELECT base_url, protocol FROM channels WHERE id=1`).Scan(&restoredURL, &restoredProto); err != nil {
		t.Fatalf("读取渠道 1 失败: %v", err)
	}
	if restoredURL != "https://legacy.example.com" || restoredProto != "openai-compat" {
		t.Fatalf("回滚未从留档还原: base_url=%q protocol=%q", restoredURL, restoredProto)
	}
	after := snapshotModelRows(t, tempDB)
	if len(after) != len(before) {
		t.Fatalf("回滚后模型行数变化：before=%d after=%d", len(before), len(after))
	}
	for id, want := range before {
		if after[id] != want {
			t.Fatalf("回滚后模型行 %d 数据变化：before=%s after=%s", id, want, after[id])
		}
	}
}

// snapshotModelRows 取「0019 不应改动」的稳定列（id / cost_rates / external_model_id / state）。
// 注意：protocols 不在快照里——0019 会按种子补齐实例里为空的 protocols（刻意行为，见迁移 (c)），
// 回滚不撤销这一补齐（种子仍在协议里，老代码行为等价）。
func snapshotModelRows(t *testing.T, tempDB *sql.DB) map[int64]string {
	t.Helper()
	rows, err := tempDB.Query(
		`SELECT id, coalesce(cost_rates::text, 'null'), external_model_id, state FROM channel_models ORDER BY id`)
	if err != nil {
		t.Fatalf("快照 channel_models 失败: %v", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id, extID int64
		var costs, state string
		if err := rows.Scan(&id, &costs, &extID, &state); err != nil {
			t.Fatalf("扫描快照失败: %v", err)
		}
		out[id] = fmt.Sprintf("%s|%d|%s", costs, extID, state)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("快照 rows.Err: %v", err)
	}
	return out
}

// columnExists 判断某表是否存在指定列。
func columnExists(t *testing.T, tempDB *sql.DB, table, column string) bool {
	t.Helper()
	var n int
	if err := tempDB.QueryRow(
		`SELECT count(*) FROM information_schema.columns WHERE table_name=$1 AND column_name=$2`,
		table, column).Scan(&n); err != nil {
		t.Fatalf("查询列 %s.%s 失败: %v", table, column, err)
	}
	return n > 0
}

// tableExists 判断表是否存在。
func tableExists(t *testing.T, tempDB *sql.DB, table string) bool {
	t.Helper()
	var oid sql.NullString
	if err := tempDB.QueryRow(`SELECT to_regclass($1)::text`, "public."+table).Scan(&oid); err != nil {
		t.Fatalf("查询表 %s 失败: %v", table, err)
	}
	return oid.Valid
}

// jsonCol 取一个 JSONB 列并归一化为 text（键序稳定，便于直接比较）。
func jsonCol(t *testing.T, tempDB *sql.DB, query string) string {
	t.Helper()
	var raw []byte
	if err := tempDB.QueryRow(query).Scan(&raw); err != nil {
		t.Fatalf("读取 JSONB 列失败（%s）: %v", query, err)
	}
	if raw == nil {
		return "null"
	}
	return string(raw)
}
