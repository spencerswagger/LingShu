package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// conversionMatrixProtocols 是跨协议转换矩阵覆盖的三个基础协议。
var conversionMatrixProtocols = []string{
	channelprotocol.ProtocolOpenAIChat,
	channelprotocol.ProtocolOpenAIResponses,
	channelprotocol.ProtocolAnthropicMessages,
}

// matrixInboundPath 返回各基础协议入站端点的路径。
func matrixInboundPath(proto string) string {
	switch proto {
	case channelprotocol.ProtocolOpenAIResponses:
		return "/v1/responses"
	case channelprotocol.ProtocolAnthropicMessages:
		return "/v1/messages"
	default:
		return "/v1/chat/completions"
	}
}

// matrixInboundBody 构造某入站协议形状的请求体（非流/流）。
func matrixInboundBody(proto string, stream bool) string {
	st := "false"
	if stream {
		st = "true"
	}
	switch proto {
	case channelprotocol.ProtocolOpenAIResponses:
		return `{"model":"gpt-4","stream":` + st + `,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]}]}`
	case channelprotocol.ProtocolAnthropicMessages:
		// 含 anthropic 专有字段（system / stop_sequences），使 openai-chat 目标的形状判别不再同义反复：
		// 转换后 system 折叠进 messages、stop_sequences 映射为 stop，均可与直通的 anthropic 体区分。
		return `{"model":"gpt-4","stream":` + st + `,"max_tokens":100,"system":"be nice","stop_sequences":["END"],"messages":[{"role":"user","content":"ping"}]}`
	default:
		return `{"model":"gpt-4","stream":` + st + `,"messages":[{"role":"user","content":"ping"}]}`
	}
}

// matrixSendInbound 以指定入站协议发出一次请求：anthropic 走 x-api-key，其余走 Bearer。
func matrixSendInbound(t *testing.T, gw *Gateway, proto, body string) *httptest.ResponseRecorder {
	t.Helper()
	if proto == channelprotocol.ProtocolAnthropicMessages {
		return postWithHeaders(t, gw, matrixInboundPath(proto), body, map[string]string{"x-api-key": "sk-gw-t"})
	}
	return doRequest(t, gw, matrixInboundPath(proto), "sk-gw-t", body)
}

// matrixTargetUpstreamPathSuffix 返回转换时上游请求路径应满足的后缀：
// 转换时上游 Provider 必须使用目标协议的规范端点（openai-chat → chat/completions、
// openai-responses → responses、anthropic-messages → messages），而非入站端点。
func matrixTargetUpstreamPathSuffix(proto string) string {
	switch proto {
	case channelprotocol.ProtocolAnthropicMessages:
		return "/v1/messages"
	case channelprotocol.ProtocolOpenAIResponses:
		return "/responses"
	default:
		return "/chat/completions"
	}
}

// matrixTargetReqKey 返回用于判别「上游请求体为目标协议形状」的关键字段。
func matrixTargetReqKey(proto string) string {
	switch proto {
	case channelprotocol.ProtocolAnthropicMessages:
		return `"max_tokens"`
	case channelprotocol.ProtocolOpenAIResponses:
		return `"input"`
	default:
		return `"messages"`
	}
}

// matrixTargetReqChecker 断言上游收到的请求体确为目标协议形状（非源协议直通残留）。
// 对 openai-chat 目标额外强化：源为 anthropic 时，直通体与转换体都含 "messages"，仅凭该键
// 无法区分；故追加目标特有判据——不得残留 anthropic 专有键 "stop_sequences"，且须已把
// system 折叠为 messages 内的 system 角色、把 stop_sequences 映射为 stop（这些仅在真正转换后出现）。
func matrixTargetReqChecker(t *testing.T, src, tgt string, body []byte) {
	t.Helper()
	s := string(body)
	if !strings.Contains(s, matrixTargetReqKey(tgt)) {
		t.Fatalf("上游应收到 %s 形状请求体（含 %s），got %s", tgt, matrixTargetReqKey(tgt), body)
	}
	if tgt == channelprotocol.ProtocolOpenAIChat && src == channelprotocol.ProtocolAnthropicMessages {
		if strings.Contains(s, `"stop_sequences"`) {
			t.Fatalf("转换到 openai-chat 后不应残留 anthropic 专有键 stop_sequences，got %s", body)
		}
		if !strings.Contains(s, `"role":"system"`) {
			t.Fatalf("转换到 openai-chat 后 system 应折叠进 messages（system 角色），got %s", body)
		}
		if !strings.Contains(s, `"stop":"END"`) {
			t.Fatalf("转换到 openai-chat 后 stop_sequences 应映射为 stop，got %s", body)
		}
	}
}

