package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/domain/upstream"
)

// fakeRuntimeConfig 是 RuntimeConfigSource 的假实现（可编程开关与基址）。
type fakeRuntimeConfig struct {
	cfg      upstream.RuntimeConfig
	problems []string
}

func (f *fakeRuntimeConfig) Load(context.Context) (upstream.RuntimeConfig, []string) {
	return f.cfg, f.problems
}

// v2Harness 是 V2 转发主链的测试装配（只依赖 fake，无需 PG）。
type v2Harness struct {
	gw      *Gateway
	billing *fakeBilling
	credit  *fakeCredit
	model   *model.ExternalModel
	// upstream 记录上游实际收到的请求（断言「只换 model」与凭据注入）。
	mu        sync.Mutex
	gotPath   string
	gotQuery  string
	gotBody   []byte
	gotHeader http.Header
	respBody  []byte
	respCode  int
	respHead  http.Header
	upstream  *httptest.Server
}

// newV2Harness 建一套 V2 转发测试装配。
func newV2Harness(t *testing.T, cfg upstream.RuntimeConfig) *v2Harness {
	t.Helper()
	h := &v2Harness{
		billing: &fakeBilling{},
		credit:  &fakeCredit{balance: 100},
		model: &model.ExternalModel{
			ID: 1, ExternalName: "public-model", Enabled: true,
			BoundInternalRef: "vendor/up-model",
			SaleRates:        billing.Rates{"input": 1, "output": 2, "cache_read": 0.1, "cache_write": 0.3, "reasoning": 1},
		},
		respCode: http.StatusOK,
		respBody: []byte(`{"id":"x","usage":{"prompt_tokens":10,"completion_tokens":5},"choices":[]}`),
		respHead: http.Header{
			"Content-Type":      []string{"application/json"},
			"x-magpie-provider": []string{"vendor-x"},
			"x-magpie-account":  []string{"acct-9"},
			"x-magpie-model":    []string{"up-model"},
			"x-request-id":      []string{"upstream-rid"},
			"server":            []string{"magpie/1.0"},
		},
	}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.gotPath, h.gotQuery, h.gotBody = r.URL.Path, r.URL.RawQuery, body
		h.gotHeader = r.Header.Clone()
		code, respBody, respHead := h.respCode, h.respBody, h.respHead
		h.mu.Unlock()
		for k, vs := range respHead {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(code)
		_, _ = w.Write(respBody)
	}))
	t.Cleanup(h.upstream.Close)

	cfg.BaseURL = h.upstream.URL
	h.gw = NewGateway(GatewayConfig{
		Tokens:         &fakeToken{tok: &identity.Token{ID: 42, UserID: 7, Status: identity.StatusTokenActive}},
		Billing:        h.billing,
		Models:         &fakeModel{mapping: h.model},
		Pricing:        func(context.Context, int64) (string, error) { return identity.PricingModeSale, nil },
		Credit:         h.credit,
		UpstreamConfig: &fakeRuntimeConfig{cfg: cfg},
		UpstreamAuth:   func() (string, string) { return "Authorization", "Bearer up-secret" },
		Now:            func() time.Time { return time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC) },
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return h
}

