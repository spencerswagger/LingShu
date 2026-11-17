package upstream

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
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

// TestBuildLedgerInsert_IdempotentShape 断言写入 SQL 的幂等形状：九列 + ON CONFLICT (ledger_id) DO UPDATE。
func TestBuildLedgerInsert_IdempotentShape(t *testing.T) {
	e1, e2 := validEntry("led-1"), validEntry("led-2")
	query, args := buildLedgerInsert([]LedgerEntry{e1, e2})

	if !strings.Contains(query, "INSERT INTO "+LedgerTable) {
		t.Fatalf("SQL 应写入 %s：%s", LedgerTable, query)
	}
	if !strings.Contains(query, "ON CONFLICT (ledger_id) DO UPDATE") {
		t.Fatalf("SQL 必须用 ledger_id 做幂等冲突键，并覆盖同 id 的高 seq 新值：%s", query)
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
// 不新增行，但同 id 的新值要覆盖旧值（代理侧同一 id 高 seq = 这一行被改过），并验证 offset 分页边界。
//
// 未设置 LLMGATEWAY_TEST_PG_DSN 时跳过（不阻塞普通 go test）。
// 表由本用例按架构侧定稿的九字段在**临时库**里就地建——迁移文件归架构侧 0019–0023，
// 本包不另写一套生产 DDL。
func TestLedgerStore_UpsertIdempotent_DuplicateNotCounted(t *testing.T) {
	conn := openMigratedTempDB(t)
	ctx := context.Background()
	if err := conn.PingContext(ctx); err != nil {
		t.Fatalf("数据库 ping 失败: %v", err)
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

	// 第二轮：同一条账本再次出现（游标重放/窗口重叠）→ 不新增行；同 id 的新值覆盖旧值。
	tx, err = store.Begin(ctx)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	firstUpdated := first
	firstUpdated.TokensIn = 999
	third := validEntry("led-third")
	third.OccurredAt = second.OccurredAt.Add(time.Second)
	if _, err = tx.Upsert(ctx, []LedgerEntry{firstUpdated, second, third}); err != nil {
		t.Fatalf("重复写入失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	var updatedIn int64
	if err := conn.QueryRowContext(ctx, fmt.Sprintf(`SELECT tokens_in FROM %s WHERE ledger_id=$1`, LedgerTable), first.LedgerID).Scan(&updatedIn); err != nil {
		t.Fatalf("读取覆盖后的账本失败: %v", err)
	}
	if updatedIn != 999 {
		t.Fatalf("同 id 的新值必须覆盖旧值，got tokens_in=%d", updatedIn)
	}

	// 库里总数 = 3（重复写入没有多出记录）；真实迁移建的 uq_ledger_entries_ledger_id 在守。
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

// TestLedgerStore_TruncateReplacesWholeLedger 是真实库用例：游标失效重放时「整段替换」的语义 ——
// 同一事务里先 DELETE 全表再写入重放结果；回滚时旧账本原样保留（不能留下「清空了但没重放」的空账）。
func TestLedgerStore_TruncateReplacesWholeLedger(t *testing.T) {
	conn := openMigratedTempDB(t)
	ctx := context.Background()
	store := NewLedgerStore(conn)

	stale := validEntry("u_session")
	keep := validEntry("u_gateway")
	keep.OccurredAt = stale.OccurredAt.Add(time.Second)
	tx, err := store.Begin(ctx)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	if _, err := tx.Upsert(ctx, []LedgerEntry{stale, keep}); err != nil {
		t.Fatalf("预置账本失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	// 回滚路径：清空必须跟着事务一起回滚。
	tx, err = store.Begin(ctx)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	if _, err := tx.Truncate(ctx); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if got := countLedgerRows(t, ctx, conn); got != 2 {
		t.Fatalf("回滚后旧账本必须原样保留，got %d 条", got)
	}

	// 提交路径：清空 + 只写重放回来的那一行 → 代理侧已删除的旧 id 不再残留。
	tx, err = store.Begin(ctx)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	removed, err := tx.Truncate(ctx)
	if err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if removed != 2 {
		t.Fatalf("应报告清空 2 条，got %d", removed)
	}
	if _, err := tx.Upsert(ctx, []LedgerEntry{keep}); err != nil {
		t.Fatalf("重放写入失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if got := countLedgerRows(t, ctx, conn); got != 1 {
		t.Fatalf("整段替换后应只剩重放回来的 1 条，got %d", got)
	}
	var left string
	if err := conn.QueryRowContext(ctx, fmt.Sprintf(`SELECT ledger_id FROM %s`, LedgerTable)).Scan(&left); err != nil {
		t.Fatalf("读取剩余行失败: %v", err)
	}
	if left != keep.LedgerID {
		t.Fatalf("残留的应是重放回来的 %s，got %s", keep.LedgerID, left)
	}
}

func countLedgerRows(t *testing.T, ctx context.Context, conn *sql.DB) int {
	t.Helper()
	var n int
	if err := conn.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, LedgerTable)).Scan(&n); err != nil {
		t.Fatalf("统计账本行数失败: %v", err)
	}
	return n
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
