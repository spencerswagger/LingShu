package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
	"github.com/team/llmgateway/internal/domain/automodel"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
	"github.com/team/llmgateway/internal/domain/console"
	"github.com/team/llmgateway/internal/domain/gateway"
	"github.com/team/llmgateway/internal/domain/identity"
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

	// 预设渠道协议首次启动拉取（仅初始化时一次）：地址未设置则不拉取；失败仅日志、不阻塞启动（AC-12）。
	// 须在装载渠道运行时之前完成，保证渠道引用的渠道协议在运行时可见。
	app.cpSvc.InitialSync(ctx)

	// 装载渠道运行态（内存权威状态），并启动健康探测与价格同步。
	if err := app.mgr.SyncFromDB(ctx); err != nil {
		slog.Error("sync channels from db", "error", err)
		os.Exit(1)
	}
	// 会话持久化恢复：从 DB 加载未过期会话重建内存注册表（粘性路由/并发计数），
	// 须在清扫循环启动前完成（ctx 取消前），避免把存活会话误判为过期。
	if err := app.sess.LoadPersisted(ctx); err != nil {
		slog.Error("load persisted sessions", "error", err)
		os.Exit(1)
	}
	app.mgr.Start(ctx)
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
	defer app.mgr.Stop()
	// 优雅退出：排空非资金异步写队列并最终 fsync，完成后释放 writer 上下文。
	// 用独立超时上下文，避免信号 ctx 已取消导致 Shutdown 立即超时（丢队列）。
	defer func() {
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.writer.Shutdown(shCtx); err != nil {
			slog.Error("asyncwrite shutdown", "error", err)
		}
		writerCancel()
	}()

	srv := server.New(cfg, d, logr, jwtMgr, app.sessions, app.auditStore, app.deps)
	if err := srv.Run(ctx); err != nil {
		slog.Error("server run", "error", err)
		os.Exit(1)
	}
}

