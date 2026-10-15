package gateway

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// UpstreamRequest 是一次上游请求的构造上下文：把「基址 / 协议端点 / 凭据 / 请求体 /
// 下游头 / 头策略 / 鉴权 / 变量」收敛为一个结构，供各基础协议适配器共享。
type UpstreamRequest struct {
	BaseURL          string                       // 有效上游基址（协议级 baseUrl 优先）
	Path             string                       // 协议端点路径覆盖（空则用基础协议内置默认）
	Cred             string                       // 渠道凭据（{api_key} 变量与 auth 注入来源）
	Body             []byte                       // 已改写 model 的请求体
	IsStream         bool                         // 是否流式
	DownstreamHeader http.Header                  // 下游原始请求头（透传/变量取值来源）
	HeaderPolicy     channelprotocol.HeaderPolicy // 请求头策略
	Auth             channelprotocol.AuthSpec     // 鉴权注入方式
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

// authHeaderNames 鉴权头名集合：这些头禁止出现在用户可编辑 set（由 auth 注入，AC-9）。
var authHeaderNames = map[string]bool{
	"authorization": true,
	"x-api-key":     true,
	"api-key":       true,
}

// buildUpstreamRequest 是各基础协议适配器共享的上游请求构造：拼 URL、注入 Content-Type、
// 应用头策略（先减强制黑名单）、注入鉴权、返回 http.Request。
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
	applyHeaderPolicy(hreq.Header, req.DownstreamHeader, req.HeaderPolicy, req.Cred, req.Vars)
	injectAuth(hreq, req.Auth, req.Cred)
	return hreq, nil
}

// joinURL 拼接基址与路径：去基址尾部 /、去路径头部 /，避免重复斜杠。
func joinURL(baseURL, path string) string {
	return strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

// applyHeaderPolicy 依据 passthroughMode 从下游头选取候选 → 减去强制黑名单 → 覆盖 set（变量替换）。
// 鉴权头由 injectAuth 在之后注入，故 set 中的鉴权头名会被跳过（AC-9）。
func applyHeaderPolicy(dst, downstream http.Header, p channelprotocol.HeaderPolicy, cred string, vars map[string]string) {
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
	// set：显式设置（覆盖语义），支持变量替换；鉴权头名跳过（由 auth 注入）。
	for name, val := range p.Set {
		if authHeaderNames[strings.ToLower(name)] {
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

// injectAuth 依据 auth(header/scheme/query) 注入凭据；缺省 header=Authorization、scheme=bearer。
func injectAuth(req *http.Request, auth channelprotocol.AuthSpec, cred string) {
	if cred == "" {
		return
	}
	header := strings.TrimSpace(auth.Header)
	if header == "" {
		header = "Authorization"
	}
	scheme := strings.ToLower(strings.TrimSpace(auth.Scheme))
	val := cred
	if scheme == "" || scheme == "bearer" {
		val = "Bearer " + cred
	}
	req.Header.Set(header, val)
	if len(auth.Query) > 0 {
		q := req.URL.Query()
		for k, v := range auth.Query {
			q.Set(k, substituteVars(v, nil, nil, cred))
		}
		req.URL.RawQuery = q.Encode()
	}
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
			return downstream.Get(name)
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
