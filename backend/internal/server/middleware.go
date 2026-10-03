// Package server 负责 HTTP 服务装配：全局中间件、路由注册与优雅退出。
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/domain/audit"
	"github.com/team/llmgateway/internal/pkg/clientip"
	"github.com/team/llmgateway/internal/pkg/jwtx"
	"github.com/team/llmgateway/internal/pkg/resp"
	"github.com/team/llmgateway/internal/pkg/session"
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

// requestIDRe 限定允许透传的客户端 request id：ASCII 可见字符子集，长度 ≤64。
// 约束长度使其不超过审计列宽；约束字符集避免任意字节进入日志/审计（日志伪造）。
// 不合法（含空串、超长、非法字符）一律丢弃客户端值，改由服务端生成，
// 而不是拒绝请求——保持对现有客户端的兼容。
var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// WithRequestID 确保请求带 x-request-id：客户端值合规则沿用，否则生成新的，
// 写入 context 与响应头。
// 必须置于中间件链最外层：它通过 r.WithContext 把 id 传给内层，
// 外层中间件（WithLogging/WithRecover）只能读到内层传入的同一个 r。
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("x-request-id")
		if !requestIDRe.MatchString(id) {
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

// WithAuth 校验 Bearer JWT：Parse（RFC 全字段）→ SessionRegistry 校验 ver/status，
// 断言 roles；must_change_password 用户仅放行白名单路径。
// 认证失败返回 40101，越权（角色不符）返回 40301，强制改密未完成返回 40302。
// 认证通过后将 userID/username/role 写入 context。
func WithAuth(mgr *jwtx.Manager, sess *session.Registry, roles ...string) func(http.Handler) http.Handler {
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
			userID, err := claims.UserID()
			if err != nil {
				resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "登录凭证无效或已过期")
				return
			}
			if !roleAllowed(claims.Role, roles) {
				resp.Err(w, r, http.StatusForbidden, resp.CodeForbidden, "无权访问该资源")
				return
			}
			if sess != nil {
				mustChange, serr := sess.Check(r.Context(), userID, claims.Ver)
				if serr != nil {
					resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "登录凭证无效或已过期")
					return
				}
				if mustChange && !allowedWhileMustChange(r) {
					resp.Err(w, r, http.StatusForbidden, resp.CodeMustChangePassword, "请先修改默认密码")
					return
				}
			}
			ctx := context.WithValue(r.Context(), keyUserID, userID)
			ctx = context.WithValue(ctx, keyUsername, claims.Username)
			ctx = context.WithValue(ctx, keyRole, claims.Role)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// allowedWhileMustChange 强制改密期间仅放行的路径。
func allowedWhileMustChange(r *http.Request) bool {
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v1/auth/me":
		return true
	case r.Method == "PUT" && r.URL.Path == "/api/v1/auth/me/password":
		return true
	case r.Method == "POST" && r.URL.Path == "/api/v1/auth/logout":
		return true
	}
	return false
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

// WithAudit 记录管理端「写操作」请求审计（method + path + 操作者 + request_id + 可信 IP）。
// 须包在 WithAuth 内层以取得 user 上下文；target_type/target_id 从路径解析。
// 只记写操作（POST/PUT/PATCH/DELETE）——读操作留痕噪声大且无追责价值。
func WithAudit(store *audit.Store, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			if store == nil || !isAuditableMethod(r.Method) {
				return
			}
			uid, _ := UserIDFrom(r.Context())
			username, _ := UsernameFrom(r.Context())
			e := audit.Entry{
				UserID:    uid,
				Username:  username,
				Action:    r.Method + " " + r.URL.Path,
				RequestID: resp.RequestID(r),
				IP:        clientip.From(r),
			}
			e.TargetType, e.TargetID = auditTarget(r)
			// 审计是对"已发生事实"的记录：脱离请求生命周期（客户端可能已断开），
			// 并给独立超时，避免拖住连接（此时响应已发出）。
			actx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
			defer cancel()
			if err := store.Insert(actx, e); err != nil {
				// 审计失败不影响业务响应，仅记日志（后续可接指标/告警）
				logger.Error("audit insert failed", "action", e.Action, "err", err)
			}
		})
	}
}

// isAuditableMethod 仅写操作入审计（读操作噪声大且无追责价值）。
func isAuditableMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// auditTarget 从管理端路径解析目标类型与目标 ID，如 /api/v1/admin/users/12 → ("user","12")。
func auditTarget(r *http.Request) (string, string) {
	const prefix = "/api/v1/admin/"
	p := strings.TrimPrefix(r.URL.Path, prefix)
	if p == r.URL.Path {
		return "", ""
	}
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if len(segs) == 0 || segs[0] == "" {
		return "", ""
	}
	// 路径首段为资源复数名（users/tokens/channels/...）；单数化去尾 s 作 target_type。
	t := strings.TrimSuffix(segs[0], "s")
	id := r.PathValue("id")
	return t, id
}
