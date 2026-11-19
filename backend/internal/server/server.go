package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/team/llmgateway/internal/config"
	"github.com/team/llmgateway/internal/domain/audit"
	"github.com/team/llmgateway/internal/domain/console"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/magpie"
	"github.com/team/llmgateway/internal/domain/magpieaccess"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/domain/subscription"
	"github.com/team/llmgateway/internal/domain/sync"
	"github.com/team/llmgateway/internal/domain/topology"
	"github.com/team/llmgateway/internal/pkg/jwtx"
	"github.com/team/llmgateway/internal/pkg/reqmeta"
	"github.com/team/llmgateway/internal/pkg/resp"
	"github.com/team/llmgateway/internal/pkg/session"
)

// Deps 汇聚全量子处理器，由 main 装配后注入。
//
// V2 第一期起渠道/渠道协议/Auto 模型/网关转发链整体迁出，对应处理器字段一并移除；
// 被摘接口在 routes() 中以 410 显式停用（见 goneHandler / gonePublicHandler），不使用 404。
type Deps struct {
	Identity     *identity.Handler
	AdminUser    *identity.AdminUserHandler
	Credit       *identity.CreditHandler
	TokenAdmin   *identity.TokenHandler
	TokenDev     *identity.TokenHandler
	Model        *model.Handler
	Announcement *identity.AnnouncementHandler
	Console      *console.Admin
	DevConsole   *console.Dev
	// MagpieKey 是代理侧 callerKey 映射的管理端接口（nil = 不挂载）。
	MagpieKey *magpie.KeyAdminHandler
	// MagpieAccess 是代理侧管理面「一键跳转」的票据签发/校验（nil = 只保留校验端点，返回 403）。
	MagpieAccess *magpieaccess.Handler
	SyncAdmin    *sync.Handler
	// Topology 是上游拓扑（T1 查询 / T3 手动同步）；nil = 不挂载。
	Topology *topology.Handler
	// Subscriptions 是订阅录入（S2）与手工重算（S6）；nil = 不挂载。
	Subscriptions *subscription.Handler
	// V2 逐请求转发链（Facade，/v1 与 /v1beta 由灵枢代理）已随 V3 下线：
	// /v1 由 nginx 直连代理侧网关端口，本服务对 /v1 恒 410（见 routes()）。
}

// Server 汇聚 HTTP 服务所需依赖。
type Server struct {
	cfg      *config.Config
	db       *sql.DB
	logger   *slog.Logger
	jwtMgr   *jwtx.Manager
	sessions *session.Registry
	audit    *audit.Store
	deps     Deps
	mux      *http.ServeMux
}

// New 构建 Server：接收全量子处理器（deps）并注册路由。
func New(cfg *config.Config, db *sql.DB, logger *slog.Logger, jwtMgr *jwtx.Manager, sessions *session.Registry, auditStore *audit.Store, deps Deps) *Server {
	s := &Server{
		cfg:      cfg,
		db:       db,
		logger:   logger,
		jwtMgr:   jwtMgr,
		sessions: sessions,
		audit:    auditStore,
		deps:     deps,
		mux:      http.NewServeMux(),
	}
	s.routes()
	return s
}

