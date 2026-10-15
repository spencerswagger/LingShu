package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// AC-16：三协议 usage 归一化为五段；Anthropic 推理 token 并入 output（不单列 reasoning）。
func TestExtractUsage_ThreeProtocols(t *testing.T) {
	t.Run("openai-chat", func(t *testing.T) {
		body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":20},"completion_tokens_details":{"reasoning_tokens":10}}}`)
		u, err := NewOpenAIProvider(EndpointChatCompletions).ExtractUsage(body)
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		if u.Input != 100 || u.Output != 40 || u.CacheRead != 20 || u.Reasoning != 10 || u.CacheWrite != 0 {
			t.Fatalf("openai-chat 归一化错误: %+v", u)
		}
	})
	t.Run("openai-responses", func(t *testing.T) {
		body := []byte(`{"usage":{"input_tokens":200,"output_tokens":80,"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":20}}}`)
		u, err := NewResponsesProvider().ExtractUsage(body)
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		if u.Input != 200 || u.Output != 60 || u.CacheRead != 30 || u.Reasoning != 20 {
			t.Fatalf("openai-responses 归一化错误: %+v", u)
		}
	})
	t.Run("anthropic-messages", func(t *testing.T) {
		body := []byte(`{"usage":{"input_tokens":300,"output_tokens":90,"cache_read_input_tokens":40,"cache_creation_input_tokens":15}}`)
		u, err := NewAnthropicProvider().ExtractUsage(body)
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		if u.Input != 300 || u.Output != 90 || u.CacheRead != 40 || u.CacheWrite != 15 || u.Reasoning != 0 {
			t.Fatalf("anthropic-messages 归一化错误: %+v", u)
		}
	})
}

// AC-17 前置：usage 缺失时三协议均返回 ErrUsageMissing。
func TestExtractUsage_Missing(t *testing.T) {
	noUsage := []byte(`{"id":"x","choices":[]}`)
	for _, p := range []Provider{NewOpenAIProvider(EndpointChatCompletions), NewResponsesProvider(), NewAnthropicProvider()} {
		if _, err := p.ExtractUsage(noUsage); !errors.Is(err, ErrUsageMissing) {
			t.Errorf("%s usage 缺失应返回 ErrUsageMissing, got %v", p.Name(), err)
		}
	}
}

// postWithHeaders 用自定义头发起一次 /v1 请求（用于 x-api-key 入站鉴权）。
func postWithHeaders(t *testing.T, gw *Gateway, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)
	return rec
}

// AC-4/AC-18：模型级 protocols 只含 anthropic-messages 时，openai-chat 入站返回固定错误且不发起上游请求。
func TestServeUnsupportedProtocol(t *testing.T) {
	var called int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	// 模型级可达协议子集仅 anthropic-messages。
	h.chans.mrt.Model.Protocols = channelprotocol.Protocols{channelprotocol.ProtocolAnthropicMessages: {}}

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("直通未命中 want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	rb := decodeOpenAIError(t, rec)
	if rb.Error.Type != "unsupported_protocol" || rb.Error.Code != "invalid_request_error" {
		t.Errorf("固定错误形状错误: %+v", rb.Error)
	}
	if called != 0 {
		t.Fatalf("协议不支持不应发起上游请求, upstream calls=%d", called)
	}
}

// AC-2：Anthropic 入站（/v1/messages）→ 上游路径 /v1/messages，入站鉴权兼容 x-api-key。
func TestServeAnthropicMessages(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.chans.rt.Channel.Protocol = channelprotocol.ProtocolAnthropicMessages

	rec := postWithHeaders(t, h.gw, "/v1/messages", `{"model":"gpt-4","stream":false}`,
		map[string]string{"x-api-key": "sk-gw-t"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Errorf("上游路径 want /v1/messages, got %q", gotPath)
	}
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Tokens.Input != 10 || h.billing.reqs[0].Tokens.Output != 5 {
		t.Fatalf("anthropic usage 计费错误: %+v", h.billing.reqs)
	}
}

// AC-3：OpenAI Responses 入站（/v1/responses）→ 上游路径 /responses。
func TestServeOpenAIResponses(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp","output":[],"usage":{"input_tokens":12,"output_tokens":6}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.chans.rt.Channel.Protocol = channelprotocol.ProtocolOpenAIResponses

	rec := doRequest(t, h.gw, "/v1/responses", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/responses" {
		t.Errorf("上游路径 want /responses, got %q", gotPath)
	}
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Tokens.Input != 12 || h.billing.reqs[0].Tokens.Output != 6 {
		t.Fatalf("responses usage 计费错误: %+v", h.billing.reqs)
	}
}

// AC-10/AC-11：全局响应头白名单默认空（不透传上游头）；命中白名单后透传。
func TestResponseHeaderWhitelist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Trace", "trace-abc")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
	}))
	defer upstream.Close()

	// 默认白名单为空 → 上游 X-Trace 不透传（AC-10）。
	h := build(t, upstream.URL)
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Trace"); got != "" {
		t.Errorf("白名单为空不应透传上游头, got %q", got)
	}

	// 白名单含 X-Trace → 透传（AC-11）。
	h2 := build(t, upstream.URL)
	h2.gw.upstreamRespHeaders = func() []string { return []string{"X-Trace"} }
	rec2 := doRequest(t, h2.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if got := rec2.Header().Get("X-Trace"); got != "trace-abc" {
		t.Errorf("白名单命中应透传 X-Trace, got %q", got)
	}
}

// 渠道协议引用时按协议 protocols 判定直通（渠道内联 protocol 被忽略）。
func TestServeChannelProtocolProtocols(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp","output":[],"usage":{"input_tokens":7,"output_tokens":3}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	// 渠道协议：仅支持 openai-responses，且为该协议指定自定义 path。
	h.chans.rt.Channel.Protocol = "openai-compat" // 内联字段应被忽略
	h.chans.rt.Protocol = &channelprotocol.ChannelProtocol{
		Protocols: channelprotocol.Protocols{
			channelprotocol.ProtocolOpenAIResponses: {Path: "/custom/responses"},
		},
	}

	rec := doRequest(t, h.gw, "/v1/responses", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/custom/responses" {
		t.Errorf("上游路径应走协议 path 覆盖, got %q", gotPath)
	}
}

// 响应头白名单过滤 hop-by-hop/Content-Length：即使被误列入白名单也不透传。
func TestApplyResponseHeaderWhitelist_FiltersHopByHop(t *testing.T) {
	upstream := http.Header{}
	upstream.Set("Content-Length", "123")
	upstream.Set("Connection", "keep-alive")
	upstream.Set("X-Trace", "trace-abc")

	rec := httptest.NewRecorder()
	applyResponseHeaderWhitelist(rec, upstream, []string{"Content-Length", "Connection", "X-Trace"})

	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length 不应透传, got %q", got)
	}
	if got := rec.Header().Get("Connection"); got != "" {
		t.Errorf("Connection 不应透传, got %q", got)
	}
	if got := rec.Header().Get("X-Trace"); got != "trace-abc" {
		t.Errorf("X-Trace 应透传, got %q", got)
	}
}

// 内联渠道 protocol 为空 → 回落 openai-chat，避免键为 "" 导致全部入站协议被判不支持。
func TestServeInlineEmptyProtocolFallsBack(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.chans.rt.Channel.Protocol = ""

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("空 protocol 应回落 openai-chat, want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/chat/completions" {
		t.Errorf("上游路径 want /chat/completions, got %q", gotPath)
	}
}
