package subscription

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/audit"
)

type fakeStore struct {
	providers    []string
	providersErr error
	accounts     map[string]struct{}
	created      *Row
	createErr    error
	existing     *Row
	allocated    float64
	allocatedErr error
	list         []ListRow
	listErr      error
}

func (f *fakeStore) Providers(ctx context.Context) ([]string, error) {
	return f.providers, f.providersErr
}

func (f *fakeStore) KnownAccounts(ctx context.Context, since time.Time) (map[string]struct{}, error) {
	if f.accounts == nil {
		return map[string]struct{}{}, nil
	}
	return f.accounts, nil
}

func (f *fakeStore) Create(ctx context.Context, row *Row) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = row
	return nil
}

func (f *fakeStore) GetByNaturalKey(ctx context.Context, provider, account string) (*Row, error) {
	if f.existing != nil {
		return f.existing, nil
	}
	if f.created != nil {
		out := *f.created
		if out.CreatedAt.IsZero() {
			out.CreatedAt = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
			out.UpdatedAt = out.CreatedAt
		}
		return &out, nil
	}
	return nil, ErrNotFound
}

func (f *fakeStore) Allocated(ctx context.Context, provider, account string, from, to time.Time) (float64, error) {
	return f.allocated, f.allocatedErr
}

func (f *fakeStore) List(ctx context.Context) ([]ListRow, error) {
	return f.list, f.listErr
}

type fakeConfig struct {
	r      int64
	rErr   error
	cny    float64
	cnyErr error
}

func (f *fakeConfig) R() (int64, error)         { return f.r, f.rErr }
func (f *fakeConfig) CnyRate() (float64, error) { return f.cny, f.cnyErr }

type fakeAudit struct{ entries []audit.Entry }

func (f *fakeAudit) Insert(ctx context.Context, e audit.Entry) error {
	f.entries = append(f.entries, e)
	return nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestService(st *fakeStore, cfg *fakeConfig, au *fakeAudit) *Service {
	return NewService(Options{
		Store: st, Config: cfg, Audit: au, Logger: testLogger(),
		Now:   func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) },
		NewID: func() int64 { return 9001 },
	})
}

func validInput() Input {
	payDay := 5
	share := true
	return Input{Provider: "acme", Account: "acct-1", Price: 100, Currency: "CNY",
		Cycle: "month", PayDay: &payDay, ShareCost: &share, CycleStart: "2026-11-01", Note: "团队套餐"}
}

// 成功录入：落库是积分（不是元），状态/判定现算，审计留痕带目标 id。
func TestCreate_Success(t *testing.T) {
	st := &fakeStore{providers: []string{"acme"}, accounts: map[string]struct{}{"acct-1": {}}}
	au := &fakeAudit{}
	svc := newTestService(st, &fakeConfig{r: 10000, cny: 6.8}, au)

	view, berr := svc.Create(context.Background(), validInput())
	if berr != nil {
		t.Fatalf("意外错误：%+v", berr)
	}
	if st.created == nil {
		t.Fatal("未落库")
	}
	if st.created.PriceCredits != 10000 {
		t.Fatalf("price_amount 落库 = %v 积分，期望 10000（100 元 × 1e6 ÷ 10000）", st.created.PriceCredits)
	}
	if view.Price != 100 || view.PriceCredits == nil || *view.PriceCredits != 10000 {
		t.Fatalf("回读价格 = %v / %v", view.Price, view.PriceCredits)
	}
	if view.Status != StatusActive || !view.WillAmortize || len(view.BlockReasons) != 0 {
		t.Fatalf("状态/判定 = %s / %v / %v", view.Status, view.WillAmortize, view.BlockReasons)
	}
	if len(view.Warnings) != 0 {
		t.Fatalf("账号命中已知集合，不该有 warning：%+v", view.Warnings)
	}
	if view.CycleAllocated == nil || *view.CycleAllocated != 0 ||
		view.CycleUnallocated == nil || *view.CycleUnallocated != 10000 {
		t.Fatalf("本期已摊/未摊出 = %v / %v", view.CycleAllocated, view.CycleUnallocated)
	}
	if len(au.entries) != 1 || au.entries[0].Action != "subscription.create" ||
		au.entries[0].TargetID != "9001" {
		t.Fatalf("审计未落：%+v", au.entries)
	}
}

// 账号没在任何近期账单/账本里出现 → 允许保存，但必须带 account_unverified。
func TestCreate_AccountUnverifiedWarning(t *testing.T) {
	st := &fakeStore{providers: []string{"acme"}, accounts: map[string]struct{}{}}
	svc := newTestService(st, &fakeConfig{r: 10000}, &fakeAudit{})

	view, berr := svc.Create(context.Background(), validInput())
	if berr != nil {
		t.Fatalf("软校验不该阻断：%+v", berr)
	}
	if len(view.Warnings) != 1 || view.Warnings[0].Code != WarningAccountUnverified {
		t.Fatalf("warnings = %+v", view.Warnings)
	}
}

