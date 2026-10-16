package logger

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
)

// CtxRequestIDKey 是 requestID 在 context 中的键类型，供 middleware 与 resp 共用。
type ctxKeyRequestID struct{}

// SetRequestID 将 requestID 写入 context。
func SetRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyRequestID{}, id)
}

// GetRequestID 从 context 读取 requestID，不存在则返回空串。
func GetRequestID(ctx context.Context) string {
	if v := ctx.Value(ctxKeyRequestID{}); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// New 创建 slog.Logger，输出到 w。
func New(w io.Writer, level slog.Level) *slog.Logger {
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(h)
}

// NewDefault 输出到 stdout，级别 INFO。
func NewDefault() *slog.Logger {
	return New(os.Stdout, slog.LevelInfo)
}

// WithRequestID 从 ctx 读取 requestID，有则返回带 requestId 字段的新 logger。
func WithRequestID(ctx context.Context, l *slog.Logger) *slog.Logger {
	if id := GetRequestID(ctx); id != "" {
		return l.With("requestId", id)
	}
	return l
}

// ---- 请求级 logger 注入 ----
//
// 供 resp 等"无 logger 参数"的底层包落日志：由装配层中间件在请求入口注入一次，
// 走 context 而非包级全局变量，避免多处构造 server 时相互覆盖。

type ctxKeyLogger struct{}

// WithLogger 把 logger 注入 context；l 为 nil 时原样返回。
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyLogger{}, l)
}

// From 从 context 取出注入的 logger；未注入返回 nil（调用方据此跳过日志，单测安全）。
func From(ctx context.Context) *slog.Logger {
	if v, ok := ctx.Value(ctxKeyLogger{}).(*slog.Logger); ok {
		return v
	}
	return nil
}

// ---- 客户端错误日志开关 ----
//
// 4xx/业务拒绝在网关与内部 API 三处都会落日志；高并发下失效令牌、脚本化错误调用
// 可能刷屏并淹没真实异常，故提供进程级开关（环境变量 LOG_CLIENT_ERRORS=off 关闭）。
// 关闭后 4xx 仍进入访问日志（降为 Info 级），5xx 始终记录。

var clientErrLog = func() *atomic.Bool {
	b := &atomic.Bool{}
	b.Store(os.Getenv("LOG_CLIENT_ERRORS") != "off")
	return b
}()

// ClientErrorLogEnabled 返回"客户端错误是否落日志"。
func ClientErrorLogEnabled() bool { return clientErrLog.Load() }

// SetClientErrorLogEnabled 设置客户端错误日志开关（运维/测试用）。
func SetClientErrorLogEnabled(on bool) { clientErrLog.Store(on) }