// post 发一次请求（token 缺省用 Authorization: Bearer good-token）。
func (h *v2Harness) post(t *testing.T, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if _, ok := hdr["Authorization"]; !ok {
		req.Header.Set("Authorization", "Bearer good-token")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.gw.Handler().ServeHTTP(rec, req)
	return rec
}

// TestV2_HappyPath_ModelRewrittenAndBilled 覆盖主链正常路径：
// 只换 model、响应字节原样、预扣→结算、归因与 usage 落账单。
func TestV2_HappyPath_ModelRewrittenAndBilled(t *testing.T) {
	h := newV2Harness(t, upstream.DefaultRuntimeConfig())
	body := `{"model":"public-model","messages":[{"role":"user","content":"hi"}],"stream":false}`

	rec := h.post(t, "/v1/chat/completions", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，got %d body=%s", rec.Code, rec.Body.String())
	}
	// 1) 响应字节原样（D12）。
	if string(rec.Body.Bytes()) != string(h.respBody) {
		t.Fatalf("响应体应逐字节原样：got %s want %s", rec.Body.String(), h.respBody)
	}
	// 2) 上游只换 model（其余字节不动）。
	wantBody := strings.Replace(body, `"model":"public-model"`, `"model":"vendor/up-model"`, 1)
	if string(h.gotBody) != wantBody {
		t.Fatalf("上游请求体只该换 model：\n got %s\nwant %s", h.gotBody, wantBody)
	}
	// 3) 凭据注入 + 不透传下游凭据。
	if got := h.gotHeader.Get("Authorization"); got != "Bearer up-secret" {
		t.Fatalf("上游应收到注入的凭据，got %q", got)
	}
	// 4) 头口径：上游标识头不许漏给下游，x-request-id 是我们的。
	if rec.Header().Get("x-magpie-provider") != "" || rec.Header().Get("server") != "" {
		t.Fatalf("上游标识头应被剔除：%v", rec.Header())
	}
	// 5) 计费：预扣一次 + 一条账单（PreConsumed 传入 → 只结算差额）。
	if len(h.credit.consumed) != 1 {
		t.Fatalf("应预扣一次，got %d", len(h.credit.consumed))
	}
	if len(h.billing.reqs) != 1 {
		t.Fatalf("应落一条账单，got %d", len(h.billing.reqs))
	}
	req := h.billing.reqs[0]
	if req.Tokens.Input != 10 || req.Tokens.Output != 5 {
		t.Fatalf("账单 token 应来自上游 usage，got %+v", req.Tokens)
	}
	if req.PreConsumed != h.credit.consumed[0] {
		t.Fatalf("账单应带上已预扣金额：pre=%v consumed=%v", req.PreConsumed, h.credit.consumed[0])
	}
	if req.UpstreamProvider != "vendor-x" || req.UpstreamAccount != "acct-9" || req.UpstreamModel != "up-model" {
		t.Fatalf("归因应落账单：%+v", req)
	}
	if req.InternalModelID != "vendor/up-model" || req.ExternalModel != "public-model" {
		t.Fatalf("模型口径不符：%+v", req)
	}
	if req.Fail {
		t.Fatal("成功请求不该落 failed 账单")
	}
}

// TestV2_UpstreamError_RefundAndFailedRecord 覆盖上游 4xx/5xx：
// 响应字节原样透传、预扣退回、落 failed 账单。
func TestV2_UpstreamError_RefundAndFailedRecord(t *testing.T) {
	h := newV2Harness(t, upstream.DefaultRuntimeConfig())
	errBody := []byte(`{"error":{"message":"upstream boom","type":"invalid_request_error"}}`)
	h.respCode, h.respBody = http.StatusBadRequest, errBody

	rec := h.post(t, "/v1/chat/completions", `{"model":"public-model"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("上游状态码应原样：got %d", rec.Code)
	}
	if string(rec.Body.Bytes()) != string(errBody) {
		t.Fatalf("错误体必须逐字节原样：got %s", rec.Body.String())
	}
	if len(h.credit.refunded) != 1 || h.credit.refunded[0] != h.credit.consumed[0] {
		t.Fatalf("失败应全额退预扣：consumed=%v refunded=%v", h.credit.consumed, h.credit.refunded)
	}
	if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
		t.Fatalf("失败应落 failed 账单：%+v", h.billing.reqs)
	}
	if h.billing.reqs[0].Tokens.Input != 0 {
		t.Fatalf("failed 账单不该计 token：%+v", h.billing.reqs[0])
	}
}

// TestV2_DegradeAndRejections 覆盖四类拒绝：passthrough 关、未知路径、无令牌、模型未绑定。
func TestV2_DegradeAndRejections(t *testing.T) {
	t.Run("passthrough 关闭 → 503", func(t *testing.T) {
		cfg := upstream.DefaultRuntimeConfig()
		cfg.Degrade.PassthroughEnabled = false
		h := newV2Harness(t, cfg)
		if rec := h.post(t, "/v1/chat/completions", `{"model":"public-model"}`, nil); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("应 503，got %d", rec.Code)
		}
		if h.gotPath != "" {
			t.Fatal("降级关闭时不该调上游")
		}
	})
	t.Run("未知路径 → 404", func(t *testing.T) {
		h := newV2Harness(t, upstream.DefaultRuntimeConfig())
		if rec := h.post(t, "/v1/embeddings", `{"model":"public-model"}`, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("未识别端点应 404，got %d", rec.Code)
		}
	})
	t.Run("无令牌 → 401", func(t *testing.T) {
		h := newV2Harness(t, upstream.DefaultRuntimeConfig())
		rec := h.post(t, "/v1/chat/completions", `{"model":"public-model"}`, map[string]string{"Authorization": ""})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("应 401，got %d", rec.Code)
		}
	})
	t.Run("模型未绑定 → 404", func(t *testing.T) {
		h := newV2Harness(t, upstream.DefaultRuntimeConfig())
		h.model.BoundInternalRef = ""
		if rec := h.post(t, "/v1/chat/completions", `{"model":"public-model"}`, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("未绑定模型应 404，got %d", rec.Code)
		}
	})
	t.Run("预扣被拒 → 402 且不打上游", func(t *testing.T) {
		h := newV2Harness(t, upstream.DefaultRuntimeConfig())
		h.credit.preErr = &identity.APIError{HTTPStatus: http.StatusPaymentRequired, Message: "积分余额不足"}
		if rec := h.post(t, "/v1/chat/completions", `{"model":"public-model"}`, nil); rec.Code != http.StatusPaymentRequired {
			t.Fatalf("应 402，got %d", rec.Code)
		}
		if h.gotPath != "" {
			t.Fatal("预扣被拒时不该打上游")
		}
	})
}

// TestV2_GeminiPathModelAndQueryKey 覆盖 Gemini：model 在 path、`?key=` 必须剔除。
func TestV2_GeminiPathModelAndQueryKey(t *testing.T) {
	h := newV2Harness(t, upstream.DefaultRuntimeConfig())
	rec := h.post(t, "/v1beta/models/public-model:generateContent?alt=sse&key=tenant-token",
		`{"contents":[{"parts":[{"text":"hi"}]}]}`,
		map[string]string{"Authorization": ""})

	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d body=%s", rec.Code, rec.Body.String())
	}
	if h.gotPath != "/v1beta/models/vendor/up-model:generateContent" {
		t.Fatalf("Gemini 只该改 path 里的 model 段，got %q", h.gotPath)
	}
	if strings.Contains(h.gotQuery, "key=") {
		t.Fatalf("租户令牌不许带给上游：%q", h.gotQuery)
	}
	if h.gotQuery != "alt=sse" {
		t.Fatalf("其余 query 应逐字保留，got %q", h.gotQuery)
	}
}

// TestV2_NoUsageEstimatesAndMarks 覆盖「上游没给 usage」：按估算计费且账单可区分。
func TestV2_NoUsageEstimatesAndMarks(t *testing.T) {
	h := newV2Harness(t, upstream.DefaultRuntimeConfig())
	h.respBody = []byte(`{"id":"x","choices":[{"message":{"content":"hello world"}}]}`)

	rec := h.post(t, "/v1/chat/completions",
		`{"model":"public-model","messages":[{"role":"user","content":"please answer"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d", rec.Code)
	}
	if len(h.billing.reqs) != 1 {
		t.Fatalf("应落一条账单，got %d", len(h.billing.reqs))
	}
	req := h.billing.reqs[0]
	if req.Tokens.Input <= 0 || req.Tokens.Output <= 0 {
		t.Fatalf("无 usage 时应按估算计费：%+v", req.Tokens)
	}
	if req.CostSource != billing.CostSourceNone {
		t.Fatalf("估算单必须可区分（cost_source=none），got %q", req.CostSource)
	}
	if !strings.Contains(req.Remark, "估算") {
		t.Fatalf("估算单应在备注里点明，got %q", req.Remark)
	}
}

// TestV2_ModelsEndpointFiltersUnbound 覆盖 GET /v1/models：只列启用且已绑定。
func TestV2_ModelsEndpointFiltersUnbound(t *testing.T) {
	h := newV2Harness(t, upstream.DefaultRuntimeConfig())
	h.gw.listEnabled = func(context.Context) ([]model.ExternalModel, error) {
		return []model.ExternalModel{
			{ExternalName: "ok-model", Enabled: true, BoundInternalRef: "vendor/m"},
			{ExternalName: "unbound", Enabled: true},
			{ExternalName: "disabled", Enabled: false, BoundInternalRef: "vendor/m2"},
		}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.gw.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d", rec.Code)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析失败: %v body=%s", err, rec.Body.String())
	}
	if len(payload.Data) != 1 || payload.Data[0].ID != "ok-model" {
		t.Fatalf("只该列启用且已绑定的模型，got %+v", payload.Data)
	}
}
