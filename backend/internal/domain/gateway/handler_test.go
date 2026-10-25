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
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
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
	result   *router.RouteResult
	routes   []*router.ChannelRoute
	excluded []router.ExcludedCandidate
	err      error
	spec     router.RouteSpec
	sess     *router.SessionRegistry
	sessNow  func() time.Time // 会话注册表时钟（与网关共用 mock clock）
	// switchFail=true 时 ConfirmSessionSwitch 返回 false（模拟目标密钥并发已满导致迁移失败）。
	switchFail  bool
	switchCalls []switchCall // 记录 ConfirmSessionSwitch 的调用（断言迁移时机与 ttl 透传）
}

// switchCall 记录一次 ConfirmSessionSwitch 调用（F1 迁移时机 / F8 ttl 透传断言用）。
type switchCall struct {
	sessionID string
	keyID     int64
	turn      int
	ttl       time.Duration
}

func (f *fakeRouter) Route(_ context.Context, spec router.RouteSpec) (*router.RouteResult, error) {
	f.spec = spec
	return f.result, f.err
}

func (f *fakeRouter) RouteAll(_ context.Context, spec router.RouteSpec) ([]*router.ChannelRoute, error) {
	return f.routesOrErr()
}

// RouteAllWithExcluded 与 RouteAll 同源，额外携带被排除候选（决策轨迹测试用）。
func (f *fakeRouter) RouteAllWithExcluded(_ context.Context, spec router.RouteSpec) (*router.RouteAllResult, error) {
	f.spec = spec
	routes, err := f.routesOrErr()
	return &router.RouteAllResult{Routes: routes, Excluded: f.excluded}, err
}

func (f *fakeRouter) routesOrErr() ([]*router.ChannelRoute, error) {
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

// ConfirmSessionSwitch 与真实引擎同语义：把会话归属迁移到目标候选（并发上限按不限处理）。
// switchFail=true 时返回 false（模拟目标密钥并发已满导致迁移失败），供网关 F1 回退用例断言。
func (f *fakeRouter) ConfirmSessionSwitch(sessionID string, cr *router.ChannelRoute, turn int, ttl time.Duration) bool {
	if sessionID == "" || cr == nil || cr.Switch == nil {
		return false
	}
	f.switchCalls = append(f.switchCalls, switchCall{sessionID: sessionID, keyID: cr.ChannelKeyID, turn: turn, ttl: ttl})
	if f.switchFail {
		return false
	}
	return f.Sessions().Migrate(sessionID, cr.ChannelKeyID, cr.InternalModelID, 0, ttl)
}

type feedEntry struct {
	keyID      int64
	modelRowID int64
	fb         channel.Feedback
}

type fakeChannels struct {
	rt   *channel.RuntimeChannel
	mrt  *channel.ModelRuntime
	keys map[int64]*channel.KeyRuntime
	// mrtByChannel 可选：按渠道返回不同模型运行时（多候选直通/转换混合场景用）。
	mrtByChannel      map[int64]*channel.ModelRuntime
	feeds             []feedEntry
	maxSessionsPerKey int // 覆盖默认 16（测试会话并发上限用）

	// 协议级错误策略动作落点（drain/disable）：记录手动设置过的密钥/模型状态。
	keyActions  []string
	modelStates []channel.State
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
	if mr, ok := f.mrtByChannel[channelID]; ok {
		return mr, true
	}
	if f.rt == nil || f.mrt == nil {
		return nil, false
	}
	return f.mrt, true
}

func (f *fakeChannels) FeedResult(keyID int64, modelRowID int64, fb channel.Feedback) {
	f.feeds = append(f.feeds, feedEntry{keyID: keyID, modelRowID: modelRowID, fb: fb})
}

// FeedResultGated 与 fakeChannels.FeedResult 同语义，countIntoHealth=false 时不记录（不影响熔断）。
func (f *fakeChannels) FeedResultGated(keyID int64, modelRowID int64, fb channel.Feedback, countIntoHealth bool) {
	if !countIntoHealth {
		return
	}
	f.feeds = append(f.feeds, feedEntry{keyID: keyID, modelRowID: modelRowID, fb: fb})
}

// ForceKeyState 记录协议级错误策略触发的密钥动作（drain/disable）；持久化由真实 Manager 承担。
func (f *fakeChannels) ForceKeyState(_ context.Context, _ int64, action string) error {
	f.keyActions = append(f.keyActions, action)
	return nil
}

// ManualSetModelState 记录协议级错误策略触发的内部模型状态。
func (f *fakeChannels) ManualSetModelState(_, _ int64, st channel.State) error {
	f.modelStates = append(f.modelStates, st)
	return nil
}

func (f *fakeChannels) KeyMaxSessions(int64) int {
	if f.maxSessionsPerKey > 0 {
		return f.maxSessionsPerKey
	}
	return 16
}

// fakeSessionExpiry 可编程的会话过期 TTL fake（fn 为空时默认 60 分钟，保持既有测试时序余量）。
type fakeSessionExpiry struct {
	fn    func(ctxTokens, turns int) time.Duration
	calls int
}

func (f *fakeSessionExpiry) TTL(ctxTokens, turns int) time.Duration {
	f.calls++
	if f.fn != nil {
		return f.fn(ctxTokens, turns)
	}
	return 60 * time.Minute
}

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
	// 可选：强制预扣估算返回固定值（用于构造 pc.Credits==0 的端到端用例）。
	estOverride    bool
	estOverrideVal float64
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
	if f.estOverride {
		return f.estOverrideVal, nil
	}
	return billing.ComputeCredits(req.Tokens, req.Rates, 1, 1, 10000)
}

// EstimateBreakdown 在 EstimateCredits 基础上额外返回本次估算所用系数（fake：时段/分档取 1，R=10000）。
func (f *fakeBilling) EstimateBreakdown(_ context.Context, req billing.RecordReq) (float64, float64, float64, int64, error) {
	if f.estErr != nil {
		return 0, 0, 0, 0, f.estErr
	}
	if f.estOverride {
		return f.estOverrideVal, 1, 1, 10000, nil
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
	preErr   error // 预扣时强制返回的错误（用于钱包缺失 40401 等语义用例）
	preCalls int   // PreConsume 被调用的次数（含 amount==0 的零金额预扣，用于验证「照常下发」契约）
}

func (f *fakeCredit) Balance(_ context.Context, _ int64) (float64, error) { return f.balance, nil }

func (f *fakeCredit) PreConsume(_ context.Context, _ int64, amount float64, _, _, _ string) error {
	f.preCalls++
	if f.preErr != nil {
		return f.preErr
	}
	if amount <= 0 {
		// 零金额=零余额守卫（对齐真实条件写语义）：余额>0 放行且不写流水/不改余额，余额<=0 拒绝。
		if f.balance <= 0 {
			return &identity.APIError{HTTPStatus: http.StatusPaymentRequired, Code: resp.CodeInsufficient, Message: "积分余额不足"}
		}
		return nil
	}
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
	SaleRates:    billing.Rates{"input": 1.0, "output": 2.0, "cache_read": 0.25},
}

var testChannelModel = &channel.ChannelModel{
	ChannelID:       1,
	InternalModelID: "gpt-4-internal",
	ExternalModelID: 1,
	CostRates:       billing.Rates{"input": 0.5, "output": 1.0, "cache_read": 0.1},
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
	expiry  *fakeSessionExpiry
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
	exp := &fakeSessionExpiry{}
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
		SessionExpiry: exp,
		Now:           clock.now,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	clog := &fakeCallLogs{}
	gw.SetCallLogs(clog)
	return &harness{gw: gw, chans: cf, billing: bf, rrouter: rr, tok: tok, models: mf, cmclass: cmf, clogs: clog, clock: clock, expiry: exp}
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

// 场景 8（AC-17 放宽）：流式响应正常结束（[DONE]）但无 usage → 按可见内容估算计费（不退预扣）。
// 「正常结束无 usage」属上游忽略平台注入的 include_usage（协议偏差），不惩罚渠道。
func TestServeStreamDoneNoUsageEstimatesWithoutPunishingChannel(t *testing.T) {
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
		t.Fatalf("want 200 (stream already responded), got %d", rec.Code)
	}
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Fail {
		t.Fatalf("usage 缺失应按估算计费（成功账单）, got %+v", h.billing.reqs)
	}
	if u := h.billing.reqs[0].Tokens; u.Input <= 0 || u.Output <= 0 {
		t.Fatalf("usage 缺失应按可见内容估算 token, got %+v", u)
	}
	if len(h.chans.feeds) != 0 {
		t.Fatalf("正常结束无 usage 不应计入渠道健康失败, got %+v", h.chans.feeds)
	}
}

// 流式「正常结束但 usage 缺失」（收到 [DONE] 无 usage，内容完整）也应写入会话级推理缓存，
// 与「不惩罚渠道」的口径一致：该类上游忽略 include_usage，但推理内容已完整返回。
func TestServeStreamDoneNoUsageRecordsReasoningCache(t *testing.T) {
	streamBody := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"思考\"," +
		"\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(streamBody))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":true}`, map[string]string{"x-session-id": "sess-cache"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (stream already responded), got %d", rec.Code)
	}
	h.gw.reasoning.mu.Lock()
	n := len(h.gw.reasoning.entries)
	h.gw.reasoning.mu.Unlock()
	if n == 0 {
		t.Fatalf("正常结束无 usage 也应写入推理缓存（内容完整）")
	}
}

// 流式成功（返回 usage、内容完整）也应写入会话级推理缓存：这是缓存主写入路径（herr==nil 分支），
// 防止 normalEnd 条件被误改成仅覆盖 usage 缺失场景。
func TestServeStreamWithUsageRecordsReasoningCache(t *testing.T) {
	streamBody := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"思考\"," +
		"\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(streamBody))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":true}`, map[string]string{"x-session-id": "sess-cache-ok"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (stream already responded), got %d", rec.Code)
	}
	h.gw.reasoning.mu.Lock()
	n := len(h.gw.reasoning.entries)
	h.gw.reasoning.mu.Unlock()
	if n == 0 {
		t.Fatalf("流式成功（含 usage）也应写入推理缓存")
	}
}

