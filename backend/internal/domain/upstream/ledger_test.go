package upstream

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/db"
)

func validEntry(ledgerID string) LedgerEntry {
	return LedgerEntry{
		LedgerID:        ledgerID,
		Provider:        "openai",
		ProviderAccount: "acct-1",
		Model:           "gpt-4o",
		TokensIn:        10,
		TokensOut:       20,
		CostCredits:     0.5,
		OccurredAt:      time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC),
	}
}

// TestLedgerEntry_Validate 断言九字段的可入库性校验：幂等键/发生时刻/token/金额各一条防线。
func TestLedgerEntry_Validate(t *testing.T) {
	if err := validEntry("led-1").Validate(); err != nil {
		t.Fatalf("合法记录不应报错，got %v", err)
	}
	cases := []struct {
		name string
		mod  func(*LedgerEntry)
	}{
		{"缺少 ledger_id", func(e *LedgerEntry) { e.LedgerID = " " }},
		{"缺少 occurred_at", func(e *LedgerEntry) { e.OccurredAt = time.Time{} }},
		{"token 为负", func(e *LedgerEntry) { e.TokensIn = -1 }},
		{"金额 NaN", func(e *LedgerEntry) { e.CostCredits = math.NaN() }},
		{"金额超出 NUMERIC(20,5)", func(e *LedgerEntry) { e.CostCredits = 1e16 }},
	}
	for _, c := range cases {
		e := validEntry("led-x")
		c.mod(&e)
		if err := e.Validate(); err == nil {
			t.Fatalf("%s 应校验失败", c.name)
		}
	}
}

// TestBuildLedgerInsert_IdempotentShape 断言写入 SQL 的幂等形状：九列 + ON CONFLICT (ledger_id) DO NOTHING。
func TestBuildLedgerInsert_IdempotentShape(t *testing.T) {
	e1, e2 := validEntry("led-1"), validEntry("led-2")
	query, args := buildLedgerInsert([]LedgerEntry{e1, e2})

	if !strings.Contains(query, "INSERT INTO "+LedgerTable) {
		t.Fatalf("SQL 应写入 %s：%s", LedgerTable, query)
	}
	if !strings.Contains(query, "ON CONFLICT (ledger_id) DO NOTHING") {
		t.Fatalf("SQL 必须用 ledger_id 做幂等冲突键：%s", query)
	}
	if !strings.Contains(query, "($1,") || !strings.Contains(query, "($10,") {
		t.Fatalf("两行应占 18 个占位参数（每行九列）：%s", query)
	}
	if len(args) != 2*LedgerFieldCount {
		t.Fatalf("参数个数应为 %d，got %d", 2*LedgerFieldCount, len(args))
	}
	// ID 为 0 时必须由组装层补雪花 ID（主键 NOT NULL）。
	if args[0].(int64) == 0 || args[LedgerFieldCount].(int64) == 0 {
		t.Fatalf("ID 未生成：%v / %v", args[0], args[LedgerFieldCount])
	}
}