// matrixSourceNonStreamKeys 返回用于判别「非流式下游响应为源协议形状」的关键字段。
func matrixSourceNonStreamKeys(proto string) []string {
	switch proto {
	case channelprotocol.ProtocolAnthropicMessages:
		return []string{`"content"`, `"stop_reason"`}
	case channelprotocol.ProtocolOpenAIResponses:
		return []string{`"output"`}
	default:
		return []string{`"choices"`}
	}
}

// matrixSourceStreamMarkers 返回用于判别「流式下游 SSE 为源协议帧」的标志串。
func matrixSourceStreamMarkers(proto string) []string {
	if proto == channelprotocol.ProtocolAnthropicMessages {
		return []string{"event: message_start"}
	}
	return []string{"data: "}
}

// matrixTargetNonStreamResp 返回目标协议形状的有效非流式响应（含可从上游协议解析的三段 usage）。
func matrixTargetNonStreamResp(proto string) string {
	switch proto {
	case channelprotocol.ProtocolAnthropicMessages:
		return `{"id":"msg1","type":"message","role":"assistant","model":"gpt-4-internal",` +
			`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":10,"output_tokens":5}}`
	case channelprotocol.ProtocolOpenAIResponses:
		return `{"id":"resp1","object":"response","status":"completed","model":"gpt-4-internal",` +
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],` +
			`"usage":{"input_tokens":14,"output_tokens":9}}`
	default:
		return `{"id":"c1","object":"chat.completion","model":"gpt-4-internal",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":12,"completion_tokens":7}}`
	}
}

// matrixTargetStreamResp 返回目标协议形状的有效 SSE 流（含文本增量与三段 usage）。
func matrixTargetStreamResp(proto string) string {
	switch proto {
	case channelprotocol.ProtocolAnthropicMessages:
		return "event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"gpt-4-internal","content":[],"usage":{"input_tokens":20,"output_tokens":1}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
			"event: content_block_stop\n" +
			`data: {"type":"content_block_stop","index":0}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n"
	case channelprotocol.ProtocolOpenAIResponses:
		return `data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":30,"output_tokens":8}}}` + "\n\n"
	default:
		return `data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n" +
			`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
			`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":22,"completion_tokens":6}}` + "\n\n" +
			"data: [DONE]\n\n"
	}
}

// matrixDirectPassBody 返回「直通」（源=目标）请求体：顶层键按字节序排列，
// 使网关的 rewriteModel（模型名替换 + 顶层键重排）输出与原文逐字节一致。
func matrixDirectPassBody(proto string) string {
	switch proto {
	case channelprotocol.ProtocolOpenAIResponses:
		return `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]}],"model":"gpt-4-internal","stream":false}`
	case channelprotocol.ProtocolAnthropicMessages:
		return `{"max_tokens":100,"messages":[{"role":"user","content":"ping"}],"model":"gpt-4-internal","stream":false}`
	default:
		return `{"messages":[{"role":"user","content":"ping"}],"model":"gpt-4-internal","stream":false}`
	}
}

