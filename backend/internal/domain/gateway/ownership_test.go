package gateway

import (
	"testing"

	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// B2 回归（网关热路径）：引用协议时有效基址只跟随协议，channels.base_url 不参与。
func TestEffectiveBaseURL_ProtocolReferenceIgnoresInlineBaseURL(t *testing.T) {
	pid := int64(7)
	proto := &channelprotocol.ChannelProtocol{
		ID:      pid,
		BaseURL: "https://proto.example.com",
		Protocols: channelprotocol.Protocols{
			channelprotocol.ProtocolAnthropicMessages: {BaseURL: "https://proto-anthropic.example.com"},
		},
	}
	// 引用协议 + 遗留内联 base_url：改协议 baseURL 必须生效（旧缺陷是内联值优先）。
	rt := &channel.RuntimeChannel{
		Channel:  channel.Channel{BaseURL: "https://legacy.example.com", ChannelProtocolID: &pid},
		Protocol: proto,
	}
	if got := effectiveBaseURL(rt, channelprotocol.ProtocolOpenAIChat); got != "https://proto.example.com" {
		t.Fatalf("引用协议时有效基址应跟随协议, got %q", got)
	}
	if got := effectiveBaseURL(rt, channelprotocol.ProtocolAnthropicMessages); got != "https://proto-anthropic.example.com" {
		t.Fatalf("协议级 baseUrl 应优先, got %q", got)
	}
	// 内联渠道仍用自己的 base_url。
	inline := &channel.RuntimeChannel{Channel: channel.Channel{BaseURL: "https://inline.example.com"}}
	if got := effectiveBaseURL(inline, channelprotocol.ProtocolOpenAIChat); got != "https://inline.example.com" {
		t.Fatalf("内联渠道应使用自身 base_url, got %q", got)
	}
	// nil 安全。
	if got := effectiveBaseURL(nil, channelprotocol.ProtocolOpenAIChat); got != "" {
		t.Fatalf("nil 运行时应返回空串, got %q", got)
	}
}

// 0019 契约 §5.2：channel_protocols.models[m].protocols 降为物化种子，不再在运行时链上。
// 三级回落收敛为两级：实例 model 行 > 协议 protocol 集；内联渠道回落 protocol 归一值。
func TestEffectiveProtocols_NoSeedModelFallback(t *testing.T) {
	pid := int64(7)
	proto := &channelprotocol.ChannelProtocol{
		ID: pid,
		Protocols: channelprotocol.Protocols{
			channelprotocol.ProtocolOpenAIChat: {},
		},
		// 种子里给该模型配了 anthropic —— 降级为种子后运行时必须**看不到**它。
		Models: map[string]channelprotocol.ModelGateway{
			"m1": {ExternalModelID: 2, Protocols: channelprotocol.Protocols{channelprotocol.ProtocolAnthropicMessages: {}}},
		},
	}
	rt := &channel.RuntimeChannel{
		Channel:  channel.Channel{ChannelProtocolID: &pid},
		Protocol: proto,
	}

	// 1) 实例行无覆盖 → 用协议的 protocol 集（不含种子里的 anthropic）。
	mr := &channel.ModelRuntime{Model: channel.ChannelModel{InternalModelID: "m1"}}
	got := effectiveProtocols(rt, mr)
	if _, ok := got[channelprotocol.ProtocolAnthropicMessages]; ok {
		t.Fatalf("种子 models[m].protocols 不得再参与运行时: %v", got)
	}
	if _, ok := got[channelprotocol.ProtocolOpenAIChat]; !ok {
		t.Fatalf("应回落到协议 protocol 集: %v", got)
	}

	// 2) 实例行有覆盖 → 实例优先（迁移已把种子值搬进实例列，行为不变）。
	mr2 := &channel.ModelRuntime{Model: channel.ChannelModel{
		InternalModelID: "m1",
		Protocols:       channelprotocol.Protocols{channelprotocol.ProtocolAnthropicMessages: {}},
	}}
	got2 := effectiveProtocols(rt, mr2)
	if len(got2) != 1 {
		t.Fatalf("实例覆盖应完全接管: %v", got2)
	}
	if _, ok := got2[channelprotocol.ProtocolAnthropicMessages]; !ok {
		t.Fatalf("实例覆盖应含 anthropic: %v", got2)
	}

	// 3) 内联渠道：channels.protocol 归一（openai-compat → openai-chat）；非法值回落 openai-chat。
	inline := &channel.RuntimeChannel{Channel: channel.Channel{Protocol: "openai-compat"}}
	got3 := effectiveProtocols(inline, &channel.ModelRuntime{Model: channel.ChannelModel{InternalModelID: "m1"}})
	if _, ok := got3[channelprotocol.ProtocolOpenAIChat]; !ok {
		t.Fatalf("openai-compat 应归一为 openai-chat: %v", got3)
	}
	bad := &channel.RuntimeChannel{Channel: channel.Channel{Protocol: "not-a-protocol"}}
	got4 := effectiveProtocols(bad, &channel.ModelRuntime{Model: channel.ChannelModel{InternalModelID: "m1"}})
	if _, ok := got4[channelprotocol.ProtocolOpenAIChat]; !ok {
		t.Fatalf("非法协议名应回落 openai-chat: %v", got4)
	}
}
