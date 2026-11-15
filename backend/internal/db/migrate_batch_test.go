package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrationBatch_V2Phase1 守住「0021 不在本批」这条边界。
//
// 0021_v2_drop_legacy 会删掉 billing_records.channel_key_id / sessions.channel_key_id /
// tokens.tag_id 并 DROP 渠道域 10 张表，而本批代码仍在这三列/这些表上（会话粘性、令牌标签路由、
// 渠道名展示）。有 DSN 的包里加回它，会让 billing/console 的用例当场 42703 / 42P01。
// 所以：0021 只能随「渠道/标签域下线」单独一批进（那一批要同时改代码 + 改控制面模型）。
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
	for _, want := range []string{
		"0019_v2_topology.sql",
		"0020_v2_billing_attribution.sql",
		"0022_v2_ledger.sql",
		"0023_v2_reconcile.sql",
	} {
		if !present[want] {
			t.Fatalf("本批应包含 %s（迁移只从架构分支取，别自己改）", want)
		}
	}
	for f := range present {
		if strings.HasPrefix(f, "0021_") {
			t.Fatalf("本批不得包含 %s：它会删掉代码仍在使用的列/表（见 docs/designs/2026-11-15-v2-decouple-channel-keyid.md）", f)
		}
	}
}
