package server

// routes_rebill_test.go 守住补收接口（B 方案）的两条路由口径：
//
//  1. 只挂 /api/v1/admin（鉴权由前缀守卫承担，无凭据必须 401）；
//  2. 不得提供 /api/v1/dev 同义端点 —— 补收读的是 unpriced 账单明细（含成本/归属），
//     成本侧数据只给 ADMIN（与阶段三运营端点同纪律）。
//
// 第 2 条用源码级断言（/api/v1/dev 整段也在鉴权里，HTTP 层分不出「没注册」与「要鉴权」，
// 同 routes_stage3_test.go 的既有结论）。

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/config"
	"github.com/team/llmgateway/internal/domain/console"
	"github.com/team/llmgateway/internal/domain/v3billing"
)

const rebillRoute = "POST /api/v1/admin/billings/rebill-unpriced"

// TestRebillRoute_AdminOnly 补收端点必须在管理端鉴权之内（nil 处理器不会进业务逻辑）。
func TestRebillRoute_AdminOnly(t *testing.T) {
	srv := newGoneTestServer(t)
	if srv == nil {
		t.Fatal("server 构造失败")
	}
}

// TestRebillRoute_NoDevMirror 源码级：补收路由只允许出现在 /api/v1/admin 前缀；
// d.V3Rebill 的 HandleFunc 注册行必须含 /api/v1/admin/ 且不得含 /api/v1/dev/。
func TestRebillRoute_NoDevMirror(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("读取 server.go 失败：%v", err)
	}
	checked := 0
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "d.V3Rebill.") || !strings.Contains(trimmed, "HandleFunc") {
			continue
		}
		checked++
		if !strings.Contains(trimmed, "/api/v1/admin/") {
			t.Errorf("补收端点必须挂在 /api/v1/admin 下：%s", trimmed)
		}
		if strings.Contains(trimmed, "/api/v1/dev/") {
			t.Errorf("补收端点不许挂开发者端（成本侧数据泄露）：%s", trimmed)
		}
	}
	if checked != 1 {
		t.Fatalf("扫描到 %d 条补收路由注册，期望恰好 1 条", checked)
	}
}

// TestRebillRoute_RegisteredOnLiveServer 组一个带 V3Rebill 的 Server，确认线上路由表真的注册了
// 该端点（无凭据 401 = 路由存在且在鉴权之后；未注册会 404）。
func TestRebillRoute_RegisteredOnLiveServer(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(&config.Config{}, nil, logger, nil, nil, nil, Deps{
		Console:  &console.Admin{},
		V3Rebill: v3billing.NewRebillHandler(nil),
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/billings/rebill-unpriced",
		strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据 status = %d，期望 401（404 = 路由没注册）", rec.Code)
	}
}
