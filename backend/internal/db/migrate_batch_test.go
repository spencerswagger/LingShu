package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrationChain_V2Batch 守住 V2 这批迁移的形状（不需要 DSN，纯源码级）。
//
// 本批的边界（裁定后的口径，覆盖上一版「0021 必须被摘出去」）：
//  1. 文件集合恰好是 0019 / 0020 / 0022 / 0023 / 0024，**没有 0021**（已更名 0024）；
//  2. 0024 是目录里文件名最大的迁移 —— 字典序 = 执行序，破坏性动作一次启动里最后执行；
//  3. 破坏性 DDL（DROP TABLE / DROP COLUMN）**只允许出现在 0024**；0019 起的其它文件必须纯 ADD。
//     上一版只查文件名，被「把 0021 改名成 0024_*」直接绕过（复审 R1 实测 PASS），所以这里查内容；
//  4. 两个裁定点必须在文件里（改回旧口径要红）：
//     - 0023 的 status 值域含 'skipped'、且有 scroll_offset 列；
//     - 0020 有跨列 CHECK（cost_credits IS NULL ⇒ cost_source = 'none'）。
//
// 注释行先剔除：0020 / 0024 的文件头大量出现 "DROP COLUMN"/"DROP TABLE" 的说明文字，
// 不剔必误报。
func TestMigrationChain_V2Batch(t *testing.T) {
	dir := migrationDir(t)
	present := migrationFileNames(t, dir)

	want := V2MigrationFiles()
	for _, name := range want {
		if !present[name] {
			t.Fatalf("本批应包含 %s（迁移只从架构分支取，别自己改）", name)
		}
	}
	if present["0021_v2_drop_legacy.sql"] {
		t.Fatalf("本批不得再有 0021_v2_drop_legacy.sql：已按裁定更名为 0024 并排到最后" +
			"（手工留两份会让同一份 DDL 跑两次、且 0021 会在 0023 之前执行）")
	}

	// 字典序 = 执行序：0024 必须是最后一个。
	var last string
	for name := range present {
		if name > last {
			last = name
		}
	}
	if last != "0024_v2_drop_legacy.sql" {
		t.Fatalf("最大的迁移文件是 %s，期望 0024_v2_drop_legacy.sql —— "+
			"破坏性迁移必须排在链尾，否则它之后的文件跑在被删掉的列/表上", last)
	}

	for name := range present {
		// 0019 之前的迁移是历史（0005/0006/0009 等本来就含 DROP），不在本批约束范围。
		if name < "0019" {
			continue
		}
		body := readMigration(t, dir, name)
		if stmt, bad := destructiveDDL(body); bad {
			if name != "0024_v2_drop_legacy.sql" {
				t.Fatalf("%s 含有破坏性 DDL（%s）：只允许 0024 删列/删表，"+
					"其余迁移必须纯 ADD（改名前缀也绕不过内容检查）", name, stmt)
			}
			continue
		}
		if name == "0024_v2_drop_legacy.sql" {
			t.Fatalf("0024 应含破坏性 DDL（删退役渠道/标签/Auto 体系），实际没有——" +
				"是不是被误改成了纯 ADD？")
		}
	}
}

// TestMigrationChain_ReconcileShape 断言 0023 的两个裁定点（status 值域 + offset 列）。
func TestMigrationChain_ReconcileShape(t *testing.T) {
	body := readMigration(t, migrationDir(t), "0023_v2_reconcile.sql")

	for _, want := range []string{
		"scroll_offset",
		"'skipped'",
		"ck_reconcile_status",
		// 旧内联 CHECK 必须先删：留着它仍旧会拒 'skipped'。
		"DROP CONSTRAINT IF EXISTS reconcile_windows_status_check",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("0023 缺少 %q（D4/F9 裁定：status 增 skipped、offset 持久化）", want)
		}
	}
	// 四个计数列必须还在（对账服务要写它们；删了就退化成「解析 detail」）。
	for _, col := range []string{"local_count", "upstream_count", "local_tokens", "upstream_tokens"} {
		if !strings.Contains(body, col) {
			t.Fatalf("0023 缺少计数列 %s：四个计数列是对账判定的唯一来源，不许改成只写 detail", col)
		}
	}
}

