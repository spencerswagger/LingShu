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

// TestBilling_Post0021Schema_DimensionFromCallLogs 断言 0021 全量迁移（列已删）之后，
// 账单的**写入与查询**都不再依赖 channel_key_id：
//   - 插入成功（写路径已不碰该列）；
//   - 列表/详情/汇总可跑，且渠道密钥维度由 call_logs 的 1:1 快照回填（值不变）；
//   - 按该维度过滤仍可用（EXISTS 到 call_logs，不是恒真也不是恒不命中）。
func TestBilling_DimensionFromCallLogs(t *testing.T) {
	conn := openAttributionTempDB(t)
	seedBillingUser(t, conn)
	store := NewSqlStore(conn)
	ctx := context.Background()

	// 本批（0019/0020/0022/0023）里列还在；写路径仍写它，等 0021 同批再摘。
	var exists bool
	if err := conn.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                 WHERE table_name='billing_records' AND column_name='channel_key_id')`).Scan(&exists); err != nil {
		t.Fatalf("查询列存在性失败: %v", err)
	}
	if !exists {
		t.Fatalf("本批不含 0021，billing_records.channel_key_id 应仍存在（0021 被误加回本批？）")
	}

	// 本批 FK 还在（0021 会删表）：先造渠道 + 密钥行，账单才能引用。
	for _, stmt := range []string{
		`INSERT INTO channels(id, name, base_url) VALUES (42, 'ch-42', 'http://upstream.local')`,
		`INSERT INTO channel_keys(id, channel_id, name, credential_enc) VALUES (42, 42, 'k-42', 'enc')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("造渠道/密钥行失败: %v", err)
		}
	}

	// 两条账单：一条有调用日志（密钥 42），一条没有（拒绝类场景）。
	withKey := newRealStoreRecord("bill-post-1")
	withKey.ChannelKeyID = 42
	if _, err := store.Insert(ctx, withKey); err != nil {
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
	// 两侧一致：写路径仍写列，读路径走快照 —— 同一账单必须是同一个值。
	var columnValue sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT channel_key_id FROM billing_records WHERE billing_id = 'bill-post-1'`).Scan(&columnValue); err != nil {
		t.Fatalf("反查列值失败: %v", err)
	}
	if !columnValue.Valid || columnValue.Int64 != 42 {
		t.Fatalf("写路径应仍把密钥 42 写进列（0021 不在本批），got %+v", columnValue)
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
