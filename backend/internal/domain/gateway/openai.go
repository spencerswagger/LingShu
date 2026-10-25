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

	"github.com/team/llmgateway/internal/domain/billing"
)

// OpenAI-compat 支持的端点（相对 baseURL 的路径）。
const (
	EndpointChatCompletions = "chat/completions"
	EndpointCompletions     = "completions"
	EndpointEmbeddings      = "embeddings"
)

// 新增基础协议的入站端点标识（相对 /v1/ 的最后一段；作为 Provider/eval 的端点键）。
const (
	EndpointResponses = "responses"
	EndpointMessages  = "messages"
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

// BuildUpstreamRequest 构建 POST 上游请求：基址 + endpoint（协议 path 覆盖优先），
// 注入 Content-Type + 按头策略/鉴权注入请求头，body 原样透传。
func (p *OpenAIProvider) BuildUpstreamRequest(ctx context.Context, req UpstreamRequest) (*http.Request, error) {
	return buildUpstreamRequest(ctx, req, p.endpoint, "application/json")
}

// usageObj 是对 openai `usage` 对象的宽松结构，数字可为整数或浮点。
// 说明：OpenAI 系的 prompt_tokens 是「含」口径——命中缓存的 cached_tokens 是它的子集；
// 百炼显式缓存新建的 cache_creation_input_tokens 同样包含在 prompt_tokens 内。
// DeepSeek 原生风格：prompt_tokens = prompt_cache_hit_tokens + prompt_cache_miss_tokens
// （同为「含」口径），命中数放在顶层 prompt_cache_hit_tokens；部分上游只给这个字段而不给
// prompt_tokens_details.cached_tokens，必须兜底识别，否则缓存读会被按全价输入计。
type usageObj struct {
	PromptTokens         any `json:"prompt_tokens"`
	CompletionTokens     any `json:"completion_tokens"`
	TotalTokens          any `json:"total_tokens"`
	PromptCacheHitTokens any `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails  struct {
		CachedTokens any `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// parseUsageObj 解析 usage JSON（支持整数/浮点数字），归一化为三段：
// Input = prompt_tokens − 缓存命中数（纯输入，不含缓存读；缓存写 token 天然已在 prompt_tokens 内）；
// Output = completion_tokens（含推理，不再从输出中剥离 reasoning 单独计费）。
// 缓存命中数取值：优先 prompt_tokens_details.cached_tokens，为空或 0 时回落顶层 prompt_cache_hit_tokens
// （DeepSeek 原生风格；两者同义，官方文档明确 cached_tokens 与 prompt_cache_hit_tokens 相同）。
func parseUsageObj(raw []byte) (billing.Usage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var u usageObj
	if err := dec.Decode(&u); err != nil {
		return billing.Usage{}, err
	}
	cached := numAsInt(u.PromptTokensDetails.CachedTokens)
	if cached == 0 {
		cached = numAsInt(u.PromptCacheHitTokens)
	}
	input := numAsInt(u.PromptTokens) - cached
	if input < 0 {
		input = 0 // 防御：上游 cached_tokens 异常大于 prompt_tokens 时按 0 计
	}
	return billing.Usage{
		Input:     input,
		Output:    numAsInt(u.CompletionTokens),
		CacheRead: cached,
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
	return u.Input == 0 && u.Output == 0 && u.CacheRead == 0
}

// ExtractUsage 从非流式响应体解析 token 用量与上游原始 usage；usage 缺失返回 ErrUsageMissing（AC-17 放宽）。
func (p *OpenAIProvider) ExtractUsage(body []byte) (billing.Usage, []byte, error) {
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
	u, err := parseUsageObj(raw.Usage)
	if err != nil {
		return billing.Usage{}, nil, err
	}
	return u, rawUsageJSON(p.Name(), []json.RawMessage{raw.Usage}), nil
}

// HandleStream 透传 SSE：逐行写回下游并 Flush，同时从 data 行提取 usage 与拼装 assistant 消息。
// usage 缺失时返回 UsageMissingError（可 errors.Is(ErrUsageMissing) 判定），并按流结束形态区分原因：
// 收到 [DONE] 但无 usage（上游忽略 include_usage）/ 未收到 [DONE] 即 EOF（上游流被截断）/ 流内 error 帧。
// 返回 (usage, 上游原始 usage JSONB 值, 拼装完成的 assistant 消息 JSON, error)；
// assistant 消息供 call_log 记录流式响应内容，原始 usage 供诊断渠道口径（call_logs.upstream_usage）。
func (p *OpenAIProvider) HandleStream(ctx context.Context, upstream *http.Response, w http.ResponseWriter) (billing.Usage, []byte, []byte, error) {
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
	sawDone := false               // 是否收到流结束标记 [DONE]（区分「正常结束无 usage」与「流被截断」）
	sawError := false              // 是否出现流内 error 帧（诊断与惩罚归因用）
	var errorFrame string          // 流内 error 帧摘要（诊断用；空=无摘要）
	var rawParts []json.RawMessage // 上游原始 usage 分片（多条累计时按同名键后写覆盖）
	asm := newStreamAssembler()    // 按 data 行增量拼装完整 assistant 消息（call_log 采集）

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return usage, rawUsageJSON(p.Name(), rawParts), asm.marshal(), wrapDownstreamWrite(werr)
			}
			if flusher != nil {
				flusher.Flush()
			}
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				data := bytes.TrimSpace(trimmed[len("data:"):])
				if len(data) > 0 && bytes.Equal(data, []byte("[DONE]")) {
					sawDone = true
				} else if len(data) > 0 {
					u, raw, has, errDetail := parseStreamData(data)
					if errDetail != "" {
						sawError = true
						errorFrame = errDetail
					}
					if has {
						usage = u
						sawUsage = true
						rawParts = append(rawParts, raw)
					}
					asm.apply(data)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return usage, rawUsageJSON(p.Name(), rawParts), asm.marshal(), err
		}
		// 请求取消则中止透传（上游连接由 ctx 控制关闭）。
		select {
		case <-ctx.Done():
			return usage, rawUsageJSON(p.Name(), rawParts), asm.marshal(), ctx.Err()
		default:
		}
	}

	if !sawUsage {
		return usage, rawUsageJSON(p.Name(), rawParts), asm.marshal(),
			&UsageMissingError{Reason: usageMissingReasonOf(sawDone, sawError), Detail: errorFrame}
	}
	return usage, rawUsageJSON(p.Name(), rawParts), asm.marshal(), nil
}

// ensureIncludeUsage 为 OpenAI Chat Completions 的流式请求幂等补 stream_options.include_usage=true。
// 平台按 usage 计费，客户端未显式请求 usage 时由网关补齐（平台必需参数）；已有该键为 true 时原样返回，
// 其余 stream_options 键保留。仅限 chat/completions 端点：legacy completions/embeddings 未定义该参数
// （注入可能被上游按未知参数拒绝），非 OpenAI 兼容协议同样不改写；解析失败时按原样返回。
//
// 取舍：判定「include_usage 已为 true」必须确认它是顶层 stream_options 的直接子键。
// 字节级启发式无法可靠区分顶层与嵌套（正文/其它键里的同名字面量），宁可每个流式请求
// 多花一次 json.Unmarshal，也不做无法保证层级的字节匹配（漏注入会让平台拿不到 usage）。
func ensureIncludeUsage(body []byte, protocol, endpoint string) []byte {
	if protocol != "openai-chat" || endpoint != EndpointChatCompletions {
		return body
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if !bytes.Equal(bytes.TrimSpace(obj["stream"]), []byte("true")) {
		return body
	}
	rawOpts, hasOpts := obj["stream_options"]
	if !hasOpts || len(bytes.TrimSpace(rawOpts)) == 0 {
		// 无 stream_options：把新键直接插到对象尾部，其余字节原样保留（避免整包重序列化）。
		return injectStreamOptions(body)
	}
	opts := make(map[string]json.RawMessage)
	if err := json.Unmarshal(rawOpts, &opts); err != nil {
		opts = make(map[string]json.RawMessage) // 结构异常：以平台必需参数为准重建
	}
	if opts == nil {
		// json.Unmarshal("null", &opts) 会把 map 置 nil，赋值前必须重新初始化。
		opts = make(map[string]json.RawMessage)
	}
	if raw, ok := opts["include_usage"]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("true")) {
		return body
	}
	opts["include_usage"] = json.RawMessage("true")
	encOpts, err := json.Marshal(opts)
	if err != nil {
		return body
	}
	obj["stream_options"] = encOpts
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// injectStreamOptions 在请求体对象末尾插入 stream_options.include_usage=true（保持其余字节不变）。
// 仅在请求体已确认是含 stream:true 的非空对象时调用，故无需处理空对象。
func injectStreamOptions(body []byte) []byte {
	trimmed := bytes.TrimRight(body, " \t\r\n")
	open := bytes.IndexByte(trimmed, '{')
	if open < 0 || trimmed[len(trimmed)-1] != '}' {
		return body
	}
	out := make([]byte, 0, len(body)+len(`"stream_options":{"include_usage":true}`)+1)
	out = append(out, trimmed[:len(trimmed)-1]...)
	out = append(out, ',')
	out = append(out, `"stream_options":{"include_usage":true}`...)
	out = append(out, '}')
	out = append(out, body[len(trimmed):]...)
	return out
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

// streamData 流式 data 行的解析目标（正文增量由 streamAssembler 处理，此处只取 usage 与 error 帧）。
type streamData struct {
	Choices []struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// parseStreamData 解析单个 SSE data 块，返回：命中 usage、该 usage 的原始 JSON、是否命中、
// 流内 error 帧摘要（空=无 error 帧）。usage 与 error 摘要取自同一次反序列化，
// 避免同一帧被重复解析（每帧双解析是流式热路径上的额外开销）。
// 原始 JSON 供 call_log 记录上游原文（诊断渠道 usage 口径）。
func parseStreamData(data []byte) (billing.Usage, json.RawMessage, bool, string) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var obj streamData
	if err := dec.Decode(&obj); err != nil {
		return billing.Usage{}, nil, false, ""
	}
	errDetail := errorFrameDetail(obj.Error)
	for _, c := range obj.Choices {
		if len(c.Usage) > 0 {
			if u, err := parseUsageObj(c.Usage); err == nil && !usageZero(u) {
				return u, c.Usage, true, errDetail
			}
		}
	}
	if len(obj.Usage) > 0 {
		if u, err := parseUsageObj(obj.Usage); err == nil && !usageZero(u) {
			return u, obj.Usage, true, errDetail
		}
	}
	return billing.Usage{}, nil, false, errDetail
}
