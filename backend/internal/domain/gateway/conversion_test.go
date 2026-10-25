package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
	"github.com/team/llmgateway/internal/domain/protocolconv"
	"github.com/team/llmgateway/internal/domain/router"
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

// TestConvertedReasoningInjectHonorsOnlyThinking 端到端覆盖转换链路的推理注入门控：
// 入站 anthropic（带 thinking）→ 上游 openai-chat 时，assistant 历史的 thinking 应被回填为
// reasoning_content；同一渠道、请求体不带任何 thinking 字段时，注入须整体关闭
// （避免向非 thinking 请求注入推理字段）。
// 说明：本用例覆盖端到端行为（门控 + 注入结果），不区分判定基准用的是入站体还是转换后请求体
// （两者在本方向上都会保留 thinking 标记），该实现细节由 TestChannelReasoningInject 单测覆盖。
func TestConvertedReasoningInjectHonorsOnlyThinking(t *testing.T) {
	const assistantHistory = `{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"CoT","signature":"s"},` +
		`{"type":"tool_use","id":"toolu_1","name":"f","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}`

	cases := []struct {
		name       string
		thinkingOn bool
		wantInject bool
	}{
		{name: "thinking_request_injects", thinkingOn: true, wantInject: true},
		{name: "plain_request_does_not_inject", thinkingOn: false, wantInject: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},` +
					`"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
			}))
			defer upstream.Close()

			h := build(t, upstream.URL)
			// 模型仅可达 openai-chat → anthropic 入站需转换为 openai-chat 上游。
			h.chans.mrt.Model.Protocols = channelprotocol.Protocols{channelprotocol.ProtocolOpenAIChat: {}}
			// 渠道声明「支持推理回传」且仅在 thinking 请求下注入。
			h.chans.rt.Protocol = &channelprotocol.ChannelProtocol{
				Features: []channelprotocol.Feature{{
					Key:    channelprotocol.FeatureThinkingRoundtrip,
					Params: map[string]any{"enabled": true, "onlyThinking": true},
				}},
			}

			thinkingField := ""
			if tc.thinkingOn {
				thinkingField = `"thinking":{"type":"enabled"},`
			}
			body := `{"model":"gpt-4","stream":false,"max_tokens":64,` + thinkingField +
				`"messages":[` + assistantHistory + `]}`
			rec := postWithHeaders(t, h.gw, "/v1/messages", body, map[string]string{"x-api-key": "sk-gw-t"})
			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
			}
			got := string(upstreamBody)
			if !strings.Contains(got, `"messages"`) {
				t.Fatalf("上游应收到 openai-chat 形状请求体, got %s", got)
			}
			injected := strings.Contains(got, `"reasoning_content":"CoT"`)
			if injected != tc.wantInject {
				t.Fatalf("reasoning_content 注入=%v, want %v；上游请求体: %s", injected, tc.wantInject, got)
			}
		})
	}
}

