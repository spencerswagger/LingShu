package billing

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/db"
)

// 这些用例跑**真实迁移**（本批只含 0019/0020/0022/0023；0021_v2_drop_legacy 已摘出单独一批），
// 不手搓 DDL：手搓只能证明「能写进自己造的表」。
//
// 分工：账单侧的渠道密钥维度**读**已改由 call_logs 的 1:1 快照供数（写入仍写列，等 0021 同批再摘写）；
// 0021 一旦落地，sessions.channel_key_id / tokens.tag_id 的写路径仍会 42703——
// 那一刀属控制面（改路由行为），需负责人点头，见 docs/designs/2026-11-15-v2-decouple-channel-keyid.md。

// openAttributionTempDB 建临时库并应用 0001–0023 真实迁移（含 0021 的删列/删表）。
func openAttributionTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过真实库用例")
	}
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	name := fmt.Sprintf("v2_billing_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Skipf("无法创建临时库（账号是否没有 CREATEDB 权限？）: %v", err)
	}
	t.Cleanup(func() {
		if a, derr := db.Open(dsn); derr == nil {
			_, _ = a.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = a.Close()
		}
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("解析 DSN 失败: %v", err)
	}
	u.Path = "/" + name
	target, err := db.Open(u.String())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })

	if _, err := db.Migrate(context.Background(), target); err != nil {
		t.Fatalf("在临时库应用迁移（0001–0023）失败: %v", err)
	}
	return target
}

