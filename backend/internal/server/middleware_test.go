package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/pkg/jwtx"
	"github.com/team/llmgateway/internal/pkg/resp"
	"github.com/team/llmgateway/internal/pkg/session"
)

// newTestJWTMgr 构造可用于签/解 token 的 jwtx.Manager。
func newTestJWTMgr(t *testing.T) *jwtx.Manager {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	dir := t.TempDir()
	privPath := filepath.Join(dir, "priv.pem")
	pubPath := filepath.Join(dir, "pub.pem")
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("pub: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	_ = os.WriteFile(privPath, privPEM, 0o600)
	_ = os.WriteFile(pubPath, pubPEM, 0o600)
	mgr, err := jwtx.NewManager(privPath, pubPath, 60)
	if err != nil {
		t.Fatalf("new mgr: %v", err)
	}
	return mgr
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) resp.Body {
	t.Helper()
	var body resp.Body
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

// newActiveSessionRegistry 构造版本一致、状态 ACTIVE 的会话注册表。
func newActiveSessionRegistry(t *testing.T) *session.Registry {
	t.Helper()
	return session.NewRegistry(func(ctx context.Context, userID int64) (session.Entry, error) {
		return session.Entry{Version: 1, Status: "ACTIVE"}, nil
	})
}

// buildAuthHandler 构造带 WithAuth(ADMIN) 的保护 handler，成功则响应 200。
func buildAuthHandler(mgr *jwtx.Manager, reg *session.Registry) http.Handler {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := UserIDFrom(r.Context())
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]int64{"uid": id})
	})
	return WithAuth(mgr, reg, "ADMIN")(h)
}

func TestWithAuth_NoToken(t *testing.T) {
	mgr := newTestJWTMgr(t)
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	buildAuthHandler(mgr, newActiveSessionRegistry(t)).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if body := decodeBody(t, w); body.Code != resp.CodeUnauthorized {
		t.Fatalf("expected 40101, got %d", body.Code)
	}
}

func TestWithAuth_WrongRole(t *testing.T) {
	mgr := newTestJWTMgr(t)
	token, err := mgr.Sign(9, "dev", "DEVELOPER", 1)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	buildAuthHandler(mgr, newActiveSessionRegistry(t)).ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if body := decodeBody(t, w); body.Code != resp.CodeForbidden {
		t.Fatalf("expected 40301, got %d", body.Code)
	}
}

func TestWithAuth_Allowed(t *testing.T) {
	mgr := newTestJWTMgr(t)
	token, err := mgr.Sign(7, "root", "ADMIN", 1)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	buildAuthHandler(mgr, newActiveSessionRegistry(t)).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if body := decodeBody(t, w); body.Code != resp.CodeOK {
		t.Fatalf("expected 0, got %d", body.Code)
	}
}

