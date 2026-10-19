package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
	"github.com/team/llmgateway/internal/domain/protocolconv"
)

// TestResolveUpstreamProtocol 覆盖跨协议解析器的决策分支：
// 直通优先、全局/模型级开关门控，以及模型级目标协议覆盖顺序。
func TestResolveUpstreamProtocol(t *testing.T) {
	const internalID = "m1"

	rtWith := func(cp *channelprotocol.ChannelProtocol) *channel.RuntimeChannel {
		return &channel.RuntimeChannel{Protocol: cp}
	}
	mr := func() *channel.ModelRuntime {
		return &channel.ModelRuntime{Model: channel.ChannelModel{InternalModelID: internalID}}
	}
	convDisabled := false

	tests := []struct {
		name          string
		gw            *Gateway
		rt            *channel.RuntimeChannel
		mr            *channel.ModelRuntime
		baseProtocol  string
		endpoint      string
		wantUpstream  string
		wantConverted bool
		wantOK        bool
	}{
		{
			name: "direct pass",
			gw:   NewGateway(GatewayConfig{}),
			rt: rtWith(&channelprotocol.ChannelProtocol{Protocols: channelprotocol.Protocols{
				channelprotocol.ProtocolOpenAIChat:        {},
				channelprotocol.ProtocolAnthropicMessages: {},
			}}),
			mr:            mr(),
			baseProtocol:  channelprotocol.ProtocolOpenAIChat,
			endpoint:      EndpointChatCompletions,
			wantUpstream:  channelprotocol.ProtocolOpenAIChat,
			wantConverted: false,
			wantOK:        true,
		},
		{
			name: "convert to anthropic",
			gw:   NewGateway(GatewayConfig{}),
			rt: rtWith(&channelprotocol.ChannelProtocol{Protocols: channelprotocol.Protocols{
				channelprotocol.ProtocolAnthropicMessages: {},
			}}),
			mr:            mr(),
			baseProtocol:  channelprotocol.ProtocolOpenAIChat,
			endpoint:      EndpointChatCompletions,
			wantUpstream:  channelprotocol.ProtocolAnthropicMessages,
			wantConverted: true,
			wantOK:        true,
		},
		{
			name: "per-model disabled",
			gw:   NewGateway(GatewayConfig{}),
			rt: rtWith(&channelprotocol.ChannelProtocol{
				Protocols: channelprotocol.Protocols{
					channelprotocol.ProtocolAnthropicMessages: {},
				},
				Models: map[string]channelprotocol.ModelGateway{
					internalID: {ConvertEnabled: &convDisabled},
				},
			}),
			mr:            mr(),
			baseProtocol:  channelprotocol.ProtocolOpenAIChat,
			endpoint:      EndpointChatCompletions,
			wantUpstream:  "",
			wantConverted: false,
			wantOK:        false,
		},
		{
			name: "global disabled",
			gw:   NewGateway(GatewayConfig{ProtocolConversionEnabled: func() bool { return false }}),
			rt: rtWith(&channelprotocol.ChannelProtocol{Protocols: channelprotocol.Protocols{
				channelprotocol.ProtocolAnthropicMessages: {},
			}}),
			mr:            mr(),
			baseProtocol:  channelprotocol.ProtocolOpenAIChat,
			endpoint:      EndpointChatCompletions,
			wantUpstream:  "",
			wantConverted: false,
			wantOK:        false,
		},
		{
			name: "override target order",
			gw:   NewGateway(GatewayConfig{}),
			rt: rtWith(&channelprotocol.ChannelProtocol{
				Protocols: channelprotocol.Protocols{
					channelprotocol.ProtocolOpenAIChat:      {},
					channelprotocol.ProtocolOpenAIResponses: {},
				},
				Models: map[string]channelprotocol.ModelGateway{
					internalID: {ConvertTargets: []string{channelprotocol.ProtocolOpenAIResponses}},
				},
			}),
			mr:            mr(),
			baseProtocol:  channelprotocol.ProtocolAnthropicMessages,
			endpoint:      EndpointMessages,
			wantUpstream:  channelprotocol.ProtocolOpenAIResponses,
			wantConverted: true,
			wantOK:        true,
		},
		{
			name: "completions endpoint not conversion-eligible",
			gw:   NewGateway(GatewayConfig{}),
			rt: rtWith(&channelprotocol.ChannelProtocol{Protocols: channelprotocol.Protocols{
				channelprotocol.ProtocolAnthropicMessages: {},
			}}),
			mr:            mr(),
			baseProtocol:  channelprotocol.ProtocolOpenAIChat,
			endpoint:      EndpointCompletions,
			wantUpstream:  "",
			wantConverted: false,
			wantOK:        false,
		},
		{
			name: "completions endpoint direct pass",
			gw:   NewGateway(GatewayConfig{}),
			rt: rtWith(&channelprotocol.ChannelProtocol{Protocols: channelprotocol.Protocols{
				channelprotocol.ProtocolOpenAIChat: {},
			}}),
			mr:            mr(),
			baseProtocol:  channelprotocol.ProtocolOpenAIChat,
			endpoint:      EndpointCompletions,
			wantUpstream:  channelprotocol.ProtocolOpenAIChat,
			wantConverted: false,
			wantOK:        true,
		},
		{
			name: "embeddings endpoint not conversion-eligible",
			gw:   NewGateway(GatewayConfig{}),
			rt: rtWith(&channelprotocol.ChannelProtocol{Protocols: channelprotocol.Protocols{
				channelprotocol.ProtocolOpenAIResponses: {},
			}}),
			mr:            mr(),
			baseProtocol:  channelprotocol.ProtocolOpenAIChat,
			endpoint:      EndpointEmbeddings,
			wantUpstream:  "",
			wantConverted: false,
			wantOK:        false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream, converted, ok := tc.gw.resolveUpstreamProtocol(tc.rt, tc.mr, tc.baseProtocol, tc.endpoint)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v, want %v", ok, tc.wantOK)
			}
			if converted != tc.wantConverted {
				t.Fatalf("converted=%v, want %v", converted, tc.wantConverted)
			}
			if upstream != tc.wantUpstream {
				t.Fatalf("upstream=%q, want %q", upstream, tc.wantUpstream)
			}
		})
	}
}

