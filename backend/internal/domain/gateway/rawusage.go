package gateway

import "encoding/json"

// 上游原始 usage 的采集与落库载体（call_logs.upstream_usage，诊断用）。
//
// 为什么连同协议一起存：各基础协议的 usage 字段名与「包含关系」并不一致——
//   - OpenAI 系（chat/responses）：prompt_tokens / input_tokens 是「含」口径，
//     命中缓存的 cached_tokens 是其子集；
//   - Anthropic messages：input_tokens 是「不含」口径（不含 cache_read / cache_creation）；
//   - DeepSeek 原生：prompt_tokens 含 prompt_cache_hit_tokens（与 OpenAI 同构，仅字段名不同）。
//
// 脱离协议就无法事后判断某个数字的口径，因此落库形态固定为：
//
//	{"Protocol":"<基础协议名>","Usage":{...上游原始 usage 键值...}}
//
// 流式响应里 usage 可能分散在多个事件（Anthropic 的 message_start/message_delta、
// OpenAI 的多个累计分片），按「同名键后写覆盖」合并（累计值语义下后出现的即最终值）。
type rawUsageEnvelope struct {
	Protocol string                     `json:"Protocol"`
	Usage    map[string]json.RawMessage `json:"Usage"`
}

// rawUsageJSON 合并若干段上游原始 usage 并序列化为 call_logs.upstream_usage 的值；
// 无有效片段时返回 nil（不落库）。非对象片段（数组/标量）忽略——三协议的 usage 都是对象。
func rawUsageJSON(protocol string, parts []json.RawMessage) []byte {
	merged := make(map[string]json.RawMessage, 8)
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(p, &m); err != nil {
			continue
		}
		for k, v := range m {
			merged[k] = v
		}
	}
	if len(merged) == 0 {
		return nil
	}
	b, err := json.Marshal(rawUsageEnvelope{Protocol: protocol, Usage: merged})
	if err != nil {
		return nil
	}
	return b
}
