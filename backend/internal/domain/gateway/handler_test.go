package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// ===== fakes =====

type fakeToken struct {
	tok *identity.Token
	err error
}

func (f *fakeToken) LookupByPlain(_ context.Context, _ string) (*identity.Token, error) {
	return f.tok, f.err
}

type fakeRouter struct {
	result  *router.RouteResult
	routes  []*router.ChannelRoute
	err     error
	spec    router.RouteSpec
	sess    *router.SessionRegistry
	sessNow func() time.Time // 会话注册表时钟（与网关共用 mock clock）
}

func (f *fakeRouter) Route(_ context.Context, spec router.RouteSpec) (*router.RouteResult, error) {
	f.spec = spec
	return f.result, f.err
}

func (f *fakeRouter) RouteAll(_ context.Context, spec router.RouteSpec) ([]*router.ChannelRoute, error) {
	f.spec = spec
	if f.err != nil {
		return nil, f.err
	}
	if f.routes != nil {
		return f.routes, nil
	}
	if f.result == nil {
		return nil, nil
	}
	return []*router.ChannelRoute{{
		ChannelID:       f.result.ChannelID,
		ChannelKeyID:    f.result.ChannelKeyID,
		InternalModelID: f.result.InternalModelID,
		ModelRowID:      f.result.ModelRowID,
		ExternalModelID: f.result.ExternalModelID,
		Revision:        f.result.Revision,
	}}, nil
}

func (f *fakeRouter) Sessions() *router.SessionRegistry {
	if f.sess == nil {
		f.sess = router.NewSessionRegistry(f.sessNow)
	}
	return f.sess
}

type feedEntry struct {
	keyID      int64
	modelRowID int64
	fb         channel.Feedback
}

type fakeChannels struct {
	rt                *channel.RuntimeChannel
	mrt               *channel.ModelRuntime
	keys              map[int64]*channel.KeyRuntime
	feeds             []feedEntry
	maxSessionsPerKey int // 覆盖默认 16（测试会话并发上限用）
}

func (f *fakeChannels) GetRuntime(_ int64) (*channel.RuntimeChannel, bool) {
	if f.rt == nil {
		return nil, false
	}
	return f.rt, true
}

func (f *fakeChannels) GetKeyRuntime(keyID int64) (*channel.KeyRuntime, bool) {
	kr, ok := f.keys[keyID]
	return kr, ok
}

func (f *fakeChannels) GetModelRuntime(channelID, modelRowID int64) (*channel.ModelRuntime, bool) {
	if f.rt == nil || f.mrt == nil {
		return nil, false
	}
	return f.mrt, true
}

func (f *fakeChannels) FeedResult(keyID int64, modelRowID int64, fb channel.Feedback) {
	f.feeds = append(f.feeds, feedEntry{keyID: keyID, modelRowID: modelRowID, fb: fb})
}

func (f *fakeChannels) KeyMaxSessions(int64) int {
	if f.maxSessionsPerKey > 0 {
		return f.maxSessionsPerKey
	}
	return 16
}

func (f *fakeChannels) SessionTTL(int64) int { return 60 }

// fakeBilling 模拟真实 billing.Record：已预扣时对同一钱包做差额结算（Settle），
// 无预扣时全额实扣（Consume）。credit 与网关预扣共用，便于验证最终净额。
type fakeBilling struct {
	reqs    []billing.RecordReq
	bids    []string        // 每次 Record 返回的 BillingID（call_log 关联断言用）
	ctxErrs []error         // 每次 Record 时的 ctx.Err()，用于验证记账 ctx 是否脱离请求取消
	err     error           // Record（结算）时返回；EstimateCredits 默认不受影响
	estErr  error           // EstimateCredits（预扣估算）时返回
	rec     *billing.Record // 可选：Record 返回值（缺省 CreditsConsumed=0）
	credit  *fakeCredit     // 可选：模拟真实 Record→钱包扣减（净额验证用）
}

func (f *fakeBilling) Record(ctx context.Context, req billing.RecordReq) (*billing.Record, error) {
	f.reqs = append(f.reqs, req)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	if f.err != nil {
		return nil, f.err
	}
	var rec *billing.Record
	if f.rec != nil {
		rec = f.rec
	} else {
		rec = &billing.Record{CreditsConsumed: 0}
	}
	rec.BillingID = fmt.Sprintf("bill-%06d", len(f.reqs))
	f.bids = append(f.bids, rec.BillingID)
	if f.credit != nil {
		if req.PreConsumed > 0 {
			delta := rec.CreditsConsumed - req.PreConsumed
			f.credit.balance -= delta
			f.credit.settled = append(f.credit.settled, delta)
		} else {
			f.credit.balance -= rec.CreditsConsumed
		}
	}
	return rec, nil
}

// EstimateCredits 与 Record 同口径的预扣估算（fake：系数取 1，仅验证「输入代入公式」的预扣语义）。
func (f *fakeBilling) EstimateCredits(_ context.Context, req billing.RecordReq) (float64, error) {
	if f.estErr != nil {
		return 0, f.estErr
	}
	return billing.ComputeCredits(req.Tokens, req.Rates, 1, 1, 10000)
}

// EstimateBreakdown 在 EstimateCredits 基础上额外返回本次估算所用系数（fake：时段/分档取 1，R=10000）。
func (f *fakeBilling) EstimateBreakdown(_ context.Context, req billing.RecordReq) (float64, float64, float64, int64, error) {
	if f.estErr != nil {
		return 0, 0, 0, 0, f.estErr
	}
	credits, err := billing.ComputeCredits(req.Tokens, req.Rates, 1, 1, 10000)
	return credits, 1, 1, 10000, err
}

// fakeCredit 实现 gateway.CreditOps（预扣/差额结算/退款），用于三阶段计费测试。
type fakeCredit struct {
	balance  float64
	consumed []float64
	refunded []float64
	settled  []float64
}

func (f *fakeCredit) Balance(_ context.Context, _ int64) (float64, error) { return f.balance, nil }

func (f *fakeCredit) PreConsume(_ context.Context, _ int64, amount float64, _, _, _ string) error {
	if f.balance < amount {
		return &identity.APIError{HTTPStatus: http.StatusPaymentRequired, Code: resp.CodeInsufficient, Message: "积分余额不足"}
	}
	f.balance -= amount
	f.consumed = append(f.consumed, amount)
	return nil
}

func (f *fakeCredit) Settle(_ context.Context, _ int64, delta float64, _, _ string) error {
	f.balance -= delta
	f.settled = append(f.settled, delta)
	return nil
}

func (f *fakeCredit) Refund(_ context.Context, _ int64, amount float64, _ string) error {
	f.balance += amount
	f.refunded = append(f.refunded, amount)
	return nil
}

type fakeModel struct {
	mapping *model.ExternalModel
	err     error
}

func (f *fakeModel) GetByExternalName(_ context.Context, _ string) (*model.ExternalModel, error) {
	return f.mapping, f.err
}

type fakeChannelModel struct {
	cm  *channel.ChannelModel
	err error
}

