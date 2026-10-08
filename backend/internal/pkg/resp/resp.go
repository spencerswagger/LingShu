package resp

import (
	"encoding/json"
	"net/http"

	"github.com/team/llmgateway/internal/pkg/logger"
)

type Body struct {
	Code      int         `json:"Code"`
	Message   string      `json:"Message"`
	RequestID string      `json:"RequestID"`
	Data      interface{} `json:"Data,omitempty"`
}

// 统一业务错误码
const (
	CodeOK                 = 0
	CodeBadRequest         = 40001
	CodeUnauthorized       = 40101
	CodeForbidden          = 40301
	CodeMustChangePassword = 40302 // 需先修改默认密码
	CodeNotFound           = 40401
	CodeConflict           = 40901
	CodeInsufficient       = 40201 // 余额不足
	CodeRateLimited        = 42901
	CodeNoRoute            = 50301 // 无匹配渠道
	CodeInternalError      = 50001
)

func Write(w http.ResponseWriter, status int, body Body) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 先序列化再写状态码：若 Data 含 NaN/Inf 等不可序列化值，避免出现
	// 「200 + 空响应体」这种静默失败（调用方无法解析），改为返回可诊断的 500。
	buf, err := json.Marshal(body)
	if err != nil {
		fallback, _ := json.Marshal(Body{
			Code:      CodeInternalError,
			Message:   "响应序列化失败",
			RequestID: body.RequestID,
		})
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(fallback)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}
func OK(w http.ResponseWriter, r *http.Request, data interface{}) {
	Write(w, http.StatusOK, Body{Code: CodeOK, Message: "ok", RequestID: RequestID(r), Data: data})
}
func Err(w http.ResponseWriter, r *http.Request, status, code int, msg string) {
	Write(w, status, Body{Code: code, Message: msg, RequestID: RequestID(r)})
}
func RequestID(r *http.Request) string {
	return logger.GetRequestID(r.Context())
}

// WithRequestID 将 requestID 写入请求上下文，供后续逻辑取用。
func WithRequestID(r *http.Request, id string) *http.Request {
	return r.WithContext(logger.SetRequestID(r.Context(), id))
}
