// Package gateway 实现 LLM 网关的核心热路径：OpenAI-compat 代理转发、
// 渠道透明（不泄漏渠道名/上游地址）、限流与计费接线。
//
// 设计约束：
//   - 对下游额度屏蔽渠道与上游实现细节（渠道透明）；
//   - 非流式请求「先计费再响应」，余额不足直接 402，不返成功 body；
//   - 流式请求因无法回改响应头，计费在流结束后执行，失败仅记审计日志。
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/team/llmgateway/internal/domain/billing"
)

// ErrUsageMissing 表示上游响应缺失或无法解析 usage（AC-17 放宽：由调用方按可见内容估算计费）。
// 计费恒不丢失；是否惩罚渠道按缺失原因区分（见 UsageMissing* 常量与 usageMissingPunishable）。
var ErrUsageMissing = errors.New("upstream response missing usage")

// errDownstreamWrite 标记「SSE 写回下游失败」。它不是上游故障：若按普通错误上抛，
// handler 会当作上游读侧中断回喂渠道健康，把健康上游误判为故障（误杀熔断）。
var errDownstreamWrite = errors.New("downstream write failed")

// wrapDownstreamWrite 把下游写失败包装为可识别哨兵错误，同时保留原始错误供日志/诊断。
func wrapDownstreamWrite(err error) error {
	return fmt.Errorf("%w: %w", errDownstreamWrite, err)
}

// isDownstreamWriteErr 判定错误是否来自下游写失败，供 handler 区分「上游读侧中断」与「下游写失败」。
func isDownstreamWriteErr(err error) bool {
	return errors.Is(err, errDownstreamWrite)
}

// errorSummary 是各协议流内 error 帧的宽松结构：字段名跨协议兼容（type/message/code）。
type errorSummary struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    any    `json:"code"`
}

// errorFrameDetail 从流内 error 帧的 error 值原文提取可读摘要：字符串直接返回；
// 对象优先 type、其次 code，再拼 message；空值返回空串（无 error 帧）。
// OpenAI / Anthropic / Responses 的 error 负载形状相近，故三协议共用同一解析。
func errorFrameDetail(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(t, &s) == nil {
		return s
	}
	var e errorSummary
	if json.Unmarshal(t, &e) != nil {
		return ""
	}
	prefix := e.Type
	if prefix == "" {
		if c, ok := e.Code.(string); ok {
			prefix = c
		}
	}
	switch {
	case e.Message == "":
		return prefix
	case prefix == "":
		return e.Message
	default:
		return prefix + ": " + e.Message
	}
}

// 流式 usage 缺失的可区分原因：计费一律按可见内容估算，但渠道健康惩罚按原因区别对待。
const (
	// UsageMissingDoneNoUsage 收到流结束标记但无 usage：平台已为 OpenAI 兼容流式请求注入
	// stream_options.include_usage=true，此类属上游忽略该参数（协议偏差），不惩罚渠道。
	UsageMissingDoneNoUsage = "done_no_usage"
	// UsageMissingTruncated 未收到流结束标记即读到 EOF（上游流被截断），计入渠道健康失败。
	UsageMissingTruncated = "truncated"
	// UsageMissingErrorFrame 上游在流内下发 error 帧后结束，计入渠道健康失败。
	UsageMissingErrorFrame = "error_frame"
	// UsageMissingUnknown 适配器无法区分流的结束形态（非 OpenAI 兼容协议），计入渠道健康失败。
	UsageMissingUnknown = "unknown"
)

// UsageMissingError 携带缺失原因的 usage 缺失错误；errors.Is(err, ErrUsageMissing) 仍成立。
type UsageMissingError struct {
	Reason string // 取值见 UsageMissing* 常量
	Detail string // 上游 error 帧摘要（可选，仅诊断用）
}

// Error 实现 error。
func (e *UsageMissingError) Error() string {
	if e.Detail != "" {
		return "upstream response missing usage (" + e.Reason + "): " + e.Detail
	}
	return "upstream response missing usage (" + e.Reason + ")"
}

// Unwrap 使 errors.Is(err, ErrUsageMissing) 成立。
func (e *UsageMissingError) Unwrap() error { return ErrUsageMissing }

// usageMissingReason 返回 usage 缺失原因；err 不是 usage 缺失时返回空串。
func usageMissingReason(err error) string {
	var ue *UsageMissingError
	if errors.As(err, &ue) {
		return ue.Reason
	}
	if errors.Is(err, ErrUsageMissing) {
		return UsageMissingUnknown
	}
	return ""
}

// usageMissingPunishable 判断 usage 缺失是否应计入渠道健康失败：
// 仅「流被截断 / 流内 error / 无法区分」计入；「正常结束但无 usage」（上游忽略
// include_usage）与客户端断开不惩罚（后者由调用方先行判定）。
func usageMissingPunishable(reason string) bool {
	switch reason {
	case "", UsageMissingDoneNoUsage:
		return false
	default:
		return true
	}
}

// usageMissingReasonOf 统一各协议适配器的 usage 缺失归因：优先采信「收到终止标记」
// （正常结束但上游忽略 include_usage），其次「流内 error」，其余按流被截断处理。
// 各适配器的 sawDone/sawError 语义由自身协议定义，归因口径保持一致。
func usageMissingReasonOf(sawDone, sawError bool) string {
	switch {
	case sawDone:
		return UsageMissingDoneNoUsage
	case sawError:
		return UsageMissingErrorFrame
	default:
		return UsageMissingTruncated
	}
}

// Provider 抽象一个上游基础协议适配器。基础协议为代码固定的 3 个：
// openai-chat / openai-responses / anthropic-messages。
//
// 实现要点：
//   - BuildUpstreamRequest 依据构造上下文（基址/路径/凭据/下游头/头策略/鉴权）构建上游请求；
//   - ExtractUsage 从非流式响应体解析 token 用量；usage 缺失/无法解析返回 ErrUsageMissing；
//   - HandleStream 逐块透传 SSE 并统计 usage，返回最终用量与拼装的 assistant 消息（call_log 用）。
//
// 两个方法都额外返回「上游原始 usage」的落库值（call_logs.upstream_usage，见 rawusage.go）：
// 诊断各渠道 usage 口径不一致时（如输入是否含缓存读）必须看上游原文，而不是只看向量后的三段。
type Provider interface {
	Name() string
	// BuildUpstreamRequest 构建上游请求，req.Body 为已改写 model 的下游请求体。
	BuildUpstreamRequest(ctx context.Context, req UpstreamRequest) (*http.Request, error)
	// ExtractUsage 从非流式响应体解析 token 用量与上游原始 usage（JSONB 值，nil=无）；
	// 缺失/无法解析返回 ErrUsageMissing。
	ExtractUsage(body []byte) (billing.Usage, []byte, error)
	// HandleStream 透传流式 SSE，返回统计到的用量、上游原始 usage（多段分片合并，JSONB 值）与
	// 拼装出的 assistant 消息 JSON；usage 缺失返回 ErrUsageMissing（内容已透传，由调用方估算计费）。
	HandleStream(ctx context.Context, upstream *http.Response, w http.ResponseWriter) (billing.Usage, []byte, []byte, error)
}
