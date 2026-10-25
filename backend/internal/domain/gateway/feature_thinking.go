package gateway

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// thinkingRoundtripFeature 请求侧特性 thinking-roundtrip：
// 按上游约定，把消息历史中 assistant 消息的推理内容回传到上游要求的字段
// （默认 reasoning_content）——修复 DeepSeek 等 thinking 模式多轮请求因未回传
// reasoning_content 而 400 的问题。
//
// 参数（Params）：
//   - enabled=true：启用（缺省 false，未显式开启则不做任何改写）；
//   - field：上游要求的推理字段名（缺省 reasoning_content，兼容 reasoning 别名来源）；
//   - onlyThinking=true：仅在请求处于 thinking 模式时才改写。
type thinkingRoundtripFeature struct {
	enabled      bool
	field        string
	onlyThinking bool
}

// newThinkingRoundtripFeature 依据挂载参数构造 thinking-roundtrip 特性。
func newThinkingRoundtripFeature(params map[string]any) RequestFeature {
	return &thinkingRoundtripFeature{
		enabled:      boolParam(params, "enabled", false),
		field:        normalizeReasoningField(stringParam(params, "field", reasoningResponseField)),
		onlyThinking: boolParam(params, "onlyThinking", false),
	}
}

// Key 返回特性标识。
func (f *thinkingRoundtripFeature) Key() string { return channelprotocol.FeatureThinkingRoundtrip }

// RewriteRequest 把 assistant 消息中已存在的推理内容回写到上游要求的字段。
// 未启用 / 无 messages / 无可用推理内容时原样返回（零差异）。
func (f *thinkingRoundtripFeature) RewriteRequest(req *UpstreamRequest) error {
	if !f.enabled || req == nil || len(req.Body) == 0 {
		return nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return nil // 非 JSON 对象：主链路已校验，交由既有流程兜底
	}
	if f.onlyThinking && !isThinkingRequest(body) {
		return nil
	}
	rawMsgs, ok := body["messages"]
	if !ok || len(bytes.TrimSpace(rawMsgs)) == 0 || string(bytes.TrimSpace(rawMsgs)) == "null" {
		return nil
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return nil
	}
	changed := false
	for _, m := range msgs {
		var role string
		_ = json.Unmarshal(m["role"], &role)
		if role != "assistant" {
			continue
		}
		if _, exists := m[f.field]; exists {
			// 已带上上游要求的字段：键存在即跳过（含客户端显式传空串，保留其原始意图）。
			// 注意与会话级推理缓存回填的差异：Backfill 把空串视为缺失并填充（见 reasoning.go），
			// 因为空值无法满足上游「必须回传」的要求；两处判定有意不同。
			continue
		}
		src, ok := reasoningSource(m, f.field)
		if !ok {
			continue // 无可回传的推理内容：不凭空构造
		}
		m[f.field] = src
		changed = true
	}
	if !changed {
		return nil
	}
	newMsgs, err := json.Marshal(msgs)
	if err != nil {
		return err
	}
	body["messages"] = newMsgs
	newBody, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req.Body = newBody
	return nil
}

// reasoningAliases 是推理内容字段的别名集合（顺序即优先级）：reasoning_content 是 OpenAI 兼容
// 上游的通用字段名，reasoning 是少数上游的替代名。请求侧回传（识别源字段）与响应侧捕获
// （字段名容错）共用该集合，避免各处硬编码字段名。
var reasoningAliases = []string{reasoningResponseField, "reasoning"}

// reasoningSource 从消息中取可回传的推理内容：优先 field 之外的已知别名
// （reasoning_content / reasoning），忽略空串与 null。
func reasoningSource(m map[string]json.RawMessage, field string) (json.RawMessage, bool) {
	for _, key := range reasoningAliases {
		if key == field {
			continue
		}
		if v, ok := m[key]; ok && isNonEmptyReasoning(v) {
			return v, true
		}
	}
	return nil, false
}

// isNonEmptyReasoning 判断推理字段值是否为非空内容（字符串非空，或数组/对象非 null）。
func isNonEmptyReasoning(v json.RawMessage) bool {
	t := bytes.TrimSpace(v)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return false
	}
	var s string
	if json.Unmarshal(t, &s) == nil {
		return strings.TrimSpace(s) != ""
	}
	return true
}

// isThinkingRequestBody 判断请求体是否显式处于 thinking 模式；解析失败视为否。
func isThinkingRequestBody(body []byte) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	return isThinkingRequest(m)
}

// isThinkingRequest 判断请求是否处于 thinking 模式（thinking 开启 / enable_thinking 为真 /
// reasoning_effort 存在）。
func isThinkingRequest(m map[string]json.RawMessage) bool {
	if raw, ok := m["thinking"]; ok {
		var b bool
		if json.Unmarshal(raw, &b) == nil && b {
			return true
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) == nil {
			var t string
			if json.Unmarshal(obj["type"], &t) == nil && strings.EqualFold(t, "enabled") {
				return true
			}
		}
	}
	if raw, ok := m["enable_thinking"]; ok {
		var b bool
		if json.Unmarshal(raw, &b) == nil && b {
			return true
		}
	}
	if raw, ok := m["reasoning_effort"]; ok {
		t := bytes.TrimSpace(raw)
		if len(t) > 0 && !bytes.Equal(t, []byte("null")) {
			return true
		}
	}
	return false
}
