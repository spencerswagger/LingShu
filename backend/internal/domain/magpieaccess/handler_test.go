package magpieaccess

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestSigner() *Signer {
	key := make([]byte, 32)
	for i := range key {
		key[i] = 9
	}
	return NewSigner(key)
}

func newTestHandler(t *testing.T, role string, enabled bool) *Handler {
	t.Helper()
	return NewHandler(Options{
		Signer:  newTestSigner(),
		Enabled: func() bool { return enabled },
		UserID:  func(context.Context) (int64, bool) { return 7, true },
		Role:    func(context.Context) (string, bool) { return role, true },
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func post(h *Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.HandleIssue(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/ops/magpie-access", nil))
	return rec
}

func verify(h *Handler, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/internal/magpie-access/verify", nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.HandleVerify(rec, req)
	return rec
}

func cookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName {
			return c
		}
	}
	t.Fatalf("响应里没有 %s cookie：%v", CookieName, rec.Header().Values("Set-Cookie"))
	return nil
}

func TestIssue_SetsHttpOnlyCookieAndRelativeURL(t *testing.T) {
	h := newTestHandler(t, "ADMIN", true)
	rec := post(h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	c := cookieFrom(t, rec)
	if c.Value == "" || !strings.HasPrefix(c.Value, "v1.") {
		t.Fatalf("cookie 值不是票据：%q", c.Value)
	}
	if !c.HttpOnly {
		t.Fatal("票据 cookie 必须 HttpOnly（不许 JS 读到）")
	}
	if c.Path != CookiePath {
		t.Fatalf("cookie Path = %q，期望 %q（只跟着管理面路径走）", c.Path, CookiePath)
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v，期望 Lax", c.SameSite)
	}
	if c.MaxAge != int(DefaultTTL.Seconds()) {
		t.Fatalf("MaxAge = %d，期望 %d", c.MaxAge, int(DefaultTTL.Seconds()))
	}
	var body struct {
		Code int
		Data accessView
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if body.Data.Url != "/magpie/" {
		t.Fatalf("Url = %q，期望同源相对路径 /magpie/（URL 里不许出现任何 key）", body.Data.Url)
	}
	if strings.Contains(body.Data.Url, "k=") || strings.Contains(body.Data.Url, "t=") {
		t.Fatalf("Url 里带了凭据：%q", body.Data.Url)
	}
	// 响应体里也不许回显票据（只能靠 Set-Cookie）。
	if strings.Contains(rec.Body.String(), c.Value) {
		t.Fatal("响应体回显了票据")
	}
}

// 非管理员：处理器自身也要拦（路由守卫是外面那层，换一处挂载不该漏）。
func TestIssue_NonAdminForbidden(t *testing.T) {
	for _, role := range []string{"DEVELOPER", "", "admin"} {
		h := newTestHandler(t, role, true)
		rec := post(h)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("role=%q status = %d，期望 403", role, rec.Code)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Fatalf("role=%q 被拒时仍种了 cookie", role)
		}
	}
}

// 代理侧未接入：503 + reason，前端据此把按钮置灰（而不是给一个点开 502 的入口）。
func TestIssue_DisabledReturns503WithReason(t *testing.T) {
	h := newTestHandler(t, "ADMIN", false)
	rec := post(h)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d，期望 503", rec.Code)
	}
	var body struct{ Reason string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Reason != "magpie_admin_unconfigured" {
		t.Fatalf("Reason = %q，期望 magpie_admin_unconfigured", body.Reason)
	}
}

func TestVerify_CookieAndQuery(t *testing.T) {
	h := newTestHandler(t, "ADMIN", true)
	tok := cookieFrom(t, post(h)).Value

	rec := verify(h, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: tok}) })
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cookie 验证 status = %d，期望 204", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("204 不应带响应体，实际 %q", rec.Body.String())
	}

	rec = verify(h, func(r *http.Request) {
		r.URL.RawQuery = "t=" + tok
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("?t= 验证 status = %d，期望 204", rec.Code)
	}
}

func TestVerify_RejectsJunk(t *testing.T) {
	h := newTestHandler(t, "ADMIN", true)
	cases := map[string]func(*http.Request){
		"无票据":       nil,
		"空 t":       func(r *http.Request) { r.URL.RawQuery = "t=" },
		"乱码 t":      func(r *http.Request) { r.URL.RawQuery = "t=abc" },
		"畸形 t":      func(r *http.Request) { r.URL.RawQuery = "t=v1.abc.def" },
		"乱码 cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: "x"}) },
	}
	for name, mutate := range cases {
		if rec := verify(h, mutate); rec.Code != http.StatusForbidden {
			t.Fatalf("%s：status = %d，期望 403", name, rec.Code)
		}
	}
	// nil 处理器（未装配）也不能放行。
	var nilH *Handler
	rec := httptest.NewRecorder()
	nilH.HandleVerify(rec, httptest.NewRequest(http.MethodGet, "/internal/magpie-access/verify?t=x", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("未装配时 status = %d，期望 403", rec.Code)
	}
}

// 校验端点的应答不得被缓存：缓存住等于把门开着。
func TestVerify_NoStore(t *testing.T) {
	h := newTestHandler(t, "ADMIN", true)
	if cc := verify(h, nil).Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control = %q，期望含 no-store", cc)
	}
}

// PublicBase（MAGPIE_ADMIN_PUBLIC_URL）非空时拼绝对地址，尾斜杠不多不少。
func TestIssue_PublicBasePrefix(t *testing.T) {
	h := NewHandler(Options{
		Signer:     newTestSigner(),
		Enabled:    func() bool { return true },
		PublicBase: "https://lingshu.example.com/",
		UserID:     func(context.Context) (int64, bool) { return 1, true },
		Role:       func(context.Context) (string, bool) { return "ADMIN", true },
	})
	var body struct{ Data accessView }
	_ = json.Unmarshal(post(h).Body.Bytes(), &body)
	if body.Data.Url != "https://lingshu.example.com/magpie/" {
		t.Fatalf("Url = %q，拼接不对", body.Data.Url)
	}
}