// 拓扑为空 → 409 topology_not_synced；不落库。
func TestCreate_TopologyNotSynced(t *testing.T) {
	st := &fakeStore{providers: nil}
	svc := newTestService(st, &fakeConfig{r: 10000}, &fakeAudit{})

	_, berr := svc.Create(context.Background(), validInput())
	if berr == nil || berr.Status != 409 || berr.Reason != ReasonTopologyNotSynced {
		t.Fatalf("err = %+v", berr)
	}
	if st.created != nil {
		t.Fatal("拓扑未同步时不许落库")
	}
}

// 供应商不在镜像里 → 400 provider_not_found（改字段即可，不用去同步）。
func TestCreate_ProviderNotFound(t *testing.T) {
	st := &fakeStore{providers: []string{"borealis"}}
	svc := newTestService(st, &fakeConfig{r: 10000}, &fakeAudit{})

	_, berr := svc.Create(context.Background(), validInput())
	if berr == nil || berr.Status != 400 || berr.Reason != ReasonProviderNotFound || berr.Field != "Provider" {
		t.Fatalf("err = %+v", berr)
	}
}

// 自然键重复 → 409 duplicate_subscription + ExistingID（不 upsert、不静默覆盖）。
func TestCreate_Duplicate(t *testing.T) {
	st := &fakeStore{
		providers: []string{"acme"},
		createErr: ErrDuplicate,
		existing:  &Row{ID: 777, Provider: "acme", Account: "acct-1"},
	}
	svc := newTestService(st, &fakeConfig{r: 10000}, &fakeAudit{})

	_, berr := svc.Create(context.Background(), validInput())
	if berr == nil || berr.Status != 409 || berr.Reason != ReasonDuplicate {
		t.Fatalf("err = %+v", berr)
	}
	data, ok := berr.Data.(map[string]any)
	if !ok || data["ExistingID"] != "777" {
		t.Fatalf("Data = %+v，期望带 ExistingID=777", berr.Data)
	}
	if !strings.Contains(berr.Message, "777") {
		t.Errorf("文案应点名现有 id：%s", berr.Message)
	}
}

// USD 缺汇率 → 422 rate_not_configured，且必须在任何写动作之前拦下。
func TestCreate_USDWithoutRate(t *testing.T) {
	st := &fakeStore{providers: []string{"acme"}}
	svc := newTestService(st, &fakeConfig{r: 10000, cny: 0}, &fakeAudit{})

	in := validInput()
	in.Currency = "USD"
	_, berr := svc.Create(context.Background(), in)
	if berr == nil || berr.Status != 422 || berr.Reason != ReasonRateNotConfigured {
		t.Fatalf("err = %+v", berr)
	}
	if st.created != nil {
		t.Fatal("缺汇率时不许落库")
	}
}

// 语法错误先于拓扑判定：否则运营会照着 topology_not_synced 去同步，回来还是 400。
func TestCreate_ValidationBeforeTopology(t *testing.T) {
	st := &fakeStore{providers: nil}
	svc := newTestService(st, &fakeConfig{r: 10000}, &fakeAudit{})

	in := validInput()
	in.Price = -1
	_, berr := svc.Create(context.Background(), in)
	if berr == nil || berr.Reason != ReasonInvalidField || berr.Field != "Price" {
		t.Fatalf("err = %+v，期望先报 invalid_field/Price", berr)
	}
}

// R 配置缺失是服务端问题（500），不能伪装成用户输入错误。
func TestCreate_RMissing(t *testing.T) {
	st := &fakeStore{providers: []string{"acme"}}
	svc := newTestService(st, &fakeConfig{rErr: errors.New("sys_configs 无键 billing.r")}, &fakeAudit{})

	_, berr := svc.Create(context.Background(), validInput())
	if berr == nil || berr.Status != 500 || berr.Reason != ReasonRNotConfigured {
		t.Fatalf("err = %+v", berr)
	}
}

// 本期已摊读失败 → 两个字段回 null（不许用 0 冒充「算过」）。
func TestView_AllocatedErrorKeepsNull(t *testing.T) {
	st := &fakeStore{providers: []string{"acme"}, allocatedErr: errors.New("db down")}
	svc := newTestService(st, &fakeConfig{r: 10000}, &fakeAudit{})

	view, berr := svc.Create(context.Background(), validInput())
	if berr != nil {
		t.Fatalf("意外错误：%+v", berr)
	}
	if view.CycleAllocated != nil || view.CycleUnallocated != nil {
		t.Fatalf("读失败时必须是 null：%v / %v", view.CycleAllocated, view.CycleUnallocated)
	}
}
