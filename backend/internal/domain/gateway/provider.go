// Package gateway 实现 LLM 网关的核心热路径：OpenAI-compat 代理转发、
// 渠道透明（不泄漏渠道名/上游地址）、限流与计费接线。
//
// 设计约束：
//   - 对下游额度屏蔽渠道与上游实现细节（渠道透明）；
//   - 非流式请求「先计费再响应」，余额不足直接 402，不返成功 body；
//   - 流式请求因无法回改响应头，计费在流结束后执行，失败仅记审计日志。
package gateway

import (
	"context"
	"net/http"

	"github.com/team/llmgateway/internal/domain/billing"
)

// Provider 抽象一个上游协议适配器。当前实现 openai-compat 一种协议。
//
// 实现要点：
//   - BuildUpstreamRequest 把下游请求转成上游请求（注入渠道凭据到 Authorization）；
//   - ExtractUsage 从非流式响应体解析 token 用量（容错，缺字段给 0）；
//   - HandleStream 逐块透传 SSE 并统计 usage，返回最终用量。
type Provider interface {
	Name() string
	// BuildUpstreamRequest 构建上游请求，body 为已改写 model 的下游请求体。
	BuildUpstreamRequest(ctx context.Context, baseURL string, cred string, body []byte, isStream bool) (*http.Request, error)
	// ExtractUsage 从非流式响应体解析 token 用量。
	ExtractUsage(body []byte) (billing.Usage, error)
	// HandleStream 透传流式 SSE 并返回统计到的用量。
	HandleStream(ctx context.Context, upstream *http.Response, w http.ResponseWriter) (billing.Usage, error)
}
