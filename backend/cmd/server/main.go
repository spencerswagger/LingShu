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
	stdsync "sync"
	"syscall"
	"time"

	"github.com/team/llmgateway/internal/config"
	"github.com/team/llmgateway/internal/db"
	"github.com/team/llmgateway/internal/domain/amortize"
	"github.com/team/llmgateway/internal/domain/audit"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/calllog"
	"github.com/team/llmgateway/internal/domain/console"
	"github.com/team/llmgateway/internal/domain/facadebill"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/ledgersettle"
	"github.com/team/llmgateway/internal/domain/magpie"
	"github.com/team/llmgateway/internal/domain/magpieaccess"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/domain/reconcile"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/domain/subscription"
	"github.com/team/llmgateway/internal/domain/sync"
	"github.com/team/llmgateway/internal/domain/topology"
	"github.com/team/llmgateway/internal/domain/upstream"
	"github.com/team/llmgateway/internal/domain/v3billing"
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

// magpieJumpKey 读取代理侧管理面票据的 HMAC 密钥（32 字节，即 64 个 hex 字符）。
//
// 与 SM4/JWT 密钥同法自举：环境变量 > keys/magpie_jump.key > 首次生成并落盘（chmod 600）。
// 独立于 sm4.key：轮换它只会让已发出的门票全部失效（所有人回灵枢再点一次按钮），
// 不会影响渠道凭据解密——两件事不该共用一把钥匙。
func magpieJumpKey() []byte {
	const (
		envName = "MAGPIE_JUMP_KEY"
		keyPath = "keys/magpie_jump.key"
	)
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		b, err := hex.DecodeString(v)
		if err != nil || len(b) != 32 {
			slog.Error("MAGPIE_JUMP_KEY 非法：必须为 64 位 hex（32 字节）。留空则自动从 keys/magpie_jump.key 生成/复用", "env", envName)
			os.Exit(1)
		}
		return b
	}
	if raw, err := os.ReadFile(keyPath); err == nil {
		b, derr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if derr != nil || len(b) != 32 {
			slog.Error("keys/magpie_jump.key 内容非法：必须为 64 位 hex（32 字节）。请修正或删除该文件后重启以重新生成", "path", keyPath)
			os.Exit(1)
		}
		return b
	} else if !errors.Is(err, os.ErrNotExist) {
		slog.Error("读取 keys/magpie_jump.key 失败", "error", err)
		os.Exit(1)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		slog.Error("生成管理面票据密钥失败", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll("keys", 0o755); err != nil {
		slog.Error("创建 keys 目录失败", "error", err)
		os.Exit(1)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(b)), 0o600); err != nil {
		slog.Error("写入 keys/magpie_jump.key 失败", "error", err)
		os.Exit(1)
	}
	slog.Info("未配置 MAGPIE_JUMP_KEY，已自动生成强随机密钥并写入 keys/magpie_jump.key（后续重启复用）")
	return b
}

// userIDFrom 从请求上下文读取当前登录用户（server.UserIDFrom 适配）。
var userIDFrom = func(ctx context.Context) (int64, bool) {
	return server.UserIDFrom(ctx)
}

// roleFrom 从请求上下文读取当前登录用户角色（server.RoleFrom 适配）。
var roleFrom = func(ctx context.Context) (string, bool) {
	return server.RoleFrom(ctx)
}

