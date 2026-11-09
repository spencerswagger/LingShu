package channelprotocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// B3 回归：models[m].baseUrl 已废弃（无运行时读点）。
// 包格式仍容忍该键（不加 DisallowUnknownFields），但落库前由 normalizeIn 清空，
// 不再回显给管理员造成「配了但没生效」的误解。
func TestNormalizeIn_DropsDeprecatedModelBaseURL(t *testing.T) {
	raw := `{
      "name": "p",
      "baseUrl": "https://proto.example.com",
      "protocols": {"openai-chat": {}},
      "models": {
        "qwen-max": {"baseUrl": "https://model-level.example.com", "externalModelID": "2"},
        "qwen-plus": {"externalModelID": "5"}
      }
    }`
	var cp ChannelProtocol
	if err := json.Unmarshal([]byte(raw), &cp); err != nil {
		t.Fatalf("解码失败（未知字段应被容忍）: %v", err)
	}
	if cp.Models["qwen-max"].BaseURL == "" {
		t.Fatalf("前置条件：解码阶段应原样接收模型级 baseUrl（包格式兼容）")
	}

	normalizeIn(&cp)

	for name, mg := range cp.Models {
		if mg.BaseURL != "" {
			t.Fatalf("models[%s].baseUrl 应在归一化时被清空, got %q", name, mg.BaseURL)
		}
	}
	// 协议级基址不受影响（仍是唯一有效来源）。
	if cp.BaseURL != "https://proto.example.com" {
		t.Fatalf("协议级 baseUrl 不应被清空, got %q", cp.BaseURL)
	}
	// 其它模型级字段保持（外部模型绑定等仍走物化）。
	if cp.Models["qwen-max"].ExternalModelID != 2 {
		t.Fatalf("外部模型绑定应保留, got %d", cp.Models["qwen-max"].ExternalModelID)
	}
}

// PreferredProtocol 决定「展示用生效基址」用哪个协议键：default 标记优先，其次 openai-chat。
func TestPreferredProtocol(t *testing.T) {
	cases := []struct {
		name string
		cp   *ChannelProtocol
		want string
	}{
		{"nil 接收者 → openai-chat", nil, ProtocolOpenAIChat},
		{"空协议集 → openai-chat", &ChannelProtocol{}, ProtocolOpenAIChat},
		{
			"default 标记优先",
			&ChannelProtocol{Protocols: Protocols{
				ProtocolOpenAIChat:        {},
				ProtocolAnthropicMessages: {Default: true},
			}},
			ProtocolAnthropicMessages,
		},
		{
			"无 default → openai-chat",
			&ChannelProtocol{Protocols: Protocols{ProtocolAnthropicMessages: {}, ProtocolOpenAIChat: {}}},
			ProtocolOpenAIChat,
		},
		{
			"只有其它协议 → 字典序第一个",
			&ChannelProtocol{Protocols: Protocols{ProtocolOpenAIResponses: {}, ProtocolAnthropicMessages: {}}},
			ProtocolAnthropicMessages,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cp.PreferredProtocol(); got != tc.want {
				t.Fatalf("PreferredProtocol = %q, want %q", got, tc.want)
			}
		})
	}
}

// EffectiveBaseURL 的解析链：协议级 baseUrl 优先，回落协议级 base_url；nil 接收者安全。
func TestEffectiveBaseURL_Chain(t *testing.T) {
	cp := &ChannelProtocol{
		BaseURL: "https://proto.example.com",
		Protocols: Protocols{
			ProtocolAnthropicMessages: {BaseURL: "https://proto-anthropic.example.com"},
		},
	}
	if got := cp.EffectiveBaseURL(ProtocolAnthropicMessages); got != "https://proto-anthropic.example.com" {
		t.Fatalf("协议级 baseUrl 应优先, got %q", got)
	}
	if got := cp.EffectiveBaseURL(ProtocolOpenAIChat); got != "https://proto.example.com" {
		t.Fatalf("应回落协议 base_url, got %q", got)
	}
	var nilCP *ChannelProtocol
	if got := nilCP.EffectiveBaseURL(ProtocolOpenAIChat); got != "" {
		t.Fatalf("nil 接收者应返回空串, got %q", got)
	}
}

// P1-③：预设同步路径（toChannelProtocol）必须与 admin 写路径同口径——模型级 baseUrl 废弃后
// 落库前清空，否则 sync 会把该键又写回库里，绕过归属收敛。
func TestToChannelProtocol_DropsModelBaseURL(t *testing.T) {
	p := Provider{
		ID:   "prov",
		Name: "Prov",
		Models: map[string]Model{
			"m1": {
				ID: "m1",
				Gateway: ModelGateway{
					BaseURL:         "https://model-level.example.com",
					ExternalModelID: 2,
					Protocols:       Protocols{ProtocolOpenAIChat: {}},
				},
			},
		},
	}
	cp := p.toChannelProtocol()
	mg, ok := cp.Models["m1"]
	if !ok {
		t.Fatalf("模型条目应被物化: %+v", cp.Models)
	}
	if mg.BaseURL != "" {
		t.Fatalf("同步路径应清空模型级 baseUrl, got %q", mg.BaseURL)
	}
	// 落库形态（channel_protocols.models JSONB）不得再出现该键。
	raw, err := json.Marshal(cp.Models)
	if err != nil {
		t.Fatalf("marshal models: %v", err)
	}
	if strings.Contains(string(raw), "baseUrl") {
		t.Fatalf("持久化形态仍含 baseUrl: %s", raw)
	}
	// 其它模型级字段照旧（不能顺手把成本/绑定也清掉）。
	if mg.ExternalModelID != 2 || len(mg.Protocols) != 1 {
		t.Fatalf("其余模型级字段应保留: %+v", mg)
	}
}

// 同步路径同时保留 provider 级基址（模板基址是协议基址的唯一来源，不能被误清）。
func TestToChannelProtocol_KeepsProviderBaseURL(t *testing.T) {
	p := Provider{ID: "prov", Name: "Prov", Gateway: GatewayBlock{BaseURL: "https://provider.example.com"}}
	cp := p.toChannelProtocol()
	if cp.BaseURL != "https://provider.example.com" {
		t.Fatalf("provider 级 baseUrl 应保留, got %q", cp.BaseURL)
	}
	if cp.PresetSourceID != "prov" || cp.SourceStatus != SourceActive || cp.Edited {
		t.Fatalf("来源字段不应被归一化改掉: %+v", cp)
	}
}