// TestProtocolConversionMatrix 是跨协议转换的端到端矩阵：
// 覆盖 {openai-chat, openai-responses, anthropic-messages} 三个协议两两之间的 6 个有序方向，
// 每个方向分别验证非流式与流式：模型仅可达目标协议强制触发转换，
// 上游必须收到目标协议形状请求体、下游必须收到源协议形状响应；计费 usage 三段口径非零。
// 另含 3 个「直通」（源=目标）用例，断言上游逐字节收到原始入站请求体（不触发转换）。
func TestProtocolConversionMatrix(t *testing.T) {
	for _, src := range conversionMatrixProtocols {
		for _, tgt := range conversionMatrixProtocols {
			if src == tgt {
				continue
			}
			for _, stream := range []bool{false, true} {
				name := src + "-to-" + tgt
				if stream {
					name += "-stream"
				} else {
					name += "-nonstream"
				}
				t.Run(name, func(t *testing.T) {
					var gotBody []byte
					var gotPath string
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						gotPath = r.URL.Path
						gotBody, _ = io.ReadAll(r.Body)
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							w.WriteHeader(http.StatusOK)
							_, _ = w.Write([]byte(matrixTargetStreamResp(tgt)))
							return
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write([]byte(matrixTargetNonStreamResp(tgt)))
					}))
					defer upstream.Close()

					h := build(t, upstream.URL)
					// 模型仅可达目标协议 → 入站源协议无法直通，必须转换为目标协议。
					h.chans.mrt.Model.Protocols = channelprotocol.Protocols{tgt: {}}

					rec := matrixSendInbound(t, h.gw, src, matrixInboundBody(src, stream))
					if rec.Code != http.StatusOK {
						t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
					}
					// (a) 上游收到目标协议形状请求体（非源协议直通残留）。
					matrixTargetReqChecker(t, src, tgt, gotBody)
					// (a2) 转换时上游必须用目标协议的规范端点（而非入站端点）。
					if suffix := matrixTargetUpstreamPathSuffix(tgt); !strings.HasSuffix(gotPath, suffix) {
						t.Fatalf("转换到 %s 时上游路径应以 %q 结尾（规范端点），got %q", tgt, suffix, gotPath)
					}
					// (b) 下游收到源协议形状响应体/SSE。
					body := rec.Body.String()
					if stream {
						for _, marker := range matrixSourceStreamMarkers(src) {
							if !strings.Contains(body, marker) {
								t.Fatalf("下游 %s SSE 应含 %q，got %s", src, marker, body)
							}
						}
					} else {
						for _, key := range matrixSourceNonStreamKeys(src) {
							if !strings.Contains(body, key) {
								t.Fatalf("下游 %s 响应应含 %q，got %s", src, key, body)
							}
						}
					}
					// (c) 计费 usage 三段口径非零（取自上游物理协议）。
					if len(h.billing.reqs) != 1 {
						t.Fatalf("want 1 billing, got %d: %+v", len(h.billing.reqs), h.billing.reqs)
					}
					if u := h.billing.reqs[0].Tokens; u.Input <= 0 || u.Output <= 0 {
						t.Fatalf("计费 usage 应非零（三段口径），got %+v", u)
					}
				})
			}
		}
	}

	// 直通 sanity：源=目标时不发生转换，上游逐字节收到原始入站请求体。
	t.Run("direct-pass", func(t *testing.T) {
		for _, proto := range conversionMatrixProtocols {
			t.Run(proto, func(t *testing.T) {
				body := matrixDirectPassBody(proto)
				var gotBody []byte
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gotBody, _ = io.ReadAll(r.Body)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(matrixTargetNonStreamResp(proto)))
				}))
				defer upstream.Close()

				h := build(t, upstream.URL)
				// 模型仅可达该协议 → 入站同协议直通，不触发转换。
				h.chans.mrt.Model.Protocols = channelprotocol.Protocols{proto: {}}

				rec := matrixSendInbound(t, h.gw, proto, body)
				if rec.Code != http.StatusOK {
					t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
				}
				if !bytes.Equal(gotBody, []byte(body)) {
					t.Fatalf("直通上游请求体应与原始入站字节一致（未被转换）\n want %s\n got  %s", body, gotBody)
				}
			})
		}
	})
}