// TestLossyDecision 直接单测转换字段助手的产出：
// 转换计划产出 converted/inbound_protocol/upstream_protocol，非空报告附 lossy；
// 直通计划不产出任何键（既有键集不变）。
func TestLossyDecision(t *testing.T) {
	rep := &protocolconv.LossyReport{}
	rep.Add("top-level extras dropped for anthropic target")

	got := conversionAttemptFields(convPlan{
		inbound:   channelprotocol.ProtocolOpenAIChat,
		upstream:  channelprotocol.ProtocolAnthropicMessages,
		converted: true,
	}, rep)
	if got == nil {
		t.Fatalf("转换计划应产出转换字段")
	}
	if got["converted"] != true {
		t.Fatalf("converted 应为 true, got %v", got["converted"])
	}
	if got["inbound_protocol"] != channelprotocol.ProtocolOpenAIChat {
		t.Fatalf("inbound_protocol 不符: %v", got["inbound_protocol"])
	}
	if got["upstream_protocol"] != channelprotocol.ProtocolAnthropicMessages {
		t.Fatalf("upstream_protocol 不符: %v", got["upstream_protocol"])
	}
	if _, ok := got["lossy"]; !ok {
		t.Fatalf("非空 LossyReport 应产出 lossy 字段: %+v", got)
	}

	// 直通计划：不新增任何键，尤其不得有 lossy。
	direct := conversionAttemptFields(convPlan{
		inbound:   channelprotocol.ProtocolOpenAIChat,
		upstream:  channelprotocol.ProtocolOpenAIChat,
		converted: false,
	}, rep)
	if _, ok := direct["lossy"]; ok {
		t.Fatalf("直通计划不应有 lossy 键: %+v", direct)
	}
	if len(direct) != 0 {
		t.Fatalf("直通计划不应产出任何字段: %+v", direct)
	}
}