// TestConvertedReasoningInjectReportsDroppedThinking 断言未注入时损耗报告如实记账：
// 同一转换链路下不开注入选项，thinking 必须记为 message.content.thinking（不静默丢弃）。
func TestConvertedReasoningInjectReportsDroppedThinking(t *testing.T) {
	const body = `{"model":"gpt-4","stream":false,"max_tokens":64,"messages":[` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"CoT","signature":"s"},` +
		`{"type":"text","text":"hi"}]}]}`
	out, rep, err := protocolconv.ConvertRequestWithOptions(
		channelprotocol.ProtocolAnthropicMessages, channelprotocol.ProtocolOpenAIChat, []byte(body), protocolconv.SerializeOptions{})
	if err != nil {
		t.Fatalf("ConvertRequestWithOptions: %v", err)
	}
	if strings.Contains(string(out), "reasoning_content") {
		t.Fatalf("默认不应注入 reasoning_content: %s", out)
	}
	found := false
	for _, d := range rep.Dropped {
		if d == "message.content.thinking" {
			found = true
		}
	}
	if !found {
		t.Fatalf("未注入时应记 lossy, got %v", rep.Dropped)
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

// 转换泵：上游以 [DONE] 正常收尾但未给 usage → 按估算计费，且不惩罚渠道
// （「正常结束无 usage」属上游忽略 include_usage 的协议偏差，与流被截断区分）。
func TestStreamConversionDoneNoUsageNotPunished(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello world\"}}]}\n\n" +
				"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	// 模型仅可达 openai-chat → anthropic-messages 入站需转换为 openai-chat 上游（走转换泵）。
	h.chans.mrt.Model.Protocols = channelprotocol.Protocols{channelprotocol.ProtocolOpenAIChat: {}}

	rec := postWithHeaders(t, h.gw, "/v1/messages",
		`{"model":"gpt-4","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"ping"}]}`,
		map[string]string{"x-api-key": "sk-gw-t"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
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

// 转换泵级归因矩阵：只有内容层收尾证据齐备才归 done_no_usage（不惩罚）；
// 「截断 + 补发 [DONE]」（只有标记、无 finish_reason）归 terminator_only 并计入健康失败；
// 未补 [DONE] 但给了 finish_reason 的兼容上游（连接干净结束）仍算正常结束。
func TestStreamConversionUsageMissingEndShape(t *testing.T) {
	const chatDelta = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"
	const chatFinish = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"

	tests := []struct {
		name       string
		sse        string
		wantReason string
		wantPunish bool
	}{
		{
			name:       "内容收尾 + [DONE]",
			sse:        chatDelta + chatFinish + "data: [DONE]\n\n",
			wantReason: UsageMissingDoneNoUsage,
			wantPunish: false,
		},
		{
			// 本 P2 的核心场景：上游被截断，代理兜底补一个 [DONE]。
			// 修复前 pump 用 sawDone||sawTerminal 判定正常结束 → done_no_usage（红）。
			name:       "截断 + 补发 [DONE]",
			sse:        chatDelta + "data: [DONE]\n\n",
			wantReason: UsageMissingTerminatorOnly,
			wantPunish: true,
		},
		{
			// 兼容上游不补 [DONE]、只靠 finish_reason 收尾且连接干净结束：内容证据齐备，算正常结束。
			name:       "仅 finish_reason 无 [DONE]",
			sse:        chatDelta + chatFinish,
			wantReason: UsageMissingDoneNoUsage,
			wantPunish: false,
		},
		{
			name:       "既无标记也无收尾证据",
			sse:        chatDelta,
			wantReason: UsageMissingTruncated,
			wantPunish: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pg := build(t, "http://unused")
			_, _, _, err := pg.gw.pumpConvertedStream(context.Background(), sseResponse(tt.sse),
				channelprotocol.ProtocolOpenAIChat, channelprotocol.ProtocolOpenAIChat,
				&protocolconv.LossyReport{}, httptest.NewRecorder())
			if !errors.Is(err, ErrUsageMissing) {
				t.Fatalf("want ErrUsageMissing, got %v", err)
			}
			if got := usageMissingReason(err); got != tt.wantReason {
				t.Fatalf("reason want %q, got %q", tt.wantReason, got)
			}
			if got := usageMissingPunishable(usageMissingReason(err)); got != tt.wantPunish {
				t.Fatalf("punishable want %v, got %v", tt.wantPunish, got)
			}
		})
	}
}

// 端到端（转换泵 + anthropic 入站）：上游截断后代理补发 [DONE]，渠道健康、调用日志两个观测面
// 都必须能看见——回喂健康失败且调用日志 failed，计费仍按估算成功（不丢计费）。
func TestStreamConversionDoneOnlyPenalized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hel\"}}]}\n\n" +
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
	if len(h.chans.feeds) != 1 || h.chans.feeds[0].fb.IsSuccess {
		t.Fatalf("补发 [DONE] 掩盖截断应计入渠道健康失败, got %+v", h.chans.feeds)
	}
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Fail {
		t.Fatalf("usage 缺失应按估算计费（成功账单）, got %+v", h.billing.reqs)
	}
	if len(h.clogs.inserted) == 0 {
		t.Fatalf("缺少调用日志")
	}
	last := h.clogs.inserted[len(h.clogs.inserted)-1]
	if last.Status != "failed" || last.RespKind != "stream" {
		t.Fatalf("补发 [DONE] 掩盖截断应记 failed/stream 调用日志, got status=%q resp_kind=%q", last.Status, last.RespKind)
	}
	if !strings.Contains(last.ErrorMessage, "仅收到结束标记") {
		t.Fatalf("调用日志应区分「仅补发结束标记」与普通截断, got %q", last.ErrorMessage)
	}
}

