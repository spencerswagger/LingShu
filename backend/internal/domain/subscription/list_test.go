package subscription

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func listRow(id int64, provider, account string, share bool, cycle string, start *time.Time, allocated float64) ListRow {
	created := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	return ListRow{
		Row: Row{ID: id, Provider: provider, Account: account, PriceCredits: 10000, Currency: "CNY",
			Cycle: cycle, PayDay: 5, ShareCost: share, CycleStart: start, CreatedAt: created, UpdatedAt: created},
		Allocated: allocated,
		HasCycle:  start != nil,
	}
}

func day(y int, m time.Month, d int) *time.Time {
	v := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return &v
}

// S1：状态是现算的，所以过滤在服务层做；分页按过滤后的结果切。
func TestList_FiltersAndPaginates(t *testing.T) {
	st := &fakeStore{list: []ListRow{
		listRow(1, "acme", "a1", true, CycleMonth, day(2026, 11, 1), 2500),
		listRow(2, "acme", "a2", false, CycleMonth, day(2026, 11, 1), 0),
		listRow(3, "borealis", "b1", true, CycleMonth, nil, 0), // 账期缺失 → 状态 missing_cycle
	}}
	svc := newTestService(st, &fakeConfig{r: 10000, cny: 6.8}, &fakeAudit{})

	all, berr := svc.List(context.Background(), Filter{})
	if berr != nil {
		t.Fatalf("意外错误：%+v", berr)
	}
	if all.Total != 3 || len(all.Items) != 3 {
		t.Fatalf("Total/Items = %d/%d", all.Total, len(all.Items))
	}
	// 本期已摊 2500 / 未摊出 7500 由 SQL 一次算完（不是 N+1）。
	if all.Items[0].CycleAllocated == nil || *all.Items[0].CycleAllocated != 2500 {
		t.Fatalf("CycleAllocated = %v", all.Items[0].CycleAllocated)
	}
	if all.Items[0].CycleUnallocated == nil || *all.Items[0].CycleUnallocated != 7500 {
		t.Fatalf("CycleUnallocated = %v", all.Items[0].CycleUnallocated)
	}
	// 账期缺失：两个字段必须是 null（没算过 ≠ 算了是 0）。
	if all.Items[2].CycleAllocated != nil || all.Items[2].CycleUnallocated != nil {
		t.Fatalf("账期缺失时应为 null：%v / %v", all.Items[2].CycleAllocated, all.Items[2].CycleUnallocated)
	}

	stopped, _ := svc.List(context.Background(), Filter{Status: StatusStopped})
	if stopped.Total != 1 || stopped.Items[0].ID != "2" {
		t.Fatalf("按状态过滤 = %+v", stopped.Items)
	}
	byProvider, _ := svc.List(context.Background(), Filter{Provider: "acme"})
	if byProvider.Total != 2 {
		t.Fatalf("按 provider 过滤 Total = %d", byProvider.Total)
	}
	shared, _ := svc.List(context.Background(), Filter{ShareCost: ptrBool(true)})
	if shared.Total != 2 {
		t.Fatalf("按 ShareCost 过滤 Total = %d", shared.Total)
	}

	// 分页：Total 是过滤后总数，Items 是本页。
	paged, _ := svc.List(context.Background(), Filter{Page: 2, Size: 2})
	if paged.Total != 3 || len(paged.Items) != 1 || paged.Items[0].ID != "3" || paged.Page != 2 {
		t.Fatalf("分页结果 = Total %d / %+v", paged.Total, paged.Items)
	}
}

// Size 上限与默认值：前端传 1000 不许一次拉全库（列表页会卡）。
func TestList_SizeClamped(t *testing.T) {
	st := &fakeStore{list: []ListRow{listRow(1, "acme", "a1", true, CycleMonth, day(2026, 11, 1), 0)}}
	svc := newTestService(st, &fakeConfig{r: 10000}, &fakeAudit{})

	out, _ := svc.List(context.Background(), Filter{Size: 1000})
	if out.Size != maxPageSize {
		t.Fatalf("Size = %d，期望收敛到 %d", out.Size, maxPageSize)
	}
	out, _ = svc.List(context.Background(), Filter{})
	if out.Size != defaultPageSize || out.Page != 1 {
		t.Fatalf("默认 Page/Size = %d/%d", out.Page, out.Size)
	}
}

