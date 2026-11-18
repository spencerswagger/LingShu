package server

// magpie_access_route_test.go 守住代理侧管理面跳转的两条路由口径：
//
//  1. 签发端点挂在 /api/v1/admin 下 —— 无凭据必须 401（不许绕过鉴权）；
//  2. 校验端点**不在** /api 前缀下、也不走鉴权中间件 —— 未带票据必须是 403 而不是 401/404：
//     401 说明它被鉴权中间件挡住了（nginx 的 auth_request 拿不到 Authorization，管理员会被误伤），
//     404 说明路由没注册（nginx 会把它当「后端挂了」）。
//
// 第 2 条用 HTTP 层就能分辨（401 vs 403），不需要源码级断言。

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/config"
	"github.com/team/llmgateway/internal/domain/magpieaccess"
)

func newMagpieAccessServer(t *testing.T) (*Server, *magpieaccess.Signer) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = 42
	}
	signer := magpieaccess.NewSigner(key)
	h := magpieaccess.NewHandler(magpieaccess.Options{
		Signer:  signer,
		Enabled: func() bool { return true },
		UserID:  func(context.Context) (int64, bool) { return 1, true },
		Role:    func(context.Context) (string, bool) { return "ADMIN", true },
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(&config.Config{}, nil, logger, nil, nil, nil, Deps{MagpieAccess: h}), signer
}

func TestMagpieAccess_IssueRequiresAdmin(t *testing.T) {
	srv, _ := newMagpieAccessServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/ops/magpie-access", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据 status = %d，期望 401（签发端点必须在 RoleAdmin 守卫内）", rec.Code)
	}
}

func TestMagpieAccess_VerifyOutsideAuth(t *testing.T) {
	srv, signer := newMagpieAccessServer(t)

	// 无票据：403（不是 401 —— 它本来就不该要登录态；也不是 404 —— 路由必须存在）。
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal/magpie-access/verify", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("无票据 status = %d，期望 403", rec.Code)
	}

	// 有效票据：204（nginx 的 auth_request 靠 2xx 放行）。
	tok, _, err := signer.Issue(1, "ADMIN", magpieaccess.DefaultTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/internal/magpie-access/verify", nil)
	req.AddCookie(&http.Cookie{Name: magpieaccess.CookieName, Value: tok})
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("有效票据 status = %d，期望 204", rec.Code)
	}
}

// 未装配（代理侧没接入 / 没密钥）时校验端点也必须存在并拒绝，而不是 404。
func TestMagpieAccess_VerifyRegisteredWhenNotWired(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(&config.Config{}, nil, logger, nil, nil, nil, Deps{})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal/magpie-access/verify?t=x", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("未装配时 status = %d，期望 403（不能是 404）", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "t=x") {
		t.Fatal("响应体回显了票据")
	}
}