// 转换泵下游写失败：必须保持 failed/error 调用日志，且不得回喂渠道健康失败。
// 若把泵内写错误改回直接返回原始 werr（不经 wrapDownstreamWrite），handler 会误判为上游故障
// 并回喂失败，本用例即变红。
func TestStreamConversionDownstreamWriteFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":10}}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	// openai-chat 入站 → 模型仅可达 anthropic-messages → 走转换泵。
	h.chans.mrt.Model.Protocols = channelprotocol.Protocols{channelprotocol.ProtocolAnthropicMessages: {}}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"ping"}]}`))
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
		t.Fatalf("下游写失败仍应保持 failed/error 调用日志, got status=%q resp_kind=%q", last.Status, last.RespKind)
	}
	if !strings.Contains(last.ErrorMessage, "下游写失败") {
		t.Fatalf("下游写失败应使用区分于上游中断的归因文案, got %q", last.ErrorMessage)
	}
}

// 转换泵的流内 error 归因：openai-responses 的 response.failed 与 anthropic 的 error 事件
// 都应返回 error_frame（计入渠道健康惩罚）；端到端调用日志记 failed，计费按估算成功。
func TestStreamConversionErrorFrame(t *testing.T) {
	tests := []struct {
		name          string
		upstreamProto string
		sse           string
		wantDetail    string
	}{
		{
			name:          "openai-responses 上游 response.failed",
			upstreamProto: channelprotocol.ProtocolOpenAIResponses,
			sse: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\"," +
				"\"message\":\"boom\"}}}\n\n",
			wantDetail: "server_error: boom",
		},
		{
			name:          "anthropic 上游 error 事件",
			upstreamProto: channelprotocol.ProtocolAnthropicMessages,
			sse: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n" +
				"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n",
			wantDetail: "busy",
		},
		{
			// 修复前：sawError 与 sawDone 同时为真，归因优先取 done_no_usage → 不惩罚渠道（红）。
			name:          "openai-responses response.failed 后补 [DONE]",
			upstreamProto: channelprotocol.ProtocolOpenAIResponses,
			sse: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\"," +
				"\"message\":\"boom\"}}}\n\ndata: [DONE]\n\n",
			wantDetail: "server_error: boom",
		},
		{
			// 修复前：空 error 负载使 responsesFailedFrame 返回空串 → sawError 不置位，
			// 再由 [DONE] 归因 done_no_usage → 不惩罚渠道（红）。
			name:          "openai-responses response.failed 空 error 后补 [DONE]",
			upstreamProto: channelprotocol.ProtocolOpenAIResponses,
			sse: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{}}}\n\n" +
				"data: [DONE]\n\n",
			wantDetail: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 泵级：归因与惩罚。
			pg := build(t, "http://unused")
			_, _, _, err := pg.gw.pumpConvertedStream(context.Background(), sseResponse(tt.sse),
				channelprotocol.ProtocolOpenAIChat, tt.upstreamProto, &protocolconv.LossyReport{}, httptest.NewRecorder())
			if !errors.Is(err, ErrUsageMissing) {
				t.Fatalf("want ErrUsageMissing, got %v", err)
			}
			if got := usageMissingReason(err); got != UsageMissingErrorFrame {
				t.Fatalf("reason want %q, got %q", UsageMissingErrorFrame, got)
			}
			if !usageMissingPunishable(usageMissingReason(err)) {
				t.Fatalf("error_frame 应计入渠道健康惩罚")
			}
			var ue *UsageMissingError
			if !errors.As(err, &ue) || ue.Detail != tt.wantDetail {
				t.Fatalf("detail want %q, got %+v", tt.wantDetail, ue)
			}

			// 端到端：渠道健康与调用日志。
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.sse))
			}))
			defer upstream.Close()

			h := build(t, upstream.URL)
			h.chans.mrt.Model.Protocols = channelprotocol.Protocols{tt.upstreamProto: {}}
			rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
				`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("want 200 (stream already responded), got %d", rec.Code)
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
			// usage 缺失按可见内容估算计费：账单不得是 failed。
			if len(h.billing.reqs) != 1 || h.billing.reqs[0].Fail {
				t.Fatalf("usage 缺失应按估算计费（成功账单）, got %+v", h.billing.reqs)
			}
		})
	}
}

