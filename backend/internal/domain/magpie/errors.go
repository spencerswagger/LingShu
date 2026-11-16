// errors.go 是灵枢自产错误的统一出口：错误体按**入站协议形状**输出，状态码按契约映射。
//
// 上游（magpie）自身的 4xx/5xx **不经过这里**：透传口径下它原样回流，不二次包装
// （包装就把上游错误体改了）。这里只处理灵枢自己判定的情况：
//
//	连不上 magpie / 建连失败 / DNS      → 503 + Retry-After: 5
//	首包超时 / 流空闲超时                → 504
//	令牌无效                            → 401
//	模型不存在 / 已停用 / 未映射代理模型  → 404（model_not_found，前者与后者文案不同）
//	模型属订阅池 / 未对租户开放          → 403（model_not_allowed）
//	令牌未开通代理访问（无 callerKey）    → 403（magpie_key_missing）
//	请求体不合法 / 缺少 model            → 400
package magpie

import (
	"encoding/json"
	"net/http"
)

// Deny 是灵枢自产拒绝/失败的描述（与协议无关的语义，由 writer 转成协议形状）。
type Deny struct {
	// Status 是下游 HTTP 状态码。
	Status int
	// Code 是网关归类码（写进 OpenAI 形状的 error.code；Anthropic/Gemini 用类型/状态表达）。
	Code string
	// Message 是给调用方看的说明（不含内部地址、不含凭据）。
	Message string
	// RetryAfter 非空时写入 Retry-After 头（秒），仅用于「可重试」的上游不可用。
	RetryAfter string
}

// 网关归类码常量（与既有网关口径保持一致，便于调用方按 code 分支）。
const (
	CodeInvalidRequest     = "invalid_request_error"
	CodeInvalidAPIKey      = "invalid_api_key"
	CodeModelNotFound      = "model_not_found"
	CodeModelNotAllowed    = "model_not_allowed"
	CodeMagpieKeyMissing   = "magpie_key_missing"
	CodeServiceUnavailable = "service_unavailable"
	CodeUpstreamTimeout    = "upstream_timeout"
	CodeInternal           = "internal_error"
)

// WriteDeny 按入站协议形状写出拒绝/失败响应。
// protocol 为空或未知时回落到 OpenAI 形状（兼容最广、且是既有网关的默认形状）。
func WriteDeny(w http.ResponseWriter, protocol Protocol, d Deny) {
	if d.Status == 0 {
		d.Status = http.StatusInternalServerError
	}
	if d.RetryAfter != "" {
		w.Header().Set("Retry-After", d.RetryAfter)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(d.Status)
	switch protocol {
	case ProtocolAnthropic:
		_ = json.NewEncoder(w).Encode(anthropicErrorBody{
			Type: "error",
			Error: anthropicErrorDetail{
				Type:    anthropicErrorType(d.Status),
				Message: d.Message,
			},
		})
	case ProtocolGemini:
		_ = json.NewEncoder(w).Encode(geminiErrorBody{
			Error: geminiErrorDetail{
				Code:    d.Status,
				Message: d.Message,
				Status:  geminiErrorStatus(d.Status),
			},
		})
	default:
		_ = json.NewEncoder(w).Encode(openAIErrorBody{
			Error: openAIErrorDetail{
				Message: d.Message,
				Type:    openAIErrorType(d.Status),
				Code:    d.Code,
			},
			RequestID: w.Header().Get("x-request-id"),
		})
	}
}

type openAIErrorBody struct {
	Error     openAIErrorDetail `json:"error"`
	RequestID string            `json:"request_id,omitempty"`
}

type openAIErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

type anthropicErrorBody struct {
	Type  string               `json:"type"`
	Error anthropicErrorDetail `json:"error"`
}

type anthropicErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type geminiErrorBody struct {
	Error geminiErrorDetail `json:"error"`
}

type geminiErrorDetail struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// openAIErrorType 沿用既有网关口径（调用方按 type 分支的存量行为不变）。
func openAIErrorType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusPaymentRequired:
		return "insufficient_quota"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// anthropicErrorType 把 HTTP 状态码映射为 Anthropic 官方错误类型枚举。
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// geminiErrorStatus 把 HTTP 状态码映射为 Google API 的 canonical status 名。
func geminiErrorStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusRequestEntityTooLarge:
		return "INVALID_ARGUMENT"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusGatewayTimeout:
		return "DEADLINE_EXCEEDED"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	default:
		return "INTERNAL"
	}
}
