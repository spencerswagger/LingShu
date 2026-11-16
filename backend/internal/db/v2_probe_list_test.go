package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这组用例守住「迁移清单 ⟷ 执行前探测清单」不漂移（不需要 DSN，纯源码级）。
//
// 为什么必须有它：探测清单是两处手抄（`0024` 文件头注释、`scripts/acceptance/v2-preflight/*.sql`），
// 而漏抄是**静默的**——本轮实测：`0023b` / `0023c` 落地后，旧白名单在**干净库**上
// 报出 2 行「污染」，运维会照状态矩阵误判成 S1/S2 并对一个健康的库跑 R-2 回基线。
// 反过来漏一个文件的 ④ 映射，那个文件的对象缺了则永远没人发现（S4 只有 ④ 有信号）。
// 清单漂移靠人眼守不住，所以钉成用例。

func probeDir(t *testing.T) string {
	t.Helper()
	// 本文件在 backend/internal/db/，探测脚本在 backend/scripts/acceptance/v2-preflight/。
	backend := filepath.Dir(filepath.Dir(filepath.Dir(mustCallerFile(t))))
	return filepath.Join(backend, "scripts", "acceptance", "v2-preflight")
}

func readProbe(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(probeDir(t), name)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取探测脚本 %s 失败: %v（运维手册要求这四个文件与迁移清单同源）", name, err)
	}
	return string(body)
}

// between 取标记行之间的内容；找不到标记即失败（防止有人把标记段删了但用例仍“通过”）。
func between(t *testing.T, body, open, close string) string {
	t.Helper()
	i := strings.Index(body, open)
	if i < 0 {
		t.Fatalf("找不到标记 %q", open)
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatalf("找不到结束标记 %q", close)
	}
	return rest[:j]
}

// sqlFileNames 抽出文本里所有 `NNNN[a-z]?_*.sql` 形式的文件名（去重，保持出现顺序）。
func sqlFileNames(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return r == '\'' || r == ',' || r == '(' || r == ')' || r == ' ' ||
			r == '\n' || r == '\t' || r == '\r'
	}) {
		if !strings.HasSuffix(field, ".sql") || len(field) < 8 {
			continue
		}
		if field[0] < '0' || field[0] > '9' {
			continue
		}
		if !seen[field] {
			seen[field] = true
			out = append(out, field)
		}
	}
	return out
}

// TestV2Probe_RecordWhitelistMatchesMigrationFiles 断言探测① 的两份白名单都等于 V2MigrationFiles()。
//
// 三处必须一致：`V2MigrationFiles()`（唯一来源）、probe-1 的标记段（人读的清单）、
// probe-1 的 `NOT IN (...)`（机器执行的清单）。任何一处加漏 → 干净库误报 / 污染库漏报。
func TestV2Probe_RecordWhitelistMatchesMigrationFiles(t *testing.T) {
	want := V2MigrationFiles()
	wantSet := map[string]bool{}
	for _, n := range want {
		wantSet[n] = true
	}

	body := readProbe(t, "probe-1-records.sql")

	marker := sqlFileNames(between(t, body, "<v2-migration-whitelist>", "</v2-migration-whitelist>"))
	assertSameSet(t, "probe-1 标记段白名单", marker, want)

	// 机器执行的 `NOT IN (...)`：只在值区里取，避免把注释里的举例也算进来。
	notIn := sqlFileNames(between(t, body, "AND name NOT IN (", ";"))
	assertSameSet(t, "probe-1 NOT IN 白名单", notIn, want)

	// 右界必须跟着常量走（后续批新增 0025 时，这里不写死会静默漏判/误判）。
	if !strings.Contains(body, "AND name < '"+V2MigrationRightBound+"'") {
		t.Fatalf("probe-1 的记录层右界必须是 name < '%s'（= V2MigrationRightBound）；"+
			"写裸条件 name >= '0019' 会在本链跑完后永远非空、每次误报", V2MigrationRightBound)
	}
}

