package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// AC-16：三协议 usage 归一化为三段（输入纯输入/缓存读/输出含推理），缓存写并入输入。
// 同时断言「上游原始 usage」被原样采集（落 call_logs.upstream_usage，供诊断渠道口径）。
func TestExtractUsage_ThreeProtocols(t *testing.T) {
	t.Run("openai-chat", func(t *testing.T) {
		body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":20},"completion_tokens_details":{"reasoning_tokens":10}}}`)
		u, raw, err := NewOpenAIProvider(EndpointChatCompletions).ExtractUsage(body)
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		// Input = prompt_tokens(100) − cached(20) = 80；Output = completion_tokens(50)（含推理）。
		if u.Input != 80 || u.Output != 50 || u.CacheRead != 20 {
			t.Fatalf("openai-chat 归一化错误: %+v", u)
		}
		assertRawUsage(t, raw, "openai-chat", `"cached_tokens":20`)
	})
	t.Run("openai-chat-deepseek-native", func(t *testing.T) {
		// DeepSeek 原生风格：只有顶层 prompt_cache_hit_tokens，没有 prompt_tokens_details.cached_tokens。
		// prompt_tokens = hit + miss = 100 = 80 + 20。
		body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20}}`)
		u, raw, err := NewOpenAIProvider(EndpointChatCompletions).ExtractUsage(body)
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		// Input = 100 − 80 = 20（缓存读必须被识别，否则命中部分会被按全价输入计）。
		if u.Input != 20 || u.Output != 20 || u.CacheRead != 80 {
			t.Fatalf("deepseek 原生风格归一化错误: %+v", u)
		}
		assertRawUsage(t, raw, "openai-chat", `"prompt_cache_hit_tokens":80`)
	})
	t.Run("openai-responses", func(t *testing.T) {
		body := []byte(`{"usage":{"input_tokens":200,"output_tokens":80,"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":20}}}`)
		u, raw, err := NewResponsesProvider().ExtractUsage(body)
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		// Input = input_tokens(200) − cached(30) = 170；Output = output_tokens(80)（含推理）。
		if u.Input != 170 || u.Output != 80 || u.CacheRead != 30 {
			t.Fatalf("openai-responses 归一化错误: %+v", u)
		}
		assertRawUsage(t, raw, "openai-responses", `"input_tokens":200`)
	})
	t.Run("anthropic-messages", func(t *testing.T) {
		body := []byte(`{"usage":{"input_tokens":300,"output_tokens":90,"cache_read_input_tokens":40,"cache_creation_input_tokens":15}}`)
		u, raw, err := NewAnthropicProvider().ExtractUsage(body)
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		// Input = input_tokens(300) + cache_creation(15) = 315（缓存写并入输入）；Output = output_tokens(90)。
		if u.Input != 315 || u.Output != 90 || u.CacheRead != 40 {
			t.Fatalf("anthropic-messages 归一化错误: %+v", u)
		}
		// 原始 usage 必须保留 cache_creation（否则事后无法核对写入 token 的处理）。
		assertRawUsage(t, raw, "anthropic-messages", `"cache_creation_input_tokens":15`)
	})
}

// assertRawUsage 断言上游原始 usage 落库值：为 {"Protocol":...,"Usage":{...}} 且含指定片段。
func assertRawUsage(t *testing.T, raw []byte, protocol, wantFragment string) {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("原始 usage 为空，应落库")
	}
	var env struct {
		Protocol string                     `json:"Protocol"`
		Usage    map[string]json.RawMessage `json:"Usage"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("原始 usage 非法 JSON: %v (%s)", err, raw)
	}
	if env.Protocol != protocol {
		t.Fatalf("原始 usage Protocol=%q, want %q", env.Protocol, protocol)
	}
	if len(env.Usage) == 0 {
		t.Fatalf("原始 usage 键值为空: %s", raw)
	}
	if !strings.Contains(string(raw), wantFragment) {
		t.Fatalf("原始 usage 缺少 %s: %s", wantFragment, raw)
	}
}