func TestWithAuth_InvalidToken(t *testing.T) {
	mgr := newTestJWTMgr(t)
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("Authorization", "Bearer not-a-jwt")
	w := httptest.NewRecorder()
	buildAuthHandler(mgr, newActiveSessionRegistry(t)).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestWithAuth_RejectsStaleVersion(t *testing.T) {
	mgr := newTestJWTMgr(t)
	reg := session.NewRegistry(func(ctx context.Context, userID int64) (session.Entry, error) {
		return session.Entry{Version: 9, Status: "ACTIVE"}, nil
	})
	tok, _ := mgr.Sign(1, "admin", "ADMIN", 2) // 旧版本
	h := WithAuth(mgr, reg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestWithAuth_MustChangeWhitelist(t *testing.T) {
	mgr := newTestJWTMgr(t)
	reg := session.NewRegistry(func(ctx context.Context, userID int64) (session.Entry, error) {
		return session.Entry{Version: 1, Status: "ACTIVE", MustChange: true}, nil
	})
	tok, _ := mgr.Sign(1, "admin", "ADMIN", 1)
	// 访问非白名单路径 → 403 业务码
	h := WithAuth(mgr, reg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest("GET", "/api/v1/admin/users", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if body := decodeBody(t, rec); body.Code != resp.CodeMustChangePassword {
		t.Fatalf("expected 40302, got %d", body.Code)
	}
	// 访问改密路径 → 放行
	req2 := httptest.NewRequest("PUT", "/api/v1/auth/me/password", nil)
	req2.Header.Set("Authorization", "Bearer "+tok)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}
}

func TestUserIDFrom_Empty(t *testing.T) {
	if _, ok := UserIDFrom(t.Context()); ok {
		t.Fatal("expected no user id in empty ctx")
	}
}

// T1：客户端 x-request-id 必须经过白名单校验——超长/非法值一律替换为服务端 id。
// 该值会经上下文最终落入 audit_logs.request_id，未校验则客户端可让审计写入失败。
func TestWithRequestID_ValidatesClientID(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		keepRaw bool // true 表示期望客户端值被原样沿用
	}{
		{"empty", "", false},
		{"overlong", strings.Repeat("A", 300), false},
		{"exactly 65", strings.Repeat("a", 65), false},
		{"illegal chars", "bad id", false},
		{"control chars", "abc\ndef", false},
		{"valid", "client-req-123", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			h := WithRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = resp.RequestID(r)
				_, _ = w.Write([]byte(got))
			}))
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
			if tc.header != "" {
				req.Header.Set("x-request-id", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if got == "" {
				t.Fatal("request id 为空，未生成服务端 id")
			}
			if len(got) > 64 {
				t.Fatalf("request id 超长未收敛: len=%d", len(got))
			}
			if !requestIDRe.MatchString(got) {
				t.Fatalf("request id 不符合白名单: %q", got)
			}
			if rec.Header().Get("x-request-id") != got {
				t.Fatalf("响应头 id 与上下文不一致: header=%q ctx=%q", rec.Header().Get("x-request-id"), got)
			}
			if tc.keepRaw {
				if got != tc.header {
					t.Fatalf("合法客户端 id 应被沿用: want %q got %q", tc.header, got)
				}
			} else if got == tc.header {
				t.Fatalf("非法客户端 id 不应被透传: %q", got)
			}
		})
	}
}

// T2/T5：用真实的 middlewareChain 断言访问日志的 requestId 非空。
// 这钉住 WithRequestID 必须处于最外层——若退回内层，WithLogging 读到的 id 恒为空。
func TestMiddlewareChain_LogsNonEmptyRequestID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	s := &Server{logger: logger}

	chain := s.middlewareChain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	headerID := rec.Header().Get("x-request-id")
	if headerID == "" {
		t.Fatal("响应头 x-request-id 为空")
	}

	var logLine struct {
		Msg       string `json:"msg"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &logLine); err != nil {
		t.Fatalf("解析访问日志失败: %v, raw=%q", err, buf.String())
	}
	if logLine.Msg != "http request" {
		t.Fatalf("非访问日志: %q", buf.String())
	}
	if logLine.RequestID == "" {
		t.Fatalf("访问日志 requestId 为空（WithRequestID 未处于最外层）: %s", buf.String())
	}
	if logLine.RequestID != headerID {
		t.Fatalf("访问日志 requestId 与响应头不一致: log=%q header=%q", logLine.RequestID, headerID)
	}
}

// U2：panic 的请求也必须出现在访问日志里（status=500），且 panic 日志能定位到接口。
// 用与 Handler 相同的嵌套关系（WithRecover 外层、WithLogging 内层）复刻。
func TestWithLogging_RecordsPanicInAccessLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	chain := WithRecover(logger)(WithLogging(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/boom", nil)
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}

	var accessLog, panicLog map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("解析日志失败: %v, line=%q", err, line)
		}
		switch m["msg"] {
		case "http request":
			accessLog = m
		case "panic recovered":
			panicLog = m
		}
	}

	if accessLog == nil {
		t.Fatalf("panic 请求未写入访问日志: %s", buf.String())
	}
	if accessLog["status"] != float64(http.StatusInternalServerError) {
		t.Fatalf("访问日志 status 应为 500，实际 %v", accessLog["status"])
	}
	if accessLog["path"] != "/api/v1/boom" {
		t.Fatalf("访问日志应含 path，实际 %v", accessLog["path"])
	}
	if panicLog == nil {
		t.Fatalf("缺少 panic 日志: %s", buf.String())
	}
	if panicLog["method"] != http.MethodGet || panicLog["path"] != "/api/v1/boom" {
		t.Fatalf("panic 日志应含 method/path，实际 method=%v path=%v", panicLog["method"], panicLog["path"])
	}
}
