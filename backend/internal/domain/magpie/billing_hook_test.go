// billing_hook_test.go 覆盖第③件在 magpie 侧的接入点：
// 转发前预扣钩子（拿不到钱就不发上游）与 Result 里给结算用的事实
// （请求体 / 额度句柄 / usage 原文 / 生成内容字符数）。
package magpie

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/model"
)

// mu 保护测试里跨 goroutine 的采集切片（结算回调在转发 goroutine 里执行）。
type collector struct {
	mu   sync.Mutex
	seen []Result
}

func (c *collector) add(_ context.Context, res Result) {
	c.mu.Lock()
	c.seen = append(c.seen, res)
	c.mu.Unlock()
}

func (c *collector) all() []Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Result(nil), c.seen...)
}

type reserveEnv struct {
	upstream    *httptest.Server
	gateway     *httptest.Server
	upstreamHit int
	mu          sync.Mutex
	results     []Result
}

// newReserveEnv 组装「假上游 + facade」，并把预扣/结算钩子换成测试替身。
func newReserveEnv(t *testing.T, models map[string]*model.ExternalModel, upstream http.HandlerFunc,
	reserve ReserveFunc, onComplete Hook) *reserveEnv {
	t.Helper()
	env := &reserveEnv{}
	env.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.mu.Lock()
		env.upstreamHit++
		env.mu.Unlock()
		upstream(w, r)
	}))
	t.Cleanup(env.upstream.Close)

	cfg := baseTestConfig()
	cfg.BaseURL = env.upstream.URL
	cipher, err := encryptForTest("caller-secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	h := NewHandler(Deps{
		Config:        cfg,
		Tokens:        &fakeTokens{tok: activeToken()},
		Models:        &fakeModels{byName: models},
		Keys:          &fakeKeys{key: &Key{ID: 1, TokenID: 7, KeyName: "core-k1", KeyCipher: cipher, State: KeyStateActive}},
		CredentialKey: testCredKey,
		Prefixes:      NewPrefixProvider(nil),
		Reserve:       reserve,
		OnComplete:    onComplete,
	})
	mux := http.NewServeMux()
	h.Mount(mux)
	env.gateway = httptest.NewServer(mux)
	t.Cleanup(env.gateway.Close)
	return env
}

func (e *reserveEnv) post(t *testing.T, path, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.gateway.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer tenant-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-session-id", "sess-42")
	resp, err := e.gateway.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return resp, string(got)
}

const chatBody = `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`

// 预扣拒绝 → 直接回拒绝响应，且上游一次都没被调用。
func TestReserve_DenySkipsUpstream(t *testing.T) {
	var called int
	env := newReserveEnv(t, map[string]*model.ExternalModel{"gpt-x": gptModel("gpt-x", "openai/gpt-4o")},
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) },
		func(context.Context, ReserveInput) (any, *Deny) {
			called++
			return nil, &Deny{Status: http.StatusPaymentRequired, Code: "insufficient_credit", Message: "余额不足"}
		}, nil)

	resp, body := env.post(t, "/v1/chat/completions", chatBody)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if called != 1 {
		t.Fatalf("预扣调用次数=%d", called)
	}
	if env.upstreamHit != 0 {
		t.Fatalf("被拒请求不应打到上游: hit=%d", env.upstreamHit)
	}
	if !strings.Contains(body, "insufficient_credit") {
		t.Fatalf("拒绝响应应带网关归类码: %s", body)
	}
}

