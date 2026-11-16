// usage.go 是**旁路**用量读取：解析上游响应里的 usage，用于第③件的计费与对账。
//
// 关键约束：只读不改。解析出来的用量只进结果结构（Result），绝不改写任何下游字节
// ——响应体、SSE 分包、错误体都原样透传（契约：出站只换凭证头 + model）。
//
// 字段路径按契约 §3.2/§3.3 写死（四协议各不相同）：
//
//	OpenAI Chat        非流式 usage.{prompt_tokens,completion_tokens,prompt_tokens_details.cached_tokens}
//	                   （兜底顶层 prompt_cache_hit_tokens）；流式取最后一个非空 usage 帧
//	OpenAI Responses   usage.{input_tokens,output_tokens,input_tokens_details.cached_tokens}；
//	                   流式取 response.completed / response.incomplete 事件的 response.usage
//	Anthropic          usage.{input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens}；
//	                   流式 message_start.message.usage（输入/缓存）+ message_delta.usage.output_tokens（累计）
//	Gemini             usageMetadata.{promptTokenCount,candidatesTokenCount,cachedContentTokenCount,thoughtsTokenCount}
//
// 归一化（三段）：
//
//	OpenAI 族  Input = prompt/input − cached，CacheRead = cached，Output = completion/output
//	Anthropic Input = input + cache_creation，CacheRead = cache_read，Output = output
//	           （缓存写不单独定价，并入 Input）
//	Gemini    Input = prompt − cached，CacheRead = cached，Output = candidates + thoughts
package magpie

import (
	"encoding/json"
)

// Usage 是归一化后的三段用量（与 billing 的三段计费口径一致）。
type Usage struct {
	Input     int64
	CacheRead int64
	Output    int64
}

// Zero 报告三段是否全零（全零视为「没取到 usage」，与既有 usageZero 守卫同口径）。
func (u Usage) Zero() bool { return u.Input == 0 && u.CacheRead == 0 && u.Output == 0 }

// 采集上限：非流式整段缓存（超过即放弃解析，不影响透传），流式单行上限（防超长坏帧吃内存）。
const (
	maxNonStreamCapture = 4 << 20
	maxStreamLine       = 1 << 20
)

// usageCollector 是协议感知的用量采集器：Feed 喂入响应字节，Result 取归一化用量。
// 非并发安全（单请求单 goroutine 使用）。
type usageCollector struct {
	protocol  Protocol
	streaming bool

	body []byte // 非流式：整段响应体（有上限）
	line []byte // 流式：未完结的行

	usage Usage
	seen  bool
	// 放弃标记：非流式超限 / 无效帧过多时置位，Result 恒返回未取到。
	dropped bool
}

func newUsageCollector(p Protocol, streaming bool) *usageCollector {
	return &usageCollector{protocol: p, streaming: streaming}
}

// Feed 喂入一段响应字节（流式为原始 SSE 字节流，非流式为整段 body）。
func (c *usageCollector) Feed(p []byte) {
	if c.dropped || len(p) == 0 {
		return
	}
	if !c.streaming {
		if len(c.body)+len(p) > maxNonStreamCapture {
			c.dropped = true
			c.body = nil
			return
		}
		c.body = append(c.body, p...)
		return
	}
	c.line = append(c.line, p...)
	for {
		i := indexByte(c.line, '\n')
		if i < 0 {
			break
		}
		line := c.line[:i]
		c.line = append([]byte(nil), c.line[i+1:]...)
		c.consumeSSELine(line)
	}
	if len(c.line) > maxStreamLine {
		c.dropped = true
		c.line = nil
	}
}

// finish 在响应结束时收尾：流式把残行也喂一遍，非流式解析整段 body。
func (c *usageCollector) finish() {
	if c.dropped {
		return
	}
	if c.streaming {
		if len(c.line) > 0 {
			c.consumeSSELine(c.line)
			c.line = nil
		}
		return
	}
	obj, ok := decodeObject(c.body)
	if !ok {
		return
	}
	if u, found := extractUsage(c.protocol, obj); found {
		c.merge(u)
	}
}

// Result 返回归一化用量与是否取到（全零一律视为未取到）。
func (c *usageCollector) Result() (Usage, bool) {
	if !c.seen || c.usage.Zero() {
		return Usage{}, false
	}
	return c.usage, true
}

// consumeSSELine 处理一行 SSE：只认 `data:` 行，忽略事件名/注释/空行。
func (c *usageCollector) consumeSSELine(raw []byte) {
	line := trimSpace(raw)
	const prefix = "data:"
	if len(line) < len(prefix) || string(line[:len(prefix)]) != prefix {
		return
	}
	payload := trimSpace(line[len(prefix):])
	if len(payload) == 0 || string(payload) == "[DONE]" {
		return
	}
	obj, ok := decodeObject(payload)
	if !ok {
		return
	}
	// usage 可能直接在事件里（OpenAI Chat / Gemini），也可能裹在 message（Anthropic）
	// 或 response（OpenAI Responses）里，逐层下探。
	if u, found := extractUsage(c.protocol, obj); found {
		c.merge(u)
		return
	}
	for _, wrapper := range []string{"message", "response"} {
		if inner, ok := obj[wrapper]; ok {
			if innerObj, ok := decodeObject(inner); ok {
				if u, found := extractUsage(c.protocol, innerObj); found {
					c.merge(u)
					return
				}
			}
		}
	}
}

