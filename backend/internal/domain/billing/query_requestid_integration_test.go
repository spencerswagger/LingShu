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

// TestListRecords_GroupByRequest_UserIsolation_Integrated 是 PR #63 Owl P2-1 + 建议 2 的真实库回归：
//   - request_id 来自客户端可控的 x-request-id，**不同用户撞同一 id 不得并成一行**；
//   - 同用户同 request_id 正常归并；
//   - 归并在 SQL 层（LIMIT/OFFSET 生效，千行量级不会整体拉进内存）；
//   - Attempts 与列表口径一致：**过滤后**计数（带 status 过滤时随之变小）。
func TestListRecords_GroupByRequest_UserIsolation_Integrated(t *testing.T) {
	conn := openBillingTempDB(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec 失败: %v\nSQL: %s", err, q)
		}
	}
	exec(`INSERT INTO users(id, username, password_hash, role) VALUES (1,'u1','x','ADMIN'), (2,'u2','x','DEVELOPER')`)
	insertBill := `INSERT INTO billing_records(
		id, billing_id, user_id, pricing_mode, external_model_name, internal_model_id, r_value, status, call_time)
		VALUES ($1,$2,$3,'sale','gpt-4','gpt-4-int',10000,$4,now())`
	insertLog := `INSERT INTO call_logs(id, billing_id, request_id) VALUES ($1,$2,$3)`

	id := int64(100)
	next := func(user int64, reqID, status string) {
		id++
		bid := fmt.Sprintf("bill-%d", id)
		exec(insertBill, id, bid, user, status)
		exec(insertLog, id, bid, reqID)
	}
	// A) 同一 request_id 被两个用户使用（客户端可造），各自两次尝试。
	next(1, "req-X", "failed")
	next(1, "req-X", "completed")
	next(2, "req-X", "completed")
	// B) 规模：user1 的 30 个请求 × 各 10 条候选尝试 = 300 行。
	for r := 0; r < 30; r++ {
		for c := 0; c < 10; c++ {
			st := "failed"
			if c == 9 {
				st = "completed"
			}
			next(1, fmt.Sprintf("req-s%02d", r), st)
		}
	}

	store := NewSqlStore(conn)

	// 1) 无过滤归并：user1-reqX / user2-reqX / 30 个规模请求 = 32 组（不是 303 行）。
	rows, total, err := store.ListRecords(ctx, RecordFilter{GroupByRequest: true, Page: 1, Size: 10})
	if err != nil {
		t.Fatalf("ListRecords(归并全量): %v", err)
	}
	if total != 32 {
		t.Fatalf("归并总组数=%d, want 32（不同用户不得并组）", total)
	}
	if len(rows) != 10 {
		t.Fatalf("返回行数=%d, want 10（LIMIT 生效，不能把 303 行全拉进内存）", len(rows))
	}

	// 2) 同一 request_id 跨用户：两组，不能并成一行。
	rows, total, err = store.ListRecords(ctx, RecordFilter{RequestID: "req-X", GroupByRequest: true, Page: 1, Size: 10})
	if err != nil {
		t.Fatalf("ListRecords(req-X): %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("req-X 归并 total=%d rows=%d, want 2/2（两个用户各一行）", total, len(rows))
	}
	attemptsByUser := map[int64]int{}
	for _, r := range rows {
		attemptsByUser[r.UserID] = r.Attempts
	}
	if attemptsByUser[1] != 2 || attemptsByUser[2] != 1 {
		t.Fatalf("Attempts 按用户=%v, want {1:2, 2:1}", attemptsByUser)
	}

	// 3) 同用户同 request_id：正常归并成一行。
	u1 := int64(1)
	rows, total, err = store.ListRecords(ctx, RecordFilter{RequestID: "req-X", UserID: &u1, GroupByRequest: true, Page: 1, Size: 10})
	if err != nil {
		t.Fatalf("ListRecords(req-X,user1): %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].Attempts != 2 {
		t.Fatalf("同用户归并 total=%d rows=%d attempts=%d, want 1/1/2", total, len(rows), rows[0].Attempts)
	}

	// 4) Attempts 是**过滤后**计数：只留 completed 时，user1 的 req-X 只剩 1 条。
	rows, total, err = store.ListRecords(ctx, RecordFilter{RequestID: "req-X", UserID: &u1, Status: "completed", GroupByRequest: true, Page: 1, Size: 10})
	if err != nil {
		t.Fatalf("ListRecords(req-X,user1,completed): %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].Attempts != 1 {
		t.Fatalf("过滤后 Attempts=%v, want 1（与列表口径一致）", rows[0].Attempts)
	}
}
