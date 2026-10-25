package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// anthropicEndpoint 是 anthropic-messages 基础协议的内置默认路径（可被渠道协议 path 覆盖）。
const anthropicEndpoint = "/v1/messages"

// AnthropicProvider 实现 Anthropic Messages 协议的 Provider（端点默认 /v1/messages）。
type AnthropicProvider struct{}

// NewAnthropicProvider 创建 anthropic-messages 适配器。
func NewAnthropicProvider() *AnthropicProvider { return &AnthropicProvider{} }

// Name 返回协议名。
func (p *AnthropicProvider) Name() string { return channelprotocol.ProtocolAnthropicMessages }

// BuildUpstreamRequest 构建 POST 上游请求（默认路径 /v1/messages，鉴权由 auth 注入）。
func (p *AnthropicProvider) BuildUpstreamRequest(ctx context.Context, req UpstreamRequest) (*http.Request, error) {
	return buildUpstreamRequest(ctx, req, anthropicEndpoint, "application/json")
}

// anthropicUsage 是 Anthropic Messages 的 usage 结构（宽松数字）。
type anthropicUsage struct {
	InputTokens              any `json:"input_tokens"`
	OutputTokens             any `json:"output_tokens"`
	CacheCreationInputTokens any `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     any `json:"cache_read_input_tokens"`
}

// parseAnthropicUsage 归一化为三段（三段口径见 billing.Usage）：
// Anthropic 的 input_tokens 是「最后一个 cache breakpoint 之后」的 token，不含缓存读/写；
// 而三段口径下「输入」是纯输入（不含缓存读），写缓存的 token 属于输入 token，
// 故 Input = input_tokens + cache_creation_input_tokens。
// 推理（thinking）已包含在 output_tokens 内，不再单列（AC-16）。
func parseAnthropicUsage(raw []byte) (billing.Usage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var u anthropicUsage
	if err := dec.Decode(&u); err != nil {
		return billing.Usage{}, err
	}
	return billing.Usage{
		Input:     numAsInt(u.InputTokens) + numAsInt(u.CacheCreationInputTokens),
		Output:    numAsInt(u.OutputTokens),
		CacheRead: numAsInt(u.CacheReadInputTokens),
	}, nil
}

// ExtractUsage 从非流式响应体解析 token 用量与上游原始 usage；usage 缺失返回 ErrUsageMissing。
func (p *AnthropicProvider) ExtractUsage(body []byte) (billing.Usage, []byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := dec.Decode(&raw); err != nil {
		return billing.Usage{}, nil, err
	}
	if len(raw.Usage) == 0 {
		return billing.Usage{}, nil, ErrUsageMissing
	}
	u, err := parseAnthropicUsage(raw.Usage)
	if err != nil {
		return billing.Usage{}, nil, err
	}
	return u, rawUsageJSON(p.Name(), []json.RawMessage{raw.Usage}), nil
}

// anthropicStreamEvent 流式事件的宽松解析目标。
type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Message struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
	Delta struct {
		Type       string `json:"type"`
		Text       any    `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage json.RawMessage `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// HandleStream 透传 Anthropic SSE 事件：拼接 text_delta，从 message_start（输入/缓存）与
// message_delta（累计输出）汇总 usage。usage 缺失返回 UsageMissingError，并按结束形态区分原因
// （口径见 streamEnd）：见过带 stop_reason 的 message_delta 即正常结束无 usage / 只有 message_stop
// 而没收尾证据 = 疑似补发标记掩盖截断 / 连 message_stop 都没有 = 流被截断 / 流内 error 事件。
// 返回 (usage, 上游原始 usage JSONB 值, assistant 消息 JSON, error)；原始 usage 由两处事件分片合并
// （message_start 的输入/缓存 + message_delta 的累计输出），供诊断渠道口径（call_logs.upstream_usage）。
func (p *AnthropicProvider) HandleStream(ctx context.Context, upstream *http.Response, w http.ResponseWriter) (billing.Usage, []byte, []byte, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	br := bufio.NewReader(upstream.Body)

	var usage billing.Usage
	sawUsage := false
	sawStop := false               // 是否收到 message_stop（终止标记；标记单独不能证明正常结束，见 streamEnd）
	sawStopReason := false         // 是否收到带 stop_reason 的 message_delta（内容层收尾证据）
	sawError := false              // 是否出现 error 事件（计入渠道健康失败）
	var errorFrame string          // error 事件摘要（诊断用）
	var rawParts []json.RawMessage // 上游原始 usage 分片（message_start + message_delta）
	var content string

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return usage, rawUsageJSON(p.Name(), rawParts), responsesAssistant(content), wrapDownstreamWrite(werr)
			}
			if flusher != nil {
				flusher.Flush()
			}
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				data := bytes.TrimSpace(trimmed[len("data:"):])
				if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) {
					var ev anthropicStreamEvent
					if json.Unmarshal(data, &ev) == nil {
						switch ev.Type {
						case "message_stop":
							sawStop = true
						case "error":
							sawError = true
							// 摘要首个非空优先：后续空 error 负载不得清掉先前的诊断信息。
							if d := errorFrameDetail(ev.Error); d != "" && errorFrame == "" {
								errorFrame = d
							}
						case "message_start":
							if len(ev.Message.Usage) > 0 {
								// 零值守卫（口径同 openai-chat / responses / 非流式）：
								// "usage":{} 或全零 usage 不算取到 usage，否则按 0 token 出账且无痕。
								if u, uerr := parseAnthropicUsage(ev.Message.Usage); uerr == nil && !usageZero(u) {
									// Input 已含 cache_creation（parseAnthropicUsage 内合并）。
									usage.Input = u.Input
									usage.CacheRead = u.CacheRead
									sawUsage = true
									rawParts = append(rawParts, ev.Message.Usage)
								}
							}
						case "content_block_delta":
							if s, ok := ev.Delta.Text.(string); ok {
								content += s
							}
						case "message_delta":
							// stop_reason 是内容层收尾证据：上游真实生成完毕才给，代理补不出来。
							if ev.Delta.StopReason != "" {
								sawStopReason = true
							}
							if len(ev.Usage) > 0 {
								if u, uerr := parseAnthropicUsage(ev.Usage); uerr == nil && !usageZero(u) {
									usage.Output = u.Output
									sawUsage = true
									rawParts = append(rawParts, ev.Usage)
								}
							}
						}
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return usage, rawUsageJSON(p.Name(), rawParts), responsesAssistant(content), err
		}
		select {
		case <-ctx.Done():
			return usage, rawUsageJSON(p.Name(), rawParts), responsesAssistant(content), ctx.Err()
		default:
		}
	}

	if !sawUsage {
		end := streamEnd{Terminal: sawStop, ContentComplete: sawStopReason}
		return usage, rawUsageJSON(p.Name(), rawParts), responsesAssistant(content),
			&UsageMissingError{Reason: usageMissingReasonOf(end, sawError), Detail: errorFrame}
	}
	return usage, rawUsageJSON(p.Name(), rawParts), responsesAssistant(content), nil
}
