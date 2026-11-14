package magpie

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/model"
)

// capturedRequest 记录上游实际收到的请求（断言「只换凭证头 + model」的唯一真相源）。
type capturedRequest struct {
	mu      sync.Mutex
	path    string
	query   string
	headers http.Header
	body    []byte
	count   int
}

func (c *capturedRequest) snapshot() capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return capturedRequest{path: c.path, query: c.query, headers: c.headers.Clone(), body: append([]byte(nil), c.body...), count: c.count}
}

type testEnv struct {
	upstream *httptest.Server
	gateway  *httptest.Server
	captured *capturedRequest
	results  []Result
	mu       sync.Mutex
}

// newTestEnv 组装「假 magpie 上游 + 灵枢 facade」两段式环境。
func newTestEnv(t *testing.T, cfg Config, models map[string]*model.ExternalModel, upstream http.HandlerFunc) *testEnv {
	t.Helper()
	env := &testEnv{captured: &capturedRequest{}}
	env.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		env.captured.mu.Lock()
		env.captured.path = r.URL.Path
		env.captured.query = r.URL.RawQuery
		env.captured.headers = r.Header.Clone()
		env.captured.body = body
		env.captured.count++
		env.captured.mu.Unlock()
		upstream(w, r)
	}))
	t.Cleanup(env.upstream.Close)

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
		OnComplete: func(_ context.Context, res Result) {
			env.mu.Lock()
			env.results = append(env.results, res)
			env.mu.Unlock()
		},
	})
	if h == nil {
		t.Fatal("handler 未装配（BaseURL 空？）")
	}
	mux := http.NewServeMux()
	h.Mount(mux)
	env.gateway = httptest.NewServer(mux)
	t.Cleanup(env.gateway.Close)
	return env
}

// lastResult 取最后一次转发结果。
func (e *testEnv) lastResult(t *testing.T) Result {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.results) == 0 {
		t.Fatal("期望至少一次转发结果")
	}
	return e.results[len(e.results)-1]
}

func baseTestConfig() Config {
	return Config{Request: 5 * time.Second, StreamFirstByte: 2 * time.Second, StreamIdle: 5 * time.Second, CountTokens: 2 * time.Second, Connect: 2 * time.Second}
}

func gptModel(name, magpieModel string) *model.ExternalModel {
	return &model.ExternalModel{ID: 1, ExternalName: name, Enabled: true, MagpieModel: mp(magpieModel)}
}

// doPost 向 facade 发请求（带调用令牌）。
func (e *testEnv) doPost(t *testing.T, path, body string, extra map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.gateway.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer tenant-token")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := e.gateway.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return resp, got
}

// TestForward_OpenAIChat_SwapsOnlyCredentialAndModel 是「只换两样东西」的核心断言：
// 出站凭证头换成 callerKey、model 换成 magpie_model，其余请求头与请求体逐字节不变；
// 响应体与状态码逐字节原样回流，usage 只在旁路被读到。
func TestForward_OpenAIChat_SwapsOnlyCredentialAndModel(t *testing.T) {
	upstreamBody := `{"id":"chatcmpl-1","object":"chat.completion","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":30}}}`
	env := newTestEnv(t, baseTestConfig(),
		map[string]*model.ExternalModel{"tenant-gpt": gptModel("tenant-gpt", "upstream/gpt-4o")},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Upstream-Trace", "trace-1")
			_, _ = io.WriteString(w, upstreamBody)
		})

	reqBody := `{"model":"tenant-gpt","messages":[{"role":"user","content":"hi"}],"stream":false}`
	resp, got := env.doPost(t, "/v1/chat/completions", reqBody, map[string]string{
		"anthropic-version": "2023-06-01",
		"X-Forwarded-For":   "1.2.3.4",
		"Accept-Encoding":   "gzip",
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, 期望 200；body=%s", resp.StatusCode, got)
	}
	// 响应体逐字节原样（不改写、不重新序列化）。
	if string(got) != upstreamBody {
		t.Fatalf("下游响应体被改写\n got: %s\nwant: %s", got, upstreamBody)
	}
	if resp.Header.Get("X-Upstream-Trace") != "trace-1" {
		t.Fatal("上游自定义响应头必须透传")
	}

	up := env.captured.snapshot()
	// 1) 只换凭证头：换成本令牌的 callerKey，且不带上游的 Bearer 令牌。
	if got := up.headers.Get("Authorization"); got != "Bearer caller-secret" {
		t.Fatalf("出站 Authorization = %q, 期望 Bearer caller-secret", got)
	}
	// 2) 其它头透传。
	if got := up.headers.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version 应透传，实际 %q", got)
	}
	// 3) 链路头一律不带（强制黑名单）。
	if got := up.headers.Get("X-Forwarded-For"); got != "" {
		t.Fatalf("x-forwarded-for 不应上游，实际 %q", got)
	}
	// 4) 固定 identity（流式不能二次压缩）。
	if got := up.headers.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("Accept-Encoding = %q, 期望 identity", got)
	}
	// 5) 只改 model：把期望值的 model 替换回来，应与原请求体逐字节相同。
	wantUpstreamBody := strings.Replace(reqBody, `"tenant-gpt"`, `"upstream/gpt-4o"`, 1)
	if string(up.body) != wantUpstreamBody {
		t.Fatalf("出站请求体不符\n got: %s\nwant: %s", up.body, wantUpstreamBody)
	}
	// 6) 不注入 include_usage（契约硬要求）。
	if bytes.Contains(up.body, []byte("include_usage")) {
		t.Fatal("不得注入 include_usage")
	}

	// 7) usage 旁路读到（三段归一）。
	res := env.lastResult(t)
	if !res.HasUsage || res.Usage != (Usage{Input: 70, CacheRead: 30, Output: 40}) {
		t.Fatalf("usage = %+v (has=%v), 期望 Input70/CacheRead30/Output40", res.Usage, res.HasUsage)
	}
	if res.Status != 200 || res.Billable != true || res.Canceled {
		t.Fatalf("result 标记不符: %+v", res)
	}
}