func (f *fakeChannelModel) GetByChannelAndInternal(_ context.Context, _ int64, _ string) (*channel.ChannelModel, error) {
	return f.cm, f.err
}

// ===== harness =====

var testExternalModel = &model.ExternalModel{
	ExternalName: "gpt-4",
	Enabled:      true,
	SaleRates:    billing.Rates{"input": 1.0, "output": 2.0, "cache_read": 0.25, "cache_write": 1.5, "reasoning": 4.0},
}

var testChannelModel = &channel.ChannelModel{
	ChannelID:       1,
	InternalModelID: "gpt-4-internal",
	ExternalModelID: 1,
	CostRates:       billing.Rates{"input": 0.5, "output": 1.0, "cache_read": 0.1, "cache_write": 0.8, "reasoning": 2.0},
}

type harness struct {
	gw      *Gateway
	chans   *fakeChannels
	billing *fakeBilling
	rrouter *fakeRouter
	tok     *fakeToken
	models  *fakeModel
	cmclass *fakeChannelModel
	clogs   *fakeCallLogs
	clock   *testClock
}

// fakeCallLogs 内存调用日志存储（测试断言采集内容）。
type fakeCallLogs struct {
	inserted []*CallLog
}

func (f *fakeCallLogs) Insert(_ context.Context, c *CallLog) error {
	cp := *c
	f.inserted = append(f.inserted, &cp)
	return nil
}

// testClock 可控时钟：网关与会话注册表共用同一指针，保证会话 TTL/过期判定确定可测。
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

func (c *testClock) add(d time.Duration) { c.t = c.t.Add(d) }

func build(t *testing.T, upstreamURL string) *harness {
	t.Helper()
	clock := &testClock{t: time.Now()}
	tok := &fakeToken{tok: &identity.Token{ID: 42, UserID: 7, Status: identity.StatusTokenActive}}
	rr := &fakeRouter{result: &router.RouteResult{ChannelID: 1, ChannelKeyID: 1, ModelRowID: 7, InternalModelID: "gpt-4-internal", ExternalModelID: 1}, sessNow: clock.now}
	mf := &fakeModel{mapping: testExternalModel}
	cmf := &fakeChannelModel{cm: testChannelModel}
	bf := &fakeBilling{}
	rt := &channel.RuntimeChannel{
		Channel: channel.Channel{
			ID:        1,
			Name:      "ch",
			Protocol:  channel.ProtocolOpenAICompat,
			BaseURL:   upstreamURL,
			RateLimit: channel.DefaultRateLimit(),
		},
		Machine: channel.NewMachine(channel.DefaultMachineConfig()),
		Limiter: channel.NewLimiter(1000, 1000000, 1.2),
	}
	cl := channel.NewLimiter(1000, 1000000, 1.2)
	mrt := &channel.ModelRuntime{
		Model:   channel.ChannelModel{ID: 7, ChannelID: 1, InternalModelID: "gpt-4-internal"},
		Machine: channel.NewMachine(channel.DefaultMachineConfig()),
		Limiter: cl,
	}
	// 默认单候选（ChannelKeyID=1）：密钥运行时携带明文凭据，供热路径转发。
	kr := &channel.KeyRuntime{
		Key:             channel.ChannelKey{ID: 1, ChannelID: 1, Name: "k1", State: channel.StateNormal},
		Channel:         &channel.Channel{ID: 1, Protocol: channel.ProtocolOpenAICompat, BaseURL: upstreamURL},
		CredentialPlain: "test-key",
		Machine:         channel.NewMachine(channel.DefaultMachineConfig()),
		Limiter:         channel.NewLimiter(1000, 1000000, 1.2),
	}
	cf := &fakeChannels{rt: rt, mrt: mrt, keys: map[int64]*channel.KeyRuntime{1: kr}}
	gw := NewGateway(GatewayConfig{
		Tokens:        tok,
		Router:        rr,
		Channels:      cf,
		Billing:       bf,
		Models:        mf,
		ChannelModels: cmf,
		TagKV: func(_ context.Context, _ *int64) (map[string]string, error) {
			return nil, nil
		},
		Pricing: func(_ context.Context, _ int64) (string, error) {
			return identity.PricingModeSale, nil
		},
		Now:    clock.now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	clog := &fakeCallLogs{}
	gw.SetCallLogs(clog)
	return &harness{gw: gw, chans: cf, billing: bf, rrouter: rr, tok: tok, models: mf, cmclass: cmf, clogs: clog, clock: clock}
}

// doRequest 向网关发送一次 OpenAI-compat 请求。
func doRequest(t *testing.T, gw *Gateway, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestH(t, gw, path, token, body, nil)
}

// doRequestH 与 doRequest 相同，额外注入自定义请求头（如 x-session-id）。
func doRequestH(t *testing.T, gw *Gateway, path, token, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)
	return rec
}

// decodeOpenAIError 解析 /v1 的 OpenAI 标准错误体 {"error":{"message","type","code"}}。
func decodeOpenAIError(t *testing.T, rec *httptest.ResponseRecorder) openAIErrorBody {
	t.Helper()
	var rb openAIErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &rb); err != nil {
		t.Fatalf("decode resp body: %v (body=%s)", err, rec.Body.String())
	}
	return rb
}

// assertChannelOpaque 校验响应头不含任何渠道信息。
func assertChannelOpaque(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for k := range rec.Header() {
		if strings.HasPrefix(strings.ToLower(k), "x-channel") {
			t.Errorf("channel header leaked: %s", k)
		}
	}
}

// ===== scenarios =====

