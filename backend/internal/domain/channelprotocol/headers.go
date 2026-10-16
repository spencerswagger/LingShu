package channelprotocol

import (
	"net/http"
	"strings"
)

// 本文件是「请求头策略 → 覆盖(set)」的唯一实现：网关热路径（真实转发）与渠道健康探测
// 共用，避免两处引擎口径漂移（探测漏套用导致「配置了却不生效」）。

// forcedBlacklist 是内置强制黑名单（常量表，不入库、不可配置）：任何注入方式（含「覆盖」）都不上游。
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

// IsForcedBlacklist 报告头名是否命中内置强制黑名单（按小写比较）。
func IsForcedBlacklist(name string) bool {
	return forcedBlacklist[strings.ToLower(name)]
}

// ApplySet 应用「覆盖(set)」：显式设置（覆盖语义），支持变量替换。
// 仅受非凭据类强制黑名单约束（Cookie/Host/Content-Length 等恒不可注入）；
// 凭据头允许写入，从而覆盖协议隐含的鉴权注入。
// vars 提供 {session_id}/{request_id}/{model}；downstream 提供 {client_header:NAME}（无下游请求时传 nil）。
func ApplySet(dst http.Header, set map[string]string, cred string, vars map[string]string, downstream http.Header) {
	for name, val := range set {
		lk := strings.ToLower(name)
		if forcedBlacklist[lk] && !credentialHeaderNames[lk] {
			continue
		}
		dst.Set(name, substituteVars(val, vars, downstream, cred))
	}
}

// substituteVars 替换值中的变量：{api_key} / {session_id} / {request_id} / {model} /
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
