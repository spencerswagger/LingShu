package protocolconv

import (
	"errors"
	"fmt"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// 转换失败阶段取值：StageParse 为入站请求体解析失败（候选无关），
// StageSerialize 为目标协议序列化失败（含未知协议，可转移）。
const (
	StageParse     = "parse"
	StageSerialize = "serialize"
)

// ConvertError 描述一次请求转换失败及其所处阶段，供上层按阶段区分处置。
type ConvertError struct {
	Stage string
	Err   error
}

// Error 实现 error 接口。
func (e *ConvertError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("protocolconv: %s stage: %v", e.Stage, e.Err)
}

// Unwrap 暴露底层错误，供 errors.Is/As 追溯。
func (e *ConvertError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ConvertStage 返回转换错误所处阶段（StageParse/StageSerialize）；非 ConvertError 返回空串。
func ConvertStage(err error) string {
	var ce *ConvertError
	if errors.As(err, &ce) {
		return ce.Stage
	}
	return ""
}

// defaultTargetOrder 是未指定 override 时选择上游协议的默认偏好顺序。
var defaultTargetOrder = []string{
	channelprotocol.ProtocolOpenAIChat,
	channelprotocol.ProtocolAnthropicMessages,
	channelprotocol.ProtocolOpenAIResponses,
}

// SelectUpstreamProtocol 在入站协议与上游支持协议集之间挑选目标协议：
// override 非空时取其中首个受支持项，否则按默认偏好顺序取首个受支持项；
// 目标不得与入站协议（归一化后）相同，若唯一受支持项即入站本身则跳过并返回 ("", false)。
func SelectUpstreamProtocol(inbound string, supported []string, override []string) (string, bool) {
	supportedSet := make(map[string]bool, len(supported))
	for _, s := range supported {
		supportedSet[channelprotocol.Canonical(s)] = true
	}
	inboundCanonical := channelprotocol.Canonical(inbound)

	if len(override) > 0 {
		for _, o := range override {
			name := channelprotocol.Canonical(o)
			if name == inboundCanonical {
				continue
			}
			if supportedSet[name] {
				return name, true
			}
		}
		return "", false
	}

	for _, p := range defaultTargetOrder {
		if p == inboundCanonical {
			continue
		}
		if supportedSet[p] {
			return p, true
		}
	}
	return "", false
}

func codecFor(protocol string) (Codec, bool) {
	switch channelprotocol.Canonical(protocol) {
	case channelprotocol.ProtocolOpenAIChat:
		return openAIChatCodec{}, true
	case channelprotocol.ProtocolOpenAIResponses:
		return openaiResponsesCodec{}, true
	case channelprotocol.ProtocolAnthropicMessages:
		return anthropicCodec{}, true
	}
	return nil, false
}

// SerializeOptions 是目标协议序列化阶段的可选行为开关（默认全关闭，保持安全态）。
type SerializeOptions struct {
	// ReasoningContent 为 true 时，anthropic-messages → openai-chat 转换会把 assistant
	// 历史的 thinking 块回填为推理字段。仅当目标上游确实支持该字段时才可开启：
	// DeepSeek 等 thinking 模型在多轮/tool 循环中要求回传，其余兼容实现可能按未知消息
	// 字段拒绝（400），因此默认关闭，由渠道协议显式声明。
	ReasoningContent bool
	// ReasoningField 是 ReasoningContent 开启时写入的字段名（空 = reasoning_content）。
	// 由渠道协议声明，与直通路径的回传字段保持一致，避免同一上游出现两种字段名。
	ReasoningField string
}

// reasoningFieldName 返回实际写入的推理字段名（缺省 reasoning_content）。
func (o SerializeOptions) reasoningFieldName() string {
	if o.ReasoningField == "" {
		return "reasoning_content"
	}
	return o.ReasoningField
}

// ConvertRequestWithOptions 将请求体从 from 协议转换为 to 协议：解析为规范请求后按目标协议序列化，并合并两侧损耗报告。
// opts 为默认零值时即全部关闭的安全态。
func ConvertRequestWithOptions(from, to string, body []byte, opts SerializeOptions) ([]byte, *LossyReport, error) {
	fromCodec, ok := codecFor(from)
	if !ok {
		return nil, nil, &ConvertError{Stage: StageSerialize, Err: fmt.Errorf("protocolconv: unknown source protocol %q", from)}
	}
	toCodec, ok := codecFor(to)
	if !ok {
		return nil, nil, &ConvertError{Stage: StageSerialize, Err: fmt.Errorf("protocolconv: unknown target protocol %q", to)}
	}

	if channelprotocol.Canonical(from) == channelprotocol.Canonical(to) {
		return body, &LossyReport{}, nil
	}

	req, parseRep, err := fromCodec.ParseRequest(body)
	if err != nil {
		return nil, parseRep, &ConvertError{Stage: StageParse, Err: err}
	}
	if channelprotocol.Canonical(to) == channelprotocol.ProtocolAnthropicMessages && req != nil && len(req.Extra) > 0 {
		parseRep.Note("top-level extras dropped for anthropic target")
		req.Extra = nil
	}
	var out []byte
	var serRep *LossyReport
	if oc, ok := toCodec.(serializeOptionsCodec); ok {
		out, serRep, err = oc.SerializeRequestWithOptions(req, opts)
	} else {
		out, serRep, err = toCodec.SerializeRequest(req)
	}
	if err != nil {
		return nil, serRep, &ConvertError{Stage: StageSerialize, Err: err}
	}

	rep := &LossyReport{}
	rep.Merge(parseRep)
	rep.Merge(serRep)
	return out, rep, nil
}

// ConvertResponse 将响应体从 from 协议转换为 to 协议：解析后把停止原因经规范词表中转再落地到目标协议，随后序列化。
func ConvertResponse(from, to string, body []byte) ([]byte, *LossyReport, error) {
	fromCodec, ok := codecFor(from)
	if !ok {
		return nil, nil, fmt.Errorf("protocolconv: unknown source protocol %q", from)
	}
	toCodec, ok := codecFor(to)
	if !ok {
		return nil, nil, fmt.Errorf("protocolconv: unknown target protocol %q", to)
	}

	if channelprotocol.Canonical(from) == channelprotocol.Canonical(to) {
		return body, &LossyReport{}, nil
	}

	resp, parseRep, err := fromCodec.ParseResponse(body)
	if err != nil {
		return nil, parseRep, err
	}

	rep := &LossyReport{}
	rep.Merge(parseRep)
	if resp != nil {
		resp.FinishReason = stopReasonFromCanonical(to, stopReasonToCanonical(from, resp.FinishReason), rep)
	}

	out, serRep, err := toCodec.SerializeResponse(resp)
	if err != nil {
		return nil, serRep, err
	}

	rep.Merge(serRep)
	return out, rep, nil
}

// DecodeStreamEvents 解码单个 protocol 协议流帧为 0..n 个规范事件（供网关流式转换泵复用）。
func DecodeStreamEvents(protocol string, data []byte) ([]CanonicalEvent, *LossyReport, error) {
	codec, ok := codecFor(protocol)
	if !ok {
		return nil, nil, fmt.Errorf("protocolconv: unknown source protocol %q", protocol)
	}
	return codec.DecodeEvent(data)
}

// EncodeStreamEvents 将规范事件序列逐条编码为 protocol 协议流帧序列（供网关流式转换泵复用），并合并各帧损耗报告。
func EncodeStreamEvents(protocol string, events []CanonicalEvent) ([][]byte, *LossyReport, error) {
	codec, ok := codecFor(protocol)
	if !ok {
		return nil, nil, fmt.Errorf("protocolconv: unknown target protocol %q", protocol)
	}
	rep := &LossyReport{}
	var frames [][]byte
	for _, ev := range events {
		encoded, encRep, err := codec.EncodeEvent(ev)
		if err != nil {
			return nil, rep, err
		}
		rep.Merge(encRep)
		frames = append(frames, encoded...)
	}
	return frames, rep, nil
}