// 场景 1：成功非流式；mock 验证 model 已被改写、认证头注入；计费被调用且数额正确。
func TestServeNonStreamSuccess(t *testing.T) {
	var upstreamModel, upstreamAuth string
	var capturedBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		upstreamModel, _ = capturedBody["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "ignored-upstream-x")
		w.Header().Set("X-Channel-Secret", "must-not-leak")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1234,"completion_tokens":567}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-testtoken", `{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if upstreamModel != "gpt-4-internal" {
		t.Errorf("model not rewritten to internal id: got %q", upstreamModel)
	}
	if upstreamAuth != "Bearer test-key" {
		t.Errorf("auth not injected: got %q", upstreamAuth)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"usage"`)) {
		t.Errorf("downstream body missing usage: %s", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("content-type not preserved: %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("request-id") != "" {
		t.Errorf("upstream misc header (request-id) not stripped")
	}
	assertChannelOpaque(t, rec)

	// 计费：唯一一次调用，参数正确，credits 按 rates input=1/out=2, R=10000, time=1, ctx=1 => 0.2368。
	if len(h.billing.reqs) != 1 {
		t.Fatalf("billing called %d times, want 1", len(h.billing.reqs))
	}
	req := h.billing.reqs[0]
	if req.ExternalModel != "gpt-4" || req.InternalModelID != "gpt-4-internal" ||
		req.ChannelKeyID != 1 || req.UserID != 7 || req.PricingMode != "sale" {
		t.Errorf("billing params wrong: %+v", req)
	}
	if req.TokenID == nil || *req.TokenID != 42 {
		t.Errorf("billing token id wrong: %v", req.TokenID)
	}
	if req.Tokens.Input != 1234 || req.Tokens.Output != 567 {
		t.Errorf("billing usage wrong: %+v", req.Tokens)
	}
	credits, _ := billing.ComputeCredits(req.Tokens, req.Rates, 1, 1, 10000)
	if credits != 0.2368 {
		t.Errorf("credits want 0.2368, got %v (usage=%+v rates=%v)", credits, req.Tokens, req.Rates)
	}
}

// 场景 2：模型未配置 → 404 40401，且留一条 failed 账单（每次请求必有一条记录）。
func TestServeModelNotFound(t *testing.T) {
	h := build(t, "http://unused")
	h.rrouter.result = nil
	h.rrouter.err = &router.RouteError{Kind: router.KindModelNotFound, HTTPStatus: http.StatusNotFound, Code: resp.CodeNotFound, Message: "model not found"}
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-invalid", `{"model":"ghost","stream":false}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	if rb := decodeOpenAIError(t, rec); rb.Error.Code != "model_not_found" || rb.Error.Type != "invalid_request_error" {
		t.Errorf("error want code=model_not_found type=invalid_request_error, got %+v", rb.Error)
	}
	assertChannelOpaque(t, rec)
	if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail || h.billing.reqs[0].ExternalModel != "ghost" {
		t.Fatalf("model-not-found must write 1 failed billing, got %+v", h.billing.reqs)
	}
}

// 场景 3：余额不足（fake billing 返回 40201）→ 402，且不返成功内容。
func TestServeInsufficient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choice":"ok","usage":{"prompt_tokens":100,"completion_tokens":100}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.billing.err = &identity.APIError{Code: resp.CodeInsufficient, Message: "余额不足"}
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("want 402, got %d: %s", rec.Code, rec.Body.String())
	}
	if rb := decodeOpenAIError(t, rec); rb.Error.Code != "insufficient_quota" || rb.Error.Type != "insufficient_quota" {
		t.Errorf("error want code/type=insufficient_quota, got %+v", rb.Error)
	}
	if strings.Contains(rec.Body.String(), `"choice"`) {
		t.Errorf("upstream success content leaked on 402: %s", rec.Body.String())
	}
	assertChannelOpaque(t, rec)
}

// 场景 4：上游 429 → 下游 429 42901，Machine.Feed(Is429) 被调用。
func TestServeUpstream429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", rec.Code)
	}
	if rb := decodeOpenAIError(t, rec); rb.Error.Code != "rate_limit_exceeded" || rb.Error.Type != "rate_limit_error" {
		t.Errorf("error want code=rate_limit_exceeded type=rate_limit_error, got %+v", rb.Error)
	}
	if len(h.chans.feeds) == 0 || !h.chans.feeds[0].fb.Is429 {
		t.Errorf("expected Is429 feedback, got %+v", h.chans.feeds)
	}
	assertChannelOpaque(t, rec)
}