// 转换泵 + openai-chat 上游：openai-chat codec 的 DecodeEvent 没有 error 分支（error 帧解码为空事件），
// 泵须按顶层 error 键原文探测，否则随后的 [DONE] 会把失败归因成 done_no_usage
// （不回喂渠道健康、调用日志记 completed、下游收到空 200）。
func TestStreamConversionErrorFrameOpenAIChatUpstream(t *testing.T) {
	tests := []struct {
		name       string
		sse        string
		wantDetail string
	}{
		{
			// 修复前：error 帧解码为空事件，[DONE] 归因 done_no_usage → 不惩罚渠道（红）。
			name:       "typed error 后补 [DONE]",
			sse:        "data: {\"error\":{\"type\":\"server_error\",\"message\":\"boom\"}}\n\ndata: [DONE]\n\n",
			wantDetail: "server_error: boom",
		},
		{
			// 修复前：探测留在解码成功分支内，畸形 error 帧被记为损耗后丢弃，[DONE] 归因 done_no_usage → 不惩罚渠道（红）。
			// 截断帧解不出摘要，Detail 为空，但失败归因必须仍是 error_frame。
			name:       "畸形 JSON error 后补 [DONE]",
			sse:        "data: {\"error\":{\"type\":\n\ndata: [DONE]\n\n",
			wantDetail: "",
		},
		{
			// 修复前：空 error 负载摘要为空，同样被 [DONE] 洗成 done_no_usage（红）。
			name:       "空 error 后补 [DONE]",
			sse:        "data: {\"error\":{}}\n\ndata: [DONE]\n\n",
			wantDetail: "",
		},
		{
			// 行为改变：修复前无 [DONE] 归因 truncated，现在按 error 帧归因 error_frame；
			// 两者都惩罚渠道，仅诊断口径更准确。
			name:       "仅 error 帧无 [DONE]",
			sse:        "data: {\"error\":{\"type\":\"server_error\",\"message\":\"boom\"}}\n\n",
			wantDetail: "server_error: boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 泵级：归因与惩罚。
			pg := build(t, "http://unused")
			_, _, _, err := pg.gw.pumpConvertedStream(context.Background(), sseResponse(tt.sse),
				channelprotocol.ProtocolOpenAIChat, channelprotocol.ProtocolOpenAIChat,
				&protocolconv.LossyReport{}, httptest.NewRecorder())
			if !errors.Is(err, ErrUsageMissing) {
				t.Fatalf("want ErrUsageMissing, got %v", err)
			}
			if got := usageMissingReason(err); got != UsageMissingErrorFrame {
				t.Fatalf("reason want %q, got %q", UsageMissingErrorFrame, got)
			}
			if !usageMissingPunishable(usageMissingReason(err)) {
				t.Fatalf("error_frame 应计入渠道健康惩罚")
			}
			var ue *UsageMissingError
			if !errors.As(err, &ue) || ue.Detail != tt.wantDetail {
				t.Fatalf("detail want %q, got %+v", tt.wantDetail, ue)
			}

			// 端到端：anthropic 入站 → 模型仅可达 openai-chat → 转换泵真正被走到。
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.sse))
			}))
			defer upstream.Close()

			h := build(t, upstream.URL)
			h.chans.mrt.Model.Protocols = channelprotocol.Protocols{channelprotocol.ProtocolOpenAIChat: {}}
			rec := postWithHeaders(t, h.gw, "/v1/messages",
				`{"model":"gpt-4","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"ping"}]}`,
				map[string]string{"x-api-key": "sk-gw-t"})
			if rec.Code != http.StatusOK {
				t.Fatalf("want 200 (stream already responded), got %d", rec.Code)
			}
			// 下游 body 为空是既有行为（泵不合成下游 error 事件，属后续特性），本 PR 只保证计费/健康/日志口径。
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
		})
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

