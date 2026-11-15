// headers.go 是转发链的请求/响应头策略。
//
// 请求侧：只换凭证头，其余按强制黑名单剔除后逐值透传。
// 响应侧（F8 裁定）：
//  1. hop-by-hop 头一律丢（连接级语义不能跨代理）；
//  2. 名以 `x-magpie-` / `x-upstream-` 开头的头一律丢（上游标识，不许漏给租户）；
//  3. `server` / `x-powered-by` / `via` 一律丢（会暴露上游实现）；
//  4. 名或值里出现**供应商/上游产品名**的头一律丢（vendorTokens，见下）；
//  5. `x-request-id` **保留，但必须是灵枢自己的**：先丢上游的，再由本包写入入站链路 ID。
//     直接透传上游的 request-id 会让用户拿它去上游查别人的单（且对账时对不上号）。
//
// §10-F 例外说明：vendorTokens 里出现上游产品名是**唯一允许**在产品代码里写这些字面量的地方
// ——不写就剥不掉，等于泄漏上游身份。界面/配置/日志回显仍然不出现。
package magpie

import (
	"net/http"
	"strings"
)

// requestForcedBlacklist 是出站请求必须丢弃的头（小写比较，常量表、不可配置）。
var requestForcedBlacklist = map[string]bool{
	// hop-by-hop（RFC 7230 §6.1）
	"connection":          true,
	"keep-alive":          true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"proxy-connection":    true,
	"proxy-authorization": true,
	// 由 URL / body 重新生成
	"host":             true,
	":authority":       true,
	":method":          true,
	":scheme":          true,
	":path":            true,
	"content-length":   true,
	"content-encoding": true,
	"accept-encoding":  true,
	// 安全 / 链路可信（可信客户端信息不冒充）
	"cookie":             true,
	"expect":             true,
	"forwarded":          true,
	"x-forwarded-for":    true,
	"x-forwarded-proto":  true,
	"x-forwarded-host":   true,
	"x-real-ip":          true,
	"x-current-password": true,
	"anthropic-dangerous-direct-browser-access": true,
	// 凭据（由调用方按令牌注入，绝不下游透传）
	"authorization":  true,
	"x-api-key":      true,
	"api-key":        true,
	"x-goog-api-key": true,
}

// IsForcedBlacklist 报告请求头是否必须丢弃（按小写比较）。
func IsForcedBlacklist(name string) bool { return requestForcedBlacklist[strings.ToLower(name)] }

// responseHopByHop 是响应侧必须丢弃的连接级头。
var responseHopByHop = map[string]bool{
	"connection":        true,
	"keep-alive":        true,
	"proxy-connection":  true,
	"te":                true,
	"trailer":           true,
	"transfer-encoding": true,
	"upgrade":           true,
}

// upstreamIdentityHeaderPrefixes 是上游标识头前缀（F8 第 2 条）。
var upstreamIdentityHeaderPrefixes = []string{"x-magpie-", "x-upstream-"}

// responseDroppedExactly 是明确要丢的单个头（F8 第 3 条 + 上游 request-id）。
var responseDroppedExactly = map[string]bool{
	"server":           true,
	"x-powered-by":     true,
	"via":              true,
	"x-request-id":     true, // 上游的丢掉，由我们写入自己的（见 WriteRequestID）
	"x-correlation-id": true,
}

// vendorTokens 是「供应商/上游产品名」清单（F8 第 4 条）。
//
// 只用于**丢弃**名字或取值里带供应商名的回程头，不参与任何对外文案、不进日志字段。
// 新增上游产品名时只追加到这里，别在别处另写一份——这是 §10-F 允许的唯一字面量位置。
//
// 为什么按「值」也要判：上游常把供应商标识放在通用头里（`x-provider: <name>`、
// `x-served-by: <name>`、`set-cookie: ... <name> ...`），只按头名剥必然漏。
var vendorTokens = []string{
	"magpie", "openai", "anthropic", "google", "gemini", "vertex", "azure",
	"bedrock", "xai", "deepseek", "moonshot", "mistral", "cohere", "groq",
	"together", "fireworks", "openrouter", "siliconflow", "qwen", "zhipu",
}

