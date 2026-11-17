package server

// routes_stage3_test.go 守住阶段三两个新接口族的两条路由口径（Spec §6.3）：
//
//  1. 只挂在 /api/v1/admin 下 —— 管理端前缀由 WithAuth(RoleAdmin) 守卫，无凭据必须 401；
//  2. **不得**提供 /api/v1/dev 同义端点：成本侧数据（订阅价、分摊、拓扑供应商）只给 ADMIN，
//     开发者端一旦有入口，等于把上游渠道结构与采购成本漏给外部租户。
//
// 第 2 条只能用源码级断言：/api/v1/dev 整段也包在鉴权中间件里，未注册路径同样先 401，
// 用 HTTP 层根本分不出「没注册」和「注册了但要鉴权」（我先写成 404 断言，实测全红才发现）。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestStage3Routes_AdminOnly(t *testing.T) {
	srv := newGoneTestServer(t) // 处理器为零值：这里只验路由表与守卫，不进入业务逻辑
	adminPaths := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/topology"},
		{http.MethodPost, "/api/v1/admin/topology/sync"},
		{http.MethodPut, "/api/v1/admin/topology/links"},
		{http.MethodGet, "/api/v1/admin/subscriptions"},
		{http.MethodGet, "/api/v1/admin/subscriptions/form-options"},
		{http.MethodPost, "/api/v1/admin/subscriptions"},
		{http.MethodPost, "/api/v1/admin/subscriptions/amortize/run"},
	}
	for _, c := range adminPaths {
		t.Run("admin "+c.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s 无凭据 status = %d，期望 401（不许绕过鉴权）", c.method, c.path, rec.Code)
			}
		})
	}
}

// 逐行扫 routes()：凡是注册 Topology / Subscriptions 处理器的地方，路径必须以 /api/v1/admin/ 开头。
// 违反即说明有人把它挂到了 /api/v1/dev 或公开面（成本侧数据泄露）。
func TestStage3Routes_NoDevMirror(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("读取 server.go 失败：%v", err)
	}
	checked := 0
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if !(strings.Contains(trimmed, "d.Topology.") || strings.Contains(trimmed, "d.Subscriptions.")) {
			continue
		}
		if !strings.Contains(trimmed, "HandleFunc") { // 跳过注释与字段赋值
			continue
		}
		checked++
		if !strings.Contains(trimmed, "/api/v1/admin/") {
			t.Errorf("阶段三端点必须挂在 /api/v1/admin 下：%s", trimmed)
		}
	}
	// 防空转：路由表里只应剩这 7 条（T1/T2/T3 + S1/S2/S6/S7）；数量变了说明有人加了端点，
	// 那也该重新按 Spec 复核一遍权限与成本口径。
	if checked != 7 {
		t.Fatalf("扫描到 %d 条阶段三路由注册，期望 7 条（T1/T2/T3/S1/S2/S6/S7）", checked)
	}
}
