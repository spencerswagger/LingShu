package magpie

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// D12 裁定：响应体必须**字节原样**，只允许剔 hop-by-hop / 上游标识头。
// 禁止按内容白名单、字段裁剪、文案映射。
//
// 所以这里的断言方式是「随机生成 + 逐字节相等」，不是「包含某片段」：
// 前者能挡住悄悄改写（比如把上游错误体换成自家文案、把 JSON 重新序列化），
// 后者挡不住。随机化覆盖：长度、字段名、嵌套层级、编码形状。

// randomJSON 生成随机嵌套 JSON（字段名随机、层级随机、长度随机）。
func randomJSON(rng *rand.Rand, depth int) any {
	if depth <= 0 {
		switch rng.Intn(4) {
		case 0:
			return rng.Int63()
		case 1:
			return randomString(rng, rng.Intn(40))
		case 2:
			return rng.Intn(2) == 0
		default:
			return nil
		}
	}
	kind := rng.Intn(3)
	switch kind {
	case 0: // 对象
		n := rng.Intn(4)
		obj := make(map[string]any, n)
		for i := 0; i < n; i++ {
			obj[randomString(rng, 1+rng.Intn(8))] = randomJSON(rng, depth-1)
		}
		return obj
	case 1: // 数组
		n := rng.Intn(4)
		arr := make([]any, 0, n)
		for i := 0; i < n; i++ {
			arr = append(arr, randomJSON(rng, depth-1))
		}
		return arr
	default: // 标量
		return randomJSON(rng, 0)
	}
}

// randomString 生成随机 ASCII 串（含引号/反斜杠/中文，考验编码不被改写）。
func randomString(rng *rand.Rand, n int) string {
	const alphabet = `abcdefghijklmnopqrstuvwxyz0123456789"\'/ <>{}$%&*()[]#@!?中文测试`
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(alphabet[rng.Intn(len(alphabet))])
	}
	return b.String()
}

// upstreamBodies 返回 8 类形状的响应体（每类随机生成，长度/字段名/层级都随机）。
func upstreamBodies(t *testing.T, rng *rand.Rand) map[string][]byte {
	t.Helper()
	encode := func(v any) []byte {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化随机体失败: %v", err)
		}
		return raw
	}
	// 大体积随机体（~200KB），考验分块读写不截断。
	big := bytes.Repeat([]byte("abcdefghij"), 20_000)
	return map[string][]byte{
		"openai_error": encode(map[string]any{
			"error": map[string]any{
				"message": randomString(rng, 200), "type": randomString(rng, 12),
				"param": randomJSON(rng, 2), "code": randomString(rng, 8),
			},
			"request_id": randomString(rng, 24),
			"extra":      randomJSON(rng, 3),
		}),
		"anthropic_error": encode(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": randomString(rng, 20), "message": randomString(rng, 300)},
		}),
		"gemini_error": encode([]any{map[string]any{
			"error": map[string]any{
				"code": rng.Intn(600), "message": randomString(rng, 120),
				"status": randomString(rng, 10),
			},
		}}),
		"nested_deep": encode(randomJSON(rng, 6)),
		"plain_text":  []byte(randomString(rng, 5000)),
		"html_page": []byte("<!DOCTYPE html><html><body><h1>" + randomString(rng, 100) +
			"</h1><script>var x=\"" + randomString(rng, 200) + "\";</script></body></html>"),
		"binaryish": big,
		"empty":     {},
	}
}