// 转换流「正常结束但 usage 缺失」也应写入会话级推理缓存（anthropic 入站 → openai-chat 上游，
// 上游忽略 include_usage）：与直通路径同口径，防止转换路径永远不写缓存导致下一轮回填 miss。
func TestStreamConversionDoneNoUsageRecordsReasoningCache(t *testing.T) {
	streamBody := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"思考\"," +
		"\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(streamBody))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	// 模型仅可达 openai-chat → anthropic 入站需转换为 openai-chat 上游。
	h.chans.mrt.Model.Protocols = channelprotocol.Protocols{channelprotocol.ProtocolOpenAIChat: {}}

	rec := postWithHeaders(t, h.gw, "/v1/messages",
		`{"model":"gpt-4","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"ping"}]}`,
		map[string]string{"x-api-key": "sk-gw-t", "x-session-id": "sess-conv-cache"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	h.gw.reasoning.mu.Lock()
	n := len(h.gw.reasoning.entries)
	h.gw.reasoning.mu.Unlock()
	if n == 0 {
		t.Fatalf("转换流正常结束无 usage 也应写入推理缓存")
	}
}

// 上游流截断（未收到 [DONE] 即 EOF，内容可能半截）不得写入会话级推理缓存：
// 半截推理回传会造成上游校验失败，与「计入健康失败」的处置口径一致。
func TestServeStreamTruncatedDoesNotRecordReasoningCache(t *testing.T) {
	streamBody := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"半截\"," +
		"\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(streamBody))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":true}`, map[string]string{"x-session-id": "sess-trunc"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (stream already responded), got %d", rec.Code)
	}
	h.gw.reasoning.mu.Lock()
	n := len(h.gw.reasoning.entries)
	h.gw.reasoning.mu.Unlock()
	if n != 0 {
		t.Fatalf("流截断不应写入推理缓存, got %d 条", n)
	}
}

// 流式响应未收到结束标记即 EOF（上游流被截断）：仍按估算计费，但计入渠道健康失败（不再禁用整条渠道）。
func TestServeStreamTruncatedEstimatesAndFeedsHealthFailure(t *testing.T) {
	streamBody := "data: {\"choices\":[{\"delta\":{\"content\":\"hello world\"}}]}\n\n" // 无 [DONE]
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(streamBody))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (stream already responded), got %d", rec.Code)
	}
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Fail {
		t.Fatalf("上游流中断仍应按估算计费, got %+v", h.billing.reqs)
	}
	if u := h.billing.reqs[0].Tokens; u.Input <= 0 || u.Output <= 0 {
		t.Fatalf("上游流中断应按可见内容估算 token, got %+v", u)
	}
	if len(h.chans.feeds) != 1 || h.chans.feeds[0].fb.IsSuccess {
		t.Fatalf("上游流中断应计入渠道健康失败, got %+v", h.chans.feeds)
	}
}

// 非 openai-chat 协议：流正常结束（message_stop / response.completed）但无 usage → 估算计费、不惩罚渠道；
// 未收到结束标记即 EOF（截断）→ 仍估算计费，但计入渠道健康失败。
func TestServeStreamNonOpenAINoUsageEndShape(t *testing.T) {
	const anthropicStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n"
	const anthropicDelta = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0," +
		"\"delta\":{\"type\":\"text_delta\",\"text\":\"hello world\"}}\n\n"
	const anthropicStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	const responsesDelta = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello world\"}\n\n"
	const responsesDone = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"

	tests := []struct {
		name       string
		protocol   string
		body       string
		wantPunish bool
	}{
		{"anthropic 正常结束无 usage", channelprotocol.ProtocolAnthropicMessages, anthropicStart + anthropicDelta + anthropicStop, false},
		{"anthropic 截断", channelprotocol.ProtocolAnthropicMessages, anthropicStart + anthropicDelta, true},
		{"responses 正常结束无 usage", channelprotocol.ProtocolOpenAIResponses, responsesDelta + responsesDone, false},
		{"responses 截断", channelprotocol.ProtocolOpenAIResponses, responsesDelta, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer upstream.Close()

			h := build(t, upstream.URL)
			h.chans.rt.Channel.Protocol = tt.protocol
			path := "/v1/responses"
			hdr := map[string]string{"Authorization": "Bearer sk-gw-t"}
			if tt.protocol == channelprotocol.ProtocolAnthropicMessages {
				path, hdr = "/v1/messages", map[string]string{"x-api-key": "sk-gw-t"}
			}
			rec := postWithHeaders(t, h.gw, path,
				`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"ping"}]}`, hdr)
			if rec.Code != http.StatusOK {
				t.Fatalf("want 200 (stream already responded), got %d", rec.Code)
			}
			// usage 缺失一律按可见内容估算计费（不丢计费）。
			if len(h.billing.reqs) != 1 || h.billing.reqs[0].Fail {
				t.Fatalf("usage 缺失应按估算计费（成功账单）, got %+v", h.billing.reqs)
			}
			if u := h.billing.reqs[0].Tokens; u.Input <= 0 || u.Output <= 0 {
				t.Fatalf("usage 缺失应按可见内容估算 token, got %+v", u)
			}
			if tt.wantPunish {
				if len(h.chans.feeds) != 1 || h.chans.feeds[0].fb.IsSuccess {
					t.Fatalf("截断应计入渠道健康失败, got %+v", h.chans.feeds)
				}
			} else if len(h.chans.feeds) != 0 {
				t.Fatalf("正常结束无 usage 不应计入渠道健康失败, got %+v", h.chans.feeds)
			}
		})
	}
}

// responses 直通仅以 [DONE] 收尾（无 response.completed/usage）：按正常结束归因，
// 估算计费且不惩罚渠道（reason=done_no_usage）。
func TestServeResponsesDoneOnlyNoUsageNoPenalty(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.chans.rt.Channel.Protocol = channelprotocol.ProtocolOpenAIResponses
	rec := doRequest(t, h.gw, "/v1/responses", "sk-gw-t", `{"model":"gpt-4","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (stream already responded), got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.chans.feeds) != 0 {
		t.Fatalf("正常结束无 usage 不应计入渠道健康失败, got %+v", h.chans.feeds)
	}
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Fail {
		t.Fatalf("usage 缺失应按估算计费（成功账单）, got %+v", h.billing.reqs)
	}
	if len(h.clogs.inserted) == 0 {
		t.Fatalf("缺少调用日志")
	}
	attempts := decisionAttempts(t, h.clogs.inserted[len(h.clogs.inserted)-1].Decision)
	if len(attempts) == 0 || attempts[len(attempts)-1]["reason"] != "上游未返回usage(按估算计费)" {
		t.Fatalf("done_no_usage 应归因为正常结束无 usage, got %+v", attempts)
	}
}

// responses 直通先发 response.failed 再补 [DONE]：[DONE] 只能收口，不得把失败洗成正常结束；
// 仍归因 error_frame，计入渠道健康失败且调用日志 failed。
func TestServeResponsesDoneAfterFailedStaysErrorFrame(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":" +
			"{\"code\":\"server_error\",\"message\":\"boom\"}}}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.chans.rt.Channel.Protocol = channelprotocol.ProtocolOpenAIResponses
	rec := doRequest(t, h.gw, "/v1/responses", "sk-gw-t", `{"model":"gpt-4","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (stream already responded), got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.chans.feeds) != 1 || h.chans.feeds[0].fb.IsSuccess {
		t.Fatalf("error_frame 应计入渠道健康失败, got %+v", h.chans.feeds)
	}
	if len(h.clogs.inserted) == 0 {
		t.Fatalf("缺少调用日志")
	}
	last := h.clogs.inserted[len(h.clogs.inserted)-1]
	if last.Status != "failed" || last.RespKind != "stream" {
		t.Fatalf("error_frame 应记 failed/stream 调用日志, got status=%q resp_kind=%q", last.Status, last.RespKind)
	}
	if !strings.Contains(last.ErrorMessage, "上游流式响应中断") {
		t.Fatalf("调用日志应体现上游流式中断, got %q", last.ErrorMessage)
	}
	// usage 缺失仍按可见内容估算计费：账单不得是 failed。
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Fail {
		t.Fatalf("usage 缺失应按估算计费（成功账单）, got %+v", h.billing.reqs)
	}
}

// openai-chat 直通：空 error 负载（{"error":{}}）后补 [DONE] 仍须归因 error_frame 并惩罚渠道。
// 修复前：摘要为空不置 sawError，[DONE] 归因 done_no_usage → 不惩罚渠道（红）。
func TestServeOpenAIChatEmptyErrorFramePunished(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"error\":{}}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (stream already responded), got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.chans.feeds) != 1 || h.chans.feeds[0].fb.IsSuccess {
		t.Fatalf("error_frame 应计入渠道健康失败, got %+v", h.chans.feeds)
	}
	if len(h.clogs.inserted) == 0 {
		t.Fatalf("缺少调用日志")
	}
	last := h.clogs.inserted[len(h.clogs.inserted)-1]
	if last.Status != "failed" || last.RespKind != "stream" {
		t.Fatalf("error_frame 应记 failed/stream 调用日志, got status=%q resp_kind=%q", last.Status, last.RespKind)
	}
	if !strings.Contains(last.ErrorMessage, "上游流式响应中断") {
		t.Fatalf("调用日志应体现上游流式中断, got %q", last.ErrorMessage)
	}
	// usage 缺失仍按可见内容估算计费：账单不得是 failed。
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Fail {
		t.Fatalf("usage 缺失应按估算计费（成功账单）, got %+v", h.billing.reqs)
	}
}

// responses 直通：先出现 response.failed、随后又有 response.completed（无 usage）。
// completed 只是收口，不能把失败洗成正常结束。
// 修复前：sawDone 与 sawError 同时为真，归因取 done_no_usage → 不惩罚渠道（红）。
func TestServeResponsesFailedThenCompletedStaysErrorFrame(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":" +
			"{\"code\":\"server_error\",\"message\":\"boom\"}}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.chans.rt.Channel.Protocol = channelprotocol.ProtocolOpenAIResponses
	rec := doRequest(t, h.gw, "/v1/responses", "sk-gw-t", `{"model":"gpt-4","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (stream already responded), got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.chans.feeds) != 1 || h.chans.feeds[0].fb.IsSuccess {
		t.Fatalf("error_frame 应计入渠道健康失败, got %+v", h.chans.feeds)
	}
	if len(h.clogs.inserted) == 0 {
		t.Fatalf("缺少调用日志")
	}
	last := h.clogs.inserted[len(h.clogs.inserted)-1]
	if last.Status != "failed" || last.RespKind != "stream" {
		t.Fatalf("error_frame 应记 failed/stream 调用日志, got status=%q resp_kind=%q", last.Status, last.RespKind)
	}
	if !strings.Contains(last.ErrorMessage, "上游流式响应中断") {
		t.Fatalf("调用日志应体现上游流式中断, got %q", last.ErrorMessage)
	}
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Fail {
		t.Fatalf("usage 缺失应按估算计费（成功账单）, got %+v", h.billing.reqs)
	}
}

// anthropic 直通写失败 handler 级端到端：下游写失败不是上游故障，不得回喂渠道健康，
// 且失败文案须与上游读侧中断区分（含「下游写失败」）。
// 纯护栏：修复前该路径已依赖 wrapDownstreamWrite + isDownstreamWriteErr，预期为绿。
func TestServeAnthropicDownstreamWriteFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	// anthropic 直通：入站 /v1/messages 与上游协议一致，不走转换泵。
	h.chans.rt.Channel.Protocol = channelprotocol.ProtocolAnthropicMessages
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("x-api-key", "sk-gw-t")
	req.Header.Set("Content-Type", "application/json")
	h.gw.Handler().ServeHTTP(failWriteRecorder{httptest.NewRecorder()}, req)

	if len(h.chans.feeds) != 0 {
		t.Fatalf("下游写失败不得回喂渠道健康（无论成功或失败）, got %+v", h.chans.feeds)
	}
	if len(h.clogs.inserted) == 0 {
		t.Fatalf("缺少调用日志")
	}
	last := h.clogs.inserted[len(h.clogs.inserted)-1]
	if last.Status != "failed" || last.RespKind != "error" {
		t.Fatalf("下游写失败仍应保持 failed/error 调用日志, got status=%q resp_kind=%q", last.Status, last.RespKind)
	}
	if !strings.Contains(last.ErrorMessage, "下游写失败") {
		t.Fatalf("下游写失败应使用区分于上游中断的归因文案, got %q", last.ErrorMessage)
	}
}

// cancelOnEOFBody 读到 EOF 时取消请求 ctx：模拟「上游流刚读完、请求侧才取消」的竞态。
type cancelOnEOFBody struct {
	r      io.Reader
	cancel context.CancelFunc
}

func (b *cancelOnEOFBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) {
		b.cancel()
	}
	return n, err
}

func (b *cancelOnEOFBody) Close() error { return nil }

// roundTripFunc 让测试直接构造上游响应（无需真实网络），用于精确控制 body 读侧与 ctx 取消时序。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// clientGone × usageMissing 竞态：请求 ctx 在上游流读完后才取消（上游也未给 usage），
// 必须先判定客户端已离开——不得把「客户端断开」误归为上游 usage 缺失而惩罚渠道。
func TestServeStreamClientGoneUsageMissingNoHealthFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := build(t, "http://unused")
	h.gw.streamClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: &cancelOnEOFBody{
				r:      strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"),
				cancel: cancel,
			},
		}, nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"ping"}]}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer sk-gw-t")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.gw.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (stream already responded), got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.chans.feeds) != 0 {
		t.Fatalf("客户端已断开不应回喂渠道健康（无论成功或失败）, got %+v", h.chans.feeds)
	}
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Fail {
		t.Fatalf("客户端断开仍应按估算计费（成功账单）, got %+v", h.billing.reqs)
	}
}

