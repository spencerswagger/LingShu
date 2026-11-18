package subscription

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/db"
	"github.com/team/llmgateway/internal/pkg/billingday"
)

// 造一条用户 + 一条订阅 + 若干账单（cost_source='amortized'，成本已写死）。
//
// 账单时刻用**北京时间的账期首日凌晨**：这正是 #88 ① 的现场 ——
// 北京 11-01 01:00 = 10-31T17:00Z，比「DATE 按 UTC 转换」出来的窗口起点（11-01T00:00Z）更早。
// 旧实现会把这笔漏掉，于是「本期已摊 = 0」。
func seedAmortizedBills(t *testing.T, conn *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO users(id, username, password_hash, role) VALUES (1, 'u1', 'x', 'DEVELOPER')`); err != nil {
		t.Fatalf("造用户失败：%v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO subscriptions(id, provider_ref, account_ref, price_amount, billing_cycle, cycle_start)
		 VALUES (1, 'p', 'a', 40, 'month', DATE '2026-11-01')`); err != nil {
		t.Fatalf("造订阅失败：%v", err)
	}
	insertBill := `INSERT INTO billing_records
		(id, billing_id, user_id, pricing_mode, external_model_name, internal_model_id,
		 call_time, tokens, r_value, upstream_provider, upstream_account, upstream_model,
		 cost_source, cost_credits)
		VALUES ($1, $2, 1, 'sale', 'm', 'g/m', $3, '{"input":10}'::jsonb, 1, 'p', 'a', 'up', 'amortized', $4)`
	loc := billingday.Location()
	// 账期首日凌晨（北京 01:00）：必须是「本期」。
	if _, err := conn.ExecContext(ctx, insertBill, int64(1), "b-in-cycle",
		time.Date(2026, 11, 1, 1, 0, 0, 0, loc), 7.0); err != nil {
		t.Fatalf("造账期内账单失败：%v", err)
	}
	// 账期前一小时（北京 10-31 23:30）：不属于本期，绝不能被算进来。
	if _, err := conn.ExecContext(ctx, insertBill, int64(2), "b-before",
		time.Date(2026, 10, 31, 23, 30, 0, 0, loc), 5.0); err != nil {
		t.Fatalf("造账期前账单失败：%v", err)
	}
}

// #88 ①：单条（S2）与列表（S1）必须算出**同一个账期窗口**。
//
// 真库实测口径：账期首日凌晨 01:00（北京）那笔必须被两边都算进来；
// 账期前 30 分钟那笔必须被两边都排除。旧实现 S2 用 UTC 0 点当窗口起点 → 比列表晚 8 小时、
// 首日凌晨的账被漏掉，页面出现「列表说已摊 7 / 单条说 0」。
func TestSQLStore_AllocatedWindowMatchesListWindow(t *testing.T) {
	conn, _, _ := openSubscriptionTempDB(t)
	seedAmortizedBills(t, conn)
	ctx := context.Background()
	store := NewSQLStore(conn)

	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) // DATE '2026-11-01' 扫出来的样子
	from := billingday.CivilDay(start)
	to := billingday.CycleEnd(from, CycleMonth)

	if from.In(billingday.Location()).Hour() != 0 || from.Day() != 1 || from.Month() != time.November {
		t.Fatalf("账期日还原错误：%s", from)
	}
	single, err := store.Allocated(ctx, "p", "a", from, to)
	if err != nil {
		t.Fatalf("单条已摊：%v", err)
	}
	if single != 7 {
		t.Fatalf("单条已摊 = %v，期望 7（账期首日凌晨那笔必须算进来）", single)
	}
	rows, err := store.List(ctx)
	if err != nil {
		t.Fatalf("列表：%v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("列表行数 = %d", len(rows))
	}
	if rows[0].Allocated != single {
		t.Fatalf("列表已摊 = %v 与单条 %v 不一致（同一个账期必须同一个窗口）", rows[0].Allocated, single)
	}
	if rows[0].Allocated != 7 {
		t.Fatalf("列表已摊 = %v，期望 7", rows[0].Allocated)
	}
}

// 列表窗口不许依赖 PG 会话的 TimeZone：把库级默认时区改成 America/New_York 后必须同值。
//
// 旧实现的 SQL 是 `b.call_time >= s.cycle_start`（DATE 与 timestamptz 比较），
// PG 会按**会话时区**把 DATE 转成时刻 —— 会话一变，窗口就整体错位。
func TestSQLStore_ListWindowIgnoresSessionTimezone(t *testing.T) {
	conn, dsn, name := openSubscriptionTempDB(t)
	seedAmortizedBills(t, conn)
	ctx := context.Background()

	base, err := NewSQLStore(conn).List(ctx)
	if err != nil {
		t.Fatalf("列表：%v", err)
	}

	// 改库级默认时区后**新开连接**（池里已有连接仍是旧时区，所以必须换句柄）。
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败：%v", err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.ExecContext(ctx, `ALTER DATABASE `+name+` SET timezone TO 'America/New_York'`); err != nil {
		t.Fatalf("改库级时区失败：%v", err)
	}
	other, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("重开连接失败：%v", err)
	}
	defer func() { _ = other.Close() }()

	var tz string
	if err := other.QueryRowContext(ctx, `SHOW timezone`).Scan(&tz); err != nil {
		t.Fatalf("读会话时区失败：%v", err)
	}
	if tz != "America/New_York" {
		t.Fatalf("会话时区没切过去（%s），本用例不成立", tz)
	}

	shifted, err := NewSQLStore(other).List(ctx)
	if err != nil {
		t.Fatalf("换时区后列表：%v", err)
	}
	if len(shifted) != 1 || shifted[0].Allocated != base[0].Allocated {
		t.Fatalf("会话时区影响了窗口：%v vs %v（SQL 必须显式 AT TIME ZONE）",
			shifted, base)
	}
	if shifted[0].Allocated != 7 {
		t.Fatalf("换时区后已摊 = %v，期望 7", shifted[0].Allocated)
	}
}

