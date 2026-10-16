package resp

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/team/llmgateway/internal/pkg/logger"
)

type Body struct {
	Code      int         `json:"Code"`
	Message   string      `json:"Message"`
	RequestID string      `json:"RequestID"`
	Data      interface{} `json:"Data,omitempty"`
}

// errorLogger 内部 API 错误响应的日志落点（装配层注入；nil 时仅写响应、不落日志，单测安全）。
// 通过本包统一落日志，保证「任何内部 API 异常响应都有且只有一条结构化日志」，
// 不必在每个 handler 里重复插桩，也不会漏掉新增接口。
var errorLogger *slog.Logger

// SetErrorLogger 注入错误日志落点（建议在装配层调用一次）。
func SetErrorLogger(l *slog.Logger) { errorLogger = l }

// logAPIError 落一条内部 API 异常日志：method/path/status/业务码/文案 + requestId。
// 级别按状态码分级（5xx=Error，其余 4xx/业务拒绝=Warn），便于运维按级别过滤与告警。
func logAPIError(r *http.Request, status, code int, msg string) {
	if errorLogger == nil || r == nil {
		return
	}
	lvl := slog.LevelWarn
	if status >= http.StatusInternalServerError {
		lvl = slog.LevelError
	}
	logger.WithRequestID(r.Context(), errorLogger).Log(r.Context(), lvl, "api error",
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"code", code,
		"message", msg,
	)
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
		// 序列化失败属网关侧缺陷（多为 Data 里混入 NaN/Inf 或不可序列化值）：必须留日志。
		if errorLogger != nil {
			errorLogger.Error("api response marshal failed",
				"status", status, "code", body.Code, "err", err, "requestId", body.RequestID)
		}
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

// Err 写出内部 API 错误响应，并同步落一条异常日志（本包是全部内部 API 错误的唯一出口）。
func Err(w http.ResponseWriter, r *http.Request, status, code int, msg string) {
	logAPIError(r, status, code, msg)
	Write(w, status, Body{Code: code, Message: msg, RequestID: RequestID(r)})
}
func RequestID(r *http.Request) string {
	return logger.GetRequestID(r.Context())
}

// WithRequestID 将 requestID 写入请求上下文，供后续逻辑取用。
func WithRequestID(r *http.Request, id string) *http.Request {
	return r.WithContext(logger.SetRequestID(r.Context(), id))
}
