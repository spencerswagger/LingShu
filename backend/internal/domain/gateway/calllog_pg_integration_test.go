package gateway

import (
	"context"
	"database/sql"
	"math/rand"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/team/llmgateway/internal/testutil/randbody"
)

// 本文件是 call_logs 读写链路的**真实 PG** 守卫（无 DSN 时跳过，CI 默认不跑）：
// sqlmock 只比对 SQL 文本与参数个数，两处一起改错（列名拼错、列序与 scan 顺序不一致）照样通过；
// 只有真库能把「写进去的字段」和「读回来的字段」对上。
//
// 运行：
//
//	LLMGATEWAY_TEST_PG_DSN='postgres://user:pass@host:port/db?sslmode=disable' \
//	  go test ./internal/domain/gateway/ -run TestCallLog_PgRoundTrip -v
//
// 覆盖点：upstream_status（迁移 0019）写入与读回、request_id 落库、失败记录的错误体与原因。
func TestCallLog_PgRoundTrip(t *testing.T) {
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过 call_logs 真库读写测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// 目标库没跑过迁移（如 CI 里 DSN 指向空库）时跳过：本用例要读写真正的 call_logs 表，
	// 不负责建表；硬跑只会在 cleanup 阶段报 relation does not exist 把整条流水线打红。
	var exists sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('public.call_logs')::text`).Scan(&exists); err != nil {
		t.Fatalf("probe call_logs: %v", err)
	}
	if !exists.Valid || exists.String == "" {
		t.Skip("目标库没有 call_logs（未执行迁移），跳过真库读写测试")
	}

	const bid = "bill-upstream-status-roundtrip"
	if _, err := db.Exec(`DELETE FROM call_logs WHERE billing_id=$1`, bid); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM call_logs WHERE billing_id=$1`, bid)
	})

	// 报文内容随机（含中文、<>&、换行）：TEXT 列必须逐字节存得进、读得出，
	// 固定样例会掩盖「存储层按内容/长度裁剪」这类缺陷。
	seed := time.Now().UnixNano()
	rnd := rand.New(rand.NewSource(seed))
	t.Logf("随机种子 seed=%d", seed)
	body := "upstream says: " + randbody.Text(rnd, 100, 400)

	upStatus := 400
	in := &CallLog{
		BillingID: bid, RequestID: "req-roundtrip", Model: "gpt-4", PricingMode: "sale",
		Status: "failed", RespKind: "error", RespBody: body,
		ErrorMessage: "上游返回 400: " + body, UpstreamStatus: &upStatus,
		CreatedAt: time.Now(),
	}
	store := NewSQLCallLogStore(db)
	if err := store.Insert(context.Background(), in); err != nil {
		t.Fatalf("insert: %v", err)
	}
	out, err := store.GetByBillingID(context.Background(), bid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out == nil {
		t.Fatal("读不到刚写入的调用日志")
	}
	if out.UpstreamStatus == nil || *out.UpstreamStatus != upStatus {
		t.Fatalf("upstream_status 读回不符: %v", out.UpstreamStatus)
	}
	if out.RequestID != "req-roundtrip" || out.Status != "failed" || out.RespKind != "error" {
		t.Fatalf("关键字段读回不符: %+v", out)
	}
	if out.RespBody != body {
		t.Fatalf("上游错误体读回不符（必须逐字节一致）\nwant=%q\ngot =%q", body, out.RespBody)
	}
}
