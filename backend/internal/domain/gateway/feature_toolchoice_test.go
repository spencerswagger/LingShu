package gateway

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
	"github.com/team/llmgateway/internal/domain/protocolconv"
)

// assertToolChoice 解析 body 后断言 tool_choice 等于期望的原始 JSON（区分字符串与对象）。
func assertToolChoice(t *testing.T, body []byte, want string) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("结果不是 JSON 对象: %v, got %s", err, body)
	}
	raw, ok := m["tool_choice"]
	if !ok {
		t.Fatalf("缺少 tool_choice, got %s", body)
	}
	var got, exp any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("tool_choice 非法 JSON: %v, got %s", err, raw)
	}
	if err := json.Unmarshal([]byte(want), &exp); err != nil {
		t.Fatalf("期望值非法 JSON: %v, want %s", err, want)
	}
	if !reflect.DeepEqual(got, exp) {
		t.Fatalf("tool_choice 不符: got %s, want %s", raw, want)
	}
}

// TestToolChoiceDowngradeFeature 覆盖强制工具选择降级为 auto 的语义与开关，
// 分 openai-chat / openai-responses（字符串或扁平对象）与 anthropic-messages（对象）形状。
func TestToolChoiceDowngradeFeature(t *testing.T) {
	cases := []struct {
		name     string
		params   map[string]any
		protocol string
		body     string
		// want 为期望改写后的 tool_choice 原始 JSON；空表示期望 body 逐字节不变。
		want string
	}{
		// openai-chat 形状（字符串 / 对象混用）。
		{
			name:   "required_to_auto",
			params: map[string]any{"enabled": true},
			body:   `{"model":"m","messages":[],"tool_choice":"required"}`,
			want:   `"auto"`,
		},
		{
			name:   "function_to_auto",
			params: map[string]any{"enabled": true},
			body:   `{"model":"m","tool_choice":{"type":"function","function":{"name":"f"}}}`,
			want:   `"auto"`,
		},
		{
			name:   "any_to_auto",
			params: map[string]any{"enabled": true},
			body:   `{"model":"m","tool_choice":{"type":"any"}}`,
			want:   `"auto"`,
		},
		{
			name:   "tool_to_auto",
			params: map[string]any{"enabled": true},
			body:   `{"model":"m","tool_choice":{"type":"tool","name":"f"}}`,
			want:   `"auto"`,
		},
		{
			name:   "null_untouched",
			params: map[string]any{"enabled": true},
			body:   `{"model":"m","tool_choice":null}`,
		},
		{
			name:   "auto_untouched",
			params: map[string]any{"enabled": true},
			body:   `{"model":"m","tool_choice":"auto"}`,
		},
		{
			name:   "none_untouched",
			params: map[string]any{"enabled": true},
			body:   `{"model":"m","tool_choice":"none"}`,
		},
		{
			name:   "absent_untouched",
			params: map[string]any{"enabled": true},
			body:   `{"model":"m","messages":[]}`,
		},
		{
			name:   "disabled_by_default",
			params: map[string]any{},
			body:   `{"model":"m","tool_choice":"required"}`,
		},
		{
			name:   "only_thinking_skips_plain",
			params: map[string]any{"enabled": true, "onlyThinking": true},
			body:   `{"model":"m","tool_choice":"required"}`,
		},
		{
			name:   "only_thinking_applies_when_enabled",
			params: map[string]any{"enabled": true, "onlyThinking": true},
			body:   `{"model":"m","tool_choice":"required","thinking":{"type":"enabled"}}`,
			want:   `"auto"`,
		},
		{
			name:   "invalid_json_untouched",
			params: map[string]any{"enabled": true},
			body:   `not-json`,
		},
		{
			name:   "array_untouched",
			params: map[string]any{"enabled": true},
			body:   `{"model":"m","tool_choice":["required"]}`,
		},
		{
			name:   "number_untouched",
			params: map[string]any{"enabled": true},
			body:   `{"model":"m","tool_choice":1}`,
		},
		// openai-responses 形状（字符串 / 扁平对象）：命中则降级为字符串 "auto"。
		{
			name:     "responses_required_to_auto",
			params:   map[string]any{"enabled": true},
			protocol: channelprotocol.ProtocolOpenAIResponses,
			body:     `{"model":"m","tool_choice":"required"}`,
			want:     `"auto"`,
		},
		{
			name:     "responses_function_to_auto",
			params:   map[string]any{"enabled": true},
			protocol: channelprotocol.ProtocolOpenAIResponses,
			body:     `{"model":"m","tool_choice":{"type":"function","name":"f"}}`,
			want:     `"auto"`,
		},
		{
			name:     "responses_custom_to_auto",
			params:   map[string]any{"enabled": true},
			protocol: channelprotocol.ProtocolOpenAIResponses,
			body:     `{"model":"m","tool_choice":{"type":"custom","name":"f"}}`,
			want:     `"auto"`,
		},
		{
			// allowed_tools 且 mode=required：强制从允许清单中选择，属强制语义。
			name:     "responses_allowed_tools_required_to_auto",
			params:   map[string]any{"enabled": true},
			protocol: channelprotocol.ProtocolOpenAIResponses,
			body:     `{"model":"m","tool_choice":{"type":"allowed_tools","mode":"required","tools":[]}}`,
			want:     `"auto"`,
		},
		{
			name:     "responses_auto_untouched",
			params:   map[string]any{"enabled": true},
			protocol: channelprotocol.ProtocolOpenAIResponses,
			body:     `{"model":"m","tool_choice":"auto"}`,
		},
		{
			// allowed_tools 的 mode 非 required（auto/none）时允许模型自选，不算强制语义。
			name:     "responses_allowed_tools_auto_untouched",
			params:   map[string]any{"enabled": true},
			protocol: channelprotocol.ProtocolOpenAIResponses,
			body:     `{"model":"m","tool_choice":{"type":"allowed_tools","mode":"auto","tools":[]}}`,
		},
		{
			name:     "responses_allowed_tools_mode_absent_untouched",
			params:   map[string]any{"enabled": true},
			protocol: channelprotocol.ProtocolOpenAIResponses,
			body:     `{"model":"m","tool_choice":{"type":"allowed_tools","tools":[]}}`,
		},
		{
			// onlyThinking 的 responses 形状：reasoning.effort 存在即视为 thinking 模式。
			name:     "responses_only_thinking_applies_with_reasoning_effort",
			params:   map[string]any{"enabled": true, "onlyThinking": true},
			protocol: channelprotocol.ProtocolOpenAIResponses,
			body:     `{"model":"m","tool_choice":"required","reasoning":{"effort":"high"}}`,
			want:     `"auto"`,
		},
		{
			name:     "responses_only_thinking_skips_without_effort",
			params:   map[string]any{"enabled": true, "onlyThinking": true},
			protocol: channelprotocol.ProtocolOpenAIResponses,
			body:     `{"model":"m","tool_choice":"required","reasoning":{"summary":"auto"}}`,
		},
		{
			name:     "responses_disabled_by_default",
			params:   map[string]any{},
			protocol: channelprotocol.ProtocolOpenAIResponses,
			body:     `{"model":"m","tool_choice":"required"}`,
		},
		// anthropic-messages 形状（恒为对象）：命中则降级为对象 {"type":"auto"}。
		{
			name:     "anthropic_any_downgraded",
			params:   map[string]any{"enabled": true},
			protocol: channelprotocol.ProtocolAnthropicMessages,
			body:     `{"model":"m","tool_choice":{"type":"any"}}`,
			want:     `{"type":"auto"}`,
		},
		{
			name:     "anthropic_tool_downgraded",
			params:   map[string]any{"enabled": true},
			protocol: channelprotocol.ProtocolAnthropicMessages,
			body:     `{"model":"m","tool_choice":{"type":"tool","name":"f"}}`,
			want:     `{"type":"auto"}`,
		},
		{
			// 仅改 type，保留 Anthropic 其余合法键（disable_parallel_tool_use 在 auto 下同样合法）。
			name:     "anthropic_any_keeps_other_keys",
			params:   map[string]any{"enabled": true},
			protocol: channelprotocol.ProtocolAnthropicMessages,
			body:     `{"model":"m","tool_choice":{"type":"any","disable_parallel_tool_use":true}}`,
			want:     `{"type":"auto","disable_parallel_tool_use":true}`,
		},
		{
			// type=tool 时 name 仅对该类型合法，降级后应删除；无关键仍保留。
			name:     "anthropic_tool_drops_name_keeps_other_keys",
			params:   map[string]any{"enabled": true},
			protocol: channelprotocol.ProtocolAnthropicMessages,
			body:     `{"model":"m","tool_choice":{"type":"tool","name":"f","disable_parallel_tool_use":false}}`,
			want:     `{"type":"auto","disable_parallel_tool_use":false}`,
		},
		{
			name:     "anthropic_auto_untouched",
			params:   map[string]any{"enabled": true},
			protocol: channelprotocol.ProtocolAnthropicMessages,
			body:     `{"model":"m","tool_choice":{"type":"auto"}}`,
		},
		{
			name:     "anthropic_none_untouched",
			params:   map[string]any{"enabled": true},
			protocol: channelprotocol.ProtocolAnthropicMessages,
			body:     `{"model":"m","tool_choice":{"type":"none"}}`,
		},
		{
			// anthropic 协议下传字符串 "required"：字符串不是 anthropic 形状，不应改写。
			name:     "anthropic_string_shape_untouched",
			params:   map[string]any{"enabled": true},
			protocol: channelprotocol.ProtocolAnthropicMessages,
			body:     `{"model":"m","tool_choice":"required"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			protocol := tc.protocol
			if protocol == "" {
				protocol = channelprotocol.ProtocolOpenAIChat
			}
			req := &UpstreamRequest{Body: []byte(tc.body), Protocol: protocol}
			if err := newToolChoiceDowngradeFeature(tc.params).RewriteRequest(req); err != nil {
				t.Fatalf("RewriteRequest: %v", err)
			}
			if tc.want == "" {
				if string(req.Body) != tc.body {
					t.Fatalf("未改变场景应逐字节一致, got %s", req.Body)
				}
				return
			}
			assertToolChoice(t, req.Body, tc.want)
		})
	}
}

// TestApplyConvertedRequestFeaturesDowngradesForcedToolChoice 覆盖跨协议转换路径：
// 转换后的 openai-chat / anthropic-messages 请求体应同样应用强制工具选择降级。
func TestApplyConvertedRequestFeaturesDowngradesForcedToolChoice(t *testing.T) {
	rt := func(feats ...channelprotocol.Feature) *channel.RuntimeChannel {
		return &channel.RuntimeChannel{Protocol: &channelprotocol.ChannelProtocol{Features: feats}}
	}
	downgrade := channelprotocol.Feature{
		Key:    channelprotocol.FeatureToolChoiceDowngrade,
		Params: map[string]any{"enabled": true},
	}
	// openai-chat 字符串形状的强制选择。
	body := []byte(`{"model":"m","messages":[],"tool_choice":"required"}`)

	t.Run("openai_chat_target_downgraded", func(t *testing.T) {
		out, err := applyConvertedRequestFeatures(body, rt(downgrade), channelprotocol.ProtocolOpenAIChat)
		if err != nil {
			t.Fatalf("applyConvertedRequestFeatures: %v", err)
		}
		assertToolChoice(t, out, `"auto"`)
	})

	t.Run("anthropic_target_downgraded_to_object", func(t *testing.T) {
		anth := []byte(`{"model":"m","messages":[],"tool_choice":{"type":"any"}}`)
		out, err := applyConvertedRequestFeatures(anth, rt(downgrade), channelprotocol.ProtocolAnthropicMessages)
		if err != nil {
			t.Fatalf("applyConvertedRequestFeatures: %v", err)
		}
		assertToolChoice(t, out, `{"type":"auto"}`)
	})

	t.Run("enabled_but_not_forced_untouched", func(t *testing.T) {
		plain := []byte(`{"model":"m","messages":[],"tool_choice":"auto"}`)
		out, err := applyConvertedRequestFeatures(plain, rt(downgrade), channelprotocol.ProtocolOpenAIChat)
		if err != nil {
			t.Fatalf("applyConvertedRequestFeatures: %v", err)
		}
		if string(out) != string(plain) {
			t.Fatalf("非强制语义应逐字节一致, got %s", out)
		}
	})

	t.Run("requires_enabled_param", func(t *testing.T) {
		disabled := channelprotocol.Feature{Key: channelprotocol.FeatureToolChoiceDowngrade}
		out, err := applyConvertedRequestFeatures(body, rt(disabled), channelprotocol.ProtocolOpenAIChat)
		if err != nil {
			t.Fatalf("applyConvertedRequestFeatures: %v", err)
		}
		if string(out) != string(body) {
			t.Fatalf("特性未启用时不应改写, got %s", out)
		}
	})

	t.Run("anthropic_target_openai_string_shape_untouched", func(t *testing.T) {
		// anthropic 目标 + openai 字符串形状（"required"）：字符串不是 anthropic 形状，不应改写。
		out, err := applyConvertedRequestFeatures(body, rt(downgrade), channelprotocol.ProtocolAnthropicMessages)
		if err != nil {
			t.Fatalf("applyConvertedRequestFeatures: %v", err)
		}
		if string(out) != string(body) {
			t.Fatalf("anthropic 目标不应改写 openai 字符串形状, got %s", out)
		}
	})

	t.Run("responses_target_downgraded", func(t *testing.T) {
		out, err := applyConvertedRequestFeatures(body, rt(downgrade), channelprotocol.ProtocolOpenAIResponses)
		if err != nil {
			t.Fatalf("applyConvertedRequestFeatures: %v", err)
		}
		assertToolChoice(t, out, `"auto"`)
	})

	t.Run("protocol_not_in_whitelist_untouched", func(t *testing.T) {
		out, err := applyConvertedRequestFeatures(body, rt(downgrade), "unknown-protocol")
		if err != nil {
			t.Fatalf("applyConvertedRequestFeatures: %v", err)
		}
		if string(out) != string(body) {
			t.Fatalf("协议不在白名单集合内应逐字节一致, got %s", out)
		}
	})

	t.Run("no_features_untouched", func(t *testing.T) {
		out, err := applyConvertedRequestFeatures(body, rt(), channelprotocol.ProtocolOpenAIChat)
		if err != nil {
			t.Fatalf("applyConvertedRequestFeatures: %v", err)
		}
		if string(out) != string(body) {
			t.Fatalf("未挂载特性时不应改写, got %s", out)
		}
	})
}

// TestConvertedRequestFeatureWhitelistConsistency 校验「转换后安全特性白名单」与实际注册表一致：
// 每个特性 key 都能在 features.request 注册表中找到，且是 channelprotocol 认可的合法特性 key；
// 目标协议名必须是合法基础协议。
func TestConvertedRequestFeatureWhitelistConsistency(t *testing.T) {
	for key, protocols := range convertedRequestFeatureKeys {
		if _, ok := features.request[key]; !ok {
			t.Errorf("白名单特性 %q 未在 features.request 注册", key)
		}
		if !channelprotocol.IsValidFeatureKey(key) {
			t.Errorf("白名单特性 %q 不是 channelprotocol 认可的合法特性 key", key)
		}
		if len(protocols) == 0 {
			t.Errorf("白名单特性 %q 未声明任何目标协议", key)
		}
		for _, protocol := range protocols {
			if !channelprotocol.IsBaseProtocol(protocol) {
				t.Errorf("白名单特性 %q 的目标协议 %q 非法", key, protocol)
			}
		}
	}
}

// TestConvertedAnthropicToolChoiceEndToEnd 端到端覆盖：openai-chat 入站的强制 tool_choice
// 经真实协议转换产出 anthropic 形状后，再被转换后特性降级为对象 {"type":"auto"}。
// 先断言转换确实产出 anthropic 的强制形状（否则本特性分支在该链路上没有作用对象）。
func TestConvertedAnthropicToolChoiceEndToEnd(t *testing.T) {
	rt := &channel.RuntimeChannel{Protocol: &channelprotocol.ChannelProtocol{
		Features: []channelprotocol.Feature{{
			Key:    channelprotocol.FeatureToolChoiceDowngrade,
			Params: map[string]any{"enabled": true},
		}},
	}}

	cases := []struct {
		name       string
		toolChoice string
		converted  string // 转换后应出现的强制形状（JSON，按语义比较，不依赖键序）
	}{
		{name: "required_to_any", toolChoice: `"required"`, converted: `{"type":"any"}`},
		{name: "function_to_tool", toolChoice: `{"type":"function","function":{"name":"f"}}`, converted: `{"type":"tool","name":"f"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inbound := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
				`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],` +
				`"tool_choice":` + tc.toolChoice + `}`)
			conv, _, err := protocolconv.ConvertRequestWithOptions(
				channelprotocol.ProtocolOpenAIChat, channelprotocol.ProtocolAnthropicMessages, inbound, protocolconv.SerializeOptions{})
			if err != nil {
				t.Fatalf("ConvertRequestWithOptions: %v", err)
			}
			// 前提：转换必须产出 anthropic 的强制形状，否则本特性分支在该链路上没有作用对象。
			assertToolChoice(t, conv, tc.converted)
			out, err := applyConvertedRequestFeatures(conv, rt, channelprotocol.ProtocolAnthropicMessages)
			if err != nil {
				t.Fatalf("applyConvertedRequestFeatures: %v", err)
			}
			assertToolChoice(t, out, `{"type":"auto"}`)
		})
	}
}

