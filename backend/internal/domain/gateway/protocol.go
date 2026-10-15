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
		base = channelprotocol.Protocols{channelprotocol.Canonical(rt.Channel.Protocol): {}}
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

// effectiveAuth 求鉴权注入方式：渠道协议读取，内联渠道缺省 Authorization/Bearer。
func effectiveAuth(rt *channel.RuntimeChannel) channelprotocol.AuthSpec {
	if rt.Protocol != nil {
		return rt.Protocol.Auth
	}
	return channelprotocol.AuthSpec{Header: "Authorization", Scheme: "bearer"}
}

// effectiveHeaderPolicy 求请求头策略：渠道协议读取，内联渠道缺省 none。
func effectiveHeaderPolicy(rt *channel.RuntimeChannel) channelprotocol.HeaderPolicy {
	if rt.Protocol != nil {
		return rt.Protocol.RequestHeaders
	}
	return channelprotocol.HeaderPolicy{PassthroughMode: channelprotocol.PassthroughNone}
}

// applyResponseHeaderWhitelist 把上游响应头中命中白名单的项复制到下游响应头。
// 未命中白名单的上游头默认不透传（维持渠道透明，AC-10/AC-11）。
func applyResponseHeaderWhitelist(w http.ResponseWriter, upstream http.Header, whitelist []string) {
	for _, name := range whitelist {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if vals, ok := upstream[http.CanonicalHeaderKey(name)]; ok {
			for _, v := range vals {
				w.Header().Add(name, v)
			}
		}
	}
}