// errText 把 error 渲染成日志字段值（nil → 空串，避免日志里出现 `<nil>`）。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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

	// model
	// 标签域（tag）随 0024 下线：semantic_tags / channel_tags / tokens.tag_id 均已删除，
	// 对应接口在 server.routes() 里以 410 显式停用（不再装配 Service/Handler）。
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
	var keySync *magpie.CallerKeySync
	billSvc := billing.NewService(billStore, billCfgProvider, creditSvc)
	// V3 旁路计费：每 60s 把未计费账本行落成账单（幂等：magpie_request_id）
	v3Poller := v3billing.NewPoller(d, billSvc, keySync, logr)
	// 结算链路 kill-switch（2026-11-20 拍板）：新结算链路默认**关**——未设置
	// V3_SETTLE_ENABLED 时走旧链路（= v0.15.0-rc4 的结算口径：名字直查、查不到留行
	// 重试、无 unpriced 兜底），补收入口一并 400。显式置 1/true/on/yes 才启用新链路
	// （组 ref 多形态候选链、route_id 绑定优先、unpriced 兜底、补收可用）。
	// 判定在 v3billing.SettleEnabledFromEnv 单源实现（fail-closed：打错字不得静默
	// 启用新链路）。开关状态启动即点名，不留「部署完为什么没出账单」的猜。
	if v3billing.SettleEnabledFromEnv() {
		v3Poller.SetNewSettleEnabled(true)
		logr.Info("V3 结算链路：新链路已启用", "env", v3billing.EnvSettleEnabled)
	} else {
		v3Poller.SetNewSettleEnabled(false)
		logr.Info("V3 结算链路：新链路未启用（默认关，走旧结算口径；置 "+v3billing.EnvSettleEnabled+"=1 启用）",
			"env", v3billing.EnvSettleEnabled)
	}
	go v3Poller.Run(context.Background())
	// unpriced 账单补收（B 方案·手动触发）：组定价后按当前售价给历史 unpriced 账单开新账单。
	v3RebillHandler := v3billing.NewRebillHandler(v3Poller)
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

	// facade 计费钩子（预扣 → 结算 / 退预扣）。口径：**无 usage 一律不计费**（整笔退预扣 +
	// failed/no-usage 账单），估算只用于预扣，绝不出估算账。依赖不全时返回 nil → 不产生计费动作。
	facadeBiller := facadebill.New(facadebill.Options{
		Ledger: billSvc,
		Wallet: creditSvc,
		Pricer: facadeSaleRates{svc: modelSvc},
		Logger: logr,
	})
	var (
		facadeReserve  magpie.ReserveHook
		facadeComplete magpie.Hook
	)
	if facadeBiller != nil {
		facadeReserve = facadeBiller.Reserve
		facadeComplete = facadeBiller.OnComplete
	} else {
		logr.Warn("facade 计费钩子未装配：/v1 转发不会产生账单与扣费")
	}

	// 代理侧 callerKey 映射的管理端入口：运维在 magpie 侧创建 callerKey 后录入灵枢
	// （灵枢不自行签发凭据；未绑定的令牌在转发时返回 403 magpie_key_missing）。
	magpieKeyAdmin := magpie.NewKeyAdminHandler(magpie.NewKeyStore(d), sm4Key, cfg.Magpie.AdminKey)

	// V3：令牌 ↔ callerKey 同步（页面接口 + magpie_keys）。
	// 页面口令来源：entrypoint 以 root 读出的 $MAGPIE_WEB_KEY，或直接读卷里的文件。
	// 缺口令就只跳过同步，不影响启动。
	{
		webBase := strings.TrimRight(strings.TrimSpace(os.Getenv("MAGPIE_WEB_BASE_URL")), "/")
		if !strings.HasPrefix(webBase, "http://") && !strings.HasPrefix(webBase, "https://") {
			webBase = "http://magpie:3430"
		}
		webKey := strings.TrimSpace(os.Getenv("MAGPIE_WEB_KEY"))
		if webKey == "" {
			if f := strings.TrimSpace(os.Getenv("MAGPIE_WEB_KEY_FILE")); f != "" {
				if b, err := os.ReadFile(f); err == nil {
					webKey = strings.TrimSpace(string(b))
				}
			}
		}
		if webKey == "" {
			logr.Warn("未取到 magpie 页面口令：令牌不会自动开通代理访问（检查 /run/magpie-secret/admin_web_key 权限）")
		} else {
			page := magpie.NewPageAdmin(webBase, webKey, nil)
			keySync = magpie.NewCallerKeySync(page, magpie.NewKeyStore(d), sm4Key)
			tokenSvc.SetCallerKeySyncer(keySync)
			keyStoreForLookup := magpie.NewKeyStore(d)
			tokenSvc.SetCallerKeyLookup(func(ctx context.Context, tokenID int64) (bool, error) {
				k, err := keyStoreForLookup.ByTokenID(ctx, tokenID)
				if err != nil || k == nil {
					return false, nil
				}
				return k.State == "ACTIVE", nil
			})
			magpieKeyAdmin.SetAutoSync(tokenSvc.Secret, keySync)
			v3Poller.SetKeySync(keySync)
			// 升级自动补建：启动即把存量令牌注册为代理侧 callerKey（指数退避，最多约 1h）
			go func() {
				for i := 0; i < 10; i++ {
					created, skipped, failed, err := tokenSvc.BackfillCallerKeys(context.Background(), 0)
					if err == nil {
						logr.Info("V3 存量令牌补建完成", "created", created, "skipped_already_bound", skipped["already-bound"], "skipped_no_cipher", skipped["no-cipher"], "failed", len(failed))
						if len(failed) == 0 {
							return
						}
					} else {
						logr.Warn("V3 存量令牌补建失败（下轮重试）", "err", err)
					}
					time.Sleep(time.Duration(10*(i+1)) * time.Second)
				}
			}()
		}
	}

	// 代理侧管理面「一键跳转」：签发入口票据 + 给 nginx 的 auth_request 提供校验端点。
	// 代理侧未接入（magpie.base_url 为空）时签发接口给 503 + reason，前端把按钮置灰，
	// 不制造「点开是 502」的死链。
	magpieAccessHandler := magpieaccess.NewHandler(magpieaccess.Options{
		Signer:     magpieaccess.NewSigner(magpieJumpKey()),
		Enabled:    cfg.MagpieEnabled,
		PublicBase: strings.TrimSpace(os.Getenv("MAGPIE_ADMIN_PUBLIC_URL")),
		UserID:     userIDFrom,
		Role:       roleFrom,
		Logger:     logr,
	})
	if !cfg.MagpieEnabled() {
		logr.Info("代理侧未接入：管理面跳转接口返回 503（按钮置灰），/magpie/ 反代不会被使用")
	}

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
		OnReserve:     facadeReserve,
		OnComplete:    facadeComplete,
		Logger:        logr,
	})
	if magpieHandler == nil {
		logr.Info("未配置 magpie.base_url：对外转发接口保持停用（410）")
	}

	// V2 运行参数（F1 三键：基址 / 两段超时 / 三开关）。凭据不进配置表：
	// 上游鉴权头只在装配层从环境变量取，转发链逐请求注入。
	// 注：/v1 转发基址由 magpie.* 配置提供；upstream.* 只服务账本同步 / 对账 / 结算三条后台链。
	upstreamCfg := upstream.NewConfigProvider(billStore)
	if upCfg, problems := upstreamCfg.Load(context.Background()); true {
		for _, pb := range problems {
			logr.Warn("上游配置有问题（已按字段回落默认值）", "problem", pb)
		}
		// 注意：base_url 只作参考，**不**再是账本链路可用性判据（见下方 UPSTREAM_LEDGER_URL）。
		logr.Info("上游运行参数（账本链路可用性见 UPSTREAM_LEDGER_URL，不看 base_url）",
			"base_url_set", upCfg.BaseURL != "",
			"stream_first_byte_ms", upCfg.Timeouts.StreamFirstByte.Milliseconds(),
			"stream_idle_ms", upCfg.Timeouts.StreamIdle.Milliseconds())
	}

	// 账本链路端点（账本链路可用性的唯一判据）。V3 起转发由 nginx 直连代理侧，
	// upstream.base_url 不再参与任何链路；账本同步/对账是否可跑只看这个端点是否配置，
	// 彻底不再依赖 upstream.base_url（旧闸门 SyncActive() 依赖它 → 恒 false → 账本永远空）。
	// 凭据只从环境变量取（不进配置表）：UPSTREAM_ADMIN_KEY 由 entrypoint 与代理侧 admin_key 对齐。
	ledgerURL := strings.TrimSpace(os.Getenv("UPSTREAM_LEDGER_URL"))
	adminKey := strings.TrimSpace(os.Getenv("UPSTREAM_ADMIN_KEY"))
	adminHeader := strings.TrimSpace(os.Getenv("UPSTREAM_ADMIN_AUTH_HEADER"))
	// 启动即把「端点 + 是否启用 + 原因」打出来，别让账本链路静默停摆。
	if ledgerURL == "" {
		logr.Warn("账本链路未启用：未配置 UPSTREAM_LEDGER_URL（账本同步/对账停摆，不写假账单）",
			"env", "UPSTREAM_LEDGER_URL")
	} else {
		authHeader := adminHeader
		if authHeader == "" {
			authHeader = "Authorization"
		}
		logr.Info("账本链路已启用",
			"endpoint", ledgerURL,
			"admin_key_set", adminKey != "",
			"auth_header", authHeader)
	}

	// 降级开关的热生效读取器（TTL 5s）：`ConfigProvider.Load` 的缓存**没有 TTL**，
	// `Invalidate()` 今天没有生产调用点（`/admin/configs/system` 不写 upstream.* 三键），
	// 所以每请求直接 Load 拿到的是**启动时冻结的值** —— 运维关掉开关不重启就无效（真机复现过）。
	// 这层每 TTL 周期至多一次 DB 读，改动 5s 内生效。账本可用性由 ledgerURL 决定（见 LedgerActive）。
	gate := upstream.NewDegradeGate(upstreamCfg, ledgerURL, 5*time.Second)

	// 转发降级开关（sys_configs：upstream.degrade.passthrough_enabled）。
	//
	// 只读开关本身，**不看** upstream.base_url：那个基址只服务账本同步/对账，
	// 未配置（默认空串）是正常态，拿它当门会把所有 /v1 请求关成 503。
	// 开关关 → /v1 与 /v1beta 返回 503 + 明确文案（不代理、不计费、不落账单）。
	if magpieHandler != nil {
		magpieHandler.SetGate(func() bool { return gate.Passthrough(context.Background()) })
	}

	// D4：对账窗口的四列计数由对账服务在窗口结束时写入（四个 0 等于「漏账不可见」）。
	// 只落计数与状态；七列归因的读取方属阶段二（本期不做）。
	windowStore := upstream.NewSQLWindowStore(d)
	recon := reconcile.NewReconciler(reconcile.Options{
		Local:    reconcile.NewLocalSource(d),
		Upstream: reconcile.NewUpstreamSource(d),
		Counts:   windowStore,
		States:   windowStore,
		Streaks:  windowStore,
		// ⚠️ 账本可用性必须走 gate.LedgerActive（= 账本端点 UPSTREAM_LEDGER_URL 已配置），
		// 不再依赖 upstream.base_url（默认空串、无管理端写入口 → 旧判据恒 false → 账本永不拉、
		// 窗口全 skipped）。端点未配 ⇒ 账本拉取停摆 ⇒ 窗口落 skipped（不是 blocked，不冒充「已对平」）。
		LedgerEnabled: func() bool { return gate.LedgerActive(context.Background()) },
		Logger:        logr,
	})
	// F9：账本同步轮次（拉代理侧用量变更流 → 幂等落 ledger_entries）。
	//
	// 口径（2026-11-17 按代理侧真实报文适配）：
	//  ① 用官方 cursor 变更流（cursor=start|now|<next_cursor>），不再用 from/to 时刻过滤
	//     （代理侧没有这两个参数，只有 period/day；cursor 才是「自建账本」的官方方式）；
	//  ② 排序不再依赖 occurred_at ASC：cursor feed 按 seq 推变更，按 id upsert 天然幂等；
	//  ③ 续读游标持久化在 reconcile_windows.detail->>'ledger_cursor'，重启不重放整本账；
	//  ④ 60s ticker + 空轮告警（`RunOnce` 的 Empty 会打 WARN，不再 0 行 + nil 静默空转）。
	// 端点/凭据已在上面（gate 创建前）从环境变量读出，此处直接使用。
	// 代理侧 cost 是 USD；灵枢账本列是积分。换算与订阅录入同源（积分 = 元 × 1e6 ÷ R，
	// USD 先按 billing.cny_rate 折成人民币），配置缺失时该页报错（blocked），不静默写 0 成本。
	// 换算按页缓存：CostConverter 是**逐行**调用的，直接读配置会让一页 500 行打 1000 次库。
	var (
		convMu     stdsync.Mutex
		convRate   float64
		convR      int64
		convLoaded time.Time
	)
	costConv := func(usd float64) (float64, error) {
		convMu.Lock()
		defer convMu.Unlock()
		if convRate <= 0 || time.Since(convLoaded) > time.Minute {
			rate, err := billCfgProvider.CnyRate()
			if err != nil {
				return 0, fmt.Errorf("读取 billing.cny_rate 失败：%w", err)
			}
			r, err := billCfgProvider.R()
			if err != nil {
				return 0, fmt.Errorf("读取 billing.r 失败：%w", err)
			}
			if rate <= 0 || r <= 0 {
				return 0, fmt.Errorf("汇率/R 非法（cny_rate=%v r=%d）", rate, r)
			}
			convRate, convR, convLoaded = rate, r, time.Now()
		}
		return usd * convRate * 1e6 / float64(convR), nil
	}
	syncSrc := upstream.NewMagpieLedgerSource(ledgerURL, adminKey, adminHeader, nil, costConv)
	rounder := upstream.NewRounder(upstream.RounderOptions{
		Feed:    syncSrc,
		Ledger:  upstream.NewLedgerStore(d),
		Windows: windowStore,
		Now:     time.Now,
		Logger:  logr,
	})
	go func() {
		// 60s 一跳：与窗口宽度同量级。
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		// 拉完即结：账本拉取成功后**立即**结算一次，不等 poller 自己的 60s ticker——
		// 否则「拉取 60s + 结算 60s」串行两跳，端到端账单耗时会在 120s 验收线上抖。
		settleNow := func() {
			defer func() {
				if rec := recover(); rec != nil {
					logr.Error("V3 计费轮次 panic", "panic", rec)
				}
			}()
			if n, err := v3Poller.Once(context.Background()); err != nil {
				logr.Error("V3 计费轮次失败（账本同步后触发）", "err", err, "settled", n)
			}
		}
		// 连续空轮告警器：端点已配但连续 5 轮 0 行 → 提示一次「疑似未通/无新行」（带最近 HTTP 状态/错误）。
		emptyWarner := upstream.NewEmptyRoundWarner(5)
		// 端点未配置时的「每轮 WARN」计数（可聚合，避免静默停摆）。
		unconfiguredRounds := 0
		for range t.C {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						logr.Error("账本同步轮次 panic", "panic", rec)
					}
				}()
				// 账本可用性只看 UPSTREAM_LEDGER_URL（不再看 upstream.base_url / 降级开关）。
				// 端点没配 = 不拉取（不落 blocked，避免每分钟制造假告警），但**每轮 WARN** 不静默。
				if ok, reason := gate.LedgerAvailability(); !ok {
					unconfiguredRounds++
					logr.Warn("账本链路未配置，本轮跳过（账本同步不运行）",
						"reason", reason, "env", "UPSTREAM_LEDGER_URL", "round", unconfiguredRounds)
					return
				}
				unconfiguredRounds = 0
				res, err := rounder.RunOnce(context.Background())
				switch {
				case err != nil:
					// 失败：记错误（下次空轮告警会带上），本轮不推进账本。
					emptyWarner.Observe(false, res.LastHTTPStatus, err)
					logr.Warn("账本同步轮次未完成", "err", err,
						"window", res.Window.String(), "pages", res.Pages,
						"fetched", res.Fetched, "inserted", res.Inserted)
				case res.Empty:
					// 空轮：RunOnce 内部已打 WARN（sync.round empty），这里补可聚合计数，
					// 并在连续多轮 0 行时提示一次「疑似未通/无新行」。
					logr.Warn("账本同步空轮（计数器）", "sync_round_empty", 1,
						"streak", emptyWarner.Streak()+1,
						"window", res.Window.String())
					if emptyWarner.Observe(true, res.LastHTTPStatus, nil) {
						logr.Warn("账本疑似未通/无新行（端点已配但连续多轮 0 行）",
							"streak", emptyWarner.Streak(),
							"last_http_status", emptyWarner.LastStatus(),
							"last_err", errText(emptyWarner.LastError()))
					}
				default:
					emptyWarner.Observe(false, res.LastHTTPStatus, nil)
				}
				// 拉取完成（无论有无新行）都结算一次：空轮的结算是一次廉价 SELECT，成本可忽略。
				if err == nil {
					settleNow()
				}
			}()
		}
	}()

	// 阶段二 A1：预扣过期扫描（15 分钟未终结 → 全额退回）。
	// 一跳 60s：过期点是「15 分钟 + 最多 1 分钟」，对资金回收足够；单轮走一个事务 + SKIP LOCKED，多副本安全。
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for range t.C {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						logr.Error("预扣过期扫描 panic", "panic", rec)
					}
				}()
				res, err := creditSvc.ExpireHolds(context.Background(), time.Now(), 200)
				if err != nil {
					logr.Warn("预扣过期扫描失败（下轮重试）", "err", err)
					return
				}
				if res.Expired > 0 {
					logr.Info("退回超时未结算的预扣", "holds", res.Expired, "credits", res.Amount)
				}
			}()
		}
	}()

	// 阶段二 A3：账本结算（账本条目 → 账单成本列 + 预扣登记）。
	// 与同步轮次分开的 60s 循环：同步负责「拉」，结算负责「挂」，两者失败互不影响。
	settler := ledgersettle.NewMatcher(ledgersettle.Options{
		Store:  ledgersettle.NewSQLStore(d),
		Now:    time.Now,
		Logger: logr,
	})
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for range t.C {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						logr.Error("账本结算轮次 panic", "panic", rec)
					}
				}()
				res, err := settler.RunOnce(context.Background())
				if err != nil {
					logr.Warn("账本结算轮次失败（窗口保留待下轮）", "err", err)
					return
				}
				if res.Linked > 0 || res.UnmatchedBilling > 0 || res.UnmatchedLedger > 0 {
					logr.Info("账本结算轮次完成",
						"linked", res.Linked, "skipped", res.Skipped,
						"unmatched_billing", res.UnmatchedBilling,
						"unmatched_ledger", res.UnmatchedLedger, "cost", res.Cost)
				}
			}()
		}
	}()

	// 阶段二 A5：订阅月费分摊（固定成本摊到当期账单）。
	// 5 分钟一跳：账本条目迟到会让某些行从「可摊」变成「已挂账本」，重算即收敛；
	// 写的是 SET 不是累加，频率高低都不影响结果。
	amortizer := amortize.NewAllocator(amortize.Options{
		Store:  amortize.NewSQLStore(d),
		Now:    time.Now,
		Logger: logr,
	})
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for range t.C {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						logr.Error("月费分摊轮次 panic", "panic", rec)
					}
				}()
				res, err := amortizer.RunOnce(context.Background())
				if err != nil {
					logr.Warn("月费分摊轮次失败（下轮重试）", "err", err)
					return
				}
				if res.Updated > 0 || res.Unallocated > 0 {
					logr.Info("月费分摊轮次完成",
						"subscriptions", res.Subscriptions, "skipped", res.Skipped,
						"updated", res.Updated, "allocated", res.Allocated,
						"unallocated", res.Unallocated)
				}
			}()
		}
	}()

	go func() {
		// 60s 一跳：与窗口宽度同量级，窗口结束后下一跳就能被算上。
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for range t.C {
			func() {
				// 单轮 panic 不许带走进程（对账是后台任务，崩了应告警继续）。
				defer func() {
					if rec := recover(); rec != nil {
						logr.Error("对账轮次 panic", "panic", rec)
					}
				}()
				if _, err := recon.RunOnce(context.Background()); err != nil {
					logr.Warn("对账轮次失败（窗口保持待重算）", "err", err)
				}
			}()
		}
	}()

	// 阶段三 T1/T3：上游拓扑（internal_models 镜像）。
	//
	// 真源在网关：模型清单 `/v1/models` + 组清单 `/api/groups`（口径见《2026-11-15-v2-upgrade》）。
	// 基址默认取 magpie 基址，可用 TOPOLOGY_SOURCE_URL / TOPOLOGY_SOURCE_KEY 覆盖
	// （网关与转发口不同源时用得上；密钥永远来自环境变量，不进 sys_configs）。
	topoURL := strings.TrimSpace(os.Getenv("TOPOLOGY_SOURCE_URL"))
	if topoURL == "" {
		topoURL = cfg.Magpie.BaseURL
	}
	topoKey := strings.TrimSpace(os.Getenv("TOPOLOGY_SOURCE_KEY"))
	if topoKey == "" {
		topoKey = cfg.Magpie.AdminKey
	}
	topoStore := topology.NewSQLStore(d)
	topoSource := topology.NewHTTPGatewaySource(topoURL, topoKey, cfg.Magpie.AuthHeader, nil)
	topoSyncer := topology.NewSyncer(topology.SyncerOptions{
		Source: topoSource,
		Store:  topoStore,
		Now:    time.Now,
		Logger: logr,
	})
	topologyHandler := topology.NewHandler(topoStore, topoSyncer)
	topologyHandler.SetRawSource(topoSource)
	if topoSource.Configured() {
		// 10 分钟一跳：拓扑变动（上/下线模型）远慢于账单；手动 T3 随时可触发同一份实现。
		go func() {
			t := time.NewTicker(10 * time.Minute)
			defer t.Stop()
			for range t.C {
				func() {
					defer func() {
						if rec := recover(); rec != nil {
							logr.Error("拓扑同步轮次 panic", "panic", rec)
						}
					}()
					if _, err := topoSyncer.RunOnce(context.Background()); err != nil {
						logr.Warn("拓扑同步轮次失败（保留上期快照）", "err", err)
					}
				}()
			}
		}()
	} else {
		// 不静默：没配基址时 T3 会明确报 topology_source_not_configured，
		// 这里再留一条启动日志，免得运维以为是同步器坏了。
		logr.Warn("未配置网关拓扑源（magpie.base_url / TOPOLOGY_SOURCE_URL）：拓扑同步不可用",
			"topology", "disabled")
	}

	// 阶段三 S2/S6：订阅录入（A5 月费分摊的唯一入口）+ 手工重算。
	// 录入端收「元/美元」，落库 price_amount 是积分（×1e6÷R，USD 先 × cny_rate）。
	subscriptionSvc := subscription.NewService(subscription.Options{
		Store:  subscription.NewSQLStore(d),
		Config: billCfgProvider,
		Audit:  auditStore,
		Logger: logr,
	})
	subscriptionHandler := subscription.NewHandler(subscriptionSvc, amortizer, auditStore, logr)

	return &app{
		deps: server.Deps{
			Identity:      identityHandler,
			AdminUser:     adminUserHandler,
			Credit:        creditHandler,
			TokenAdmin:    tokenAdminHandler,
			TokenDev:      tokenDevHandler,
			Model:         modelHandler,
			Announcement:  announceHandler,
			Console:       consoleAdmin,
			DevConsole:    consoleDev,
			SyncAdmin:     syncHandler,
			Facade:        magpieHandler,
			MagpieKey:     magpieKeyAdmin,
			MagpieAccess:  magpieAccessHandler,
			Topology:      topologyHandler,
			Subscriptions: subscriptionHandler,
			V3Rebill:      v3RebillHandler,
		},
		syncer:     syncer,
		sess:       sess,
		billSvc:    billSvc,
		sessions:   sessions,
		auditStore: auditStore,
		writer:     asyncWriter,
	}, nil
}

