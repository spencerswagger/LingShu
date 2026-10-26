package billing

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/db"
)

// 本文件是 PR #63 验收 P1 的**真实库回归**（可选执行，只在自己的临时库里跑）：
//
//	cd backend
//	LLMGATEWAY_TEST_PG_DSN='postgres://user:pass@host:port/postgres?sslmode=disable' \
//	  go test ./internal/domain/billing -run TestListRecords_RequestIDFilter_Integrated -v
//
// 未设置该变量时 t.Skip（不阻塞普通 go test ./...）。
// 结论口径：按 request_id 搜索时，逐条（Group=0 展开子记录）只返回该 request_id 的子记录，
// 归并（默认）只返回一条主行且 Attempts 正确。

// openBillingTempDB 建临时库 + 跑全部迁移，测试结束自动删库。
func openBillingTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过 request_id 过滤集成测试")
	}
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.Ping(); err != nil {
		t.Fatalf("管理连接 ping 失败（DSN 是否可达?）: %v", err)
	}
	name := fmt.Sprintf("llmgateway-rid-%d", time.Now().UnixNano())
	if !regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(name) {
		t.Fatalf("临时库名非法: %s", name)
	}
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("创建临时库 %s 失败: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, name)
		if _, derr := admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`); derr != nil {
			t.Logf("删除临时库 %s 失败: %v", name, derr)
		}
	})
	tmpl, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("解析 DSN 失败: %v", err)
	}
	tmpl.Path = "/" + name
	temp, err := db.Open(tmpl.String())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = temp.Close() })
	if _, err := db.Migrate(context.Background(), temp); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	return temp
}

// TestListRecords_RequestIDFilter_Integrated 用真实 PostgreSQL 验证 P1 修复：
//   - 搜索 + 展开（Group=0）：只返回该 request_id 的 2 条子记录（修复前会返回全表 3 条）；
//   - 搜索 + 归并（默认）：返回 1 条主行，Attempts=2，主行取 completed；
//   - 不搜索：3 条全返回；
//   - 搜不到的 request_id：0 条。
func TestListRecords_RequestIDFilter_Integrated(t *testing.T) {
	conn := openBillingTempDB(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec 失败: %v\nSQL: %s", err, q)
		}
	}
	exec(`INSERT INTO users(id, username, password_hash, role) VALUES (1, 'u1', 'x', 'ADMIN')`)
	// 三条账单：A 组两次尝试（一次失败、一次成功），B 组一次成功。
	insertBill := `INSERT INTO billing_records(
		id, billing_id, user_id, pricing_mode, external_model_name, internal_model_id, r_value, status, call_time)
		VALUES ($1,$2,$3,'sale','gpt-4','gpt-4-int',10000,$4,$5)`
	now := time.Now()
	exec(insertBill, int64(1), "bill-a1", int64(1), "failed", now.Add(-2*time.Second))
	exec(insertBill, int64(2), "bill-a2", int64(1), "completed", now.Add(-time.Second))
	exec(insertBill, int64(3), "bill-b1", int64(1), "completed", now)
	insertLog := `INSERT INTO call_logs(id, billing_id, request_id) VALUES ($1,$2,$3)`
	exec(insertLog, int64(11), "bill-a1", "req-A")
	exec(insertLog, int64(12), "bill-a2", "req-A")
	exec(insertLog, int64(13), "bill-b1", "req-B")

	store := NewSqlStore(conn)

	// 1) 搜索 + 展开（Group=0）：必须只剩 A 组的 2 条。
	rows, total, err := store.ListRecords(ctx, RecordFilter{RequestID: "req-A", Page: 1, Size: 50})
	if err != nil {
		t.Fatalf("ListRecords(展开): %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("搜索+展开 total=%d rows=%d, want 2/2（修复前会返回全表 3 条）", total, len(rows))
	}
	for _, r := range rows {
		if r.BillingID != "bill-a1" && r.BillingID != "bill-a2" {
			t.Fatalf("搜索+展开返回了别人的账单: %s", r.BillingID)
		}
	}

	// 2) 搜索 + 归并（默认）：1 条主行，取 completed，Attempts=2。
	grouped, gtotal, err := store.ListRecords(ctx, RecordFilter{RequestID: "req-A", GroupByRequest: true, Page: 1, Size: 50})
	if err != nil {
		t.Fatalf("ListRecords(归并): %v", err)
	}
	if gtotal != 1 || len(grouped) != 1 {
		t.Fatalf("搜索+归并 total=%d rows=%d, want 1/1", gtotal, len(grouped))
	}
	if grouped[0].BillingID != "bill-a2" {
		t.Fatalf("归并主行=%s, want bill-a2（completed 优先）", grouped[0].BillingID)
	}
	if grouped[0].Attempts != 2 || grouped[0].RequestID != "req-A" {
		t.Fatalf("主行 Attempts=%d RequestID=%q, want 2/req-A", grouped[0].Attempts, grouped[0].RequestID)
	}

	// 3) 不搜索：3 条全返回（过滤为空时行为不变）。
	all, atotal, err := store.ListRecords(ctx, RecordFilter{Page: 1, Size: 50})
	if err != nil {
		t.Fatalf("ListRecords(全部): %v", err)
	}
	if atotal != 3 || len(all) != 3 {
		t.Fatalf("不搜索 total=%d rows=%d, want 3/3", atotal, len(all))
	}

	// 4) 搜不存在的 request_id：0 条（且不因恒真条件返回任何行）。
	none, ntotal, err := store.ListRecords(ctx, RecordFilter{RequestID: "req-nope", Page: 1, Size: 50})
	if err != nil {
		t.Fatalf("ListRecords(不存在): %v", err)
	}
	if ntotal != 0 || len(none) != 0 {
		t.Fatalf("不存在的 request_id 返回 total=%d rows=%d, want 0/0", ntotal, len(none))
	}
}
