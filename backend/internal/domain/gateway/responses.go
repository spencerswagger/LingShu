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

// responsesEndpoint 是 openai-responses 基础协议的内置默认路径（可被渠道协议 path 覆盖）。
const responsesEndpoint = "/responses"

// ResponsesProvider 实现 OpenAI Responses 协议的 Provider（端点默认 /responses）。
type ResponsesProvider struct{}

// NewResponsesProvider 创建 openai-responses 适配器。
func NewResponsesProvider() *ResponsesProvider { return &ResponsesProvider{} }

// Name 返回协议名。
func (p *ResponsesProvider) Name() string { return channelprotocol.ProtocolOpenAIResponses }

// BuildUpstreamRequest 构建 POST 上游请求（默认路径 /responses）。
func (p *ResponsesProvider) BuildUpstreamRequest(ctx context.Context, req UpstreamRequest) (*http.Request, error) {
	return buildUpstreamRequest(ctx, req, responsesEndpoint, "application/json")
}

// responsesUsage 是 Responses API 的 usage 结构（宽松数字）。
type responsesUsage struct {
	InputTokens        any `json:"input_tokens"`
	OutputTokens       any `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens any `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens any `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// parseResponsesUsage 把 Responses usage 归一化为五段（推理 token 单列，从 output 中剔除）。
func parseResponsesUsage(raw []byte) (billing.Usage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var u responsesUsage
	if err := dec.Decode(&u); err != nil {
		return billing.Usage{}, err
	}
	output := numAsInt(u.OutputTokens)
	reasoning := numAsInt(u.OutputTokensDetails.ReasoningTokens)
	if reasoning > 0 && output >= reasoning {
		output -= reasoning
	}
	return billing.Usage{
		Input:     numAsInt(u.InputTokens),
		Output:    output,
		CacheRead: numAsInt(u.InputTokensDetails.CachedTokens),
		Reasoning: reasoning,
	}, nil
}

// ExtractUsage 从非流式响应体解析 token 用量；usage 缺失返回 ErrUsageMissing。
func (p *ResponsesProvider) ExtractUsage(body []byte) (billing.Usage, error) {
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
	return parseResponsesUsage(raw.Usage)
}

// responsesStreamEvent 流式事件的宽松解析目标。
type responsesStreamEvent struct {
	Type     string `json:"type"`
	Delta    any    `json:"delta"`
	Response struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"response"`
}

// HandleStream 透传 Responses SSE 事件：增量拼接 output_text，从 response.completed 取 usage。
// usage 缺失返回 ErrUsageMissing（内容已透传，由调用方按 AC-17 放宽：估算计费并禁用渠道）。
func (p *ResponsesProvider) HandleStream(ctx context.Context, upstream *http.Response, w http.ResponseWriter) (billing.Usage, []byte, error) {
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
					var ev responsesStreamEvent
					if json.Unmarshal(data, &ev) == nil {
						switch ev.Type {
						case "response.output_text.delta":
							if s, ok := ev.Delta.(string); ok {
								content += s
							}
						case "response.completed":
							if len(ev.Response.Usage) > 0 {
								if u, uerr := parseResponsesUsage(ev.Response.Usage); uerr == nil {
									usage = u
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

// responsesAssistant 拼装 Responses 流式输出为 assistant 消息 JSON（call_log 用）。
func responsesAssistant(content string) []byte {
	msg := map[string]any{"role": "assistant"}
	if content != "" {
		msg["content"] = content
	}
	b, _ := json.Marshal(msg)
	return b
}