// TestForward_ResponseBytesIdentical 是 D12 的核心断言：随机响应体逐字节相等。
func TestForward_ResponseBytesIdentical(t *testing.T) {
	rng := rand.New(rand.NewSource(20261115))
	bodies := upstreamBodies(t, rng)

	for name, body := range bodies {
		for round := 0; round < 3; round++ {
			t.Run(fmt.Sprintf("%s/round%d", name, round), func(t *testing.T) {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// 上游故意给一堆「必须被剔」的头 + 一个要保留的。
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.Header().Set("x-magpie-provider", "vendor-x")
					w.Header().Set("x-magpie-request-id", "up-123")
					w.Header().Set("server", "magpie/1.0")
					w.Header().Set("x-powered-by", "magpie")
					w.Header().Set("x-provider-note", "served by magpie")
					w.Header().Set("x-request-id", "upstream-rid")
					w.Header().Set("anthropic-ratelimit-requests-limit", "50")
					w.WriteHeader(http.StatusTeapot)
					_, _ = w.Write(body)
				}))
				defer up.Close()

				f := NewForwarder()
				rec := httptest.NewRecorder()
				res := f.Forward(t.Context(), rec, ForwardRequest{
					BaseURL:    up.URL,
					Path:       "/v1/chat/completions",
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       []byte(`{"model":"m","messages":[]}`),
					RequestID:  "our-rid",
					Billable:   false,
					Protocol:   ProtocolOpenAIChat,
					Credential: func(h http.Header) { h.Set("Authorization", "Bearer secret") },
				}, Timeouts{StreamFirstByte: 5 * time.Second, StreamIdle: 5 * time.Second})

				if res.Err != nil {
					t.Fatalf("转发失败: %v", res.Err)
				}
				if rec.Code != http.StatusTeapot {
					t.Fatalf("状态码必须原样：got %d", rec.Code)
				}
				if !bytes.Equal(rec.Body.Bytes(), body) {
					t.Fatalf("响应体必须逐字节相等：got %d 字节 want %d 字节（首个差异位置 %d）",
						rec.Body.Len(), len(body), firstDiff(rec.Body.Bytes(), body))
				}
				// 头口径（F8）：上游标识头 / server / 供应商名 一律不许漏。
				for _, h := range []string{"x-magpie-provider", "x-magpie-request-id", "server",
					"x-powered-by", "x-provider-note"} {
					if v := rec.Header().Get(h); v != "" {
						t.Fatalf("头 %s 应被剔除，got %q", h, v)
					}
				}
				// 保留上游给的限流头（不是标识头，运营要看）。
				if rec.Header().Get("anthropic-ratelimit-requests-limit") != "50" {
					t.Fatalf("非标识头应透传")
				}
				// x-request-id 必须是灵枢自己的。
				if got := rec.Header().Get("x-request-id"); got != "our-rid" {
					t.Fatalf("x-request-id 应为灵枢自己的（our-rid），got %q", got)
				}
			})
		}
	}
}

// TestForward_StreamResponseBytesIdentical 断言 SSE 流式响应同样逐字节相等，且 usage 被读到。
func TestForward_StreamResponseBytesIdentical(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	// 随机内容 + 一段固定 usage 帧（usage 必须能解析出来）。
	var sse bytes.Buffer
	for i := 0; i < 5; i++ {
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": randomString(rng, 20)}}},
		})
		sse.WriteString("data: " + string(payload) + "\n\n")
	}
	sse.WriteString(`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}` + "\n\n")
	sse.WriteString("data: [DONE]\n\n")
	body := sse.Bytes()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-magpie-model", "up-model")
		w.Header().Set("x-request-id", "up-rid")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < len(body); i += 37 { // 故意分包，考验跨包解析
			end := i + 37
			if end > len(body) {
				end = len(body)
			}
			_, _ = w.Write(body[i:end])
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer up.Close()

	f := NewForwarder()
	rec := httptest.NewRecorder()
	res := f.Forward(t.Context(), rec, ForwardRequest{
		BaseURL: up.URL, Path: "/v1/chat/completions", Header: http.Header{},
		Body: []byte(`{"model":"m","stream":true}`), RequestID: "our-rid",
		Streaming: true, Billable: true, Protocol: ProtocolOpenAIChat,
	}, Timeouts{StreamFirstByte: 5 * time.Second, StreamIdle: 5 * time.Second})

	if res.Err != nil {
		t.Fatalf("转发失败: %v", res.Err)
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("SSE 响应体必须逐字节相等：got %d want %d", rec.Body.Len(), len(body))
	}
	if !res.HasUsage || res.Usage.Input != 11 || res.Usage.Output != 7 {
		t.Fatalf("usage 应被旁路读到：%+v has=%v", res.Usage, res.HasUsage)
	}
	if rec.Header().Get("x-magpie-model") != "" {
		t.Fatal("上游标识头必须剔除")
	}
}

