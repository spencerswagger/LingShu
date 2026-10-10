package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/team/llmgateway/internal/domain/billing"
)

// OpenAI-compat 支持的端点（相对 baseURL 的路径）。
const (
	EndpointChatCompletions = "chat/completions"
	EndpointCompletions     = "completions"
	EndpointEmbeddings      = "embeddings"
)

// OpenAIProvider 实现 OpenAI-compat 协议的 Provider。
// endpoint 决定拼出的上游路径，如 "chat/completions"。
type OpenAIProvider struct {
	endpoint string
}

// NewOpenAIProvider 创建 openai-compat 适配器，endpoint 为上表中的常量。
func NewOpenAIProvider(endpoint string) *OpenAIProvider {
	return &OpenAIProvider{endpoint: endpoint}
}

// Name 返回协议名。
func (p *OpenAIProvider) Name() string { return endpointName(p.endpoint) }

func endpointName(ep string) string {
	switch ep {
	case EndpointChatCompletions:
		return "openai-chat"
	case EndpointCompletions:
		return "openai-completions"
	case EndpointEmbeddings:
		return "openai-embeddings"
	}
	return "openai-" + ep
}

// BuildUpstreamRequest 构建 POST 上游请求：baseURL(去尾部 /) + / + endpoint，
// 注入 Content-Type 与 Authorization: Bearer cred，body 原样透传。
func (p *OpenAIProvider) BuildUpstreamRequest(ctx context.Context, baseURL, cred string, body []byte, _ bool) (*http.Request, error) {
	url := strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(p.endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cred)
	return req, nil
}

