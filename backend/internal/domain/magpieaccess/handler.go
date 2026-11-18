package magpieaccess

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// DefaultTTL 是票据/门票的有效期：一个工作日。过期后管理员回灵枢再点一次按钮即可。
const DefaultTTL = 8 * time.Hour

// Options 是装配参数。
type Options struct {
	// Signer 为 nil 时不挂载签发接口（没配密钥就不发票据）。
	Signer *Signer
	// Enabled 表示代理侧已配置（MAGPIE_BASE_URL 非空）。false 时签发接口返回 503：
	// 给一个点开必然 502 的入口等于制造死链。
	Enabled func() bool
	// PublicBase 是可选的对外前缀（如 https://lingshu.example.com）；空 = 相对路径
	// `/magpie/`（同源，前端直接 window.open）。相对路径是默认：免去猜 Host。
	PublicBase string
	TTL        time.Duration

	UserID func(context.Context) (int64, bool)
	Role   func(context.Context) (string, bool)
	Logger *slog.Logger

	// EntryPath 是管理面在灵枢同源下的路径，默认 "/magpie/"。
	EntryPath string
}

// Handler 挂两个端点：
//
//	POST /api/v1/admin/ops/magpie-access   （WithAuth + RoleAdmin）签发票据并种 cookie
//	GET  /internal/magpie-access/verify    （**无鉴权**，只给 nginx 的 auth_request）
type Handler struct {
	signer     *Signer
	enabled    func() bool
	publicBase string
	ttl        time.Duration
	userID     func(context.Context) (int64, bool)
	role       func(context.Context) (string, bool)
	logger     *slog.Logger
	entryPath  string
}

// NewHandler 创建处理器；Signer 为 nil 返回 nil（调用方据此不注册签发路由）。
func NewHandler(o Options) *Handler {
	if o.Signer == nil {
		return nil
	}
	h := &Handler{
		signer:     o.Signer,
		enabled:    o.Enabled,
		publicBase: strings.TrimRight(strings.TrimSpace(o.PublicBase), "/"),
		ttl:        o.TTL,
		userID:     o.UserID,
		role:       o.Role,
		logger:     o.Logger,
		entryPath:  o.EntryPath,
	}
	if h.ttl <= 0 {
		h.ttl = DefaultTTL
	}
	if h.entryPath == "" {
		h.entryPath = "/magpie/"
	}
	return h
}

// accessView 是签发接口的响应。字段名沿用仓库既有的 PascalCase 约定。
type accessView struct {
	Url       string `json:"Url"`
	ExpiresAt string `json:"ExpiresAt"`
}

// HandleIssue 签发票据、种 cookie，并回一个同源路径给前端 window.open。
//
// 路由层已由 WithAuth(RoleAdmin) 守卫，这里再按 ctx 里的角色判一次：路由守卫是「外面」
// 那层，处理器自己也该能独立成立（换一处挂载不该漏）。
func (h *Handler) HandleIssue(w http.ResponseWriter, r *http.Request) {
	if h == nil {
		http.Error(w, "not available", http.StatusServiceUnavailable)
		return
	}
	var role string
	if h.role != nil {
		role, _ = h.role(r.Context())
	}
	if role != identity.RoleAdmin {
		resp.Err(w, r, http.StatusForbidden, resp.CodeForbidden, "仅管理员可访问代理侧管理面")
		return
	}
	if h.enabled != nil && !h.enabled() {
		resp.ErrReason(w, r, http.StatusServiceUnavailable, resp.CodeInternalError,
			"magpie_admin_unconfigured", "代理侧未接入（magpie.base_url 未配置），管理面不可用", nil)
		return
	}
	var uid int64
	if h.userID != nil {
		uid, _ = h.userID(r.Context())
	}
	tok, exp, err := h.signer.Issue(uid, role, h.ttl)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "签发管理面票据失败")
		return
	}
	// 票据只以 HttpOnly cookie 形式存在：不进 URL、不给 JS 读、不落任何前端存储。
	// SameSite=Lax：管理面是顶层导航打开的，Lax 足够，且挡掉跨站携带。
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    tok,
		Path:     CookiePath,
		MaxAge:   int(h.ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	if h.logger != nil {
		// 只记「谁、多长」，不记票据本身（票据等价于一把管理面钥匙）。
		h.logger.Info("签发代理侧管理面票据", "user_id", uid, "ttl_seconds", int(h.ttl.Seconds()))
	}
	resp.OK(w, r, accessView{Url: h.publicBase + h.entryPath, ExpiresAt: exp.UTC().Format(time.RFC3339)})
}

// HandleVerify 供 nginx 的 auth_request 使用：验过 → 204，否则 403。
//
// 票据来源二选一（都是 nginx 转发过来的）：`magpie_gate` cookie（正常路径）、`?t=` 查询
// 参数（排障/自检用；nginx 不会自己拼）。
func (h *Handler) HandleVerify(w http.ResponseWriter, r *http.Request) {
	if h == nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	tok := strings.TrimSpace(r.URL.Query().Get("t"))
	if tok == "" {
		if c, err := r.Cookie(CookieName); err == nil {
			tok = strings.TrimSpace(c.Value)
		}
	}
	if tok == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if _, err := h.signer.Verify(tok); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
