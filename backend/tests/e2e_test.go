// Package e2e 端到端冒烟测试：覆盖「登录 → 建开发者用户 → 充值 → 建渠道(假上游) →
// 建模型映射/语义标签 → 建令牌 → 网关转发 → 计费扣积分 → 账单一致」的完整闭环。
//
// 守卫式设计：本机可无 Docker/PG。仅当设置了 TEST_DATABASE_URL 环境变量时才真正
// 连接 PostgreSQL 执行；未设置则自动 Skip，保证 `go test ./...` 默认全绿。
//
// 运行方式（建议指向独立测试库，会 DROP SCHEMA 重建）：
//
//	TEST_DATABASE_URL='postgres://llmgw:llmgw@127.0.0.1:5432/llmgw_test?sslmode=disable' \
//	    go test ./tests/ -v
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
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
	"github.com/team/llmgateway/internal/pkg/jwtx"
	"github.com/team/llmgateway/internal/pkg/logger"
	"github.com/team/llmgateway/internal/pkg/ratelimit"
	"github.com/team/llmgateway/internal/pkg/session"
	"github.com/team/llmgateway/internal/seeding"
	"github.com/team/llmgateway/internal/server"
)

// testSm4Key 渠道凭据加密密钥（16 字节）。与 cmd/server 一致语义，用固定测试密钥保证可复现。
var testSm4Key = []byte("0123456789abcdef")

// testDSN 从 TEST_DATABASE_URL 读；为空则整个 e2e 跳过。
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过 e2e（可用 docker compose / 系统 PG 起库后设置）")
	}
	return dsn
}

// generateKeyFiles 生成并落盘 RSA 密钥对（参考 internal/pkg/jwtx 测试写法），返回私/公钥路径。
func generateKeyFiles(t *testing.T) (privPath, pubPath string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	dir := t.TempDir()
	privPath = filepath.Join(dir, "jwt_priv.pem")
	pubPath = filepath.Join(dir, "jwt_pub.pem")
	if err := os.WriteFile(privPath, privPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pubPath, pubPEM, 0600); err != nil {
		t.Fatal(err)
	}
	return privPath, pubPath
}

