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

// responseDroppedExactly 是响应侧明确要丢的单个头（口径②：上游实现暴露类，与供应商无关也丢）。
var responseDroppedExactly = map[string]bool{
	"server":       true,
	"x-powered-by": true,
}

// upstreamIdentityHeaderPrefixes 是灵枢/代理侧内部头前缀（口径②）：
// `x-magpie-*` 一律不带给下游（代理侧用它标识自己，透传等于把内部链路亮给租户）。
var upstreamIdentityHeaderPrefixes = []string{"x-magpie-"}

// vendorTokens 是「供应商/上游产品名」清单（口径②）。
//
// 只用于**丢弃**名字或取值里带供应商名的回程头，不参与任何对外文案、不进日志字段。
// 新增上游产品名时只追加到这里，别在别处另写一份——这是唯一允许出现这些字面量的位置
// （不写就剥不掉，等于泄漏上游身份）。
//
// 为什么按「值」也要判：上游常把供应商标识放在通用头里（`x-provider: <name>`、
// `x-served-by: <name>`、`set-cookie: ... <name> ...`），只按头名剥必然漏。
var vendorTokens = []string{
	"magpie", "openai", "anthropic", "google", "goog", "gemini", "vertex", "azure",
	"bedrock", "xai", "deepseek", "moonshot", "mistral", "cohere", "groq",
	"together", "fireworks", "openrouter", "siliconflow", "qwen", "zhipu",
}

// identityNameTokens 是「上游身份类」头名的特征词（小写比较）：
// 名字本身是身份声明但不含供应商字面量（`X-Vendor-Trace` / `x-served-by`）按此剥离。
// 注意别把 CORS 的 `access-control-allow-origin` 之类的头也带进来（"origin" 故意不在列表里）。
var identityNameTokens = []string{"provider", "account", "vendor", "served", "backend", "worker", "region"}

// hasVendorToken 报告小写头名里是否出现供应商名。
func hasVendorToken(lower string) bool {
	for _, token := range vendorTokens {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return false
}

// hasIdentityToken 报告小写头名里是否出现身份类特征词。
func hasIdentityToken(lower string) bool {
	for _, token := range identityNameTokens {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return false
}

// containsFold 是大小写不敏感的子串判断（不引入额外依赖）。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// shouldDropResponseHeader 报告回程头是否要丢弃（口径② + hop-by-hop）。
// name 按小写比较；value 非空时还按「取值含供应商名」判（多值头任一值命中即丢）。
func shouldDropResponseHeader(name, value string) bool {
	lower := strings.ToLower(name)
	if responseHopByHop[lower] || responseDroppedExactly[lower] {
		return true
	}
	for _, p := range upstreamIdentityHeaderPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	// 取值里出现供应商名：`x-served-by: <name>`、`x-note: powered by <name>` 一律挡住。
	for _, token := range vendorTokens {
		if containsFold(value, token) {
			return true
		}
	}
	// 头名里出现供应商名或身份类特征词：一律丢（子串匹配、大小写不敏感）。
	// 典型漏点：`X-Vendor-Trace`（名字带身份词）/ `X-OpenAI-Organization`（名字带供应商名）
	// —— 既不是 `x-magpie-` 前缀，值里也没有供应商字面量。
	if hasVendorToken(lower) || hasIdentityToken(lower) {
		return true
	}
	return false
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

// copyResponseHeaders 透传上游响应头，按口径②剔除，再叠加原有 hop-by-hop 丢弃：
//   - 灵枢/代理内部标识（`x-magpie-*` 前缀）；
//   - 上游实现暴露（`server` / `x-powered-by`）；
//   - 头名或任一取值含供应商名/身份类特征词的头（`X-Vendor-Trace` / `x-provider: openai` 等）；
//   - `x-request-id` 只保留灵枢自己的：全局中间件（server.WithRequestID）已在最外层把
//     链路 ID 写入响应头，这里丢掉上游同名头（再 Add 会并成第二值，下游拿到两个 ID）。
//     中间件未挂载时（如单测裸 mux）不动，上游的原样透传，不算误删。
//
// 其余头逐值透传。Content-Length 保留（本链路不改写响应体，长度仍然成立）；上游未给则由
// Go 传输层决定（SSE 走 chunked，长度本来就未知）。
func copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		if strings.EqualFold(name, "x-request-id") && dst.Get("X-Request-Id") != "" {
			continue
		}
		if shouldDropResponseHeader(name, "") {
			continue
		}
		drop := false
		for _, v := range values {
			if shouldDropResponseHeader(name, v) {
				drop = true
				break
			}
		}
		if drop {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}
