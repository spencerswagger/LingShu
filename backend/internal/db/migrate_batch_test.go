package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrationBatch_V2Phase1 守住「本批不做破坏性 DDL」这条边界。
//
// 0021_v2_drop_legacy 会删掉 billing_records.channel_key_id / sessions.channel_key_id /
// tokens.tag_id 并 DROP 渠道域 10 张表，而本批代码仍在这三列/这些表上（会话粘性、令牌标签路由、
// 渠道名展示）。有 DSN 的包里加回它，会让 billing/console 的用例当场 42703 / 42P01。
//
// 两道检查，缺一不可：
//  1. 文件名：本批只允许 0019 / 0020 / 0022 / 0023（0021 单独一批）；
//  2. 内容：0019 之后的任何迁移文件（含未来新增的 0024+）里**不得出现破坏性 DDL**——
//     `DROP TABLE` / `DROP COLUMN`。只查文件名会被「把 0021 改名成 0024_*」绕过（复审 R1 实测 PASS）。
//
// 纯 ADD 的新迁移（0024 加列/建表）不受影响：它们没有 DROP。注释行先剔除，
// 否则「可回滚：DROP COLUMN 即可」这类说明会误报（0020 的文件头就是这种）。
func TestMigrationBatch_V2Phase1(t *testing.T) {
	dir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取迁移目录失败: %v", err)
	}
	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			present[e.Name()] = true
		}
	}

	allowed := map[string]bool{
		"0019_v2_topology.sql":            true,
		"0020_v2_billing_attribution.sql": true,
		"0022_v2_ledger.sql":              true,
		"0023_v2_reconcile.sql":           true,
	}
	for name := range allowed {
		if !present[name] {
			t.Fatalf("本批应包含 %s（迁移只从架构分支取，别自己改）", name)
		}
	}

	for name := range present {
		if strings.HasPrefix(name, "0021_") {
			t.Fatalf("本批不得包含 %s：它会删掉代码仍在使用的列/表（见 docs/designs/2026-11-15-v2-decouple-channel-keyid.md）", name)
		}
		// 文件名改成 0024_* 也逃不掉：查内容。0019 之前的迁移是历史，不在本批约束范围。
		if name < "0019" {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("读取 %s 失败: %v", name, rerr)
		}
		if stmt, bad := destructiveDDL(string(body)); bad {
			t.Fatalf("%s 含有破坏性 DDL（%s）：0019 之后的迁移只允许纯 ADD；删列/删表必须单独成批"+
				"（那批要同时改代码与控制面模型），改名前缀也绕不过我", name, stmt)
		}
	}
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