// merge 合并一次解析结果：非零字段覆盖（流式为累计口径，最后一次最大最全）。
func (c *usageCollector) merge(u Usage) {
	c.seen = true
	if u.Input != 0 {
		c.usage.Input = u.Input
	}
	if u.CacheRead != 0 {
		c.usage.CacheRead = u.CacheRead
	}
	if u.Output != 0 {
		c.usage.Output = u.Output
	}
}

// extractUsage 从「已定位到 usage 所在层」的对象里取归一化用量。
func extractUsage(p Protocol, obj map[string]json.RawMessage) (Usage, bool) {
	switch p {
	case ProtocolOpenAIChat:
		u, ok := objectField(obj, "usage")
		if !ok {
			return Usage{}, false
		}
		prompt := numField(u, "prompt_tokens")
		completion := numField(u, "completion_tokens")
		cached := numPath(u, "prompt_tokens_details", "cached_tokens")
		if cached == 0 {
			cached = numField(u, "prompt_cache_hit_tokens")
		}
		if prompt == 0 && completion == 0 && cached == 0 {
			return Usage{}, false
		}
		return Usage{Input: nonNegative(prompt - cached), CacheRead: cached, Output: completion}, true
	case ProtocolOpenAIResponses:
		u, ok := objectField(obj, "usage")
		if !ok {
			return Usage{}, false
		}
		input := numField(u, "input_tokens")
		output := numField(u, "output_tokens")
		cached := numPath(u, "input_tokens_details", "cached_tokens")
		if input == 0 && output == 0 && cached == 0 {
			return Usage{}, false
		}
		return Usage{Input: nonNegative(input - cached), CacheRead: cached, Output: output}, true
	case ProtocolAnthropic:
		u, ok := objectField(obj, "usage")
		if !ok {
			return Usage{}, false
		}
		input := numField(u, "input_tokens")
		output := numField(u, "output_tokens")
		cacheRead := numField(u, "cache_read_input_tokens")
		cacheWrite := numField(u, "cache_creation_input_tokens")
		if input == 0 && output == 0 && cacheRead == 0 && cacheWrite == 0 {
			return Usage{}, false
		}
		return Usage{Input: input + cacheWrite, CacheRead: cacheRead, Output: output}, true
	case ProtocolGemini:
		u, ok := objectField(obj, "usageMetadata")
		if !ok {
			return Usage{}, false
		}
		prompt := numField(u, "promptTokenCount")
		candidates := numField(u, "candidatesTokenCount")
		cached := numField(u, "cachedContentTokenCount")
		thoughts := numField(u, "thoughtsTokenCount")
		if prompt == 0 && candidates == 0 && cached == 0 && thoughts == 0 {
			return Usage{}, false
		}
		return Usage{Input: nonNegative(prompt - cached), CacheRead: cached, Output: candidates + thoughts}, true
	default:
		return Usage{}, false
	}
}

func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// decodeObject 解析 JSON 对象（非对象返回 false，不报错：坏帧只影响 usage，不影响透传）。
func decodeObject(b []byte) (map[string]json.RawMessage, bool) {
	if len(b) == 0 {
		return nil, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// objectField 取对象字段并解析为子对象。
func objectField(obj map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	raw, ok := obj[key]
	if !ok {
		return nil, false
	}
	return decodeObject(raw)
}

// numField 取整数字段（JSON 数字/字符串数字都接受；缺失或非法返回 0）。
func numField(obj map[string]json.RawMessage, key string) int64 {
	raw, ok := obj[key]
	if !ok {
		return 0
	}
	return toInt64(raw)
}

// numPath 取两级嵌套整数字段（例如 prompt_tokens_details.cached_tokens）。
func numPath(obj map[string]json.RawMessage, key, sub string) int64 {
	inner, ok := objectField(obj, key)
	if !ok {
		return 0
	}
	return numField(inner, sub)
}

// toInt64 把 JSON 数字（或数字字符串）转为 int64；小数按截断处理（usage 恒为整数）。
func toInt64(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int64(f)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		var parsed int64
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			return parsed
		}
	}
	return 0
}

// indexByte / trimSpace 是 bytes 包的最小封装（避免为两处调用引入额外 import 别名）。
func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && isSpaceByte(b[i]) {
		i++
	}
	for j > i && isSpaceByte(b[j-1]) {
		j--
	}
	return b[i:j]
}

func isSpaceByte(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