// creditHoldAdapter 把 identity.CreditService 的预扣登记能力适配为 gateway.HoldOps（阶段二 A1）。
type creditHoldAdapter struct {
	svc *identity.CreditService
}

func (a *creditHoldAdapter) ReserveHold(ctx context.Context, billingID string, userID, tokenID int64, amount float64, sessionID, remark string) error {
	var tid *int64
	if tokenID != 0 {
		id := tokenID
		tid = &id
	}
	_, err := a.svc.PreConsumeHold(ctx, identity.HoldRequest{
		BillingID: billingID,
		UserID:    userID,
		TokenID:   tid,
		Amount:    amount,
		SessionID: sessionID,
		Remark:    remark,
	})
	return err
}

func (a *creditHoldAdapter) CloseHold(ctx context.Context, billingID, status, ledgerEntryID string) (bool, error) {
	return a.svc.CloseHold(ctx, billingID, status, ledgerEntryID)
}

// facadeSaleRates 把 model.Service 适配为 facadebill.Pricer（售价 + 模型级时段/分档）。
type facadeSaleRates struct{ svc *model.Service }

func (a facadeSaleRates) SaleRatesFor(ctx context.Context, externalModel string) (billing.Rates, *billing.TimeCoeffConfig, []billing.TierRule, error) {
	m, err := a.svc.GetByExternalName(ctx, externalModel)
	if err != nil {
		return nil, nil, nil, err
	}
	return m.SaleRates, m.TimeConfig, m.ContextTiers, nil
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

// keyNameResolver 装配渠道密钥名称批量反查钩子（console.Admin.SetKeyNameResolver）。
//
// ⚠️ 渠道体系已下线（0024 删了 channel_keys）：**没有来源了**，恒返回空 map，
// 于是 channel_key_name 在各列表/详情里恒为空串（前端按空串渲染，结构不变）。
// 保留装配点是为了让 console 的既有调用链不变；后续可连同钩子一起删。
func keyNameResolver(db *sql.DB) func(ctx context.Context, ids []int64) (map[int64]string, error) {
	_ = db
	return func(ctx context.Context, ids []int64) (map[int64]string, error) {
		_ = ctx
		_ = ids
		return nil, nil
	}
}
