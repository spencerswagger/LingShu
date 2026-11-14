package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/team/llmgateway/internal/config"
	"github.com/team/llmgateway/internal/db"
	"github.com/team/llmgateway/internal/domain/audit"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/calllog"
	"github.com/team/llmgateway/internal/domain/console"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/magpie"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/domain/sync"
	"github.com/team/llmgateway/internal/domain/tag"
	"github.com/team/llmgateway/internal/pkg/asyncwrite"
	"github.com/team/llmgateway/internal/pkg/clientip"
	"github.com/team/llmgateway/internal/pkg/idgen"
	"github.com/team/llmgateway/internal/pkg/jwtx"
	"github.com/team/llmgateway/internal/pkg/logger"
	"github.com/team/llmgateway/internal/pkg/ratelimit"
	"github.com/team/llmgateway/internal/pkg/session"
	"github.com/team/llmgateway/internal/seeding"
	"github.com/team/llmgateway/internal/server"
)

// init 统一进程时区为北京时间（Asia/Shanghai）：按日/按小时切分的统计、
// 时段计费系数与按日补零展示都以北京时间为准。
func init() {
	if err := os.Setenv("TZ", "Asia/Shanghai"); err == nil {
		if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
			time.Local = loc
		}
	}
}

// sm4KeyFromConfig 读取渠道凭据加密密钥（SM4 要求 16 字节，即 32 个 hex 字符）。
// 优先级：
//  1. 配置了 security.sm4_key：校验 32 位 hex（非法直接退出），与 SM4_KEY 环境变量
//     （docker-entrypoint 写入 config）路径保持兼容；
//  2. 未配置：从 keys/sm4.key 读取，文件不存在则生成 16 字节强随机密钥写入
//     （chmod 600，目录缺失先创建）并打印日志；之后重启复用该文件。
func sm4KeyFromConfig(cfg *config.Config) []byte {
	k := strings.TrimSpace(cfg.Security.SM4Key)
	if k != "" {
		b, err := hex.DecodeString(k)
		if err != nil || len(b) != 16 {
			slog.Error("security.sm4_key 非法：必须为 32 位 hex（16 字节）密钥。请参照 config.example.yaml 设置；留空则自动从 keys/sm4.key 生成/复用")
			os.Exit(1)
		}
		return b
	}

	const keyPath = "keys/sm4.key"
	if b, err := os.ReadFile(keyPath); err == nil {
		kb, derr := hex.DecodeString(strings.TrimSpace(string(b)))
		if derr != nil || len(kb) != 16 {
			slog.Error("keys/sm4.key 内容非法：必须为 32 位 hex（16 字节）。请修正或删除该文件后重启以重新生成")
			os.Exit(1)
		}
		return kb
	} else if !errors.Is(err, os.ErrNotExist) {
		slog.Error("读取 keys/sm4.key 失败", "error", err)
		os.Exit(1)
	}

	kb := make([]byte, 16)
	if _, err := rand.Read(kb); err != nil {
		slog.Error("生成 SM4 密钥失败", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll("keys", 0o755); err != nil {
		slog.Error("创建 keys 目录失败", "error", err)
		os.Exit(1)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(kb)), 0o600); err != nil {
		slog.Error("写入 keys/sm4.key 失败", "error", err)
		os.Exit(1)
	}
	slog.Info("未配置 security.sm4_key，已自动生成强随机密钥并写入 keys/sm4.key（后续重启复用）")
	return kb
}

// userIDFrom 从请求上下文读取当前登录用户（server.UserIDFrom 适配）。
var userIDFrom = func(ctx context.Context) (int64, bool) {
	return server.UserIDFrom(ctx)
}

func main() {
	cfgPath := flag.String("config", "config.yaml", "config file path")
	migrateOnly := flag.Bool("migrate-only", false, "run migrations and exit")
	flag.Parse()

	// 雪花 ID 工作节点：多实例部署须为每个实例分配唯一 worker（0~31），否则同毫秒会撞主键。
	// 未设置时保留默认 0（单实例场景）；越界或非法值 fail-fast 退出。
	if v := strings.TrimSpace(os.Getenv("IDGEN_WORKER")); v != "" {
		w, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			slog.Error("IDGEN_WORKER 须为整数", "value", v, "error", err)
			os.Exit(1)
		}
		if err := idgen.SetWorker(w); err != nil {
			slog.Error("IDGEN_WORKER 越界", "value", v, "error", err)
			os.Exit(1)
		}
		slog.Info("idgen worker 已按 IDGEN_WORKER 设置", "worker", w)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	d, err := db.Open(cfg.Database.DSN)
	if err != nil {
		slog.Error("open db", "error", err)
		os.Exit(1)
	}
	defer d.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if _, err := db.Migrate(ctx, d); err != nil {
		slog.Error("migrate", "error", err)
		os.Exit(1)
	}

	if *migrateOnly {
		slog.Info("migrations applied, exiting")
		return
	}

	if err := seeding.Seed(ctx, d); err != nil {
		slog.Error("seed", "error", err)
		os.Exit(1)
	}

	jwtMgr, err := jwtx.NewManager(cfg.JWT.PrivateKeyPath, cfg.JWT.PublicKeyPath, cfg.JWT.TTLMinutes)
	if err != nil {
		slog.Error("init jwt manager", "error", err)
		os.Exit(1)
	}
	logr := logger.NewDefault()
	// 打印生效的可信代理网段：信任边界失配是静默失败（审计 IP 退化为容器 IP），
	// 显式打出便于部署时一眼发现。
	clientip.LogTrustedNets()

	// 装配全部领域服务、handler、网关与价格同步器。
	app, err := buildApp(d, cfg, jwtMgr, logr)
	if err != nil {
		slog.Error("build app", "error", err)
		os.Exit(1)
	}

	// 会话持久化恢复：从 DB 加载未过期会话重建内存注册表（粘性路由/并发计数），
	// 须在清扫循环启动前完成（ctx 取消前），避免把存活会话误判为过期。
	if err := app.sess.LoadPersisted(ctx); err != nil {
		slog.Error("load persisted sessions", "error", err)
		os.Exit(1)
	}
	// 非资金异步持久化层启动：先重放上次崩溃遗留的 WAL，再开始接收新的异步写。
	// 使用独立于信号 ctx 的上下文：退出时信号 ctx 已取消，若以之作 Exec 上下文会导致排空阶段
	// 落库全部失败；其生命周期由下方 Shutdown（关闭队列 + stopCh）终结，排空完成后才 cancel。
	writerCtx, writerCancel := context.WithCancel(context.Background())
	app.writer.Start(writerCtx)
	go app.sess.Run(ctx, time.Minute) // 会话清扫：清理过期会话并回算各渠道存活数
	interval := time.Duration(cfg.Sync.IntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 60 * time.Minute
	}
	go app.syncer.Run(ctx)
	// 账单重试队列：启动即补投上次退出前积压的记录，并按配置周期重投（落库失败不丢账单）。
	if n := app.billSvc.PendingRetry(); n > 0 {
		slog.Info("billing retry queue has pending records", "pending", n)
	}
	go app.billSvc.RunRetryQueue(ctx, time.Duration(cfg.Billing.RetryIntervalSeconds)*time.Second)
	// 审计写入失败汇总：审计失效必须早于业务失效被发现，把进程内 expvar 计数
	// 变成日志侧可告警信号（本服务不暴露 /debug/vars）。
	go audit.ReportInsertFailures(ctx, logr, time.Minute)
	// 优雅退出：排空非资金异步写队列并最终 fsync，完成后释放 writer 上下文。
	// 用独立超时上下文，避免信号 ctx 已取消导致 Shutdown 立即超时（丢队列）。
	// 封装为闭包，便于在 os.Exit 路径（会跳过 defer）显式调用；Shutdown/取消均幂等，可重复调用。
	shutdownWriter := func() {
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.writer.Shutdown(shCtx); err != nil {
			slog.Error("asyncwrite shutdown", "error", err)
		}
		writerCancel()
	}
	defer shutdownWriter()

	srv := server.New(cfg, d, logr, jwtMgr, app.sessions, app.auditStore, app.deps)
	if err := srv.Run(ctx); err != nil {
		slog.Error("server run", "error", err)
		// os.Exit 会跳过所有 defer：此处显式先排空并关闭 async writer，避免退出时丢队列（P2-9）。
		shutdownWriter()
		os.Exit(1)
	}
}

// app 是装配结果的统称。
type app struct {
	deps       server.Deps
	syncer     *sync.Syncer
	sess       *router.SessionRegistry
	billSvc    *billing.Service
	sessions   *session.Registry
	auditStore *audit.Store
	// writer 是全局非资金异步持久化层（会话投影/渠道状态事件/调用日志），随进程优雅退出 flush。
	writer *asyncwrite.Writer
}

// buildApp 装配全部服务。
func buildApp(d *sql.DB, cfg *config.Config, jwtMgr *jwtx.Manager, logr *slog.Logger) (*app, error) {
	// 渠道凭据与令牌明文共用 16 字节 SM4 密钥（取自安全配置）
	sm4Key := sm4KeyFromConfig(cfg)

	// 只读配置缓存总开关：默认启用；READ_CACHE_ENABLED / read_cache.enabled=false 时全部退回直查。
	readCacheOn := cfg.ReadCacheEnabled()

	// 非资金异步持久化层（asyncwrite）：承接会话投影的写后异步落库。
	// WALPath 复用账单重试队列所在目录（默认 data/），落 data/asyncwrite.wal；崩溃后由类别重放函数恢复。
	// AsyncKinds 只启用会话投影一类；KindTokenLastUsed 故意不启用（内存模式、不落 WAL，仅统计用途，允许丢失）。
	// 渠道状态事件 / 调用日志 / 会话成本事件三类随转发链停写，不再登记（常量保留，见 asyncwrite/job.go）。
	walPath := filepath.Join(filepath.Dir(cfg.Billing.RetryQueuePath), "asyncwrite.wal")
	asyncWriter := asyncwrite.New(asyncwrite.Config{
		WALPath: walPath,
		AsyncKinds: map[asyncwrite.Kind]bool{
			asyncwrite.KindSession: true,
		},
	})
	// 配置了 WALPath 却打不开（目录不可写/磁盘满）时致命化启动：绝不静默降级为内存模式，
	// 否则会话投影将失去 WAL 兜底、崩溃即丢，违背「绝不静默丢弃」契约。
	if err := asyncWriter.Err(); err != nil {
		return nil, fmt.Errorf("asyncwrite WAL 不可用（拒绝静默降级为内存模式）: %w", err)
	}

	// 安全组件：会话注册表 / 审计 / 限流 / preauth / TOTP
	sessions := session.NewRegistry(func(ctx context.Context, userID int64) (session.Entry, error) {
		var e session.Entry
		err := d.QueryRowContext(ctx,
			`SELECT token_version, status, must_change_password FROM users WHERE id = $1 AND deleted_at IS NULL`,
			userID).Scan(&e.Version, &e.Status, &e.MustChange)
		return e, err
	})
	auditStore := audit.NewStore(d)
	rl := ratelimit.New()
	// 注册接口限流：独立实例，键为 IP 本身，与登录限流隔离，且不引入全局用户名维度锁定。
	regLimiter := ratelimit.New()
	// 验证码生成限流：独立实例，键为 IP 本身，按请求计数节流，避免无节流生成被滥用。
	capLimiter := ratelimit.New()
	preAuth := identity.NewPreAuthStore()
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			rl.Cleanup()
			regLimiter.Cleanup()
			capLimiter.Cleanup()
		}
	}()

	// identity
	userStore := identity.NewStore(d)
	identitySvc := identity.NewService(userStore, jwtMgr)
	identitySvc.SetSessionRegistry(sessions)
	identitySvc.SetAudit(auditStore)
	totpSvc := identity.NewTOTPService(userStore, sm4Key)
	totpSvc.SetAudit(auditStore)
	identitySvc.SetTOTP(totpSvc)
	// 二次验证口令尝试限流：复用登录限流器（key 前缀 "sf:" 与登录计数隔离）。
	identitySvc.SetRateLimiter(rl)

	// 令牌鉴权读缓存的服务实例在下方装配（须晚于 d）；此处先声明以便失效钩子引用：
	// 用户写路径（含口令重置）经该钩子即时失效令牌鉴权缓存，避免鉴权读到旧态。
	var tokenSvc *identity.TokenService
	identitySvc.SetUserWriteHook(func(userID int64) {
		if tokenSvc != nil {
			tokenSvc.InvalidateByUserID(userID)
		}
	})

	creditStore := identity.NewCreditStore(d)
	creditSvc := identity.NewCreditService(creditStore)

	identityHandler := identity.NewHandler(identitySvc, creditSvc, userIDFrom)
	identityHandler.SetRateLimiter(rl)
	identityHandler.SetPreAuth(preAuth)
	// 注册接口：图形验证码 + 独立限流器（键为 IP 本身），与登录限流隔离。
	identityHandler.SetCaptcha(identity.NewImageCaptcha())
	identityHandler.SetRegisterLimiter(regLimiter)
	identityHandler.SetCaptchaLimiter(capLimiter)
	creditHandler := identity.NewCreditHandler(creditSvc, userIDFrom)
	// 充值/覆盖/调整积分属资金敏感操作：需操作者当前口令二次验证。
	creditHandler.SetPasswordVerifier(identitySvc.VerifyCurrentPassword)

	tokenStore := identity.NewTokenStore(d)
	tokenSvc = identity.NewTokenService(tokenStore)
	tokenSvc.SetSecretKey(sm4Key)
	// 令牌鉴权读缓存：与只读配置缓存总开关一致（关闭时退回纯 DB 直查的安全态）。
	// 命中零 DB；即时失效由各写路径失效钩子保证，安全 TTL 仅作最后防线（默认 60s，可配置）。
	tokenSvc.SetTokenCacheEnabled(readCacheOn)
	tokenSvc.SetTokenCacheTTL(cfg.TokenCacheTTL())
	tokenAdminHandler := identity.NewTokenHandler(tokenSvc, userIDFrom)
	tokenDevHandler := identity.NewTokenHandler(tokenSvc, userIDFrom)
	// 查看明文密钥属敏感操作：需当前口令二次验证。
	tokenAdminHandler.SetPasswordVerifier(identitySvc.VerifyCurrentPassword)
	tokenDevHandler.SetPasswordVerifier(identitySvc.VerifyCurrentPassword)

	adminUserHandler := identity.NewAdminUserHandler(identitySvc, creditSvc, userIDFrom)
	adminUserHandler.SetTOTP(totpSvc)

	announceStore := identity.NewAnnouncementStore(d)
	announceSvc := identity.NewAnnouncementService(announceStore)
	announceHandler := identity.NewAnnouncementHandler(announceSvc, userIDFrom)

	// tag / model
	tagStore := tag.NewStore(d)
	tagSvc := tag.NewService(tagStore)
	tagSvc.SetReadCacheEnabled(readCacheOn)
	tagHandler := tag.NewHandler(tagSvc)
	modelSvc := model.NewService(model.NewStore(d))
	modelSvc.SetReadCacheEnabled(readCacheOn)
	modelHandler := model.NewHandler(modelSvc)

	// 反向跨表重名校验：对外模型名不得与任何 Auto 重名。Auto 模型（auto_models 表）已随
	// 转发链停用，表只读保留，此处恒返回「未被占用」。
	modelSvc.SetAutoNameChecker(func(ctx context.Context, name string) (bool, error) {
		return false, nil
	})

	// billing
	billStore := billing.NewSqlStore(d)
	billCfgProvider := billing.NewConfigProvider(billStore)
	// 只读配置缓存开关：关闭时 R/context_tiers/时段/样值 均退回直查。
	billCfgProvider.SetCacheEnabled(readCacheOn)
	// 失效钩子：sys_configs 写入（console 经 billing.UpsertConfig）后清空计费配置读缓存，即时生效。
	// 订阅池前缀名单（magpie.subscription_prefixes）：低频变更，读侧缓存，写入后失效即生效。
	magpiePrefixes := magpie.NewPrefixProvider(billStore.GetConfig)
	billStore.SetConfigWriteHook(func() {
		billCfgProvider.InvalidateCostConfig()
		magpiePrefixes.Invalidate()
	})
	billSvc := billing.NewService(billStore, billCfgProvider, creditSvc)
	// 账单落库失败时的本地持久重试队列：DB 抖动不丢账单（billing_id 唯一索引保证重投幂等）。
	billSvc.SetRetryQueue(billing.NewRetryQueue(cfg.Billing.RetryQueuePath, logr))
	// 「扣款 + 积分流水 + 账单」同事务提交：消灭「扣了钱没账单」。
	billSvc.SetTxBeginner(d)

	// router：仅保留会话注册表（历史会话只读展示 + 管理端改名/踢下线）。
	// 路由决策/候选选择/成本择优已随转发链迁出，代理侧负责。
	sessionStore := router.NewSessionStore(d)
	sess := router.NewSessionRegistry(time.Now)
	// 会话投影异步化：Upsert/Delete/SetClosed 走 asyncwrite（Key=session_id 保序、幂等可重放）；
	// LoadActive 保持同步直读，供启动恢复即时重建内存注册表。
	sess.SetStore(router.NewAsyncPersister(asyncWriter, sessionStore))

	// console
	consoleAdmin := console.NewAdmin(d, billStore, userIDFrom)
	consoleAdmin.SetSessions(sess)
	consoleAdmin.SetKeyNameResolver(keyNameResolver(d))
	consoleAdmin.SetSessionNameSetter(sessionStore.SetName)
	consoleDev := console.NewDev(billStore, userIDFrom)
	consoleDev.SetDB(d)
	consoleDev.SetSessions(sess)
	consoleDev.SetTokenCounter(identity.NewTokenStore(d))
	// 调用日志：只保留历史数据只读查询（call_logs 已停写，表与数据保留）。
	callLogStore := calllog.NewSQLStore(d)
	consoleAdmin.SetCallLogStore(callLogStore)
	consoleAdmin.SetModelSource(modelSvc)
	// 成本曲线积分换算系数 R 来源（billing.ConfigProvider 满足，含 R() 方法）：曲线 Σ(token×费率) ÷ R 得积分。
	consoleAdmin.SetCostRProvider(billCfgProvider)
	consoleDev.SetCallLogStore(callLogStore)

	// sync
	syncStore := sync.NewStore(d)
	syncer := sync.NewSyncer(sync.SyncerConfig{
		Store:     syncStore,
		SourceURL: cfg.Sync.PriceSourceURL,
		Interval:  time.Duration(cfg.Sync.IntervalMinutes) * time.Minute,
		Logger:    logr,
	})
	syncHandler := sync.NewHandler(syncer, userIDFrom)

	// 供对外模型「查看 models.dev 参考价 / 一键应用售价」使用；参考价按 billing.cny_rate 折算为人民币倍率。
	modelSvc.SetPriceSource(priceRefAdapter{
		syncer:  syncer,
		cnyRate: billCfgProvider.CnyRate,
	})

	// 对外转发入口（/v1 与 /v1beta）：灵枢只做 facade，转发与协议互译在代理侧。
	// 未配置 magpie.base_url 时 NewHandler 返回 nil，server 侧保持 410 停用口径。
	// OnComplete 刻意不注入：扣费/退预扣属第③件，本件只产出 Result 事实。
	magpieHandler := magpie.NewHandler(magpie.Deps{
		Config:        magpie.FromApp(cfg),
		Tokens:        tokenSvc,
		Models:        modelSvc,
		Keys:          magpie.NewKeyStore(d),
		CredentialKey: sm4Key,
		Prefixes:      magpiePrefixes,
		Logger:        logr,
	})
	if magpieHandler == nil {
		logr.Info("未配置 magpie.base_url：对外转发接口保持停用（410）")
	}

	return &app{
		deps: server.Deps{
			Identity:     identityHandler,
			AdminUser:    adminUserHandler,
			Credit:       creditHandler,
			TokenAdmin:   tokenAdminHandler,
			TokenDev:     tokenDevHandler,
			Tag:          tagHandler,
			Model:        modelHandler,
			Announcement: announceHandler,
			Console:      consoleAdmin,
			DevConsole:   consoleDev,
			SyncAdmin:    syncHandler,
			Facade:       magpieHandler,
		},
		syncer:     syncer,
		sess:       sess,
		billSvc:    billSvc,
		sessions:   sessions,
		auditStore: auditStore,
		writer:     asyncWriter,
	}, nil
}

