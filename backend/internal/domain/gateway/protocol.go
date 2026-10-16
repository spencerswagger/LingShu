package gateway

import (
	"net/http"
	"strings"

	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// UpstreamResponseHeaderWhitelistKey 是全局上游响应头白名单在 sys_configs 中的键（默认空，零配置）。
const UpstreamResponseHeaderWhitelistKey = "gateway.upstream_response_header_whitelist"

// protocolForEndpoint 把入站端点映射到基础协议（路径 → 基础协议）。
func protocolForEndpoint(endpoint string) string {
	switch endpoint {
	case EndpointResponses:
		return channelprotocol.ProtocolOpenAIResponses
	case EndpointMessages:
		return channelprotocol.ProtocolAnthropicMessages
	default: // chat/completions、completions、embeddings 归 openai-chat
		return channelprotocol.ProtocolOpenAIChat
	}
}

// effectiveProtocols 求某模型可达的上游基础协议集合：
// 渠道协议（含模型级覆盖）优先；无渠道协议时用渠道内联 protocol；模型级 protocols 覆盖一切。
func effectiveProtocols(rt *channel.RuntimeChannel, mr *channel.ModelRuntime) channelprotocol.Protocols {
	var base channelprotocol.Protocols
	if rt.Protocol != nil {
		base = rt.Protocol.EffectiveModelProtocols(mr.Model.InternalModelID, mr.Model.Protocols)
	} else if len(mr.Model.Protocols) > 0 {
		base = mr.Model.Protocols
	} else {
		// 内联渠道：渠道 protocol 归一化；空/非法值回落 openai-chat（否则会以 "" 为键，
		// 导致全部入站协议被判为不支持，AC-4 错误外溢到所有请求）。
		name := channelprotocol.Canonical(strings.TrimSpace(rt.Channel.Protocol))
		if !channelprotocol.IsBaseProtocol(name) {
			name = channelprotocol.ProtocolOpenAIChat
		}
		base = channelprotocol.Protocols{name: {}}
	}
	if len(base) == 0 {
		return nil
	}
	out := make(channelprotocol.Protocols, len(base))
	for name, spec := range base {
		out[channelprotocol.Canonical(name)] = spec
	}
	return out
}

// effectiveBaseURL 求某基础协议的有效上游基址：渠道级 base_url 覆盖优先，回落渠道协议（协议级优先）。
func effectiveBaseURL(rt *channel.RuntimeChannel, protocol string) string {
	if s := strings.TrimSpace(rt.Channel.BaseURL); s != "" {
		return s
	}
	if rt.Protocol != nil {
		return rt.Protocol.EffectiveBaseURL(protocol)
	}
	return ""
}

// effectiveHeaderPolicy 求请求头策略：渠道协议读取，内联渠道缺省空策略（等效 none）。
func effectiveHeaderPolicy(rt *channel.RuntimeChannel) channelprotocol.HeaderPolicy {
	return rt.Protocol.EffectiveHeaderPolicy()
}

// responseHopByHop 响应侧禁止透传的头：hop-by-hop（RFC 7230 §6.1）与由响应体长度决定的 Content-Length。
// 即使被误列入白名单也不透传，避免破坏下游读体/连接复用。
var responseHopByHop = map[string]bool{
	"connection":         true,
	"keep-alive":         true,
	"te":                 true,
	"trailer":            true,
	"transfer-encoding":  true,
	"upgrade":            true,
	"proxy-connection":   true,
	"proxy-authenticate": true,
	"content-length":     true,
}

// applyResponseHeaderWhitelist 把上游响应头中命中白名单的项复制到下游响应头。
// 未命中白名单的上游头默认不透传（维持渠道透明，AC-10/AC-11）；hop-by-hop/Content-Length 恒不透传。
func applyResponseHeaderWhitelist(w http.ResponseWriter, upstream http.Header, whitelist []string) {
	for _, name := range whitelist {
		name = strings.TrimSpace(name)
		if name == "" || responseHopByHop[strings.ToLower(name)] {
			continue
		}
		if vals, ok := upstream[http.CanonicalHeaderKey(name)]; ok {
			for _, v := range vals {
				w.Header().Add(name, v)
			}
		}
	}
}