// TestChannelReasoningInject 覆盖转换注入/缓存回填的生效门控：
// 需渠道声明 thinking-roundtrip(enabled=true)；若同时声明 onlyThinking=true，则仅当调用方
// 判定入站请求处于 thinking 模式（布尔入参）时才生效（与直通路径语义一致）。
// 请求体形状 → 布尔的具体判定由 TestIsThinkingRequestBody 单独覆盖，本测试只验门控逻辑。
func TestChannelReasoningInject(t *testing.T) {
	mk := func(params map[string]any) *channel.RuntimeChannel {
		return &channel.RuntimeChannel{Protocol: &channelprotocol.ChannelProtocol{
			Features: []channelprotocol.Feature{
				{Key: channelprotocol.FeatureThinkingRoundtrip, Params: params},
			},
		}}
	}

	t.Run("undeclared_or_disabled_zero", func(t *testing.T) {
		if got := channelReasoningInject(mk(map[string]any{}), true); got.ReasoningContent {
			t.Fatalf("未启用时不应生效, got %+v", got)
		}
		if got := channelReasoningInject(&channel.RuntimeChannel{}, true); got.ReasoningContent {
			t.Fatalf("未挂载渠道协议时不应生效, got %+v", got)
		}
	})

	t.Run("enabled_without_only_thinking", func(t *testing.T) {
		got := channelReasoningInject(mk(map[string]any{"enabled": true, "field": "reasoning"}), false)
		if !got.ReasoningContent || got.ReasoningField != "reasoning" {
			t.Fatalf("非 onlyThinking 时应无条件生效并沿用字段名, got %+v", got)
		}
	})

	t.Run("only_thinking_skips_plain_request", func(t *testing.T) {
		got := channelReasoningInject(mk(map[string]any{"enabled": true, "onlyThinking": true}), false)
		if got.ReasoningContent {
			t.Fatalf("onlyThinking 下非 thinking 请求不应生效, got %+v", got)
		}
	})

	t.Run("only_thinking_applies_on_thinking_request", func(t *testing.T) {
		got := channelReasoningInject(mk(map[string]any{"enabled": true, "onlyThinking": true}), true)
		if !got.ReasoningContent || got.ReasoningField != "reasoning_content" {
			t.Fatalf("onlyThinking 下 thinking 请求应生效, got %+v", got)
		}
	})
}

