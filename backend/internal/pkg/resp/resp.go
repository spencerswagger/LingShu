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

// logAPIError 落一条内部 API 异常日志：method/path/status/业务码/文案 + requestId。
// logger 由装配层中间件经 context 注入（无包级全局，避免多处构造 server 相互覆盖）；
// 未注入时回退 logger.Fallback()（stdout，与主 logger 同出口）——宁可日志换个出口，也不静默丢弃异常记录。
// 级别按状态码分级（5xx=Error，其余 4xx/业务拒绝=Warn）；
// 4xx 受 logger.ClientErrorLogEnabled 开关控制（默认开，可用 LOG_CLIENT_ERRORS=off 关闭以防刷屏），
// 5xx 始终记录。
func logAPIError(r *http.Request, status, code int, msg string) {
	if r == nil || (status < http.StatusInternalServerError && !logger.ClientErrorLogEnabled()) {
		return
	}
	l := logger.From(r.Context())
	if l == nil {
		// 装配层漏注入时不允许静默丢弃：回退 logger.Fallback()（stdout，与主 logger 同出口），
		// 保证内部 API 异常至少留下一条可见记录（否则只剩访问日志的 status，无法定位到业务码与文案）。
		l = logger.Fallback()
	}
	lvl := slog.LevelWarn
	if status >= http.StatusInternalServerError {
		lvl = slog.LevelError
	}
	args := []any{
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"code", code,
		"message", msg,
	}
	logger.WithRequestID(r.Context(), l).Log(r.Context(), lvl, "api error", args...)
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

// Write 写出统一响应壳；序列化失败时写出可诊断的 500 并返回错误，
// 由调用方（OK/Err，持有请求上下文）落日志——Write 自身没有请求上下文，不能直接落日志。
func Write(w http.ResponseWriter, status int, body Body) error {
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
		return err
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf)
	return nil
}

func OK(w http.ResponseWriter, r *http.Request, data interface{}) {
	if err := Write(w, http.StatusOK, Body{Code: CodeOK, Message: "ok", RequestID: RequestID(r), Data: data}); err != nil {
		logAPIError(r, http.StatusInternalServerError, CodeInternalError, "响应序列化失败："+err.Error())
	}
}

// Err 写出内部 API 错误响应，并同步落一条异常日志（本包是全部内部 API 错误的唯一出口）。
func Err(w http.ResponseWriter, r *http.Request, status, code int, msg string) {
	logAPIError(r, status, code, msg)
	if err := Write(w, status, Body{Code: code, Message: msg, RequestID: RequestID(r)}); err != nil {
		logAPIError(r, http.StatusInternalServerError, CodeInternalError, "响应序列化失败："+err.Error())
	}
}

// ErrNoLog 与 Err 等价，但**不落异常日志**：只给「预期内的拒绝」用（如功能开关未启用 → 403 / 能力降级）。
// 这类响应是正常业务状态，落 Warn 会让 4xx 告警统计把「开关关着」误报成故障。
// 序列化失败仍按 5xx 落日志（那是真异常）。除此之外的一切错误都必须走 Err。
func ErrNoLog(w http.ResponseWriter, r *http.Request, status, code int, msg string) {
	if err := Write(w, status, Body{Code: code, Message: msg, RequestID: RequestID(r)}); err != nil {
		logAPIError(r, http.StatusInternalServerError, CodeInternalError, "响应序列化失败："+err.Error())
	}
}
func RequestID(r *http.Request) string {
	return logger.GetRequestID(r.Context())
}

// WithRequestID 将 requestID 写入请求上下文，供后续逻辑取用。
func WithRequestID(r *http.Request, id string) *http.Request {
	return r.WithContext(logger.SetRequestID(r.Context(), id))
}