// 场景 5：无 Bearer → 401 40101。
func TestServeUnauthorized(t *testing.T) {
	h := build(t, "http://unused")
	rec := doRequest(t, h.gw, "/v1/chat/completions", "", `{"model":"gpt-4"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if rb := decodeOpenAIError(t, rec); rb.Error.Code != "invalid_api_key" || rb.Error.Type != "invalid_request_error" {
		t.Errorf("error want code=invalid_api_key type=invalid_request_error, got %+v", rb.Error)
	}
}

// 场景 6：标签无匹配渠道 → 503 50301，且留一条 failed 账单（每次请求必有一条记录）。
func TestServeNoRoute(t *testing.T) {
	h := build(t, "http://unused")
	h.rrouter.result = nil
	h.rrouter.err = &router.RouteError{Kind: router.KindNoRoute, HTTPStatus: http.StatusServiceUnavailable, Code: resp.CodeNoRoute, Message: "no route"}
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	if rb := decodeOpenAIError(t, rec); rb.Error.Code != "service_unavailable" || rb.Error.Type != "server_error" {
		t.Errorf("error want code=service_unavailable type=server_error, got %+v", rb.Error)
	}
	if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
		t.Fatalf("no-route must write 1 failed billing, got %+v", h.billing.reqs)
	}
}

// 场景 7：流式透传；usage 计费存在；无渠道头。
func TestServeStream(t *testing.T) {
	streamBody := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50}}\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Channel-Switch", "must-not-leak")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(streamBody))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":true}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Errorf("not event-stream: %q", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Errorf("stream not fully proxied: %s", rec.Body.String())
	}
	if len(h.billing.reqs) != 1 {
		t.Fatalf("billing called %d times, want 1", len(h.billing.reqs))
	}
	if u := h.billing.reqs[0].Tokens; u.Input != 100 || u.Output != 50 {
		t.Errorf("stream usage wrong: %+v", u)
	}
	assertChannelOpaque(t, rec)
}

// 场景 8：流式无边 result 头透传（顺带覆盖 stream_options/include_usage 之前的普通 data）。
func TestServeStreamNoUsageEstimatesCompletion(t *testing.T) {
	streamBody := "data: {\"choices\":[{\"delta\":{\"content\":\"hello world\"}}]}\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(streamBody))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if len(h.billing.reqs) != 1 {
		t.Fatalf("billing called %d, want 1", len(h.billing.reqs))
	}
	// "hello world" = 11 chars * 0.25 = 2.75 → 2。
	if out := h.billing.reqs[0].Tokens.Output; out != 2 {
		t.Errorf("estimated output want 2, got %d", out)
	}
}

// 场景 8：GET /v1/models 返回启用模型列表（OpenAI 兼容格式）；未认证 → 401。
func TestServeModels(t *testing.T) {
	h := build(t, "http://unused")
	h.gw.listEnabled = func(_ context.Context) ([]model.ExternalModel, error) {
		return []model.ExternalModel{
			{ID: 1, ExternalName: "gpt-4", Enabled: true, CreatedAt: time.Unix(1700000000, 0)},
			{ID: 2, ExternalName: "disabled-model", Enabled: false, CreatedAt: time.Unix(1700000001, 0)},
			{ID: 3, ExternalName: "claude-3", Enabled: true, CreatedAt: time.Unix(1700000002, 0)},
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-gw-t")
	rec := httptest.NewRecorder()
	h.gw.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode models resp: %v", err)
	}
	if body.Object != "list" || len(body.Data) != 2 {
		t.Fatalf("want object=list with 2 enabled models, got %s", rec.Body.String())
	}
	if body.Data[0].ID != "gpt-4" || body.Data[1].ID != "claude-3" {
		t.Errorf("enabled model ids wrong: %+v", body.Data)
	}

	// 未带 Bearer → 401。
	badReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	badRec := httptest.NewRecorder()
	h.gw.Handler().ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusUnauthorized {
		t.Errorf("want 401 without token, got %d", badRec.Code)
	}
}

// 场景 B5-1：双密钥候选，keyA 已 DISABLED → 循环跳过 keyA，转发使用 keyB 明文凭据。
func TestServeDisabledKeyFailover(t *testing.T) {
	var upstreamAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	// D3：入口统一读 Machine.State()（kr.Key.State 为落库快照可能滞后）——401 自动禁用后立即生效。
	// 此处直接把 keyA 状态机切到 DISABLED 模拟自动禁用，网关应跳过 keyA 转 keyB。
	h.chans.keys[1].Machine.ForceDisable(h.clock.t)
	h.chans.keys[2] = &channel.KeyRuntime{
		Key:             channel.ChannelKey{ID: 2, ChannelID: 1, Name: "k2", State: channel.StateNormal},
		Channel:         &channel.Channel{ID: 1, Protocol: channel.ProtocolOpenAICompat, BaseURL: upstream.URL},
		CredentialPlain: "key-b-secret",
		Machine:         channel.NewMachine(channel.DefaultMachineConfig()),
		Limiter:         channel.NewLimiter(1000, 1000000, 1.2),
	}
	h.rrouter.routes = []*router.ChannelRoute{
		{ChannelID: 1, ChannelKeyID: 1, ModelRowID: 7, InternalModelID: "gpt-4-internal", ExternalModelID: 1},
		{ChannelID: 1, ChannelKeyID: 2, ModelRowID: 7, InternalModelID: "gpt-4-internal", ExternalModelID: 1},
	}

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if upstreamAuth != "Bearer key-b-secret" {
		t.Errorf("forward should use keyB plain credential, got %q", upstreamAuth)
	}
	// 账单维度同样落在密钥 2：成功记录 ChannelKeyID 与所选候选一致。
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Fail {
		t.Fatalf("want 1 completed billing on key 2, got %+v", h.billing.reqs)
	}
	if h.billing.reqs[0].ChannelKeyID != 2 {
		t.Errorf("billing channel_key_id want 2, got %d", h.billing.reqs[0].ChannelKeyID)
	}
}

// 场景 B5-3：密钥 A 并发会话满（max_concurrent 已达）→ 新会话 failover 到密钥 B（仍有槽），
// 转发与账单落在密钥 B；不会因密钥 A 满而直接归为限流类 429。
func TestServeSessionFull_FailoverToNextKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.chans.maxSessionsPerKey = 1
	h.chans.keys[2] = &channel.KeyRuntime{
		Key:             channel.ChannelKey{ID: 2, ChannelID: 1, Name: "k2", State: channel.StateNormal},
		Channel:         &channel.Channel{ID: 1, Protocol: channel.ProtocolOpenAICompat, BaseURL: upstream.URL},
		CredentialPlain: "key-b-secret",
		Machine:         channel.NewMachine(channel.DefaultMachineConfig()),
		Limiter:         channel.NewLimiter(1000, 1000000, 1.2),
	}
	h.rrouter.routes = []*router.ChannelRoute{
		{ChannelID: 1, ChannelKeyID: 1, ModelRowID: 7, InternalModelID: "gpt-4-internal", ExternalModelID: 1},
		{ChannelID: 1, ChannelKeyID: 2, ModelRowID: 7, InternalModelID: "gpt-4-internal", ExternalModelID: 1},
	}
	// 预占密钥 A 的唯一插槽（cap=1 → 1/1 满）。
	h.rrouter.Sessions().Attach(&router.Session{
		SessionID: "occupied", ChannelKeyID: 1, InternalModelID: "gpt-4-internal",
		ExpireAt: time.Now().Add(time.Hour),
	}, 1)

	rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`,
		map[string]string{"x-session-id": "new-sess"})
	if rec.Code != http.StatusOK {
		t.Fatalf("会话满应 failover 到 keyB，want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Fail || h.billing.reqs[0].ChannelKeyID != 2 {
		t.Fatalf("账单应落在 key 2（completed），got %+v", h.billing.reqs)
	}
}

// 场景 B5-4：全部候选并发会话满 → 终结 503「无可用渠道」（不再错误归为限流类 429）。
func TestServeSessionFull_AllCandidatesFull_Returns503(t *testing.T) {
	h := build(t, "http://unused")
	h.chans.maxSessionsPerKey = 1
	h.chans.keys[2] = &channel.KeyRuntime{
		Key:             channel.ChannelKey{ID: 2, ChannelID: 1, Name: "k2", State: channel.StateNormal},
		Channel:         &channel.Channel{ID: 1, Protocol: channel.ProtocolOpenAICompat, BaseURL: "http://unused"},
		CredentialPlain: "key-b-secret",
		Machine:         channel.NewMachine(channel.DefaultMachineConfig()),
		Limiter:         channel.NewLimiter(1000, 1000000, 1.2),
	}
	h.rrouter.routes = []*router.ChannelRoute{
		{ChannelID: 1, ChannelKeyID: 1, ModelRowID: 7, InternalModelID: "gpt-4-internal", ExternalModelID: 1},
		{ChannelID: 1, ChannelKeyID: 2, ModelRowID: 7, InternalModelID: "gpt-4-internal", ExternalModelID: 1},
	}
	// 两个密钥插槽各自占满。
	for _, keyID := range []int64{1, 2} {
		h.rrouter.Sessions().Attach(&router.Session{
			SessionID:       fmt.Sprintf("occ-%d", keyID),
			ChannelKeyID:    keyID,
			InternalModelID: "gpt-4-internal",
			ExpireAt:        time.Now().Add(time.Hour),
		}, 1)
	}

	rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`,
		map[string]string{"x-session-id": "new-sess"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("全部候选会话满应 503，got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail ||
		!strings.Contains(h.billing.reqs[0].ErrorMessage, "无可用渠道") {
		t.Fatalf("应落一条 failed 账单（无可用渠道），got %+v", h.billing.reqs)
	}
}

// 场景 B5-2：密钥凭据不可用（CredentialPlain==""）→ 按密钥粒度回喂失败 + recordFailure + continue。
func TestServeKeyCredentialEmptyRecordsFailure(t *testing.T) {
	h := build(t, "http://unused")
	h.chans.keys[1].CredentialPlain = ""

	rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`,
		map[string]string{"x-session-id": "abc"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.chans.feeds) != 1 || h.chans.feeds[0].keyID != 1 || h.chans.feeds[0].fb.IsSuccess {
		t.Errorf("want key 1 failure feedback, got %+v", h.chans.feeds)
	}
	if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
		t.Fatalf("want 1 failure billing, got %+v", h.billing.reqs)
	}
	fail := h.billing.reqs[0]
	if fail.ChannelKeyID != 1 || fail.ErrorMessage != "密钥凭据不可用" {
		t.Errorf("failure billing wrong: %+v", fail)
	}
	if want := router.SessionID(7, 42, "gpt-4", "abc"); fail.SessionID != want {
		t.Errorf("failure billing session_id want %q, got %q", want, fail.SessionID)
	}
}