// priceRefAdapter 把 sync.Syncer 适配为 model.PriceSource，供对外模型价格参考/应用售价接口使用。
// cnyRate 为 billing.cny_rate：USD/CNY 汇率（1 USD = cny_rate CNY）；models.dev 美元价 × cny_rate = 人民币倍率。
// 汇率缺失/异常时视为未命中（返回 false），避免把 USD 数值当作人民币倍率写库。
type priceRefAdapter struct {
	syncer  *sync.Syncer
	cnyRate func() (float64, error)
}

func (a priceRefAdapter) ReferencePrice(externalName string) (model.PriceRef, bool) {
	p, ok := a.syncer.Recommended(externalName)
	if !ok {
		return model.PriceRef{}, false
	}
	rate := 0.0
	if a.cnyRate != nil {
		if v, err := a.cnyRate(); err == nil && v > 0 {
			rate = v
		}
	}
	if rate <= 0 {
		return model.PriceRef{}, false // 汇率配置缺失，不能把 USD 当 CNY 写库
	}
	return model.PriceRef{
		Input:     p.Pricing.Prompt * rate,
		Output:    p.Pricing.Completion * rate,
		CacheRead: p.Pricing.CacheRead * rate,
		UpdatedAt: a.syncer.LastUpdated(),
	}, true
}

