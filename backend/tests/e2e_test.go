// Package e2e 端到端冒烟测试：覆盖「登录 → 建开发者用户 → 充值 → 建令牌 → 账单查询」与
// 安全闭环（单会话 / 登出失效 / TOTP 两步登录 / 恢复码一次性）。
//
// 说明：原「建渠道 → 网关转发 → 计费扣积分」闭环随转发链迁出灵枢而移除（V2 第一期），
// 该链路由代理侧承接，本仓库不再覆盖。
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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/team/llmgateway/internal/config"
	"github.com/team/llmgateway/internal/db"
	"github.com/team/llmgateway/internal/domain/audit"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/console"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/model"
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

	tagStore := tag.NewStore(d)
	tagSvc := tag.NewService(tagStore)
	tagHandler := tag.NewHandler(tagSvc)
	modelSvc := model.NewService(model.NewStore(d))
	modelHandler := model.NewHandler(modelSvc)

	billStore := billing.NewSqlStore(d)

	consoleAdmin := console.NewAdmin(d, billStore, userIDFrom)
	consoleDev := console.NewDev(billStore, userIDFrom)

	syncStore := sync.NewStore(d)
	syncer := sync.NewSyncer(sync.SyncerConfig{Store: syncStore, SourceURL: cfg.Sync.PriceSourceURL, Interval: time.Duration(cfg.Sync.IntervalMinutes) * time.Minute, Logger: logr})
	syncHandler := sync.NewHandler(syncer, userIDFrom)

	app := &testApp{
		syn: syncer,
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
		},
	}

	srv := server.New(cfg, d, logr, jwtMgr, sessions, auditStore, app.deps)
	ts := httptest.NewServer(srv.Handler())
	cleanup := func() {
		ts.Close()
		_ = d.Close()
	}
	return ts, cleanup
}

// ===== HTTP 辅助 =====

// apiResp 是内部管理/开发端统一响应壳（PascalCase：{Code,Message,RequestID,Data}）。
// 注意：/v1 OpenAI 兼容面不使用此壳，其成功响应保持小写、错误体为 {"error":{...}}。
type apiResp struct {
	Code      int             `json:"Code"`
	Message   string          `json:"Message"`
	RequestID string          `json:"RequestID"`
	Data      json.RawMessage `json:"Data"`
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

// 管理员新建账号强制首登改密（identity.MustChangePassword=true），故 e2e 中由管理员创建的
// dev/secdev 需先改密后才能访问受门禁保护的接口；以下为其改密后的口令，后续登录与
// 二次验证（X-Current-Password / TOTP 的 Password 字段）一律使用新口令。
const devNewPassword = "Dev@1234567"
const secdevNewPassword = "Secdev@1234567"

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

// changePassword 走强制改密白名单路径 /auth/me/password，返回改密后的新 token
// （改密会 bump 会话代数，旧 token 立即失效，故必须使用返回值）。
func changePassword(t *testing.T, base, token, oldPw, newPw string) string {
	t.Helper()
	status, raw := req(t, http.MethodPut, base+"/api/v1/auth/me/password", token, map[string]string{
		"OldPassword": oldPw,
		"NewPassword": newPw,
	})
	if status != http.StatusOK {
		t.Fatalf("change password status=%d body=%s", status, raw)
	}
	ar := decodeResp(t, raw)
	var c struct {
		Token string `json:"Token"`
	}
	if err := json.Unmarshal(ar.Data, &c); err != nil {
		t.Fatalf("decode change password: %v", err)
	}
	if c.Token == "" {
		t.Fatal("change password 返回空 token")
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
		// 管理员新建账号强制首登改密：先改密，后续二次验证与登录一律使用新口令。
		devToken = changePassword(t, base, devToken, "Secdev@12345", secdevNewPassword)
		status, raw = req(t, http.MethodPost, base+"/api/v1/auth/me/totp/setup", devToken, map[string]string{"Password": secdevNewPassword})
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
		status, raw = req(t, http.MethodPost, base+"/api/v1/auth/me/totp/confirm", devToken, map[string]string{"Password": secdevNewPassword, "Code": code})
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
			"Username": "secdev", "Password": secdevNewPassword,
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
			"Username": "secdev", "Password": secdevNewPassword,
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
			"Username": "secdev", "Password": secdevNewPassword,
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
