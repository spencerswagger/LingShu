package gateway

import (
	"strings"

	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
	"github.com/team/llmgateway/internal/domain/protocolconv"
)

// 协议特性（protocol feature）：特性逻辑在代码中实现并注册，渠道协议按
// 「key + 参数」挂载（可多项启停）。两个扩展点：
//   - RequestFeature：请求构造阶段改写上游请求体；
//   - ResponseFeature：响应解析阶段改写/归一上游响应体。
//
// 未挂载任何特性时，两个扩展点均为空，主链路输出与改造前逐字节一致（默认安全态）。

// RequestFeature 请求构造阶段的协议特性：按上游约定改写请求体。
type RequestFeature interface {
	// Key 返回特性标识（与渠道协议挂载的 Feature.Key 对应）。
	Key() string
	// RewriteRequest 就地改写上游请求（通常改写 Body；失败返回错误，由调用方决定处置）。
	RewriteRequest(req *UpstreamRequest) error
}

// ResponseFeature 响应解析阶段的协议特性：改写/归一上游响应体。
type ResponseFeature interface {
	Key() string
	// NormalizeResponse 返回归一新后的响应体；未改动可原样返回入参。
	NormalizeResponse(body []byte) ([]byte, error)
}

// featureRegistry 协议特性注册表：请求/响应两个扩展点各自维护 key → 构造器。
// 构造器按挂载特性携带的 Params 生成特性实例。
type featureRegistry struct {
	request  map[string]func(params map[string]any) RequestFeature
	response map[string]func(params map[string]any) ResponseFeature
}

var features = &featureRegistry{
	request:  make(map[string]func(params map[string]any) RequestFeature),
	response: make(map[string]func(params map[string]any) ResponseFeature),
}

func init() { registerFeatures() }

// registerFeatures 注册内置协议特性（Step 6 / Step 7 实现）。
func registerFeatures() {
	features.registerRequest(channelprotocol.FeatureThinkingRoundtrip, newThinkingRoundtripFeature)
	features.registerRequest(channelprotocol.FeatureToolSchemaNormalize, newToolSchemaNormalizeFeature)
	features.registerRequest(channelprotocol.FeatureToolChoiceDowngrade, newToolChoiceDowngradeFeature)
}

// registerRequest 注册一个请求侧特性构造器。
func (r *featureRegistry) registerRequest(key string, newFn func(params map[string]any) RequestFeature) {
	r.request[key] = newFn
}

// registerResponse 注册一个响应侧特性构造器。
func (r *featureRegistry) registerResponse(key string, newFn func(params map[string]any) ResponseFeature) {
	r.response[key] = newFn
}

// requestFeaturesFor 按协议 features 顺序解析出请求侧特性实例。
// 未知 key 忽略（合法性已由 channelprotocol 校验层拦截）；未挂载返回 nil。
func requestFeaturesFor(protocolFeatures []channelprotocol.Feature) []RequestFeature {
	if len(protocolFeatures) == 0 {
		return nil
	}
	out := make([]RequestFeature, 0, len(protocolFeatures))
	for _, f := range protocolFeatures {
		if newFn, ok := features.request[f.Key]; ok {
			out = append(out, newFn(f.Params))
		}
	}
	return out
}

// responseFeaturesFor 按协议 features 顺序解析出响应侧特性实例（与请求侧同构）。
func responseFeaturesFor(protocolFeatures []channelprotocol.Feature) []ResponseFeature {
	if len(protocolFeatures) == 0 {
		return nil
	}
	out := make([]ResponseFeature, 0, len(protocolFeatures))
	for _, f := range protocolFeatures {
		if newFn, ok := features.response[f.Key]; ok {
			out = append(out, newFn(f.Params))
		}
	}
	return out
}

// applyRequestFeatures 依次应用请求侧特性改写（未挂载时调用方不应进入）。
func applyRequestFeatures(req *UpstreamRequest, feats []RequestFeature) error {
	for _, f := range feats {
		if err := f.RewriteRequest(req); err != nil {
			return err
		}
	}
	return nil
}

// applyResponseFeatures 依次应用响应侧特性归一；未挂载/无改动时原样返回 body。
func applyResponseFeatures(body []byte, feats []ResponseFeature) ([]byte, error) {
	for _, f := range feats {
		next, err := f.NormalizeResponse(body)
		if err != nil {
			return nil, err
		}
		if next != nil {
			body = next
		}
	}
	return body, nil
}

