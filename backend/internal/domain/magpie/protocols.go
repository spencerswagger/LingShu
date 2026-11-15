// protocols.go 定义入站协议识别与 model 改写落点。
//
// 四类入站端点（与 magpie 出站同形，同端口靠 path 区分）：
//
//	POST /v1/chat/completions                         OpenAI Chat        model 在 body
//	POST /v1/responses                                OpenAI Responses   model 在 body
//	POST /v1/messages                                 Anthropic Messages model 在 body
//	POST /v1/messages/count_tokens                    Anthropic 计数     model 在 body（不计费、无 usage）
//	POST /v1beta/models/{model}:generateContent       Gemini             model 在 **path**
//	POST /v1beta/models/{model}:streamGenerateContent Gemini             model 在 path（stream）
//	POST /v1beta/models/{model}:countTokens           Gemini 计数        model 在 path（不计费、无 usage）
//
// Gemini 的 model 在 URL path 里，是唯一「改写点不在 body」的特例。
package magpie

import (
	"fmt"
	"net/http"
	"strings"
)

// Protocol 是入站协议标识（错误体形状、usage 字段路径、model 改写落点都由它决定）。
type Protocol string

// 四类入站协议（OpenAI 两个端点的协议不同：chat 与 responses 的 usage 字段路径不一样）。
const (
	ProtocolOpenAIChat      Protocol = "openai-chat"
	ProtocolOpenAIResponses Protocol = "openai-responses"
	ProtocolAnthropic       Protocol = "anthropic-messages"
	ProtocolGemini          Protocol = "gemini"
)

// Route 是一次转发的路由事实。
type Route struct {
	// Protocol 决定 usage 解析与错误体形状。
	Protocol Protocol
	// Endpoint 是入站端点名（日志/结果标记用，例如 "chat.completions"）。
	Endpoint string
	// UpstreamPath 是转发到 magpie 的 path（与入站一致；Gemini 由 rewritePathModel 改写 model 段）。
	UpstreamPath string
	// ModelInPath 表示 model 在 path 里（Gemini），body 内没有 model 字段。
	ModelInPath bool
	// Billable 表示该端点参与计费（count_tokens 类为 false：不计费、不读 usage）。
	Billable bool
	// StreamHint 表示入站已声明流式（Gemini 的 :streamGenerateContent）；其余协议看 body.stream。
	StreamHint bool
}

// ErrUnsupportedEndpoint 表示入站路径不属于本代理支持的协议族。
var ErrUnsupportedEndpoint = fmt.Errorf("不支持的转发端点")

// Classify 把入站请求识别为 Route。
//
// 只认契约里写死的路径，其余一律 ErrUnsupportedEndpoint（不猜、不透传任意 path：
// 放宽 path 等于把灵枢变成一个可被扫描的开放代理）。
func Classify(method, path string) (Route, error) {
	if method != http.MethodPost {
		return Route{}, ErrUnsupportedEndpoint
	}
	switch path {
	case "/v1/chat/completions":
		return Route{Protocol: ProtocolOpenAIChat, Endpoint: "chat.completions", UpstreamPath: path, Billable: true}, nil
	case "/v1/responses":
		return Route{Protocol: ProtocolOpenAIResponses, Endpoint: "responses", UpstreamPath: path, Billable: true}, nil
	case "/v1/messages":
		return Route{Protocol: ProtocolAnthropic, Endpoint: "messages", UpstreamPath: path, Billable: true}, nil
	case "/v1/messages/count_tokens":
		return Route{Protocol: ProtocolAnthropic, Endpoint: "messages.count_tokens", UpstreamPath: path, Billable: false}, nil
	}
	if strings.HasPrefix(path, "/v1beta/models/") {
		return classifyGemini(path)
	}
	return Route{}, ErrUnsupportedEndpoint
}

// geminiPrefix 是 Gemini 端点的 path 前缀。
const geminiPrefix = "/v1beta/models/"

// classifyGemini 解析 /v1beta/models/{model}:{action}。
// action 只认 generateContent / streamGenerateContent / countTokens（其余视为不支持）。
func classifyGemini(path string) (Route, error) {
	rest := strings.TrimPrefix(path, geminiPrefix)
	i := strings.LastIndex(rest, ":")
	if i <= 0 || i == len(rest)-1 {
		return Route{}, ErrUnsupportedEndpoint
	}
	model, action := rest[:i], rest[i+1:]
	if model == "" || strings.Contains(model, "/") {
		return Route{}, ErrUnsupportedEndpoint
	}
	r := Route{Protocol: ProtocolGemini, ModelInPath: true, UpstreamPath: path}
	switch action {
	case "generateContent":
		r.Endpoint, r.Billable = "models.generateContent", true
	case "streamGenerateContent":
		r.Endpoint, r.Billable, r.StreamHint = "models.streamGenerateContent", true, true
	case "countTokens":
		r.Endpoint, r.Billable = "models.countTokens", false
	default:
		return Route{}, ErrUnsupportedEndpoint
	}
	return r, nil
}

// pathModel 取出 Gemini path 里的 model 段（其余协议返回空串）。
func pathModel(path string) string {
	rest := strings.TrimPrefix(path, geminiPrefix)
	i := strings.LastIndex(rest, ":")
	if i <= 0 {
		return ""
	}
	return rest[:i]
}

// rewritePathModel 把 Gemini path 里的 model 段替换为代理模型名，其余部分（action/前缀）逐字不动。
func rewritePathModel(path, magpieModel string) (string, error) {
	rest := strings.TrimPrefix(path, geminiPrefix)
	i := strings.LastIndex(rest, ":")
	if i <= 0 || i == len(rest)-1 {
		return "", ErrUnsupportedEndpoint
	}
	return geminiPrefix + magpieModel + rest[i:], nil
}

// PathModel 取出 path 里的 model 段（仅 Gemini；其余协议返回空串）。
//
// 对外导出是给主链用的：model 解析（准入/计费）在主链，本包只负责「model 在哪」。
func PathModel(path string) string { return pathModel(path) }

// RewritePathModel 把 path 里的 model 段替换为上游模型名，其余部分逐字不动（仅 Gemini）。
func RewritePathModel(path, upstreamModel string) (string, error) {
	return rewritePathModel(path, upstreamModel)
}