// TestForward_Anthropic_And_Gemini_Rewrite 断言另外两个协议的 model 改写落点。
func TestForward_Anthropic_And_Gemini_Rewrite(t *testing.T) {
	t.Run("Anthropic 改 body.model", func(t *testing.T) {
		env := newTestEnv(t, baseTestConfig(),
			map[string]*model.ExternalModel{"tenant-claude": gptModel("tenant-claude", "anthropic/claude-sonnet-4")},
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"type":"message","usage":{"input_tokens":10,"output_tokens":2}}`)
			})
		reqBody := `{"model":"tenant-claude","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
		resp, body := env.doPost(t, "/v1/messages", reqBody, map[string]string{"x-api-key": "ignored-by-test"})
		if resp.StatusCode != 200 {
			t.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
		up := env.captured.snapshot()
		if up.path != "/v1/messages" {
			t.Fatalf("上游 path = %q", up.path)
		}
		want := strings.Replace(reqBody, "tenant-claude", "anthropic/claude-sonnet-4", 1)
		if string(up.body) != want {
			t.Fatalf("出站体不符\n got: %s\nwant: %s", up.body, want)
		}
		if up.headers.Get("Authorization") != "Bearer caller-secret" {
			t.Fatalf("凭据头未替换: %q", up.headers.Get("Authorization"))
		}
	})

	t.Run("Gemini 改 path 的 model 段并保留 action/查询串", func(t *testing.T) {
		env := newTestEnv(t, baseTestConfig(),
			map[string]*model.ExternalModel{"tenant-gemini": gptModel("tenant-gemini", "upstream/gemini-2.0-flash")},
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"candidates":[],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":3}}`)
			})
		reqBody := `{"contents":[{"parts":[{"text":"hi"}]}]}`
		resp, body := env.doPost(t, "/v1beta/models/tenant-gemini:generateContent?alt=sse", reqBody, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
		up := env.captured.snapshot()
		if up.path != "/v1beta/models/upstream/gemini-2.0-flash:generateContent" {
			t.Fatalf("上游 path = %q", up.path)
		}
		if up.query != "alt=sse" {
			t.Fatalf("查询串应原样保留，实际 %q", up.query)
		}
		// Gemini 的 body 里没有 model，必须逐字节原样。
		if string(up.body) != reqBody {
			t.Fatalf("Gemini 请求体不得改动\n got: %s\nwant: %s", up.body, reqBody)
		}
	})
}

// TestForward_UpstreamErrorPassthrough 断言上游 5xx **原样**回流（不二次包装）。
func TestForward_UpstreamErrorPassthrough(t *testing.T) {
	upstreamErr := `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	env := newTestEnv(t, baseTestConfig(),
		map[string]*model.ExternalModel{"tenant-gpt": gptModel("tenant-gpt", "upstream/gpt-4o")},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, upstreamErr)
		})
	resp, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[]}`, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, 期望原样 503", resp.StatusCode)
	}
	if string(body) != upstreamErr {
		t.Fatalf("上游错误体被改写\n got: %s\nwant: %s", body, upstreamErr)
	}
	res := env.lastResult(t)
	if res.Status != 503 || res.HasUsage {
		t.Fatalf("result = %+v", res)
	}
}

// newHandlerWithBase 直接用给定基址装配 handler（连不上/超时类用例需要指定真实不可达地址）。
func newHandlerWithBase(t *testing.T, base string, models map[string]*model.ExternalModel) http.Handler {
	t.Helper()
	cipher, err := encryptForTest("caller-secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	h := NewHandler(Deps{
		Config: Config{
			BaseURL:         base,
			Request:         2 * time.Second,
			StreamFirstByte: time.Second,
			StreamIdle:      time.Second,
			CountTokens:     time.Second,
			Connect:         500 * time.Millisecond,
		},
		Tokens:        &fakeTokens{tok: activeToken()},
		Models:        &fakeModels{byName: models},
		Keys:          &fakeKeys{key: &Key{ID: 1, TokenID: 7, KeyName: "core-k1", KeyCipher: cipher, State: KeyStateActive}},
		CredentialKey: testCredKey,
		Prefixes:      NewPrefixProvider(nil),
	})
	if h == nil {
		t.Fatal("handler 未装配")
	}
	mux := http.NewServeMux()
	h.Mount(mux)
	return mux
}