// 场景 B5-3：上游 401 → 先转移到下一候选（无更多候选 → 终结 503）；
// FeedResult 以密钥 ID 维度回喂（IsAuthFailure）且账单失败记录挂 keyID。
func TestServeUpstream401FeedsKeyChannelKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.chans.keys[2] = &channel.KeyRuntime{
		Key:             channel.ChannelKey{ID: 2, ChannelID: 1, Name: "k2", State: channel.StateNormal},
		Channel:         &channel.Channel{ID: 1, Protocol: channel.ProtocolOpenAICompat, BaseURL: upstream.URL},
		CredentialPlain: "k2-secret",
		Machine:         channel.NewMachine(channel.DefaultMachineConfig()),
		Limiter:         channel.NewLimiter(1000, 1000000, 1.2),
	}
	h.rrouter.result = &router.RouteResult{ChannelID: 1, ChannelKeyID: 2, ModelRowID: 7, InternalModelID: "gpt-4-internal", ExternalModelID: 1}

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	// 401 属可转移失败：先转移下一候选；无更多候选 → 终结 503「无可用渠道」。
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rec.Code, rec.Body.String())
	}
	found := false
	for _, fe := range h.chans.feeds {
		if fe.keyID == 2 && fe.fb.IsAuthFailure {
			found = true
		}
	}
	if !found {
		t.Errorf("want key 2 IsAuthFailure feedback, got %+v", h.chans.feeds)
	}
	if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail || h.billing.reqs[0].ChannelKeyID != 2 {
		t.Errorf("want failure billing on key 2, got %+v", h.billing.reqs)
	}
}

// 场景 B5-4：成功计费记录携带 ChannelKeyID 与 SessionID（x-session-id 时）。
func TestServeBillingKeyAndSession(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`,
		map[string]string{"x-session-id": "abc"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.billing.reqs) != 1 {
		t.Fatalf("billing called %d times, want 1", len(h.billing.reqs))
	}
	req := h.billing.reqs[0]
	if req.ChannelKeyID != 1 {
		t.Errorf("billing channel_key_id want 1, got %d", req.ChannelKeyID)
	}
	if want := router.SessionID(7, 42, "gpt-4", "abc"); req.SessionID != want {
		t.Errorf("billing session_id want %q, got %q", want, req.SessionID)
	}
}

// 场景 B5-5：会话 attach 绑定 ChannelKeyID；再次请求续期（Renew）延长 ExpireAt。
func TestServeSessionAttachKeyAndRenew(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	sid := router.SessionID(7, 42, "gpt-4", "abc")
	body := `{"model":"gpt-4","stream":false}`

	rec1 := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", body, map[string]string{"x-session-id": "abc"})
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request want 200, got %d: %s", rec1.Code, rec1.Body.String())
	}
	s1, ok := h.rrouter.sess.Lookup(sid)
	if !ok {
		t.Fatalf("session not attached after first request")
	}
	if s1.ChannelKeyID != 1 || s1.InternalModelID != "gpt-4-internal" {
		t.Errorf("session bound to wrong key/model: key=%d model=%s", s1.ChannelKeyID, s1.InternalModelID)
	}
	// Lookup 返回注册表内同一指针，Renew 原地续期；先快照再发第二次请求。
	exp1 := s1.ExpireAt
	last1 := s1.LastActive

	h.clock.add(5 * time.Minute)
	rec2 := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", body, map[string]string{"x-session-id": "abc"})
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request want 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	s2, ok := h.rrouter.sess.Lookup(sid)
	if !ok {
		t.Fatalf("session lost after renew")
	}
	if s2.ChannelKeyID != 1 {
		t.Errorf("session key drifted after renew: %d", s2.ChannelKeyID)
	}
	if d := s2.ExpireAt.Sub(exp1); d < 4*time.Minute || d > 6*time.Minute {
		t.Errorf("renew should extend expiry by ~5m, got %v", d)
	}
	if !s2.LastActive.After(last1) {
		t.Errorf("renew did not advance last_active")
	}
}

// 场景 F1：余额前置检查——余额 <= 0 时流式/非流式均直接 402，且不发起任何上游调用、不写计费。
func TestServeZeroBalanceRejected_BothModes(t *testing.T) {
	var called int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.gw.balance = func(_ context.Context, _ int64) (float64, error) { return 0, nil }

	for _, tc := range []struct {
		name string
		body string
	}{
		{"non-stream", `{"model":"gpt-4","stream":false}`},
		{"stream", `{"model":"gpt-4","stream":true}`},
	} {
		rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", tc.body)
		if rec.Code != http.StatusPaymentRequired {
			t.Fatalf("[%s] want 402, got %d: %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	if n := atomic.LoadInt32(&called); n != 0 {
		t.Fatalf("zero balance must not reach upstream, got %d upstream calls", n)
	}
	// 零余额拒绝也要留 failed 账单（每次请求必有一条记录），虽未进入渠道。
	if len(h.billing.reqs) != 2 {
		t.Fatalf("zero balance must write 1 failed billing per request, got %d: %+v", len(h.billing.reqs), h.billing.reqs)
	}
	for _, req := range h.billing.reqs {
		if !req.Fail || !strings.Contains(req.ErrorMessage, "账户余额为 0") {
			t.Fatalf("zero-balance billing want failed record, got %+v", req)
		}
	}
}

// 场景 F3：预扣阶段余额不足（余额>0 但低于预扣估算额）→ 402，不发起上游调用（流式/非流式一致）。
// 预扣 = 估算输入(≈7t)×输入单价(2.75)/R ≈ 0.0019；余额低于该值即拦。
func TestServeBudgetInsufficient_SkipsUpstream(t *testing.T) {
	var called int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.gw.credit = &fakeCredit{balance: 0.0005}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"non-stream", `{"model":"gpt-4","stream":false,"max_tokens":4096,"messages":[{"role":"user","content":"hello"}]}`},
		{"stream", `{"model":"gpt-4","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"hello"}]}`},
	} {
		rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", tc.body)
		if rec.Code != http.StatusPaymentRequired {
			t.Fatalf("[%s] want 402, got %d: %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	if n := atomic.LoadInt32(&called); n != 0 {
		t.Fatalf("budget-insufficient must not reach upstream, got %d upstream calls", n)
	}
	// 预扣拒绝也要落 failed 账单（含公式与预扣积分），供用户核对与对账。
	if len(h.billing.reqs) != 2 {
		t.Fatalf("budget-insufficient must write 1 failed billing per request, got %d: %+v", len(h.billing.reqs), h.billing.reqs)
	}
	for _, req := range h.billing.reqs {
		if !req.Fail {
			t.Fatalf("预扣拒绝的账单应为 failed，got %+v", req)
		}
		if req.ErrorMessage == "" || !strings.Contains(req.ErrorMessage, "公式") {
			t.Fatalf("failed 账单应含公式与预扣积分，got %q", req.ErrorMessage)
		}
		// 公式应为与正常结算一致的三步式多行格式（Token×单价 → 系数调整 → 积分换算）。
		if !strings.Contains(req.ErrorMessage, "\n") || !strings.Contains(req.ErrorMessage, "积分换算") ||
			!strings.Contains(req.ErrorMessage, "Token × 单价") {
			t.Fatalf("failed 账单公式应为多行三步式，got %q", req.ErrorMessage)
		}
	}
}

// 场景 F4：三阶段计费结算——预扣冻结 + 实际消费差额结算（多退少补）。
// 已预扣时不做全额退款，只增减「实际消费 - 预扣」差额；净扣 = 实际消费。
func TestServePreConsume_SettlesDiff(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	fc := &fakeCredit{balance: 5000}
	h.gw.credit = fc
	// 模拟真实装配：billing.Record 按实际消费 0.2368，与预扣共用同一钱包。
	h.billing.rec = &billing.Record{CreditsConsumed: 0.2368}
	h.billing.credit = fc

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"max_tokens":2000,"messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(fc.consumed) != 1 {
		t.Fatalf("want 1 pre-consume, got %d", len(fc.consumed))
	}
	pre := fc.consumed[0]
	if pre <= 0 {
		t.Fatalf("pre-consume must be positive, got %v", pre)
	}
	// 已预扣 → 只做一次差额结算（多退少补），不应产生全额退款。
	if len(fc.refunded) != 0 {
		t.Fatalf("settle must not full-refund, got refunds %v", fc.refunded)
	}
	if len(fc.settled) != 1 {
		t.Fatalf("want 1 settle-diff, got %v", fc.settled)
	}
	if math.Abs(fc.settled[0]-(0.2368-pre)) > 0.0001 {
		t.Fatalf("settle delta must be actual-preconsumed, got %v want %v", fc.settled[0], 0.2368-pre)
	}
	// 净扣 = 实际消费（预扣 + 差额相抵）。
	wantBalance := 5000 - 0.2368
	if math.Abs(fc.balance-wantBalance) > 0.0001 {
		t.Fatalf("balance want ~%.4f, got %.4f (net must be actual charge)", wantBalance, fc.balance)
	}
}

// 场景 F5：上游 500 属可转移失败（上游未完成生成）；全部候选失败后全额退还预扣。
func TestServePreConsume_RefundsOnUpstreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	fc := &fakeCredit{balance: 5000}
	h.gw.credit = fc

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"max_tokens":2000,"messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(fc.consumed) != 1 || len(fc.refunded) != 1 {
		t.Fatalf("want pre-consume+full refund, consumed=%v refunded=%v", fc.consumed, fc.refunded)
	}
	if math.Abs(fc.consumed[0]-fc.refunded[0]) > 0.0001 {
		t.Fatalf("failure must refund full pre-consume, consumed=%v refunded=%v", fc.consumed, fc.refunded)
	}
}

