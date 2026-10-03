package clientip

import (
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

// 可信对端但 X-Real-IP 非法（非 IP）→ 回退 TCP 对端，避免任意字符串注入审计 ip 字段。
func TestFrom_InvalidHeaderFallsBack(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:443"
	r.Header.Set("X-Real-IP", "not-an-ip'; DROP TABLE")
	if got := From(r); got != "127.0.0.1" {
		t.Fatalf("invalid X-Real-IP should fall back to peer, got %q", got)
	}
}

// 非法 TRUSTED_PROXY_CIDRS 条目不应影响默认（仅回环）信任集。
func TestFrom_PrivateRangeNotImplicitlyTrusted(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.1.2.3:443"
	r.Header.Set("X-Real-IP", "198.51.100.9")
	if got := From(r); got != "10.1.2.3" {
		t.Fatalf("10/8 should NOT be implicitly trusted, got %q", got)
	}
}