// hasSession 不重排契约：既有会话命中时保持路由层候选顺序（即使「需转换在前、直通在后」），
// 决策轨迹不得出现 passthrough_first / passthrough_fallback（直通重排只用于新会话）。
func TestServeSessionHitKeepsCandidateOrder(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg1","type":"message","role":"assistant","model":"gpt-4-internal",` +
			`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	modelRuntime := func(rowID, channelID int64, protocols channelprotocol.Protocols) *channel.ModelRuntime {
		return &channel.ModelRuntime{
			Model:   channel.ChannelModel{ID: rowID, ChannelID: channelID, InternalModelID: "gpt-4-internal", Protocols: protocols},
			Machine: channel.NewMachine(channel.DefaultMachineConfig()),
			Limiter: channel.NewLimiter(1000, 1000000, 1.2),
		}
	}
	// 与 TestServeDecisionPassthroughReorderMarkers 同一夹具：候选 1 需转换（兜底）、候选 2 直通。
	convRoute := &router.ChannelRoute{ChannelID: 1, ChannelKeyID: 1, InternalModelID: "gpt-4-internal", ModelRowID: 7, ExternalModelID: 1}
	directRoute := &router.ChannelRoute{ChannelID: 2, ChannelKeyID: 2, InternalModelID: "gpt-4-internal", ModelRowID: 8, ExternalModelID: 1}

	h := build(t, upstream.URL)
	h.rrouter.routes = []*router.ChannelRoute{convRoute, directRoute}
	// 候选 2 的密钥不在运行时 → 被跳过，最终命中候选 1（两条轨迹都会落痕）。
	h.chans.mrtByChannel = map[int64]*channel.ModelRuntime{
		1: modelRuntime(7, 1, channelprotocol.Protocols{channelprotocol.ProtocolAnthropicMessages: {}}),
		2: modelRuntime(8, 2, channelprotocol.Protocols{channelprotocol.ProtocolOpenAIChat: {}}),
	}
	h.rrouter.Sessions().Attach(&router.Session{
		SessionID:       router.SessionID(7, 42, "gpt-4", "s-sticky"),
		ChannelKeyID:    1,
		InternalModelID: "gpt-4-internal",
		ExpireAt:        h.clock.now().Add(time.Hour),
	}, 16)

	rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`,
		map[string]string{"x-session-id": "s-sticky"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.clogs.inserted) == 0 {
		t.Fatalf("缺少调用日志")
	}
	attempts := decisionAttempts(t, h.clogs.inserted[len(h.clogs.inserted)-1].Decision)
	if len(attempts) == 0 || attempts[0]["channel_id"] != "1" {
		t.Fatalf("既有会话应保持路由层候选顺序（需转换在前，首个候选即命中）, got %+v", attempts)
	}
	for _, a := range attempts {
		if _, ok := a["passthrough_first"]; ok {
			t.Fatalf("既有会话命中不得出现 passthrough_first: %+v", attempts)
		}
		if _, ok := a["passthrough_fallback"]; ok {
			t.Fatalf("既有会话命中不得出现 passthrough_fallback: %+v", attempts)
		}
	}
}

// 下游写失败不是上游故障：保持 interrupted（failed 调用日志）语义，但不得回喂渠道健康失败
// （否则健康上游会被误判为故障、误杀熔断）。
func TestServeStreamDownstreamWriteDoesNotFeedHealthFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]," +
			"\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-gw-t")
	req.Header.Set("Content-Type", "application/json")
	h.gw.Handler().ServeHTTP(failWriteRecorder{httptest.NewRecorder()}, req)

	if len(h.chans.feeds) != 0 {
		t.Fatalf("下游写失败不得回喂渠道健康（无论成功或失败）, got %+v", h.chans.feeds)
	}
	if len(h.clogs.inserted) == 0 {
		t.Fatalf("缺少调用日志")
	}
	last := h.clogs.inserted[len(h.clogs.inserted)-1]
	if last.Status != "failed" || last.RespKind != "error" {
		t.Fatalf("下游写失败仍应保持 failed 调用日志语义, got status=%q resp_kind=%q", last.Status, last.RespKind)
	}
	if !strings.Contains(last.ErrorMessage, "下游写失败") {
		t.Fatalf("下游写失败应使用区分于上游中断的归因文案, got %q", last.ErrorMessage)
	}
}

// 平台必需参数：客户端未带 stream_options.include_usage 时由网关为 OpenAI 兼容流式请求补齐，
// 且保留客户端已有的其它 stream_options 键；非流式请求不改写。
func TestServeStreamInjectsIncludeUsage(t *testing.T) {
	var gotBodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBodies = append(gotBodies, string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":true,"stream_options":{"foo":"bar"},"messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(gotBodies) != 1 {
		t.Fatalf("上游应收到 1 次请求, got %d", len(gotBodies))
	}
	var body struct {
		StreamOptions map[string]any `json:"stream_options"`
	}
	if err := json.Unmarshal([]byte(gotBodies[0]), &body); err != nil {
		t.Fatalf("上游请求体非法 JSON: %v (%s)", err, gotBodies[0])
	}
	if body.StreamOptions["include_usage"] != true {
		t.Fatalf("应补齐 include_usage=true, got %s", gotBodies[0])
	}
	if body.StreamOptions["foo"] != "bar" {
		t.Fatalf("应保留已有 stream_options 键, got %s", gotBodies[0])
	}
}

// 非流式请求不应被写入 stream_options（客户端未请求 usage 时非流式响应本就应带 usage）。
func TestServeNonStreamDoesNotInjectIncludeUsage(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(gotBody, "stream_options") {
		t.Fatalf("非流式请求不应被写入 stream_options, got %s", gotBody)
	}
}

// legacy /v1/completions 未定义 stream_options，注入可能被上游按未知参数拒绝：不得改写。
func TestServeCompletionsStreamDoesNotInjectIncludeUsage(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"text\":\"hi\",\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"id\":\"x\",\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequest(t, h.gw, "/v1/completions", "sk-gw-t", `{"model":"gpt-4","prompt":"hello","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(gotBody, "stream_options") {
		t.Fatalf("legacy completions 不应被写入 stream_options, got %s", gotBody)
	}
}

// duplicateJSONKeys 逐 token 扫描 JSON，返回所有对象层级中重复出现的键。
// map 反序列化会静默保留最后一个同名键，无法发现重复；这里按 token 流校验，供改写结果断言使用。
func duplicateJSONKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	type frame struct {
		isObject bool
		keys     map[string]bool
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var stack []*frame
	var dups []string
	expectKey := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("扫描 JSON token 失败: %v (%s)", err, raw)
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				stack = append(stack, &frame{isObject: true, keys: map[string]bool{}})
				expectKey = true
			case '[':
				stack = append(stack, &frame{})
				expectKey = false
			case '}', ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				expectKey = len(stack) > 0 && stack[len(stack)-1].isObject
			}
		case string:
			if len(stack) == 0 {
				continue
			}
			top := stack[len(stack)-1]
			if top.isObject && expectKey {
				if top.keys[v] {
					dups = append(dups, v)
				}
				top.keys[v] = true
				expectKey = false
			} else {
				expectKey = top.isObject
			}
		default:
			if len(stack) > 0 && stack[len(stack)-1].isObject {
				expectKey = true
			}
		}
	}
	return dups
}

// ensureIncludeUsage 的重构契约：已是 true 时原样返回（仍解析校验顶层归属，但不做任何改写/重序列化）；
// include_usage=false 仍改写为 true；无 stream_options 时只插入新键、其余字节保持不变。
func TestEnsureIncludeUsage(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		protocol    string
		endpoint    string
		wantSame    bool
		wantInclude bool
		wantOptions map[string]any
	}{
		{
			name:     "已含 include_usage=true 原样返回",
			body:     `{"model":"m","stream":true,"stream_options":{"include_usage":true,"foo":"bar"}}`,
			protocol: "openai-chat", endpoint: EndpointChatCompletions,
			wantSame: true, wantInclude: true,
		},
		{
			name:     "键值间空白也识别为已开启",
			body:     `{"stream":true,"stream_options":{"include_usage" : true }}`,
			protocol: "openai-chat", endpoint: EndpointChatCompletions,
			wantSame: true, wantInclude: true,
		},
		{
			name:     "include_usage=false 改写为 true",
			body:     `{"stream":true,"stream_options":{"include_usage":false}}`,
			protocol: "openai-chat", endpoint: EndpointChatCompletions,
			wantInclude: true,
		},
		{
			name:     "无 stream_options 时插入新键并保留其余字节",
			body:     `{"model":"m","stream":true}`,
			protocol: "openai-chat", endpoint: EndpointChatCompletions,
			wantInclude: true,
		},
		{
			name:     "已有其它 stream_options 键保留",
			body:     `{"stream":true,"stream_options":{"foo":"bar"}}`,
			protocol: "openai-chat", endpoint: EndpointChatCompletions,
			wantInclude: true, wantOptions: map[string]any{"foo": "bar"},
		},
		{
			name:     "stream_options 为 null 时重建为单键",
			body:     `{"model":"m","stream":true,"stream_options":null}`,
			protocol: "openai-chat", endpoint: EndpointChatCompletions,
			wantInclude: true,
		},
		{
			name:     "stream_options 为空对象时注入",
			body:     `{"model":"m","stream":true,"stream_options":{}}`,
			protocol: "openai-chat", endpoint: EndpointChatCompletions,
			wantInclude: true,
		},
		{
			name: "嵌套对象里的 include_usage 不误判",
			// 反例：include_usage=true 不在 stream_options 内，必须注入（否则平台拿不到 usage）。
			body:     `{"stream":true,"stream_options":{},"extra":{"include_usage":true}}`,
			protocol: "openai-chat", endpoint: EndpointChatCompletions,
			wantInclude: true,
		},
		{
			name: "顶层 include_usage=false 不得被嵌套同名字面量误判",
			// stream_options 的直接子键才是有效配置；更深层的 include_usage=true 不能当作已请求 usage。
			body:     `{"stream":true,"stream_options":{"include_usage":false,"foo":{"include_usage":true}}}`,
			protocol: "openai-chat", endpoint: EndpointChatCompletions,
			wantInclude: true,
		},
		{
			name: "嵌套在其它键里的 stream_options 不算顶层配置",
			// extra.stream_options 不是顶层 stream_options：顶层必须补齐，否则上游不会返回 usage。
			body:     `{"stream":true,"extra":{"stream_options":{"include_usage":true}}}`,
			protocol: "openai-chat", endpoint: EndpointChatCompletions,
			wantInclude: true,
		},
		{
			name: "重复 stream_options 键改写后只剩单键",
			// 客户端自带重复键：map 反序列化取最后一个（false），改写后必须输出单个 stream_options。
			body:     `{"stream":true,"stream_options":{"include_usage":true},"stream_options":{"include_usage":false}}`,
			protocol: "openai-chat", endpoint: EndpointChatCompletions,
			wantInclude: true,
		},
		{
			name:     "messages 正文含同名字面量不误判",
			body:     `{"model":"m","stream":true,"messages":[{"role":"user","content":"please set stream_options and include_usage"}]}`,
			protocol: "openai-chat", endpoint: EndpointChatCompletions,
			wantInclude: true,
		},
		{
			name:     "非法 JSON 原样返回",
			body:     `{"stream":true,`,
			protocol: "openai-chat", endpoint: EndpointChatCompletions,
			wantSame: true,
		},
		{
			name:     "非流式不改写",
			body:     `{"stream":false}`,
			protocol: "openai-chat", endpoint: EndpointChatCompletions,
			wantSame: true,
		},
		{
			name:     "legacy completions 端点不改写",
			body:     `{"stream":true}`,
			protocol: "openai-chat", endpoint: EndpointCompletions,
			wantSame: true,
		},
		{
			name:     "非 openai-chat 协议不改写",
			body:     `{"stream":true}`,
			protocol: channelprotocol.ProtocolAnthropicMessages, endpoint: EndpointChatCompletions,
			wantSame: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ensureIncludeUsage([]byte(tt.body), tt.protocol, tt.endpoint)
			if tt.wantSame {
				if string(got) != tt.body {
					t.Fatalf("应原样返回（不得改写）, got %s", got)
				}
				return
			}
			var body struct {
				StreamOptions map[string]any `json:"stream_options"`
			}
			if err := json.Unmarshal(got, &body); err != nil {
				t.Fatalf("改写结果非法 JSON: %v (%s)", err, got)
			}
			if dups := duplicateJSONKeys(t, got); len(dups) > 0 {
				t.Fatalf("改写结果不得含重复键 %v: %s", dups, got)
			}
			if body.StreamOptions["include_usage"] != tt.wantInclude {
				t.Fatalf("include_usage want %v, got %s", tt.wantInclude, got)
			}
			for k, v := range tt.wantOptions {
				if body.StreamOptions[k] != v {
					t.Fatalf("stream_options[%s] want %v, got %s", k, v, got)
				}
			}
		})
	}
	// 无 stream_options 的注入路径：原有字段逐字节保留（仅末尾追加）。
	in := []byte(`{"model":"m","stream":true}`)
	out := ensureIncludeUsage(in, "openai-chat", EndpointChatCompletions)
	if !strings.HasPrefix(string(out), `{"model":"m","stream":true,`) {
		t.Fatalf("注入时不得改动既有字节, got %s", out)
	}
}

