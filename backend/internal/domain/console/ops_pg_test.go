package console

// 真迁移库用例：A6 两个接口的 SQL 在**真实 schema** 上能跑通。
// sqlmock 只能证明「我发的 SQL 形状与我写的期望一致」，证明不了列名/类型/枚举在库里存在——
// 而盈亏查询横跨 billing_records 的归因列、token 三段表达式、reconcile_windows 的十列形状，
// 正是最容易出现「代码里编了个不存在的列」的地方。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/db"
	"github.com/team/llmgateway/internal/domain/billing"
)

// openOpsTempDB 建临时库并跑全量迁移（无 DSN 或无 CREATEDB 权限时跳过）。
func openOpsTempDB(t *testing.T) *sql.DB {
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

	name := fmt.Sprintf("v2_ops_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Skipf("无法创建临时库（账号没有 CREATEDB？）: %v", err)
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
		t.Fatalf("应用迁移失败: %v", err)
	}
	return target
}

// TestHandlePnL_RealSQL_GroupByDimensions：三个分组维度都能在真实库上跑，数值口径正确。
func TestHandlePnL_RealSQL_GroupByDimensions(t *testing.T) {
	conn := openOpsTempDB(t)
	ctx := context.Background()

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO users(id, username, password_hash, role) VALUES (1, 'u1', 'x', 'DEVELOPER')`); err != nil {
		t.Fatalf("造用户失败：%v", err)
	}
	insert := `INSERT INTO billing_records
		(id, billing_id, user_id, pricing_mode, external_model_name, internal_model_id,
		 call_time, tokens, r_value, credits_consumed, cost_credits, cost_source,
		 upstream_provider, upstream_account, upstream_model)
		VALUES ($1, $2, 1, 'sale', $3, $4, $5, $6::jsonb, 1, $7, $8, $9, $10, $11, $12)`
	base := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	rows := []struct {
		id      int64
		bid     string
		ext     string
		im      string
		at      time.Time
		tokens  string
		credit  float64
		cost    sql.NullFloat64
		source  sql.NullString
		prov    sql.NullString
		acct    sql.NullString
		upModel sql.NullString
	}{
		{1, "b1", "gpt-4", "g/gpt-4", base, `{"input":100,"output":0}`, 10, sql.NullFloat64{Float64: 3, Valid: true}, sql.NullString{String: "ledger", Valid: true}, sql.NullString{String: "p1", Valid: true}, sql.NullString{String: "acc-a", Valid: true}, sql.NullString{String: "up-1", Valid: true}},
		{2, "b2", "gpt-4", "g/gpt-4", base.Add(time.Minute), `{"input":300,"output":0}`, 30, sql.NullFloat64{Float64: 9, Valid: true}, sql.NullString{String: "amortized", Valid: true}, sql.NullString{String: "p1", Valid: true}, sql.NullString{String: "acc-a", Valid: true}, sql.NullString{String: "up-1", Valid: true}},
		{3, "b3", "claude", "a/claude", base.Add(2 * time.Minute), `{"input":50,"output":50}`, 5, sql.NullFloat64{}, sql.NullString{String: "none", Valid: true}, sql.NullString{String: "p2", Valid: true}, sql.NullString{String: "acc-b", Valid: true}, sql.NullString{String: "up-2", Valid: true}},
	}
	for _, r := range rows {
		if _, err := conn.ExecContext(ctx, insert, r.id, r.bid, r.ext, r.im, r.at, r.tokens,
			r.credit, r.cost, r.source, r.prov, r.acct, r.upModel); err != nil {
			t.Fatalf("造账单 %s 失败：%v", r.bid, err)
		}
	}

	admin := NewAdmin(conn, billing.NewSqlStore(conn), nil, testUserIDFrom)
	admin.now = func() time.Time { return base.Add(time.Hour) }

	call := func(groupBy string) pnlResponse {
		target := "/api/v1/admin/ops/pnl?From=" + base.Add(-time.Hour).Format(time.RFC3339) +
			"&To=" + base.Add(time.Hour).Format(time.RFC3339)
		if groupBy != "" {
			target += "&GroupBy=" + groupBy
		}
		rec := httptest.NewRecorder()
		admin.HandlePnL(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GroupBy=%s 状态码 %d body=%s", groupBy, rec.Code, rec.Body.String())
		}
		var payload struct {
			Data pnlResponse `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("解析失败：%v", err)
		}
		return payload.Data
	}

	// 默认维度 = provider：p1 收入 40 / 成本 12（账本 3 + 摊销 9），p2 收入 5 / 成本 0 且 1 条未计成本。
	byProvider := call("")
	if len(byProvider.Groups) != 2 {
		t.Fatalf("按供应商应 2 组，got %+v", byProvider.Groups)
	}
	if byProvider.Revenue != 45 || byProvider.Cost != 12 || byProvider.Margin != 33 {
		t.Fatalf("合计不符：%+v", byProvider)
	}
	if byProvider.Uncosted != 1 {
		t.Fatalf("未计成本应为 1，got %d", byProvider.Uncosted)
	}
	var p1 pnlRow
	for _, g := range byProvider.Groups {
		if g.Key == "p1" {
			p1 = g
		}
	}
	if p1.LedgerCost != 3 || p1.AmortizedCost != 9 || p1.Tokens != 400 {
		t.Fatalf("p1 的成本来源拆分/token 不符：%+v", p1)
	}

	// 按账号：p1/acc-a 与 p2/acc-b。
	byAccount := call("account")
	if len(byAccount.Groups) != 2 || byAccount.Groups[0].Key == "" {
		t.Fatalf("按账号分组不符：%+v", byAccount.Groups)
	}

	// 按模型：g/gpt-4 合并两行（收入 40、成本 12），a/claude 单独。
	byModel := call("model")
	if len(byModel.Groups) != 2 {
		t.Fatalf("按模型应 2 组，got %+v", byModel.Groups)
	}
	var gpt pnlRow
	for _, g := range byModel.Groups {
		if g.Key == "g/gpt-4" {
			gpt = g
		}
	}
	if gpt.Requests != 2 || gpt.Revenue != 40 || gpt.Cost != 12 {
		t.Fatalf("g/gpt-4 聚合不符：%+v", gpt)
	}
}