// TestConvertParseFailureReturns400 断言 AC-P2-9：入站请求体「合法 JSON 且含 model，但对其声明协议不合法」
// 时，转换解析失败应立即回 HTTP 400，且绝不发起上游调用（解析失败与候选无关，不 failover）。
func TestConvertParseFailureReturns400(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	// 模型仅可达 anthropic-messages → openai-chat 入站需转换（才会触发请求转换）。
	h.chans.mrt.Model.Protocols = channelprotocol.Protocols{channelprotocol.ProtocolAnthropicMessages: {}}

	// messages 为对象（非数组）→ 合法 JSON 且含 model，但 openai-chat 解析失败。
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","messages":{"bad":"shape"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("入站解析失败不应发起上游调用（不 failover），上游被调用 %d 次", n)
	}
	// 仍落一条 failed 账单（对齐既有 400 路径的留痕口径）。
	if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
		t.Fatalf("解析失败应落 1 条 failed 账单, got %+v", h.billing.reqs)
	}
}

// TestReorderPassthroughFirst 断言新会话候选收敛：可直通的候选按相对顺序前移，
// 需跨协议转换的候选保持相对顺序靠后；无区分度时原序不变（零行为变化）。
func TestReorderPassthroughFirst(t *testing.T) {
	cr := func(id int64) *router.ChannelRoute { return &router.ChannelRoute{ChannelID: id} }
	ids := func(routes []*router.ChannelRoute) []int64 {
		out := make([]int64, 0, len(routes))
		for _, r := range routes {
			out = append(out, r.ChannelID)
		}
		return out
	}
	equal := func(got, want []int64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	tests := []struct {
		name        string
		in          []int64
		pass        map[int64]bool
		want        []int64
		wantReorder bool
	}{
		{"混合：直通前移且各自保序", []int64{1, 2, 3, 4, 5}, map[int64]bool{2: true, 4: true}, []int64{2, 4, 1, 3, 5}, true},
		{"全部直通：原序不变", []int64{1, 2, 3}, map[int64]bool{1: true, 2: true, 3: true}, []int64{1, 2, 3}, false},
		{"全部需转换：原序不变", []int64{1, 2, 3}, map[int64]bool{}, []int64{1, 2, 3}, false},
		{"单候选：不重排", []int64{1}, map[int64]bool{}, []int64{1}, false},
		{"仅第二名可直通：交换", []int64{1, 2}, map[int64]bool{2: true}, []int64{2, 1}, true},
		{"直通已在队首：顺序不变则不标记重排", []int64{1, 2, 3}, map[int64]bool{1: true, 2: true}, []int64{1, 2, 3}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routes := make([]*router.ChannelRoute, 0, len(tt.in))
			for _, id := range tt.in {
				routes = append(routes, cr(id))
			}
			reordered := reorderPassthroughFirst(routes, func(r *router.ChannelRoute) bool { return tt.pass[r.ChannelID] })
			if reordered != tt.wantReorder {
				t.Fatalf("重排标记 want %v, got %v", tt.wantReorder, reordered)
			}
			if got := ids(routes); !equal(got, tt.want) {
				t.Fatalf("候选顺序 want %v, got %v", tt.want, got)
			}
		})
	}
}

