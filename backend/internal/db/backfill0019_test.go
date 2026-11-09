package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// 0019 回填脚本（>5 万行路径）的两条验收：幂等（同一批跑两遍结果一致）与断点续跑（中断后续做）。
//
// 场景构造：迁移 0019 **不**执行（只跑到 0018），再手工建 0019 的 DDL 部分（列 + 留档表），
// 让回填脚本成为唯一的数据改造者——这样才真的在测脚本，而不是测迁移。
func newBackfillFixture(t *testing.T) (db *sql.DB, migDir string) {
	t.Helper()
	tempDB := openTempDB(t)
	dir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
	applyMigrationsBelow(t, tempDB, dir, "0019")

	// 只建 0019 的结构，不跑 0019 的数据段（由回填脚本负责）。
	exec(t, tempDB, `ALTER TABLE channel_models ADD COLUMN IF NOT EXISTS edited BOOLEAN NOT NULL DEFAULT false`)
	exec(t, tempDB, `CREATE TABLE IF NOT EXISTS channel_base_url_archive (
		channel_id BIGINT PRIMARY KEY,
		base_url   TEXT NOT NULL DEFAULT '',
		protocol   TEXT NOT NULL DEFAULT '',
		archived_at TIMESTAMPTZ NOT NULL DEFAULT now())`)

	// 协议 P 的种子：max 与种子一致、drift 偏离、plus 种子无成本键。
	exec(t, tempDB, `INSERT INTO external_models(id, external_name) VALUES (2,'ext-a'), (5,'ext-b')`)
	exec(t, tempDB, `
INSERT INTO channel_protocols(id, name, base_url, protocols, request_headers, models)
VALUES (7, 'p7', 'https://proto.example.com', '{"openai-chat":{}}'::jsonb, '{"passthroughMode":"none"}'::jsonb,
        '{"qwen-max":{"externalModelID":"2","costRates":{"Input":1,"Output":2,"CacheRead":0.1}},
          "qwen-drift":{"externalModelID":"2","costRates":{"Input":3,"Output":4,"CacheRead":0.3}},
          "qwen-plus":{"externalModelID":"5"}}'::jsonb)`)

	// 5 条引用协议的渠道（都有遗留基址）+ 1 条内联渠道（必须完全不动）。
	for i := int64(1); i <= 5; i++ {
		exec(t, tempDB, `INSERT INTO channels(id, name, base_url, protocol, channel_protocol_id)
			VALUES ($1, $2, $3, 'openai-compat', 7)`,
			i, "ref-"+itoa(i), "https://legacy-"+itoa(i)+".example.com")
		// 每条渠道 3 个模型行：101/102/103 → drift、max、plus（id 唯一化）。
		base := i * 10
		exec(t, tempDB, `INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates) VALUES
			($1, $4, 'qwen-drift', 2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb),
			($2, $4, 'qwen-max',   2, '{"input":1,"output":2,"cache_read":0.1}'::jsonb),
			($3, $4, 'qwen-plus',  5, '{"input":9,"output":9,"cache_read":9}'::jsonb)`,
			base+1, base+2, base+3, i)
	}
	exec(t, tempDB, `INSERT INTO channels(id, name, base_url, protocol) VALUES (99, 'inline', 'https://inline.example.com', 'openai-chat')`)
	exec(t, tempDB, `INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates)
		VALUES (991, 99, 'inline-1', 2, '{"input":7,"output":7,"cache_read":7}'::jsonb)`)
	return tempDB, dir
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// backfillSnapshot 取「回填结果」的可比快照：渠道基址/协议 + 留档 + edited。
func backfillSnapshot(t *testing.T, tempDB *sql.DB) string {
	t.Helper()
	out := ""
	rows, err := tempDB.Query(`SELECT id, base_url, protocol FROM channels ORDER BY id`)
	if err != nil {
		t.Fatalf("快照 channels 失败: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var base, proto string
		if err := rows.Scan(&id, &base, &proto); err != nil {
			t.Fatalf("扫描 channels 失败: %v", err)
		}
		out += "ch:" + itoa(id) + "=" + base + "|" + proto + "\n"
	}
	rows2, err := tempDB.Query(`SELECT channel_id, base_url, protocol FROM channel_base_url_archive ORDER BY channel_id`)
	if err != nil {
		t.Fatalf("快照 archive 失败: %v", err)
	}
	defer rows2.Close()
	for rows2.Next() {
		var id int64
		var base, proto string
		if err := rows2.Scan(&id, &base, &proto); err != nil {
			t.Fatalf("扫描 archive 失败: %v", err)
		}
		out += "ar:" + itoa(id) + "=" + base + "|" + proto + "\n"
	}
	rows3, err := tempDB.Query(`SELECT id, edited FROM channel_models ORDER BY id`)
	if err != nil {
		t.Fatalf("快照 channel_models 失败: %v", err)
	}
	defer rows3.Close()
	for rows3.Next() {
		var id int64
		var edited bool
		if err := rows3.Scan(&id, &edited); err != nil {
			t.Fatalf("扫描 channel_models 失败: %v", err)
		}
		flag := "0"
		if edited {
			flag = "1"
		}
		out += "cm:" + itoa(id) + "=" + flag + "\n"
	}
	return out
}

// 幂等：同一批连跑两遍，结果完全一致（留档不重复、清空不反复、edited 稳定）。
func TestBackfill0019_Idempotent(t *testing.T) {
	tempDB, _ := newBackfillFixture(t)
	ctx := context.Background()

	first, err := Backfill0019(ctx, tempDB, Backfill0019Options{BatchSize: 2})
	if err != nil {
		t.Fatalf("第一次回填失败: %v", err)
	}
	stateAfterFirst := backfillSnapshot(t, tempDB)

	second, err := Backfill0019(ctx, tempDB, Backfill0019Options{BatchSize: 2})
	if err != nil {
		t.Fatalf("第二次回填失败: %v", err)
	}
	stateAfterSecond := backfillSnapshot(t, tempDB)

	if stateAfterFirst != stateAfterSecond {
		t.Fatalf("重复执行结果不一致：\n--- first ---\n%s\n--- second ---\n%s", stateAfterFirst, stateAfterSecond)
	}
	// 第一遍：5 条引用协议渠道全部归档并清空；drift 行被标 edited（每渠道 1 行）。
	if first.ChannelsArchived != 5 {
		t.Fatalf("期望新留档 5 条，got %d（result=%+v）", first.ChannelsArchived, first)
	}
	if first.ModelsMarked != 5 {
		t.Fatalf("期望标记 5 行 edited（每渠道 1 行 drift），got %d（result=%+v）", first.ModelsMarked, first)
	}
	// 第二遍：幂等空跑，不应再有新留档/新标记。
	if second.ChannelsArchived != 0 || second.ModelsMarked != 0 {
		t.Fatalf("幂等重复执行不应再写入：%+v", second)
	}
	// 内联渠道必须未被触碰。
	var inlineURL string
	if err := tempDB.QueryRow(`SELECT base_url FROM channels WHERE id=99`).Scan(&inlineURL); err != nil {
		t.Fatalf("读取内联渠道失败: %v", err)
	}
	if inlineURL != "https://inline.example.com" {
		t.Fatalf("内联渠道基址被改动: %q", inlineURL)
	}
}

// 断点续跑：限批跑一轮（模拟中断）→ checkpoint 前进且只处理了前几批 → 再跑续做完成，最终状态与一次跑完一致。
func TestBackfill0019_ResumeFromCheckpoint(t *testing.T) {
	ctx := context.Background()

	// 参照组：一次跑完。
	fullDB, _ := newBackfillFixture(t)
	if _, err := Backfill0019(ctx, fullDB, Backfill0019Options{BatchSize: 2}); err != nil {
		t.Fatalf("参照组回填失败: %v", err)
	}
	want := backfillSnapshot(t, fullDB)

	// 实验组：每阶段只跑 1 批（模拟中途崩溃/停机）。
	partDB, _ := newBackfillFixture(t)
	part, err := Backfill0019(ctx, partDB, Backfill0019Options{BatchSize: 2, MaxBatchesPerPhase: 1})
	if err != nil {
		t.Fatalf("第一轮（限批）失败: %v", err)
	}
	if part.Batches != 3 {
		t.Fatalf("限批 1 时每阶段 1 批，三阶段（归档清空 / 下移 / 漂移）共 3 批，got %d", part.Batches)
	}
	// 断点已前进：不能还是 0（否则等于没做）。
	for _, phase := range []string{Backfill0019PhaseChannels, Backfill0019PhaseDownshift, Backfill0019PhaseModels} {
		last, err := BackfillCheckpoint(ctx, partDB, phase)
		if err != nil {
			t.Fatalf("读取断点 %s 失败: %v", phase, err)
		}
		if last == 0 {
			t.Fatalf("限批跑完后断点 %s 应已前进，got %d", phase, last)
		}
	}
	mid := backfillSnapshot(t, partDB)
	if mid == want {
		t.Fatalf("限批 1 批不该已经跑完全量（场景未生效）")
	}

	// 续跑：不限批，应当补齐剩下部分，且最终状态与一次跑完完全一致。
	if _, err := Backfill0019(ctx, partDB, Backfill0019Options{BatchSize: 2}); err != nil {
		t.Fatalf("续跑失败: %v", err)
	}
	if got := backfillSnapshot(t, partDB); got != want {
		t.Fatalf("续跑后状态与一次跑完不一致：\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
	// 留档仍然只有 5 条（跨轮次没有重复写）。
	var archived int
	if err := partDB.QueryRow(`SELECT count(*) FROM channel_base_url_archive`).Scan(&archived); err != nil {
		t.Fatalf("统计留档失败: %v", err)
	}
	if archived != 5 {
		t.Fatalf("续跑后留档行数应为 5（断点续跑不重复写），got %d", archived)
	}
}

// 结构存在保障：回填脚本不承担 DDL 之外的职责，但必须能在「迁移已跑过」的库上安全空跑。
func TestBackfill0019_AfterMigrationIsNoop(t *testing.T) {
	tempDB := openTempDB(t)
	ctx := context.Background()
	migDir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
	for _, name := range migrationNames(t, migDir) {
		runMigration(t, tempDB, migDir, name)
	}
	res, err := Backfill0019(ctx, tempDB, Backfill0019Options{BatchSize: 100})
	if err != nil {
		t.Fatalf("在迁移后的库上回填应安全空跑: %v", err)
	}
	if res.ChannelsArchived != 0 || res.ModelsMarked != 0 {
		t.Fatalf("空库回填应无写入，got %+v", res)
	}
}