// normalizeUpstreamBody 对上游响应体按渠道协议挂载的响应侧特性依次归一；
// 未挂载/无特性/归一失败时原样返回 body（保持默认安全态，计费读取归一后的内容）。
func normalizeUpstreamBody(body []byte, rt *channel.RuntimeChannel) []byte {
	feats := responseFeaturesFor(protocolFeatures(rt))
	if len(feats) == 0 {
		return body
	}
	out, err := applyResponseFeatures(body, feats)
	if err != nil {
		return body
	}
	return out
}

// protocolFeatures 取渠道运行时挂载的协议特性；内联渠道（无渠道协议）返回 nil。
func protocolFeatures(rt *channel.RuntimeChannel) []channelprotocol.Feature {
	if rt == nil || rt.Protocol == nil {
		return nil
	}
	return rt.Protocol.Features
}

// convertedRequestFeatureKeys 是「对跨协议转换后的请求体同样安全」的请求侧特性白名单：
// 键为特性 key，值为该特性可安全应用的目标协议集合（转换结果已具备这些协议的标准字段）。
// 其余特性依赖源协议字段（thinking / anthropic 形状等），转换后已丢失，因此不登记在此。
var convertedRequestFeatureKeys = map[string][]string{
	channelprotocol.FeatureToolChoiceDowngrade: {
		channelprotocol.ProtocolOpenAIChat,
		channelprotocol.ProtocolOpenAIResponses,
		channelprotocol.ProtocolAnthropicMessages,
	},
}

// convertedRequestFeatureApplies 判断某请求侧特性是否可安全应用于目标协议转换后的请求体。
func convertedRequestFeatureApplies(key, protocol string) bool {
	for _, p := range convertedRequestFeatureKeys[key] {
		if p == protocol {
			return true
		}
	}
	return false
}

// applyConvertedRequestFeatures 对转换后的请求体应用上述安全白名单内的渠道特性。
// 是否适用由「特性 key → 目标协议集合」白名单判定；协议不在集合内即原样返回（无 error）。
// 改写失败向上返回错误，由调用方按「本候选失败」处置：跳过该候选继续、并置
// sawConvertSerializeFail，使全部候选失败时最终归为 500（本地构建失败）而非 503（上游不可用）。
func applyConvertedRequestFeatures(body []byte, rt *channel.RuntimeChannel, protocol string) ([]byte, error) {
	feats := requestFeaturesFor(protocolFeatures(rt))
	if len(feats) == 0 {
		return body, nil
	}
	req := UpstreamRequest{Body: body, Protocol: protocol}
	for _, f := range feats {
		if !convertedRequestFeatureApplies(f.Key(), protocol) {
			continue
		}
		if err := f.RewriteRequest(&req); err != nil {
			return body, err
		}
	}
	return req.Body, nil
}

// thinkingRoundtripParams 是 thinking-roundtrip 特性的挂载参数：请求侧直通改写
// （feature_thinking.go）与会话级推理缓存回填（reasoningDeclarationOf）共用同一解析口径，
// 避免 enabled / field / onlyThinking 在两条路径上出现语义漂移。
type thinkingRoundtripParams struct {
	Enabled      bool   // 是否启用（缺省 false）
	Field        string // 请求侧写入字段名（缺省 reasoning_content，已去空白）
	OnlyThinking bool   // 是否仅对显式 thinking 模式的请求生效
}

// parseThinkingRoundtripParams 解析 thinking-roundtrip 挂载参数（字段名已归一）。
func parseThinkingRoundtripParams(params map[string]any) thinkingRoundtripParams {
	return thinkingRoundtripParams{
		Enabled:      boolParam(params, "enabled", false),
		Field:        normalizeReasoningField(stringParam(params, "field", reasoningResponseField)),
		OnlyThinking: boolParam(params, "onlyThinking", false),
	}
}

// reasoningDeclaration 是渠道对「推理内容回传」的声明（来自挂载的 thinking-roundtrip 参数）。
type reasoningDeclaration struct {
	Mounted      bool   // 渠道协议是否显式挂载 thinking-roundtrip 特性
	Supported    bool   // 是否声明该上游支持推理内容回传（enabled=true）
	Field        string // 请求侧写入字段名（缺省 reasoning_content，已去空白）
	OnlyThinking bool   // 是否仅对显式 thinking 模式的请求生效
}