// TestV2Probe_ReverseMapCoversEveryMigration 断言探测④ 为**每一个** V2 迁移文件都留了映射。
//
// ④ 是 S4（记录在、对象缺）唯一有信号的探测；漏掉某个文件的映射 = 该文件的对象缺了没人数。
// 数据型迁移（0023c）没有对象可断言，但它必须以 `data` 出现在清单里，注明“无对象断言”。
func TestV2Probe_ReverseMapCoversEveryMigration(t *testing.T) {
	body := readProbe(t, "probe-4-objects-by-record.sql")
	listed := map[string]bool{}
	for _, n := range sqlFileNames(between(t, body, "WITH want(file, kind, obj) AS (VALUES", "), rec AS")) {
		listed[n] = true
	}

	var missing []string
	for _, n := range V2MigrationFiles() {
		if !listed[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("探测④ 的映射清单缺 %d 个迁移文件: %v —— 那些文件的对象缺了没有探测能发现（S4 只有 ④ 有信号）",
			len(missing), missing)
	}

	// 数据型迁移必须显式标注，不许“忘了写”被当成“覆盖了”。
	if !strings.Contains(body, "'data'") {
		t.Fatalf("探测④ 缺少 kind='data' 的口径：纯数据迁移（如 0023c）没有对象可断言，" +
			"必须以 data 显式出现在清单里，否则会被读成“已覆盖”")
	}
}

// TestV2Probe_LegacyProbeExcludesKeptObjects 断言探测② 不再把 2026-11-16 裁定保留下来的对象当旧对象。
//
// 裁定 A 之后 `magpie_keys` / `external_models.magpie_model` / `uq_external_models_magpie_model`
// 由 `0019b_v2_passthrough_credentials.sql` 正当创建；留在这条探测里 = 干净库上永远非 0 行。
func TestV2Probe_LegacyProbeExcludesKeptObjects(t *testing.T) {
	// 只看可执行 SQL：注释里会解释“曾经把它当过旧对象”，那不是判定条件。
	body := stripSQLComments(readProbe(t, "probe-2-legacy-objects.sql"))
	for _, mustNotAppear := range []string{
		"magpie_keys",
		"magpie_model",
		"uq_external_models_magpie_model",
	} {
		if strings.Contains(body, mustNotAppear) {
			t.Fatalf("探测② 仍把 %s 当「本链不建」的旧对象：裁定 A 之后它们由 0019b 正当创建，"+
				"留在这里会让干净库每次误报", mustNotAppear)
		}
	}
	// 反向：仍属本链不建的三个账单列必须留着，否则这条探测变成空转。
	for _, mustAppear := range []string{"magpie_key_name", "estimated", "upstream_usage"} {
		if !strings.Contains(body, mustAppear) {
			t.Fatalf("探测② 漏了 billing_records.%s：阶段二才建的列，本链若建出来说明链被污染", mustAppear)
		}
	}
}

// stripSQLComments 去掉整行 `--` 注释（探测脚本里注释与语句混写，判据只能看语句）。
func stripSQLComments(body string) string {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func assertSameSet(t *testing.T, what string, got, want []string) {
	t.Helper()
	gotSet := map[string]bool{}
	for _, n := range got {
		gotSet[n] = true
	}
	wantSet := map[string]bool{}
	for _, n := range want {
		wantSet[n] = true
	}
	var extra, missing []string
	for n := range gotSet {
		if !wantSet[n] {
			extra = append(extra, n)
		}
	}
	for n := range wantSet {
		if !gotSet[n] {
			missing = append(missing, n)
		}
	}
	if len(extra) > 0 || len(missing) > 0 {
		t.Fatalf("%s 与 V2MigrationFiles() 不一致：多出 %v，缺少 %v\n"+
			"多出 → 干净库误报污染；缺少 → 真污染漏判。加迁移文件时三处要一起改", what, extra, missing)
	}
}

// TestV2Probe_LegacyProbeHandlesPartialColumns 断言探测② 不把“三列齐全”当查询开关。
//
// 半迁移库里 `billing_records` 可能只多出 1~2 列（手工加过 / 迁移中途失败）。旧写法
// `count(...) = 3 THEN ...` 在这种库上**静默不查询**，探测输出 0 行看着干净；
// 复审实测：只补 `magpie_key_name` 并写入值，探测仍然 0 行。
func TestV2Probe_LegacyProbeHandlesPartialColumns(t *testing.T) {
	body := stripSQLComments(readProbe(t, "probe-2-legacy-objects.sql"))
	if strings.Contains(body, "= 3") {
		t.Fatalf("探测② 不许用 `= 3` 当查询开关：只剩 1~2 列的半迁移库会被静默跳过；" +
			"应按实际存在的列动态拼条件")
	}
	if !strings.Contains(body, "string_agg") {
		t.Fatalf("探测② 必须按实际存在的列动态拼条件（string_agg + format + EXECUTE）")
	}
}