// resetSchema 清空并重建 public schema，保证测试从干净库开始。
func resetSchema(t *testing.T, d *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := d.ExecContext(ctx, `DROP SCHEMA public CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if _, err := d.ExecContext(ctx, `CREATE SCHEMA public`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
}

// userIDFrom 适配 server.UserIDFrom，供装配注入。
var userIDFrom = func(ctx context.Context) (int64, bool) { return server.UserIDFrom(ctx) }

// testApp 装配结果（镜像 cmd/server buildApp，因 buildApp 位于 package main 不可导入，此处复制装配并注入测试依赖）。
type testApp struct {
	mgr  *channel.Manager
	syn  *sync.Syncer
	deps server.Deps
}

// buildTestServer 完整装配应用并经 httptest 暴露 http.Handler，返回服务与清理函数。
func buildTestServer(t *testing.T, dsn string) (*httptest.Server, func()) {
	t.Helper()
	ctx := context.Background()

	d, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	resetSchema(t, d)
	if _, err := db.Migrate(ctx, d); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seeding.Seed(ctx, d); err != nil {
		t.Fatalf("seed: %v", err)
	}

	privPath, pubPath := generateKeyFiles(t)
	cfg := &config.Config{
		Server:   config.ServerConfig{Addr: "127.0.0.1:0"},
		Database: config.DBConfig{DSN: dsn},
		JWT:      config.JWTConfig{PrivateKeyPath: privPath, PublicKeyPath: pubPath, TTLMinutes: 720},
		Sync:     config.SyncConfig{PriceSourceURL: "http://127.0.0.1:1/not-used", IntervalMinutes: 60},
	}
	jwtMgr, err := jwtx.NewManager(cfg.JWT.PrivateKeyPath, cfg.JWT.PublicKeyPath, cfg.JWT.TTLMinutes)
	if err != nil {
		t.Fatalf("init jwt: %v", err)
	}
	logr := logger.NewDefault()

	// 安全组件：会话注册表（loader 查 users 表 token_version/status/must_change，镜像 main 装配）、
	// 审计 Store、登录限流、preauth 与 TOTP，保证安全端点（logout/totp 两步登录）可用。
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

	// 复制 main.go buildApp 装配（含测试用 resolvers）。
	userStore := identity.NewStore(d)
	identitySvc := identity.NewService(userStore, jwtMgr)
	identitySvc.SetSessionRegistry(sessions)
	identitySvc.SetAudit(auditStore)
	identitySvc.SetTOTP(identity.NewTOTPService(userStore, testSm4Key))
	identitySvc.SetRateLimiter(rl)

	creditStore := identity.NewCreditStore(d)
	creditSvc := identity.NewCreditService(creditStore)

	identityHandler := identity.NewHandler(identitySvc, creditSvc, userIDFrom)
	identityHandler.SetRateLimiter(rl)
	identityHandler.SetPreAuth(preAuth)
	creditHandler := identity.NewCreditHandler(creditSvc, userIDFrom)
	// 资金敏感操作（充值/覆盖/调整）需操作者当前口令二次验证（镜像 main 装配）。
	creditHandler.SetPasswordVerifier(identitySvc.VerifyCurrentPassword)

	tokenStore := identity.NewTokenStore(d)
	tokenSvc := identity.NewTokenService(tokenStore)
	tokenSvc.SetSecretKey(testSm4Key) // 令牌明文 SM4 加密落库，支持事后查看/复制（镜像 main 装配）
	tokenAdminHandler := identity.NewTokenHandler(tokenSvc, userIDFrom)
	tokenDevHandler := identity.NewTokenHandler(tokenSvc, userIDFrom)
	// 查看明文密钥属敏感操作：需当前口令二次验证（X-Current-Password 头）。
	tokenAdminHandler.SetPasswordVerifier(identitySvc.VerifyCurrentPassword)
	tokenDevHandler.SetPasswordVerifier(identitySvc.VerifyCurrentPassword)
	adminUserHandler := identity.NewAdminUserHandler(identitySvc, creditSvc, userIDFrom)
	adminUserHandler.SetTOTP(identity.NewTOTPService(userStore, testSm4Key))
	announceStore := identity.NewAnnouncementStore(d)
	announceSvc := identity.NewAnnouncementService(announceStore)
	announceHandler := identity.NewAnnouncementHandler(announceSvc, userIDFrom)

	chStore := channel.NewStore(d)
	cmStore := channel.NewChannelModelStore(d)
	chMgr := channel.NewManager(chStore, cmStore, testSm4Key, nil, logr, nil)
	chSvc := channel.NewService(chStore)
	chSvc.SetManager(chMgr)
	chSvc.SetChannelModelStore(cmStore)
	chHandler := channel.NewHandler(chSvc, userIDFrom)

	tagStore := tag.NewStore(d)
	tagSvc := tag.NewService(tagStore)
	tagHandler := tag.NewHandler(tagSvc)
	modelSvc := model.NewService(model.NewStore(d))
	modelHandler := model.NewHandler(modelSvc)

	billStore := billing.NewSqlStore(d)
	billCfgProvider := billing.NewConfigProvider(billStore)
	billSvc := billing.NewService(billStore, billCfgProvider, creditSvc)

	routerEngine := router.NewEngine(modelSvc, cmStore, chMgr, nil)
	consoleAdmin := console.NewAdmin(d, billStore, chMgr, userIDFrom)
	consoleDev := console.NewDev(billStore, userIDFrom)

	syncStore := sync.NewStore(d)
	syncer := sync.NewSyncer(sync.SyncerConfig{Store: syncStore, SourceURL: cfg.Sync.PriceSourceURL, Interval: time.Duration(cfg.Sync.IntervalMinutes) * time.Minute, Logger: logr})
	syncHandler := sync.NewHandler(syncer, userIDFrom)

	tagKVResolver := func(ctx context.Context, tagID *int64) (map[string]string, error) {
		if tagID == nil {
			return nil, nil
		}
		tag, err := tagStore.GetByID(ctx, *tagID)
		if err != nil {
			if err == sql.ErrNoRows {
				return nil, nil
			}
			return nil, err
		}
		if !tag.Enabled {
			return nil, nil
		}
		return tag.KVPairs, nil
	}
	pricingModeResolver := func(ctx context.Context, userID int64) (string, error) {
		u, err := userStore.GetByID(ctx, userID)
		if err != nil {
			return identity.PricingModeSale, nil
		}
		if u.PricingMode == "" {
			return identity.PricingModeSale, nil
		}
		return u.PricingMode, nil
	}

	gw := gateway.NewGateway(gateway.GatewayConfig{
		Tokens:          tokenSvc,
		Router:          routerEngine,
		Channels:        chMgr,
		Billing:         billSvc,
		Models:          modelSvc,
		ChannelModels:   cmStore,
		TagKV:           tagKVResolver,
		Pricing:         pricingModeResolver,
		ProviderFactory: gateway.DefaultProviderFactory,
		Logger:          logr,
	})

	app := &testApp{
		mgr: chMgr,
		syn: syncer,
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
	}

	if err := app.mgr.SyncFromDB(ctx); err != nil {
		t.Fatalf("sync channels: %v", err)
	}

	srv := server.New(cfg, d, logr, jwtMgr, sessions, auditStore, app.deps)
	ts := httptest.NewServer(srv.Handler())
	cleanup := func() {
		ts.Close()
		app.mgr.Stop()
		_ = d.Close()
	}
	return ts, cleanup
}

// ===== HTTP 辅助 =====

type apiResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// rawReq 发送一次请求。token 非空时带 Authorization: Bearer。body 非 nil 时序列化为 JSON。
// 返回 (状态码, 响应头, 响应体)。
func rawReq(t *testing.T, method, url, token string, body any) (int, http.Header, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	r, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("do request %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	hdr := resp.Header
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, hdr, raw
}

// req 是 rawReq 的轻量封装，丢弃响应头。
func req(t *testing.T, method, url, token string, body any) (int, []byte) {
	t.Helper()
	status, _, raw := rawReq(t, method, url, token, body)
	return status, raw
}

func decodeResp(t *testing.T, raw []byte) *apiResp {
	t.Helper()
	var ar apiResp
	if err := json.Unmarshal(raw, &ar); err != nil {
		t.Fatalf("decode resp %s: %v", raw, err)
	}
	if ar.Code != 0 {
		t.Fatalf("业务错误: code=%d msg=%s", ar.Code, ar.Message)
	}
	return &ar
}

// ===== mock 上游：OpenAI-compat chat completion =====

var mockCompletion = `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"qw-max","choices":[{"index":0,"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1234,"completion_tokens":567,"total_tokens":1801}}`

func startMockUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 探测端点：/models 返回空模型表亦可；/chat/completions 返回带 usage 的体。
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, mockCompletion)
	}))
	t.Cleanup(s.Close)
	return s
}