// routes 注册全部路由。
//
//   - POST /api/v1/auth/login      登录（无需鉴权）
//   - /api/v1/admin/               管理端（需 ADMIN）
//   - /api/v1/dev/                 开发端（需 DEVELOPER）
//   - /v1/                         网关（若注入）
func (s *Server) routes() {
	d := s.deps
	s.mux.HandleFunc("POST /api/v1/auth/login", d.Identity.HandleLogin)
	s.mux.HandleFunc("POST /api/v1/auth/login/totp", d.Identity.HandleLoginTOTP)
	s.mux.HandleFunc("GET /api/v1/auth/captcha", d.Identity.HandleCaptcha)
	s.mux.HandleFunc("POST /api/v1/auth/register", d.Identity.HandleRegister)
	s.mux.Handle("GET /api/v1/auth/me", WithAuth(s.jwtMgr, s.sessions)(http.HandlerFunc(d.Identity.HandleMe)))
	s.mux.Handle("GET /api/v1/auth/me/summary", WithAuth(s.jwtMgr, s.sessions)(http.HandlerFunc(d.DevConsole.HandleMeSummary)))
	s.mux.Handle("PUT /api/v1/auth/me", WithAuth(s.jwtMgr, s.sessions)(http.HandlerFunc(d.Identity.HandleUpdateMe)))
	s.mux.Handle("PUT /api/v1/auth/me/password", WithAuth(s.jwtMgr, s.sessions)(http.HandlerFunc(d.Identity.HandleChangePassword)))
	s.mux.Handle("POST /api/v1/auth/logout", WithAuth(s.jwtMgr, s.sessions)(http.HandlerFunc(d.Identity.HandleLogout)))
	s.mux.Handle("POST /api/v1/auth/me/totp/setup", WithAuth(s.jwtMgr, s.sessions)(http.HandlerFunc(d.Identity.HandleTOTPSetup)))
	s.mux.Handle("POST /api/v1/auth/me/totp/confirm", WithAuth(s.jwtMgr, s.sessions)(http.HandlerFunc(d.Identity.HandleTOTPConfirm)))
	s.mux.Handle("DELETE /api/v1/auth/me/totp", WithAuth(s.jwtMgr, s.sessions)(http.HandlerFunc(d.Identity.HandleTOTPDisable)))

	// ---- 管理端 ----
	admin := http.NewServeMux()
	s.mux.Handle("/api/v1/admin/", WithAuth(s.jwtMgr, s.sessions, identity.RoleAdmin)(WithAudit(s.audit, s.logger)(admin)))

	// users / wallet / credit
	admin.HandleFunc("GET /api/v1/admin/users", d.AdminUser.HandleListUsers)
	admin.HandleFunc("POST /api/v1/admin/users", d.AdminUser.HandleCreateUser)
	admin.HandleFunc("PUT /api/v1/admin/users/{id}", d.AdminUser.HandleUpdateUser)
	admin.HandleFunc("POST /api/v1/admin/users/batch-delete", d.AdminUser.HandleBatchDeleteUsers)
	admin.HandleFunc("POST /api/v1/admin/users/{id}/reset-password", d.AdminUser.HandleResetPassword)
	admin.HandleFunc("POST /api/v1/admin/users/{id}/reset-totp", d.AdminUser.HandleResetTOTP)
	admin.HandleFunc("GET /api/v1/admin/users/{id}", d.AdminUser.HandleGetUser)
	admin.HandleFunc("GET /api/v1/admin/users/{id}/wallet", d.Credit.HandleAdminWallet)
	admin.HandleFunc("POST /api/v1/admin/users/{id}/wallet/recharge", d.Credit.HandleAdminRecharge)
	admin.HandleFunc("POST /api/v1/admin/users/{id}/wallet/set", d.Credit.HandleAdminSet)
	admin.HandleFunc("POST /api/v1/admin/users/{id}/wallet/adjust", d.Credit.HandleAdminAdjust)
	admin.HandleFunc("GET /api/v1/admin/users/{id}/wallet/flows", d.Credit.HandleAdminFlows)

	// tokens
	admin.HandleFunc("GET /api/v1/admin/tokens", d.TokenAdmin.HandleAdminList)
	admin.HandleFunc("POST /api/v1/admin/tokens", d.TokenAdmin.HandleAdminCreate)
	admin.HandleFunc("POST /api/v1/admin/tokens/batch-delete", d.TokenAdmin.HandleAdminBatchDelete)
	admin.Handle("GET /api/v1/admin/tokens/{id}/secret", WithAuditSensitiveRead(s.audit, s.logger)(http.HandlerFunc(d.TokenAdmin.HandleAdminSecret)))

	// ---- 已停用的渠道族接口（V2 第一期：渠道/密钥/模型/探测/协议整体迁出）----
	// 统一 410 + 明确文案：前端页面本期不改，410 比 404 更能说明「接口存在但已停用」。
	for _, path := range []string{
		"GET /api/v1/admin/channels", "POST /api/v1/admin/channels",
		"PUT /api/v1/admin/channels/{id}", "POST /api/v1/admin/channels/batch-delete",
		"GET /api/v1/admin/channels/{id}/model-events",
		"GET /api/v1/admin/channels/{id}/probe-logs",
		"GET /api/v1/admin/channels/cron-preview",
		"PUT /api/v1/admin/channels/{id}/models/{mid}/state",
		"GET /api/v1/admin/channels/snapshot",
		"GET /api/v1/admin/service-status",
	} {
		admin.HandleFunc(path, goneHandler)
	}

	// 渠道协议（channel_protocols·已停用）
	for _, path := range []string{
		"GET /api/v1/admin/channel-protocols", "POST /api/v1/admin/channel-protocols",
		"POST /api/v1/admin/channel-protocols/sync", "GET /api/v1/admin/channel-protocols/export",
		"POST /api/v1/admin/channel-protocols/import", "GET /api/v1/admin/channel-protocols/{id}",
		"GET /api/v1/admin/channel-protocols/{id}/error-stats", "PUT /api/v1/admin/channel-protocols/{id}",
		"DELETE /api/v1/admin/channel-protocols/{id}", "POST /api/v1/admin/channel-protocols/{id}/models/pull",
	} {
		admin.HandleFunc(path, goneHandler)
	}

	// 渠道密钥（channel_keys·已停用）
	for _, path := range []string{
		"GET /api/v1/admin/channels/{id}/keys", "POST /api/v1/admin/channels/{id}/keys",
		"PUT /api/v1/admin/channels/{id}/keys/{kid}", "DELETE /api/v1/admin/channels/{id}/keys/{kid}",
		"POST /api/v1/admin/channels/{id}/keys/{kid}/state",
		"POST /api/v1/admin/channels/{id}/keys/{kid}/probe",
		"GET /api/v1/admin/channels/{id}/keys/{kid}/events",
	} {
		admin.HandleFunc(path, goneHandler)
	}

	// 渠道内部模型（channel_models·已停用）
	for _, path := range []string{
		"GET /api/v1/admin/channel-models",
		"GET /api/v1/admin/channels/{id}/models", "POST /api/v1/admin/channels/{id}/models",
		"POST /api/v1/admin/channels/{id}/models/pull",
		"PUT /api/v1/admin/channels/{id}/models/{mid}",
		"DELETE /api/v1/admin/channels/{id}/models/{mid}",
		"POST /api/v1/admin/channels/{id}/models/{mid}/probe",
	} {
		admin.HandleFunc(path, goneHandler)
	}

	// Auto 模型（ auto_models·已停用）
	for _, path := range []string{
		"GET /api/v1/admin/auto-models", "GET /api/v1/admin/auto-models/{id}",
		"POST /api/v1/admin/auto-models", "PUT /api/v1/admin/auto-models/{id}",
		"DELETE /api/v1/admin/auto-models/{id}",
	} {
		admin.HandleFunc(path, goneHandler)
	}

	// 成本（只读）：成本/售价对照、默认参数、成本曲线、单会话成本明细
	admin.HandleFunc("GET /api/v1/admin/cost/stats", d.Console.HandleCostStats)
	admin.HandleFunc("GET /api/v1/admin/cost/defaults", d.Console.HandleCostDefaults)
	admin.HandleFunc("GET /api/v1/admin/cost/curves", d.Console.HandleCostCurves)
	admin.HandleFunc("GET /api/v1/admin/cost/pricing-curve", d.Console.HandleCostPricingCurve)
	admin.HandleFunc("GET /api/v1/admin/cost/session/{id}", d.Console.HandleCostSession)
	// 会话渠道轨迹（只读，恒可用）。
	admin.HandleFunc("GET /api/v1/admin/cost/session/{id}/trace", d.Console.HandleCostSessionTrace)

	// 标签（semantic_tags / channel_tags·已停用）：0024 已删这两张表，标签路由能力整体下线。
	for _, path := range []string{
		"GET /api/v1/admin/tags", "POST /api/v1/admin/tags",
		"PUT /api/v1/admin/tags/{id}", "POST /api/v1/admin/tags/batch-delete",
	} {
		admin.HandleFunc(path, goneHandler)
	}

	// models（对外模型）
	admin.HandleFunc("GET /api/v1/admin/models", d.Model.HandleListAdmin)
	if d.MagpieKey != nil {
		admin.HandleFunc("GET /api/v1/admin/tokens/{id}/magpie-key", d.MagpieKey.HandleGet)
		admin.HandleFunc("PUT /api/v1/admin/tokens/{id}/magpie-key", d.MagpieKey.HandlePut)
		admin.HandleFunc("POST /api/v1/admin/tokens/{id}/magpie-key/auto", d.MagpieKey.HandleAutoPut)
		admin.HandleFunc("DELETE /api/v1/admin/tokens/{id}/magpie-key", d.MagpieKey.HandleDelete)
	}
	admin.HandleFunc("POST /api/v1/admin/models", d.Model.HandleCreate)
	admin.HandleFunc("PUT /api/v1/admin/models/{id}", d.Model.HandleUpdate)
	admin.HandleFunc("POST /api/v1/admin/models/batch-delete", d.Model.HandleBatchDelete)
	admin.HandleFunc("GET /api/v1/admin/models/{id}/price-reference", d.Model.HandlePriceReference)
	admin.HandleFunc("POST /api/v1/admin/models/{id}/apply-price", d.Model.HandleApplyPrice)
	admin.HandleFunc("POST /api/v1/admin/models/sync-prices", d.Model.HandleSyncPrices)
	admin.HandleFunc("GET /api/v1/admin/models/price-catalog", d.Model.HandlePriceCatalog)

	// 代理侧管理面一键跳转：签发入口票据（前端按钮 → /magpie/?t=…）。
	// 校验端点**不在 /api/ 前缀下**、也不走鉴权中间件：它只给 nginx 的 auth_request 调，
	// 回答的是「这串票据是不是我签的」，不含任何用户数据（见 magpieaccess 包说明）。
	// 无条件注册：未装配时返回 403，而不是让 nginx 拿到 404（那会被当成「后端挂了」）。
	s.mux.HandleFunc("GET /internal/magpie-access/verify", d.MagpieAccess.HandleVerify)
	if d.MagpieAccess != nil {
		admin.HandleFunc("POST /api/v1/admin/ops/magpie-access", d.MagpieAccess.HandleIssue)
	}

	// billings / stats / configs / sessions
	admin.HandleFunc("GET /api/v1/admin/billings", d.Console.HandleListBillings)
	admin.HandleFunc("GET /api/v1/admin/billings/stats", d.Console.HandleBillingsStats)
	// 阶段二 A6：盈亏（收入 vs 成本）与对账窗口（漏账可见）。
	admin.HandleFunc("GET /api/v1/admin/ops/pnl", d.Console.HandlePnL)
	admin.HandleFunc("GET /api/v1/admin/ops/reconcile-windows", d.Console.HandleReconcileWindows)
	// 阶段三 T1/T3：上游拓扑（四层聚合 + 手动同步）。与后台定时同步共用一份实现。
	if d.Topology != nil {
		admin.HandleFunc("GET /api/v1/admin/topology", d.Topology.HandleGet)
		admin.HandleFunc("GET /api/v1/admin/topology/gateway", d.Topology.HandleGateway)
		admin.HandleFunc("PUT /api/v1/admin/internal-models/{ref}/cost-rates", d.Topology.HandlePutCostRates)
		admin.HandleFunc("POST /api/v1/admin/topology/sync", d.Topology.HandleSync)
		// T2：画布落位（只写坐标，不写绑定 —— 绑定真源是 external_models.bound_internal_ref）。
		admin.HandleFunc("PUT /api/v1/admin/topology/links", d.Topology.HandlePutLinks)
	}
	// 阶段三 S2/S6：订阅录入（A5 月费分摊的唯一入口）与手工重算。
	// 成本侧数据只给 ADMIN：整段已由 /api/v1/admin 前缀守卫，不提供 /dev 同义端点。
	if d.Subscriptions != nil {
		admin.HandleFunc("GET /api/v1/admin/subscriptions", d.Subscriptions.HandleList)
		admin.HandleFunc("GET /api/v1/admin/subscriptions/form-options", d.Subscriptions.HandleFormOptions)
		admin.HandleFunc("POST /api/v1/admin/subscriptions", d.Subscriptions.HandleCreate)
		admin.HandleFunc("POST /api/v1/admin/subscriptions/amortize/run", d.Subscriptions.HandleAmortizeRun)
	}
	admin.HandleFunc("GET /api/v1/admin/billings/{billing_id}", d.Console.HandleGetBilling)
	admin.HandleFunc("GET /api/v1/admin/stats/dashboard", d.Console.HandleStatsDashboard)
	admin.HandleFunc("GET /api/v1/admin/stats/live", d.Console.HandleStatsLive)
	admin.HandleFunc("GET /api/v1/admin/configs/system", d.Console.HandleGetSystemConfig)
	admin.HandleFunc("PUT /api/v1/admin/configs/system", d.Console.HandlePutSystemConfig)
	admin.HandleFunc("GET /api/v1/admin/sessions", d.Console.HandleListSessions)
	admin.HandleFunc("GET /api/v1/admin/sessions/{session_id}/calls", d.Console.HandleListSessionCalls)
	admin.HandleFunc("POST /api/v1/admin/sessions/kick", d.Console.HandleKickSessions)
	admin.HandleFunc("PUT /api/v1/admin/sessions/{id}/name", d.Console.HandleRenameSession)

	// announcements
	admin.HandleFunc("GET /api/v1/admin/announcements", d.Announcement.HandleAdminList)
	admin.HandleFunc("POST /api/v1/admin/announcements", d.Announcement.HandleAdminCreate)
	admin.HandleFunc("PUT /api/v1/admin/announcements/{id}", d.Announcement.HandleAdminUpdate)
	admin.HandleFunc("POST /api/v1/admin/announcements/batch-delete", d.Announcement.HandleAdminBatchDelete)

	// watchlist + sync
	admin.HandleFunc("GET /api/v1/admin/watchlist", d.SyncAdmin.HandleListWatchlist)
	admin.HandleFunc("POST /api/v1/admin/watchlist", d.SyncAdmin.HandleUpsertWatchlist)
	admin.HandleFunc("PUT /api/v1/admin/watchlist", d.SyncAdmin.HandleUpsertWatchlist)
	admin.HandleFunc("POST /api/v1/admin/watchlist/batch-delete", d.SyncAdmin.HandleBatchDeleteWatchlist)
	admin.HandleFunc("POST /api/v1/admin/sync/run", d.SyncAdmin.HandleRunSync)
	admin.HandleFunc("GET /api/v1/admin/sync/alerts", d.SyncAdmin.HandleListAlerts)
	admin.HandleFunc("POST /api/v1/admin/sync/alerts/{id}/resolve", d.SyncAdmin.HandleResolveAlert)

	// ---- 开发端 ----
	dev := http.NewServeMux()
	s.mux.Handle("/api/v1/dev/", WithAuth(s.jwtMgr, s.sessions, identity.RoleDeveloper)(WithAudit(s.audit, s.logger)(dev)))

	dev.HandleFunc("GET /api/v1/dev/tokens", d.TokenDev.HandleDevList)
	dev.HandleFunc("POST /api/v1/dev/tokens", d.TokenDev.HandleDevCreate)
	dev.HandleFunc("POST /api/v1/dev/tokens/{id}/rotate", d.TokenDev.HandleDevRotate)
	dev.HandleFunc("POST /api/v1/dev/tokens/{id}/toggle", d.TokenDev.HandleDevToggle)
	dev.Handle("GET /api/v1/dev/tokens/{id}/secret", WithAuditSensitiveRead(s.audit, s.logger)(http.HandlerFunc(d.TokenDev.HandleDevSecret)))
	// 标签（语义标签·已停用；0024 删表）
	dev.HandleFunc("GET /api/v1/dev/tags", gonePublicHandler)
	// 只读“可用模型”：服务端强制仅返回启用项，忽略客户端 ?Enabled= 覆盖。
	dev.HandleFunc("GET /api/v1/dev/models", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		q.Set("Enabled", "true")
		fr := r.Clone(r.Context())
		fr.URL.RawQuery = q.Encode()
		d.Model.HandleList(w, fr)
	})
	dev.HandleFunc("GET /api/v1/dev/stats/dashboard", d.DevConsole.HandleStatsDashboard)
	dev.HandleFunc("GET /api/v1/dev/stats/live", d.DevConsole.HandleStatsLive)
	dev.HandleFunc("GET /api/v1/dev/sessions", d.DevConsole.HandleListSessions)
	dev.HandleFunc("GET /api/v1/dev/sessions/{session_id}/calls", d.DevConsole.HandleListSessionCalls)
	dev.HandleFunc("GET /api/v1/dev/billings/stats", d.DevConsole.HandleBillingsStats)
	dev.HandleFunc("GET /api/v1/dev/billings", d.DevConsole.HandleListBillings)
	dev.HandleFunc("GET /api/v1/dev/billings/{billing_id}", d.DevConsole.HandleGetBilling)
	dev.HandleFunc("GET /api/v1/dev/announcements", d.Announcement.HandleDevList)

	// Auto 模型（dev·已停用）
	for _, path := range []string{
		"GET /api/v1/dev/auto-models", "GET /api/v1/dev/auto-models/{id}",
		"POST /api/v1/dev/auto-models", "PUT /api/v1/dev/auto-models/{id}",
		"DELETE /api/v1/dev/auto-models/{id}",
	} {
		dev.HandleFunc(path, gonePublicHandler)
	}

	// ---- 对外转发入口（/v1 与 /v1beta）----
	// V3 起转发由 nginx 直连代理侧网关端口（不再经过灵枢）；未走 nginx 的直连流量
	// 一律 410 停用，公开文案不暴露内部组件名。挂在**根 mux 且无鉴权**。
	s.mux.HandleFunc("/v1/", gonePublicHandler)
	s.mux.HandleFunc("/v1beta/", gonePublicHandler)

	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// 已停用接口统一 410 Gone。用 410 而不是 404：这些路径确实存在过、且前端本期不改，