// TestForward_QueryPassthroughExceptGeminiKey 覆盖 F6：query 原样，只有 ?key= 被剔除。
func TestForward_QueryPassthroughExceptGeminiKey(t *testing.T) {
	var gotQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	f := NewForwarder()
	cases := []struct {
		name  string
		query string
		strip bool
		want  string
	}{
		{"原样保留", "alt=sse&prettyPrint=false&x=%2F", false, "alt=sse&prettyPrint=false&x=%2F"},
		{"剔 Gemini key", "key=secret&alt=sse", true, "alt=sse"},
		{"只剔 key 本身", "key=secret", true, ""},
		{"不 strip 时 key 也原样", "key=secret&alt=sse", false, "key=secret&alt=sse"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if res := f.Forward(t.Context(), rec, ForwardRequest{
				BaseURL: up.URL, Path: "/v1beta/models/m:generateContent", Query: c.query,
				StripQueryKey: c.strip, Header: http.Header{}, Body: []byte(`{}`),
				RequestID: "rid", Protocol: ProtocolGemini,
			}, Timeouts{StreamFirstByte: 5 * time.Second, StreamIdle: 5 * time.Second}); res.Err != nil {
				t.Fatalf("转发失败: %v", res.Err)
			}
			if gotQuery != c.want {
				t.Fatalf("query 不符：got %q want %q", gotQuery, c.want)
			}
		})
	}
}

// TestForward_HooksOrderAndAttribution 断言 D1 的两个钩子：首字节给归因、结束给 usage。
func TestForward_HooksOrderAndAttribution(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-magpie-provider", "vendor-x")
		w.Header().Set("x-magpie-account", "acct-1")
		w.Header().Set("x-magpie-model", "up-model")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":4},"choices":[]}`))
	}))
	defer up.Close()

	var order []string
	var facts ResponseFacts
	var final Result

	f := NewForwarder()
	rec := httptest.NewRecorder()
	f.Forward(t.Context(), rec, ForwardRequest{
		BaseURL: up.URL, Path: "/v1/chat/completions", Header: http.Header{},
		Body: []byte(`{"model":"m"}`), RequestID: "rid", Billable: true,
		Protocol: ProtocolOpenAIChat,
		OnUpstreamResponse: func(_ context.Context, ff ResponseFacts) {
			facts = ff
			order = append(order, "response")
		},
		OnComplete: func(_ context.Context, r Result) {
			final = r
			order = append(order, "done")
		},
	}, Timeouts{StreamFirstByte: 5 * time.Second, StreamIdle: 5 * time.Second})

	if len(order) != 2 || order[0] != "response" || order[1] != "done" {
		t.Fatalf("钩子顺序应为 response → done，got %v", order)
	}
	if facts.Boundary.Provider != "vendor-x" {
		t.Fatalf("首字节钩子应拿到归因，got %+v", facts.Boundary)
	}
	if final.Usage.Input != 3 || final.Usage.Output != 4 {
		t.Fatalf("结束时的 usage 不符：%+v", final.Usage)
	}
	if final.Boundary.Account != "acct-1" {
		t.Fatalf("归因应在结束时也带回来，got %+v", final.Boundary)
	}
	if rec.Header().Get("x-magpie-provider") != "" {
		t.Fatal("归因头读完后必须剥掉")
	}
}

// firstDiff 返回两段字节的首个差异位置（排障用；相同返回 -1）。
func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