// TestIsThinkingRequestBody 覆盖入站请求体 thinking 模式判定（供 channelReasoningInject 的
// onlyThinking 门控使用，由调用方在候选循环外以原始入站请求体单次求值）：
// thinking 布尔/对象（type=enabled）、enable_thinking、reasoning_effort、
// openai-responses 的 reasoning.effort 均视为 thinking 模式；
// 仅模型名（无 thinking 字段）、显式关闭与非法 JSON 判定为否。
func TestIsThinkingRequestBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{name: "thinking_bool_true", body: `{"model":"m","thinking":true}`, want: true},
		{name: "thinking_object_enabled", body: `{"model":"m","thinking":{"type":"enabled"}}`, want: true},
		{name: "thinking_object_enabled_with_budget", body: `{"model":"m","thinking":{"type":"enabled","budget_tokens":1024}}`, want: true},
		{name: "enable_thinking_true", body: `{"model":"m","enable_thinking":true}`, want: true},
		{name: "reasoning_effort_present", body: `{"model":"m","reasoning_effort":"high"}`, want: true},
		{name: "responses_reasoning_effort_present", body: `{"model":"m","reasoning":{"effort":"high"}}`, want: true},
		{name: "responses_reasoning_effort_with_summary", body: `{"model":"m","reasoning":{"effort":"low","summary":"auto"}}`, want: true},
		{name: "responses_reasoning_summary_only", body: `{"model":"m","reasoning":{"summary":"auto"}}`, want: false},
		{name: "responses_reasoning_empty_object", body: `{"model":"m","reasoning":{}}`, want: false},
		{name: "responses_reasoning_effort_null", body: `{"model":"m","reasoning":{"effort":null}}`, want: false},
		{name: "responses_reasoning_effort_none", body: `{"model":"m","reasoning":{"effort":"none"}}`, want: false},
		{name: "responses_reasoning_string_shape", body: `{"model":"m","reasoning":"high"}`, want: false},
		{name: "model_name_only", body: `{"model":"deepseek-reasoner"}`, want: false},
		{name: "thinking_bool_false", body: `{"model":"m","thinking":false}`, want: false},
		{name: "thinking_object_disabled", body: `{"model":"m","thinking":{"type":"disabled"}}`, want: false},
		{name: "enable_thinking_false", body: `{"model":"m","enable_thinking":false}`, want: false},
		{name: "invalid_json", body: `not-json`, want: false},
		{name: "empty_body", body: ``, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isThinkingRequestBody([]byte(tc.body)); got != tc.want {
				t.Fatalf("isThinkingRequestBody(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}

	// 已声明的已知盲区（非缺陷、由前端提示与注释告知运维）：模型名隐含 thinking（如 reasoner 类）
	// 且请求体不带任何 thinking 字段时判定为非 thinking → onlyThinking=true 下不注入/不回填。
	t.Run("model_name_implied_thinking_is_not_thinking", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-reasoner","messages":[]}`)
		if isThinkingRequestBody(body) {
			t.Fatalf("仅模型名隐含 thinking 时不应判定为 thinking 模式（该盲区已在注释与前端提示中声明）")
		}
	})
}

// TestReasoningDeclarationOf 覆盖渠道「推理内容回传」声明的解析（字段名归一 + 开关）。
func TestReasoningDeclarationOf(t *testing.T) {
	mk := func(params map[string]any) *channel.RuntimeChannel {
		return &channel.RuntimeChannel{Protocol: &channelprotocol.ChannelProtocol{
			Features: []channelprotocol.Feature{
				{Key: channelprotocol.FeatureThinkingRoundtrip, Params: params},
			},
		}}
	}

	got := reasoningDeclarationOf(mk(map[string]any{"enabled": true, "field": "reasoning", "onlyThinking": true}))
	if !got.Supported || got.Field != "reasoning" || !got.OnlyThinking {
		t.Fatalf("应完整解析声明, got %+v", got)
	}

	got = reasoningDeclarationOf(mk(map[string]any{"enabled": true, "field": "  reasoning  "}))
	if got.Field != "reasoning" {
		t.Fatalf("字段名必须去空白, 否则会产出非法 JSON 键, got %+v", got)
	}

	got = reasoningDeclarationOf(mk(map[string]any{"enabled": true}))
	if !got.Supported || got.Field != "reasoning_content" || got.OnlyThinking {
		t.Fatalf("缺省应为 reasoning_content 且 onlyThinking 为假, got %+v", got)
	}

	if got := reasoningDeclarationOf(mk(map[string]any{})); got.Supported {
		t.Fatalf("未显式启用时不应视为支持, got %+v", got)
	}
	if got := reasoningDeclarationOf(&channel.RuntimeChannel{}); got.Supported || got.Field != "reasoning_content" {
		t.Fatalf("未挂载渠道协议时应为不支持且保留默认字段名, got %+v", got)
	}
}