// 记账脱离请求 ctx：客户端提前断开（请求 ctx 已取消）时，留痕记账仍必须完成。
func TestServeBillingSurvivesCanceledRequestCtx(t *testing.T) {
	h := build(t, "http://unused")
	h.rrouter.result = nil
	h.rrouter.err = &router.RouteError{Kind: router.KindModelNotFound, HTTPStatus: http.StatusNotFound,
		Code: resp.CodeNotFound, Message: "model not found"}

	// 模拟客户端已断开：请求 ctx 立即处于已取消状态。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ghost","stream":false}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer sk-gw-invalid")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.gw.Handler().ServeHTTP(rec, req)

	if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
		t.Fatalf("应落一条 failed 账单, got %+v", h.billing.reqs)
	}
	if len(h.billing.ctxErrs) != 1 || h.billing.ctxErrs[0] != nil {
		t.Fatalf("记账 ctx 不应携带请求取消信号, got %+v", h.billing.ctxErrs)
	}
}

// ===== 失败转移（failover）规则 =====

// 同渠道的第二个密钥（与 keys[1] 共用渠道 BaseURL，靠 Authorization 区分上游响应）。
func addSecondKey(h *harness, upstreamURL, cred string) {
	h.chans.keys[2] = &channel.KeyRuntime{
		Key:             channel.ChannelKey{ID: 2, ChannelID: 1, Name: "k2", State: channel.StateNormal},
		Channel:         &channel.Channel{ID: 1, Protocol: channel.ProtocolOpenAICompat, BaseURL: upstreamURL},
		CredentialPlain: cred,
		Machine:         channel.NewMachine(channel.DefaultMachineConfig()),
		Limiter:         channel.NewLimiter(1000, 1000000, 1.2),
	}
	h.rrouter.routes = []*router.ChannelRoute{
		{ChannelID: 1, ChannelKeyID: 1, ModelRowID: 7, InternalModelID: "gpt-4-internal", ExternalModelID: 1},
		{ChannelID: 1, ChannelKeyID: 2, ModelRowID: 7, InternalModelID: "gpt-4-internal", ExternalModelID: 1},
	}
}

const testOKBody = `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`

// 上游 402（供应商欠费）属可转移失败 → 自动转移到下一候选并成功；
// 一次请求落 1 条 failed（候选1）+ 1 条 completed（候选2）账单。
func TestServeFailover_Upstream402ThenSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer k1-secret" {
			w.WriteHeader(http.StatusPaymentRequired) // 候选 1：供应商欠费
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(testOKBody))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.chans.keys[1].CredentialPlain = "k1-secret"
	addSecondKey(h, upstream.URL, "k2-secret")

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("402 后应转移到下一候选并成功, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.billing.reqs) != 2 {
		t.Fatalf("want 2 次尝试账单, got %d: %+v", len(h.billing.reqs), h.billing.reqs)
	}
	if !h.billing.reqs[0].Fail || h.billing.reqs[0].ChannelKeyID != 1 {
		t.Errorf("候选 1 应落 failed 账单, got %+v", h.billing.reqs[0])
	}
	if h.billing.reqs[1].Fail || h.billing.reqs[1].ChannelKeyID != 2 {
		t.Errorf("候选 2 应落 completed 账单, got %+v", h.billing.reqs[1])
	}
}

// 上游 401（凭据失效）同样先转移：候选 1 失效 → 候选 2 成功。
func TestServeFailover_Upstream401ThenSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer k1-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(testOKBody))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.chans.keys[1].CredentialPlain = "k1-secret"
	addSecondKey(h, upstream.URL, "k2-secret")

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("401 后应转移到下一候选并成功, got %d: %s", rec.Code, rec.Body.String())
	}
	// 候选 1 的鉴权失败按密钥维度回喂，用于连续失败熔断计数。
	authFed := false
	for _, fe := range h.chans.feeds {
		if fe.keyID == 1 && fe.fb.IsAuthFailure {
			authFed = true
		}
	}
	if !authFed {
		t.Errorf("want key 1 IsAuthFailure feedback, got %+v", h.chans.feeds)
	}
}