// ===== 登录与鉴权辅助 =====

// adminNewPassword 是 e2e 中默认管理员首登改密后的新口令（满足强度要求：≥8 位且含多类字符）。
const adminNewPassword = "Admin@123456"

// login 以用户名/口令走 /auth/login 一步登录，返回 JWT；失败直接终止测试。
func login(t *testing.T, base, username, password string) string {
	t.Helper()
	status, raw := req(t, http.MethodPost, base+"/api/v1/auth/login", "", map[string]string{"Username": username, "Password": password})
	if status != http.StatusOK {
		t.Fatalf("login %s status=%d body=%s", username, status, raw)
	}
	ar := decodeResp(t, raw)
	var d struct {
		Token string `json:"Token"`
	}
	if err := json.Unmarshal(ar.Data, &d); err != nil {
		t.Fatalf("decode login data: %v", err)
	}
	return d.Token
}

// loginAdmin 以默认管理员登录。seeding 新建的 admin 带 must_change_password=true，
// 首次登录后走 /auth/me/password 白名单改密并返回新 token；口令已改过的后续登录
// 自动改用 adminNewPassword，保证同一 server 内可重复调用。
func loginAdmin(t *testing.T, base string) string {
	t.Helper()
	status, raw := req(t, http.MethodPost, base+"/api/v1/auth/login", "", map[string]string{"Username": "admin", "Password": "admin123"})
	var d struct {
		Token              string `json:"Token"`
		MustChangePassword bool   `json:"MustChangePassword"`
	}
	switch status {
	case http.StatusOK:
		ar := decodeResp(t, raw)
		if err := json.Unmarshal(ar.Data, &d); err != nil {
			t.Fatalf("decode admin login: %v", err)
		}
	case http.StatusUnauthorized:
		// 默认口令已改（首登改密后再次登录），改用新口令
		status, raw := req(t, http.MethodPost, base+"/api/v1/auth/login", "", map[string]string{"Username": "admin", "Password": adminNewPassword})
		if status != http.StatusOK {
			t.Fatalf("admin login(new pw) status=%d body=%s", status, raw)
		}
		ar := decodeResp(t, raw)
		if err := json.Unmarshal(ar.Data, &d); err != nil {
			t.Fatalf("decode admin login(new pw): %v", err)
		}
	default:
		t.Fatalf("admin login status=%d body=%s", status, raw)
	}
	if !d.MustChangePassword {
		return d.Token
	}
	// 强制首登改密：走白名单路径改密，返回新 token（旧 token 因 ver bump 失效）
	status, raw = req(t, http.MethodPut, base+"/api/v1/auth/me/password", d.Token, map[string]string{
		"OldPassword": "admin123",
		"NewPassword": adminNewPassword,
	})
	if status != http.StatusOK {
		t.Fatalf("admin change password status=%d body=%s", status, raw)
	}
	ar := decodeResp(t, raw)
	var c struct {
		Token string `json:"Token"`
	}
	if err := json.Unmarshal(ar.Data, &c); err != nil {
		t.Fatalf("decode change password: %v", err)
	}
	return c.Token
}

// apiGet 携带 Bearer token 发起 GET，返回 (状态码, 响应体)。
func apiGet(t *testing.T, base, token, path string) (int, []byte) {
	t.Helper()
	return req(t, http.MethodGet, base+path, token, nil)
}

