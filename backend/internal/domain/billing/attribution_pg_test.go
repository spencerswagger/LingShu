package billing

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/db"
)

// 这两个用例跑**真实迁移**（架构侧 0019–0023），不手搓 DDL：手搓只能证明「能写进自己造的表」。
//
// 0021_v2_drop_legacy 会删掉 billing_records.channel_key_id / sessions.channel_key_id /
// tokens.tag_id 并 DROP 渠道域 10 张表。写路径已不再写该列（渠道密钥维度改由 call_logs 的
// 1:1 快照供数），所以这里**不再需要补列 hack**；渠道域与 sessions/tokens 两处控制面列的
// 下线属下一刀，本文件只用 schema 级事实把「还没摘的部分」钉住（见 TestPost0021Schema_RemainingBlockers）。

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

	// 非法协议：写库前报错并点名字段（不留到 CHECK 约束里）。
	bad := newRealStoreRecord("bill-real-3")
	bad.UpstreamProtocol = "openai-embeddings"
	if _, err := store.Insert(ctx, bad); err == nil || !strings.Contains(err.Error(), "upstream_protocol") {
		t.Fatalf("非法协议应写库前报错并点名字段，got %v", err)
	}
	// 非法 cost_source 同理。
	bad2 := newRealStoreRecord("bill-real-4")
	bad2.CostSource = "invoice"
	if _, err := store.Insert(ctx, bad2); err == nil || !strings.Contains(err.Error(), "cost_source") {
		t.Fatalf("非法 cost_source 应写库前报错并点名字段，got %v", err)
	}
}

// TestBilling_Post0021Schema_DimensionFromCallLogs 断言 0021 全量迁移（列已删）之后，
// 账单的**写入与查询**都不再依赖 channel_key_id：
//   - 插入成功（写路径已不碰该列）；
//   - 列表/详情/汇总可跑，且渠道密钥维度由 call_logs 的 1:1 快照回填（值不变）；
//   - 按该维度过滤仍可用（EXISTS 到 call_logs，不是恒真也不是恒不命中）。
func TestBilling_Post0021Schema_DimensionFromCallLogs(t *testing.T) {
	conn := openAttributionTempDB(t)
	seedBillingUser(t, conn)
	store := NewSqlStore(conn)
	ctx := context.Background()

	var exists bool
	if err := conn.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                 WHERE table_name='billing_records' AND column_name='channel_key_id')`).Scan(&exists); err != nil {
		t.Fatalf("查询列存在性失败: %v", err)
	}
	if exists {
		t.Fatalf("0021 应已删掉 billing_records.channel_key_id（迁移没跑全？）")
	}

	// 两条账单：一条有调用日志（密钥 42），一条没有（拒绝类场景）。
	if _, err := store.Insert(ctx, newRealStoreRecord("bill-post-1")); err != nil {
		t.Fatalf("迁移后插入账单失败（写路径仍依赖 channel_key_id？）: %v", err)
	}
	if _, err := store.Insert(ctx, newRealStoreRecord("bill-post-2")); err != nil {
		t.Fatalf("插入第二条账单失败: %v", err)
	}
	insertCallLog := func(id int64, billingID string, keyID any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO call_logs(id, billing_id, request_id, status, channel_key_id) VALUES ($1,$2,$3,'completed',$4)`,
			id, billingID, "req-"+billingID, keyID); err != nil {
			t.Fatalf("插入调用日志失败: %v", err)
		}
	}
	insertCallLog(9001, "bill-post-1", int64(42))

	// 列表：维度从快照回填，缺日志的行为 0（与历史「未进入渠道」一致）。
	list, total, err := store.ListRecords(ctx, RecordFilter{Page: 1, Size: 20})
	if err != nil {
		t.Fatalf("迁移后查询账单列表失败: %v", err)
	}
	if total != 2 || len(list) != 2 {
		t.Fatalf("total=%d len=%d, want 2/2", total, len(list))
	}
	byID := map[string]Record{}
	for _, rec := range list {
		byID[rec.BillingID] = rec
	}
	if got := byID["bill-post-1"].ChannelKeyID; got != 42 {
		t.Fatalf("有调用日志的账单应回填密钥 42，got %d", got)
	}
	if got := byID["bill-post-2"].ChannelKeyID; got != 0 {
		t.Fatalf("无调用日志的账单应为 0，got %d", got)
	}

	// 详情。
	one, err := store.GetByBillingID(ctx, "bill-post-1")
	if err != nil {
		t.Fatalf("迁移后查询账单详情失败: %v", err)
	}
	if one.ChannelKeyID != 42 {
		t.Fatalf("详情应回填密钥 42，got %d", one.ChannelKeyID)
	}

	// 按该维度过滤（1 命中 / 1 不命中，两个方向都要断，否则「恒真」与「恒假」都测不出来）。
	key := int64(42)
	hit, hitTotal, err := store.ListRecords(ctx, RecordFilter{ChannelKeyID: &key, Page: 1, Size: 20})
	if err != nil {
		t.Fatalf("按渠道密钥过滤失败: %v", err)
	}
	if hitTotal != 1 || len(hit) != 1 || hit[0].BillingID != "bill-post-1" {
		t.Fatalf("过滤应只命中 bill-post-1，got total=%d list=%+v", hitTotal, hit)
	}
	other := int64(77)
	miss, missTotal, err := store.ListRecords(ctx, RecordFilter{ChannelKeyID: &other, Page: 1, Size: 20})
	if err != nil {
		t.Fatalf("按不存在的渠道密钥过滤失败: %v", err)
	}
	if missTotal != 0 || len(miss) != 0 {
		t.Fatalf("不存在的密钥不应命中任何账单，got total=%d len=%d", missTotal, len(miss))
	}

	// 汇总（统计口径不依赖被删列）。
	sum, err := store.Summarize(ctx, RecordFilter{Page: 1, Size: 20})
	if err != nil {
		t.Fatalf("迁移后汇总失败: %v", err)
	}
	if sum.Requests != 2 {
		t.Fatalf("汇总请求数=%d, want 2", sum.Requests)
	}
}