// TestHandleReconcileWindows_RealSQL：真实窗口表上 blocked 行计数为 null、alarm 行偏差率透出、倒序返回。
func TestHandleReconcileWindows_RealSQL(t *testing.T) {
	conn := openOpsTempDB(t)
	ctx := context.Background()

	start := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO reconcile_windows (window_start, window_end, status, alarm_streak, detail, scroll_offset)
		 VALUES ($1, $2, 'blocked', 0, '{"blocked":"blocked: pending POC"}'::jsonb, 4000)`,
		start, start.Add(time.Minute)); err != nil {
		t.Fatalf("造 blocked 窗口失败：%v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO reconcile_windows (window_start, window_end, status, alarm_streak,
		        local_count, upstream_count, local_tokens, upstream_tokens, detail)
		 VALUES ($1, $2, 'alarm', 2, 10, 9, 1000, 950, '{"deviation_rate":0.05}'::jsonb)`,
		start.Add(time.Minute), start.Add(2*time.Minute)); err != nil {
		t.Fatalf("造 alarm 窗口失败：%v", err)
	}

	admin := NewAdmin(conn, billing.NewSqlStore(conn), nil, testUserIDFrom)
	admin.now = func() time.Time { return start.Add(time.Hour) }
	rec := httptest.NewRecorder()
	admin.HandleReconcileWindows(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/ops/reconcile-windows?From="+start.Add(-time.Hour).Format(time.RFC3339)+
			"&To="+start.Add(time.Hour).Format(time.RFC3339), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Data struct {
			Items []reconcileWindow `json:"Items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	items := payload.Data.Items
	if len(items) != 2 {
		t.Fatalf("应 2 行，got %d", len(items))
	}
	// 倒序：alarm（更晚）在前。
	if items[0].Status != "alarm" || items[1].Status != "blocked" {
		t.Fatalf("应按窗口倒序：%+v", items)
	}
	if items[1].LocalCount != nil || items[1].UpstreamTokens != nil {
		t.Fatalf("blocked 行的计数必须是 null（= 没算过），got %+v", items[1])
	}
	if items[0].DeviationRate == nil || *items[0].DeviationRate != 0.05 || items[0].AlarmStreak != 2 {
		t.Fatalf("偏差率/alarm 不符：%+v", items[0])
	}
	if items[1].ScrollOffset != 4000 {
		t.Fatalf("补拉断点应透出，got %d", items[1].ScrollOffset)
	}
}
