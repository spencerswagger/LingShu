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

// parseAnthropicUsage 归一化为五段：推理 token 已包含在 output 中，并入 output（AC-16，不单列 reasoning）。
func parseAnthropicUsage(raw []byte) (billing.Usage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var u anthropicUsage
	if err := dec.Decode(&u); err != nil {
		return billing.Usage{}, err
	}
	return billing.Usage{
		Input:      numAsInt(u.InputTokens),
		Output:     numAsInt(u.OutputTokens),
		CacheRead:  numAsInt(u.CacheReadInputTokens),
		CacheWrite: numAsInt(u.CacheCreationInputTokens),
	}, nil
}

// ExtractUsage 从非流式响应体解析 token 用量；usage 缺失返回 ErrUsageMissing。
func (p *AnthropicProvider) ExtractUsage(body []byte) (billing.Usage, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := dec.Decode(&raw); err != nil {
		return billing.Usage{}, err
	}
	if len(raw.Usage) == 0 {
		return billing.Usage{}, ErrUsageMissing
	}
	return parseAnthropicUsage(raw.Usage)
}

// anthropicStreamEvent 流式事件的宽松解析目标。
type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Message struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
	Delta struct {
		Type string `json:"type"`
		Text any    `json:"text"`
	} `json:"delta"`
	Usage json.RawMessage `json:"usage"`
}

// HandleStream 透传 Anthropic SSE 事件：拼接 text_delta，从 message_start（输入/缓存）与
// message_delta（累计输出）汇总 usage。usage 缺失返回 ErrUsageMissing（内容已透传，按 AC-17 处理）。
func (p *AnthropicProvider) HandleStream(ctx context.Context, upstream *http.Response, w http.ResponseWriter) (billing.Usage, []byte, error) {
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
	var content string

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return usage, responsesAssistant(content), werr
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
						case "message_start":
							if len(ev.Message.Usage) > 0 {
								if u, uerr := parseAnthropicUsage(ev.Message.Usage); uerr == nil {
									usage.Input = u.Input
									usage.CacheRead = u.CacheRead
									usage.CacheWrite = u.CacheWrite
									sawUsage = true
								}
							}
						case "content_block_delta":
							if s, ok := ev.Delta.Text.(string); ok {
								content += s
							}
						case "message_delta":
							if len(ev.Usage) > 0 {
								if u, uerr := parseAnthropicUsage(ev.Usage); uerr == nil {
									usage.Output = u.Output
									sawUsage = true
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
			return usage, responsesAssistant(content), err
		}
		select {
		case <-ctx.Done():
			return usage, responsesAssistant(content), ctx.Err()
		default:
		}
	}

	if !sawUsage {
		return usage, responsesAssistant(content), ErrUsageMissing
	}
	return usage, responsesAssistant(content), nil
}
