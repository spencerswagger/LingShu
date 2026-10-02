package clientip

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFrom_RemoteAddrDefault(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.5:443"
	// 直连对端不可信 → 忽略 X-Real-IP，取 TCP 对端
	r.Header.Set("X-Real-IP", "198.51.100.9")
	if got := From(r); got != "203.0.113.5" {
		t.Fatalf("untrusted peer should fall back to remote addr, got %q", got)
	}
}

func TestFrom_LoopbackTrusted(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:443"
	r.Header.Set("X-Real-IP", "198.51.100.9")
	if got := From(r); got != "198.51.100.9" {
		t.Fatalf("loopback peer should trust X-Real-IP, got %q", got)
	}
}

func TestFrom_NoHeader(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:443"
	if got := From(r); got != "127.0.0.1" {
		t.Fatalf("no header should fall back to peer, got %q", got)
	}
}

var _ = http.MethodGet
