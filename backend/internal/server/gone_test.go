package server

// gone_test.go 守住「已停用接口」的两条口径（Owl II 评审 P1-2 + P2）：
//
//  1. 公开入口（/v1、/v1beta、开发端）返 410，文案**通用**——不含内部组件名、不提迁移去向；
//  2. 管理端渠道族 410 仍然存在，且必须经过鉴权（无凭据 → 401，不会因为「已停用」而放宽）。
//
// 这两条以前零覆盖（e2e 只剩登录/充值/令牌/账单），后人很容易把 /v1 改回明细文案或把 410 挪出鉴权。

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/config"
	"github.com/team/llmgateway/internal/domain/console"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/domain/sync"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// newGoneTestServer 组一个只用于「路由表口径」的 Server：处理器全为零值（不会被调用到），
// 目的是让 routes() 真实跑一遍，从而断言的是**线上那条路由**而不是测试里另写一份。
func newGoneTestServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(&config.Config{}, nil, logger, nil, nil, nil, Deps{
		Identity:     &identity.Handler{},
		AdminUser:    &identity.AdminUserHandler{},
		Credit:       &identity.CreditHandler{},
		TokenAdmin:   &identity.TokenHandler{},
		TokenDev:     &identity.TokenHandler{},
		Model:        &model.Handler{},
		Announcement: &identity.AnnouncementHandler{},
		Console:      &console.Admin{},
		DevConsole:   &console.Dev{},
		SyncAdmin:    &sync.Handler{},
	})
}

// TestPublicGoneRoutes 公开入口一律 410 + 通用文案。
func TestPublicGoneRoutes(t *testing.T) {
	srv := newGoneTestServer(t)
	cases := []struct{ method, path string }{
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodPost, "/v1/responses"},
		{http.MethodPost, "/v1/messages"},
		{http.MethodGet, "/v1/models"},
		{http.MethodPost, "/v1beta/models/gemini-2.0-flash:generateContent"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))

			if rec.Code != http.StatusGone {
				t.Fatalf("status=%d, want 410; body=%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"Code":41001`) {
				t.Fatalf("业务码应为 %d；body=%s", resp.CodeGone, body)
			}
			// 公开面：不许出现内部组件名、迁移去向、也不许暗示「换个地址还能用」。
			for _, banned := range []string{"magpie", "代理侧", "迁出", "渠道"} {
				if strings.Contains(body, banned) {
					t.Fatalf("公开 410 文案不得包含 %q；body=%s", banned, body)
				}
			}
			if !strings.Contains(body, "请查阅最新接口文档") {
				t.Fatalf("公开 410 文案应为通用口径；body=%s", body)
			}
		})
	}
}

// TestAdminGoneRoutesRequireAuth 管理端渠道族 410 存在，但不能绕过鉴权。
func TestAdminGoneRoutesRequireAuth(t *testing.T) {
	srv := newGoneTestServer(t)
	cases := []string{
		"/api/v1/admin/channels",
		"/api/v1/admin/channel-models",
		"/api/v1/admin/channel-protocols",
		"/api/v1/admin/auto-models",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			// 无凭据必须是 401：410 不能挂在鉴权之外（否则等于公开告知「这些接口存在」）。
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("无凭据访问 %s status=%d, want 401; body=%s", path, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestGoneHandlersWording 两套文案的边界：管理端可说明去向，公开面只给通用话术。
func TestGoneHandlersWording(t *testing.T) {
	t.Run("管理端口径", func(t *testing.T) {
		rec := httptest.NewRecorder()
		goneHandler(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/channels", nil))
		if rec.Code != http.StatusGone {
			t.Fatalf("status=%d, want 410", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "已停用") {
			t.Fatalf("管理端口径应说明停用；body=%s", rec.Body.String())
		}
	})
	t.Run("公开口径", func(t *testing.T) {
		rec := httptest.NewRecorder()
		gonePublicHandler(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
		if rec.Code != http.StatusGone {
			t.Fatalf("status=%d, want 410", rec.Code)
		}
		body := rec.Body.String()
		for _, banned := range []string{"magpie", "代理侧", "迁出", "渠道"} {
			if strings.Contains(body, banned) {
				t.Fatalf("公开文案不得包含 %q；body=%s", banned, body)
			}
		}
	})
}
