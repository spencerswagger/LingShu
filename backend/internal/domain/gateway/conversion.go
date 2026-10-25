package gateway

import (
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
	"github.com/team/llmgateway/internal/domain/protocolconv"
	"github.com/team/llmgateway/internal/domain/router"
)

// canonicalProviderEndpoint 返回某基础协议对应的「规范 Provider 端点」。
// 跨协议转换时上游 Provider 必须用目标协议的规范端点，而非入站端点——
// 否则 openai-chat 目标的 OpenAIProvider 会用入站路径（如 /messages）拼上游 URL，
// 在渠道协议未显式配置该协议 Path 时打错上游端点。
func canonicalProviderEndpoint(protocol string) string {
	switch channelprotocol.Canonical(protocol) {
	case channelprotocol.ProtocolOpenAIResponses:
		return EndpointResponses
	case channelprotocol.ProtocolAnthropicMessages:
		return EndpointMessages
	default:
		return EndpointChatCompletions
	}
}

// conversionEligibleEndpoint 判断入站端点是否为「chat 形状」——只有它们可参与跨协议转换。
// /v1/completions 与 /v1/embeddings 虽映射 openai-chat，但请求体无 messages，转换会产出非法上游体，
// 故仅直通（入站协议 ∈ 可达协议）可用，否则按不支持（AC-4）处理。
func conversionEligibleEndpoint(endpoint string) bool {
	switch endpoint {
	case EndpointChatCompletions, EndpointResponses, EndpointMessages:
		return true
	}
	return false
}

// resolveUpstreamProtocol 决定某内部模型本次上游调用的有效协议：
// 直通优先（入站协议在模型可达协议集内），否则在全局/模型级开关允许且端点可转换时按转换器挑选目标协议。
// 返回 (上游协议, 是否转换为其他协议, 是否有可用上游)；ok=false 表示无可达协议或转换被禁用。
func (g *Gateway) resolveUpstreamProtocol(rt *channel.RuntimeChannel, mr *channel.ModelRuntime, baseProtocol, endpoint string) (upstream string, converted bool, ok bool) {
	eff := effectiveProtocols(rt, mr)
	if len(eff) == 0 {
		return "", false, false
	}
	if eff.Has(baseProtocol) {
		return baseProtocol, false, true
	}
	if !conversionEligibleEndpoint(endpoint) {
		return "", false, false
	}
	if !g.conversionEnabled() {
		return "", false, false
	}
	var targets []string
	if rt.Protocol != nil {
		enabled, t := rt.Protocol.EffectiveModelConvert(mr.Model.InternalModelID)
		if enabled != nil && !*enabled {
			return "", false, false
		}
		targets = t
	}
	supported := make([]string, 0, len(eff))
	for name := range eff {
		supported = append(supported, name)
	}
	target, found := protocolconv.SelectUpstreamProtocol(baseProtocol, supported, targets)
	if !found {
		return "", false, false
	}
	return target, true, true
}

// candidatePassesThrough 判断候选渠道能否以入站协议直通（可达协议集包含入站协议）。
// 运行时缺失时保守返回 true：查询失败不应改变既有候选排序。
func (g *Gateway) candidatePassesThrough(cr *router.ChannelRoute, baseProtocol string) bool {
	rt, ok := g.channels.GetRuntime(cr.ChannelID)
	if !ok {
		return true
	}
	mr, ok := g.channels.GetModelRuntime(cr.ChannelID, cr.ModelRowID)
	if !ok {
		return true
	}
	return effectiveProtocols(rt, mr).Has(baseProtocol)
}

// reorderPassthroughFirst 稳定分区：passthrough 为真的候选按相对顺序前移，其余靠后。
// 无区分度（全真/全假）或原序已满足分区时保持原序，保证既有行为零变化。
// 返回是否真的发生了顺序变化，供调用方仅在重排时给决策轨迹追加标记（未重排时键集不变）。
func reorderPassthroughFirst(routes []*router.ChannelRoute, passthrough func(*router.ChannelRoute) bool) bool {
	if len(routes) < 2 {
		return false
	}
	direct := make([]*router.ChannelRoute, 0, len(routes))
	rest := make([]*router.ChannelRoute, 0, len(routes))
	for _, cr := range routes {
		if passthrough(cr) {
			direct = append(direct, cr)
		} else {
			rest = append(rest, cr)
		}
	}
	if len(direct) == 0 || len(rest) == 0 {
		return false
	}
	ordered := append(direct, rest...)
	for i := range ordered {
		if ordered[i] != routes[i] {
			copy(routes, ordered)
			return true
		}
	}
	return false
}

// 直通重排标记取值：直通候选前移 / 被后移的兜底候选（仅在实际发生重排时写入轨迹）。
const (
	reorderMarkFirst    = "first"
	reorderMarkFallback = "fallback"
)

// convPlan 记录一次候选的协议转换上下文：入站协议、实际上游协议、是否发生转换，
// 以及该候选在直通重排中的位置标记（空=未重排，不产出任何字段）。
// 直通（converted=false）且未重排时 conversionAttemptFields 不产出任何字段，保持既有键集不变。
type convPlan struct {
	inbound   string
	upstream  string
	converted bool
	reorder   string
}

// conversionAttemptFields 把转换上下文、损耗报告与直通重排标记转为决策轨迹附加字段：
// 全部为空时返回 nil——纯直通且未重排的候选不新增任何键；
// 转换时写入 converted/inbound_protocol/upstream_protocol，报告非空时再附 lossy/lossy_notes。
func conversionAttemptFields(plan convPlan, rep *protocolconv.LossyReport) map[string]any {
	// 纯直通且未重排：不产出任何字段，直接返回 nil（避免每次命中都白白分配 map）。
	if !plan.converted && plan.reorder == "" {
		return nil
	}
	m := make(map[string]any, 6)
	if plan.converted {
		m["converted"] = true
		m["inbound_protocol"] = plan.inbound
		m["upstream_protocol"] = plan.upstream
		if rep != nil && !rep.IsEmpty() {
			m["lossy"] = rep.Dropped
			m["lossy_notes"] = rep.Notes
		}
	}
	switch plan.reorder {
	case reorderMarkFirst:
		m["passthrough_first"] = true
	case reorderMarkFallback:
		m["passthrough_fallback"] = true
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// callLogDecisionConv 在 callLogDecision 基础上，将转换上下文/损耗并入「当前候选」的时间线条目
// （即决策轨迹最后一项），使 call_logs 能看到该候选的入站→上游协议与丢失项。
// 纯直通（converted=false）时不改动任何内容，输出与 callLogDecision 完全一致。
func (g *Gateway) callLogDecisionConv(attempts []map[string]any, result string, pc preCharge,
	cur *router.RouteResult, conv convPlan, lossy *protocolconv.LossyReport) map[string]any {
	dec := g.callLogDecision(attempts, result, pc, cur)
	fields := conversionAttemptFields(conv, lossy)
	if len(fields) == 0 {
		return dec
	}
	if arr, ok := dec["attempts"].([]map[string]any); ok && len(arr) > 0 {
		for k, v := range fields {
			arr[len(arr)-1][k] = v
		}
	}
	return dec
}