// 写 DATE 列不许经过「按会话时区取日期」：纽约会话下写 2026-11-17 仍必须是 11-17。
//
// 旧实现把 time.Time（DATE 扫出来的 UTC 0 点）直接塞进 DATE 列 —— 纽约会话下
// `2026-11-17T00:00Z` 会被取成 11-16，整整差一天。
func TestSQLStore_CreateCycleStartIsDateLiteral(t *testing.T) {
	_, dsn, name := openSubscriptionTempDB(t)
	ctx := context.Background()
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败：%v", err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.ExecContext(ctx, `ALTER DATABASE `+name+` SET timezone TO 'America/New_York'`); err != nil {
		t.Fatalf("改库级时区失败：%v", err)
	}
	other, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("重开连接失败：%v", err)
	}
	defer func() { _ = other.Close() }()

	store := NewSQLStore(other)
	start := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC) // 录入侧 Parse 出来的样子
	row := &Row{ID: 900, Provider: "p", Account: "a", PriceCredits: 100, Currency: "CNY",
		Cycle: CycleMonth, PayDay: 1, ShareCost: true, CycleStart: &start}
	if err := store.Create(ctx, row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := store.GetByNaturalKey(ctx, "p", "a")
	if err != nil {
		t.Fatalf("GetByNaturalKey: %v", err)
	}
	if got.CycleStart == nil {
		t.Fatal("cycle_start 丢了")
	}
	if d := billingday.Date(*got.CycleStart); d != "2026-11-17" {
		t.Fatalf("写入的账期日 = %s，期望 2026-11-17（DATE 必须走字面量，不经会话时区）", d)
	}
}

// #88 ①（月末）：列表窗口用的是 PG 的 `+ interval '1 month'`（月末**截断**），
// 分摊窗口（Go）用的是 billingday.CycleEnd。两者必须同语义，否则月末起账的订阅
// 会在 2-28 ~ 3-3 这几天出现「列表说没摊、分摊却摊了」。
//
// 现场：cycle_start = 2026-01-31。
//
//	PG：  2026-02-28（截断）
//	Go 原生 AddDate：2026-03-03（溢出）→ 3-01 那笔会被分摊算进 1 月账期，列表却不认
func TestSQLStore_MonthEndWindowMatchesGoCycleEnd(t *testing.T) {
	conn, _, _ := openSubscriptionTempDB(t)
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO users(id, username, password_hash, role) VALUES (1, 'u1', 'x', 'DEVELOPER')`); err != nil {
		t.Fatalf("造用户失败：%v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO subscriptions(id, provider_ref, account_ref, price_amount, billing_cycle, cycle_start)
		 VALUES (1, 'p', 'a', 40, 'month', DATE '2026-01-31')`); err != nil {
		t.Fatalf("造订阅失败：%v", err)
	}
	loc := billingday.Location()
	insertBill := `INSERT INTO billing_records
		(id, billing_id, user_id, pricing_mode, external_model_name, internal_model_id,
		 call_time, tokens, r_value, upstream_provider, upstream_account, upstream_model,
		 cost_source, cost_credits)
		VALUES ($1, $2, 1, 'sale', 'm', 'g/m', $3, '{"input":10}'::jsonb, 1, 'p', 'a', 'up', 'amortized', $4)`
	// 期内（2-27）
	if _, err := conn.ExecContext(ctx, insertBill, int64(1), "b-in",
		time.Date(2026, 2, 27, 12, 0, 0, 0, loc), 7.0); err != nil {
		t.Fatalf("造账单失败：%v", err)
	}
	// 期外（3-01）：Go 溢出窗口（到 3-03）会把它算进来，PG 截断窗口（到 2-28）不会
	if _, err := conn.ExecContext(ctx, insertBill, int64(2), "b-after",
		time.Date(2026, 3, 1, 12, 0, 0, 0, loc), 5.0); err != nil {
		t.Fatalf("造账单失败：%v", err)
	}

	rows, err := NewSQLStore(conn).List(ctx)
	if err != nil {
		t.Fatalf("列表：%v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("列表行数 = %d", len(rows))
	}
	if rows[0].Allocated != 7 {
		t.Fatalf("列表已摊 = %v，期望 7（3-01 那笔属下一账期）", rows[0].Allocated)
	}

	start := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC) // DATE 扫出来的样子
	to := billingday.CycleEnd(billingday.CivilDay(start), CycleMonth)
	if got := to.Format("2006-01-02"); got != "2026-02-28" {
		t.Fatalf("Go 侧账期右界 = %s，期望 2026-02-28（必须与 PG interval 同语义）", got)
	}
	single, err := NewSQLStore(conn).Allocated(ctx, "p", "a", billingday.CivilDay(start), to)
	if err != nil {
		t.Fatalf("单条已摊：%v", err)
	}
	if single != rows[0].Allocated {
		t.Fatalf("单条已摊 = %v 与列表 %v 不一致", single, rows[0].Allocated)
	}
}