// 流式同样按可转移规则转移：候选 1 返回 503（上游未完成生成）→ 候选 2 输出 SSE。
func TestServeFailover_Stream503ThenSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer k1-secret" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.chans.keys[1].CredentialPlain = "k1-secret"
	addSecondKey(h, upstream.URL, "k2-secret")

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("流式 503 后应转移到下一候选并成功, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("应透传 SSE 内容, got %q", rec.Body.String())
	}
}

// 上游 400（请求本身的问题）不可转移：即使还有候选也不再尝试，直接返回。
func TestServeNoFailover_Upstream400(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	addSecondKey(h, upstream.URL, "k2-secret")

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("400 不转移应直接 502, got %d: %s", rec.Code, rec.Body.String())
	}
	// 只应有候选 1 的一次失败尝试；若发生转移会多出候选 2 的账单。
	if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail || h.billing.reqs[0].ChannelKeyID != 1 {
		t.Fatalf("400 不应触发第二次上游调用, got %+v", h.billing.reqs)
	}
}

// 上游 504（网关类超时，源站可能已生成并计费）不可转移。
func TestServeNoFailover_Upstream504(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	addSecondKey(h, upstream.URL, "k2-secret")

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("504 不转移应直接 502, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.billing.reqs) != 1 {
		t.Fatalf("504 不应触发第二次上游调用, got %+v", h.billing.reqs)
	}
}

// 连接失败（请求未送达上游、不可能被计费）可安全转移；
// 此处单候选 → 转移到无候选后终结 503（若走「已送达」分支则会是 502）。
func TestServeFailover_ConnectionError(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // 关闭监听 → 连接被拒

	h := build(t, deadURL)
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("连接失败应可转移（单候选→503）, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
		t.Fatalf("want 1 failed 账单, got %+v", h.billing.reqs)
	}
}

// 场景 F6：上游成功但未返回 usage（usageZero）→ 仍按输入估算记账，保证每次调用都消耗积分。
func TestServeZeroUsageChargesEstimatedInput(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)) // 无 usage 块
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	fc := &fakeCredit{balance: 5000}
	h.gw.credit = fc

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.billing.reqs) != 1 {
		t.Fatalf("zero-usage must still bill once, got %d records", len(h.billing.reqs))
	}
	if h.billing.reqs[0].Fail || h.billing.reqs[0].Tokens.Input <= 0 {
		t.Fatalf("zero-usage billing want normal record with estimated input, got %+v", h.billing.reqs[0])
	}
}

// 场景 F7：计费失败（内容已响应但实际扣费出错）→ 预扣不退（按预扣计费），杜绝赊账。
func TestServeBillingFailureKeepsPreConsumed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	fc := &fakeCredit{balance: 5000}
	h.gw.credit = fc
	h.billing.err = errors.New("billing store down")

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(fc.consumed) != 1 {
		t.Fatalf("want 1 pre-consume, got %v", fc.consumed)
	}
	if len(fc.refunded) != 0 {
		t.Fatalf("billing failure must NOT refund pre-consume, got refunds %v", fc.refunded)
	}
}

// sessionNameFor 可读名称生成：首条 user 消息摘要 / 数组成员 / 回落 raw / 截断。
func TestSessionNameFor(t *testing.T) {
	long := strings.Repeat("A", 60)
	cases := []struct {
		raw  string
		body string
		want string
	}{
		{"", `{"messages":[{"role":"user","content":"你好世界"},{"role":"assistant","content":"hi"}]}`, "你好世界"},
		{"", `{"messages":[{"role":"user","content":[{"type":"text","text":"  hello   world "}]}]}`, "hello world"},
		{"xyz", `{"messages":[{"role":"system","content":"sys"}]}`, "xyz"},
		{"", `{"messages":[{"role":"user","content":"` + long + `"}]}`, strings.Repeat("A", 40) + "…"},
	}
	for i, tc := range cases {
		if got := sessionNameFor(tc.raw, []byte(tc.body)); got != tc.want {
			t.Errorf("case %d: want %q, got %q", i, tc.want, got)
		}
	}
}

// 场景 F2：无 x-session-id 时按请求前几条消息 hash 生成隐式会话——同一对话前缀映射同一会话，
// 会话可查询、账单携带 session_id；追加消息不改变前 3 条前缀时会话保持。
func TestServeImplicitSessionFromMessages(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	const body = `{"model":"gpt-4","stream":false,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}`
	seed := implicitSessionSeed([]byte(body))
	if seed == "" {
		t.Fatalf("implicit seed must be non-empty")
	}
	sid := router.SessionID(7, 42, "gpt-4", seed)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("first want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	s, ok := h.rrouter.sess.Lookup(sid)
	if !ok {
		t.Fatalf("implicit session not attached, sid=%s", sid)
	}
	if s.SessionRaw != seed {
		t.Errorf("session raw want %q, got %q", seed, s.SessionRaw)
	}
	if len(h.billing.reqs) != 1 {
		t.Fatalf("billing called %d times, want 1", len(h.billing.reqs))
	}
	if h.billing.reqs[0].SessionID != sid {
		t.Errorf("billing session_id want %q, got %q", sid, h.billing.reqs[0].SessionID)
	}

	// 同一对话前缀（新增第 4 条消息）：前 3 条不变 → 同一隐式会话，Attach 命中既有会话并续期。
	body2 := `{"model":"gpt-4","stream":false,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"next"}]}`
	rec2 := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", body2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second want 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if _, ok2 := h.rrouter.sess.Lookup(sid); !ok2 {
		t.Fatalf("implicit session should persist across same-prefix turns")
	}
}

// 场景 D3：限流拒绝不占会话名额——Attach 在两次 acquireLimit 之后才执行。
// 模型级限流 REJECT 时返回 429，且该会话不被创建；恢复限流后同一会话可正常创建（未被拒请求占用）。
func TestServeRateLimitedReject_DoesNotOccupySession(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	sid := router.SessionID(7, 42, "gpt-4", "abc")
	body := `{"model":"gpt-4","stream":false}`

	// 模型级限流降到最小且 REJECT：请求必被限流拒绝。
	h.chans.mrt.Model.RateLimit = channel.RateLimitConfig{OnExceed: channel.OnExceedReject}
	h.chans.mrt.Limiter = channel.NewLimiter(100, 1, 1.0)

	rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", body, map[string]string{"x-session-id": "abc"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("限流拒绝 want 429, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, found := h.rrouter.sess.Lookup(sid); found {
		t.Fatal("限流拒绝的请求不应创建会话（Attach 应在限流通过后进行）")
	}

	// 恢复限流：同一会话再次请求应成功创建（未被之前被拒请求占用名额）。
	h.chans.mrt.Model.RateLimit = channel.RateLimitConfig{}
	h.chans.mrt.Limiter = channel.NewLimiter(1000, 1000000, 1.2)
	h.chans.keys[1].Limiter = channel.NewLimiter(1000, 1000000, 1.2)

	rec2 := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", body, map[string]string{"x-session-id": "abc"})
	if rec2.Code != http.StatusOK {
		t.Fatalf("恢复限流后 want 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	s, found := h.rrouter.sess.Lookup(sid)
	if !found || s.ChannelKeyID != 1 {
		t.Fatalf("恢复限流后应创建会话并绑定密钥 1，found=%v s=%+v", found, s)
	}
}

// ===== 调用日志（call_logs）采集 =====

// 调用日志-1：非流式成功——采集 1 条，billing_id 与账单一致、RespKind=non_stream、RespBody 非空。
func TestServeCallLog_NonStreamSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.clogs.inserted) != 1 {
		t.Fatalf("call log inserted %d, want 1", len(h.clogs.inserted))
	}
	cl := h.clogs.inserted[0]
	if cl.BillingID != h.billing.bids[0] || cl.BillingID == "" {
		t.Errorf("call log billing_id want %q, got %q", h.billing.bids[0], cl.BillingID)
	}
	if cl.RespKind != "non_stream" || cl.Status != "completed" {
		t.Errorf("call log resp_kind/status want non_stream/completed, got %q/%q", cl.RespKind, cl.Status)
	}
	if !strings.Contains(cl.RespBody, `"content":"hi"`) || cl.RespBody == "" {
		t.Errorf("call log resp_body want upstream body, got %q", cl.RespBody)
	}
	if cl.Model != "gpt-4" {
		t.Errorf("call log model wrong: model=%q", cl.Model)
	}
}