// TestForward_HookBeforeFirstByteAndRequestIDFallback 覆盖 Ark §9.2.2 的时序：
// OnUpstreamResponse 必须在**任何字节写下游之前**同步返回；且 ctx 没链路 ID 时也要写我们自己的。
func TestForward_HookBeforeFirstByteAndRequestIDFallback(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-request-id", "upstream-rid")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	f := NewForwarder()
	rec := httptest.NewRecorder()
	hookSawWritten := -1
	res := f.Forward(t.Context(), rec, ForwardRequest{
		BaseURL: up.URL, Path: "/v1/chat/completions", Header: http.Header{},
		Body: []byte(`{"model":"m"}`), Billable: false, Protocol: ProtocolOpenAIChat,
		// RequestID 故意留空：模拟 ctx 没有链路 ID 的情形。
		OnUpstreamResponse: func(_ context.Context, _ ResponseFacts) {
			hookSawWritten = rec.Body.Len() // 钩子运行时下游必须还没有任何字节
			if rec.Code != http.StatusOK {
				// 记录器在 WriteHeader 前 Code 默认 200，用 body 长度作为「是否已写」的判据。
				return
			}
		},
	}, Timeouts{StreamFirstByte: 5 * time.Second, StreamIdle: 5 * time.Second})

	if res.Err != nil {
		t.Fatalf("转发失败: %v", res.Err)
	}
	if hookSawWritten != 0 {
		t.Fatalf("首字节钩子必须在写下游之前同步返回，钩子内已写 %d 字节", hookSawWritten)
	}
	got := rec.Header().Get("x-request-id")
	if got == "" {
		t.Fatal("ctx 无链路 ID 时也必须给下游生成一个（不能留空、更不许透传上游值）")
	}
	if got == "upstream-rid" {
		t.Fatal("上游的 x-request-id 不许透传")
	}
	if res.RequestID != got {
		t.Fatalf("Result.RequestID 应与下游一致：got %q want %q", res.RequestID, got)
	}
}

// TestForward_VendorIdentityHeadersDropped 是 F8 四类头的单元级覆盖（端到端在 gateway 包）。
func TestForward_VendorIdentityHeadersDropped(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("server", "magpie/1.0")
		h.Set("x-magpie-route", "r1")
		h.Set("x-magpie-account", "acct")
		h.Set("x-upstream-model", "m")
		h.Set("x-provider", "openai")                     // 名带供应商
		h.Set("x-served-by", "some-vendor")               // 值带供应商
		h.Set("x-note", "powered by anthropic")           // 值带供应商
		h.Set("x-account-owner", "acme")                  // 身份类头名
		h.Set("anthropic-ratelimit-requests-limit", "50") // 该透传
		h.Set("x-request-id", "up-rid")
		h.Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	f := NewForwarder()
	rec := httptest.NewRecorder()
	f.Forward(t.Context(), rec, ForwardRequest{
		BaseURL: up.URL, Path: "/v1/chat/completions", Header: http.Header{},
		Body: []byte(`{"model":"m"}`), RequestID: "our-rid", Protocol: ProtocolOpenAIChat,
	}, Timeouts{StreamFirstByte: 5 * time.Second, StreamIdle: 5 * time.Second})

	for _, h := range []string{"server", "x-magpie-route", "x-magpie-account", "x-upstream-model",
		"x-provider", "x-served-by", "x-note", "x-account-owner"} {
		if v := rec.Header().Get(h); v != "" {
			t.Fatalf("F8 要求剔除 %s，仍漏出 %q", h, v)
		}
	}
	if rec.Header().Get("anthropic-ratelimit-requests-limit") != "50" {
		t.Fatal("限流类可观测头不该被过度剥除")
	}
	if rec.Header().Get("x-request-id") != "our-rid" {
		t.Fatalf("x-request-id 必须是灵枢自己的，got %q", rec.Header().Get("x-request-id"))
	}
}