// TestPost0021Schema_RemainingBlockers 记录 0021 全量迁移后**仍未摘**的部分（schema 级事实）：
// sessions.channel_key_id（粘性路由状态）与 tokens.tag_id（标签路由输入）都已被删，
// 但 router.SessionStore / identity.TokenStore 的 SQL 仍按列名读写它们 → 上线后是 42703。
//
// 为什么这一刀不能顺手做：这两列是**控制面**（会话粘性/切换判定、令牌默认路由），
// 摘掉等于改路由行为，必须与「渠道/标签域下线」同一批处理，需负责人点头。
// 所以这里先断言 schema 事实，再 skip 说明——它不参与门禁变绿，也不会被误当成已解决。
func TestPost0021Schema_RemainingBlockers(t *testing.T) {
	conn := openAttributionTempDB(t)
	seedBillingUser(t, conn)
	ctx := context.Background()

	for _, tc := range []struct{ table, column string }{
		{"sessions", "channel_key_id"},
		{"tokens", "tag_id"},
		{"billing_records", "channel_key_id"},
	} {
		var exists bool
		if err := conn.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			                 WHERE table_name=$1 AND column_name=$2)`, tc.table, tc.column).Scan(&exists); err != nil {
			t.Fatalf("查询 %s.%s 存在性失败: %v", tc.table, tc.column, err)
		}
		if exists {
			t.Fatalf("0021 后 %s.%s 不该存在", tc.table, tc.column)
		}
	}

	// 日志表保留渠道维度（历史快照），这是账单维度能继续供数的前提。
	for _, col := range []string{"channel_id", "channel_key_id"} {
		var exists bool
		if err := conn.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			                 WHERE table_name='call_logs' AND column_name=$1)`, col).Scan(&exists); err != nil {
			t.Fatalf("查询 call_logs.%s 失败: %v", col, err)
		}
		if !exists {
			t.Fatalf("call_logs.%s 应保留（账单维度的唯一留存来源）", col)
		}
	}

	t.Skip("控制面剩余两处（sessions.channel_key_id / tokens.tag_id）的写路径仍引用已删列；" +
		"0021 不能单独上线，摘它们会改路由行为，需负责人点头后与渠道/标签域下线同批处理")
}
