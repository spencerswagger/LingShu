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

// reasoningDeclaration 是渠道对「推理内容回传」的声明（来自挂载的 thinking-roundtrip 参数）。
type reasoningDeclaration struct {
	Supported    bool   // 是否声明该上游支持推理内容回传（enabled=true）
	Field        string // 请求侧写入字段名（缺省 reasoning_content，已去空白）
	OnlyThinking bool   // 是否仅对显式 thinking 模式的请求生效
}

// reasoningDeclarationOf 单次遍历协议特性解析上述声明；未挂载该特性时返回「不支持 + 默认字段名」。
func reasoningDeclarationOf(rt *channel.RuntimeChannel) reasoningDeclaration {
	for _, f := range protocolFeatures(rt) {
		if f.Key != channelprotocol.FeatureThinkingRoundtrip {
			continue
		}
		return reasoningDeclaration{
			Supported:    boolParam(f.Params, "enabled", false),
			Field:        normalizeReasoningField(stringParam(f.Params, "field", reasoningResponseField)),
			OnlyThinking: boolParam(f.Params, "onlyThinking", false),
		}
	}
	return reasoningDeclaration{Field: reasoningResponseField}
}

// channelReasoningInject 返回本次请求实际生效的「推理注入/回填」选项：
// 需渠道声明支持（thinking-roundtrip enabled=true）；若同时声明 onlyThinking=true，则仅当
// 调用方以入站请求体判定请求处于 thinking 模式（thinking 布尔/对象、enable_thinking、
// reasoning_effort，见 isThinkingRequestBody）后传入 true 时才生效。该判定与直通路径
// thinking-roundtrip 的 onlyThinking 同源，三条路径（直通改写、转换注入、会话级缓存回填）语义一致。
// 注意：判定只按请求体字段进行，不含「模型名隐含 thinking」（如 reasoner 类模型）；这类渠道应保持
// onlyThinking=false（前端提示已注明），否则推理内容不会回传、上游仍会 400。
// 未声明/未启用/不满足 onlyThinking 时返回零值：调用方据此既不注入也不回填，请求体逐字节不变。
// thinking 标记与候选渠道无关（body 与 rewriteModel 后的 body 仅 model 字段不同），
// 由调用方在候选循环外对原始入站请求体单次求值后传入，避免每个候选重复解析同一请求体。
func channelReasoningInject(rt *channel.RuntimeChannel, thinking bool) protocolconv.SerializeOptions {
	decl := reasoningDeclarationOf(rt)
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