// CopyRequestHeaders 把入站 header 带到出站请求上：
//  1. 命中强制黑名单的一律不带（凭据由调用方注入）；
//  2. Content-Type 按入站原值重设（body 除 model 外没变）；
//  3. Accept-Encoding 固定 identity（流式不能二次压缩；压缩会让 SSE 分包与 usage 读取失真）。
func CopyRequestHeaders(dst, src http.Header) {
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

// ShouldDropResponseHeader 报告回程头是否要丢弃（F8 四条规则 + 上游 request-id）。
func ShouldDropResponseHeader(name, value string) bool {
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
	// 按值判是必要的一刀——上游经常把供应商标识塞进通用头名里。
	for _, token := range vendorTokens {
		if containsFold(value, token) {
			return true
		}
	}
	// 头名里带供应商名时，只对「身份类」头名动手（provider/account/vendor/…）：
	// `anthropic-ratelimit-*` 这类确实是 Anthropic 客户端的正常可观测头，
	// 按名字一刀切会把限流信息也剥掉，属于过度剥除。
	// `x-served-by` 这类头名本身就是身份声明（不含供应商字面量），按身份词单独剥。
	if hasIdentityToken(lower) {
		return true
	}
	return false
}

// CopyResponseHeaders 透传上游响应头，按 F8 规则剔除，并写入灵枢自己的 x-request-id。
//
// Content-Length 保留（本链路不改写响应体，长度仍然成立）；流式没有长度头，走 chunked。
// ourRequestID 为空时由调用方保证已生成（不能留空：留空等于下游没有链路 ID，
// 排障时对不上号，而 F8 要求「x-request-id 是灵枢自己的」）。
func CopyResponseHeaders(dst, src http.Header, ourRequestID string) {
	for name, values := range src {
		drop := false
		for _, v := range values {
			if ShouldDropResponseHeader(name, v) {
				drop = true
				break
			}
		}
		if drop || ShouldDropResponseHeader(name, "") {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
	if ourRequestID != "" {
		dst.Set("x-request-id", ourRequestID)
	}
}

// Attribution 是从回程头读到的归因事实（写入 billing_records 的 upstream_* 四列）。
//
// 取不到就留空（落 NULL），**不猜**：猜出来的供应商会让盈亏页把成本记到别人账上。
// 必须在 CopyResponseHeaders 之前读 —— 这些头本身是要剥掉的。
type Attribution struct {
	Provider string
	Account  string
	Model    string
}

// attributionHeaderCandidates 是归因头候选名（大小写不敏感）。
//
// 列多个候选是刻意的：上游换一版头名不该让对账整列变 NULL；第一个非空的胜出。
var attributionHeaderCandidates = struct {
	Provider []string
	Account  []string
	Model    []string
}{
	Provider: []string{"x-magpie-provider", "x-upstream-provider", "x-provider"},
	Account:  []string{"x-magpie-account", "x-upstream-account", "x-account"},
	Model:    []string{"x-magpie-model", "x-upstream-model", "x-model"},
}

// ReadAttribution 从回程头解析归因（必须在剥头之前调用）。
func ReadAttribution(h http.Header) Attribution {
	pick := func(names []string) string {
		for _, n := range names {
			if v := strings.TrimSpace(h.Get(n)); v != "" {
				return v
			}
		}
		return ""
	}
	return Attribution{
		Provider: pick(attributionHeaderCandidates.Provider),
		Account:  pick(attributionHeaderCandidates.Account),
		Model:    pick(attributionHeaderCandidates.Model),
	}
}

// identityNameTokens 是「上游身份类」头名的特征词（小写比较）。
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

// IsEventStream 判断响应是否 SSE（Content-Type 含 text/event-stream）。
func IsEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}
