package ledgersettle

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("时间解析失败 %q: %v", s, err)
	}
	return v
}

// TestMatchRows_NearestWithinTolerance：同键多条时按「时间最近」配对，而不是按顺序硬配。
func TestMatchRows_NearestWithinTolerance(t *testing.T) {
	t0 := mustTime(t, "2026-11-16T10:00:00Z")
	bills := []BillingRow{
		{BillingID: "b1", Provider: "p", Account: "a", Model: "m", CallTime: t0, Tokens: 100},
		{BillingID: "b2", Provider: "p", Account: "a", Model: "m", CallTime: t0.Add(30 * time.Second), Tokens: 200},
	}
	ledgers := []LedgerRow{
		{LedgerID: "l2", Provider: "p", Account: "a", Model: "m", TokensIn: 150, TokensOut: 40, Cost: 1.5, OccurredAt: t0.Add(32 * time.Second)},
		{LedgerID: "l1", Provider: "p", Account: "a", Model: "m", TokensIn: 60, TokensOut: 30, Cost: 0.5, OccurredAt: t0.Add(2 * time.Second)},
	}
	res := MatchRows(bills, ledgers, 2*time.Minute)
	if len(res.Matched) != 2 {
		t.Fatalf("应配上 2 对，got %+v", res)
	}
	// 配对形状：l1↔b1（2s）、l2↔b2（2s）。
	byBilling := map[string]Match{}
	for _, m := range res.Matched {
		byBilling[m.BillingID] = m
	}
	if byBilling["b1"].LedgerID != "l1" || byBilling["b2"].LedgerID != "l2" {
		t.Fatalf("应按时间最近配对：%+v", byBilling)
	}
	if byBilling["b1"].TokenDelta != 100-90 {
		t.Fatalf("token 差值应为 灵枢-账本 = 10，got %d", byBilling["b1"].TokenDelta)
	}
	if byBilling["b1"].Cost != 0.5 {
		t.Fatalf("成本应原样带过来，got %v", byBilling["b1"].Cost)
	}
}

// TestMatchRows_StrictTripleKey：三列任缺一列都不匹配（不按 model 单键猜）。
func TestMatchRows_StrictTripleKey(t *testing.T) {
	t0 := mustTime(t, "2026-11-16T10:00:00Z")
	bills := []BillingRow{
		{BillingID: "b1", Provider: "p", Account: "a", Model: "m", CallTime: t0},
		{BillingID: "b2", Provider: "p", Account: "OTHER", Model: "m", CallTime: t0},
		{BillingID: "b3", Provider: "p", Account: "a", Model: "", CallTime: t0},
	}
	ledgers := []LedgerRow{
		{LedgerID: "l1", Provider: "p", Account: "a", Model: "m", OccurredAt: t0},
		{LedgerID: "l2", Provider: "", Account: "a", Model: "m", OccurredAt: t0},
	}
	res := MatchRows(bills, ledgers, time.Minute)
	if len(res.Matched) != 1 || res.Matched[0].BillingID != "b1" {
		t.Fatalf("只该配上 b1↔l1：%+v", res.Matched)
	}
	// 没配上的：b2（账号不同）、b3（模型缺失）、l2（供应商缺失）。
	if res.UnmatchedBilling != 2 || res.UnmatchedLedger != 1 {
		t.Fatalf("未匹配计数不符：billing=%d ledger=%d", res.UnmatchedBilling, res.UnmatchedLedger)
	}
}

// TestMatchRows_ToleranceBoundary：超容差就不配对（宁可不结，也不错结）。
func TestMatchRows_ToleranceBoundary(t *testing.T) {
	t0 := mustTime(t, "2026-11-16T10:00:00Z")
	bills := []BillingRow{{BillingID: "b1", Provider: "p", Account: "a", Model: "m", CallTime: t0}}
	ledgers := []LedgerRow{{LedgerID: "l1", Provider: "p", Account: "a", Model: "m", OccurredAt: t0.Add(121 * time.Second)}}
	res := MatchRows(bills, ledgers, 2*time.Minute)
	if len(res.Matched) != 0 || res.UnmatchedLedger != 1 || res.UnmatchedBilling != 1 {
		t.Fatalf("超容差不应配对：%+v", res)
	}
	// 容差放宽到 3 分钟就该配上。
	res = MatchRows(bills, ledgers, 3*time.Minute)
	if len(res.Matched) != 1 {
		t.Fatalf("容差内应配上：%+v", res)
	}
}

// TestMatchRows_OneToOne：一条账本条目只能配一条账单。
func TestMatchRows_OneToOne(t *testing.T) {
	t0 := mustTime(t, "2026-11-16T10:00:00Z")
	bills := []BillingRow{{BillingID: "b1", Provider: "p", Account: "a", Model: "m", CallTime: t0}}
	ledgers := []LedgerRow{
		{LedgerID: "l1", Provider: "p", Account: "a", Model: "m", OccurredAt: t0},
		{LedgerID: "l2", Provider: "p", Account: "a", Model: "m", OccurredAt: t0.Add(time.Second)},
	}
	res := MatchRows(bills, ledgers, time.Minute)
	if len(res.Matched) != 1 {
		t.Fatalf("一条账单最多配一条账本条目：%+v", res.Matched)
	}
	if res.UnmatchedLedger != 1 {
		t.Fatalf("多余账本条目应计入未匹配，got %d", res.UnmatchedLedger)
	}
}

