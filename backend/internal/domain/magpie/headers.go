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

// connectionNamedHeaders 解析 `Connection` 头**点名的字段**（RFC 7230 §6.1）。
//
// 为什么不能只靠固定黑名单：`Connection` 是「逐跳声明」，它点名的每一个字段都只对**本跳**有意义，
// 代理不许转发。名字可以是任意扩展头（`Connection: x-trace-id` / `Connection: x-foo, x-bar`），
// 固定表（connection/keep-alive/te/trailer/transfer-encoding/upgrade）挡不住这些自定义点名。
// 只按固定表剥 → 上游把内部链路字段挂在 `Connection: x-internal-*` 上就能漏到下游。
//
// 口径细节：
//   - 多值头、逗号分隔、大小写混用都要处理（`Header.Values` 拿全部值，逐个 split(',')）；
//   - 返回值是**小写头名集合**，调用方按小写比对；
//   - `close` / `keep-alive` 这类是**连接选项**不是字段名，收进来也无害（没有同名头会被误删）；
//   - ⚠️ `content-length` **即使被点名也不剥**（见 withoutLengthExemption 的说明）：
//     响应体未被改写时长度仍然成立，剥掉只会让 Go 退化成 chunked，下游得自己重算。
func connectionNamedHeaders(h http.Header) map[string]bool {
	var out map[string]bool
	for _, value := range h.Values("Connection") {
		for _, part := range strings.Split(value, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "" || name == "content-length" {
				continue // content-length 永不由 Connection 剥（见函数头注释）
			}
			if out == nil {
				out = make(map[string]bool, 4)
			}
			out[name] = true
		}
	}
	return out
}

// copyRequestHeaders 把入站 header 原样带到出站请求上，只做三类处理：
//  1. 命中强制丢弃表的头一律不带（凭据头由 credHeaderValue 重新注入）；
//  2. Content-Type 按下游原值重设（body 没变，不换类型）；
//  3. Accept-Encoding 固定 identity（流式不能二次压缩；上游压缩会让 SSE 分包/usage 读取失真）。
//
// 其余头（anthropic-version / anthropic-beta / openai-beta / user-agent / accept 等）逐值透传。
func copyRequestHeaders(dst, src http.Header) {
	// 客户端点名的逐跳字段：`Connection: x-foo` 时 x-foo 不许带到上游（自定义扩展头同样适用）。
	named := connectionNamedHeaders(src)
	for name, values := range src {
		if IsForcedBlacklist(name) || named[strings.ToLower(name)] {
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
//
//   - 灵枢/代理内部标识（`x-magpie-*` 前缀）；
//
//   - 上游实现暴露（`server` / `x-powered-by`）；
//
//   - 头名或任一取值含供应商名/身份类特征词的头（`X-Vendor-Trace` / `x-provider: openai` 等）；
//
//   - `x-request-id` 只保留灵枢自己的：全局中间件（server.WithRequestID）已在最外层把
//     链路 ID 写入响应头，这里丢掉上游同名头（再 Add 会并成第二值，下游拿到两个 ID）。
//     中间件未挂载时（如单测裸 mux）不动，上游的原样透传，不算误删。
//
//   - `Connection` 头**点名的字段**同样丢（逐跳转发语义，见 connectionNamedHeaders）。
//
// 其余头逐值透传。两个头的处置是刻意分开写的，改动前先读这两段：
//
//   - `Content-Length`：**保留**，不参与任何剔除（包括 Connection 点名、身份类特征词判定）。
//     依据：本链路在不遮蔽/不解压时**不改写响应体**，上游给的字节数仍然成立；剥掉只会让 Go
//     退化成 chunked，下游流式库要重算长度、HTTP/1.0 客户端甚至拿不到长度。
//     真正需要改长度的场合由转发层显式处理（见下一条与 proxy.go 的遮蔽分支）。
//   - `Content-Encoding`（**强制 gzip 时**）：不属于「透传决策」，由转发层按实际字节决定。
//     判据是**头与体自洽**，不是「头该不该透传」——体是什么，头就必须说什么。三态：
//     ① 解压成功（上游无视 `Accept-Encoding: identity` 仍压缩）→ **剥编码头**（body 已是明文，
//     留着等于让下游再解一次 → `gzip: invalid header`），`Content-Length` 一并剥（压缩前长度
//     对解压后的字节不成立，交 Go 重算/chunked）；
//     ② 解码器在**读头部时**就拒绝（`gzip.NewReader` 只看魔数，失败即证明这些字节不是该编码，
//     典型是中间层硬加 `Content-Encoding: gzip` 而体已被它解压）→ **剥编码头**、原字节转发、
//     `Content-Length` 保留（转发的是上游原字节，长度成立）；
//     ③ 解不开也**不该解**（br 等 stdlib 不支持的编码）或**头合法但解到中途失败**（截断）→
//     **保留编码头**，原字节透传 + 记 `UnmaskableEncoding` 告警（剥头会让下游把压缩字节当明文，
//     比「下游解压报错」更糟）。
//     处置点全在 proxy.go（decoded / stripEncoding / UnmaskableEncoding 三条分支），
//     本函数不碰这两个头。
func copyResponseHeaders(dst, src http.Header) {
	named := connectionNamedHeaders(src)
	for name, values := range src {
		if strings.EqualFold(name, "x-request-id") && dst.Get("X-Request-Id") != "" {
			continue
		}
		if named[strings.ToLower(name)] {
			continue // Connection 点名的逐跳字段
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
