package server

import (
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

// buildAuthHandler 构造带 WithAuth(ADMIN) 的保护 handler，成功则响应 200。
func buildAuthHandler(mgr *jwtx.Manager) http.Handler {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := UserIDFrom(r.Context())
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]int64{"uid": id})
	})
	return WithAuth(mgr, "ADMIN")(h)
}

func TestWithAuth_NoToken(t *testing.T) {
	mgr := newTestJWTMgr(t)
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	buildAuthHandler(mgr).ServeHTTP(w, r)

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
	buildAuthHandler(mgr).ServeHTTP(w, r)

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
	buildAuthHandler(mgr).ServeHTTP(w, r)

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
	buildAuthHandler(mgr).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestUserIDFrom_Empty(t *testing.T) {
	if _, ok := UserIDFrom(t.Context()); ok {
		t.Fatal("expected no user id in empty ctx")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.5:443"
	if got := clientIP(r); got != "203.0.113.5" {
		t.Fatalf("expected remoteaddr host, got %q", got)
	}
	r.Header.Set("X-Real-IP", "198.51.100.9")
	if got := clientIP(r); got != "198.51.100.9" {
		t.Fatalf("expected x-real-ip, got %q", got)
	}
}