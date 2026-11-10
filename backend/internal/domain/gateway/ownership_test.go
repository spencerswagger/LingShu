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

// 协议被软删（rt.Protocol == nil）时，运行时必须回落到渠道内联基址：
// 该状态下网关整体按内联渠道处理，内联值此刻就是生效配置，不回落的后果是渠道被打向空地址。
// 这条与迁移/预检脚本的 S1 口径一致（引用已软删协议的渠道保持内联值不动）。
func TestEffectiveBaseURL_ProtocolDeletedFallsBackToInline(t *testing.T) {
	protoID := int64(7)
	rt := &channel.RuntimeChannel{
		Channel:  channel.Channel{ID: 1, ChannelProtocolID: &protoID, BaseURL: "https://inline.example.com"},
		Protocol: nil, // 协议已软删 / 加载失败
	}
	if got := effectiveBaseURL(rt, channelprotocol.ProtocolOpenAIChat); got != "https://inline.example.com" {
		t.Fatalf("协议不可用时应回落到内联基址, got %q", got)
	}
	// 内联渠道（未引用协议）同样走内联值。
	inline := &channel.RuntimeChannel{Channel: channel.Channel{BaseURL: "https://inline2.example.com"}}
	if got := effectiveBaseURL(inline, channelprotocol.ProtocolOpenAIChat); got != "https://inline2.example.com" {
		t.Fatalf("内联渠道应使用自身 base_url, got %q", got)
	}
	// 引用协议且协议可用 → 只跟随协议，内联值不参与（即便非空）。
	live := &channel.RuntimeChannel{
		Channel:  channel.Channel{ChannelProtocolID: &protoID, BaseURL: "https://stale.example.com"},
		Protocol: &channelprotocol.ChannelProtocol{BaseURL: "https://proto.example.com"},
	}
	if got := effectiveBaseURL(live, channelprotocol.ProtocolOpenAIChat); got != "https://proto.example.com" {
		t.Fatalf("协议可用时应只跟随协议, got %q", got)
	}
}

// 转发与探测/视图必须同一实现：channel.EffectiveBaseURL 是唯一解析链，
// gateway 侧只是委托（此断言防止又出现一份平行实现）。
func TestEffectiveBaseURL_DelegatesToChannelPackage(t *testing.T) {
	protoID := int64(7)
	cases := []*channel.RuntimeChannel{
		nil,
		{Channel: channel.Channel{BaseURL: "https://inline.example.com"}},
		{Channel: channel.Channel{ChannelProtocolID: &protoID, BaseURL: "https://fallback.example.com"}},
		{
			Channel:  channel.Channel{ChannelProtocolID: &protoID, BaseURL: "https://stale.example.com"},
			Protocol: &channelprotocol.ChannelProtocol{BaseURL: "https://proto.example.com"},
		},
	}
	for i, rt := range cases {
		got := effectiveBaseURL(rt, channelprotocol.ProtocolOpenAIChat)
		want := channel.EffectiveBaseURL(rt, channelprotocol.ProtocolOpenAIChat)
		if got != want {
			t.Fatalf("case %d：gateway 与 channel 包解析结果不一致 got=%q want=%q", i, got, want)
		}
	}
}