// AC-17 前置：usage 缺失时三协议均返回 ErrUsageMissing。
func TestExtractUsage_Missing(t *testing.T) {
	noUsage := []byte(`{"id":"x","choices":[]}`)
	for _, p := range []Provider{NewOpenAIProvider(EndpointChatCompletions), NewResponsesProvider(), NewAnthropicProvider()} {
		if _, _, err := p.ExtractUsage(noUsage); !errors.Is(err, ErrUsageMissing) {
			t.Errorf("%s usage 缺失应返回 ErrUsageMissing, got %v", p.Name(), err)
		}
	}
}

// 流式分片的 usage 解析：DeepSeek 原生风格（只有顶层 prompt_cache_hit_tokens）也要归一化，
// 且必须同时返回原始 JSON 供 call_log 记录上游原文。
func TestParseStreamData_DeepSeekNative(t *testing.T) {
	data := []byte(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_cache_hit_tokens":80}}`)
	u, raw, ok, hasErr, errDetail := parseStreamData(data)
	if !ok {
		t.Fatalf("应命中 usage")
	}
	if hasErr || errDetail != "" {
		t.Fatalf("非 error 帧不应置位或产生摘要, got hasErr=%v detail=%q", hasErr, errDetail)
	}
	// Input = 100 − 80 = 20（纯输入）；CacheRead = 80；Output = 20。
	if u.Input != 20 || u.CacheRead != 80 || u.Output != 20 {
		t.Fatalf("流式 DeepSeek 归一化错误: %+v", u)
	}
	if !strings.Contains(string(raw), `"prompt_cache_hit_tokens":80`) {
		t.Fatalf("原始 usage 应保留顶层命中字段: %s", raw)
	}
}

// 单帧同时携带 usage 与 error 时，两者都必须取自同一次解析（一次反序列化拿全信息）。
func TestParseStreamDataErrorFrame(t *testing.T) {
	u, raw, ok, hasErr, errDetail := parseStreamData([]byte(
		`{"choices":[],"error":{"type":"server_error","message":"boom"},"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
	if !ok || u.Input != 7 || u.Output != 3 {
		t.Fatalf("同一帧的 usage 应照常解析: %+v ok=%v", u, ok)
	}
	if len(raw) == 0 {
		t.Fatalf("应返回原始 usage 分片")
	}
	if !hasErr || errDetail != "server_error: boom" {
		t.Fatalf("error 帧应置位且摘要 want %q, got hasErr=%v detail=%q", "server_error: boom", hasErr, errDetail)
	}
	// error 为 null / 字符串形式的老上游：分别为「无 error 帧」与「字符串摘要」。
	if _, _, _, hasErr, d := parseStreamData([]byte(`{"error":null}`)); hasErr || d != "" {
		t.Fatalf("error=null 不应置位或产生摘要, got hasErr=%v detail=%q", hasErr, d)
	}
	if _, _, _, hasErr, d := parseStreamData([]byte(`{"error":"plain failure"}`)); !hasErr || d != "plain failure" {
		t.Fatalf("字符串 error 应置位并原样作为摘要, got hasErr=%v detail=%q", hasErr, d)
	}
	// 空对象负载：摘要为空但仍是 error 帧（HandleStream 依赖该布尔置 sawError，
	// 否则 {"error":{}} 会被 [DONE] 洗成 done_no_usage，修复前即红）。
	if _, _, _, hasErr, d := parseStreamData([]byte(`{"error":{}}`)); !hasErr || d != "" {
		t.Fatalf("空 error 对象应置位且摘要为空, got hasErr=%v detail=%q", hasErr, d)
	}
}

// sseResponse 把 SSE 文本包装为 HandleStream 期望的上游响应。
func sseResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// 非 openai-chat 协议同样按流结束形态区分 usage 缺失原因：
// 正常结束（message_stop / response.completed）不惩罚渠道；截断与流内 error 帧计入健康失败。
func TestHandleStreamUsageMissingReason(t *testing.T) {
	const anthropicStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n"
	const anthropicStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	const responsesDelta = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"

	tests := []struct {
		name       string
		provider   Provider
		body       string
		wantReason string
		wantPunish bool
		wantDetail string
	}{
		{
			name:     "anthropic 正常结束无 usage",
			provider: NewAnthropicProvider(),
			body:     anthropicStart + anthropicStop,
			// 上游已按正常形态收尾，只是没给 usage（协议偏差），不惩罚渠道。
			wantReason: UsageMissingDoneNoUsage,
			wantPunish: false,
		},
		{
			name:       "anthropic 截断",
			provider:   NewAnthropicProvider(),
			body:       anthropicStart,
			wantReason: UsageMissingTruncated,
			wantPunish: true,
		},
		{
			name:     "anthropic error 事件",
			provider: NewAnthropicProvider(),
			body: anthropicStart +
				"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n",
			wantReason: UsageMissingErrorFrame,
			wantPunish: true,
			wantDetail: "overloaded_error: busy",
		},
		{
			name:     "responses 正常结束无 usage",
			provider: NewResponsesProvider(),
			body: responsesDelta +
				"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n",
			wantReason: UsageMissingDoneNoUsage,
			wantPunish: false,
		},
		{
			name:       "responses 截断",
			provider:   NewResponsesProvider(),
			body:       responsesDelta,
			wantReason: UsageMissingTruncated,
			wantPunish: true,
		},
		{
			name:     "responses failed",
			provider: NewResponsesProvider(),
			body: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\"," +
				"\"message\":\"boom\"}}}\n\n",
			wantReason: UsageMissingErrorFrame,
			wantPunish: true,
			wantDetail: "server_error: boom",
		},
		{
			name:     "responses failed 后补 [DONE]",
			provider: NewResponsesProvider(),
			body: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\"," +
				"\"message\":\"boom\"}}}\n\ndata: [DONE]\n\n",
			// [DONE] 只是收口标记：上游先报 failed 再补 [DONE] 时仍须归因 error_frame（惩罚不丢失）。
			wantReason: UsageMissingErrorFrame,
			wantPunish: true,
			wantDetail: "server_error: boom",
		},
		{
			name:       "openai 正常结束无 usage",
			provider:   NewOpenAIProvider(EndpointChatCompletions),
			body:       "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
			wantReason: UsageMissingDoneNoUsage,
			wantPunish: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := tt.provider.HandleStream(context.Background(), sseResponse(tt.body), httptest.NewRecorder())
			if !errors.Is(err, ErrUsageMissing) {
				t.Fatalf("want ErrUsageMissing, got %v", err)
			}
			if got := usageMissingReason(err); got != tt.wantReason {
				t.Fatalf("reason want %q, got %q", tt.wantReason, got)
			}
			if got := usageMissingPunishable(usageMissingReason(err)); got != tt.wantPunish {
				t.Fatalf("punishable want %v, got %v", tt.wantPunish, got)
			}
			if tt.wantDetail != "" {
				var ue *UsageMissingError
				if !errors.As(err, &ue) || ue.Detail != tt.wantDetail {
					t.Fatalf("detail want %q, got %+v", tt.wantDetail, ue)
				}
			}
		})
	}
}

// 直通路径的 error 帧摘要按「首个非空优先」聚合：上游可能先给出带详情的高价值 error，
// 再补一个空负载 error 收尾；后者若无条件覆盖会把诊断信息清空。
// 修复前 responses/anthropic 直接赋值，空摘要会清掉前一条，本用例即红。
func TestHandleStreamErrorFrameKeepsFirstDetail(t *testing.T) {
	tests := []struct {
		name       string
		provider   Provider
		body       string
		wantDetail string
	}{
		{
			name:     "responses 连续 failed",
			provider: NewResponsesProvider(),
			body: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\"," +
				"\"message\":\"boom\"}}}\n\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"error\":{}}}\n\n",
			wantDetail: "server_error: boom",
		},
		{
			name:     "anthropic 连续 error",
			provider: NewAnthropicProvider(),
			body: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"," +
				"\"message\":\"busy\"}}\n\n" +
				"event: error\ndata: {\"type\":\"error\",\"error\":{}}\n\n",
			wantDetail: "overloaded_error: busy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := tt.provider.HandleStream(context.Background(), sseResponse(tt.body), httptest.NewRecorder())
			if !errors.Is(err, ErrUsageMissing) {
				t.Fatalf("want ErrUsageMissing, got %v", err)
			}
			if got := usageMissingReason(err); got != UsageMissingErrorFrame {
				t.Fatalf("reason want %q, got %q", UsageMissingErrorFrame, got)
			}
			var ue *UsageMissingError
			if !errors.As(err, &ue) || ue.Detail != tt.wantDetail {
				t.Fatalf("detail want %q, got %+v", tt.wantDetail, ue)
			}
		})
	}
}

// failWriteRecorder 包装 ResponseRecorder：响应头照常可写，但每次 Write 都失败（模拟下游断开）。
type failWriteRecorder struct {
	*httptest.ResponseRecorder
}

func (w failWriteRecorder) Write([]byte) (int, error) { return 0, errors.New("downstream gone") }

func (w failWriteRecorder) Flush() {}

// 下游写失败必须被标记为 errDownstreamWrite：handler 据此区分「上游读侧中断」与「下游写失败」。
func TestHandleStreamDownstreamWriteSentinel(t *testing.T) {
	const body = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	for _, p := range []Provider{
		NewOpenAIProvider(EndpointChatCompletions), NewAnthropicProvider(), NewResponsesProvider(),
	} {
		_, _, _, err := p.HandleStream(context.Background(), sseResponse(body),
			failWriteRecorder{httptest.NewRecorder()})
		if !isDownstreamWriteErr(err) {
			t.Fatalf("%s 下游写失败应可识别为下游错误, got %v", p.Name(), err)
		}
	}
}

// rawUsageJSON 合并流式多分片：同名键后写覆盖（Anthropic 的 message_start 给输入/缓存、
// message_delta 给累计输出），其余键保留；空输入返回 nil（不写 NULL 以外的值）。
func TestRawUsageJSON_MergeShards(t *testing.T) {
	start := json.RawMessage(`{"input_tokens":191,"output_tokens":1,"cache_read_input_tokens":96896,"cache_creation_input_tokens":0}`)
	delta := json.RawMessage(`{"output_tokens":94}`)

	got := rawUsageJSON("anthropic-messages", []json.RawMessage{start, delta})
	var env struct {
		Protocol string                     `json:"Protocol"`
		Usage    map[string]json.RawMessage `json:"Usage"`
	}
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatalf("原始 usage 非法 JSON: %v (%s)", err, got)
	}
	if env.Protocol != "anthropic-messages" {
		t.Fatalf("Protocol=%q, want anthropic-messages", env.Protocol)
	}
	// output_tokens 后写覆盖为 94；input/cache_read 保留。
	if string(env.Usage["output_tokens"]) != "94" {
		t.Fatalf("output_tokens 应被后分片覆盖为 94，got %s", env.Usage["output_tokens"])
	}
	if string(env.Usage["input_tokens"]) != "191" || string(env.Usage["cache_read_input_tokens"]) != "96896" {
		t.Fatalf("输入/缓存读键应保留，got %s", got)
	}
	// 无有效分片 → nil（落库为 NULL）。
	if b := rawUsageJSON("openai-chat", nil); b != nil {
		t.Fatalf("空分片应返回 nil，got %s", b)
	}
	// 非对象分片忽略，不 panic。
	if b := rawUsageJSON("openai-chat", []json.RawMessage{json.RawMessage(`[1,2]`)}); b != nil {
		t.Fatalf("非对象分片应被忽略，got %s", b)
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
	// 关闭跨协议转换：本用例验证「无直通且不可转换」时回固定 400（AC-4）。
	h.gw.protocolConvEnabled = func() bool { return false }

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
