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
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/console"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/domain/sync"
	"github.com/team/llmgateway/internal/domain/tag"
	"github.com/team/llmgateway/internal/pkg/jwtx"
	"github.com/team/llmgateway/internal/pkg/reqmeta"
	"github.com/team/llmgateway/internal/pkg/session"
)

// Deps 汇聚全量子处理器与可选网关 handler，由 main 装配后注入。
type Deps struct {
	Identity     *identity.Handler
	AdminUser    *identity.AdminUserHandler
	Credit       *identity.CreditHandler
	TokenAdmin   *identity.TokenHandler
	TokenDev     *identity.TokenHandler
	Channel      *channel.Handler
	Tag          *tag.Handler
	Model        *model.Handler
	Announcement *identity.AnnouncementHandler
	Console      *console.Admin
	DevConsole   *console.Dev
	SyncAdmin    *sync.Handler
	Gateway      http.Handler // 可选：挂载到 /v1/
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
	s.mux.Handle("GET /api/v1/auth/me", WithAuth(s.jwtMgr, s.sessions)(http.HandlerFunc(d.Identity.HandleMe)))
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

	// channels
	admin.HandleFunc("GET /api/v1/admin/channels", d.Channel.HandleList)
	admin.HandleFunc("POST /api/v1/admin/channels", d.Channel.HandleCreate)
	admin.HandleFunc("PUT /api/v1/admin/channels/{id}", d.Channel.HandleUpdate)
	admin.HandleFunc("POST /api/v1/admin/channels/batch-delete", d.Channel.HandleBatchDelete)
	admin.HandleFunc("GET /api/v1/admin/channels/{id}/events", d.Channel.HandleListEvents)
	admin.HandleFunc("GET /api/v1/admin/channels/{id}/model-events", d.Channel.HandleListModelEvents)
	admin.HandleFunc("GET /api/v1/admin/channels/{id}/probe-logs", d.Channel.HandleListProbeLogs)
	admin.HandleFunc("POST /api/v1/admin/channels/{id}/state", d.Channel.HandleState)
	admin.HandleFunc("GET /api/v1/admin/channels/cron-preview", d.Channel.HandleCronPreview)
	admin.HandleFunc("PUT /api/v1/admin/channels/{id}/models/{mid}/state", d.Channel.HandleModelState)
	admin.HandleFunc("GET /api/v1/admin/channels/snapshot", d.Console.HandleChannelSnapshot)

	// 渠道密钥（channel_keys 运行时实体）
	admin.HandleFunc("GET /api/v1/admin/channels/{id}/keys", d.Channel.HandleListKeys)
	admin.HandleFunc("POST /api/v1/admin/channels/{id}/keys", d.Channel.HandleCreateKey)
	admin.HandleFunc("PUT /api/v1/admin/channels/{id}/keys/{kid}", d.Channel.HandleUpdateKey)
	admin.HandleFunc("DELETE /api/v1/admin/channels/{id}/keys/{kid}", d.Channel.HandleDeleteKey)
	admin.HandleFunc("POST /api/v1/admin/channels/{id}/keys/{kid}/state", d.Channel.HandleKeyState)
	admin.HandleFunc("POST /api/v1/admin/channels/{id}/keys/{kid}/probe", d.Channel.HandleProbeKey)
	admin.HandleFunc("GET /api/v1/admin/channels/{id}/keys/{kid}/events", d.Channel.HandleListKeyEvents)

	// 渠道内部模型（双层模型）
	admin.HandleFunc("GET /api/v1/admin/channel-models", d.Channel.HandleListAllChannelModels)
	admin.HandleFunc("GET /api/v1/admin/channels/{id}/models", d.Channel.HandleListChannelModels)
	admin.HandleFunc("POST /api/v1/admin/channels/{id}/models", d.Channel.HandleCreateChannelModel)
	admin.HandleFunc("POST /api/v1/admin/channels/{id}/models/pull", d.Channel.HandlePullModels)
	admin.HandleFunc("PUT /api/v1/admin/channels/{id}/models/{mid}", d.Channel.HandleUpdateChannelModel)
	admin.HandleFunc("DELETE /api/v1/admin/channels/{id}/models/{mid}", d.Channel.HandleDeleteChannelModel)
	admin.HandleFunc("POST /api/v1/admin/channels/{id}/models/{mid}/probe", d.Channel.HandleProbeModel)

	// tags
	admin.HandleFunc("GET /api/v1/admin/tags", d.Tag.HandleList)
	admin.HandleFunc("POST /api/v1/admin/tags", d.Tag.HandleCreate)
	admin.HandleFunc("PUT /api/v1/admin/tags/{id}", d.Tag.HandleUpdate)
	admin.HandleFunc("POST /api/v1/admin/tags/batch-delete", d.Tag.HandleBatchDelete)

	// models（对外模型）
	admin.HandleFunc("GET /api/v1/admin/models", d.Model.HandleList)
	admin.HandleFunc("POST /api/v1/admin/models", d.Model.HandleCreate)
	admin.HandleFunc("PUT /api/v1/admin/models/{id}", d.Model.HandleUpdate)
	admin.HandleFunc("POST /api/v1/admin/models/batch-delete", d.Model.HandleBatchDelete)
	admin.HandleFunc("GET /api/v1/admin/models/{id}/price-reference", d.Model.HandlePriceReference)
	admin.HandleFunc("POST /api/v1/admin/models/{id}/apply-price", d.Model.HandleApplyPrice)
	admin.HandleFunc("POST /api/v1/admin/models/sync-prices", d.Model.HandleSyncPrices)
	admin.HandleFunc("GET /api/v1/admin/models/price-catalog", d.Model.HandlePriceCatalog)

	// billings / stats / configs / sessions
	admin.HandleFunc("GET /api/v1/admin/billings", d.Console.HandleListBillings)
	admin.HandleFunc("GET /api/v1/admin/billings/stats", d.Console.HandleBillingsStats)
	admin.HandleFunc("GET /api/v1/admin/billings/{billing_id}", d.Console.HandleGetBilling)
	admin.HandleFunc("GET /api/v1/admin/stats/dashboard", d.Console.HandleStatsDashboard)
	admin.HandleFunc("GET /api/v1/admin/configs/billing", d.Console.HandleGetBillingConfig)
	admin.HandleFunc("PUT /api/v1/admin/configs/billing", d.Console.HandlePutBillingConfig)
	admin.HandleFunc("GET /api/v1/admin/sessions", d.Console.HandleListSessions)
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
	dev.HandleFunc("GET /api/v1/dev/tags", d.Tag.HandleList) // 列表过滤由 query enabled=true 控制
	dev.HandleFunc("GET /api/v1/dev/wallet", d.Credit.HandleDevWallet)
	dev.HandleFunc("GET /api/v1/dev/wallet/flows", d.Credit.HandleDevFlows)
	dev.HandleFunc("GET /api/v1/dev/stats/dashboard", d.DevConsole.HandleStatsDashboard)
	dev.HandleFunc("GET /api/v1/dev/usage", d.DevConsole.HandleUsage)
	dev.HandleFunc("GET /api/v1/dev/billings", d.DevConsole.HandleListBillings)
	dev.HandleFunc("GET /api/v1/dev/billings/{billing_id}", d.DevConsole.HandleGetBilling)
	dev.HandleFunc("GET /api/v1/dev/announcements", d.Announcement.HandleDevList)

	// ---- 网关 ----
	if d.Gateway != nil {
		s.mux.Handle("/v1/", d.Gateway)
	}

	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// middlewareChain 组装全局中间件链（顺序敏感）。
// WithRequestID 必须在最外层：它把 request id 写入请求 context 后向内层传递，
// 放在内层会导致 WithLogging / WithRecover 读到的 requestId 恒为空。
func (s *Server) middlewareChain(next http.Handler) http.Handler {
	return WithRequestID(
		WithRecover(s.logger)(
			WithLogging(s.logger)(
				reqmeta.Middleware(next),
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