// fakeStore 是 Store 的用例替身（可编程返回与错误）。
type fakeStore struct {
	bills    []BillingRow
	ledgers  []LedgerRow
	linkFail map[string]bool
	linkErr  error
	linked   []string
	readErr  error
}

func (f *fakeStore) UnmatchedBilling(context.Context, time.Time, time.Time) ([]BillingRow, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.bills, nil
}

func (f *fakeStore) LedgerRows(context.Context, time.Time, time.Time) ([]LedgerRow, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.ledgers, nil
}

func (f *fakeStore) Link(_ context.Context, m Match) (bool, error) {
	if f.linkErr != nil {
		return false, f.linkErr
	}
	if f.linkFail[m.BillingID] {
		return false, nil
	}
	f.linked = append(f.linked, m.BillingID+"->"+m.LedgerID)
	return true, nil
}

// TestMatcher_RunOnce_LinksAndCounts：一轮 = 读两侧 + 匹配 + 落库 + 汇总。
func TestMatcher_RunOnce_LinksAndCounts(t *testing.T) {
	t0 := mustTime(t, "2026-11-16T09:59:30Z")
	fs := &fakeStore{
		bills: []BillingRow{
			{BillingID: "b1", Provider: "p", Account: "a", Model: "m", CallTime: t0},
			{BillingID: "b2", Provider: "p", Account: "a", Model: "m", CallTime: t0},
		},
		ledgers: []LedgerRow{
			{LedgerID: "l1", Provider: "p", Account: "a", Model: "m", Cost: 2.25, OccurredAt: t0.Add(time.Second)},
		},
		linkFail: map[string]bool{},
	}
	m := NewMatcher(Options{Store: fs, Now: func() time.Time { return mustTime(t, "2026-11-16T10:01:00Z") }})
	res, err := m.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("结算轮次：%v", err)
	}
	if res.Linked != 1 || res.Cost != 2.25 {
		t.Fatalf("应落 1 条、成本 2.25：%+v", res)
	}
	if len(fs.linked) != 1 {
		t.Fatalf("Link 调用次数不符：%v", fs.linked)
	}
	if res.UnmatchedBilling != 1 {
		t.Fatalf("另一条账单没账本条目应计入未匹配，got %d", res.UnmatchedBilling)
	}
}

// TestMatcher_RunOnce_SkipCounts：并发下账单已被别的条目占用 → 计入 Skipped，不算失败。
func TestMatcher_RunOnce_SkipCounts(t *testing.T) {
	t0 := mustTime(t, "2026-11-16T09:59:30Z")
	fs := &fakeStore{
		bills:    []BillingRow{{BillingID: "b1", Provider: "p", Account: "a", Model: "m", CallTime: t0}},
		ledgers:  []LedgerRow{{LedgerID: "l1", Provider: "p", Account: "a", Model: "m", Cost: 1, OccurredAt: t0}},
		linkFail: map[string]bool{"b1": true},
	}
	m := NewMatcher(Options{Store: fs, Now: func() time.Time { return t0.Add(time.Minute) }})
	res, err := m.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("跳过不算错误：%v", err)
	}
	if res.Linked != 0 || res.Skipped != 1 {
		t.Fatalf("应计 1 条跳过：%+v", res)
	}
}

// TestMatcher_RunOnce_ReadError：读失败直接上抛（本轮不动库）。
func TestMatcher_RunOnce_ReadError(t *testing.T) {
	fs := &fakeStore{readErr: errors.New("db down"), linkFail: map[string]bool{}}
	m := NewMatcher(Options{Store: fs})
	if _, err := m.RunOnce(context.Background()); err == nil {
		t.Fatal("读失败应上抛")
	}
}

// 与 settle.go 逐字对应的 SQL（sqlmock 会折叠空白）。
const (
	linkBillingSQL = `UPDATE billing_records
		    SET ledger_entry_id = $2,
		        cost_credits    = $3,
		        cost_source     = 'ledger'
		  WHERE billing_id = $1
		    AND ledger_entry_id IS NULL`
)

// TestSQLStore_Link_WritesCost：成本列在同一事务里写（账本已占用则不碰）。
func TestSQLStore_Link_WritesCost(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(linkBillingSQL)).
		WithArgs("bill-1", "led-1", 3.5).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	store := NewSQLStore(db)
	ok, err := store.Link(context.Background(), Match{BillingID: "bill-1", LedgerID: "led-1", Cost: 3.5})
	if err != nil || !ok {
		t.Fatalf("落库应成功：ok=%v err=%v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock 期望未满足：%v", err)
	}
}

// TestSQLStore_Link_SkipsWhenAlreadyLinked：账单已被占用 → 不碰登记、返回 false。
func TestSQLStore_Link_SkipsWhenAlreadyLinked(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(linkBillingSQL)).
		WithArgs("bill-1", "led-1", 1.0).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	store := NewSQLStore(db)
	ok, err := store.Link(context.Background(), Match{BillingID: "bill-1", LedgerID: "led-1", Cost: 1})
	if err != nil {
		t.Fatalf("已被占用不是错误：%v", err)
	}
	if ok {
		t.Fatal("没写到行应返回 false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock 期望未满足（不该更新登记）：%v", err)
	}
}
