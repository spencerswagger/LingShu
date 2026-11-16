package magpie

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
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
					w.Header().Set("x-ratelimit-limit", "50")
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
				// 通用限流头（头名不含供应商名）必须透传——证明不是「一刀切全剥」。
				if rec.Header().Get("x-ratelimit-limit") != "50" {
					t.Fatalf("通用限流头应透传")
				}
				// 供应商前缀的限流头按 F8 原意一并剥掉（身份泄漏不可逆，取舍见 headers.go）。
				if v := rec.Header().Get("anthropic-ratelimit-requests-limit"); v != "" {
					t.Fatalf("头名含供应商名的头应剔除，got %q", v)
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

// TestForward_QueryPassthroughExceptKey 覆盖 F6（安全默収紧版）：query 逐字原样，
// **任何路径**上的 `key` 参数一律剔除（不再对 Gemini 例外），剔完为空不留尾部 `?`。
func TestForward_QueryPassthroughExceptKey(t *testing.T) {
	var gotQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	f := NewForwarder()
	cases := []struct {
		name  string
		path  string
		query string
		want  string
	}{
		{"原样保留", "/v1/chat/completions", "alt=sse&prettyPrint=false&x=%2F", "alt=sse&prettyPrint=false&x=%2F"},
		{"Gemini 原生也剔", "/v1beta/models/m:generateContent", "key=secret&alt=sse", "alt=sse"},
		{"只剔 key 本身（不留 ?）", "/v1beta/models/m:generateContent", "key=secret", ""},
		{"非 Gemini 路径同样剔", "/v1/chat/completions", "key=secret&alt=sse", "alt=sse"},
		{"key 在中间也剔", "/v1/messages", "a=1&key=secret&b=2", "a=1&b=2"},
		{"大写 K 不是 key（不误剔）", "/v1/messages", "Key=secret", "Key=secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if res := f.Forward(t.Context(), rec, ForwardRequest{
				BaseURL: up.URL, Path: c.path, Query: c.query,
				Header: http.Header{}, Body: []byte(`{}`),
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
		h.Set("x-provider", "openai")           // 名带供应商
		h.Set("x-served-by", "some-vendor")     // 值带供应商
		h.Set("x-note", "powered by anthropic") // 值带供应商
		h.Set("x-account-owner", "acme")        // 身份类头名
		h.Set("x-ratelimit-limit", "50")        // 通用限流头：该透传
		h.Set("X-OpenAI-Organization", "org-1") // 头名含供应商名：Gate 实测漏出的三类之一
		h.Set("OpenAI-Processing-Ms", "12")
		h.Set("OpenAI-Version", "2020-10-01")
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
	if rec.Header().Get("x-ratelimit-limit") != "50" {
		t.Fatal("通用限流头不该被剥（证明不是一刀切）")
	}
	for _, h := range []string{"anthropic-ratelimit-requests-limit", "X-OpenAI-Organization",
		"OpenAI-Processing-Ms", "OpenAI-Version"} {
		if v := rec.Header().Get(h); v != "" {
			t.Fatalf("头名含供应商名的头必须剔除，%s 仍漏出 %q", h, v)
		}
	}
	if rec.Header().Get("x-request-id") != "our-rid" {
		t.Fatalf("x-request-id 必须是灵枢自己的，got %q", rec.Header().Get("x-request-id"))
	}
}

// TestShouldDropResponseHeader_F8Classes 是 F8 的**判定级**守卫表：
// 「头名或头值含供应商/渠道/账号名一律剔」（子串、大小写不敏感）。
//
// Gate 实测漏出的三类（`X-OpenAI-Organization` / `OpenAI-Processing-Ms` / `OpenAI-Version`）
// 必须在这里被显式钉住：它们的值是普通串、也不是 `x-magpie-` 前缀，
// 只按「值 + 身份词」判就会全放过（早前版本正是这样，`hasVendorToken` 写了没接线）。
func TestShouldDropResponseHeader_F8Classes(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  bool
		why   string
	}{
		{"X-OpenAI-Organization", "org-1", true, "头名含供应商名（Gate 漏点①）"},
		{"OpenAI-Processing-Ms", "12", true, "头名含供应商名（Gate 漏点②）"},
		{"OpenAI-Version", "2020-10-01", true, "头名含供应商名（Gate 漏点③）"},
		{"openai-organization", "x", true, "大小写不敏感"},
		{"X-ANTHROPIC-Trace", "t", true, "头名含供应商名（大写）"},
		{"x-goog-request-id", "r", true, "渠道/供应商名前缀"},
		{"x-magpie-route", "r", true, "上游标识前缀"},
		{"x-upstream-model", "m", true, "上游标识前缀"},
		{"x-served-by", "edge-1", true, "身份类头名（不含供应商字面量）"},
		{"x-account-owner", "acme", true, "身份类头名"},
		{"x-note", "powered by gemini", true, "取值含供应商名"},
		{"server", "nginx", true, "实现泄漏"},
		{"x-powered-by", "php", true, "实现泄漏"},
		{"via", "1.1 proxy", true, "链路泄漏"},
		{"x-request-id", "up-rid", true, "上游同名头必须先丢，再写我们的"},
		{"content-type", "application/json", false, "普通头必须透传"},
		{"x-ratelimit-limit", "50", false, "通用限流头透传（不一刀切）"},
		{"cache-control", "no-store", false, "普通头必须透传"},
		{"x-request-cost", "0.02", false, "不含身份信息的业务头透传"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ShouldDropResponseHeader(c.name, c.value); got != c.want {
				t.Fatalf("ShouldDropResponseHeader(%q,%q)=%v, want %v（%s）",
					c.name, c.value, got, c.want, c.why)
			}
		})
	}
}

// TestForward_OurRequestIDWinsOverUpstream 断言 x-request-id 归灵枢：
// 上游即使发多个同名值（大小写混用会被规范化成同一个键的多个值），下游只能看到我们那一个。
func TestForward_OurRequestIDWinsOverUpstream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("x-request-id", "up-1")
		w.Header().Add("X-Request-Id", "up-2")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	f := NewForwarder()
	rec := httptest.NewRecorder()
	if res := f.Forward(t.Context(), rec, ForwardRequest{
		BaseURL: up.URL, Path: "/v1/chat/completions", Header: http.Header{},
		Body: []byte(`{"model":"m"}`), RequestID: "ours-1", Protocol: ProtocolOpenAIChat,
	}, Timeouts{StreamFirstByte: 5 * time.Second, StreamIdle: 5 * time.Second}); res.Err != nil {
		t.Fatalf("转发失败: %v", res.Err)
	}
	vals := rec.Header().Values("x-request-id")
	if len(vals) != 1 || vals[0] != "ours-1" {
		t.Fatalf("下游只该看到灵枢自己的 x-request-id（ours-1），got %v", vals)
	}
}