// 410 表达「接口存在但已停用」，404 会被误读成路由写错或部署不全。
//
// 两套文案，按**受众**区分，不按路径区分：
//   - goneHandler：管理端（走 WithAuth(RoleAdmin)，受众是内部管理员），可以说明能力去了哪里；
//   - gonePublicHandler：无鉴权的公开入口（/v1、/v1beta、开发端），只给通用文案——
//     对公开 API 消费者不暴露内部组件名与架构变动，也不暗示「换个地址还能用」。
func goneHandler(w http.ResponseWriter, r *http.Request) {
	resp.Err(w, r, http.StatusGone, resp.CodeGone,
		"接口已停用：渠道与转发能力已迁出本服务，请改用代理侧接口（运维文档）")
}

// gonePublicHandler 公开入口的停用响应：通用文案，不带内部组件名、不提迁移去向。
func gonePublicHandler(w http.ResponseWriter, r *http.Request) {
	resp.Err(w, r, http.StatusGone, resp.CodeGone, "该接口已停用，请查阅最新接口文档")
}

// middlewareChain 组装全局中间件链（顺序敏感）。
// WithRequestID 必须在最外层：它把 request id 写入请求 context 后向内层传递，
// 放在内层会导致 WithLogging / WithRecover 读到的 requestId 恒为空。
// WithLoggerContext 紧随其后，把进程 logger 注入 context 供 resp 等底层包落日志。
func (s *Server) middlewareChain(next http.Handler) http.Handler {
	return WithRequestID(
		WithLoggerContext(s.logger)(
			WithRecover(s.logger)(
				WithLogging(s.logger)(
					reqmeta.Middleware(next),
				),
			),
		),
	)
}

// Handler 返回带全局中间件链的最终 handler。
func (s *Server) Handler() http.Handler {
	return s.middlewareChain(s.mux)
}

// Run 启动 HTTP 服务并阻塞，监听 SIGINT/SIGTERM 优雅退出。
func (s *Server) Run(ctx context.Context) error {
	// 注意：严禁设置 WriteTimeout——/v1 网关有 SSE 流式长连接，
	// WriteTimeout 会切断流式响应。故只补 ReadTimeout/IdleTimeout/MaxHeaderBytes。
	httpServer := &http.Server{
		Addr:              s.cfg.Server.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Log(ctx, slog.LevelInfo, "http server listening", "addr", s.cfg.Server.Addr)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		s.logger.Log(ctx, slog.LevelInfo, "shutting down http server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}
