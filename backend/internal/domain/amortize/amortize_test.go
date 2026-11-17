package amortize

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestAllocateShares_ProRataAndExactTotal：按 token 比例摊，且合计严格等于订阅价。
func TestAllocateShares_ProRataAndExactTotal(t *testing.T) {
	rows := []Row{
		{BillingID: "b1", Tokens: 100},
		{BillingID: "b2", Tokens: 300},
		{BillingID: "b3", Tokens: 0}, // 没用量不分摊
	}
	shares := AllocateShares(100, rows)
	if len(shares) != 2 {
		t.Fatalf("0 用量的行不该分摊，got %+v", shares)
	}
	sum := 0.0
	for _, s := range shares {
		sum += s.Cost
	}
	if sum != 100 {
		t.Fatalf("合计必须等于订阅价 100，got %v（%+v）", sum, shares)
	}
	byID := map[string]float64{}
	for _, s := range shares {
		byID[s.BillingID] = s.Cost
	}
	if byID["b1"] != 25 || byID["b2"] != 75 {
		t.Fatalf("比例应为 1:3：%+v", byID)
	}
}

// TestAllocateShares_RoundingRemainderLandsOnLargest：除不尽时差额补到 token 最多的行。
func TestAllocateShares_RoundingRemainderLandsOnLargest(t *testing.T) {
	rows := []Row{
		{BillingID: "b1", Tokens: 1},
		{BillingID: "b2", Tokens: 1},
		{BillingID: "b3", Tokens: 1},
	}
	shares := AllocateShares(10, rows)
	sum := 0.0
	largest := 0.0
	for _, s := range shares {
		sum += s.Cost
		if s.Cost > largest {
			largest = s.Cost
		}
	}
	if sum != 10 {
		t.Fatalf("合计必须等于 10，got %v", sum)
	}
	// 3 等分 10 → 3.33333 * 3 = 9.99999，差额 0.00001 补一行。
	if largest != 3.33334 {
		t.Fatalf("差额应补在 token 最多的行（这里是第一行），got %v（%+v）", largest, shares)
	}
}

// TestAllocateShares_NoTokensEqualSplit：有账单但 token 全 0 → 均分（固定成本照样要摊出去）。
func TestAllocateShares_NoTokensEqualSplit(t *testing.T) {
	shares := AllocateShares(6, []Row{{BillingID: "b1"}, {BillingID: "b2"}})
	if len(shares) != 2 || shares[0].Cost != 3 || shares[1].Cost != 3 {
		t.Fatalf("应均分为 3/3：%+v", shares)
	}
}

// TestAllocateShares_EmptyRows：没账单 → nil（调用方记「未摊出去」并告警）。
func TestAllocateShares_EmptyRows(t *testing.T) {
	if got := AllocateShares(50, nil); got != nil {
		t.Fatalf("没有账单应返回 nil，got %+v", got)
	}
	if got := AllocateShares(0, []Row{{BillingID: "b1", Tokens: 1}}); got != nil {
		t.Fatalf("价格为 0 不该摊，got %+v", got)
	}
}

// TestCycleEnd：月费 +1 月、年费 +1 年、未知按 1 月。
func TestCycleEnd(t *testing.T) {
	start := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	if got := cycleEnd(start, CycleMonth); !got.Equal(time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)) {
		// Go 的 AddDate 会把 1/31 + 1 月归一到 3/3（2 月没有 31 日）——这是可接受的口径，
		// 重要的是「不会跨过一整年」。这里显式锁定行为，改动时会红。
		t.Fatalf("月费右界口径变了：%s", got)
	}
	if got := cycleEnd(start, CycleYear); !got.Equal(time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("年费应 +1 年，got %s", got)
	}
	if got := cycleEnd(start, "unknown"); !got.Equal(cycleEnd(start, CycleMonth)) {
		t.Fatalf("未知账期应按 1 月，got %s", got)
	}
}

// fakeStore 是 Store 的用例替身。
type fakeStore struct {
	subs    []Subscription
	rows    map[string][]Row
	blocked map[string]bool
	err     error
	calls   []string
}

func (f *fakeStore) Subscriptions(context.Context) ([]Subscription, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.subs, nil
}

func (f *fakeStore) CycleRows(_ context.Context, provider, account string, _, _ time.Time) ([]Row, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows[provider+"/"+account], nil
}

func (f *fakeStore) Allocate(_ context.Context, billingID string, cost float64) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if f.blocked[billingID] {
		return false, nil
	}
	f.calls = append(f.calls, billingID)
	return true, nil
}

