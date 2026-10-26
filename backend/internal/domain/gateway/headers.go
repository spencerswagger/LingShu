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
	// Features 渠道协议挂载的协议特性（空=未挂载任何特性，请求体保持原样）。
	Features []channelprotocol.Feature
}

// buildUpstreamRequest 是各基础协议适配器共享的上游请求构造：拼 URL、注入 Content-Type，
// 依次应用「透传 → 协议隐含鉴权 → 覆盖(set)」，返回 http.Request。
// defaultPath 为基址后拼接的基础协议内置默认路径（相对基址，可含 v1/…）。
func buildUpstreamRequest(ctx context.Context, req UpstreamRequest, defaultPath, contentType string) (*http.Request, error) {
	// 协议特性（请求侧）：按渠道协议挂载顺序改写请求体（未挂载时零差异）。
	// 特性只改写 Body，与头部注入相互独立，故在构建请求前先行应用。
	if feats := requestFeaturesFor(req.Features); len(feats) > 0 {
		if err := applyRequestFeatures(&req, feats); err != nil {
			return nil, err
		}
	}
	url := resolveUpstreamURL(req.BaseURL, req.Path, defaultPath)
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
	channelprotocol.ApplySet(hreq.Header, req.HeaderPolicy.Set, req.Cred, req.Vars, req.DownstreamHeader)
	return hreq, nil
}

// resolveUpstreamURL 求最终上游 URL：协议端点路径覆盖优先，空则用基础协议内置默认路径。
// 这是「基址 + 路径」拼接的**唯一实现**：真实请求构造与请求参数快照（call_logs.req_params
// 里的 upstream_url）共用它，避免快照记的地址与实际发出去的地址出现口径漂移。
func resolveUpstreamURL(baseURL, pathOverride, defaultPath string) string {
	path := strings.TrimSpace(pathOverride)
	if path == "" {
		path = defaultPath
	}
	return joinURL(baseURL, path)
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

// copyHeaders 把 downstream 中通过 allow 的头复制进 dst（跳过强制黑名单）。
func copyHeaders(dst, downstream http.Header, allow func(key string) bool) {
	for k, vs := range downstream {
		if channelprotocol.IsForcedBlacklist(k) || !allow(k) {
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

// lowerSet 把字符串切片转为小写集合。
func lowerSet(in []string) map[string]bool {
	out := make(map[string]bool, len(in))
	for _, s := range in {
		out[strings.ToLower(strings.TrimSpace(s))] = true
	}
	return out
}