// apiPost 携带 Bearer token 发起 POST，返回 (状态码, 响应体)。
func apiPost(t *testing.T, base, token, path string, body any) (int, []byte) {
	t.Helper()
	return req(t, http.MethodPost, base+path, token, body)
}

// ===== 冒烟用例 =====

func TestE2E_GatewayBillingLoop(t *testing.T) {
	dsn := testDSN(t)
	ts, cleanup := buildTestServer(t, dsn)
	defer cleanup()
	base := ts.URL

	mock := startMockUpstream(t)

	// 1) admin 登录（seeding 建了 admin/admin123，强制首登改密后获得可用 token）
	adminToken := loginAdmin(t, base)

	// 2) admin 建开发者用户 + 充值
	// 注：雪花 ID 超出 JS 安全整数，API 返回的 ID 一律为字符串，解析后转 int64 供内部使用。
	var devID int64
	{
		status, raw := req(t, http.MethodPost, base+"/api/v1/admin/users", adminToken, map[string]string{
			"Username": "dev1", "Password": "Dev@123456", "Role": "DEVELOPER", "PricingMode": "sale",
		})
		if status != http.StatusOK {
			t.Fatalf("create dev status=%d body=%s", status, raw)
		}
		ar := decodeResp(t, raw)
		var u struct {
			ID string `json:"ID"`
		}
		if err := json.Unmarshal(ar.Data, &u); err != nil {
			t.Fatalf("decode create dev: %v", err)
		}
		id, err := strconv.ParseInt(u.ID, 10, 64)
		if err != nil {
			t.Fatalf("parse dev id %q: %v", u.ID, err)
		}
		devID = id
	}
	const rechargeAmount = 100.0
	{
		// 负向断言：不带当前口令调用 recharge → 403。
		path := fmt.Sprintf("/api/v1/admin/users/%d/wallet/recharge", devID)
		status, raw := req(t, http.MethodPost, base+path, adminToken, map[string]any{"Amount": rechargeAmount})
		if status != http.StatusForbidden {
			t.Fatalf("recharge without password 期望 403, 实际 status=%d body=%s", status, raw)
		}

		// 正向充值：资金敏感操作需携带操作者当前口令。
		status, raw = req(t, http.MethodPost, base+path, adminToken, map[string]any{
			"Amount": rechargeAmount, "Password": adminNewPassword,
		})
		if status != http.StatusOK {
			t.Fatalf("recharge status=%d body=%s", status, raw)
		}
		decodeResp(t, raw)
	}

	// 3) admin 建渠道（base_url 指向 mock 上游），断言可路由（初始 HEALTHY）
	var channelID int64
	{
		status, raw := req(t, http.MethodPost, base+"/api/v1/admin/channels", adminToken, map[string]any{
			"Name": "mock-provider", "Protocol": "openai-compat", "BaseURL": mock.URL,
			"Tags": map[string]string{"provider": "domestic"},
		})
		if status != http.StatusOK {
			t.Fatalf("create channel status=%d body=%s", status, raw)
		}
		ar := decodeResp(t, raw)
		var ch struct {
			ID string `json:"ID"`
		}
		if err := json.Unmarshal(ar.Data, &ch); err != nil {
			t.Fatalf("decode channel: %v", err)
		}
		id, err := strconv.ParseInt(ch.ID, 10, 64)
		if err != nil {
			t.Fatalf("parse channel id %q: %v", ch.ID, err)
		}
		channelID = id
	}

	// 4) admin 建对外模型(qw-max，sale_rates 1.0/2.0) 与语义标签 {provider:domestic}；
	//    随后为渠道绑定内部模型（channel_models：internal qwen-max + cost 与 sale 相同）
	var tagID int64
	var extModelID int64
	{
		rates := map[string]float64{"input": 1.0, "output": 2.0, "cache_read": 1.0, "cache_write": 1.0, "reasoning": 1.0}
		status, raw := req(t, http.MethodPost, base+"/api/v1/admin/models", adminToken, map[string]any{
			"ExternalName": "qw-max", "SaleRates": rates,
		})
		if status != http.StatusOK {
			t.Fatalf("create model status=%d body=%s", status, raw)
		}
		ar := decodeResp(t, raw)
		var em struct {
			ID string `json:"ID"`
		}
		if err := json.Unmarshal(ar.Data, &em); err != nil {
			t.Fatalf("decode external model: %v", err)
		}
		id, err := strconv.ParseInt(em.ID, 10, 64)
		if err != nil {
			t.Fatalf("parse ext model id %q: %v", em.ID, err)
		}
		extModelID = id
	}
	{
		path := fmt.Sprintf("/api/v1/admin/channels/%d/models", channelID)
		status, raw := req(t, http.MethodPost, base+path, adminToken, map[string]any{
			"InternalModelID": "qwen-max", "ExternalModelID": strconv.FormatInt(extModelID, 10),
			"CostRates": map[string]float64{"input": 1.0, "output": 2.0, "cache_read": 1.0, "cache_write": 1.0, "reasoning": 1.0},
		})
		if status != http.StatusOK {
			t.Fatalf("create channel model status=%d body=%s", status, raw)
		}
		decodeResp(t, raw)
	}
	{
		status, raw := req(t, http.MethodPost, base+"/api/v1/admin/tags", adminToken, map[string]any{
			"Name": "dp", "KVPairs": map[string]string{"provider": "domestic"},
		})
		if status != http.StatusOK {
			t.Fatalf("create tag status=%d body=%s", status, raw)
		}
		ar := decodeResp(t, raw)
		var tg struct {
			ID string `json:"ID"`
		}
		if err := json.Unmarshal(ar.Data, &tg); err != nil {
			t.Fatalf("decode tag: %v", err)
		}
		id, err := strconv.ParseInt(tg.ID, 10, 64)
		if err != nil {
			t.Fatalf("parse tag id %q: %v", tg.ID, err)
		}
		tagID = id
	}

	// 5) dev 登录 → 创建令牌（带标签）记明文；查看密钥需 X-Current-Password 二次验证
	var devPlain string
	var devToken string
	var devTokenID string
	{
		devToken = login(t, base, "dev1", "Dev@123456")
		status, raw := req(t, http.MethodPost, base+"/api/v1/dev/tokens", devToken, map[string]any{
			"DisplayName": "e2e", "TagID": strconv.FormatInt(tagID, 10),
		})
		if status != http.StatusOK {
			t.Fatalf("create dev token status=%d body=%s", status, raw)
		}
		ar := decodeResp(t, raw)
		var tk struct {
			Plain string `json:"Plain"`
		}
		if err := json.Unmarshal(ar.Data, &tk); err != nil {
			t.Fatalf("decode token: %v", err)
		}
		if tk.Plain == "" {
			t.Fatal("令牌明文为空")
		}
		devPlain = tk.Plain

		// 负向断言：不带 X-Current-Password 调用查看密钥 → 403。
		status, raw = req(t, http.MethodGet, base+"/api/v1/dev/tokens", devToken, nil)
		if status != http.StatusOK {
			t.Fatalf("list dev tokens status=%d body=%s", status, raw)
		}
		ar = decodeResp(t, raw)
		var tl struct {
			List []struct {
				ID string `json:"ID"`
			} `json:"list"`
		}
		if err := json.Unmarshal(ar.Data, &tl); err != nil || len(tl.List) == 0 {
			t.Fatalf("decode dev token list: %v", err)
		}
		devTokenID = tl.List[0].ID
		status, raw = req(t, http.MethodGet, base+"/api/v1/dev/tokens/"+devTokenID+"/secret", devToken, nil)
		if status != http.StatusForbidden {
			t.Fatalf("secret without X-Current-Password 期望 403, 实际 status=%d body=%s", status, raw)
		}
		// 正向：携带当前口令可查看明文密钥。
		r2, err := http.NewRequest(http.MethodGet, base+"/api/v1/dev/tokens/"+devTokenID+"/secret", nil)
		if err != nil {
			t.Fatalf("new secret request: %v", err)
		}
		r2.Header.Set("Authorization", "Bearer "+devToken)
		r2.Header.Set("X-Current-Password", "Dev@123456")
		resp, err := http.DefaultClient.Do(r2)
		if err != nil {
			t.Fatalf("do secret request: %v", err)
		}
		defer resp.Body.Close()
		secretRaw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("secret with password status=%d body=%s", resp.StatusCode, secretRaw)
		}
		secretAr := decodeResp(t, secretRaw)
		var sk struct {
			Plain string `json:"Plain"`
		}
		if err := json.Unmarshal(secretAr.Data, &sk); err != nil || sk.Plain == "" {
			t.Fatalf("decode secret: %v raw=%s", err, secretRaw)
		}
		if sk.Plain != devPlain {
			t.Fatalf("secret 明文与创建时不符: want %s got %s", devPlain, sk.Plain)
		}
	}

	// 6) 网关转发：/v1/chat/completions → 200、无 X-Channel* 头、body 有 choices
	{
		status, hdr, raw := rawReq(t, http.MethodPost, base+"/v1/chat/completions", devPlain, map[string]any{
			"model": "qw-max", "messages": []map[string]string{{"role": "user", "content": "你好"}},
		})
		if status != http.StatusOK {
			t.Fatalf("gateway status=%d body=%s", status, raw)
		}
		// 渠道透明：任何 X-Channel* 头都不应泄漏到下游。
		for k := range hdr {
			if strings.HasPrefix(strings.ToLower(k), "x-channel") {
				t.Fatalf("网关泄漏渠道头 %s=%s", k, hdr.Get(k))
			}
		}
		var cc struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &cc); err != nil {
			t.Fatalf("网关响应非 openai body: %s err=%v", raw, err)
		}
		if cc.ID == "" {
			t.Fatalf("网关响应缺少 id(choices): %s", raw)
		}
	}

	// 7) dev billings → 恰好 1 条 completed，credits 命中 0.2368*时段系数
	var credits float64
	{
		status, raw := req(t, http.MethodGet, base+"/api/v1/dev/billings", devToken, nil)
		if status != http.StatusOK {
			t.Fatalf("list billings status=%d body=%s", status, raw)
		}
		ar := decodeResp(t, raw)
		var d struct {
			Total int64 `json:"total"`
			List  []struct {
				Status          string  `json:"Status"`
				CreditsConsumed float64 `json:"CreditsConsumed"`
				Model           string  `json:"ExternalModel"`
				PricingMode     string  `json:"PricingMode"`
			} `json:"list"`
		}
		if err := json.Unmarshal(ar.Data, &d); err != nil {
			t.Fatalf("decode billings: %v", err)
		}
		if d.Total != 1 || len(d.List) != 1 {
			t.Fatalf("应恰好 1 条账单, 实际 total=%d len=%d", d.Total, len(d.List))
		}
		b := d.List[0]
		if b.Status != "completed" {
			t.Fatalf("账单状态应为 completed, 实际 %s", b.Status)
		}
		if b.Model != "qw-max" || b.PricingMode != "sale" {
			t.Fatalf("账单 model/pricing_mode 不符: %+v", b)
		}
		credits = b.CreditsConsumed
		// 1234*1.0 + 567*2.0 = 2368，×时段系数/10000；时段系数 ∈ {0.5,1.0,1.5}
		ok := false
		for _, k := range []float64{0.5, 1.0, 1.5} {
			want := 0.2368 * k
			if math.Abs(credits-want) < 1e-6 {
				ok = true
				break
			}
		}
		if !ok {
			t.Fatalf("credits=%v 不在预期集合 0.2368*{0.5,1.0,1.5} 内", credits)
		}
	}

	// 8) dev wallet → 余额 = 原充值 - credits
	{
		status, raw := req(t, http.MethodGet, base+"/api/v1/dev/wallet", devToken, nil)
		if status != http.StatusOK {
			t.Fatalf("get wallet status=%d body=%s", status, raw)
		}
		ar := decodeResp(t, raw)
		var d struct {
			Balance float64 `json:"balance"`
		}
		if err := json.Unmarshal(ar.Data, &d); err != nil {
			t.Fatalf("decode wallet: %v", err)
		}
		want := rechargeAmount - credits
		if math.Abs(d.Balance-want) > 1e-6 {
			t.Fatalf("余额=%v 与期望 %v(充值%v-扣%v) 不符", d.Balance, want, rechargeAmount, credits)
		}
	}

	// 9) 负路径 A：错误标签令牌 → 网关 503（无匹配渠道）
	{
		var wrongTagID int64
		{
			status, raw := req(t, http.MethodPost, base+"/api/v1/admin/tags", adminToken, map[string]any{
				"Name": "overseas", "KVPairs": map[string]string{"provider": "overseas"},
			})
			if status != http.StatusOK {
				t.Fatalf("create wrong tag status=%d body=%s", status, raw)
			}
			ar := decodeResp(t, raw)
			var tg struct {
				ID string `json:"ID"`
			}
			if err := json.Unmarshal(ar.Data, &tg); err != nil {
				t.Fatalf("decode wrong tag: %v", err)
			}
			id, err := strconv.ParseInt(tg.ID, 10, 64)
			if err != nil {
				t.Fatalf("parse wrong tag id %q: %v", tg.ID, err)
			}
			wrongTagID = id
		}
		var wrongPlain string
		{
			status, raw := req(t, http.MethodPost, base+"/api/v1/dev/tokens", devToken, map[string]any{
				"DisplayName": "wrong-tag", "TagID": strconv.FormatInt(wrongTagID, 10),
			})
			if status != http.StatusOK {
				t.Fatalf("create wrong token status=%d body=%s", status, raw)
			}
			ar := decodeResp(t, raw)
			var tk struct {
				Plain string `json:"Plain"`
			}
			if err := json.Unmarshal(ar.Data, &tk); err != nil {
				t.Fatalf("decode wrong token: %v", err)
			}
			wrongPlain = tk.Plain
		}
		status, raw := req(t, http.MethodPost, base+"/v1/chat/completions", wrongPlain, map[string]any{
			"model": "qw-max", "messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusServiceUnavailable {
			t.Fatalf("负路径A期望503, 实际 status=%d body=%s", status, raw)
		}
	}

	// 10) 负路径 B：余额不足 → 网关 402（把 dev 余额调到极低，留少量避免 adjust 需余额充足）
	{
		var bal float64
		{
			status, raw := req(t, http.MethodGet, base+"/api/v1/dev/wallet", devToken, nil)
			if status != http.StatusOK {
				t.Fatalf("get wallet pre-adjust status=%d body=%s", status, raw)
			}
			ar := decodeResp(t, raw)
			var d struct {
				Balance float64 `json:"balance"`
			}
			if err := json.Unmarshal(ar.Data, &d); err != nil {
				t.Fatalf("decode wallet pre-adjust: %v", err)
			}
			bal = d.Balance
		}
		// 调低至 0.001（保留极小余额，避免负数 adjust 需余额充足的问题）
		path := fmt.Sprintf("/api/v1/admin/users/%d/wallet/adjust", devID)
		status, raw := req(t, http.MethodPost, base+path, adminToken, map[string]any{
			"Amount": -(bal - 0.001), "Password": adminNewPassword,
		})
		if status != http.StatusOK {
			t.Fatalf("adjust status=%d body=%s", status, raw)
		}
		decodeResp(t, raw)

		status, raw = req(t, http.MethodPost, base+"/v1/chat/completions", devPlain, map[string]any{
			"model": "qw-max", "messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusPaymentRequired {
			t.Fatalf("负路径B期望402, 实际 status=%d body=%s", status, raw)
		}
	}
}

// TestE2E_Security 安全加固回归：单会话（二次登录旧 token 失效）、登出失效、
// TOTP 两步登录闭环、恢复码一次性。依赖 buildTestServer 已装配 sessions/audit/ratelimit/preauth/totp。
func TestE2E_Security(t *testing.T) {
	dsn := testDSN(t)
	ts, cleanup := buildTestServer(t, dsn)
	defer cleanup()
	base := ts.URL

	// 单会话：第二次登录后，第一次签发的 token 因 token_version 不匹配而失效（401）。
	t.Run("single_session", func(t *testing.T) {
		tok1 := loginAdmin(t, base)
		tok2 := loginAdmin(t, base)
		status, raw := apiGet(t, base, tok1, "/api/v1/auth/me")
		if status != http.StatusUnauthorized {
			t.Fatalf("expected old token 401, got %d body=%s", status, raw)
		}
		// 新 token 仍可用
		status, raw = apiGet(t, base, tok2, "/api/v1/auth/me")
		if status != http.StatusOK {
			t.Fatalf("expected new token 200, got %d body=%s", status, raw)
		}
	})

	// 登出后当前 token 立即失效。
	t.Run("logout_invalidates", func(t *testing.T) {
		tok := loginAdmin(t, base)
		status, raw := apiPost(t, base, tok, "/api/v1/auth/logout", nil)
		if status != http.StatusOK {
			t.Fatalf("logout status=%d body=%s", status, raw)
		}
		status, raw = apiGet(t, base, tok, "/api/v1/auth/me")
		if status != http.StatusUnauthorized {
			t.Fatalf("expected 401 after logout, got %d body=%s", status, raw)
		}
	})

	// TOTP 两步登录闭环 + 恢复码一次性。
	t.Run("totp_two_step_login", func(t *testing.T) {
		adminToken := loginAdmin(t, base)

		// 建 developer 用户（后续 TOTP 绑定/两步登录）
		status, raw := req(t, http.MethodPost, base+"/api/v1/admin/users", adminToken, map[string]string{
			"Username": "secdev", "Password": "Secdev@12345", "Role": "DEVELOPER", "PricingMode": "sale",
		})
		if status != http.StatusOK {
			t.Fatalf("create secdev status=%d body=%s", status, raw)
		}
		ar := decodeResp(t, raw)
		var u struct {
			ID string `json:"ID"`
		}
		if err := json.Unmarshal(ar.Data, &u); err != nil {
			t.Fatalf("decode create secdev: %v", err)
		}
		if u.ID == "" {
			t.Fatal("create secdev 返回空 ID")
		}

		// dev 登录 → TOTP setup（拿 base32 secret）；需当前口令二次验证
		devToken := login(t, base, "secdev", "Secdev@12345")
		status, raw = req(t, http.MethodPost, base+"/api/v1/auth/me/totp/setup", devToken, map[string]string{"Password": "Secdev@12345"})
		if status != http.StatusOK {
			t.Fatalf("totp setup status=%d body=%s", status, raw)
		}
		ar = decodeResp(t, raw)
		var st struct {
			Secret string `json:"Secret"`
		}
		if err := json.Unmarshal(ar.Data, &st); err != nil {
			t.Fatalf("decode totp setup: %v", err)
		}
		if st.Secret == "" {
			t.Fatal("totp secret 为空")
		}

		// 用 pquerna 生成动态码并 confirm（返回恢复码）；需当前口令二次验证
		code, err := totp.GenerateCode(st.Secret, time.Now())
		if err != nil {
			t.Fatalf("generate totp code: %v", err)
		}
		status, raw = req(t, http.MethodPost, base+"/api/v1/auth/me/totp/confirm", devToken, map[string]string{"Password": "Secdev@12345", "Code": code})
		if status != http.StatusOK {
			t.Fatalf("totp confirm status=%d body=%s", status, raw)
		}
		ar = decodeResp(t, raw)
		var cf struct {
			RecoveryCodes []string `json:"RecoveryCodes"`
		}
		if err := json.Unmarshal(ar.Data, &cf); err != nil {
			t.Fatalf("decode totp confirm: %v", err)
		}
		if len(cf.RecoveryCodes) == 0 {
			t.Fatal("恢复码为空")
		}

		// 第一步：密码登录 → need_totp + preauth_token（不发 token）
		status, raw = req(t, http.MethodPost, base+"/api/v1/auth/login", "", map[string]string{
			"Username": "secdev", "Password": "Secdev@12345",
		})
		if status != http.StatusOK {
			t.Fatalf("two-step login step1 status=%d body=%s", status, raw)
		}
		ar = decodeResp(t, raw)
		var l1 struct {
			NeedTOTP     bool   `json:"NeedTOTP"`
			PreAuthToken string `json:"PreAuthToken"`
		}
		if err := json.Unmarshal(ar.Data, &l1); err != nil {
			t.Fatalf("decode login step1: %v", err)
		}
		if !l1.NeedTOTP || l1.PreAuthToken == "" {
			t.Fatalf("expected need_totp + preauth_token, got %+v", l1)
		}

		// 第二步：TOTP 动态码 → 成功签发 token，token 可访问 /me
		code2, err := totp.GenerateCode(st.Secret, time.Now())
		if err != nil {
			t.Fatalf("generate totp code2: %v", err)
		}
		status, raw = req(t, http.MethodPost, base+"/api/v1/auth/login/totp", "", map[string]string{
			"PreAuthToken": l1.PreAuthToken, "Code": code2,
		})
		if status != http.StatusOK {
			t.Fatalf("two-step login step2 status=%d body=%s", status, raw)
		}
		ar = decodeResp(t, raw)
		var l2 struct {
			Token string `json:"Token"`
		}
		if err := json.Unmarshal(ar.Data, &l2); err != nil {
			t.Fatalf("decode login step2: %v", err)
		}
		if l2.Token == "" {
			t.Fatal("two-step 登录 token 为空")
		}
		status, raw = apiGet(t, base, l2.Token, "/api/v1/auth/me")
		if status != http.StatusOK {
			t.Fatalf("two-step token /me status=%d body=%s", status, raw)
		}

		// 恢复码一次性：首次使用成功，再次使用同一恢复码失败。
		recovery := cf.RecoveryCodes[0]
		status, raw = req(t, http.MethodPost, base+"/api/v1/auth/login", "", map[string]string{
			"Username": "secdev", "Password": "Secdev@12345",
		})
		if status != http.StatusOK {
			t.Fatalf("recovery login step1 status=%d body=%s", status, raw)
		}
		ar = decodeResp(t, raw)
		var l1b struct {
			PreAuthToken string `json:"PreAuthToken"`
		}
		if err := json.Unmarshal(ar.Data, &l1b); err != nil {
			t.Fatalf("decode recovery step1: %v", err)
		}
		status, raw = req(t, http.MethodPost, base+"/api/v1/auth/login/totp", "", map[string]string{
			"PreAuthToken": l1b.PreAuthToken, "Code": recovery,
		})
		if status != http.StatusOK {
			t.Fatalf("recovery code first use status=%d body=%s", status, raw)
		}
		decodeResp(t, raw)

		// 复用同一恢复码 → 401（一次性）
		status, raw = req(t, http.MethodPost, base+"/api/v1/auth/login", "", map[string]string{
			"Username": "secdev", "Password": "Secdev@12345",
		})
		if status != http.StatusOK {
			t.Fatalf("reuse login step1 status=%d body=%s", status, raw)
		}
		ar = decodeResp(t, raw)
		var l1c struct {
			PreAuthToken string `json:"PreAuthToken"`
		}
		if err := json.Unmarshal(ar.Data, &l1c); err != nil {
			t.Fatalf("decode reuse step1: %v", err)
		}
		status, raw = req(t, http.MethodPost, base+"/api/v1/auth/login/totp", "", map[string]string{
			"PreAuthToken": l1c.PreAuthToken, "Code": recovery,
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("recovery code reuse should 401, got %d body=%s", status, raw)
		}
	})
}