// hardErr（上游传输中断，如 unexpected EOF）已落 failed 账单与调用日志，必须回喂健康失败，
// 否则持续读失败的上游永远进不了错误率窗口、无法被 DRAIN。
func TestServeStreamBrokenBodyFeedsHealthFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "1000") // 声明长度远大于实际写入 → 客户端读到 unexpected EOF
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (stream already responded), got %d", rec.Code)
	}
	// hardErr 先落 failed 账单，随后仍会按零用量走一次结算记录（既有行为），故只断言首条为失败。
	if len(h.billing.reqs) == 0 || !h.billing.reqs[0].Fail {
		t.Fatalf("上游传输中断应落 failed 账单, got %+v", h.billing.reqs)
	}
	if len(h.chans.feeds) != 1 || h.chans.feeds[0].fb.IsSuccess {
		t.Fatalf("上游传输中断应计入渠道健康失败, got %+v", h.chans.feeds)
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

// 场景 8b：别名仅用于模型名解析，绝不进入 GET /v1/models——
// 输出仅含启用对外模型名与 Auto 名，别名无论规范模型是否启用都不得出现。
func TestHandleModels_AliasNotListed(t *testing.T) {
	h := build(t, "http://unused")
	h.gw.listEnabled = func(_ context.Context) ([]model.ExternalModel, error) {
		return []model.ExternalModel{
			{ID: 1, ExternalName: "real-model", Enabled: true,
				Aliases: []string{"cc-default", "sonnet"}, CreatedAt: time.Unix(1700000000, 0)},
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
	ids := map[string]bool{}
	for _, d := range body.Data {
		ids[d.ID] = true
	}
	if !ids["real-model"] {
		t.Fatalf("启用对外模型名应被列出，data=%s", rec.Body.String())
	}
	if ids["cc-default"] || ids["sonnet"] {
		t.Fatalf("别名绝不进入 /v1/models，data=%s", rec.Body.String())
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

// 会话过期 TTL 按档位（上下文长度 + 对话次数）计算：续期只增不减。
func TestServeSessionExpiryByTiers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	if h.expiry == nil {
		t.Fatalf("harness expiry resolver not wired")
	}
	sid := router.SessionID(7, 42, "gpt-4", "tiers")
	body := `{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"hello"}]}`

	// 首次请求：TTL 档位返回 10 分钟。
	h.expiry.fn = func(ctxTokens, turns int) time.Duration {
		if turns >= 10 {
			return 3 * time.Hour
		}
		return 10 * time.Minute
	}
	rec1 := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", body, map[string]string{"x-session-id": "tiers"})
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request want 200, got %d: %s", rec1.Code, rec1.Body.String())
	}
	s1, ok := h.rrouter.sess.Lookup(sid)
	if !ok {
		t.Fatalf("session not attached after first request")
	}
	wantFirst := h.clock.now().Add(10 * time.Minute)
	if d := s1.ExpireAt.Sub(wantFirst); d < -time.Second || d > time.Second {
		t.Fatalf("first ExpireAt want ~now+10m, got %v (delta %v)", s1.ExpireAt, d)
	}
	expAfterFirst := s1.ExpireAt

	// 第二次请求：TTL 更短（1 分钟），续期只增不减 → 不得缩短。
	h.clock.add(time.Minute)
	h.expiry.fn = func(ctxTokens, turns int) time.Duration { return time.Minute }
	rec2 := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", body, map[string]string{"x-session-id": "tiers"})
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request want 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	s2, ok := h.rrouter.sess.Lookup(sid)
	if !ok {
		t.Fatalf("session lost after second request")
	}
	if !s2.ExpireAt.Equal(expAfterFirst) {
		t.Fatalf("ExpireAt should not shrink on shorter TTL, want %v got %v", expAfterFirst, s2.ExpireAt)
	}

	// 第三次请求：TTL 更长（3 小时），续期延长至当前时刻 + 3h。
	h.expiry.fn = func(ctxTokens, turns int) time.Duration { return 3 * time.Hour }
	rec3 := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", body, map[string]string{"x-session-id": "tiers"})
	if rec3.Code != http.StatusOK {
		t.Fatalf("third request want 200, got %d: %s", rec3.Code, rec3.Body.String())
	}
	s3, ok := h.rrouter.sess.Lookup(sid)
	if !ok {
		t.Fatalf("session lost after third request")
	}
	wantThird := h.clock.now().Add(3 * time.Hour)
	if d := s3.ExpireAt.Sub(wantThird); d < -time.Second || d > time.Second {
		t.Fatalf("third ExpireAt want ~now+3h, got %v (delta %v)", s3.ExpireAt, d)
	}

	if h.expiry.calls < 3 {
		t.Fatalf("resolver should be called at least 3 times, got %d", h.expiry.calls)
	}
}

// 已关闭会话不得继续被服务：关闭后同标识请求应视为「新会话」重建（重新占并发槽、Closed 复位）。
func TestServeClosedSessionReactivates(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	sid := router.SessionID(7, 42, "gpt-4", "abc")
	body := `{"model":"gpt-4","stream":false}`

	// 首次请求建会话，随后关闭（释放并发槽）。
	if rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", body, map[string]string{"x-session-id": "abc"}); rec.Code != http.StatusOK {
		t.Fatalf("first request want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !h.rrouter.sess.Close(sid) {
		t.Fatalf("close session failed")
	}
	if n := h.rrouter.sess.CountActive(1); n != 0 {
		t.Fatalf("关闭后并发槽应释放，got %d", n)
	}

	// 同标识再次请求：应重建为新会话（修复前会被当作 existing 续行，Closed 不复位、不占槽）。
	rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", body, map[string]string{"x-session-id": "abc"})
	if rec.Code != http.StatusOK {
		t.Fatalf("reactivated request want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	s, ok := h.rrouter.sess.Lookup(sid)
	if !ok {
		t.Fatalf("reactivated session not found")
	}
	if s.Closed {
		t.Errorf("重新激活后会话不应仍为已关闭")
	}
	if n := h.rrouter.sess.CountActive(1); n != 1 {
		t.Errorf("重新激活后应重新占并发槽，got %d", n)
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

// TestServeFundSemantics 表驱动覆盖资金语义三类用例（去掉独立余额读取后由预扣条件写统一拦截）：
//   - 零余额且预扣估算为 0（费率缺失）→ 402，不得放行；
//   - 余额不足 → 402；
//   - 钱包缺失 → 对外同样归 402（历史上前置 GetWallet 兜底建 0 余额钱包 → 归 402，
//     移除前置读后需显式保持该对外行为一致；域层 40401/40201 仍可区分）。
//
// 三类均不得发起上游调用，且各留 1 条 failed 账单（每次请求必有一条记录）。
func TestServeFundSemantics(t *testing.T) {
	var called int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	// 费率缺失 → 预扣估算为 0，用于验证「估算为 0 仍拦截零余额」。
	noRatesModel := &model.ExternalModel{ExternalName: "gpt-4", Enabled: true}

	cases := []struct {
		name   string
		mutate func(h *harness)
		want   int
	}{
		{
			name: "零余额且预扣估算为 0→402",
			mutate: func(h *harness) {
				h.models.mapping = noRatesModel
				h.gw.credit = &fakeCredit{balance: 0}
			},
			want: http.StatusPaymentRequired,
		},
		{
			name: "余额不足→402",
			mutate: func(h *harness) {
				h.gw.credit = &fakeCredit{balance: 0.0005}
			},
			want: http.StatusPaymentRequired,
		},
		{
			name: "钱包缺失→对外归 402（保留既有对外行为）",
			mutate: func(h *harness) {
				h.gw.credit = &fakeCredit{preErr: &identity.APIError{
					HTTPStatus: http.StatusNotFound, Code: resp.CodeNotFound, Message: "用户积分钱包不存在"}}
			},
			want: http.StatusPaymentRequired,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := atomic.LoadInt32(&called)
			h := build(t, upstream.URL)
			tc.mutate(h)
			rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
				`{"model":"gpt-4","stream":false,"max_tokens":2000,"messages":[{"role":"user","content":"hello"}]}`)
			if rec.Code != tc.want {
				t.Fatalf("want %d, got %d: %s", tc.want, rec.Code, rec.Body.String())
			}
			if atomic.LoadInt32(&called) != before {
				t.Fatalf("资金拦截不得发起上游调用")
			}
			if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
				t.Fatalf("资金拦截应留 1 条 failed 账单，got %+v", h.billing.reqs)
			}
		})
	}
}

// TestServeZeroCredits_DispatchesPreConsume 端到端锁定「pc.Credits==0 仍照常下发预扣」契约：
// 去掉独立余额读取后，预扣条件写是唯一的资金拦截点，故即便预扣估算为 0（pc.Credits==0）
// 也必须照常下发（不得因金额为 0 短路跳过）：
//   - 零余额 + pc.Credits==0 → 被零余额守卫拦截（402，不发起上游）；
//   - 有余额 + pc.Credits==0 → 零金额预扣放行，正常走上游并 200。
//
// 两例均断言 PreConsume 恰好被调用一次，且不产生正额扣款（证明零金额预扣语义与条件写一致）。
//
// 注：此处 402 由 fakeCredit 的零余额守卫模拟（见 fakeCredit.PreConsume），用于验证网关**仍照常下发**
// 零金额预扣这一契约；真实零余额条件写（钱包缺失→40401 / 余额≤0→40201）由 identity 包
// credit_zero_guard_test.go 覆盖，二者互补。
func TestServeZeroCredits_DispatchesPreConsume(t *testing.T) {
	var called int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	const body = `{"model":"gpt-4","stream":false,"max_tokens":2000,"messages":[{"role":"user","content":"hello"}]}`

	t.Run("零余额且预扣为0→402", func(t *testing.T) {
		before := atomic.LoadInt32(&called)
		h := build(t, upstream.URL)
		fc := &fakeCredit{balance: 0}
		h.gw.credit = fc
		h.billing.estOverride = true // 预扣估算强制为 0 → pc.Credits==0
		rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", body)
		if rec.Code != http.StatusPaymentRequired {
			t.Fatalf("want 402, got %d: %s", rec.Code, rec.Body.String())
		}
		if fc.preCalls != 1 {
			t.Fatalf("即便 pc.Credits==0 也必须下发一次预扣（条件写是唯一拦截点），got %d", fc.preCalls)
		}
		if len(fc.consumed) != 0 {
			t.Fatalf("零金额预扣不应产生正额扣款，got %+v", fc.consumed)
		}
		if atomic.LoadInt32(&called) != before {
			t.Fatal("零余额拦截不得发起上游调用")
		}
		if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
			t.Fatalf("拦截应留 1 条 failed 账单，got %+v", h.billing.reqs)
		}
	})

	t.Run("有余额且预扣为0→200", func(t *testing.T) {
		before := atomic.LoadInt32(&called)
		h := build(t, upstream.URL)
		fc := &fakeCredit{balance: 5000}
		h.gw.credit = fc
		h.billing.estOverride = true // 预扣估算强制为 0 → pc.Credits==0
		rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if fc.preCalls != 1 {
			t.Fatalf("pc.Credits==0 也必须下发一次预扣，got %d", fc.preCalls)
		}
		if len(fc.consumed) != 0 {
			t.Fatalf("零金额预扣不应扣款，got %+v", fc.consumed)
		}
		if got := atomic.LoadInt32(&called) - before; got != 1 {
			t.Fatalf("有余额应正常走上游一次，got %d", got)
		}
	})
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

// 上游非 2xx 时，上游错误响应体必须落进失败账单与调用日志（供管理员定位上游故障）。
func TestServeNonStream_CapturesUpstreamErrorBody(t *testing.T) {
	const upErrJSON = `{"error":{"message":"Insufficient balance","type":"insufficient_quota"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(upErrJSON))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	_ = doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

	if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
		t.Fatalf("want 1 failed billing, got %+v", h.billing.reqs)
	}
	if !strings.Contains(h.billing.reqs[0].ErrorMessage, "Insufficient balance") {
		t.Fatalf("失败账单应包含上游错误体, got %q", h.billing.reqs[0].ErrorMessage)
	}
	if len(h.clogs.inserted) == 0 {
		t.Fatalf("want at least 1 call log")
	}
	cl := h.clogs.inserted[len(h.clogs.inserted)-1]
	if !strings.Contains(cl.ErrorMessage, "Insufficient balance") {
		t.Fatalf("调用日志 ErrorMessage 应包含上游错误体, got %q", cl.ErrorMessage)
	}
	if !strings.Contains(cl.RespBody, "Insufficient balance") {
		t.Fatalf("调用日志 RespBody 应包含上游错误体, got %q", cl.RespBody)
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

// 场景 F6（AC-17 放宽）：上游成功但未返回 usage → 透传上游正文(200) + 按可见内容估算计费（不退预扣）
// + 计入渠道健康失败（窗口错误率达标才 DRAIN，探针可自愈；不再单次禁用整条渠道）。
func TestServeZeroUsageEstimatesAndFeedsHealthFailure(t *testing.T) {
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
		t.Fatalf("usage 缺失应透传上游正文(200), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hi") {
		t.Fatalf("usage 缺失应透传上游正文, got %s", rec.Body.String())
	}
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Fail {
		t.Fatalf("usage 缺失应按估算计费（成功账单）, got %+v", h.billing.reqs)
	}
	if u := h.billing.reqs[0].Tokens; u.Input <= 0 || u.Output <= 0 {
		t.Fatalf("usage 缺失应按可见内容估算 token, got %+v", u)
	}
	if len(h.chans.feeds) != 1 || h.chans.feeds[0].fb.IsSuccess {
		t.Fatalf("usage 缺失应计入渠道健康失败, got %+v", h.chans.feeds)
	}
	// 内容已透传、按估算正常结算 → 不应退回预扣（差额由结算多退少补）。
	if len(fc.refunded) != 0 {
		t.Fatalf("usage 缺失内容已透传，不应退回预扣, got refunded=%v", fc.refunded)
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

// sessionNameFor 可读名称生成（通用 XML 规则，不绑定客户端标签）：游离文本 /
// <user*> 标签 / 首条正文 三段选取，兼容纯文本与多模态数组；回退 raw / 截断。
func TestSessionNameFor(t *testing.T) {
	long := strings.Repeat("A", 60)
	cases := []struct {
		raw  string
		body string
		want string
	}{
		// 规则 1：纯文本（无 XML）→ 原文摘要
		{"", `{"messages":[{"role":"user","content":"你好世界"},{"role":"assistant","content":"hi"}]}`, "你好世界"},
		// 规则 1：content 数组收集全部文本块并折叠空白
		{"", `{"messages":[{"role":"user","content":[{"type":"text","text":"  hello   world "}]}]}`, "hello world"},
		// 无 user 消息：回退 raw
		{"xyz", `{"messages":[{"role":"system","content":"sys"}]}`, "xyz"},
		// 超长截断
		{"", `{"messages":[{"role":"user","content":"` + long + `"}]}`, strings.Repeat("A", 40) + "…"},
		// 64 位 hex 隐式种子不可读 → 占位名
		{strings.Repeat("ab", 32), `{}`, "未命名会话"},
		// 规则 2：全文在 XML 块内（块间无游离文本）→ 取 <user*> 标签内的真实提问
		{"", `{"messages":[{"role":"user","content":"<trae_command>\n--- Command Name: trae-remote-official:browser ---\n--- End Command ---\n</trae_command>\n<system-reminder>环境上下文……</system-reminder>\n<user_input>如何排查 5 个问题</user_input>"}]}`, "如何排查 5 个问题"},
		// 规则 1 优先于规则 2：游离在标签之外的文本最先命中
		{"", `{"messages":[{"role":"user","content":"先决条件<x>包装内容</x><user>末尾</user>"}]}`, "先决条件"},
		// 规则 3：无游离文本、无 user 标签 → 首条正文（标签块内部文本）
		{"", `{"messages":[{"role":"user","content":"<trae_command>--- Command Name: browser ---</trae_command>"}]}`, "--- Command Name: browser ---"},
		// 普通 OpenAI 请求（无 XML）：原样作名称，即使以 "<" 开头（HTML/XML 提问，标签不进入名称）
		{"", `{"messages":[{"role":"user","content":"<div>页面布局异常</div> 如何定位"}]}`, "如何定位"},
		// 标准多模态数组（OpenAI 格式）：各 text 块收集拼接
		{"", `{"messages":[{"role":"user","content":[{"type":"text","text":"图片里橙色按钮"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"在哪一版"}]}]}`, "图片里橙色按钮 在哪一版"},
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
	// 决策轨迹：成功路径也应写入（attempts 含当前候选 success + result=success）。
	dec, ok := cl.Decision.(map[string]any)
	if !ok {
		t.Fatalf("success call log decision missing: %v", cl.Decision)
	}
	raw, _ := json.Marshal(dec)
	var sdec struct {
		Attempts []map[string]any `json:"attempts"`
		Result   string           `json:"result"`
	}
	if err := json.Unmarshal(raw, &sdec); err != nil {
		t.Fatalf("unmarshal success decision: %v", err)
	}
	if len(sdec.Attempts) < 1 {
		t.Fatalf("success decision attempts want >=1, got %+v", sdec.Attempts)
	}
	last := sdec.Attempts[len(sdec.Attempts)-1]
	if last["reason"] != "success" {
		t.Errorf("success decision last attempt want success, got %+v", last)
	}
	// OQ-3：决策轨迹每条候选应携带成本标量/是否有效/时段系数。
	assertAttemptCostFields(t, last)
	if sdec.Result != "success" {
		t.Errorf("success decision result want success, got %q", sdec.Result)
	}
}

// assertAttemptCostFields 断言决策轨迹候选条目含成本相关键（OQ-3）。
func assertAttemptCostFields(t *testing.T, attempt map[string]any) {
	t.Helper()
	for _, k := range []string{"cost", "cost_known", "route_time_coeff"} {
		if _, ok := attempt[k]; !ok {
			t.Errorf("decision attempt 缺少成本字段 %q, got %+v", k, attempt)
		}
	}
}

// 调用日志-2：流式成功——RespKind=stream、RespBody 为拼接的 assistant content（增量拼接）。
func TestServeCallLog_StreamSuccess(t *testing.T) {
	streamBody := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
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
	// 决策轨迹：流式成功路径同样应写入（result=success）。
	dec, ok := cl.Decision.(map[string]any)
	if !ok {
		t.Fatalf("stream success call log decision missing: %v", cl.Decision)
	}
	raw, _ := json.Marshal(dec)
	var sdec struct {
		Attempts []map[string]any `json:"attempts"`
		Result   string           `json:"result"`
	}
	if err := json.Unmarshal(raw, &sdec); err != nil {
		t.Fatalf("unmarshal stream success decision: %v", err)
	}
	if len(sdec.Attempts) < 1 {
		t.Fatalf("stream success decision attempts want >=1, got %+v", sdec.Attempts)
	}
	if sdec.Result != "success" {
		t.Errorf("stream success decision result want success, got %q", sdec.Result)
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
	// OQ-3：被拦截候选同样应携带成本字段。
	assertAttemptCostFields(t, a0)
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

// 估算器：content 数组容错、多 choice 聚合（与流式一致）、anthropic content[]、未知形状响应体兜底、input 字段。
func TestMissingUsageEstimate(t *testing.T) {
	// content 为数组（多模态）不应导致整条解析失败。
	cArr, _, _ := rawParts([]byte(`{"choices":[{"message":{"content":[{"type":"text","text":"hi"}]}}]}`))
	if cArr != "hi" {
		t.Fatalf("rawParts 应兼容 content 数组, got %q", cArr)
	}

	// 多 choice 聚合（与流式拼装口径一致）。
	cMulti, _, _ := rawParts([]byte(`{"choices":[{"message":{"content":"a"}},{"message":{"content":"b"}}]}`))
	if cMulti != "ab" {
		t.Fatalf("rawParts 应聚合全部 choices, got %q", cMulti)
	}

	// anthropic 顶层 content[]。
	cAnth, _, _ := rawParts([]byte(`{"content":[{"type":"text","text":"hi"}]}`))
	if cAnth != "hi" {
		t.Fatalf("rawParts 应识别 anthropic content[], got %q", cAnth)
	}

	// 未知形状（如 openai-responses）：正文提取为空 → 按响应体兜底，Output/Input 均 > 0。
	u := missingUsageEstimate(
		[]byte(`{"input":"hello world"}`),
		[]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"hi there"}]}]}`),
		"gpt-4", "", "", nil)
	if u.Output <= 0 {
		t.Fatalf("未知形状应兜底估算输出, got %+v", u)
	}
	if u.Input <= 0 {
		t.Fatalf("input 字段应计入输入估算, got %+v", u)
	}
}

// Step 2：路由层排除项（排空）并入决策轨迹——skipped 项置于已尝试候选之前，
// 且带稳定 reason 与运维动作 hint；无排除项时行为不变（见其余既有用例）。
func TestDecision_ExcludedPrependedBeforeTried(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(testOKBody))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.rrouter.routes = []*router.ChannelRoute{
		{ChannelID: 1, ChannelKeyID: 1, ModelRowID: 7, InternalModelID: "gpt-4-internal", ExternalModelID: 1},
	}
	h.rrouter.excluded = []router.ExcludedCandidate{
		{ChannelID: 2, ChannelName: "ch2", ChannelKeyID: 21, InternalModelID: "gpt-4-internal", Reason: router.ExcludeDrained},
	}

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.clogs.inserted) != 1 {
		t.Fatalf("want 1 call log, got %d", len(h.clogs.inserted))
	}
	dec, ok := h.clogs.inserted[0].Decision.(map[string]any)
	if !ok {
		t.Fatalf("decision missing: %v", h.clogs.inserted[0].Decision)
	}
	raw, _ := json.Marshal(dec)
	var sdec struct {
		Attempts []map[string]any `json:"attempts"`
	}
	if err := json.Unmarshal(raw, &sdec); err != nil {
		t.Fatalf("unmarshal decision: %v", err)
	}
	if len(sdec.Attempts) != 2 {
		t.Fatalf("want 2 attempts（排除项 + 成功候选）, got %+v", sdec.Attempts)
	}
	first := sdec.Attempts[0]
	if first["reason"] != "drained" || first["skipped"] != true {
		t.Errorf("排除项应为 drained/skipped，got %+v", first)
	}
	if first["hint"] == nil || first["hint"] == "" {
		t.Errorf("排除项应带运维动作 hint，got %+v", first)
	}
	if sdec.Attempts[1]["reason"] != "success" {
		t.Errorf("末项应为 success，got %+v", sdec.Attempts[1])
	}
}