// ---------------------------------------------------------------------------
// SSE 流式路径的响应头防线（缺口的回归用例：此前没有任何用例钉住 hop-by-hop 剔除）。
//
// 为什么用裸 TCP 上游而不是 httptest.NewServer：Go 的 http.Server 会自己吞掉/改写
// Transfer-Encoding、Content-Length 等头，测不出「上游/中间层把响应头整包拷来、没剔除」
// 时的真实穿透。实测（2026-11-16）：Go 传输层**不**替你剥 hop-by-hop ——
// resp.Header 里 Connection / Keep-Alive / Proxy-Connection / TE / Trailer / Upgrade 全在，
// 最外层唯一防线就是本包的 responseHopByHop + ShouldDropResponseHeader。
// ---------------------------------------------------------------------------

// rawHeaderUpstream 起一个只应答一次的裸 TCP 上游，手写响应行/头/体。
// chunked=true 时用 chunked 框架（真实 SSE 的传输形态，且不带 Content-Length）。
func rawHeaderUpstream(t *testing.T, headers [][2]string, body []byte, chunked bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("裸上游监听失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		for { // 吃掉请求头，读到空行为止
			line, rerr := br.ReadString('\n')
			if rerr != nil {
				return
			}
			if line == "\r\n" || line == "\n" {
				break
			}
		}
		var b strings.Builder
		b.WriteString("HTTP/1.1 200 OK\r\n")
		for _, h := range headers {
			b.WriteString(h[0] + ": " + h[1] + "\r\n")
		}
		if chunked {
			b.WriteString("Transfer-Encoding: chunked\r\n\r\n")
			b.WriteString(fmt.Sprintf("%x\r\n", len(body)))
			b.Write(body)
			b.WriteString("\r\n0\r\n\r\n")
		} else {
			b.WriteString(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)))
			b.Write(body)
		}
		_, _ = conn.Write([]byte(b.String()))
	}()
	return "http://" + ln.Addr().String()
}

