package protocolconv

import (
	"encoding/json"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// 规范停止原因取值，作为各协议间归一化的中间词表。
const (
	stopReasonStop          = "stop"
	stopReasonLength        = "length"
	stopReasonToolCalls     = "tool_calls"
	stopReasonContentFilter = "content_filter"
)

// stopReasonToCanonical 把指定协议的原生停止原因归一化为规范词表取值
// （stop|length|tool_calls|content_filter|""），为流式解码与 ConvertResponse 共用的唯一解码映射。
func stopReasonToCanonical(protocol, native string) string {
	if native == "" {
		return ""
	}
	switch channelprotocol.Canonical(protocol) {
	case channelprotocol.ProtocolOpenAIChat:
		switch native {
		case "function_call":
			return stopReasonToolCalls
		}
	case channelprotocol.ProtocolOpenAIResponses:
		switch native {
		case "completed":
			return stopReasonStop
		case "incomplete", "max_output_tokens":
			return stopReasonLength
		}
	case channelprotocol.ProtocolAnthropicMessages:
		switch native {
		case "end_turn", "stop_sequence":
			return stopReasonStop
		case "max_tokens":
			return stopReasonLength
		case "tool_use":
			return stopReasonToolCalls
		}
	}
	return native
}

// stopReasonFromCanonical 把规范停止原因落回指定协议的原生取值，
// 为流式编码与 ConvertResponse 共用的唯一编码映射。
func stopReasonFromCanonical(protocol, canonical string, rep *LossyReport) string {
	if canonical == "" {
		return ""
	}
	switch channelprotocol.Canonical(protocol) {
	case channelprotocol.ProtocolOpenAIChat:
		return canonical
	case channelprotocol.ProtocolOpenAIResponses:
		switch canonical {
		case stopReasonLength:
			return "max_output_tokens"
		case stopReasonContentFilter:
			return "content_filter"
		case stopReasonStop:
			return "stop"
		case stopReasonToolCalls:
			return "tool_calls"
		}
		return canonical
	case channelprotocol.ProtocolAnthropicMessages:
		switch canonical {
		case stopReasonStop:
			return "end_turn"
		case stopReasonLength:
			return "max_tokens"
		case stopReasonToolCalls:
			return "tool_use"
		case stopReasonContentFilter:
			rep.Note("stop reason content_filter remapped to end_turn for anthropic target")
			return "end_turn"
		}
		return canonical
	}
	return canonical
}

// responsesStatus 依据规范 stop reason 生成 OpenAI Responses 的 status。
func responsesStatus(canonical string) string {
	switch canonical {
	case "":
		return ""
	case stopReasonLength:
		return "incomplete"
	default:
		return "completed"
	}
}

// usageToOpenAI 将规范用量转换为 OpenAI Chat 用量对象。
func usageToOpenAI(u billing.Usage) map[string]interface{} {
	m := map[string]interface{}{
		"prompt_tokens":     u.Input + u.CacheRead,
		"completion_tokens": u.Output,
		"total_tokens":      u.Input + u.CacheRead + u.Output,
	}
	if u.CacheRead > 0 {
		m["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": u.CacheRead}
	}
	return m
}

// usageToResponses 将规范用量转换为 OpenAI Responses 用量对象。
func usageToResponses(u billing.Usage) map[string]interface{} {
	m := map[string]interface{}{
		"input_tokens":  u.Input + u.CacheRead,
		"output_tokens": u.Output,
		"total_tokens":  u.Input + u.CacheRead + u.Output,
	}
	if u.CacheRead > 0 {
		m["input_tokens_details"] = map[string]interface{}{"cached_tokens": u.CacheRead}
	}
	return m
}

// usageToAnthropic 将规范用量转换为 Anthropic 用量对象。
func usageToAnthropic(u billing.Usage) map[string]interface{} {
	return map[string]interface{}{
		"input_tokens":            u.Input,
		"output_tokens":           u.Output,
		"cache_read_input_tokens": u.CacheRead,
	}
}

// anthropicFrame 构造带 event 名的 Anthropic SSE 帧。
func anthropicFrame(event string, payload map[string]interface{}) []byte {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte("{}")
	}
	out := make([]byte, 0, len(event)+len(body)+16)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, "\ndata: "...)
	out = append(out, body...)
	out = append(out, "\n\n"...)
	return out
}

// dataFrame 构造仅含 data 行的 OpenAI SSE 帧。
func dataFrame(payload map[string]interface{}) []byte {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte("{}")
	}
	out := make([]byte, 0, len(body)+10)
	out = append(out, "data: "...)
	out = append(out, body...)
	out = append(out, "\n\n"...)
	return out
}

// rawIntValue 读取 JSON 数值字段为 int，缺省或非数值返回 0。
func rawIntValue(raw json.RawMessage) int {
	if len(raw) == 0 || isJSONNull(raw) {
		return 0
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0
	}
	return int(jsonNumberToInt64(n))
}