// Step 4：「所有候选因排空被排除」→ 503，且调用日志 decision.attempts 含 drained skipped 项，
// 请求级结论 attempt_trail 含排空步骤（不再静默无记录）。
func TestDrainTrail_AllDrainedRecorded(t *testing.T) {
	h := build(t, "http://unused")
	h.rrouter.err = &router.RouteError{Kind: router.KindNoAvailable, HTTPStatus: http.StatusServiceUnavailable, Message: msgNoAvailable}
	h.rrouter.excluded = []router.ExcludedCandidate{
		{ChannelID: 1, ChannelName: "ch1", ChannelKeyID: 11, InternalModelID: "gpt-4-internal", Reason: router.ExcludeDrained},
	}
	dump := withLogCapture(h)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.clogs.inserted) != 1 {
		t.Fatalf("want 1 call log with decision, got %d", len(h.clogs.inserted))
	}
	dec, ok := h.clogs.inserted[0].Decision.(map[string]any)
	if !ok {
		t.Fatalf("decision missing: %v", h.clogs.inserted[0].Decision)
	}
	raw, _ := json.Marshal(dec)
	var sdec struct {
		Attempts []map[string]any `json:"attempts"`
		Result   string           `json:"result"`
	}
	if err := json.Unmarshal(raw, &sdec); err != nil {
		t.Fatalf("unmarshal decision: %v", err)
	}
	if len(sdec.Attempts) != 1 || sdec.Attempts[0]["reason"] != "drained" || sdec.Attempts[0]["skipped"] != true {
		t.Fatalf("decision attempts want 1 drained skipped, got %+v", sdec.Attempts)
	}
	if sdec.Result != "rejected" {
		t.Errorf("decision result want rejected, got %q", sdec.Result)
	}
	out := dump()
	if !strings.Contains(out, "attempt_trail=") || !strings.Contains(out, "drained") {
		t.Errorf("请求级结论应含排空步骤 attempt_trail：\n%s", out)
	}
}

