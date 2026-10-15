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
	"errors"
	"net/http"

	"github.com/team/llmgateway/internal/domain/billing"
)

// ErrUsageMissing 表示上游响应缺失或无法解析 usage（AC-17 放宽：由调用方按可见内容估算计费并禁用渠道）。
var ErrUsageMissing = errors.New("upstream response missing usage")

// Provider 抽象一个上游基础协议适配器。基础协议为代码固定的 3 个：
// openai-chat / openai-responses / anthropic-messages。
//
// 实现要点：
//   - BuildUpstreamRequest 依据构造上下文（基址/路径/凭据/下游头/头策略/鉴权）构建上游请求；
//   - ExtractUsage 从非流式响应体解析 token 用量；usage 缺失/无法解析返回 ErrUsageMissing；
//   - HandleStream 逐块透传 SSE 并统计 usage，返回最终用量与拼装的 assistant 消息（call_log 用）。
type Provider interface {
	Name() string
	// BuildUpstreamRequest 构建上游请求，req.Body 为已改写 model 的下游请求体。
	BuildUpstreamRequest(ctx context.Context, req UpstreamRequest) (*http.Request, error)
	// ExtractUsage 从非流式响应体解析 token 用量；缺失/无法解析返回 ErrUsageMissing。
	ExtractUsage(body []byte) (billing.Usage, error)
	// HandleStream 透传流式 SSE，返回统计到的用量与拼装出的 assistant 消息 JSON；
	// usage 缺失返回 ErrUsageMissing（内容已透传，由调用方按 AC-17 处理）。
	HandleStream(ctx context.Context, upstream *http.Response, w http.ResponseWriter) (billing.Usage, []byte, error)
}