// decisionAttempts 取出调用日志决策轨迹的 attempts 列表（JSONB 值 → 通用 map）。
func decisionAttempts(t *testing.T, decision any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(decision)
	if err != nil {
		t.Fatalf("marshal decision: %v", err)
	}
	var dec struct {
		Attempts []map[string]any `json:"attempts"`
	}
	if err := json.Unmarshal(raw, &dec); err != nil {
		t.Fatalf("unmarshal decision: %v", err)
	}
	return dec.Attempts
}

// TestServeDecisionPassthroughReorderMarkers 断言直通重排可观测：
// 确实发生重排时，直通候选轨迹带 passthrough_first=true、被后移的兜底候选带 passthrough_fallback=true；
// 未发生重排时轨迹不出现这两个键（键集与既有行为完全一致）。
func TestServeDecisionPassthroughReorderMarkers(t *testing.T) {
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
	// 候选 1 仅可达 anthropic-messages（需转换 → 兜底）；候选 2 可达 openai-chat（直通）。
	convRoute := &router.ChannelRoute{ChannelID: 1, ChannelKeyID: 1, InternalModelID: "gpt-4-internal", ModelRowID: 7, ExternalModelID: 1}
	directRoute := &router.ChannelRoute{ChannelID: 2, ChannelKeyID: 2, InternalModelID: "gpt-4-internal", ModelRowID: 8, ExternalModelID: 1}

	tests := []struct {
		name      string
		routes    []*router.ChannelRoute
		wantMarks bool
	}{
		{"需转换在前 → 重排后直通前移", []*router.ChannelRoute{convRoute, directRoute}, true},
		{"直通已在前 → 未重排", []*router.ChannelRoute{directRoute, convRoute}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := build(t, upstream.URL)
			h.rrouter.routes = tt.routes
			// 候选 2 的密钥不在运行时 → 该候选被跳过，请求最终命中候选 1（两条轨迹都会落痕）。
			h.chans.mrtByChannel = map[int64]*channel.ModelRuntime{
				1: modelRuntime(7, 1, channelprotocol.Protocols{channelprotocol.ProtocolAnthropicMessages: {}}),
				2: modelRuntime(8, 2, channelprotocol.Protocols{channelprotocol.ProtocolOpenAIChat: {}}),
			}
			rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
				`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
			}
			if len(h.clogs.inserted) == 0 {
				t.Fatalf("缺少调用日志")
			}
			attempts := decisionAttempts(t, h.clogs.inserted[len(h.clogs.inserted)-1].Decision)
			byChannel := make(map[string]map[string]any, len(attempts))
			for _, a := range attempts {
				if id, ok := a["channel_id"].(string); ok {
					byChannel[id] = a
				}
			}
			_, directFirst := byChannel["2"]["passthrough_first"]
			_, directFallback := byChannel["2"]["passthrough_fallback"]
			_, convFirst := byChannel["1"]["passthrough_first"]
			_, convFallback := byChannel["1"]["passthrough_fallback"]
			if tt.wantMarks {
				if !directFirst || !convFallback {
					t.Fatalf("重排后应标记直通/兜底候选: %+v", attempts)
				}
				if directFallback || convFirst {
					t.Fatalf("直通/兜底标记不得互换: %+v", attempts)
				}
			} else if directFirst || directFallback || convFirst || convFallback {
				t.Fatalf("未发生重排时不得追加重排标记: %+v", attempts)
			}
		})
	}
}
