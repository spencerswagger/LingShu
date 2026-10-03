package reqmeta_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/team/llmgateway/internal/pkg/reqmeta"
	"github.com/team/llmgateway/internal/server"
)

// 回归 R2：按 server.Handler 的真实顺序（WithRequestID 在外、reqmeta 在内）组合时，
// IP 与 request_id 必须都被注入上下文——审计才能回答"哪个 IP 在爆破 admin"。
func TestMiddleware_InjectsIPAndRequestID(t *testing.T) {
	var got reqmeta.Meta
	chain := server.WithRequestID(reqmeta.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = reqmeta.From(r.Context())
	})))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	req.RemoteAddr = "192.0.2.9:4567"
	chain.ServeHTTP(httptest.NewRecorder(), req)

	if got.RequestID == "" {
		t.Fatal("request id must be injected into context")
	}
	if got.IP != "192.0.2.9" {
		t.Fatalf("ip must be injected into context, got %q", got.IP)
	}
}
