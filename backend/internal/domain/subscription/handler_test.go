package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/amortize"
)

type fakeAmortizer struct {
	res   amortize.Result
	err   error
	calls int
}

func (f *fakeAmortizer) RunOnce(ctx context.Context) (amortize.Result, error) {
	f.calls++
	return f.res, f.err
}

func newTestHandler(st *fakeStore, cfg *fakeConfig, am *fakeAmortizer) (*Handler, *fakeAudit) {
	au := &fakeAudit{}
	h := NewHandler(newTestService(st, cfg, au), am, au, testLogger())
	h.now = func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }
	return h, au
}

func postJSON(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/subscriptions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// 201 + 字段名与 View 的 Subscription 类型逐字对齐（含 PriceCredits / WillAmortize / BlockReasons）。
func TestHandler_HandleCreate_201(t *testing.T) {
	h, _ := newTestHandler(&fakeStore{providers: []string{"acme"}}, &fakeConfig{r: 10000}, &fakeAmortizer{})
	rec := postJSON(t, h.HandleCreate, `{"Provider":"acme","Account":"acct-1","Price":100,"Currency":"CNY",
		"Cycle":"month","PayDay":5,"ShareCost":true,"CycleStart":"2026-11-01","Note":"x"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d，body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code int
		Data struct {
			ID           string
			Provider     string
			Account      string
			Price        float64
			PriceCredits *float64
			Status       string
			WillAmortize bool
			BlockReasons []string
			Warnings     []map[string]any
			NextRunHint  string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	d := body.Data
	if d.ID != "9001" || d.Status != StatusActive || !d.WillAmortize {
		t.Fatalf("响应 = %+v", d)
	}
	if d.PriceCredits == nil || *d.PriceCredits != 10000 {
		t.Fatalf("PriceCredits = %v", d.PriceCredits)
	}
	if d.BlockReasons == nil {
		t.Fatal("BlockReasons 必须是空数组（不是 null）")
	}
	if d.NextRunHint == "" {
		t.Error("缺少 NextRunHint（标准 §5.3 要求回显下一轮提示）")
	}
}

// 400：Reason + Data.Field 让前端定位到具体输入框。
func TestHandler_HandleCreate_InvalidField(t *testing.T) {
	h, _ := newTestHandler(&fakeStore{providers: []string{"acme"}}, &fakeConfig{r: 10000}, &fakeAmortizer{})
	rec := postJSON(t, h.HandleCreate, `{"Provider":"acme","Account":"a","Price":100,"PayDay":29}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d，body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Reason string
		Data   struct{ Field string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if body.Reason != ReasonInvalidField || body.Data.Field != "PayDay" {
		t.Fatalf("Reason/Field = %s/%s", body.Reason, body.Data.Field)
	}
}

// 409：duplicate_subscription + ExistingID。
func TestHandler_HandleCreate_Duplicate(t *testing.T) {
	st := &fakeStore{providers: []string{"acme"}, createErr: ErrDuplicate,
		existing: &Row{ID: 777, Provider: "acme", Account: "acct-1"}}
	h, _ := newTestHandler(st, &fakeConfig{r: 10000}, &fakeAmortizer{})
	rec := postJSON(t, h.HandleCreate, `{"Provider":"acme","Account":"acct-1","Price":100,"CycleStart":"2026-11-01"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d，body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Reason string
		Data   struct{ ExistingID string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if body.Reason != ReasonDuplicate || body.Data.ExistingID != "777" {
		t.Fatalf("Reason/ExistingID = %s/%s", body.Reason, body.Data.ExistingID)
	}
}

// S6 限流：10 秒内第二次 429 + Retry-After；过了窗口放行；定时轮不受影响（这里验的是入口）。
func TestHandler_HandleAmortizeRun_RateLimited(t *testing.T) {
	am := &fakeAmortizer{res: amortize.Result{Subscriptions: 1, Updated: 2, Allocated: 100}}
	h, au := newTestHandler(&fakeStore{providers: []string{"acme"}}, &fakeConfig{r: 10000}, am)

	base := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return base }

	rec := httptest.NewRecorder()
	h.HandleAmortizeRun(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/subscriptions/amortize/run", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("首次 status = %d，body=%s", rec.Code, rec.Body.String())
	}
	if len(au.entries) != 1 || au.entries[0].Action != "subscription.amortize.run" {
		t.Fatalf("手工重算未写审计：%+v", au.entries)
	}

	// 立刻再来一次 → 429，且必须给出 Retry-After（页面能倒计时，而不是让运营瞎点）。
	rec = httptest.NewRecorder()
	h.HandleAmortizeRun(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/subscriptions/amortize/run", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第二次 status = %d，期望 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 缺少 Retry-After")
	}
	if am.calls != 1 {
		t.Fatalf("被限流时不许真的跑分摊，calls = %d", am.calls)
	}

	// 10 秒后放行。
	h.now = func() time.Time { return base.Add(amortizeMinInterval) }
	rec = httptest.NewRecorder()
	h.HandleAmortizeRun(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/subscriptions/amortize/run", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("窗口过后 status = %d", rec.Code)
	}
	if am.calls != 2 {
		t.Fatalf("calls = %d，期望 2", am.calls)
	}
}

// 分摊失败：500 + 明确原因（已写入的部分由下一轮 SET 重算收敛）。
func TestHandler_HandleAmortizeRun_Error(t *testing.T) {
	am := &fakeAmortizer{err: errors.New("db down")}
	h, _ := newTestHandler(&fakeStore{providers: []string{"acme"}}, &fakeConfig{r: 10000}, am)

	rec := httptest.NewRecorder()
	h.HandleAmortizeRun(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/subscriptions/amortize/run", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d，body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "下轮会重算收敛") {
		t.Errorf("文案应说明幂等收敛：%s", rec.Body.String())
	}
}

// S1/S7 的 HTTP 层：query 参数要能透到服务层，响应字段名与 View 的 SubscriptionView /
// SubscriptionFormOptions 逐字对齐（Items/Total/Providers/Accounts/CnyRate/R）。
func TestHandler_ListAndFormOptions(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	st := &fakeStore{
		providers: []string{"acme"},
		accounts:  map[string]struct{}{"acct-1": {}},
		list: []ListRow{
			{Row: Row{ID: 1, Provider: "acme", Account: "acct-1", PriceCredits: 10000, Currency: "CNY",
				Cycle: CycleMonth, PayDay: 5, ShareCost: true, CycleStart: &start,
				CreatedAt: start, UpdatedAt: start},
				Allocated: 2500, HasCycle: true},
			{Row: Row{ID: 2, Provider: "acme", Account: "acct-2", PriceCredits: 10000, Currency: "CNY",
				Cycle: CycleMonth, PayDay: 5, ShareCost: false, CycleStart: &start,
				CreatedAt: start, UpdatedAt: start},
				HasCycle: true},
		},
	}
	h, _ := newTestHandler(st, &fakeConfig{r: 10000, cny: 6.8}, &fakeAmortizer{})

	rec := httptest.NewRecorder()
	h.HandleList(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/subscriptions?Status=active&Size=10", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d，body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data struct {
			Items []struct {
				ID             string
				Status         string
				CycleAllocated *float64
			}
			Total, Page, Size int
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if list.Data.Total != 1 || len(list.Data.Items) != 1 || list.Data.Items[0].Status != StatusActive {
		t.Fatalf("Status=active 过滤失效：%+v", list.Data)
	}
	if list.Data.Items[0].CycleAllocated == nil || *list.Data.Items[0].CycleAllocated != 2500 {
		t.Fatalf("CycleAllocated = %v", list.Data.Items[0].CycleAllocated)
	}
	if list.Data.Size != 10 {
		t.Fatalf("Size 未透传：%d", list.Data.Size)
	}

	rec = httptest.NewRecorder()
	h.HandleFormOptions(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/subscriptions/form-options", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d，body=%s", rec.Code, rec.Body.String())
	}
	var opts struct {
		Data struct {
			Providers []string
			Accounts  []string
			CnyRate   *float64
			R         *int64
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &opts); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if len(opts.Data.Providers) != 1 || len(opts.Data.Accounts) != 1 {
		t.Fatalf("候选 = %+v", opts.Data)
	}
	if opts.Data.R == nil || *opts.Data.R != 10000 || opts.Data.CnyRate == nil || *opts.Data.CnyRate != 6.8 {
		t.Fatalf("R/CnyRate = %v/%v", opts.Data.R, opts.Data.CnyRate)
	}
}
