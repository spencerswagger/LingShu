package channel

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

// probe_logs 读写链路的**真实 PG** 守卫（无 DSN 时跳过）：
// sqlmock 只比对 SQL 文本与参数，列序与 scan 顺序一起写错照样通过；只有真库能证明
// 「上游原文写进去、运营读回来还是同一串字节」（含中文/HTML 特殊字符/换行）。
//
//	LLMGATEWAY_TEST_PG_DSN='postgres://user:pass@host:port/db?sslmode=disable' \
//	  go test ./internal/domain/channel/ -run TestProbeLog_PgRoundTrip -v
func TestProbeLog_PgRoundTrip(t *testing.T) {
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过 probe_logs 真库读写测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// 目标库没跑过迁移（如 CI 里 DSN 指向空库）时跳过——本用例不负责建表。
	var exists sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('public.probe_logs')::text`).Scan(&exists); err != nil {
		t.Fatalf("probe probe_logs: %v", err)
	}
	if !exists.Valid || exists.String == "" {
		t.Skip("目标库没有 probe_logs（未执行迁移），跳过真库读写测试")
	}

	seed := time.Now().UnixNano()
	rnd := rand.New(rand.NewSource(seed))
	t.Logf("随机种子 seed=%d", seed)
	body := randbody.JSONText(randbody.OpenAIError(rnd)) + "\n" + randbody.HTML(rnd)

	// probe_logs.channel_key_id 有外键：先造一对「渠道 + 密钥」测试行（负 ID 专用，避免与真实数据撞号），
	// 结束时按 channel 级联删除（channel_keys/probe_logs 都是 ON DELETE CASCADE）。
	const (
		channelID = int64(-424242)
		keyID     = int64(-424242)
	)
	cleanup := func() {
		_, _ = db.Exec(`DELETE FROM channels WHERE id=$1`, channelID)
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := db.Exec(`INSERT INTO channels(id, name, protocol, base_url) VALUES($1,$2,$3,$4)`,
		channelID, "pg-roundtrip", "openai-compat", "https://up.example.com"); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO channel_keys(id, channel_id, name, credential_enc) VALUES($1,$2,$3,$4)`,
		keyID, channelID, "pg-roundtrip-key", "enc"); err != nil {
		t.Fatalf("insert channel_key: %v", err)
	}
	store := NewChannelModelStore(db)

	if err := store.InsertProbeLog(context.Background(), &ProbeLog{
		ChannelKeyID: keyID, ModelID: "m1", Level: "model", Target: "https://up.example.com/chat/completions",
		OK: false, Error: "probe http status: 400", UpstreamStatus: 400, ErrorBody: body,
		DurationMS: 12,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	logs, err := store.ListProbeLogs(context.Background(), keyID, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("want 1 条探测记录, got %d", len(logs))
	}
	if logs[0].ErrorBody != body {
		t.Fatalf("上游原文读回不符（必须逐字节一致）\nwant=%q\ngot =%q", body, logs[0].ErrorBody)
	}
	if logs[0].UpstreamStatus != 400 {
		t.Fatalf("upstream_status 读回不符: %d", logs[0].UpstreamStatus)
	}

	// 没打到上游的探测（没有状态码/报文）：NULL/空串读回不报错。
	if err := store.InsertProbeLog(context.Background(), &ProbeLog{
		ChannelKeyID: keyID, ModelID: "m1", Level: "model", OK: false, Error: "connect failed",
	}); err != nil {
		t.Fatalf("insert(no upstream): %v", err)
	}
	logs, err = store.ListProbeLogs(context.Background(), keyID, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("want 2 条探测记录, got %d", len(logs))
	}
	if logs[0].UpstreamStatus != 0 || logs[0].ErrorBody != "" {
		t.Fatalf("无上游状态的记录应读回 0/空, got %d/%q", logs[0].UpstreamStatus, logs[0].ErrorBody)
	}
}