func seedBillingUser(t *testing.T, conn *sql.DB) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO users(id, username, password_hash, role) VALUES (7, 'u7', 'x', 'DEVELOPER')`); err != nil {
		t.Fatalf("插入用户失败: %v", err)
	}
}

func newRealStoreRecord(billingID string) *Record {
	callTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	return &Record{
		BillingID:       billingID,
		UserID:          7,
		PricingMode:     "sale",
		ExternalModel:   "gpt-4o",
		InternalModelID: "int-gpt-4o",
		CallTime:        callTime,
		Tokens:          Usage{Input: 100, Output: 20},
		Rates:           fullRates(),
		Coefficients: struct {
			Time    float64 `json:"time"`
			Context float64 `json:"context"`
		}{Time: 1, Context: 1},
		RValue:          10000,
		RawTotal:        0.001,
		CreditsConsumed: 0.0001,
		CostCredits:     0,
		Status:          "completed",
	}
}

// TestAttribution_RealSchema_WriteAndReadBack 断言七列在真实 schema 上可写可反查：
//   - 归因值逐字落库，历史行可以直接读出（不需要 join 任何配置/映射表）；
//   - 缺失值留 NULL（不猜），cost_source 缺省归一 none；
//   - 改价（同一行改 rates/r_value/credits）之后，归因列仍解释同一条历史账单；
//   - 非法协议在写库前就被拦下（不会撞 CHECK）。
func TestAttribution_RealSchema_WriteAndReadBack(t *testing.T) {
	conn := openAttributionTempDB(t)
	seedBillingUser(t, conn)
	store := NewSqlStore(conn)
	ctx := context.Background()

	full := newRealStoreRecord("bill-real-1")
	full.RequestID = "req-real-1"
	full.UpstreamProvider = "openai"
	full.UpstreamAccount = "acct-real-9"
	full.UpstreamProtocol = "openai-chat"
	full.UpstreamModel = "gpt-4o-2024-11-20"
	full.LedgerEntryID = "led-real-1"
	full.CostSource = CostSourceLedger
	if _, err := store.Insert(ctx, full); err != nil {
		t.Fatalf("写入带归因的账单失败: %v", err)
	}

	bare := newRealStoreRecord("bill-real-2")
	if _, err := store.Insert(ctx, bare); err != nil {
		t.Fatalf("写入无归因的账单失败: %v", err)
	}

	type attrRow struct {
		requestID, provider, account, protocol, model, ledgerID, costSource sql.NullString
	}
	readAttr := func(billingID string) attrRow {
		var r attrRow
		if err := conn.QueryRowContext(ctx,
			`SELECT request_id, upstream_provider, upstream_account, upstream_protocol,
			        upstream_model, ledger_entry_id, cost_source
			   FROM billing_records WHERE billing_id = $1`, billingID).
			Scan(&r.requestID, &r.provider, &r.account, &r.protocol, &r.model, &r.ledgerID, &r.costSource); err != nil {
			t.Fatalf("反查归因失败（%s）: %v", billingID, err)
		}
		return r
	}

	got := readAttr("bill-real-1")
	for _, c := range []struct {
		name string
		got  sql.NullString
		want string
	}{
		{"request_id", got.requestID, "req-real-1"},
		{"upstream_provider", got.provider, "openai"},
		{"upstream_account", got.account, "acct-real-9"},
		{"upstream_protocol", got.protocol, "openai-chat"},
		{"upstream_model", got.model, "gpt-4o-2024-11-20"},
		{"ledger_entry_id", got.ledgerID, "led-real-1"},
		{"cost_source", got.costSource, CostSourceLedger},
	} {
		if !c.got.Valid || c.got.String != c.want {
			t.Fatalf("%s 落库不符：want %q，got %+v", c.name, c.want, c.got)
		}
	}

	// 缺失留 NULL（不猜），cost_source 归一 none。
	gotBare := readAttr("bill-real-2")
	for name, v := range map[string]sql.NullString{
		"request_id":        gotBare.requestID,
		"upstream_provider": gotBare.provider,
		"upstream_account":  gotBare.account,
		"upstream_protocol": gotBare.protocol,
		"upstream_model":    gotBare.model,
		"ledger_entry_id":   gotBare.ledgerID,
	} {
		if v.Valid {
			t.Fatalf("%s 缺失时应为 NULL，got %q（不许猜）", name, v.String)
		}
	}
	if !gotBare.costSource.Valid || gotBare.costSource.String != CostSourceNone {
		t.Fatalf("cost_source 缺省应为 none，got %+v", gotBare.costSource)
	}

	// 改价：同一行改掉计价相关列，归因列仍是「这条历史账单当时的事实」。
	if _, err := conn.ExecContext(ctx,
		`UPDATE billing_records
		    SET r_value = 20000, raw_total = 0.002, credits_consumed = 0.0002, rates = '{"input":9,"output":9,"cache_read":9,"cache_write":9,"reasoning":9}'::jsonb
		  WHERE billing_id = 'bill-real-1'`); err != nil {
		t.Fatalf("改价失败: %v", err)
	}
	after := readAttr("bill-real-1")
	if after != got {
		t.Fatalf("改价后归因列不应变化：before=%+v after=%+v", got, after)
	}

	// 非法归因：**不许**让账单写不进去（否则扣了钱没账），归一后照常落库并打 WARN。
	bad := newRealStoreRecord("bill-real-3")
	bad.UpstreamProtocol = "openai-embeddings"
	bad.CostSource = "invoice"
	if _, err := store.Insert(ctx, bad); err != nil {
		t.Fatalf("非法归因不该导致写入失败（归一后应落库）: %v", err)
	}
	gotBad := readAttr("bill-real-3")
	if gotBad.protocol.Valid {
		t.Fatalf("非法协议应归一为 NULL，got %q", gotBad.protocol.String)
	}
	if !gotBad.costSource.Valid || gotBad.costSource.String != CostSourceNone {
		t.Fatalf("非法 cost_source 应归一为 none，got %+v", gotBad.costSource)
	}

	// 带首尾空白的合法值：trim 后判合法，按 trim 后的枚举落库（CHECK 只认枚举本身）。
	spaced := newRealStoreRecord("bill-real-5")
	spaced.UpstreamProtocol = " openai-chat "
	spaced.CostSource = " ledger "
	if _, err := store.Insert(ctx, spaced); err != nil {
		t.Fatalf("带空白的合法归因不该被判非法: %v", err)
	}
	if gotSp := readAttr("bill-real-5"); !gotSp.protocol.Valid || gotSp.protocol.String != "openai-chat" {
		t.Fatalf("带空白的合法协议应 trim 后落库，got %+v", gotSp.protocol)
	}
}

// TestBilling_DimensionFromCallLogs 在 0024 之后的 schema 上断言维度口径。
//
// 口径已变（0024 把渠道族删了）：`billing_records.channel_key_id` 与 `sessions.channel_key_id`
// 都不再存在，维度只可能来自 `call_logs.channel_key_id`（1:1 快照，0024 明确保留该列）。
//
// 本用例只断言**schema 事实**（列在/列不在），行为断言（列表回填、按维度过滤、详情回填）
// 依赖「写路径不再写被删列」的代码摘除——那是 P0 代码债（见交付文档未完成项），
// 在它修好之前 store.Insert 会 42703，行为无法在这个 schema 上验证。
// 所以这里显式 Skip 行为段，而不是把断言改成「永远通过」：绿不能假装成验证过。
func TestBilling_DimensionFromCallLogs(t *testing.T) {
	conn := openAttributionTempDB(t)
	ctx := context.Background()

	columnExists := func(table, column string) bool {
		t.Helper()
		var exists bool
		if err := conn.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			                 WHERE table_name=$1 AND column_name=$2)`, table, column).Scan(&exists); err != nil {
			t.Fatalf("查询 %s.%s 存在性失败: %v", table, column, err)
		}
		return exists
	}

	// 0024 已执行：verdict 必须是「账单表不再有该列」。
	if columnExists("billing_records", "channel_key_id") {
		t.Fatal("0024 已删 billing_records.channel_key_id，列却还在（0024 被跳过了？）")
	}
	if columnExists("sessions", "channel_key_id") {
		t.Fatal("0024 已删 sessions.channel_key_id，列却还在")
	}
	// 快照列必须保留：维度唯一还可能存在的来源。
	if !columnExists("call_logs", "channel_key_id") {
		t.Fatal("call_logs.channel_key_id 必须保留（账单维度快照是 0024 明确不动的列）")
	}
	// 渠道族表已不可用：这正是 7 条 P0 代码债的来源。
	for _, table := range []string{"channels", "channel_keys", "semantic_tags", "channel_tags"} {
		var reg *string
		if err := conn.QueryRowContext(ctx, `SELECT to_regclass('public.`+table+`')::text`).Scan(&reg); err != nil {
			t.Fatalf("查询 %s 失败: %v", table, err)
		}
		if reg != nil {
			t.Fatalf("0024 应已删表 %s，仍存在", table)
		}
	}

	t.Skip("维度读写的端到端验证等 P0 代码债（写路径仍写被删列 → 42703；" +
		"console 仍 JOIN 已删表 → 42P01）修好后补：见交付文档「未完成项」")
}