// app 是装配结果的统称。
type app struct {
	deps       server.Deps
	syncer     *sync.Syncer
	mgr        *channel.Manager
	cpSvc      *channelprotocol.Service
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

	// 非资金异步持久化层（asyncwrite）：统一承接会话投影、渠道状态事件、调用日志的写后异步落库。
	// WALPath 复用账单重试队列所在目录（默认 data/），落 data/asyncwrite.wal；崩溃后由各类别重放函数恢复。
	// AsyncKinds 仅启用四类需落盘的类别；KindTokenLastUsed 故意不启用（内存模式、不落 WAL，仅统计用途，允许丢失）。
	walPath := filepath.Join(filepath.Dir(cfg.Billing.RetryQueuePath), "asyncwrite.wal")
	asyncWriter := asyncwrite.New(asyncwrite.Config{
		WALPath: walPath,
		AsyncKinds: map[asyncwrite.Kind]bool{
			asyncwrite.KindSession:           true,
			asyncwrite.KindChannelKeyEvent:   true,
			asyncwrite.KindChannelModelEvent: true,
			asyncwrite.KindCallLog:           true,
		},
	})

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

	// 定价模式读缓存（键 = userID）：网关每请求解析计费模式，用户计费模式低频变更。
	// 交付顺序（D9）：失效钩子先于缓存启用接线——先注册 SetUserWriteHook，缓存随后才被网关消费，
	// 保证不存在「已读缓存但写路径未接失效」的窗口。
	var pricingCache *pricingModeCache
	if readCacheOn {
		pricingCache = newPricingModeCache()
	}
	// 令牌鉴权读缓存的服务实例在下方装配（须晚于 d）；此处先声明以便失效钩子引用：
	// 用户写路径（含口令重置）经同一钩子「同一代码点」同时失效计费模式缓存与令牌缓存，避免双缓存漂移。
	var tokenSvc *identity.TokenService
	identitySvc.SetUserWriteHook(func(userID int64) {
		if pricingCache != nil {
			pricingCache.Invalidate(userID)
		}
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

	// channel（后端服务创建需 16 字节 SM4 密钥，取自安全配置）
	chStore := channel.NewStore(d)
	cmStore := channel.NewChannelModelStore(d)
	keyStore := channel.NewKeyStore(d)
	chMgr := channel.NewManager(chStore, cmStore, sm4Key, nil, logr, time.Now, keyStore)
	// 渠道状态事件异步化：事件 INSERT 走 asyncwrite（预生成事件 ID 幂等可重放）；
	// 状态列（channel_keys.state / channel_models.state）仍由 Manager 同步落库。
	chMgr.SetEventSink(channel.NewAsyncEventSink(asyncWriter, keyStore, cmStore))
	chSvc := channel.NewService(chStore)
	chSvc.SetManager(chMgr)
	chSvc.SetChannelModelStore(cmStore)
	keySvc := channel.NewKeyService(keyStore, sm4Key)
	chSvc.SetKeyService(keySvc)
	chHandler := channel.NewHandler(chSvc)
	chHandler.SetKeyDeps(keySvc, chMgr)

	// 渠道协议（由外挂预设包物化的可复用接入模板）：首启拉取一次，后续管理员手动同步。
	cpStore := channelprotocol.NewStore(d)
	cpSvc := channelprotocol.NewService(cpStore)
	cpSvc.SetPackURL(cfg.Preset.PackURL)
	cpSvc.SetLogger(logr)
	cpHandler := channelprotocol.NewHandler(cpSvc)
	chMgr.SetChannelProtocolStore(cpStore)
	chSvc.SetChannelProtocolStore(cpStore)
	// 协议变更钩子：先定向重物化引用渠道的 channel_models，再全量重建运行时。
	// 保证协议（含请求头策略）变更后引用渠道的模型与运行时即时生效（无重启、无渠道重存）。
	cpSvc.SetChannelRefresher(func(ctx context.Context, protocolID int64) error {
		if err := chSvc.RematerializeProtocol(ctx, protocolID); err != nil {
			return err
		}
		return chMgr.SyncFromDB(ctx)
	})

	// tag / model
	tagStore := tag.NewStore(d)
	tagSvc := tag.NewService(tagStore)
	tagSvc.SetReadCacheEnabled(readCacheOn)
	chSvc.SetTagResolver(channelTagResolver{svc: tagSvc})
	tagHandler := tag.NewHandler(tagSvc)
	modelSvc := model.NewService(model.NewStore(d))
	modelSvc.SetReadCacheEnabled(readCacheOn)
	modelHandler := model.NewHandler(modelSvc)

	// automodel（Auto 模型：对外模型之上的有序别名组）
	autoStore := automodel.NewStore(d)
	autoSvc := automodel.NewService(autoStore)
	autoSvc.SetReadCacheEnabled(readCacheOn)
	autoHandler := automodel.NewHandler(autoSvc)
	// 开发端接口需从 context 解析当前登录用户（server.UserIDFrom 由中间件写入）。
	autoHandler.SetUserIDFrom(server.UserIDFrom)
	// 跨表重名校验：Auto 名不得与任何 external_models.external_name 重名。
	// 用 GetByName（忽略 enabled）判断「该名是否已被任何对外模型占用」——含未启用者，
	// 避免「先建停用的对外模型 → 再建同名 Auto → 之后重新启用」绕过扁平命名空间约束。
	autoSvc.SetExternalNameChecker(func(ctx context.Context, name string) (bool, error) {
		if _, err := modelSvc.GetByName(ctx, name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	// 反向跨表重名校验：对外模型名不得与任何 Auto 重名（与上方对称，堵住「先建 Auto 后建模型」）。
	modelSvc.SetAutoNameChecker(func(ctx context.Context, name string) (bool, error) {
		if _, err := autoStore.GetByNameAny(ctx, name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})

	// billing
	billStore := billing.NewSqlStore(d)
	billCfgProvider := billing.NewConfigProvider(billStore)
	// 只读配置缓存开关：关闭时 R/context_tiers/时段/样值 均退回直查。
	billCfgProvider.SetCacheEnabled(readCacheOn)
	// 会话过期分档配置 provider：读取 sys_configs 的 session.expiration，供 gateway 按
	// 「上下文长度 + 用户对话次数」双维度计算会话 TTL。
	sessExpiryProvider := router.NewSessionExpiryProvider(billStore)
	// 上游响应头白名单读缓存（单键 sys_configs，键 = gateway.UpstreamResponseHeaderWhitelistKey）：
	// 网关每请求读取，console 低频写入；启用时命中零 DB，关闭时退回直查。
	var whitelistCache *configValueCache
	if readCacheOn {
		whitelistCache = newConfigValueCache(billStore, gateway.UpstreamResponseHeaderWhitelistKey)
	}
	// 交付顺序（D9）：失效钩子必须先于缓存启用接线——此处注册的写钩子会清空全部配置缓存
	//（路由成本样值/全局时段/R/上下文分档 + 会话过期 + 上游白名单），保证不存在
	// 「已读缓存但写路径未接失效」的窗口。sys_configs 写入（console 经 billing.UpsertConfig）后即时生效。
	billStore.SetConfigWriteHook(func() {
		billCfgProvider.InvalidateCostConfig()
		sessExpiryProvider.Invalidate()
		if whitelistCache != nil {
			whitelistCache.Invalidate()
		}
	})
	billSvc := billing.NewService(billStore, billCfgProvider, creditSvc)
	// 账单落库失败时的本地持久重试队列：DB 抖动不丢账单（billing_id 唯一索引保证重投幂等）。
	billSvc.SetRetryQueue(billing.NewRetryQueue(cfg.Billing.RetryQueuePath, logr))
	// 「扣款 + 积分流水 + 账单」同事务提交：消灭「扣了钱没账单」。
	billSvc.SetTxBeginner(d)

	// 探测开销记账（系统身份）：健康探测的真实最小对话 usage 以系统用户/密钥记入 billing_records。
	// 注：ProbeRecorder 首参为 channel_key_id（密钥维度）；记账前先由 keyID 反查所属渠道 ID，
	// 再查 channel_models 成本单价；密钥不在运行时（未装载/已删除）时无渠道映射，直接跳过记账。
	chMgr.SetProbeRecorder(func(ctx context.Context, keyID int64, modelID, target string, ok bool, errMsg string, usage channel.ProbeUsage, durMS int64) error {
		if !ok || usage.TotalTokens == 0 {
			return nil // 失败探测或零开销不入账
		}
		kr, kfound := chMgr.GetKeyRuntime(keyID)
		if !kfound {
			return nil // 无 key→channel 映射，跳过记账
		}
		sysUID, sysTID, err := seeding.SystemIdentity(ctx, d)
		if err != nil {
			return fmt.Errorf("resolve system identity: %w", err)
		}
		cm, err := cmStore.GetByChannelAndInternal(ctx, kr.Key.ChannelID, modelID)
		if err != nil {
			return fmt.Errorf("resolve channel model: %w", err)
		}
		ext, err := modelSvc.GetByID(ctx, cm.ExternalModelID)
		if err != nil {
			return fmt.Errorf("resolve external model: %w", err)
		}
		dur := durMS
		_, err = billSvc.Record(ctx, billing.RecordReq{
			UserID:          sysUID,
			PricingMode:     identity.PricingModeCost,
			TokenID:         &sysTID,
			ExternalModel:   ext.ExternalName,
			InternalModelID: cm.InternalModelID,
			ChannelKeyID:    keyID, // 探测记账落 channel_key_id（ProbeRecorder 首参为密钥 ID）
			Tokens: billing.Usage{
				Input:     int64(usage.PromptTokens),
				Output:    int64(usage.CompletionTokens),
				CacheRead: int64(usage.CachedTokens),
			},
			Rates:      cm.CostRates,
			Remark:     "健康探测",
			DurationMs: &dur,
		})
		return err
	})

	// router（双层模型：对外模型 + 绑定渠道内部模型）
	sessionStore := router.NewSessionStore(d)
	sess := router.NewSessionRegistry(time.Now)
	// 会话投影异步化：Upsert/Delete/SetClosed 走 asyncwrite（Key=session_id 保序、幂等可重放）；
	// LoadActive 保持同步直读，供启动恢复即时重建内存注册表。
	sess.SetStore(router.NewAsyncPersister(asyncWriter, sessionStore))
	// 候选绑定改读 channel.Manager 内存快照（方案 A）：chMgr 实现了 router.channelModelSource，
	// 每请求不再查询 channel_models 配置表；快照经既有 SyncFromDB/Upsert/UpsertModel/DropModel 钩子失效。
	// 对外模型名称解析仍由 modelSvc（GetByExternalName）负责，未在此缓存。
	routerEngine := router.NewEngine(modelSvc, chMgr, chMgr, sess)
	// 新会话创建的成本比较依赖：样值用量（routing.cost_sample）+ 全局时段（billing.time_config）。
	// billing.ConfigProvider 结构上满足 router.CostConfig，直接注入（router 不直接读库）。
	routerEngine.SetCostConfig(billCfgProvider)

	// 密钥删除联动会话清理：路由注册表经 KillByFilter(channel_key_id=keyID) 踢下线该密钥全部存活会话
	//（内存+DB 投影）。装配在 routerEngine 之后（SessionRegistry 已就绪）。
	chHandler.SetSessionKiller(func(keyID int64) int {
		return routerEngine.Sessions().KillByFilter(0, 0, keyID)
	})

	// console
	consoleAdmin := console.NewAdmin(d, billStore, chMgr, userIDFrom)
	consoleAdmin.SetSessions(sess)
	consoleAdmin.SetKeyNameResolver(keyNameResolver(d))
	consoleAdmin.SetSessionNameSetter(sessionStore.SetName)
	consoleDev := console.NewDev(billStore, userIDFrom)
	consoleDev.SetDB(d)
	consoleDev.SetSessions(sess)
	consoleDev.SetTokenCounter(identity.NewTokenStore(d))
	// 调用日志：网关写入 + 控制台只读查询共用同一存储（billing_id 1:1 关联）。
	callLogStore := gateway.NewSQLCallLogStore(d)
	consoleAdmin.SetCallLogStore(callLogStore)
	consoleAdmin.SetRouterEngine(routerEngine)
	consoleAdmin.SetModelSource(modelSvc)
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

	// gateway resolvers
	// 渠道内部模型查询：启用读缓存时改读 channel.Manager 内存快照（零 DB 查询，
	// 由 UpsertModel/DropModel/SyncFromDB 即时维护），否则退回 channelModelStore 直查（安全态）。
	var channelModelsResolver gateway.ChannelModelResolver = cmStore
	if readCacheOn {
		channelModelsResolver = chMgr
	}
	gw := gateway.NewGateway(gateway.GatewayConfig{
		Tokens:        tokenSvc,
		Router:        routerEngine,
		Channels:      chMgr,
		Billing:       billSvc,
		Models:        modelSvc,
		ChannelModels: channelModelsResolver,
		TagKV:         tagKVResolver(tagStore),
		Auto:          autoSvc,
		Pricing:       newPricingModeResolver(userStore, pricingCache),
		// SessionExpiry 会话过期 TTL 分档计算（上下文长度 + 用户对话次数，取较大者，最低兜底 5 分钟）。
		SessionExpiry: sessExpiryProvider,
		// 三阶段计费（预扣/结算退补）：余额不足直接 402，请求后按实际多退少补。
		Credit:          &creditOpsAdapter{svc: creditSvc},
		Balance:         func(ctx context.Context, userID int64) (float64, error) { return creditSvc.GetWallet(ctx, userID) },
		ProviderFactory: gateway.DefaultProviderFactory,
		// 令牌「最后使用时间」：内存模式（KindTokenLastUsed 未列入 AsyncKinds，不落 WAL），
		// 由 asyncwrite 安全模式同步执行；调用方已在独立 goroutine 中触发，不阻塞请求热路径。
		// 该字段仅用于统计展示，进程异常退出或未 flush 时允许丢失，不影响鉴权与计费。
		TouchLastUsed: func(ctx context.Context, tokenID int64) error {
			return asyncWriter.Submit(asyncwrite.Job{
				Kind: asyncwrite.KindTokenLastUsed,
				Key:  strconv.FormatInt(tokenID, 10),
				Exec: func(c context.Context) error {
					return tokenSvc.TouchLastUsed(c, tokenID)
				},
			})
		},
		ListEnabledModels: func(ctx context.Context) ([]model.ExternalModel, error) {
			enabled := true
			return modelSvc.ListModels(ctx, &enabled)
		},
		// GET /v1/models 一并列出全局 Auto + 该令牌用户的私有 Auto（store.List 保证可见性）。
		ListAutoModels: func(ctx context.Context, ownerUserID int64) ([]automodel.AutoModel, error) {
			uid := ownerUserID
			return autoSvc.List(ctx, "", &uid)
		},
		// 全局上游响应头白名单（sys_configs，默认空=不透传任何上游头）。
		// 启用读缓存时走 whitelistCache（命中零 DB）；失效由 SetConfigWriteHook 触发（配置变更即时生效）。
		UpstreamResponseHeaderWhitelist: func() ([]string, error) {
			var raw []byte
			var err error
			if whitelistCache != nil {
				raw, err = whitelistCache.Get(context.Background())
			} else {
				raw, err = billStore.GetConfig(context.Background(), gateway.UpstreamResponseHeaderWhitelistKey)
			}
			if err != nil {
				return nil, err
			}
			var names []string
			if err := json.Unmarshal(raw, &names); err != nil {
				return nil, err
			}
			return names, nil
		},
		Logger: logr,
	})
	// 调用日志采集注入（nil=不采集；生产装配 SQL 存储，与 console 只读共用）。
	// 写侧异步化：Insert 走 asyncwrite（Kind=KindCallLog，Key/幂等键=billing_id，底层 ON CONFLICT 幂等可重放），
	// console 只读查询仍直读同一 SQL 存储。
	gw.SetCallLogs(gateway.NewAsyncCallLogStore(asyncWriter, callLogStore))

	return &app{
		deps: server.Deps{
			Identity:     identityHandler,
			AdminUser:    adminUserHandler,
			Credit:       creditHandler,
			TokenAdmin:   tokenAdminHandler,
			TokenDev:     tokenDevHandler,
			Channel:      chHandler,
			ChannelProto: cpHandler,
			Tag:          tagHandler,
			Model:        modelHandler,
			AutoModel:    autoHandler,
			Announcement: announceHandler,
			Console:      consoleAdmin,
			DevConsole:   consoleDev,
			SyncAdmin:    syncHandler,
			Gateway:      gw.Handler(),
		},
		syncer:     syncer,
		mgr:        chMgr,
		cpSvc:      cpSvc,
		sess:       sess,
		billSvc:    billSvc,
		sessions:   sessions,
		auditStore: auditStore,
		writer:     asyncWriter,
	}, nil
}

// channelTagResolver 把 tag.Service 适配为 channel.TagResolver。
type channelTagResolver struct {
	svc *tag.Service
}

func (a channelTagResolver) ResolveChannelTags(ctx context.Context, ids []int64) ([]channel.TagRef, error) {
	tags, err := a.svc.Resolve(ctx, ids)
	if err != nil {
		return nil, err
	}
	refs := make([]channel.TagRef, 0, len(tags))
	for i := range tags {
		refs = append(refs, channel.TagRef{ID: tags[i].ID, Name: tags[i].Name, KV: tags[i].KVPairs})
	}
	return refs, nil
}

// creditOpsAdapter 把 identity.CreditService 适配为 gateway.CreditOps（三阶段计费预扣/退款）。
type creditOpsAdapter struct {
	svc *identity.CreditService
}

func (a *creditOpsAdapter) Balance(ctx context.Context, userID int64) (float64, error) {
	return a.svc.GetWallet(ctx, userID)
}

func (a *creditOpsAdapter) PreConsume(ctx context.Context, userID int64, amount float64, billingID, sessionID, remark string) error {
	_, _, err := a.svc.Consume(ctx, userID, amount, billingID, sessionID, remark)
	return err
}

func (a *creditOpsAdapter) Settle(ctx context.Context, userID int64, delta float64, sessionID, remark string) error {
	_, _, err := a.svc.SettleBalance(ctx, userID, delta, sessionID, remark)
	return err
}

func (a *creditOpsAdapter) Refund(ctx context.Context, userID int64, amount float64, remark string) error {
	return a.svc.Recharge(ctx, userID, 0, amount, remark)
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
		Input:      p.Pricing.Prompt * rate,
		Output:     p.Pricing.Completion * rate,
		CacheRead:  p.Pricing.CacheRead * rate,
		CacheWrite: p.Pricing.CacheWrite * rate,
		Reasoning:  p.Pricing.Reasoning * rate,
		UpdatedAt:  a.syncer.LastUpdated(),
	}, true
}

func (a priceRefAdapter) SearchPrices(q string) []model.CatalogEntry {
	entries := a.syncer.SearchPrices(q)
	out := make([]model.CatalogEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, model.CatalogEntry{
			Provider:      e.Provider,
			ModelID:       e.ModelID,
			InputUSD:      e.InputUSD,
			OutputUSD:     e.OutputUSD,
			CacheUSD:      e.CacheUSD,
			CacheWriteUSD: e.CacheWriteUSD,
			ReasoningUSD:  e.ReasoningUSD,
		})
	}
	return out
}

func (a priceRefAdapter) LastUpdated() time.Time {
	return a.syncer.LastUpdated()
}

// tagKVResolver 由令牌 tag_id 解析其 KV 标签集合；tagID 为 nil 或标签不存在/停用时返回 nil。
func tagKVResolver(store *tag.Store) gateway.TagKVResolver {
	return func(ctx context.Context, tagID *int64) (map[string]string, error) {
		if tagID == nil {
			return nil, nil
		}
		t, err := store.GetByID(ctx, *tagID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			return nil, err
		}
		if !t.Enabled {
			return nil, nil
		}
		return t.KVPairs, nil
	}
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