// usageObj 是对 openai `usage` 对象的宽松结构，数字可为整数或浮点。
type usageObj struct {
	PromptTokens        any `json:"prompt_tokens"`
	CompletionTokens    any `json:"completion_tokens"`
	TotalTokens         any `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens any `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens any `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// parseUsageObj 解析 usage JSON（支持整数/浮点数字）。
func parseUsageObj(raw []byte) (billing.Usage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var u usageObj
	if err := dec.Decode(&u); err != nil {
		return billing.Usage{}, err
	}
	output := numAsInt(u.CompletionTokens)
	reasoning := numAsInt(u.CompletionTokensDetails.ReasoningTokens)
	// openai/deepseek 惯例：completion_tokens 已包含推理 token，而推理单独给出并单独计费，
	// 从 output 中剔除推理部分，避免同一段 token 被输出价与推理价重复累计。
	if reasoning > 0 && output >= reasoning {
		output -= reasoning
	}
	return billing.Usage{
		Input:     numAsInt(u.PromptTokens),
		Output:    output,
		CacheRead: numAsInt(u.PromptTokensDetails.CachedTokens),
		Reasoning: reasoning,
	}, nil
}

// numAsInt 宽松地把 JSON 数字（json.Number/float64）转 int64，失败给 0。
func numAsInt(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
		if f, err := n.Float64(); err == nil {
			return int64(f)
		}
	case float64:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

// usageZero 判断 usage 是否全零。
func usageZero(u billing.Usage) bool {
	return u.Input == 0 && u.Output == 0 && u.CacheRead == 0 &&
		u.CacheWrite == 0 && u.Reasoning == 0
}

// ExtractUsage 从非流式响应体解析 token 用量；缺字段容错给 0。
func (p *OpenAIProvider) ExtractUsage(body []byte) (billing.Usage, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := dec.Decode(&raw); err != nil {
		return billing.Usage{}, err
	}
	if len(raw.Usage) == 0 {
		return billing.Usage{}, nil
	}
	return parseUsageObj(raw.Usage)
}

// HandleStream 透传 SSE：逐行写回下游并 Flush，同时从 data 行提取 usage 与拼装 assistant 消息。
// 若流中没有 usage（未开启 include_usage），按 delta content 字符数估算 completion tokens（*0.25）。
// 返回 (usage, 拼装完成的 assistant 消息 JSON, error)；assistant 消息供 call_log 记录流式响应内容。
func (p *OpenAIProvider) HandleStream(ctx context.Context, upstream *http.Response, w http.ResponseWriter) (billing.Usage, []byte, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	br := bufio.NewReader(upstream.Body)

	var usage billing.Usage
	var estChars int64
	sawUsage := false
	asm := newStreamAssembler() // 按 data 行增量拼装完整 assistant 消息（call_log 采集）

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return usage, asm.marshal(), werr
			}
			if flusher != nil {
				flusher.Flush()
			}
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				data := bytes.TrimSpace(trimmed[len("data:"):])
				if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) {
					u, has, chars := parseStreamData(data)
					if has {
						usage = u
						sawUsage = true
					}
					estChars += chars
					asm.apply(data)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return usage, asm.marshal(), err
		}
		// 请求取消则中止透传（上游连接由 ctx 控制关闭）。
		select {
		case <-ctx.Done():
			return usage, asm.marshal(), ctx.Err()
		default:
		}
	}

	if !sawUsage {
		// 无 usage 时按 content 字符估算 completion tokens。
		usage.Output = int64(float64(estChars) * 0.25)
	}
	return usage, asm.marshal(), nil
}

// streamToolCall 拼装中的单个工具调用（index → 增量累积状态）。
type streamToolCall struct {
	Index        int
	ID           string
	Type         string
	FunctionName string // function.name 增量拼接
	Arguments    string // function.arguments 增量拼接（字符串直拼，不做 JSON 解析）
}

// streamAssembler 流式 data 行 → 完整 assistant 消息的拼装器：
// 逐块提取 delta.content / delta.reasoning_content / delta.tool_calls（index/name/arguments 增量）并拼接。
type streamAssembler struct {
	content          string
	reasoningContent string
	toolCalls        map[int]streamToolCall
}

// newStreamAssembler 创建空拼装器。
func newStreamAssembler() *streamAssembler {
	return &streamAssembler{toolCalls: make(map[int]streamToolCall)}
}

// asmDeltaRow 拼装所需的 delta 字段宽松解析目标（与 parseStreamData 独立，不改既有 usage 提取）。
type asmDeltaRow struct {
	Choices []struct {
		Delta struct {
			Content          any `json:"content"`
			ReasoningContent any `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    *int `json:"index"`
				ID       any  `json:"id"`
				Type     any  `json:"type"`
				Function struct {
					Name      any `json:"name"`
					Arguments any `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Content any `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// apply 解析单个 SSE data 块，把 content/reasoning_content/tool_calls 增量拼进装配器。
func (a *streamAssembler) apply(data []byte) {
	var obj asmDeltaRow
	if err := json.Unmarshal(data, &obj); err != nil {
		return
	}
	for _, c := range obj.Choices {
		if s, ok := c.Delta.Content.(string); ok {
			a.content += s
		}
		// 兼容部分上游在 message.content（而非 delta）里给首段内容。
		if a.content == "" && c.Message.Content != nil {
			if s, ok := c.Message.Content.(string); ok {
				a.content += s
			}
		}
		if s, ok := c.Delta.ReasoningContent.(string); ok {
			a.reasoningContent += s
		}
		for _, tc := range c.Delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			cur, ok := a.toolCalls[idx]
			if !ok {
				cur = streamToolCall{Index: idx}
			}
			if tc.ID != nil {
				if s, ok := tc.ID.(string); ok && cur.ID == "" {
					cur.ID = s
				}
			}
			if tc.Type != nil {
				if s, ok := tc.Type.(string); ok && cur.Type == "" {
					cur.Type = s
				}
			}
			if tc.Function.Name != nil {
				if s, ok := tc.Function.Name.(string); ok {
					cur.FunctionName += s
				}
			}
			if tc.Function.Arguments != nil {
				if s, ok := tc.Function.Arguments.(string); ok {
					cur.Arguments += s
				}
			}
			a.toolCalls[idx] = cur
		}
	}
}

// marshal 输出拼装完成的 assistant 消息 JSONB：
// {"role":"assistant","content":"...","reasoning_content":"...","tool_calls":[{...}]}，
// 空字段不输出；tool_calls 按 index 升序（arguments 为字符串直接拼接结果）。
func (a *streamAssembler) marshal() []byte {
	msg := map[string]any{"role": "assistant"}
	if a.content != "" {
		msg["content"] = a.content
	}
	if a.reasoningContent != "" {
		msg["reasoning_content"] = a.reasoningContent
	}
	if len(a.toolCalls) > 0 {
		indices := make([]int, 0, len(a.toolCalls))
		for i := range a.toolCalls {
			indices = append(indices, i)
		}
		sort.Ints(indices)
		tcs := make([]map[string]any, 0, len(indices))
		for _, i := range indices {
			tc := a.toolCalls[i]
			m := map[string]any{"index": i}
			if tc.ID != "" {
				m["id"] = tc.ID
			}
			if tc.Type != "" {
				m["type"] = tc.Type
			}
			fn := make(map[string]any)
			if tc.FunctionName != "" {
				fn["name"] = tc.FunctionName
			}
			if tc.Arguments != "" {
				fn["arguments"] = tc.Arguments
			}
			if len(fn) > 0 {
				m["function"] = fn
			}
			tcs = append(tcs, m)
		}
		msg["tool_calls"] = tcs
	}
	b, _ := json.Marshal(msg)
	return b
}

// streamData 流式 data 行的宽松解析目标。
type streamData struct {
	Choices []struct {
		Delta struct {
			Content          any `json:"content"`
			ReasoningContent any `json:"reasoning_content"`
		} `json:"delta"`
		Message struct {
			Content any `json:"content"`
		} `json:"message"`
		Usage json.RawMessage `json:"usage"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

// parseStreamData 解析单个 SSE data 块，返回：命中 usage、flag、累计 content 字符数。
func parseStreamData(data []byte) (billing.Usage, bool, int64) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var obj streamData
	if err := dec.Decode(&obj); err != nil {
		return billing.Usage{}, false, 0
	}
	var chars int64
	for _, c := range obj.Choices {
		if s, ok := c.Delta.Content.(string); ok {
			chars += int64(len(s))
		}
		if s, ok := c.Delta.ReasoningContent.(string); ok {
			chars += int64(len(s))
		}
		if s, ok := c.Message.Content.(string); ok {
			chars += int64(len(s))
		}
		if len(c.Usage) > 0 {
			if u, err := parseUsageObj(c.Usage); err == nil && !usageZero(u) {
				return u, true, chars
			}
		}
	}
	if len(obj.Usage) > 0 {
		if u, err := parseUsageObj(obj.Usage); err == nil && !usageZero(u) {
			return u, true, chars
		}
	}
	return billing.Usage{}, false, chars
}
