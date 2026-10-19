package protocolconv

import (
	"encoding/json"

	"github.com/team/llmgateway/internal/domain/billing"
)

// 规范停止原因取值，作为各协议间归一化的中间词表。
const (
	stopReasonStop          = "stop"
	stopReasonLength        = "length"
	stopReasonToolCalls     = "tool_calls"
	stopReasonContentFilter = "content_filter"
)

// normalizeStopReason 将任意协议原生 stop reason 归一化为规范取值。
func normalizeStopReason(native string) string {
	switch native {
	case "", stopReasonStop, stopReasonLength, stopReasonToolCalls, stopReasonContentFilter:
		return native
	case "end_turn", "stop_sequence":
		return stopReasonStop
	case "max_tokens":
		return stopReasonLength
	case "tool_use", "function_call":
		return stopReasonToolCalls
	default:
		return native
	}
}

// anthropicStopReason 将规范 stop reason 转换为 Anthropic 原生取值。
func anthropicStopReason(canonical string) string {
	switch canonical {
	case "":
		return ""
	case stopReasonStop:
		return "end_turn"
	case stopReasonLength:
		return "max_tokens"
	case stopReasonToolCalls:
		return "tool_use"
	case stopReasonContentFilter:
		return "end_turn"
	default:
		return canonical
	}
}

// openAIStopReason 将规范 stop reason 转换为 OpenAI 原生取值。
func openAIStopReason(canonical string) string {
	switch canonical {
	case "", stopReasonStop, stopReasonLength, stopReasonToolCalls, stopReasonContentFilter:
		return canonical
	case "end_turn", "stop_sequence":
		return stopReasonStop
	case "max_tokens":
		return stopReasonLength
	case "tool_use", "function_call":
		return stopReasonToolCalls
	default:
		return canonical
	}
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

// responsesStopReason 将 OpenAI Responses 的 status 归一化为规范 stop reason。
func responsesStopReason(status string) string {
	switch status {
	case "":
		return ""
	case "completed":
		return stopReasonStop
	case "incomplete":
		return stopReasonLength
	default:
		return status
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