// TestLedgerStore_UpsertIdempotent_DuplicateNotCounted 是真实库用例：同一 ledger_id 重复写入
// 只算一次（ON CONFLICT DO NOTHING 的 RowsAffected 语义），并验证 offset 分页边界。
//
// 未设置 LLMGATEWAY_TEST_PG_DSN 时跳过（不阻塞普通 go test）。
// 表由本用例按架构侧定稿的九字段在**临时库**里就地建——迁移文件归架构侧 0019–0023，
// 本包不另写一套生产 DDL。
func TestLedgerStore_UpsertIdempotent_DuplicateNotCounted(t *testing.T) {
	conn := openLedgerTempDB(t)
	ctx := context.Background()
	if err := conn.PingContext(ctx); err != nil {
		t.Fatalf("数据库 ping 失败: %v", err)
	}

	ddl := fmt.Sprintf(`CREATE TABLE %s (
		id              BIGINT PRIMARY KEY,
		ledger_id       TEXT NOT NULL,
		provider        TEXT NOT NULL DEFAULT '',
		provider_account TEXT NOT NULL DEFAULT '',
		model           TEXT NOT NULL DEFAULT '',
		tokens_in       BIGINT NOT NULL DEFAULT 0,
		tokens_out      BIGINT NOT NULL DEFAULT 0,
		cost_credits    NUMERIC(20,5) NOT NULL DEFAULT 0,
		occurred_at     TIMESTAMPTZ NOT NULL
	)`, LedgerTable)
	if _, err := conn.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`CREATE UNIQUE INDEX %s_ledger_id_key ON %s(ledger_id)`, LedgerTable, LedgerTable)); err != nil {
		t.Fatalf("建唯一索引失败: %v", err)
	}

	first := validEntry("led-same")
	second := validEntry("led-other")
	second.OccurredAt = first.OccurredAt.Add(time.Second)
	store := NewLedgerStore(conn)

	// 第一轮：两条都新 → 插入 2。
	tx, err := store.Begin(ctx)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	n, err := tx.Upsert(ctx, []LedgerEntry{first, second})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("首次写入应插入 2 条，got %d", n)
	}

	// 第二轮：同一条账本重复出现（窗口重叠导致）→ 只算 1 条新（另一条被幂等键吃掉）。
	tx, err = store.Begin(ctx)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	third := validEntry("led-third")
	third.OccurredAt = second.OccurredAt.Add(time.Second)
	n, err = tx.Upsert(ctx, []LedgerEntry{first, second, third})
	if err != nil {
		t.Fatalf("重复写入失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("重复的 ledger_id 不应重复计数：应只插入 1 条，got %d", n)
	}

	// 库里总数 = 3（重复写入没有多出记录）。
	var total int
	if err := conn.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, LedgerTable)).Scan(&total); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if total != 3 {
		t.Fatalf("库内应有 3 条账本，got %d", total)
	}

	// offset 分页边界：limit=2 第一页 2 条、offset=2 第二页 1 条、offset=3 空页。
	page1, err := store.List(ctx, 0, 2)
	if err != nil {
		t.Fatalf("分页读取失败: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("第一页应为 2 条，got %d", len(page1))
	}
	page2, err := store.List(ctx, 2, 2)
	if err != nil {
		t.Fatalf("分页读取失败: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("第二页应为 1 条，got %d", len(page2))
	}
	page3, err := store.List(ctx, 3, 2)
	if err != nil {
		t.Fatalf("分页读取失败: %v", err)
	}
	if len(page3) != 0 {
		t.Fatalf("越界 offset 应返回空页，got %d 条", len(page3))
	}
}

// TestLedgerStore_UnassembledDB 断言未装配数据库时返回错误而不是 panic。
func TestLedgerStore_UnassembledDB(t *testing.T) {
	store := NewLedgerStore((*sql.DB)(nil))
	if _, err := store.Begin(context.Background()); err == nil {
		t.Fatal("未装配 db 时 Begin 应报错")
	}
	if _, err := store.List(context.Background(), 0, 10); err == nil {
		t.Fatal("未装配 db 时 List 应报错")
	}
}

// openLedgerTempDB 以管理 DSN 建一个临时库（用例结束整体 DROP），
// 与仓库既有「真实库用例自建临时库」口径一致：分页断言不会被历史残留行干扰。
// 未设置 LLMGATEWAY_TEST_PG_DSN 或账号无 CREATEDB 权限时跳过。
func openLedgerTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过账本真实库用例")
	}
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	name := fmt.Sprintf("v2_ledger_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Skipf("无法创建临时库（账号是否没有 CREATEDB 权限？）: %v", err)
	}
	t.Cleanup(func() {
		// 用新连接执行 DROP：测试主体的连接可能已关闭。
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
	return target
}
