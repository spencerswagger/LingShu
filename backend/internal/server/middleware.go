// Package server 负责 HTTP 服务装配：全局中间件、路由注册与优雅退出。
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"
	"unicode/utf8"

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
//
// 日志写在 defer 中：panic 会把控制流从 next.ServeHTTP 直接掀起，函数尾部语句永不执行——
// 而 500（panic）恰恰是最需要出现在访问日志里的请求。defer 保证"每一次进入的请求都留下
// 一条访问记录"；panic 时状态记为 500，随后重新抛出，交给外层 WithRecover 写响应与 panic
// 日志（defer 内→外执行，故此处先记录、再上抛）。
func WithLogging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			defer func() {
				rec := recover()
				status := sw.status
				if rec != nil {
					status = http.StatusInternalServerError
				}
				logger.Log(r.Context(), slog.LevelInfo, "http request",
					"method", r.Method,
					"path", r.URL.Path,
					"status", status,
					"duration_ms", time.Since(start).Milliseconds(),
					"requestId", resp.RequestID(r),
				)
				if rec != nil {
					panic(rec)
				}
			}()
			next.ServeHTTP(sw, r)
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
						"method", r.Method,
						"path", r.URL.Path,
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
			// fail-closed：会话注册表是 ver/status 吊销校验的依赖，缺失时必须拒绝而非静默放行，
			// 否则禁用/删除用户的令牌会在仅剩签名校验的情况下继续有效。
			if sess == nil {
				resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "服务器内部错误")
				return
			}
			mustChange, serr := sess.Check(r.Context(), userID, claims.Ver)
			if serr != nil {
				resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "登录凭证无效或已过期")
				return
			}
			if mustChange && !allowedWhileMustChange(r) {
				resp.Err(w, r, http.StatusForbidden, resp.CodeMustChangePassword, "请先修改默认密码")
				return
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
// 只记写操作（POST/PUT/PATCH/DELETE）——读操作留痕噪声大且无追责价值；
// 敏感读（如明文密钥查看）走 WithAuditSensitiveRead 单独插桩。
// Detail：从请求体解析常见金额字段（amount/balance/credits/delta 等）写入，
// 便于追查账务类操作；解析失败或非 JSON 时静默跳过，不影响业务。
func WithAudit(store *audit.Store, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var detail any
			auditWrite := store != nil && isAuditableMethod(r.Method)
			if auditWrite {
				detail = readAuditDetail(r)
			}
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			if !auditWrite {
				return
			}
			e := audit.Entry{
				Action: r.Method + " " + r.URL.Path,
				Detail: detail,
			}
			storeAuditEntry(store, logger, r, e)
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

// sensitiveReadRe 匹配需要审计的敏感读路径：明文密钥查看等 GET 请求虽非写操作，
// 但「谁能看到明文、何时看的、从哪个 IP」本身就是最高价值追责点。
// 管理端与开发端均须覆盖——dev 用户查看自己令牌明文同样要留痕。
// 注意不能用 ServeMux pattern 直接做白名单 key（r.URL.Path 是实际路径），故用正则。
var sensitiveReadRe = regexp.MustCompile(`^GET /api/v1/(?:admin|dev)/tokens/[^/]+/secret$`)

// isSensitiveRead 判断请求是否为需审计的敏感读。
func isSensitiveRead(r *http.Request) bool {
	return sensitiveReadRe.MatchString(r.Method + " " + r.URL.Path)
}

// WithAuditSensitiveRead 审计「敏感读」请求：仅当命中敏感读白名单时留痕
// （含操作者/ip/request_id/目标 id），其余 GET 仍不审计。须包在 WithAuth 内层。
func WithAuditSensitiveRead(store *audit.Store, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			if store == nil || !isSensitiveRead(r) {
				return
			}
			e := audit.Entry{
				Action: "SENSITIVE_READ " + r.Method + " " + r.URL.Path,
			}
			storeAuditEntry(store, logger, r, e)
		})
	}
}

// storeAuditEntry 补全审计条目（操作者/request_id/可信 IP/目标）并写入存储：
// 审计是对"已发生事实"的记录，脱离请求生命周期（客户端可能已断开），
// 并给独立超时，避免拖住连接（此时响应已发出）；失败仅记日志。
func storeAuditEntry(store *audit.Store, logger *slog.Logger, r *http.Request, e audit.Entry) {
	e.UserID, _ = UserIDFrom(r.Context())
	e.Username, _ = UsernameFrom(r.Context())
	e.RequestID = resp.RequestID(r)
	e.IP = clientip.From(r)
	e.TargetType, e.TargetID = auditTarget(r)
	actx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
	defer cancel()
	if err := store.Insert(actx, e); err != nil {
		// 审计失败不影响业务响应，仅记日志（后续可接指标/告警）
		logger.Error("audit insert failed", "action", e.Action, "err", err)
	}
}

// auditAmountFields 写入审计 Detail 的常见金额字段名（契约 PascalCase：与 Go 字段名一致）。
var auditAmountFields = []string{"Amount", "Balance", "Credits", "Delta", "Credit", "Value", "Price", "Fee"}

// auditBodyLimit 读取审计 Detail 的请求体字节上限：足够覆盖常见 JSON 请求体，
// 同时防止超大体（如大文件上传）拖慢审计前置读取。
const auditBodyLimit = 1 << 20 // 1 MiB

// auditDetailMaxLen Detail 中字符串字段按 rune 截断的上限，防止超长值撑爆审计列。
const auditDetailMaxLen = 500

// readAuditDetail 在请求体传给业务 handler 前读取并解析顶层常见金额字段，
// 随后把 body 恢复原样，保证业务侧照常读取。解析失败/非 JSON 返回 nil（跳过），
// 不报错、不影响业务。数字与数字字符串形式均支持，字符串按 rune 安全截断。
//
// 关键：审计只解析前 auditBodyLimit 字节，但回灌时必须拼回「已读前缀 + 剩余未读流」，
// 否则 >1MiB 的请求体（大 JSON / 附件类接口）会被静默截断，业务侧读到残缺数据。
func readAuditDetail(r *http.Request) any {
	if r.Body == nil {
		return nil
	}
	prefix, err := io.ReadAll(io.LimitReader(r.Body, auditBodyLimit))
	// 无论读取成功与否都先恢复完整流：LimitReader 读满上限后 r.Body 仍指向剩余部分。
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(prefix), r.Body))
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(prefix, &m); err != nil {
		return nil
	}
	detail := map[string]any{}
	for _, key := range auditAmountFields {
		raw, ok := m[key]
		if !ok {
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			continue
		}
		if s, ok := v.(string); ok {
			detail[key] = truncateAuditString(s, auditDetailMaxLen)
		} else {
			detail[key] = v
		}
	}
	if len(detail) == 0 {
		return nil
	}
	return detail
}

// truncateAuditString 按 rune 边界截断字符串，保证结果仍是合法 UTF-8。
func truncateAuditString(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes])
}

// auditTarget 从管理端/开发端路径解析目标类型与目标 ID，如 /api/v1/admin/users/12 → ("user","12")。
func auditTarget(r *http.Request) (string, string) {
	p := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/")
	if p == r.URL.Path {
		p = strings.TrimPrefix(r.URL.Path, "/api/v1/dev/")
	}
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