// Step 5：流式上游失败诊断——「未送达」时结构化日志与调用日志均带
// delivered / first_byte_received / elapsed_ms，且两条中文文案保持不变（此处为连接失败文案）。
func TestStreamDiag_NotDeliveredFields(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // 关闭监听 → 连接被拒（请求未送达）

	h := build(t, deadURL)
	dump := withLogCapture(h)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":true}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503（单候选流式连接失败转移后无可用渠道）, got %d: %s", rec.Code, rec.Body.String())
	}
	out := dump()
	for _, want := range []string{"upstream call failed", "kind=connect_failed", "stage=stream",
		"delivered=false", "first_byte_received=false", "elapsed_ms="} {
		if !strings.Contains(out, want) {
			t.Errorf("流式失败诊断日志缺少 %q\n%s", want, out)
		}
	}
	if len(h.clogs.inserted) == 0 {
		t.Fatalf("want call log for stream connect failure")
	}
	cl := h.clogs.inserted[0]
	if cl.ErrorMessage != "上游流式连接失败" {
		t.Errorf("文案应保持「上游流式连接失败」, got %q", cl.ErrorMessage)
	}
	dec, ok := cl.Decision.(map[string]any)
	if !ok {
		t.Fatalf("decision missing: %v", cl.Decision)
	}
	if dec["delivered"] != false {
		t.Errorf("调用日志 decision delivered want false, got %v", dec["delivered"])
	}
	if _, ok := dec["first_byte_received"]; !ok {
		t.Errorf("调用日志 decision 缺 first_byte_received: %v", dec)
	}
	if _, ok := dec["elapsed_ms"]; !ok {
		t.Errorf("调用日志 decision 缺 elapsed_ms: %v", dec)
	}
}

// noWriteTraceTransport 模拟「请求未送达上游」的连接阶段失败：首次调用直接返回错误且不触发
// WroteRequest 轨迹 → httptrace 判定 wrote=false，即 delivered=false（可安全转移）；
// 后续调用交给真实 transport，用于验证 failover 到下一候选后能成功。
type noWriteTraceTransport struct {
	calls int32
	real  http.RoundTripper
}

func (t *noWriteTraceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if atomic.AddInt32(&t.calls, 1) == 1 {
		return nil, errors.New("dial tcp 127.0.0.1:0: connect: connection refused")
	}
	if t.real == nil {
		t.real = http.DefaultTransport
	}
	return t.real.RoundTrip(req)
}

// canceledTransport 模拟「下游客户端/前置代理提前断开导致的请求取消」：RoundTrip 直接返回
// context.Canceled（与上游无关，上游并未失败），用于验证 client-gone 归因分支。
type canceledTransport struct{ calls int32 }

func (t *canceledTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	atomic.AddInt32(&t.calls, 1)
	return nil, context.Canceled
}

// wroteThenFailTransport 模拟「请求已送达上游后失败」：先主动触发 WroteRequest（wrote=true，
// 即 delivered=true），再返回错误——对应上游可能已生成并计费、不可转移的响应超时场景。
type wroteThenFailTransport struct{ calls int32 }

func (t *wroteThenFailTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&t.calls, 1)
	if tr := httptrace.ContextClientTrace(req.Context()); tr != nil && tr.WroteRequest != nil {
		tr.WroteRequest(httptrace.WroteRequestInfo{Err: nil})
	}
	return nil, errors.New("read tcp 127.0.0.1:0: read: connection reset by peer")
}

// Step 9：流式「未送达」——连接阶段失败（delivered=false）→ 可 failover 到下一候选；
// 结构化日志与失败候选调用日志均带 delivered=false。
// 对照用例：上游「明确返回错误状态码」（非 client-gone、非无状态码超时）必须仍整单退预扣，
// 与「已送达超时」改为按估算结算形成对照（对齐 new-api「除非上游明确报错」）。
func TestUpstreamExplicitError_StillRefundsFullPreConsume(t *testing.T) {
	upstream := upstreamErrServer(t, http.StatusGatewayTimeout, `{"error":{"message":"timeout"}}`)
	defer upstream.Close()

	h := build(t, upstream.URL)
	fc := &fakeCredit{balance: 5000}
	h.gw.credit = fc
	dump := withLogCapture(h)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"max_tokens":2000,"messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502（默认网关错误）, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(fc.consumed) != 1 || len(fc.refunded) != 1 || math.Abs(fc.consumed[0]-fc.refunded[0]) > 0.0001 {
		t.Fatalf("上游明确报错必须整单退预扣, consumed=%v refunded=%v", fc.consumed, fc.refunded)
	}
	if out := dump(); strings.Contains(out, "kind=client_gone") {
		t.Errorf("上游明确报错不应归为 client_gone：\n%s", out)
	}
}

// 结算失败（recordBilling 报错）时置 billFailed（预扣不退=对齐既有 billFailed 语义），
// 请求级归因为「计费失败」且不整单退预扣——避免「扣了却无账单」的静默不一致。
func TestDeliveredTimeout_BillingFailureMarksBillFailed(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer live.Close()

	h := build(t, live.URL)
	fc := &fakeCredit{balance: 5000}
	h.gw.credit = fc
	h.billing.err = errors.New("record boom")                        // 结算失败注入
	h.gw.client = &http.Client{Transport: &wroteThenFailTransport{}} // 已送达后失败（非流式）
	dump := withLogCapture(h)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"max_tokens":2000,"messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502（已送达后失败）, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(fc.refunded) != 0 {
		t.Fatalf("结算失败不应整单退预扣（billFailed 语义）, refunded=%v", fc.refunded)
	}
	if out := dump(); !strings.Contains(out, "计费失败") {
		t.Errorf("结算失败应归因为计费失败：\n%s", out)
	}
}