// TestForward_ConnectFailure_Maps503 断言连不上代理侧 → 503 + Retry-After（可重试）。
func TestForward_ConnectFailure_Maps503(t *testing.T) {
	// 占一个端口再立刻释放，确保连接被拒（而不是挂起）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	srv := httptest.NewServer(newHandlerWithBase(t, "http://"+addr,
		map[string]*model.ExternalModel{"tenant-gpt": gptModel("tenant-gpt", "upstream/gpt-4o")}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"tenant-gpt","messages":[]}`))
	req.Header.Set("Authorization", "Bearer tenant-token")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, 期望 503；body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("Retry-After = %q, 期望 5", resp.Header.Get("Retry-After"))
	}
	var shape struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatalf("错误体不是 JSON: %s", body)
	}
	if shape.Error.Code != CodeServiceUnavailable {
		t.Fatalf("code = %q, 期望 %q", shape.Error.Code, CodeServiceUnavailable)
	}
}

// TestForward_FirstByteTimeout_Maps504 断言首包超时 → 504（协议形状错误体）。
func TestForward_FirstByteTimeout_Maps504(t *testing.T) {
	cfg := baseTestConfig()
	cfg.StreamFirstByte = 50 * time.Millisecond
	env := newTestEnv(t, cfg,
		map[string]*model.ExternalModel{"tenant-gpt": gptModel("tenant-gpt", "upstream/gpt-4o")},
		func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(400 * time.Millisecond):
			case <-r.Context().Done():
			}
			w.WriteHeader(http.StatusOK)
		})
	resp, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","stream":true,"messages":[]}`, nil)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, 期望 504；body=%s", resp.StatusCode, body)
	}
	res := env.lastResult(t)
	if !res.TimedOut {
		t.Fatalf("result 应标记 TimedOut: %+v", res)
	}
}

// TestForward_StreamPassthrough 断言 SSE 逐块透传（字节不变）且 usage 取自末帧。
func TestForward_StreamPassthrough(t *testing.T) {
	frames := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":2}}\n\n",
		"data: [DONE]\n\n",
	}
	env := newTestEnv(t, baseTestConfig(),
		map[string]*model.ExternalModel{"tenant-gpt": gptModel("tenant-gpt", "upstream/gpt-4o")},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			fl, _ := w.(http.Flusher)
			for _, f := range frames {
				_, _ = io.WriteString(w, f)
				if fl != nil {
					fl.Flush()
				}
			}
		})
	resp, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","stream":true,"messages":[]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if want := strings.Join(frames, ""); string(body) != want {
		t.Fatalf("SSE 透传被改写\n got: %q\nwant: %q", body, want)
	}
	if resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatal("上游响应头应透传")
	}
	res := env.lastResult(t)
	if !res.Streamed || !res.HasUsage || res.Usage != (Usage{Input: 12, Output: 2}) {
		t.Fatalf("result = %+v, 期望 Streamed 且 usage 12/2", res)
	}
}

// TestForward_CountTokensNotBillable 断言计数端点不计费、不产出结果（第③件不会为它扣费）。
func TestForward_CountTokensNotBillable(t *testing.T) {
	env := newTestEnv(t, baseTestConfig(),
		map[string]*model.ExternalModel{"tenant-claude": gptModel("tenant-claude", "anthropic/claude-sonnet-4")},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens":7}`)
		})
	resp, body := env.doPost(t, "/v1/messages/count_tokens", `{"model":"tenant-claude","messages":[]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	env.mu.Lock()
	n := len(env.results)
	env.mu.Unlock()
	if n != 0 {
		t.Fatalf("计数端点不应产出计费结果，实际 %d 条", n)
	}
}

// TestHandler_ModelsListAndAuth 断言 /v1/models 的鉴权与「列出即可用」口径。
func TestHandler_ModelsListAndAuth(t *testing.T) {
	env := newTestEnv(t, baseTestConfig(), map[string]*model.ExternalModel{}, func(w http.ResponseWriter, _ *http.Request) {})
	// 未带凭据 → 401。
	resp, err := env.gateway.Client().Get(env.gateway.URL + "/v1/models")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无凭据 status = %d, 期望 401", resp.StatusCode)
	}
}

// TestNewHandler_DisabledWhenNoBaseURL 断言未配置基址时保持 410 停用口径（返回 nil handler）。
func TestNewHandler_DisabledWhenNoBaseURL(t *testing.T) {
	h := NewHandler(Deps{Config: Config{}, Tokens: &fakeTokens{}, Models: &fakeModels{}, Keys: &fakeKeys{}})
	if h != nil {
		t.Fatal("未配置 magpie.base_url 时不应装配 handler")
	}
}