// TestAllocator_RunOnce_AllocatesAndCounts：一轮 = 读订阅 + 按账期摊 + 汇总。
func TestAllocator_RunOnce_AllocatesAndCounts(t *testing.T) {
	fs := &fakeStore{
		subs: []Subscription{{
			Provider: "p", Account: "a", Price: 30, Cycle: CycleMonth,
			CycleStart: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		}},
		rows:    map[string][]Row{"p/a": {{BillingID: "b1", Tokens: 10}, {BillingID: "b2", Tokens: 20}}},
		blocked: map[string]bool{},
	}
	a := NewAllocator(Options{Store: fs, Now: func() time.Time { return time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC) }})
	res, err := a.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("分摊轮次：%v", err)
	}
	if res.Subscriptions != 1 || res.Updated != 2 {
		t.Fatalf("汇总不符：%+v", res)
	}
	if res.Allocated != 30 {
		t.Fatalf("摊出去的成本合计应为 30，got %v", res.Allocated)
	}
	if res.Unallocated != 0 {
		t.Fatalf("不该有摊不出去的钱：%v", res.Unallocated)
	}
}

// TestAllocator_RunOnce_SkipsUnallocatable：无账期/一次性 → 不摊，计入 Skipped；
// 账期内没账单 → Unallocated 告警（不许静默吞掉固定成本）。
func TestAllocator_RunOnce_SkipsUnallocatable(t *testing.T) {
	fs := &fakeStore{
		subs: []Subscription{
			{Provider: "p", Account: "a", Price: 10, Cycle: CycleMonth},                         // 无 cycle_start
			{Provider: "p", Account: "b", Price: 10, Cycle: CycleOnce, CycleStart: time.Now()},  // 一次性
			{Provider: "p", Account: "c", Price: 88, Cycle: CycleMonth, CycleStart: time.Now()}, // 没账单
			{Provider: "p", Account: "d", Price: 0, Cycle: CycleMonth, CycleStart: time.Now()},  // 价格 0
		},
		rows:    map[string][]Row{},
		blocked: map[string]bool{},
	}
	a := NewAllocator(Options{Store: fs})
	res, err := a.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("分摊轮次：%v", err)
	}
	if res.Skipped != 3 {
		t.Fatalf("应跳过 3 条（无账期/一次性/价格 0），got %+v", res)
	}
	if res.Unallocated != 88 {
		t.Fatalf("账期内没账单的那笔应计入未摊出：%+v", res)
	}
}

// TestAllocator_RunOnce_LedgerTookRowIsSkipped：某行刚被账本成本占用 → 不计入 Updated。
func TestAllocator_RunOnce_LedgerTookRowIsSkipped(t *testing.T) {
	fs := &fakeStore{
		subs:    []Subscription{{Provider: "p", Account: "a", Price: 20, Cycle: CycleMonth, CycleStart: time.Now()}},
		rows:    map[string][]Row{"p/a": {{BillingID: "b1", Tokens: 10}, {BillingID: "b2", Tokens: 10}}},
		blocked: map[string]bool{"b1": true},
	}
	a := NewAllocator(Options{Store: fs})
	res, err := a.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("分摊轮次：%v", err)
	}
	if res.Updated != 1 || res.Allocated != 10 {
		t.Fatalf("被账本占用的行应跳过：%+v", res)
	}
}

// TestAllocator_RunOnce_Error：读失败上抛。
func TestAllocator_RunOnce_Error(t *testing.T) {
	a := NewAllocator(Options{Store: &fakeStore{err: errors.New("db down")}})
	if _, err := a.RunOnce(context.Background()); err == nil {
		t.Fatal("读失败应上抛")
	}
}

// TestSQLStore_Allocate_Guards：写分摊时有两条 SQL 级守卫（无账本条目 + 成本来源可覆盖）。
func TestSQLStore_Allocate_Guards(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	sqlText := `UPDATE billing_records
		    SET cost_credits = $2,
		        cost_source  = 'amortized'
		  WHERE billing_id = $1
		    AND ledger_entry_id IS NULL
		    AND (cost_source IS NULL OR cost_source IN ('none','amortized'))`
	mock.ExpectExec(regexp.QuoteMeta(sqlText)).
		WithArgs("bill-1", 12.5).
		WillReturnResult(sqlmock.NewResult(0, 1))

	store := NewSQLStore(db)
	ok, err := store.Allocate(context.Background(), "bill-1", 12.5)
	if err != nil || !ok {
		t.Fatalf("写入应成功：ok=%v err=%v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock 期望未满足：%v", err)
	}
}