// client-gone（非流式）分支下 recordBilling 报错 → 置 billFailed（预扣不退），
// 请求级与调用日志归因「计费失败」，对齐主循环 switch 的 billFailed 优先归因。
func TestClientGone_BillingFailureMarksBillFailed(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer live.Close()

	h := build(t, live.URL)
	fc := &fakeCredit{balance: 5000}
	h.gw.credit = fc
	h.billing.credit = fc
	h.billing.err = errors.New("record boom") // 结算失败注入
	h.gw.client = &http.Client{Transport: &canceledTransport{}}
	dump := withLogCapture(h)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != 499 {
		t.Fatalf("client-gone 应写回 499, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(fc.refunded) != 0 {
		t.Fatalf("结算失败不应整单退预扣（billFailed 语义）, refunded=%v", fc.refunded)
	}
	if out := dump(); !strings.Contains(out, "计费失败") {
		t.Errorf("结算失败应归因为计费失败：\n%s", out)
	}
	var cl *CallLog
	for _, c := range h.clogs.inserted {
		if c.ErrorMessage == "计费失败" {
			cl = c
		}
	}
	if cl == nil {
		t.Fatalf("应有调用日志归因「计费失败」, got %+v", h.clogs.inserted)
	}
	if dec, ok := cl.Decision.(map[string]any); !ok || dec["result"] != "计费失败" {
		t.Errorf("调用日志决策 reason 应为计费失败, got %v", cl.Decision)
	}
}

// 流式「已送达超时」分支下 recordBilling 报错 → 置 billFailed（预扣不退），
// 请求级与调用日志归因「计费失败」，对齐主循环 switch 的 billFailed 优先归因。
func TestDeliveredTimeout_StreamBillingFailureMarksBillFailed(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer live.Close()

	h := build(t, live.URL)
	fc := &fakeCredit{balance: 5000}
	h.gw.credit = fc
	h.billing.credit = fc
	h.billing.err = errors.New("record boom")                              // 结算失败注入
	h.gw.streamClient = &http.Client{Transport: &wroteThenFailTransport{}} // 已送达后失败（流式）
	dump := withLogCapture(h)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("已送达超时应回 502, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(fc.refunded) != 0 {
		t.Fatalf("结算失败不应整单退预扣（billFailed 语义）, refunded=%v", fc.refunded)
	}
	if out := dump(); !strings.Contains(out, "计费失败") {
		t.Errorf("结算失败应归因为计费失败：\n%s", out)
	}
	var cl *CallLog
	for _, c := range h.clogs.inserted {
		if c.ErrorMessage == "计费失败" {
			cl = c
		}
	}
	if cl == nil {
		t.Fatalf("应有调用日志归因「计费失败」, got %+v", h.clogs.inserted)
	}
	if dec, ok := cl.Decision.(map[string]any); !ok || dec["result"] != "计费失败" {
		t.Errorf("调用日志决策 reason 应为计费失败, got %v", cl.Decision)
	}
}

func TestStreamDelivered_ConnectFailureFailsOver(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer live.Close()

	h := build(t, live.URL)
	addSecondKey(h, live.URL, "k2-secret")
	// 第一候选：连接阶段失败（未送达）→ 安全转移到第二候选成功。
	tt := &noWriteTraceTransport{}
	h.gw.streamClient = &http.Client{Transport: tt}
	dump := withLogCapture(h)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("未送达应可 failover 到下一候选并成功, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("应透传下一候选 SSE 内容, got %q", rec.Body.String())
	}
	if got := atomic.LoadInt32(&tt.calls); got != 2 {
		t.Fatalf("未送达候选应发起一次失败调用后 failover 到下一候选（共 2 次）, got %d", got)
	}
	out := dump()
	if !strings.Contains(out, "delivered=false") {
		t.Errorf("结构化日志应含 delivered=false：\n%s", out)
	}
	var connFailed *CallLog
	for _, cl := range h.clogs.inserted {
		if cl.ErrorMessage == "上游流式连接失败" {
			connFailed = cl
		}
	}
	if connFailed == nil {
		t.Fatalf("应有一条「上游流式连接失败」调用日志, got %+v", h.clogs.inserted)
	}
	dec, ok := connFailed.Decision.(map[string]any)
	if !ok || dec["delivered"] != false {
		t.Errorf("未送达候选 decision.delivered want false, got %v", connFailed.Decision)
	}
}

// Step 9：流式「已送达」——请求送达后失败（delivered=true）→ 回 502 且不转移
// （即便仍有候选）；结构化日志与调用日志均带 delivered=true。
// rev2（D9）：上游未返回错误状态码（Do 报错/响应超时）改为按估算输入用量结算，不再整单退预扣。
func TestStreamDelivered_AfterDeliveredNoFailover(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer live.Close()

	h := build(t, live.URL)
	addSecondKey(h, live.URL, "k2-secret")
	fc := &fakeCredit{balance: 5000}
	h.gw.credit = fc
	h.billing.credit = fc
	// 请求已送达后失败（多为响应超时）：不转移，直接回 502。
	tt := &wroteThenFailTransport{}
	h.gw.streamClient = &http.Client{Transport: tt}
	dump := withLogCapture(h)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":true}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("已送达后失败应回 502 且不转移, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&tt.calls); got != 1 {
		t.Fatalf("已送达不应触发第二次上游调用, got %d", got)
	}
	out := dump()
	if !strings.Contains(out, "delivered=true") {
		t.Errorf("结构化日志应含 delivered=true：\n%s", out)
	}
	var deliveredFailed *CallLog
	for _, cl := range h.clogs.inserted {
		if cl.ErrorMessage == "上游流式请求失败（已送达，不转移）" {
			deliveredFailed = cl
		}
	}
	if deliveredFailed == nil {
		t.Fatalf("应有一条「已送达，不转移」调用日志, got %+v", h.clogs.inserted)
	}
	dec, ok := deliveredFailed.Decision.(map[string]any)
	if !ok || dec["delivered"] != true {
		t.Errorf("已送达候选 decision.delivered want true, got %v", deliveredFailed.Decision)
	}
	// rev2：按估算输入用量结算（Input>0、Output==0），且不整单退预扣。
	if len(h.billing.reqs) != 1 {
		t.Fatalf("已送达超时应按估算结算一次 Record, got %d: %+v", len(h.billing.reqs), h.billing.reqs)
	}
	if u := h.billing.reqs[0].Tokens; u.Input <= 0 || u.Output != 0 {
		t.Fatalf("已送达超时应按估算输入用量结算（Input>0, Output==0）, got %+v", u)
	}
	if len(fc.consumed) != 1 {
		t.Fatalf("want 1 次预扣, got %v", fc.consumed)
	}
	pre := fc.consumed[0]
	for _, r := range fc.refunded {
		if math.Abs(r-pre) < 0.0001 {
			t.Fatalf("已送达超时不应整单退还预扣 %.6f, refunded=%v", pre, fc.refunded)
		}
	}
}

// nonStreamDeliveredFailTransport 与 wroteThenFailTransport 等价，专供非流式 gw.client：
// 先触发 WroteRequest（delivered=true），再返回连接重置错误（上游未返回错误状态码）。
type nonStreamDeliveredFailTransport struct{ calls int32 }

func (t *nonStreamDeliveredFailTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&t.calls, 1)
	if tr := httptrace.ContextClientTrace(req.Context()); tr != nil && tr.WroteRequest != nil {
		tr.WroteRequest(httptrace.WroteRequestInfo{Err: nil})
	}
	return nil, errors.New("read tcp 127.0.0.1:0: read: connection reset by peer")
}

// rev2（D9）：流式「已送达超时」（Do 报错、无上游状态码）→ HTTP 502、按估算输入结算（Input>0/Output==0）、
// 不整单退预扣。
func TestDeliveredTimeout_StreamSettlesByEstimate(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer live.Close()

	h := build(t, live.URL)
	fc := &fakeCredit{balance: 5000}
	h.gw.credit = fc
	h.billing.credit = fc
	tt := &wroteThenFailTransport{}
	h.gw.streamClient = &http.Client{Transport: tt}

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("已送达超时应回 502, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&tt.calls); got != 1 {
		t.Fatalf("已送达超时不应触发第二次上游调用, got %d", got)
	}
	if len(h.billing.reqs) != 1 {
		t.Fatalf("已送达超时应按估算结算一次 Record, got %d: %+v", len(h.billing.reqs), h.billing.reqs)
	}
	if u := h.billing.reqs[0].Tokens; u.Input <= 0 || u.Output != 0 {
		t.Fatalf("应按估算输入用量结算（Input>0, Output==0）, got %+v", u)
	}
	if len(fc.consumed) != 1 {
		t.Fatalf("want 1 次预扣, got %v", fc.consumed)
	}
	pre := fc.consumed[0]
	for _, r := range fc.refunded {
		if math.Abs(r-pre) < 0.0001 {
			t.Fatalf("已送达超时不应整单退还预扣 %.6f, refunded=%v", pre, fc.refunded)
		}
	}
}

// rev2（D9）：非流式「已送达超时」（gw.client Do 报错、无上游状态码）语义与流式一致：
// HTTP 502、按估算输入结算（Input>0/Output==0）、不整单退预扣。
func TestDeliveredTimeout_NonStreamSettlesByEstimate(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer live.Close()

	h := build(t, live.URL)
	fc := &fakeCredit{balance: 5000}
	h.gw.credit = fc
	h.billing.credit = fc
	tt := &nonStreamDeliveredFailTransport{}
	h.gw.client = &http.Client{Transport: tt}

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("已送达超时应回 502, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&tt.calls); got != 1 {
		t.Fatalf("已送达超时不应触发第二次上游调用, got %d", got)
	}
	if len(h.billing.reqs) != 1 {
		t.Fatalf("已送达超时应按估算结算一次 Record, got %d: %+v", len(h.billing.reqs), h.billing.reqs)
	}
	if u := h.billing.reqs[0].Tokens; u.Input <= 0 || u.Output != 0 {
		t.Fatalf("应按估算输入用量结算（Input>0, Output==0）, got %+v", u)
	}
	if len(fc.consumed) != 1 {
		t.Fatalf("want 1 次预扣, got %v", fc.consumed)
	}
	pre := fc.consumed[0]
	for _, r := range fc.refunded {
		if math.Abs(r-pre) < 0.0001 {
			t.Fatalf("已送达超时不应整单退还预扣 %.6f, refunded=%v", pre, fc.refunded)
		}
	}
}

// logLineContaining 返回文本日志中首条包含 substr 的行（TextHandler 每条记录占一行）。
func logLineContaining(out, substr string) string {
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, substr) {
			return ln
		}
	}
	return ""
}

// Step 1（RED）：流式建连阶段因下游客户端/前置代理提前断开而取消（Do 返回 context.Canceled），
// 必须归因为 client_gone（Info），不得误记为上游失败、不得写出 502，且不回喂渠道健康。
// 改造前该测试应失败（RED）。
func TestClientGone_StreamConnectStage_NoUpstreamFailure(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer live.Close()

	h := build(t, live.URL)
	h.gw.streamClient = &http.Client{Transport: &canceledTransport{}}
	dump := withLogCapture(h)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != 499 {
		t.Fatalf("client-gone 应写回 499, got %d: %s", rec.Code, rec.Body.String())
	}

	out := dump()
	if !strings.Contains(out, "kind=client_gone") {
		t.Errorf("建连阶段 client-gone 应归因为 kind=client_gone：\n%s", out)
	}
	if !strings.Contains(out, "status=499") {
		t.Errorf("请求级结论应含 status=499：\n%s", out)
	}
	if strings.Contains(out, "上游流式调用失败") {
		t.Errorf("client-gone 不应误记为上游流式失败：\n%s", out)
	}
	if strings.Contains(out, "status=502") {
		t.Errorf("client-gone 不应写出 502：\n%s", out)
	}
	if len(h.chans.feeds) != 0 {
		t.Errorf("client-gone 不应回喂渠道健康, got %+v", h.chans.feeds)
	}
}

// Step 2（RED）：非流式建连阶段因下游客户端断开而取消（gw.client 返回 context.Canceled），
// 语义与流式一致：kind=client_gone、不含 502、不回喂渠道健康。
func TestClientGone_NonStreamConnectStage(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer live.Close()

	h := build(t, live.URL)
	h.gw.client = &http.Client{Transport: &canceledTransport{}}
	dump := withLogCapture(h)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != 499 {
		t.Fatalf("client-gone 应写回 499, got %d: %s", rec.Code, rec.Body.String())
	}

	out := dump()
	if !strings.Contains(out, "kind=client_gone") {
		t.Errorf("建连阶段 client-gone 应归因为 kind=client_gone：\n%s", out)
	}
	if !strings.Contains(out, "status=499") {
		t.Errorf("请求级结论应含 status=499：\n%s", out)
	}
	if strings.Contains(out, "上游调用失败") {
		t.Errorf("client-gone 不应误记为上游调用失败：\n%s", out)
	}
	if strings.Contains(out, "status=502") {
		t.Errorf("client-gone 不应写出 502：\n%s", out)
	}
	if len(h.chans.feeds) != 0 {
		t.Errorf("client-gone 不应回喂渠道健康, got %+v", h.chans.feeds)
	}
}

// Step 3（RED）：client-gone 应按估算输入用量结算（Input>0、Output==0），且不整单退还预扣。
func TestClientGone_SettlesByEstimateNotFullRefund(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer live.Close()

	h := build(t, live.URL)
	fc := &fakeCredit{balance: 5000}
	h.gw.credit = fc
	h.billing.credit = fc
	h.gw.streamClient = &http.Client{Transport: &canceledTransport{}}

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != 499 {
		t.Fatalf("client-gone 应写回 499, got %d: %s", rec.Code, rec.Body.String())
	}

	if len(h.billing.reqs) != 1 {
		t.Fatalf("client-gone 应记录一次结算 Record, got %d: %+v", len(h.billing.reqs), h.billing.reqs)
	}
	u := h.billing.reqs[0].Tokens
	if u.Input <= 0 || u.Output != 0 {
		t.Fatalf("应按估算输入用量结算（Input>0, Output==0）, got %+v", u)
	}
	if len(fc.consumed) != 1 {
		t.Fatalf("want 1 次预扣, got %v", fc.consumed)
	}
	pre := fc.consumed[0]
	for _, r := range fc.refunded {
		if math.Abs(r-pre) < 0.0001 {
			t.Fatalf("client-gone 不应整单退还预扣 %.6f, refunded=%v", pre, fc.refunded)
		}
	}
}

