// Package clientip 解析请求的真实客户端 IP。
//
// 信任边界：仅当 TCP 直连对端属于可信代理网段（本机回环 + TRUSTED_PROXY_CIDRS
// 环境变量，逗号分隔 CIDR）时，才采信 X-Real-IP；否则一律以 TCP 对端为准，
// 杜绝客户端伪造来源头（限流/审计依赖此来源的不可伪造性）。
package clientip

import (
	"net"
	"net/http"
	"os"
	"strings"
)

var trustedNets = parseTrustedNets()

func parseTrustedNets() []*net.IPNet {
	var out []*net.IPNet
	if _, loop4, err := net.ParseCIDR("127.0.0.0/8"); err == nil {
		out = append(out, loop4)
	}
	if _, loop6, err := net.ParseCIDR("::1/128"); err == nil {
		out = append(out, loop6)
	}
	for _, cidr := range strings.Split(os.Getenv("TRUSTED_PROXY_CIDRS"), ",") {
		c := strings.TrimSpace(cidr)
		if c == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// From 返回请求的真实客户端 IP。可信代理转发时取 X-Real-IP，否则取 TCP 对端。
func From(r *http.Request) string {
	host := remoteHost(r.RemoteAddr)
	if isTrusted(host) {
		if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
			return v
		}
	}
	return host
}

func remoteHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

func isTrusted(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range trustedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
