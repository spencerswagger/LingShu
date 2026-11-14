// headers.go 是转发链的请求/响应头策略：出站只换凭证头，其余按白名单剔除 + 原样透传。
//
// 强制黑名单（hop-by-hop / 由 URL·body 重新生成 / 安全·链路可信 / 凭据）口径沿用 V1 网关的
// channelprotocol 常量表（该包已在第一期代码摘除中整体删除，这里按原表逐条重建，
// 保持与既有预设契约 docs/preset-pack-format.md 6.3 一致）：任何注入方式都不上游。
package magpie

import (
	"net/http"
	"strings"
)

// forcedBlacklist 是内置强制丢弃的头（小写比较，常量表、不可配置）。
var forcedBlacklist = map[string]bool{
	// hop-by-hop（RFC 7230 §6.1）
	"connection":          true,
	"keep-alive":          true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"proxy-connection":    true,
	"proxy-authorization": true,
	// 由 URL / body 重新生成（下游值必须丢弃）
	"host":             true,
	":authority":       true,
	":method":          true,
	":scheme":          true,
	":path":            true,
	"content-length":   true,
	"content-type":     true,
	"content-encoding": true,
	"accept-encoding":  true,
	// 安全 / 链路可信
	"cookie":             true,
	"expect":             true,
	"via":                true,
	"forwarded":          true,
	"x-forwarded-for":    true,
	"x-forwarded-proto":  true,
	"x-forwarded-host":   true,
	"x-real-ip":          true,
	"x-current-password": true,
	"anthropic-dangerous-direct-browser-access": true,
	// 凭据（由本包按令牌注入，绝不下游透传）
	"authorization": true,
	"x-api-key":     true,
	"api-key":       true,
}

// IsForcedBlacklist 报告头名是否命中强制丢弃表（按小写比较）。
func IsForcedBlacklist(name string) bool {
	return forcedBlacklist[strings.ToLower(name)]
}

// responseHopByHop 是响应侧必须丢弃的头（连接级语义，不能跨代理复用）。
var responseHopByHop = map[string]bool{
	"connection":        true,
	"keep-alive":        true,
	"proxy-connection":  true,
	"te":                true,
	"trailer":           true,
	"transfer-encoding": true,
	"upgrade":           true,
}

// copyRequestHeaders 把入站 header 原样带到出站请求上，只做三类处理：
//  1. 命中强制丢弃表的头一律不带（凭据头由 credHeaderValue 重新注入）；
//  2. Content-Type 按下游原值重设（body 没变，不换类型）；
//  3. Accept-Encoding 固定 identity（流式不能二次压缩；上游压缩会让 SSE 分包/usage 读取失真）。
//
// 其余头（anthropic-version / anthropic-beta / openai-beta / user-agent / accept 等）逐值透传。
func copyRequestHeaders(dst, src http.Header) {
	for name, values := range src {
		if IsForcedBlacklist(name) {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
	if ct := src.Get("Content-Type"); ct != "" {
		dst.Set("Content-Type", ct)
	} else {
		dst.Set("Content-Type", "application/json")
	}
	dst.Set("Accept-Encoding", "identity")
}

// copyResponseHeaders 透传上游响应头：只丢弃连接级 hop-by-hop 头。
//
// Content-Length 保留（本链路不改写响应体，长度仍然成立）；上游未给则由 Go 传输层决定
// （SSE 走 chunked，长度本来就未知）。
func copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		if responseHopByHop[strings.ToLower(name)] {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}
