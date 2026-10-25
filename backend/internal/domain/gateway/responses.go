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
}

// parseResponsesUsage 把 Responses usage 归一化为三段：
// Input = input_tokens − cached_tokens（纯输入，不含缓存读）；Output = output_tokens（含推理）。
func parseResponsesUsage(raw []byte) (billing.Usage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var u responsesUsage
	if err := dec.Decode(&u); err != nil {
		return billing.Usage{}, err
	}
	cached := numAsInt(u.InputTokensDetails.CachedTokens)
	input := numAsInt(u.InputTokens) - cached
	if input < 0 {
		input = 0 // 防御：上游 cached_tokens 异常大于 input_tokens 时按 0 计
	}
	return billing.Usage{
		Input:     input,
		Output:    numAsInt(u.OutputTokens),
		CacheRead: cached,
	}, nil
}

// ExtractUsage 从非流式响应体解析 token 用量与上游原始 usage；usage 缺失返回 ErrUsageMissing。
func (p *ResponsesProvider) ExtractUsage(body []byte) (billing.Usage, []byte, error) {
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
	u, err := parseResponsesUsage(raw.Usage)
	if err != nil {
		return billing.Usage{}, nil, err
	}
	return u, rawUsageJSON(p.Name(), []json.RawMessage{raw.Usage}), nil
}

// responsesStreamEvent 流式事件的宽松解析目标。
type responsesStreamEvent struct {
	Type     string `json:"type"`
	Delta    any    `json:"delta"`
	Response struct {
		Usage json.RawMessage `json:"usage"`
		Error json.RawMessage `json:"error"`
	} `json:"response"`
}

// HandleStream 透传 Responses SSE 事件：增量拼接 output_text，从 response.completed 取 usage。
// usage 缺失返回 UsageMissingError，并按结束形态区分原因：收到 response.completed/incomplete
// （正常结束但无 usage，不惩罚渠道）/ 未收到即 EOF（流被截断）/ response.failed（流内错误）。
// 返回 (usage, 上游原始 usage JSONB 值, assistant 消息 JSON, error)。
func (p *ResponsesProvider) HandleStream(ctx context.Context, upstream *http.Response, w http.ResponseWriter) (billing.Usage, []byte, []byte, error) {
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
	sawDone := false               // 是否收到 response.completed/incomplete（区分「正常结束无 usage」与「流被截断」）
	sawError := false              // 是否出现 response.failed（计入渠道健康失败）
	var errorFrame string          // response.failed 摘要（诊断用）
	var rawParts []json.RawMessage // 上游原始 usage 分片（诊断渠道口径，call_logs.upstream_usage）
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
				if len(data) > 0 && bytes.Equal(data, []byte("[DONE]")) {
					// 部分 Responses 兼容上游仍以 [DONE] 收尾：与转换泵口径一致视为正常结束。
					// 但上游可能先报 response.failed 再补 [DONE]；此时 [DONE] 只是收口，
					// 不能把失败洗成「正常结束无 usage」，否则渠道健康惩罚会丢失。
					if !sawError {
						sawDone = true
					}
				} else if len(data) > 0 {
					var ev responsesStreamEvent
					if json.Unmarshal(data, &ev) == nil {
						switch ev.Type {
						case "response.output_text.delta":
							if s, ok := ev.Delta.(string); ok {
								content += s
							}
						case "response.completed", "response.incomplete":
							sawDone = true
							if len(ev.Response.Usage) > 0 {
								if u, uerr := parseResponsesUsage(ev.Response.Usage); uerr == nil {
									usage = u
									sawUsage = true
									rawParts = append(rawParts, ev.Response.Usage)
								}
							}
						case "response.failed":
							sawError = true
							errorFrame = errorFrameDetail(ev.Response.Error)
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
		return usage, rawUsageJSON(p.Name(), rawParts), responsesAssistant(content),
			&UsageMissingError{Reason: usageMissingReasonOf(sawDone, sawError), Detail: errorFrame}
	}
	return usage, rawUsageJSON(p.Name(), rawParts), responsesAssistant(content), nil
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