// sseBodyWithUsage 是一段最小 SSE：两个内容帧 + 一个 usage 帧 + [DONE]。
func sseBodyWithUsage() []byte {
	return []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"there\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n" +
		"data: [DONE]\n\n")
}

// TestForward_StreamResponseHeadersFiltered 是「拷贝头但不剔除」的端到端回归用例：
// 上游/中间层把响应头整包拷过来（含 hop-by-hop），下游只许看到经本包过滤后的结果。
//
// 两个方向都要钉：
//   - 正向：hop-by-hop 一律不许穿透（漏一个就是连接级语义跨代理：可能把上游连接头 /
//     内部路由头亮给租户，或让下游按错误的分帧方式读流）；
//   - 反向：SSE 语义头（Content-Type: text/event-stream、Cache-Control）与普通通用头
//     （X-Accel-Buffering、X-RateLimit-*）必须保留，过滤不能退化成「一刀切全剥」。
func TestForward_StreamResponseHeadersFiltered(t *testing.T) {
	body := sseBodyWithUsage()

	cases := []struct {
		name, value string
		drop        bool   // true=下游必须看不到；false=下游必须原值看到
		why         string // 判定依据（漏了要能一眼看出违反了哪条口径）
	}{
		{"Connection", "keep-alive", true, "hop-by-hop（RFC 7230 §6.1）"},
		{"Keep-Alive", "timeout=5", true, "hop-by-hop"},
		{"Proxy-Connection", "keep-alive", true, "hop-by-hop（非标但中间层常发）"},
		{"TE", "trailers", true, "hop-by-hop"},
		{"Trailer", "X-Sum", true, "hop-by-hop"},
		{"Upgrade", "h2c", true, "hop-by-hop（协议切换不能跨代理）"},
		{"Content-Type", "text/event-stream; charset=utf-8", false, "SSE 语义头：掉了下游就不是流"},
		{"Cache-Control", "no-cache", false, "SSE 语义头（禁缓存）"},
		{"X-Accel-Buffering", "no", false, "反代不缓冲 SSE 的语义头"},
		{"X-RateLimit-Limit", "50", false, "普通通用头：陪跑，证明不是一刀切"},
	}

	// 单个上游一次发出全部头，更接近「整包拷贝」；再加一个由 Connection 点名的自定义头。
	headers := make([][2]string, 0, len(cases)+1)
	for _, c := range cases {
		headers = append(headers, [2]string{c.name, c.value})
	}
	headers = append(headers, [2]string{"X-Internal-Hop", "secret-route"})

	f := NewForwarder()
	rec := httptest.NewRecorder()
	res := f.Forward(t.Context(), rec, ForwardRequest{
		BaseURL: rawHeaderUpstream(t, headers, body, false),
		Path:    "/v1/chat/completions", Header: http.Header{},
		Body: []byte(`{"model":"m","stream":true}`), RequestID: "our-rid",
		Streaming: true, Billable: true, Protocol: ProtocolOpenAIChat,
	}, Timeouts{StreamFirstByte: 5 * time.Second, StreamIdle: 5 * time.Second})
	if res.Err != nil {
		t.Fatalf("转发失败: %v", res.Err)
	}
	// 过滤只动头、不动体：字节仍须逐字节相等，usage 仍须旁路读到。
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("SSE 响应体必须逐字节相等：got %d want %d（首差异 %d）",
			rec.Body.Len(), len(body), firstDiff(rec.Body.Bytes(), body))
	}
	if !res.HasUsage || res.Usage.Input != 11 || res.Usage.Output != 7 {
		t.Fatalf("usage 应仍被旁路读到：%+v has=%v", res.Usage, res.HasUsage)
	}
	if got := rec.Header().Get("x-request-id"); got != "our-rid" {
		t.Fatalf("x-request-id 仍是灵枢自己的，got %q", got)
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rec.Header().Get(c.name)
			if c.drop {
				if got != "" {
					t.Fatalf("%s 必须剔除（%s），下游却收到 %q", c.name, c.why, got)
				}
				return
			}
			if got != c.value {
				t.Fatalf("%s 必须保留 %q（%s），got %q", c.name, c.value, c.why, got)
			}
		})
	}

}