// 列表读库失败 → 500（不能静默返回空列表：那会让运营以为订阅都没了）。
func TestList_StoreError(t *testing.T) {
	st := &fakeStore{listErr: errors.New("db down")}
	svc := newTestService(st, &fakeConfig{r: 10000}, &fakeAudit{})

	_, berr := svc.List(context.Background(), Filter{})
	if berr == nil || berr.Status != 500 {
		t.Fatalf("err = %+v", berr)
	}
}

// S7：provider 只读候选（镜像），账号软候选排序截断，R/汇率缺失时给 null。
func TestFormOptions(t *testing.T) {
	known := map[string]struct{}{}
	for _, a := range []string{"z-account", "a-account", "m-account"} {
		known[a] = struct{}{}
	}
	st := &fakeStore{providers: []string{"acme", "borealis"}, accounts: known}
	svc := newTestService(st, &fakeConfig{r: 10000, cny: 6.8}, &fakeAudit{})

	out, berr := svc.FormOptions(context.Background())
	if berr != nil {
		t.Fatalf("意外错误：%+v", berr)
	}
	if len(out.Providers) != 2 || out.Providers[0] != "acme" {
		t.Fatalf("Providers = %+v", out.Providers)
	}
	if len(out.Accounts) != 3 || out.Accounts[0] != "a-account" || out.Accounts[2] != "z-account" {
		t.Fatalf("Accounts 未排序 = %+v", out.Accounts)
	}
	if out.R == nil || *out.R != 10000 || out.CnyRate == nil || *out.CnyRate != 6.8 {
		t.Fatalf("R/CnyRate = %v/%v", out.R, out.CnyRate)
	}

	// 选不出的情况：拓扑为空 → Providers 是空数组（不是 null）；缺汇率 → CnyRate 为 null。
	empty := newTestService(&fakeStore{}, &fakeConfig{r: 10000, cnyErr: errors.New("未配置")}, &fakeAudit{})
	out, berr = empty.FormOptions(context.Background())
	if berr != nil {
		t.Fatalf("缺汇率不该让候选失败：%+v", berr)
	}
	if out.Providers == nil || len(out.Providers) != 0 || out.Accounts == nil {
		t.Fatalf("空候选必须是空数组：%+v", out)
	}
	if out.CnyRate != nil {
		t.Fatalf("缺汇率时 CnyRate 必须是 null，得到 %v", *out.CnyRate)
	}
}

// SQLStore.List 一条 SQL 算完「订阅 + 本期已摊」：列数对不上会在这里立刻红。
func TestSQLStore_List_Scan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cols := []string{"id", "provider_ref", "account_ref", "price_amount", "price_currency", "billing_cycle",
		"pay_day", "share_cost", "cycle_start", "note", "created_at", "updated_at", "has_cycle", "allocated"}
	mock.ExpectQuery(regexp.QuoteMeta("FROM subscriptions s")).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow(1, "acme", "a1", 10000, "CNY", "month", 5, true, start, "", start, start, true, 2500).
			AddRow(2, "acme", "a2", 10000, "CNY", "month", 5, true, nil, "", start, start, false, 0))

	rows, err := NewSQLStore(db).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("行数 = %d", len(rows))
	}
	if !rows[0].HasCycle || rows[0].Allocated != 2500 || rows[0].CycleStart == nil {
		t.Fatalf("第一行解析错误：%+v", rows[0])
	}
	if rows[1].HasCycle || rows[1].CycleStart != nil {
		t.Fatalf("无账期行不该有 HasCycle/CycleStart：%+v", rows[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("期望未满足：%v", err)
	}
}

// query 参数解析：坏数字按默认值处理（列表页不该因为一个坏参数整页打不开）。
func TestParseFilter(t *testing.T) {
	f := parseFilter(map[string][]string{
		"Provider":  {"acme"},
		"Account":   {"a1"},
		"Status":    {"Stopped"},
		"ShareCost": {"false"},
		"Page":      {"abc"},
		"Size":      {"50"},
	})
	if f.Provider != "acme" || f.Account != "a1" || f.Status != "Stopped" {
		t.Fatalf("字符串参数解析错误：%+v", f)
	}
	if f.ShareCost == nil || *f.ShareCost {
		t.Fatalf("ShareCost 解析错误：%+v", f.ShareCost)
	}
	if f.Page != 0 || f.Size != 50 {
		t.Fatalf("数字参数解析错误：%+v", f)
	}
	if f := parseFilter(map[string][]string{}); f.ShareCost != nil {
		t.Fatal("未传 ShareCost 必须是 nil（= 不过滤）")
	}
}