// 预扣通过 → 额度句柄、请求体、usage 原文与生成内容字符数都回传给结算钩子。
func TestReserve_HandleAndFactsReachSettlement(t *testing.T) {
	type handle struct{ id string }
	h := &handle{id: "resv-1"}
	var got ReserveInput
	col := &collector{}
	env := newReserveEnv(t, map[string]*model.ExternalModel{"gpt-x": gptModel("gpt-x", "openai/gpt-4o")},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"hello there"}}],
				"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}}`))
		},
		func(_ context.Context, in ReserveInput) (any, *Deny) {
			got = in
			return h, nil
		},
		col.add)

	resp, body := env.post(t, "/v1/chat/completions", chatBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	// 预扣入参：模型已改写、带售价与请求体、会话来自头。
	if got.MagpieModel != "openai/gpt-4o" || got.CallerKeyName != "core-k1" || got.SessionID != "sess-42" {
		t.Fatalf("预扣入参=%+v", got)
	}
	if !strings.Contains(string(got.Body), `"model":"openai/gpt-4o"`) {
		t.Fatalf("预扣入参请求体应为改写后的字节: %s", got.Body)
	}
	seenAll := col.all()
	if len(seenAll) != 1 {
		t.Fatalf("结算回调次数=%d", len(seenAll))
	}
	res := seenAll[0]
	if res.Reservation != any(h) {
		t.Fatalf("额度句柄未回传: %+v", res.Reservation)
	}
	if string(res.RequestBody) != string(got.Body) {
		t.Fatalf("Result.RequestBody 应与预扣入参一致")
	}
	if !res.HasUsage || res.Usage.Input != 8 || res.Usage.CacheRead != 3 || res.Usage.Output != 7 {
		t.Fatalf("归一化用量=%+v has=%v", res.Usage, res.HasUsage)
	}
	if len(res.UsageRaw) == 0 {
		t.Fatalf("usage 原文应留痕")
	}
	if res.ObservedTextRunes != len([]rune("hello there")) {
		t.Fatalf("生成内容字符数=%d", res.ObservedTextRunes)
	}
}

// count_tokens 端点不计费：既不预扣也不结算。
func TestReserve_CountTokensSkipsBillingHooks(t *testing.T) {
	var reserved, settled int
	env := newReserveEnv(t, map[string]*model.ExternalModel{"gpt-x": gptModel("gpt-x", "openai/gpt-4o")},
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"count":1}`)) },
		func(context.Context, ReserveInput) (any, *Deny) { reserved++; return nil, nil },
		func(context.Context, Result) { settled++ })

	if resp, body := env.post(t, "/v1/messages/count_tokens", chatBody); resp.StatusCode >= 300 {
		t.Fatalf("count_tokens 请求失败: %d %s", resp.StatusCode, body)
	}
	if reserved != 0 || settled != 0 {
		t.Fatalf("不计费端点触发了账务: reserved=%d settled=%d", reserved, settled)
	}
}

// 流式：旁路累计生成内容字符数（逐帧增量），usage 取末帧并留下原文。
func TestReserve_StreamObservesTextAndUsage(t *testing.T) {
	col := &collector{}
	env := newReserveEnv(t, map[string]*model.ExternalModel{"gpt-x": gptModel("gpt-x", "openai/gpt-4o")},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		},
		func(context.Context, ReserveInput) (any, *Deny) { return nil, nil },
		col.add)

	resp, body := env.post(t, "/v1/chat/completions", `{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	seenAll := col.all()
	if len(seenAll) != 1 {
		t.Fatalf("结算回调次数=%d", len(seenAll))
	}
	res := seenAll[0]
	if !res.Streamed || !res.HasUsage || res.Usage.Output != 2 {
		t.Fatalf("流式结果=%+v", res)
	}
	if res.ObservedTextRunes != len([]rune("hello")) {
		t.Fatalf("流式生成内容字符数=%d", res.ObservedTextRunes)
	}
	if len(res.UsageRaw) == 0 {
		t.Fatalf("流式 usage 原文应留痕")
	}
}

// 价目表由模型带出（售价三段），magpie 只搬运。
func TestReserve_CarriesModelRates(t *testing.T) {
	m := gptModel("gpt-x", "openai/gpt-4o")
	m.SaleRates = billing.Rates{"input": 1000, "output": 2000, "cache_read": 100}
	var got ReserveInput
	env := newReserveEnv(t, map[string]*model.ExternalModel{"gpt-x": m},
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) },
		func(_ context.Context, in ReserveInput) (any, *Deny) { got = in; return nil, nil }, nil)

	env.post(t, "/v1/chat/completions", chatBody)
	if got.Rates["input"] != 1000 || got.Rates["output"] != 2000 {
		t.Fatalf("售价未带到预扣入参: %+v", got.Rates)
	}
}
