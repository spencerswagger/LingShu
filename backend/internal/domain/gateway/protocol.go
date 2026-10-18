package gateway

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
	"github.com/team/llmgateway/internal/domain/router"
)

// policyForProtocol 解析某协议针对给定上游错误命中的错误策略：
// 经注入的协议查询接口取该协议 error_policy，按 (内部模型, status, errType, errCode, msg) 调 MatchError。
//
// 未装配协议查询接口 / 协议不存在 / 无命中时返回 (nil, false)——上层据此保持改造前行为
// （默认安全态：不计入健康、沿用 retryableUpstreamStatus、默认 502）。
func (g *Gateway) policyForProtocol(protocolID int64, channelID int64, internalModelID int64, status int, errType, errCode, msg string) (*channelprotocol.ErrorPolicy, bool) {
	if g.protocols == nil || protocolID <= 0 {
		return nil, false
	}
	cp, err := g.protocols.GetByID(context.Background(), protocolID)
	if err != nil || cp == nil {
		g.logger.Warn("load channel protocol for error policy failed", "protocol_id", protocolID, "channel_id", channelID, "err", err)
		return nil, false
	}
	return channelprotocol.MatchError(cp.ErrorPolicy, "", internalModelID, status, errType, errCode, msg)
}

// errorPolicyOutcome 一次「上游错误 → 错误策略」决策的结果。
// 未命中策略时保持改造前行为（默认安全态）：
//   - countIntoHealth=true（照常计入渠道健康）；
//   - failover=retryableUpstreamStatus(status)（沿用既有可转移判定）；
//   - downstreamStatus=502（默认网关错误）。
type errorPolicyOutcome struct {
	countIntoHealth  bool // 是否计入渠道健康错误率/429/鉴权计数
	failover         bool // 是否转移到下一候选
	downstreamStatus int  // 未转移时写出的下游状态码（命中 Disposition 时替代默认 502）
}

// applyErrorPolicy 对一次上游错误（status + errType/errCode/msg）做统一策略决策并执行副作用：
//   - 未命中策略 → 默认安全态（见 errorPolicyOutcome 注释）；
//   - CountIntoHealth=false → 调用方以 FeedResultGated(..., false) 回喂（完全不影响熔断）；
//   - Failover 显式设置时按其值决定是否转移（未设置沿用 retryableUpstreamStatus）；
//   - Action=drain|disable → 手动置密钥/内部模型状态（ManualSetKeyState/ManualSetModelState）；
//   - DownstreamStatus≠0 → 替代默认 502；
//   - 命中策略时按错误类别累加协议级错误统计（protocol_error_stats）。
//
// 非流式（serveNonStream）与流式（serveStream）内部错误分支共用本函数，保证两处语义完全一致。
func (g *Gateway) applyErrorPolicy(rt *channel.RuntimeChannel, routeRes *router.RouteResult,
	status int, errType, errCode, msg string) errorPolicyOutcome {

	out := errorPolicyOutcome{
		countIntoHealth:  true,
		failover:         retryableUpstreamStatus(status),
		downstreamStatus: http.StatusBadGateway,
	}
	protocolID := protocolIDOf(rt)
	pol, ok := g.policyForProtocol(protocolID, routeRes.ChannelID, routeRes.ModelRowID, status, errType, errCode, msg)
	if !ok {
		return out
	}
	out.countIntoHealth = pol.Disposition.CountIntoHealth
	if pol.Disposition.Failover != nil {
		out.failover = *pol.Disposition.Failover
	}
	if pol.Disposition.DownstreamStatus != 0 {
		out.downstreamStatus = pol.Disposition.DownstreamStatus
	}
	g.applyPolicyAction(routeRes, pol)
	g.recordProtocolErrorStat(protocolID, pol, status, errCode)
	return out
}

// applyPolicyAction 按 Disposition.Action 手动置密钥/内部模型状态（none 不动作）。
// drain/disable 同时作用于命中候选的密钥与内部模型行（双回喂语义一致）。
func (g *Gateway) applyPolicyAction(routeRes *router.RouteResult, pol *channelprotocol.ErrorPolicy) {
	switch pol.Disposition.Action {
	case channelprotocol.ErrorActionDrain:
		g.manualSetStates(routeRes, channel.ActionDrain, channel.StateDrain)
	case channelprotocol.ErrorActionDisable:
		g.manualSetStates(routeRes, channel.ActionDisable, channel.StateDisabled)
	}
}

// manualSetStates 手动设置密钥与内部模型状态（失败仅记日志，不阻断错误响应）。
func (g *Gateway) manualSetStates(routeRes *router.RouteResult, action string, st channel.State) {
	if g.channels == nil {
		return
	}
	if err := g.channels.ManualSetKeyState(routeRes.ChannelKeyID, action); err != nil {
		g.logger.Warn("apply error policy key action failed",
			"channel_key_id", routeRes.ChannelKeyID, "action", action, "err", err)
	}
	if err := g.channels.ManualSetModelState(routeRes.ChannelID, routeRes.ModelRowID, st); err != nil {
		g.logger.Warn("apply error policy model action failed",
			"channel_id", routeRes.ChannelID, "model_row_id", routeRes.ModelRowID, "state", st, "err", err)
	}
}

// recordProtocolErrorStat 命中策略时按错误类别累加协议级统计（量小，同步 upsert；失败仅记日志）。
func (g *Gateway) recordProtocolErrorStat(protocolID int64, pol *channelprotocol.ErrorPolicy, status int, errCode string) {
	if g.protocolStats == nil || protocolID <= 0 {
		return
	}
	if err := g.protocolStats.IncrProtocolErrorStat(context.Background(), protocolID, errorCategory(pol, status, errCode), g.now()); err != nil {
		g.logger.Warn("record protocol error stat failed", "protocol_id", protocolID, "err", err)
	}
}

// errorCategory 归出命中错误的类别标识：优先 code（code:<errCode>），其次 type，最后 status（status:<n>）。
func errorCategory(pol *channelprotocol.ErrorPolicy, status int, errCode string) string {
	switch {
	case pol.Match.ErrorCode != "":
		return "code:" + pol.Match.ErrorCode
	case pol.Match.ErrorType != "":
		return "type:" + pol.Match.ErrorType
	case pol.Match.Status != 0:
		return "status:" + strconv.Itoa(pol.Match.Status)
	default:
		return "status:" + strconv.Itoa(status)
	}
}

// protocolIDOf 返回渠道运行时引用的渠道协议 ID（未引用/内联配置返回 0）。
func protocolIDOf(rt *channel.RuntimeChannel) int64 {
	if rt == nil || rt.Protocol == nil {
		return 0
	}
	return rt.Protocol.ID
}

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