// Step 4（RED）：请求级结论文案区分「客户端已断开」，且不计入 failed_candidates。
// 改造前该测试应失败（RED）。
func TestClientGone_RequestOutcomeDistinct(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer live.Close()

	h := build(t, live.URL)
	h.gw.streamClient = &http.Client{Transport: &canceledTransport{}}
	dump := withLogCapture(h)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != 499 {
		t.Fatalf("client-gone 应写回 499, got %d: %s", rec.Code, rec.Body.String())
	}

	out := dump()
	line := logLineContaining(out, "gateway request outcome")
	if line == "" {
		t.Fatalf("缺少请求级结论日志 gateway request outcome：\n%s", out)
	}
	if !strings.Contains(line, "status=499") {
		t.Errorf("请求级结论应含 status=499：\n%s", line)
	}
	if !strings.Contains(line, "客户端已断开") {
		t.Errorf("请求级结论 reason 应含「客户端已断开」：\n%s", line)
	}
	if !strings.Contains(line, "failed_candidates=0") {
		t.Errorf("client-gone 不应计入 failed_candidates：\n%s", line)
	}
	if strings.Contains(out, "候选失败") {
		t.Errorf("client-gone 不应出现「候选失败」文案：\n%s", out)
	}
}

// cancelThenErrTransport 模拟「请求 ctx 在转发过程中被取消，但上游返回的是普通错误
// （非 context.Canceled）」：先取消请求 ctx，再返回一个与取消无关的上游错误。
// 用于覆盖 streamClientGone 中 `errors.Is(ctx.Err(), context.Canceled)` 这条回退分支。
type cancelThenErrTransport struct {
	cancel context.CancelFunc
	err    error
}

func (t *cancelThenErrTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.cancel()
	return nil, t.err
}

// F5：用真实已取消的请求 ctx（而非仅 transport 直接返回 context.Canceled）触发 client-gone，
// 覆盖 `ctx.Err()==context.Canceled` 分支——上游错误本身并非 context.Canceled。
func TestClientGone_RequestCtxCanceled(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer live.Close()

	h := build(t, live.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.gw.client = &http.Client{Transport: &cancelThenErrTransport{
		cancel: cancel,
		err:    errors.New("read tcp 127.0.0.1:0: read: connection reset by peer"),
	}}
	dump := withLogCapture(h)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`))
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer sk-gw-t")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.gw.Handler().ServeHTTP(rec, req)
	if rec.Code != 499 {
		t.Fatalf("client-gone 应写回 499, got %d: %s", rec.Code, rec.Body.String())
	}

	out := dump()
	if !strings.Contains(out, "kind=client_gone") {
		t.Errorf("请求 ctx 已取消应归因为 kind=client_gone：\n%s", out)
	}
	if !strings.Contains(out, "status=499") {
		t.Errorf("请求级结论应含 status=499：\n%s", out)
	}
	if strings.Contains(out, "上游调用失败") {
		t.Errorf("client-gone 不应误记为上游调用失败：\n%s", out)
	}
	if strings.Contains(out, "status=502") {
		t.Errorf("client-gone 不应写出 502：\n%s", out)
	}
	if len(h.chans.feeds) != 0 {
		t.Errorf("client-gone 不应回喂渠道健康, got %+v", h.chans.feeds)
	}
}

// failThenCanceledTransport 模拟「首个候选真实上游失败（未送达、可转移），下一候选才因下游
// 断开取消」：第 1 次 RoundTrip 返回普通连接错误（非 context.Canceled），第 2 次返回 context.Canceled。
type failThenCanceledTransport struct{ calls int32 }

func (t *failThenCanceledTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if atomic.AddInt32(&t.calls, 1) == 1 {
		return nil, errors.New("dial tcp 127.0.0.1:0: connect: connection refused")
	}
	return nil, context.Canceled
}

// client-gone 出现在「已有真实候选失败之后」时，不得用「客户端已断开」遮蔽真实失败：
// 第 1 候选连接失败（计入 failed_candidates）→ 转移到第 2 候选命中 client-gone；
// 请求级结论应为「候选失败且请求最终失败」（failed_candidates=1），而非 client-gone 的 Info 结论。
func TestClientGone_AfterRealCandidateFailure(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer live.Close()

	h := build(t, live.URL)
	fc := &fakeCredit{balance: 5000}
	h.gw.credit = fc
	h.billing.credit = fc
	addSecondKey(h, live.URL, "k2-secret")
	h.gw.client = &http.Client{Transport: &failThenCanceledTransport{}}
	dump := withLogCapture(h)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != 499 {
		t.Fatalf("client-gone 应写回 499, got %d: %s", rec.Code, rec.Body.String())
	}

	out := dump()
	// 第 1 候选（连接失败）与第 2 候选（client-gone）都应留痕。
	if !strings.Contains(out, "kind=connect_failed") {
		t.Errorf("首个候选连接失败应留痕 kind=connect_failed：\n%s", out)
	}
	if !strings.Contains(out, "kind=client_gone") {
		t.Errorf("次个候选应留痕 kind=client_gone：\n%s", out)
	}
	line := logLineContaining(out, "gateway request outcome")
	if line == "" {
		t.Fatalf("缺少请求级结论日志 gateway request outcome：\n%s", out)
	}
	// 关键：真实失败优先——请求级结论不得被 client-gone 的 Info 文案遮蔽。
	if !strings.Contains(line, "候选失败且请求最终失败") {
		t.Errorf("存在真实候选失败时应归因为候选失败且请求最终失败：\n%s", line)
	}
	if !strings.Contains(line, "failed_candidates=1") {
		t.Errorf("failed_candidates 应为 1（仅计首个真实失败，client-gone 不计）：\n%s", line)
	}
	if strings.Contains(line, "客户端已断开") {
		t.Errorf("client-gone 结论不得遮蔽真实候选失败：\n%s", line)
	}
}

// ---- F1：中途切换的迁移时机（路由只决策，网关在候选通过校验后才迁移） ----

// newSwitchTestHarness 构造「会话已绑定 key 21，路由返回 [目标 key 11（带切换决策）, 原渠道 key 21]」的网关夹具：
// 目标候选按 finalize 回调决定是否可用（如密钥被禁用），用于断言「候选被跳过时不迁移会话」。
func newSwitchTestHarness(t *testing.T, upstreamURL string, disableTargetKey bool) *harness {
	t.Helper()
	h := build(t, upstreamURL)
	normalCh := func(id int64, name string) *channel.KeyRuntime {
		return &channel.KeyRuntime{
			Key:             channel.ChannelKey{ID: id, ChannelID: 1, Name: name, State: channel.StateNormal},
			Channel:         &channel.Channel{ID: 1, Protocol: channel.ProtocolOpenAICompat, BaseURL: upstreamURL},
			CredentialPlain: "k-secret",
			Machine:         channel.NewMachine(channel.DefaultMachineConfig()),
			Limiter:         channel.NewLimiter(1000, 1000000, 1.2),
		}
	}
	h.chans.keys[21] = normalCh(21, "k21")
	if disableTargetKey {
		kr := normalCh(11, "k11")
		kr.Machine.ForceDisable(time.Now()) // 目标密钥禁用 → 候选在 ConfirmSessionSwitch 之前被跳过
		h.chans.keys[11] = kr
	} else {
		h.chans.keys[11] = normalCh(11, "k11")
	}
	// 目标候选携带「从中途原渠道 21 切到 11」的决策产物；其后为原渠道回退候选（F1 回退保障）。
	h.rrouter.routes = []*router.ChannelRoute{
		{ChannelID: 1, ChannelKeyID: 11, ModelRowID: 7, InternalModelID: "gpt-4-internal", ExternalModelID: 1,
			Switch: &router.SessionCostDecision{Switch: true, FromChannelKeyID: 21, ToChannelKeyID: 11}},
		{ChannelID: 1, ChannelKeyID: 21, ModelRowID: 7, InternalModelID: "gpt-4-internal", ExternalModelID: 1},
	}
	// 会话已绑定原渠道 key 21。
	h.rrouter.Sessions().Attach(&router.Session{
		SessionID: router.SessionID(7, 42, "gpt-4", "s-mid"), ChannelKeyID: 21, InternalModelID: "gpt-4-internal",
		ExpireAt: h.clock.now().Add(time.Hour),
	}, 16)
	return h
}

// okUpstream 返回一个固定 200 响应体的上游服务（供切换后转发成功路径使用）。
func okUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	}))
	t.Cleanup(up.Close)
	return up
}

// TestMidSwitchGateway_MigratesOnlyAfterConfirm 候选通过校验后迁移：会话归属迁到 key 11，
// 且 ConfirmSessionSwitch 收到真实 ttl（F8 透传）与请求级轮次（F7）。
func TestMidSwitchGateway_MigratesOnlyAfterConfirm(t *testing.T) {
	up := okUpstream(t)
	h := newSwitchTestHarness(t, up.URL, false)
	sid := router.SessionID(7, 42, "gpt-4", "s-mid")

	rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false}`, map[string]string{"x-session-id": "s-mid"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.rrouter.switchCalls) != 1 {
		t.Fatalf("应恰好调用一次 ConfirmSessionSwitch，got %d", len(h.rrouter.switchCalls))
	}
	call := h.rrouter.switchCalls[0]
	if call.keyID != 11 || call.sessionID != sid {
		t.Fatalf("ConfirmSessionSwitch 目标应为 key 11 / 该会话，got %+v", call)
	}
	if call.ttl <= 0 {
		t.Fatalf("迁移应透传真实 ttl（F8），got %v", call.ttl)
	}
	s, ok := h.rrouter.Sessions().Lookup(sid)
	if !ok || s.ChannelKeyID != 11 {
		t.Fatalf("确认后会话应迁移到 key 11，got %+v ok=%v", s, ok)
	}
}

// TestMidSwitchGateway_NoMigrateWhenCandidateSkipped 目标候选在确认前被跳过（密钥禁用）：
// 不得迁移会话（留在原渠道 key 21），且请求经回退候选（原渠道）成功。
func TestMidSwitchGateway_NoMigrateWhenCandidateSkipped(t *testing.T) {
	up := okUpstream(t)
	h := newSwitchTestHarness(t, up.URL, true) // 目标密钥 11 被禁用 → 候选在确认前被跳过
	sid := router.SessionID(7, 42, "gpt-4", "s-mid")

	rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false}`, map[string]string{"x-session-id": "s-mid"})
	if rec.Code != http.StatusOK {
		t.Fatalf("应 failover 到回退的原渠道并成功，got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.rrouter.switchCalls) != 0 {
		t.Fatalf("候选被跳过时不得调用 ConfirmSessionSwitch，got %d 次", len(h.rrouter.switchCalls))
	}
	s, ok := h.rrouter.Sessions().Lookup(sid)
	if !ok || s.ChannelKeyID != 21 {
		t.Fatalf("候选被跳过时会话应保持原渠道 key 21，got %+v ok=%v", s, ok)
	}
}

// TestMidSwitchGateway_MigrationFailureKeepsOldChannel 确认迁移失败（目标密钥会话满，F1）：
// 该候选按「会话满」跳过，请求回退到原渠道 key 21 成功，会话归属不变。
func TestMidSwitchGateway_MigrationFailureKeepsOldChannel(t *testing.T) {
	up := okUpstream(t)
	h := newSwitchTestHarness(t, up.URL, false)
	h.rrouter.switchFail = true // 模拟目标密钥并发已满 → 迁移失败
	sid := router.SessionID(7, 42, "gpt-4", "s-mid")

	rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false}`, map[string]string{"x-session-id": "s-mid"})
	if rec.Code != http.StatusOK {
		t.Fatalf("迁移失败应回退原渠道并成功，got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.rrouter.switchCalls) != 1 {
		t.Fatalf("应尝试一次确认迁移，got %d", len(h.rrouter.switchCalls))
	}
	s, ok := h.rrouter.Sessions().Lookup(sid)
	if !ok || s.ChannelKeyID != 21 {
		t.Fatalf("迁移失败时会话应保持原渠道 key 21，got %+v ok=%v", s, ok)
	}
}