// TestNonStreamConversion 端到端：openai-chat 入站 → 模型仅可达 anthropic-messages →
// 上游收到 anthropic 形状请求体（含 max_tokens），下游收到 openai-chat 形状响应体（含 choices）。
func TestNonStreamConversion(t *testing.T) {
	var upstreamBody []byte
	var upstreamPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg1","type":"message","role":"assistant","model":"gpt-4-internal",` +
			`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	// 模型仅可达 anthropic-messages → openai-chat 入站需转换为 anthropic 上游。
	h.chans.mrt.Model.Protocols = channelprotocol.Protocols{channelprotocol.ProtocolAnthropicMessages: {}}

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// 上游收到 anthropic 形状请求体（含 max_tokens；原 openai 请求体未给 max_tokens）。
	if !strings.Contains(string(upstreamBody), `"max_tokens"`) {
		t.Fatalf("上游应收到 anthropic 形状请求体（含 max_tokens），got %s", upstreamBody)
	}
	if upstreamPath != "/v1/messages" {
		t.Errorf("上游路径应为 anthropic 默认 /v1/messages, got %q", upstreamPath)
	}
	// 下游收到入站（openai-chat）形状响应体。
	if !strings.Contains(rec.Body.String(), `"choices"`) {
		t.Fatalf("下游应为 openai-chat 形状（含 choices），got %s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("转换响应 Content-Type 应为 application/json, got %q", ct)
	}
	// 计费用量按上游（anthropic）物理协议解析：input=10, output=5。
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Tokens.Input != 10 || h.billing.reqs[0].Tokens.Output != 5 {
		t.Fatalf("计费用量应取上游 anthropic usage, got %+v", h.billing.reqs)
	}
}

// TestStreamConversion 端到端：anthropic-messages 入站 → 模型仅可达 openai-chat →
// 上游收到 openai-chat 形状请求体（含 messages），下游 SSE 为 anthropic 帧（含 event: message_start / message_stop）。
func TestStreamConversion(t *testing.T) {
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
				"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7}}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	// 模型仅可达 openai-chat → anthropic-messages 入站需转换为 openai-chat 上游。
	h.chans.mrt.Model.Protocols = channelprotocol.Protocols{channelprotocol.ProtocolOpenAIChat: {}}

	rec := postWithHeaders(t, h.gw, "/v1/messages",
		`{"model":"gpt-4","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"ping"}]}`,
		map[string]string{"x-api-key": "sk-gw-t"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// 上游收到 openai-chat 形状请求体（含 messages）。
	if !strings.Contains(string(upstreamBody), `"messages"`) {
		t.Fatalf("上游应收到 openai-chat 形状请求体（含 messages），got %s", upstreamBody)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: message_start") {
		t.Fatalf("下游 anthropic SSE 应含 event: message_start，got %s", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Fatalf("下游 anthropic SSE 应含 event: message_stop，got %s", body)
	}
	// 计费用量取上游 openai usage（12/7）。
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Tokens.Input != 12 || h.billing.reqs[0].Tokens.Output != 7 {
		t.Fatalf("流式计费用量应取上游 openai usage, got %+v", h.billing.reqs)
	}
}

// TestStreamConversionMalformedFrameObserved 断言转换泵遇上游畸形帧时不再静默丢弃：
// 流仍完整完成（200 + message_stop），且畸形帧经损耗诊断随调用日志决策轨迹的 lossy_notes 落痕。
func TestStreamConversionMalformedFrameObserved(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
				"data: {oops-not-json}\n\n" +
				"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7}}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.chans.mrt.Model.Protocols = channelprotocol.Protocols{channelprotocol.ProtocolOpenAIChat: {}}

	rec := postWithHeaders(t, h.gw, "/v1/messages",
		`{"model":"gpt-4","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"ping"}]}`,
		map[string]string{"x-api-key": "sk-gw-t"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: message_stop") {
		t.Fatalf("畸形帧不应中断流：下游应以 message_stop 收尾，got %s", body)
	}
	// 畸形帧不得影响后续有效帧解析与计费（12/7）。
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Tokens.Input != 12 || h.billing.reqs[0].Tokens.Output != 7 {
		t.Fatalf("畸形帧不应影响后续有效帧计费（12/7），got %+v", h.billing.reqs)
	}
	// 畸形帧须可观测：随调用日志决策轨迹的 lossy_notes 落痕。
	if len(h.clogs.inserted) == 0 {
		t.Fatalf("缺少调用日志")
	}
	raw, err := json.Marshal(h.clogs.inserted[0].Decision)
	if err != nil {
		t.Fatalf("marshal decision: %v", err)
	}
	var dec struct {
		Attempts []map[string]any `json:"attempts"`
	}
	if err := json.Unmarshal(raw, &dec); err != nil {
		t.Fatalf("unmarshal decision: %v", err)
	}
	if len(dec.Attempts) == 0 {
		t.Fatalf("decision attempts 为空: %s", raw)
	}
	notes, _ := dec.Attempts[len(dec.Attempts)-1]["lossy_notes"].([]any)
	found := false
	for _, n := range notes {
		if s, ok := n.(string); ok && strings.Contains(s, "malformed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("畸形帧未在 lossy_notes 中体现: %s", raw)
	}
}

// TestCompletionsEndpointNoConversion 断言非 chat 形状端点（/v1/completions）不做跨协议转换：
// 模型可达协议不含 openai-chat 时按不支持处理（HTTP 400 + unsupported_protocol），且绝不发起上游调用。
func TestCompletionsEndpointNoConversion(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	// 模型仅可达 anthropic-messages（不含 openai-chat）→ /v1/completions 不可转换。
	h.chans.mrt.Model.Protocols = channelprotocol.Protocols{channelprotocol.ProtocolAnthropicMessages: {}}

	rec := doRequest(t, h.gw, "/v1/completions", "sk-gw-t", `{"model":"gpt-4","prompt":"ping"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 不支持协议，got %d: %s", rec.Code, rec.Body.String())
	}
	if et := decodeOpenAIError(t, rec).Error.Type; et != "unsupported_protocol" {
		t.Fatalf("want type=unsupported_protocol，got %q", et)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("/v1/completions 不应转换或发起上游调用，上游被调用 %d 次", n)
	}
}

// TestStreamConversionToolOnlyAnthropicTarget 端到端：anthropic-messages 入站 → 模型仅可达 openai-chat →
// 上游返回「仅工具调用」且 finish_reason 与 usage 分片的 openai-chat 流。断言下游 anthropic SSE：
// 工具块下标从 0 连续起始（上游 tool_calls 下标为 1 也不得起始于 1）、终止帧前先补 content_block_stop，
// 且 finish_reason 与 usage 合并为唯一一帧 message_delta。
func TestStreamConversionToolOnlyAnthropicTarget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}\n\n" +
				"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]}}]}\n\n" +
				"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
				"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	// 模型仅可达 openai-chat → anthropic-messages 入站需转换为 openai-chat 上游。
	h.chans.mrt.Model.Protocols = channelprotocol.Protocols{channelprotocol.ProtocolOpenAIChat: {}}

	rec := postWithHeaders(t, h.gw, "/v1/messages",
		`{"model":"gpt-4","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"ping"}]}`,
		map[string]string{"x-api-key": "sk-gw-t"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if n := strings.Count(body, "event: message_delta"); n != 1 {
		t.Fatalf("finish_reason 与 usage 应合并为唯一 message_delta 帧，got %d: %s", n, body)
	}
	if !strings.Contains(body, "event: content_block_stop") {
		t.Fatalf("anthropic 下游应先补 content_block_stop，got %s", body)
	}
	if strings.Contains(body, `"index":1`) {
		t.Fatalf("下游块下标应从 0 连续起始（工具块不得起始于 1），got %s", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Fatalf("anthropic 下游应以 message_stop 收尾，got %s", body)
	}
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Tokens.Input != 10 || h.billing.reqs[0].Tokens.Output != 5 {
		t.Fatalf("计费用量应取上游 openai usage(10/5)，got %+v", h.billing.reqs)
	}
}
