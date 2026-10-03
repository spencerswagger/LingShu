// Package clientip 解析请求的真实客户端 IP。
//
// 信任边界：仅当 TCP 直连对端属于可信代理网段时才采信 X-Real-IP，否则一律以
// TCP 对端为准，杜绝客户端伪造来源头（限流/审计依赖此来源的不可伪造性）。
//
// 默认仅信任回环地址；转发代理（如 nginx 容器）的网段必须由部署者通过
// TRUSTED_PROXY_CIDRS 环境变量显式声明（逗号分隔 CIDR）。不做任何私网段的
// 隐式信任，避免"同网段任意主机可伪造 X-Real-IP"。
package clientip

import (
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
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
		} else {
			log.Printf("[clientip] 忽略非法的 TRUSTED_PROXY_CIDRS 条目: %q (%v)", c, err)
		}
	}
	return out
}

// LogTrustedNets 打印生效的可信代理网段（进程启动时调用一次）。
// 目的：把"信任边界失配"从静默失败变为可观测——否则 X-Real-IP 永远不被采信，
// 审计 IP 全部退化为代理容器 IP 且无人察觉。
func LogTrustedNets() {
	nets := make([]string, 0, len(trustedNets))
	for _, n := range trustedNets {
		nets = append(nets, n.String())
	}
	log.Printf("[clientip] trusted proxy CIDRs: %v（仅这些对端会采信 X-Real-IP；如代理网段不在此列表，请检查 TRUSTED_PROXY_CIDRS）", nets)
}

var warnOnce sync.Once

// From 返回请求的真实客户端 IP。可信代理转发且 X-Real-IP 为合法 IP 时取其值，
// 否则取 TCP 对端，并对"来源不可信却带了 X-Real-IP"告警一次。
func From(r *http.Request) string {
	host := remoteHost(r.RemoteAddr)
	raw := strings.TrimSpace(r.Header.Get("X-Real-IP"))
	if isTrusted(host) {
		if ip := net.ParseIP(raw); ip != nil {
			return ip.String()
		}
		return host
	}
	if raw != "" {
		warnOnce.Do(func() {
			log.Printf("[clientip] 对端 %s 不在可信代理网段内但携带了 X-Real-IP，已忽略并使用 TCP 对端地址；"+
				"限流与审计将基于该地址，请检查 TRUSTED_PROXY_CIDRS 配置", host)
		})
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