// 调用日志-2：流式成功——RespKind=stream、RespBody 为拼接的 assistant content（增量拼接）。
func TestServeCallLog_StreamSuccess(t *testing.T) {
	streamBody := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}]}\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(streamBody))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.clogs.inserted) != 1 {
		t.Fatalf("call log inserted %d, want 1", len(h.clogs.inserted))
	}
	cl := h.clogs.inserted[0]
	if cl.RespKind != "stream" || cl.Status != "completed" {
		t.Errorf("call log resp_kind/status want stream/completed, got %q/%q", cl.RespKind, cl.Status)
	}
	// RespBody 为拼装的 assistant 消息 JSON，content 增量拼接完整。
	if !strings.Contains(cl.RespBody, `"content":"hi there"`) {
		t.Errorf("call log resp_body want assembled content \"hi there\", got %q", cl.RespBody)
	}
	if cl.BillingID != h.billing.bids[0] || cl.BillingID == "" {
		t.Errorf("call log billing_id want %q, got %q", h.billing.bids[0], cl.BillingID)
	}
}

// 调用日志-3：全部候选被拦截（无可用渠道 503）——插入 1 条 failed + error，
// Decision.attempts 包含候选渠道与跳过原因。
func TestServeCallLog_AllCandidatesRejected(t *testing.T) {
	h := build(t, "http://unused")
	// 密钥运行时直接置为禁用（状态机入口复查拦截）。
	h.chans.keys[1].Machine.ForceDisable(h.clock.t)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.clogs.inserted) != 1 {
		t.Fatalf("call log inserted %d, want 1", len(h.clogs.inserted))
	}
	cl := h.clogs.inserted[0]
	if cl.Status != "failed" || cl.RespKind != "error" {
		t.Errorf("call log want failed/error, got %q/%q", cl.Status, cl.RespKind)
	}
	if cl.BillingID != h.billing.bids[0] || cl.BillingID == "" {
		t.Errorf("reject call log billing_id want %q, got %q", h.billing.bids[0], cl.BillingID)
	}
	dec, ok := cl.Decision.(map[string]any)
	if !ok {
		t.Fatalf("reject call log decision missing: %v", cl.Decision)
	}
	raw, _ := json.Marshal(dec)
	var decision struct {
		Attempts []map[string]any `json:"attempts"`
		PreSum   float64          `json:"pre_consumed"`
		Result   string           `json:"result"`
	}
	if err := json.Unmarshal(raw, &decision); err != nil {
		t.Fatalf("unmarshal decision: %v", err)
	}
	if len(decision.Attempts) != 1 {
		t.Fatalf("decision attempts want 1, got %+v", decision.Attempts)
	}
	a0 := decision.Attempts[0]
	if a0["reason"] != "禁用" || a0["channel_id"] != "1" || a0["channel_key_id"] != "1" {
		t.Errorf("decision attempt want 禁用/1/1, got %+v", a0)
	}
	if decision.Result != "rejected" {
		t.Errorf("decision result want rejected, got %q", decision.Result)
	}
}

// 调用日志-4：请求增量 diff——同一会话两次请求（第二次多 1 条消息），第二次只记录增量新增条。
func TestServeCallLog_ReqIncrementDiff(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	hdr := map[string]string{"x-session-id": "diff-sess"}
	body1 := `{"model":"gpt-4","stream":false,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"a"}]}`
	body2 := `{"model":"gpt-4","stream":false,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"a"},{"role":"user","content":"b"}]}`

	rec1 := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", body1, hdr)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first want 200, got %d: %s", rec1.Code, rec1.Body.String())
	}
	rec2 := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", body2, hdr)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second want 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if len(h.clogs.inserted) != 2 {
		t.Fatalf("call logs inserted %d, want 2", len(h.clogs.inserted))
	}
	// 第一次无上次指纹：降级取最后一条 user（1 条）。
	first := h.clogs.inserted[0]
	firstMsgs, ok := first.ReqMessages.([]json.RawMessage)
	if !ok || len(firstMsgs) != 1 {
		t.Fatalf("first increment want 1 message, got %T len=%d (%v)", first.ReqMessages, len(firstMsgs), first.ReqMessages)
	}
	// 第二次与上次指纹对齐：仅返回新增的第 3 条。
	second := h.clogs.inserted[1]
	secondMsgs, ok := second.ReqMessages.([]json.RawMessage)
	if !ok || len(secondMsgs) != 1 {
		t.Fatalf("second increment want 1 message, got %T len=%d (%v)", second.ReqMessages, len(secondMsgs), second.ReqMessages)
	}
	if !strings.Contains(string(secondMsgs[0]), `"content":"b"`) {
		t.Errorf("second increment should only contain new message b, got %s", secondMsgs[0])
	}
	// 事件顺序：会话注册表内存里指纹已推进到第二次全量。
	sid := router.SessionID(7, 42, "gpt-4", "diff-sess")
	s, found := h.rrouter.sess.Lookup(sid)
	if !found || len(s.LastMsgFingerprints) != 3 {
		t.Fatalf("session fingerprints should be 3 after 2nd request, found=%v fps=%v", found, s.LastMsgFingerprints)
	}
	// 第三次（消息回退到 2 条）→ 与指纹对齐后 diff 为空（0 条增量）。
	rec3 := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", body1, hdr)
	if rec3.Code != http.StatusOK {
		t.Fatalf("third want 200, got %d: %s", rec3.Code, rec3.Body.String())
	}
	third := h.clogs.inserted[2]
	if thirdMsgs, ok := third.ReqMessages.([]json.RawMessage); !ok || len(thirdMsgs) != 0 {
		t.Fatalf("third increment want 0 messages (prefix aligned), got %T len=%d (%v)", third.ReqMessages, len(thirdMsgs), third.ReqMessages)
	}
}
