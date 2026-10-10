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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/team/llmgateway/internal/config"
	"github.com/team/llmgateway/internal/db"
	"github.com/team/llmgateway/internal/domain/audit"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/console"
	"github.com/team/llmgateway/internal/domain/gateway"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/domain/sync"
	"github.com/team/llmgateway/internal/domain/tag"
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
	sess       *router.SessionRegistry
	billSvc    *billing.Service
	sessions   *session.Registry
	auditStore *audit.Store
}

// buildApp 装配全部服务。
func buildApp(d *sql.DB, cfg *config.Config, jwtMgr *jwtx.Manager, logr *slog.Logger) (*app, error) {
	// 渠道凭据与令牌明文共用 16 字节 SM4 密钥（取自安全配置）
	sm4Key := sm4KeyFromConfig(cfg)

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
	preAuth := identity.NewPreAuthStore()
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			rl.Cleanup()
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

	creditStore := identity.NewCreditStore(d)
	creditSvc := identity.NewCreditService(creditStore)

	identityHandler := identity.NewHandler(identitySvc, creditSvc, userIDFrom)
	identityHandler.SetRateLimiter(rl)
	identityHandler.SetPreAuth(preAuth)
	creditHandler := identity.NewCreditHandler(creditSvc, userIDFrom)
	// 充值/覆盖/调整积分属资金敏感操作：需操作者当前口令二次验证。
	creditHandler.SetPasswordVerifier(identitySvc.VerifyCurrentPassword)

	tokenStore := identity.NewTokenStore(d)
	tokenSvc := identity.NewTokenService(tokenStore)
	tokenSvc.SetSecretKey(sm4Key)
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
	chSvc := channel.NewService(chStore)
	chSvc.SetManager(chMgr)
	chSvc.SetChannelModelStore(cmStore)
	keySvc := channel.NewKeyService(keyStore, sm4Key)
	chSvc.SetKeyService(keySvc)
	chHandler := channel.NewHandler(chSvc, userIDFrom)
	chHandler.SetKeyDeps(keySvc, chMgr)

	// tag / model
	tagStore := tag.NewStore(d)
	tagSvc := tag.NewService(tagStore)
	chSvc.SetTagResolver(channelTagResolver{svc: tagSvc})
	tagHandler := tag.NewHandler(tagSvc)
	modelSvc := model.NewService(model.NewStore(d))
	modelHandler := model.NewHandler(modelSvc)

	// billing
	billStore := billing.NewSqlStore(d)
	billCfgProvider := billing.NewConfigProvider(billStore)
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
	sess.SetStore(sessionStore)
	routerEngine := router.NewEngine(modelSvc, cmStore, chMgr, sess)

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
	// 调用日志：网关写入 + 控制台只读查询共用同一存储（billing_id 1:1 关联）。
	callLogStore := gateway.NewSQLCallLogStore(d)
	consoleAdmin.SetCallLogStore(callLogStore)
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
	gw := gateway.NewGateway(gateway.GatewayConfig{
		Tokens:        tokenSvc,
		Router:        routerEngine,
		Channels:      chMgr,
		Billing:       billSvc,
		Models:        modelSvc,
		ChannelModels: cmStore,
		TagKV:         tagKVResolver(tagStore),
		Pricing:       pricingModeResolver(userStore),
		// 三阶段计费（预扣/结算退补）：余额不足直接 402，请求后按实际多退少补。
		Credit:          &creditOpsAdapter{svc: creditSvc},
		Balance:         func(ctx context.Context, userID int64) (float64, error) { return creditSvc.GetWallet(ctx, userID) },
		ProviderFactory: gateway.DefaultProviderFactory,
		TouchLastUsed:   tokenSvc.TouchLastUsed,
		ListEnabledModels: func(ctx context.Context) ([]model.ExternalModel, error) {
			enabled := true
			return modelSvc.ListModels(ctx, &enabled)
		},
		Logger: logr,
	})
	// 调用日志采集注入（nil=不采集；生产装配 SQL 存储，与 console 只读共用）。
	gw.SetCallLogs(callLogStore)

	return &app{
		deps: server.Deps{
			Identity:     identityHandler,
			AdminUser:    adminUserHandler,
			Credit:       creditHandler,
			TokenAdmin:   tokenAdminHandler,
			TokenDev:     tokenDevHandler,
			Channel:      chHandler,
			Tag:          tagHandler,
			Model:        modelHandler,
			Announcement: announceHandler,
			Console:      consoleAdmin,
			DevConsole:   consoleDev,
			SyncAdmin:    syncHandler,
			Gateway:      gw.Handler(),
		},
		syncer:     syncer,
		mgr:        chMgr,
		sess:       sess,
		billSvc:    billSvc,
		sessions:   sessions,
		auditStore: auditStore,
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

// pricingModeResolver 由用户 ID 解析计费模式，默认 sale。
func pricingModeResolver(store *identity.Store) gateway.PricingModeResolver {
	return func(ctx context.Context, userID int64) (string, error) {
		u, err := store.GetByID(ctx, userID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return identity.PricingModeSale, nil
			}
			return "", err
		}
		if u.PricingMode == "" {
			return identity.PricingModeSale, nil
		}
		return u.PricingMode, nil
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