// TestAmortize_RealSQL_AllocatesCycleCost：真迁移库上跑一轮——
// 订阅行可读、账期内账单被 SET 成 amortized、合计等于订阅价、已挂账本的行不动。
func TestAmortize_RealSQL_AllocatesCycleCost(t *testing.T) {
	conn := openMigratedTempDB(t)
	ctx := context.Background()

	// 造一条用户 + 两条账单 + 一条订阅（cycle_start 2026-11-01）。
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO users(id, username, password_hash, role) VALUES (1, 'u1', 'x', 'DEVELOPER')`); err != nil {
		t.Fatalf("造用户失败：%v", err)
	}
	insertBill := `INSERT INTO billing_records
		(id, billing_id, user_id, pricing_mode, external_model_name, internal_model_id,
		 call_time, tokens, r_value, upstream_provider, upstream_account, upstream_model, cost_source)
		VALUES ($1, $2, 1, 'sale', 'm', 'g/m', $3, $4::jsonb, 1, 'p', 'a', 'up', $5)`
	if _, err := conn.ExecContext(ctx, insertBill, int64(1), "b1",
		time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC), `{"input":10,"output":0}`, "none"); err != nil {
		t.Fatalf("造账单 b1 失败：%v", err)
	}
	if _, err := conn.ExecContext(ctx, insertBill, int64(2), "b2",
		time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC), `{"input":30,"output":0}`, "none"); err != nil {
		t.Fatalf("造账单 b2 失败：%v", err)
	}
	// b3 已经被账本成本占用，绝不能参与分摊。
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO billing_records
		 (id, billing_id, user_id, pricing_mode, external_model_name, internal_model_id,
		  call_time, tokens, r_value, upstream_provider, upstream_account, upstream_model,
		  cost_source, cost_credits, ledger_entry_id)
		 VALUES (3, 'b3', 1, 'sale', 'm', 'g/m', $1, '{"input":999}'::jsonb, 1, 'p', 'a', 'up',
		         'ledger', 5, 'led-3')`,
		time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("造账单 b3 失败：%v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO subscriptions(id, provider_ref, account_ref, price_amount, billing_cycle, cycle_start)
		 VALUES (1, 'p', 'a', 40, 'month', DATE '2026-11-01')`); err != nil {
		t.Fatalf("造订阅失败：%v", err)
	}

	svc := NewAllocator(Options{Store: NewSQLStore(conn)})
	res, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("分摊轮次：%v", err)
	}
	if res.Subscriptions != 1 || res.Updated != 2 || res.Allocated != 40 {
		t.Fatalf("汇总不符：%+v", res)
	}

	// b1:b2 = 10:30 → 10 / 30。
	var cost1, cost2 float64
	var src1 string
	if err := conn.QueryRowContext(ctx,
		`SELECT cost_credits, cost_source FROM billing_records WHERE billing_id='b1'`).
		Scan(&cost1, &src1); err != nil {
		t.Fatalf("读 b1 失败：%v", err)
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT cost_credits FROM billing_records WHERE billing_id='b2'`).Scan(&cost2); err != nil {
		t.Fatalf("读 b2 失败：%v", err)
	}
	if cost1 != 10 || cost2 != 30 || src1 != "amortized" {
		t.Fatalf("分摊结果不符：b1=%v b2=%v src=%s", cost1, cost2, src1)
	}

	// 幂等：再跑一轮，值不变（SET 不是 ADD）。
	if _, err := svc.RunOnce(ctx); err != nil {
		t.Fatalf("第二轮分摊：%v", err)
	}
	var cost1Again float64
	if err := conn.QueryRowContext(ctx,
		`SELECT cost_credits FROM billing_records WHERE billing_id='b1'`).Scan(&cost1Again); err != nil {
		t.Fatalf("重跑后读 b1 失败：%v", err)
	}
	if cost1Again != cost1 {
		t.Fatalf("重跑不该翻倍：%v → %v", cost1, cost1Again)
	}

	// 账本占用的行原样。
	var cost3 float64
	var ledger3 sql.NullString
	if err := conn.QueryRowContext(ctx,
		`SELECT cost_credits, ledger_entry_id FROM billing_records WHERE billing_id='b3'`).
		Scan(&cost3, &ledger3); err != nil {
		t.Fatalf("读 b3 失败：%v", err)
	}
	if cost3 != 5 || ledger3.String != "led-3" {
		t.Fatalf("账本行不许被分摊覆盖：cost=%v ledger=%v", cost3, ledger3)
	}
}
