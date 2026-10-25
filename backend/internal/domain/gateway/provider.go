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
// 对象优先 type、其次 code，再拼 message；空值返回空串。
// 注意：`{"error":{}}` 这类无字段 error 帧同样返回空串，所以「返回空串」不等于「没有 error 帧」——
// 调用方必须按帧类型（response.failed / EventError）或 error 键是否存在且非 null 来置位失败，
// 不能只看摘要是否为空。
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
	// UsageMissingDoneNoUsage 流按协议正常收尾（收到内容层收尾证据）但无 usage：平台已为
	// OpenAI 兼容流式请求注入 stream_options.include_usage=true，此类属上游忽略该参数
	// （协议偏差），不惩罚渠道。
	UsageMissingDoneNoUsage = "done_no_usage"
	// UsageMissingTruncated 既未收到流结束标记、也未收到内容层收尾证据即读到 EOF（上游流被截断），
	// 计入渠道健康失败。
	UsageMissingTruncated = "truncated"
	// UsageMissingTerminatorOnly 收到流结束标记（[DONE] / message_stop / 终止事件），但没有内容层
	// 收尾证据（finish_reason / message_delta / response.completed）：典型形态是代理在上游被截断后
	// 兜底补一个 [DONE] 掩盖截断。终止标记对中间层是零成本可伪造的，不能单独证明「正常结束」，
	// 故计入渠道健康失败——否则渠道健康 / 调用日志 / 计费三个观测面会同时看不见这类坏渠道。
	UsageMissingTerminatorOnly = "terminator_only"
	// UsageMissingErrorFrame 上游在流内下发 error 帧后结束，计入渠道健康失败；
	// 该归因优先于任何结束标记（见 streamEnd.reason）。
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
// 仅「流被截断 / 只有结束标记没有内容收尾证据 / 流内 error / 无法区分」计入；
// 「协议正常收尾但无 usage」（上游忽略 include_usage）与客户端断开不惩罚（后者由调用方先行判定）。
func usageMissingPunishable(reason string) bool {
	switch reason {
	case "", UsageMissingDoneNoUsage:
		return false
	default:
		return true
	}
}

// streamEnd 汇总判断「流是否正常结束」的两类独立证据，供 usage 缺失归因交叉验证。
//
// 只认终止标记是不够的：终止标记（openai-chat 的 [DONE]、anthropic 的 message_stop、
// responses 的终止事件）只说明「有人宣布流结束了」，而中间的代理 / 兼容层往往在字节流结束时
// 无条件补一个 [DONE]——上游被截断、根本拿不到真实终止事件时同样会补。若把这种「补发的标记」
// 当成正常结束，渠道健康 / 调用日志 / 计费三个观测面会同时看不见这个坏渠道，等于永远不处罚。
//
// 所以「正常结束」必须再叠加内容层收尾证据（ContentComplete）：
//   - openai-chat：choices[].finish_reason 非空（上游真实生成完毕才有的字段）；
//   - anthropic-messages：message_delta 且 stop_reason 非空；
//   - openai-responses：response.completed / response.incomplete 终止事件本身。
//
// 这类证据是上游真实产出的内容，代理要伪造成本高得多；只有标记而没有内容证据时归因
// UsageMissingTerminatorOnly（计入健康失败，但归因与「干脆没有标记」的 truncated 可区分）。
//
// 连接层信号（非 io.EOF 的读错误 / 超时 / 连接被重置）不进本结构：各适配器读到即直接返回原始
// 错误，由 handler 按「流式传输中断」处置（同样回喂健康失败），无需在此重复判定。
type streamEnd struct {
	Terminal        bool // 是否收到流结束标记
	ContentComplete bool // 是否收到内容层收尾证据
}

// normal 判断是否可归因为「正常结束但无 usage」：内容层收尾证据是必要条件（标记单独不算）。
func (e streamEnd) normal() bool { return e.ContentComplete }

// reason 按 error_frame > done_no_usage > terminator_only > truncated 归因。
// error 帧优先：上游已明确报错时，随后的终止标记只是收口，不能把失败洗成「正常结束无 usage」。
func (e streamEnd) reason(sawError bool) string {
	switch {
	case sawError:
		return UsageMissingErrorFrame
	case e.normal():
		return UsageMissingDoneNoUsage
	case e.Terminal:
		return UsageMissingTerminatorOnly
	default:
		return UsageMissingTruncated
	}
}

// usageMissingReasonOf 统一各协议适配器的 usage 缺失归因（口径见 streamEnd.reason）：
// 上游已明确报错时，随后的 [DONE]/终止事件只是收口，不能把失败洗成「正常结束无 usage」，
// 否则渠道健康惩罚会丢失。只有整段确实出现过 usage 时调用方才不报缺失（不受本函数影响）。
// 各适配器的 Terminal/ContentComplete 语义由自身协议定义，归因口径保持一致。
func usageMissingReasonOf(end streamEnd, sawError bool) string {
	return end.reason(sawError)
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
