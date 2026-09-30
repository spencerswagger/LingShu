// Package server 负责 HTTP 服务装配：全局中间件、路由注册与优雅退出。
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/pkg/jwtx"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// 用户身份在 context 中的键类型，避免字符串键冲突。
type ctxKey string

const (
	keyUserID   ctxKey = "user.id"
	keyUsername ctxKey = "user.username"
	keyRole     ctxKey = "user.role"
)

// UserIDFrom 从 context 读取认证用户 ID。
func UserIDFrom(ctx context.Context) (int64, bool) {
	if v := ctx.Value(keyUserID); v != nil {
		if id, ok := v.(int64); ok {
			return id, true
		}
	}
	return 0, false
}

// UsernameFrom 从 context 读取认证用户名。
func UsernameFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(keyUsername).(string)
	return v, ok
}

// RoleFrom 从 context 读取认证用户角色。
func RoleFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(keyRole).(string)
	return v, ok
}

// newRequestID 生成请求 ID（16 字节随机 hex）。
func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000000")))
	}
	return hex.EncodeToString(b)
}

// WithRequestID 确保请求带 x-request-id：缺失时生成并写入 context 与响应头。
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("x-request-id")
		if id == "" {
			id = newRequestID()
		}
		r = resp.WithRequestID(r, id)
		w.Header().Set("x-request-id", id)
		next.ServeHTTP(w, r)
	})
}

// WithLogging 输出请求访问日志：method/path/status/耗时/requestId。
func WithLogging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(sw, r)
			logger.Log(r.Context(), slog.LevelInfo, "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"requestId", resp.RequestID(r),
			)
		})
	}
}

// statusWriter 捕获写入的 HTTP 状态码用于日志。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// WithRecover 捕获 panic，返回 500 并附带 requestId。
func WithRecover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Log(r.Context(), slog.LevelError, "panic recovered",
						"panic", rec,
						"stack", string(debug.Stack()),
						"requestId", resp.RequestID(r),
					)
					resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "服务器内部错误")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken 从 Authorization: Bearer <token> 中提取 token。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// WithAuth 校验 Bearer JWT，并断言 claims.Role 属于 roles。
// 认证失败返回 40101，越权（角色不符）返回 40301。
// 认证通过后将 userID/username/role 写入 context。
func WithAuth(mgr *jwtx.Manager, roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
				return
			}
			claims, err := mgr.Parse(token)
			if err != nil {
				resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "登录凭证无效或已过期")
				return
			}
			if !roleAllowed(claims.Role, roles) {
				resp.Err(w, r, http.StatusForbidden, resp.CodeForbidden, "无权访问该资源")
				return
			}
			ctx := context.WithValue(r.Context(), keyUserID, claims.UserID)
			ctx = context.WithValue(ctx, keyUsername, claims.Username)
			ctx = context.WithValue(ctx, keyRole, claims.Role)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func roleAllowed(role string, roles []string) bool {
	if len(roles) == 0 {
		return true
	}
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}