// TestForward_StreamConnectionTokenHeadersNotCopied 单列「Connection 点名的头」这一类：
// RFC 7230 §6.1 第 2 段规定 `Connection: X-Route` 里点名的字段同样是 hop-by-hop，必须一并丢。
// 上游/中间层常把内部路由头写进 Connection，只按静态表匹配头名就会把它原样漏给租户。
//
// 与上面那条用例分开，是为了让「既有防线」和「已知缺口」各自可判：
// 本条红 = 缺口未修；上面那条红 = 已生效的防线被改坏。
func TestForward_StreamConnectionTokenHeadersNotCopied(t *testing.T) {
	body := sseBodyWithUsage()
	headers := [][2]string{
		{"Content-Type", "text/event-stream; charset=utf-8"},
		{"Connection", "keep-alive, x-internal-hop"},
		{"X-Internal-Hop", "secret-route"},
	}

	f := NewForwarder()
	rec := httptest.NewRecorder()
	if res := f.Forward(t.Context(), rec, ForwardRequest{
		BaseURL: rawHeaderUpstream(t, headers, body, false),
		Path:    "/v1/chat/completions", Header: http.Header{},
		Body: []byte(`{"model":"m","stream":true}`), RequestID: "our-rid",
		Streaming: true, Billable: true, Protocol: ProtocolOpenAIChat,
	}, Timeouts{StreamFirstByte: 5 * time.Second, StreamIdle: 5 * time.Second}); res.Err != nil {
		t.Fatalf("转发失败: %v", res.Err)
	}
	if got := rec.Header().Get("X-Internal-Hop"); got != "" {
		t.Fatalf("Connection 点名的 X-Internal-Hop 属 hop-by-hop（RFC 7230 §6.1），必须剔除，下游却收到 %q", got)
	}
	// 反向：Connection 本身被丢，不影响 SSE 语义头。
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("SSE 语义头必须保留，got %q", got)
	}
}

// TestShouldDropResponseHeader_HopByHop 是把 hop-by-hop 口径钉在判定函数上的表：
// 端到端用例保证链路生效，这里保证口径本身不被悄悄放宽（例如为了「少剥几个头」
// 把某一行从 responseHopByHop 里删掉）。
func TestShouldDropResponseHeader_HopByHop(t *testing.T) {
	cases := []struct {
		name, value string
		want        bool
		why         string
	}{
		{"Connection", "keep-alive", true, "hop-by-hop"},
		{"Keep-Alive", "timeout=5", true, "hop-by-hop"},
		{"Proxy-Connection", "keep-alive", true, "hop-by-hop"},
		{"TE", "trailers", true, "hop-by-hop"},
		{"Trailer", "X-Sum", true, "hop-by-hop"},
		{"Transfer-Encoding", "chunked", true, "hop-by-hop（分帧由本链路自己决定）"},
		{"Upgrade", "h2c", true, "hop-by-hop"},
		{"Content-Type", "text/event-stream; charset=utf-8", false, "SSE 语义头"},
		{"Cache-Control", "no-cache", false, "SSE 语义头"},
		{"X-Accel-Buffering", "no", false, "反代不缓冲"},
		// 现状记录，不是「正确性」断言：本链路不改写响应体，长度仍然成立，headers.go 注释
		// 明确保留 Content-Length。SSE 口径若要改成「流式不得带长度」，改实现时这一行同步变红，
		// 逼一次显式决策（而不是悄悄放宽/收紧）。
		{"Content-Length", "134", false, "现状：长度成立故保留（口径待定，见 headers.go 注释）"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ShouldDropResponseHeader(c.name, c.value); got != c.want {
				t.Fatalf("ShouldDropResponseHeader(%q,%q)=%v, want %v（%s）",
					c.name, c.value, got, c.want, c.why)
			}
		})
	}
}
