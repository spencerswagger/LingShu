package gateway

import (
	"strings"

	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
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

// protocolFeatures 取渠道运行时挂载的协议特性；内联渠道（无渠道协议）返回 nil。
func protocolFeatures(rt *channel.RuntimeChannel) []channelprotocol.Feature {
	if rt == nil || rt.Protocol == nil {
		return nil
	}
	return rt.Protocol.Features
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
