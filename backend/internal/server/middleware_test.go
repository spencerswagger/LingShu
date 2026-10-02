package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