func (a priceRefAdapter) SearchPrices(q string) []model.CatalogEntry {
	entries := a.syncer.SearchPrices(q)
	out := make([]model.CatalogEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, model.CatalogEntry{
			Provider:  e.Provider,
			ModelID:   e.ModelID,
			InputUSD:  e.InputUSD,
			OutputUSD: e.OutputUSD,
			CacheUSD:  e.CacheUSD,
		})
	}
	return out
}

func (a priceRefAdapter) LastUpdated() time.Time {
	return a.syncer.LastUpdated()
}

// keyNameResolver 装配渠道密钥名称批量反查钩子（console.Admin.SetKeyNameResolver）：
// 会话列表/账单展示的 channel_key_name 由任一 channel_keys 批量查询填充；空列表直接返回。
func keyNameResolver(db *sql.DB) func(ctx context.Context, ids []int64) (map[int64]string, error) {
	return func(ctx context.Context, ids []int64) (map[int64]string, error) {
		if len(ids) == 0 {
			return nil, nil
		}
		rows, err := db.QueryContext(ctx,
			`SELECT id, name FROM channel_keys WHERE id = ANY($1)`, ids)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := make(map[int64]string, len(ids))
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				return nil, err
			}
			out[id] = name
		}
		return out, rows.Err()
	}
}