// reasoningDeclarationOf 单次遍历协议特性解析上述声明；未挂载该特性时返回「未挂载 + 不支持 + 默认字段名」。
func reasoningDeclarationOf(rt *channel.RuntimeChannel) reasoningDeclaration {
	for _, f := range protocolFeatures(rt) {
		if f.Key != channelprotocol.FeatureThinkingRoundtrip {
			continue
		}
		p := parseThinkingRoundtripParams(f.Params)
		return reasoningDeclaration{
			Mounted:      true,
			Supported:    p.Enabled,
			Field:        p.Field,
			OnlyThinking: p.OnlyThinking,
		}
	}
	return reasoningDeclaration{Field: reasoningResponseField}
}

// channelReasoningInject 返回本次请求实际生效的「推理注入/回填」选项：
//
// 生效规则（2026-11-11 修订）：
//   - 渠道协议**显式挂载** thinking-roundtrip：以挂载参数为准（enabled 缺省 false，仍须显式
//     enabled=true 才启用；onlyThinking 同前）。显式挂载是运维的明确意图，保持「缺省关闭」语义不变，
//     避免存量「挂载但未开 enabled」的渠道被悄悄改行为。
//   - 渠道协议**未挂载**：当且仅当 defaultEnabled 为真时生效。defaultEnabled 由调用方传入
//     `converted && 全局开关`（见 handler.serve 的 reasoningInject 求值）——
//     **默认值只覆盖跨协议转换路径**（如 Anthropic 入站 → openai-chat 上游的 thinking 多轮），
//     直通渠道（converted=false）即便全局开关为真也**不**默认回填，仍须显式挂载该特性。
//
// 为什么默认不覆盖直通：会话级推理缓存（reasoning.go）的键刻意只有 session + tool_call_id、
// 跨渠道/跨模型共享（failover 与成本路由场景下同一会话会落到不同渠道/厂商）。对直通渠道默认回填
// 会把 A 厂商上一轮 CoT 注入给 B 厂商，既可能泄漏推理内容，也依赖「上游忽略未知消息字段」这一
// 未验证假设。收窄到转换路径后，「只有客户端确实缺少 thinking 块、且网关确有该会话推理」的
// anthropic→openai 转换场景才默认生效，风险面最小。
//
// 未启用/不满足 onlyThinking 时返回零值：调用方据此既不注入也不回填，请求体逐字节不变。
// thinking 标记与候选渠道无关（body 与 rewriteModel 后的 body 仅 model 字段不同），
// 由调用方在候选循环外对原始入站请求体单次求值后传入，避免每个候选重复解析同一请求体。
func channelReasoningInject(rt *channel.RuntimeChannel, thinking bool, defaultEnabled bool) protocolconv.SerializeOptions {
	decl := reasoningDeclarationOf(rt)
	if !decl.Mounted {
		// 未挂载该特性：仅当调用方允许默认（跨协议转换路径 + 全局开关开启）时回填。
		// 显式挂载（含 enabled=false）是运维的明确意图，全局默认不覆盖。
		if !defaultEnabled {
			return protocolconv.SerializeOptions{}
		}
		decl = reasoningDeclaration{Supported: true, Field: reasoningResponseField}
	}
	if !decl.Supported {
		return protocolconv.SerializeOptions{}
	}
	if decl.OnlyThinking && !thinking {
		return protocolconv.SerializeOptions{}
	}
	return protocolconv.SerializeOptions{ReasoningContent: true, ReasoningField: decl.Field}
}

// boolParam 读取布尔参数（兼容 bool / 数字 / "true" 等字符串），缺失或类型不符返回 def。
func boolParam(params map[string]any, key string, def bool) bool {
	if params == nil {
		return def
	}
	v, ok := params[key]
	if !ok {
		return def
	}
	switch b := v.(type) {
	case bool:
		return b
	case float64:
		return b != 0
	case int:
		return b != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
	}
	return def
}

// stringParam 读取非空字符串参数，缺失/空/类型不符返回 def。
func stringParam(params map[string]any, key, def string) string {
	if params == nil {
		return def
	}
	if v, ok := params[key]; ok {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return def
}
