package logger

import (
	"context"
	"io"
	"log/slog"
	"os"
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