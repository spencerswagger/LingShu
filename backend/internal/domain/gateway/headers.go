package gateway

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// UpstreamRequest 是一次上游请求的构造上下文：把「基址 / 协议端点 / 凭据 / 请求体 /
// 下游头 / 头策略 / 协议 / 变量」收敛为一个结构，供各基础协议适配器共享。
type UpstreamRequest struct {
	BaseURL          string                       // 有效上游基址（协议级 baseUrl 优先）
	Path             string                       // 协议端点路径覆盖（空则用基础协议内置默认）
	Cred             string                       // 渠道凭据（{api_key} 变量与鉴权注入来源）
	Body             []byte                       // 已改写 model 的请求体
	DownstreamHeader http.Header                  // 下游原始请求头（透传/变量取值来源）
	HeaderPolicy     channelprotocol.HeaderPolicy // 请求头策略
	Protocol         string                       // 入站基础协议（决定隐含鉴权注入方式）
	Vars             map[string]string            // 变量表：session_id / request_id / model
}

// forcedBlacklist 是内置强制黑名单（常量表，不入库、不可配置）：任何透传模式下都不上游。
// 依据 docs/preset-pack-format.md 6.3：hop-by-hop / 由 URL·body 重新生成 / 安全·链路可信 / 凭据。
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
	// 凭据（由 auth 注入）
	"authorization": true,
	"x-api-key":     true,
	"api-key":       true,
}

// credentialHeaderNames 凭据头名集合：透传恒禁止（避免把网关自身的凭据带上游）；
// 但「覆盖(set)」允许显式写入，用于覆盖协议隐含的鉴权注入。
var credentialHeaderNames = map[string]bool{
	"authorization": true,
	"x-api-key":     true,
	"api-key":       true,
}

// buildUpstreamRequest 是各基础协议适配器共享的上游请求构造：拼 URL、注入 Content-Type，
// 依次应用「透传 → 协议隐含鉴权 → 覆盖(set)」，返回 http.Request。
// defaultPath 为基址后拼接的基础协议内置默认路径（相对基址，可含 v1/…）。
func buildUpstreamRequest(ctx context.Context, req UpstreamRequest, defaultPath, contentType string) (*http.Request, error) {
	path := strings.TrimSpace(req.Path)
	if path == "" {
		path = defaultPath
	}
	url := joinURL(req.BaseURL, path)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		hreq.Header.Set("Content-Type", contentType)
	}
	// 1) 透传：按模式从下游选取头（强制黑名单含凭据头，恒不透传，AC-5/AC-8）。
	applyPassthrough(hreq.Header, req.DownstreamHeader, req.HeaderPolicy)
	// 2) 协议隐含鉴权：openai-* → Authorization Bearer；anthropic-messages → x-api-key。
	injectProtocolAuth(hreq, req.Protocol, req.Cred)
	// 3) 覆盖(set)：最后应用，可覆盖协议隐含鉴权头（显式意图优先）。
	applyOverrides(hreq.Header, req.HeaderPolicy.Set, req.Cred, req.Vars, req.DownstreamHeader)
	return hreq, nil
}

// joinURL 拼接基址与路径：去基址尾部 /、去路径头部 /，避免重复斜杠。
func joinURL(baseURL, path string) string {
	return strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

// applyPassthrough 依据 passthroughMode 从下游头选取候选 → 减去强制黑名单（含凭据头，AC-5/AC-8）。
func applyPassthrough(dst, downstream http.Header, p channelprotocol.HeaderPolicy) {
	mode := p.PassthroughMode
	if mode == "" {
		mode = channelprotocol.PassthroughNone
	}
	switch mode {
	case channelprotocol.PassthroughAll:
		copyHeaders(dst, downstream, func(string) bool { return true })
	case channelprotocol.PassthroughWhitelist:
		allow := lowerSet(p.Whitelist)
		copyHeaders(dst, downstream, func(k string) bool { return allow[strings.ToLower(k)] })
	case channelprotocol.PassthroughBlacklist:
		deny := lowerSet(p.Blacklist)
		copyHeaders(dst, downstream, func(k string) bool { return !deny[strings.ToLower(k)] })
	default: // none
		// 不透传任何下游头
	}
}

// applyOverrides 应用「覆盖(set)」：显式设置（覆盖语义），支持变量替换。
// 仅受非凭据类强制黑名单约束（Cookie/Host/Content-Length 等恒不可注入，AC-8）；
// 凭据头允许写入，从而覆盖协议隐含的鉴权注入。
func applyOverrides(dst http.Header, set map[string]string, cred string, vars map[string]string, downstream http.Header) {
	for name, val := range set {
		lk := strings.ToLower(name)
		if forcedBlacklist[lk] && !credentialHeaderNames[lk] {
			continue
		}
		dst.Set(name, substituteVars(val, vars, downstream, cred))
	}
}

// copyHeaders 把 downstream 中通过 allow 的头复制进 dst（跳过强制黑名单）。
func copyHeaders(dst, downstream http.Header, allow func(key string) bool) {
	for k, vs := range downstream {
		lk := strings.ToLower(k)
		if forcedBlacklist[lk] || !allow(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// injectProtocolAuth 按基础协议隐含方式注入凭据；无凭据则不注入。
func injectProtocolAuth(req *http.Request, protocol, cred string) {
	if cred == "" {
		return
	}
	header, scheme := channelprotocol.ProtocolAuth(protocol)
	val := cred
	if scheme == "bearer" {
		val = "Bearer " + cred
	}
	req.Header.Set(header, val)
}

// substituteVars 替换 set/query 值中的变量：{api_key} / {session_id} / {request_id} / {model} /
// {client_header:NAME}（下游原始头）。
func substituteVars(s string, vars map[string]string, downstream http.Header, cred string) string {
	if !strings.Contains(s, "{") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '{' {
			b.WriteByte(s[i])
			i++
			continue
		}
		end := strings.IndexByte(s[i:], '}')
		if end < 0 {
			b.WriteString(s[i:])
			break
		}
		token := s[i+1 : i+end]
		b.WriteString(resolveVar(token, vars, downstream, cred))
		i += end + 1
	}
	return b.String()
}

// resolveVar 解析单个变量名（花括号内内容）；未知变量原样保留。
func resolveVar(token string, vars map[string]string, downstream http.Header, cred string) string {
	switch token {
	case "api_key":
		return cred
	case "session_id":
		return vars["session_id"]
	case "request_id":
		return vars["request_id"]
	case "model":
		return vars["model"]
	}
	if name, ok := strings.CutPrefix(token, "client_header:"); ok {
		if downstream != nil {
			return downstream.Get(strings.TrimSpace(name))
		}
		return ""
	}
	return "{" + token + "}"
}

// lowerSet 把字符串切片转为小写集合。
func lowerSet(in []string) map[string]bool {
	out := make(map[string]bool, len(in))
	for _, s := range in {
		out[strings.ToLower(strings.TrimSpace(s))] = true
	}
	return out
}
