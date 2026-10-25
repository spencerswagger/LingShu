package gateway

import (
	"bytes"
	"encoding/json"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// toolChoiceDowngradeFeature 请求侧特性 tool-choice-downgrade：
// 部分 thinking 模型（DeepSeek 官网 / 火山方舟 deepseek 系）拒绝「强制工具选择」，
// 上游返回 400（Thinking mode does not support this tool_choice / InvalidParameter；
// Anthropic 手动 extended thinking 下 tool_choice 仅支持 auto/none，any 与 tool 会报
// tool_choice: type "tool" and "any" are not supported for this model.）。
// 本特性把强制语义降级为 auto（模型仍可自行调用工具，但不再被强制），按协议形状分别改写：
//   - openai-chat（字符串 / 对象混用）："required" → "auto"；
//     {"type":"function"|"any"|"tool"} → "auto"（字符串）
//   - anthropic-messages（恒为对象）：{"type":"any"} / {"type":"tool"}（可带 name）→ 仅把 type
//     改为 auto 并移除仅对 type=tool 有意义的 name，保留 disable_parallel_tool_use 等其余合法键
//
// 两种形状不可混用：OpenAI 用字符串 "auto"，Anthropic 必须用对象 {"type":"auto"}，写错会
// 破坏请求体，故按 req.Protocol 分支各自识别与改写（字符串在 anthropic 下不算强制形状）。
//
// 已为 auto/none、对象 {"type":"auto"}/{"type":"none"}、null、缺省，或非上述两种协议时原样返回
// （逐字节一致）。跨协议转换后的请求体由 handler 在转换后单独应用本特性
// （见 applyConvertedRequestFeatures）。
//
// 注意：命中强制语义时会整体重编码请求体（顶层键按字典序重排），与 rewriteModel 同源行为，
// 只在「不改写必 400」的请求上发生；勿把本特性挂到不返回该错误的渠道上。
//
// 参数（Params）：
//   - enabled=true：启用（缺省 false，未显式开启则不做任何改写）；
//   - onlyThinking=true：仅在请求显式声明 thinking 时改写（缺省 false）。
//     thinking 在转换后是否保留取决于目标协议（codec 把顶层未知字段收进 canonical Extra，
//     而转换仅在目标为 anthropic 时清空 Extra）：
//   - 目标 openai-chat（如 anthropic 入站 → DeepSeek）：thinking 经 Extra 透出，onlyThinking 生效；
//   - 目标 anthropic-messages：转换阶段丢弃顶层 thinking，isThinkingRequest 恒 false，onlyThinking
//     不生效（该请求亦不处于 extended thinking 模式，本不需要降级）。
//     因此该组合（目标 anthropic + onlyThinking=true）应视为配置错误，不要开启。
type toolChoiceDowngradeFeature struct {
	enabled      bool
	onlyThinking bool
}

// newToolChoiceDowngradeFeature 依据挂载参数构造 tool-choice-downgrade 特性。
func newToolChoiceDowngradeFeature(params map[string]any) RequestFeature {
	return &toolChoiceDowngradeFeature{
		enabled:      boolParam(params, "enabled", false),
		onlyThinking: boolParam(params, "onlyThinking", false),
	}
}

// Key 返回特性标识。
func (f *toolChoiceDowngradeFeature) Key() string { return channelprotocol.FeatureToolChoiceDowngrade }

// RewriteRequest 把强制工具选择降级为 auto；
// 未启用 / 非本特性覆盖协议 / 无 tool_choice / 非强制时零差异（逐字节一致）。
func (f *toolChoiceDowngradeFeature) RewriteRequest(req *UpstreamRequest) error {
	if !f.enabled || req == nil || len(req.Body) == 0 {
		return nil
	}
	// 按协议形状选定「强制识别函数」与「降级改写函数」：两者一一对应，不可跨协议混用。
	var (
		isForced  func(json.RawMessage) bool
		downgrade func(json.RawMessage) (json.RawMessage, bool)
	)
	switch req.Protocol {
	case channelprotocol.ProtocolOpenAIChat:
		isForced, downgrade = isForcedOpenAIToolChoice, downgradeOpenAIToolChoice
	case channelprotocol.ProtocolAnthropicMessages:
		isForced, downgrade = isForcedAnthropicToolChoice, downgradeAnthropicToolChoice
	default:
		return nil // 非本特性覆盖的协议：零差异
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return nil // 非 JSON 对象：交由既有流程兜底
	}
	if f.onlyThinking && !isThinkingRequest(body) {
		return nil
	}
	raw, ok := body["tool_choice"]
	if !ok || !isForced(raw) {
		return nil
	}
	replacement, ok := downgrade(raw)
	if !ok {
		return nil
	}
	body["tool_choice"] = replacement
	newBody, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req.Body = newBody
	return nil
}

// downgradeOpenAIToolChoice 把强制语义整体替换为 OpenAI 的字符串 "auto"
// （对象形态的指定函数一并丢弃，与 OpenAI 的 "required"/指定函数语义等价降级）。
func downgradeOpenAIToolChoice(json.RawMessage) (json.RawMessage, bool) {
	return json.RawMessage(`"auto"`), true
}

// downgradeAnthropicToolChoice 仅把对象里的 type 改为 auto，并移除只对 type=tool 有意义的
// name，保留 disable_parallel_tool_use 等其余 Anthropic 合法键（该键在 auto 下同样合法，
// 整对象替换会丢客户端参数）。
func downgradeAnthropicToolChoice(raw json.RawMessage) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	obj["type"] = json.RawMessage(`"auto"`)
	delete(obj, "name")
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// isForcedOpenAIToolChoice 判断 openai-chat 形状的 tool_choice 是否表达「强制使用工具」语义：
// 字符串 "required"，或对象 {"type":"function"|"any"|"tool"}。
func isForcedOpenAIToolChoice(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return false
	}
	var s string
	if json.Unmarshal(t, &s) == nil {
		return s == "required"
	}
	return hasToolChoiceType(t, "function", "any", "tool")
}

// isForcedAnthropicToolChoice 判断 anthropic-messages 形状的 tool_choice 是否表达「强制使用工具」语义：
// 仅对象 {"type":"any"} / {"type":"tool"}（含带 name 的形态）；字符串一律不算（Anthropic 形状恒为对象）。
func isForcedAnthropicToolChoice(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return false
	}
	return hasToolChoiceType(t, "any", "tool")
}

// hasToolChoiceType 判断 raw 是否为对象且其 "type" 命中给定集合（非对象/非 JSON 一律 false）。
func hasToolChoiceType(raw json.RawMessage, want ...string) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return false
	}
	var typ string
	if json.Unmarshal(obj["type"], &typ) != nil {
		return false
	}
	for _, w := range want {
		if typ == w {
			return true
		}
	}
	return false
}
