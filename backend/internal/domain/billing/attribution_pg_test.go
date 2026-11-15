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
// ⚠️ 已上报的冲突：0021_v2_drop_legacy 会 DROP billing_records.channel_key_id，
// 而当前写路径（SqlStore.insertOn）仍在写这一列 → 全量迁移后插入会报
// `column "channel_key_id" of relation "billing_records" does not exist`。
// 迁移文件不归本切片改，写路径也不在本切片摘（摘它要连带改 console/统计/会话，
// 属「渠道体系下线」那一刀）。所以：
//   - TestAttribution_RealSchema_WriteAndReadBack 临时把该列补回来，只验证本次新增的归因列；
//   - TestAttribution_RealSchema_0021Conflict 不补列，把冲突钉成确定性失败信息（修好后它会红，提醒删掉补列 hack）。

// openAttributionTempDB 建临时库并应用 0001–0023 真实迁移；restoreChannelKeyID 时把 0021 删掉的列补回来。
func openAttributionTempDB(t *testing.T, restoreChannelKeyID bool) *sql.DB {
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
	if restoreChannelKeyID {
		if _, err := target.ExecContext(context.Background(),
			`ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS channel_key_id BIGINT`); err != nil {
			t.Fatalf("补回 channel_key_id 失败: %v", err)
		}
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
	conn := openAttributionTempDB(t, true)
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

// TestAttribution_RealSchema_0021Conflict 把 0021 与当前写路径的冲突钉成确定性事实：
// 全量迁移（含 0021 删列）后，插入账单必须因 channel_key_id 不存在而失败。
//
// 这条用例是一颗地雷：等「渠道体系下线」那一刀把 channel_key_id 从写路径摘掉后，它会变红，
// 提醒直接删掉本用例与上面那处 ADD COLUMN hack。在此之前的每一次变红都说明有人改错了地方。
func TestAttribution_RealSchema_0021Conflict(t *testing.T) {
	conn := openAttributionTempDB(t, false)
	seedBillingUser(t, conn)
	store := NewSqlStore(conn)
	ctx := context.Background()

	rec := newRealStoreRecord("bill-real-conflict")

	var exists bool
	if err := conn.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                 WHERE table_name='billing_records' AND column_name='channel_key_id')`).Scan(&exists); err != nil {
		t.Fatalf("查询列存在性失败: %v", err)
	}
	if exists {
		t.Fatalf("0021 应已删掉 billing_records.channel_key_id（迁移没跑全？）")
	}

	_, err := store.Insert(ctx, rec)
	if err == nil {
		t.Fatal("写路径仍在写 channel_key_id，但列已被 0021 删除 —— 不该插入成功；" +
			"若这是刚摘掉该列的改动，请删除本用例与 WriteAndReadBack 里的补列 hack")
	}
	if !strings.Contains(err.Error(), "channel_key_id") {
		t.Fatalf("失败原因应是 channel_key_id 列不存在，got %v", err)
	}
	t.Logf("已确认冲突（0021 删列 vs 当前写路径）：%v", err)
}