// TestMigrationChain_BillingCostPresence 断言 0020 的跨列 CHECK（F10）。
func TestMigrationChain_BillingCostPresence(t *testing.T) {
	body := readMigration(t, migrationDir(t), "0020_v2_billing_attribution.sql")

	if !strings.Contains(body, "ck_billing_cost_presence") {
		t.Fatalf("0020 缺少跨列 CHECK ck_billing_cost_presence（F10：cost_credits IS NULL ⇒ cost_source = 'none'）")
	}
	if !strings.Contains(body, "cost_credits IS NOT NULL OR cost_source = 'none'") {
		t.Fatalf("ck_billing_cost_presence 的表达式不是「cost_credits 非空 或 cost_source = none」——" +
			"方向写反会让「有成本却标 none」的行合法、把真正要挡的组合放过去")
	}
	// 约束前必须有存量归一：不 UPDATE 的话存量 failed 行（成本 NULL + 来源 NULL）会让 ADD CONSTRAINT 直接失败。
	if !strings.Contains(body, "SET cost_source = 'none'") {
		t.Fatalf("0020 加跨列 CHECK 前必须先把存量 NULL 来源归一为 'none'，否则约束建不上")
	}
}

// TestMigrationChain_TableLists 断言保留/退役表清单的数量与成员（F2）。
func TestMigrationChain_TableLists(t *testing.T) {
	retained := RetainedTables()
	if len(retained) != 15 {
		t.Fatalf("保留表应为 15 张，got %d: %v", len(retained), retained)
	}
	for _, want := range []string{"watchlist_items", "price_change_alerts"} {
		found := false
		for _, name := range retained {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("保留表缺少 %s（价格监控表未批准下架，别被「渠道域全删」顺手带走）", want)
		}
	}

	retired := RetiredTables()
	if len(retired) != 10 {
		t.Fatalf("退役表应为 10 张（channel_events 在 0009 已删，不计入），got %d: %v", len(retired), retired)
	}
	// 交叉断言：退役表不许同时出现在保留清单里（两份清单打架 = 迁移会删掉「保留」的东西）。
	for _, r := range retired {
		for _, k := range retained {
			if r == k {
				t.Fatalf("%s 同时在退役与保留清单里", r)
			}
		}
	}
	// 退役表必须真的在 0024 里被 DROP（清单写了、SQL 没写等于没删干净）。
	body := readMigration(t, migrationDir(t), "0024_v2_drop_legacy.sql")
	for _, r := range retired {
		if !strings.Contains(body, "DROP TABLE IF EXISTS "+r+";") {
			t.Fatalf("0024 没有 DROP TABLE IF EXISTS %s;（与 RetiredTables() 不一致）", r)
		}
	}
}

// migrationDir 返回内嵌迁移的源码目录（tests 与 db.Migrate 用同一批文件）。
func migrationDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
}

// migrationFileNames 返回目录下 .sql 文件名集合。
func migrationFileNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取迁移目录失败: %v", err)
	}
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		out[e.Name()] = true
	}
	return out
}

// readMigration 读取单个迁移文件正文。
func readMigration(t *testing.T, dir, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("读取迁移 %s 失败: %v", name, err)
	}
	return string(body)
}

// destructiveDDL 剔除 `--` 注释后，返回第一处破坏性 DDL 语句片段；没有则 ok=false。
func destructiveDDL(body string) (string, bool) {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue // 注释里的说明不算
		}
		// 行尾注释去掉，避免 `ALTER TABLE t DROP COLUMN c; -- 说明` 之类误判/漏判。
		if i := strings.Index(trimmed, "--"); i >= 0 {
			trimmed = strings.TrimSpace(trimmed[:i])
		}
		kept = append(kept, trimmed)
	}
	sql := strings.Join(kept, "\n")
	upper := strings.ToUpper(sql)
	for _, kw := range []string{"DROP TABLE", "DROP COLUMN"} {
		if idx := strings.Index(upper, kw); idx >= 0 {
			start := idx - 40
			if start < 0 {
				start = 0
			}
			end := idx + 80
			if end > len(sql) {
				end = len(sql)
			}
			return strings.TrimSpace(sql[start:end]), true
		}
	}
	return "", false
}
