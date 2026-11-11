package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"testing"
	"time"
	"unicode/utf8"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/team/llmgateway/internal/db"
)

// 本文件是 call_logs 读写链路的**真实 PG** 守卫：
// sqlmock 只比对 SQL 文本与参数个数，两处一起改错（列名拼错、列序与 scan 顺序不一致）照样通过；
// 只有真库能把「写进去的字段」和「读回来的字段」对上。
//
// 运行：
//
//	LLMGATEWAY_TEST_PG_DSN='postgres://user:pass@host:port/db?sslmode=disable' \
//	  go test ./internal/domain/gateway/ -run TestCallLog_PgRoundTrip -v
//
// 用例自己建**临时库 + 跑全部迁移**（与 internal/db、billing 的真库用例同一套路），
// 因此 CI 里 `LLMGATEWAY_TEST_PG_DSN` 指向空库也真跑，不再靠「库里没有表就跳过」假装绿。
//
// 覆盖点：upstream_status（迁移 0019）写入与读回、request_id 落库、失败记录的错误体与原因。

// pgRoundTripErrBody 只是「一段上游报文」的样本，用例断言的是写入/读回一致，不绑定文案。
const pgRoundTripErrBody = `{"error":{"type":"invalid_request_error","message":"This model is hosted outside Europe. Select Global in the console."},"request_id":"up-9f2c"}`

func TestCallLog_PgRoundTrip(t *testing.T) {
	d := openCallLogTempDB(t)

	const bid = "bill-upstream-status-roundtrip"
	upStatus := 400
	in := &CallLog{
		BillingID: bid, RequestID: "req-roundtrip", Model: "gpt-4", PricingMode: "sale",
		Status: "failed", RespKind: "error", RespBody: pgRoundTripErrBody,
		ErrorMessage: "上游返回 400: " + pgRoundTripErrBody, UpstreamStatus: &upStatus,
		CreatedAt: time.Now(),
	}
	store := NewSQLCallLogStore(d)
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
	if out.RespBody != pgRoundTripErrBody {
		t.Fatalf("上游错误体读回不符: %q", out.RespBody)
	}
}

// TestCallLog_PgRoundTrip_InvalidUTF8Payload 上游错误体含非法 UTF-8 与 0x00 时，
// 落库链路必须能写进去（清洗成合法 UTF-8），且内容仍可辨认——否则这条失败记录整行消失。
func TestCallLog_PgRoundTrip_InvalidUTF8Payload(t *testing.T) {
	d := openCallLogTempDB(t)

	const bid = "bill-invalid-utf8"
	in := &CallLog{
		BillingID: bid, RequestID: "req-invalid", Model: "gpt-4", PricingMode: "sale",
		Status: "failed", RespKind: "error",
		// 非法 UTF-8（GBK 片段）+ NUL：直接写 PG text 列会整行 INSERT 失败。
		RespBody:     "{\"error\":\"\xd6\xd0\xce\xc4\x00tail\"}",
		ErrorMessage: "上游返回 400: {\"error\":\"\xd6\xd0\xce\xc4\x00tail\"}",
		CreatedAt:    time.Now(),
	}
	store := NewSQLCallLogStore(d)
	if err := store.Insert(context.Background(), in); err != nil {
		t.Fatalf("含非法 UTF-8/0x00 的错误体必须能落库: %v", err)
	}
	out, err := store.GetByBillingID(context.Background(), bid)
	if err != nil || out == nil {
		t.Fatalf("get: %v (%v)", err, out)
	}
	if !utf8.ValidString(out.RespBody) {
		t.Fatalf("读回内容必须是合法 UTF-8: %q", out.RespBody)
	}
	if out.RespBody == "" {
		t.Fatal("清洗后不应丢成空串")
	}
}

// openCallLogTempDB 建一次性库并跑全部迁移（含 0019 upstream_status）；测试结束无条件删库。
// 未设置 LLMGATEWAY_TEST_PG_DSN 时才跳过（本地无 PG 的机器）。
func openCallLogTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过 call_logs 真库读写测试")
	}
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.Ping(); err != nil {
		t.Fatalf("管理连接 ping 失败（DSN 是否可达?）: %v", err)
	}

	name := fmt.Sprintf("llmgateway-calllog-%d", time.Now().UnixNano())
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
		t.Fatalf("临时库迁移失败: %v", err)
	}
	return temp
}
