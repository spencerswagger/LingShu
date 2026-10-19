package protocolconv

import (
	"fmt"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

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

// ConvertRequest 将请求体从 from 协议转换为 to 协议：解析为规范请求后按目标协议序列化，并合并两侧损耗报告。
func ConvertRequest(from, to string, body []byte) ([]byte, *LossyReport, error) {
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

	req, parseRep, err := fromCodec.ParseRequest(body)
	if err != nil {
		return nil, parseRep, err
	}
	if channelprotocol.Canonical(to) == channelprotocol.ProtocolAnthropicMessages && req != nil && len(req.Extra) > 0 {
		parseRep.Note("top-level extras dropped for anthropic target")
		req.Extra = nil
	}
	out, serRep, err := toCodec.SerializeRequest(req)
	if err != nil {
		return nil, serRep, err
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
		resp.FinishReason = nativeFinishReason(to, canonicalFinishReason(from, resp.FinishReason), rep)
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

// ConvertStreamEvent 将单个 from 协议流帧转换为 to 协议帧序列：先解码为规范事件再逐条编码，停止原因已由流编解码器归一。
func ConvertStreamEvent(from, to string, data []byte) ([][]byte, *LossyReport, error) {
	events, decRep, err := DecodeStreamEvents(from, data)
	if err != nil {
		return nil, decRep, err
	}
	frames, encRep, err := EncodeStreamEvents(to, events)
	if err != nil {
		return nil, encRep, err
	}
	rep := &LossyReport{}
	rep.Merge(decRep)
	rep.Merge(encRep)
	return frames, rep, nil
}

func canonicalFinishReason(protocol, native string) string {
	if native == "" {
		return ""
	}
	switch channelprotocol.Canonical(protocol) {
	case channelprotocol.ProtocolOpenAIChat:
		switch native {
		case "function_call":
			return stopReasonToolCalls
		}
	case channelprotocol.ProtocolOpenAIResponses:
		switch native {
		case "max_output_tokens":
			return stopReasonLength
		case "completed":
			return stopReasonStop
		}
	case channelprotocol.ProtocolAnthropicMessages:
		switch native {
		case "end_turn", "stop_sequence":
			return stopReasonStop
		case "max_tokens":
			return stopReasonLength
		case "tool_use":
			return stopReasonToolCalls
		}
	}
	return native
}

func nativeFinishReason(protocol, canonical string, rep *LossyReport) string {
	if canonical == "" {
		return ""
	}
	switch channelprotocol.Canonical(protocol) {
	case channelprotocol.ProtocolOpenAIChat:
		return canonical
	case channelprotocol.ProtocolOpenAIResponses:
		switch canonical {
		case stopReasonLength:
			return "max_output_tokens"
		case stopReasonContentFilter:
			return "content_filter"
		case stopReasonStop:
			return "stop"
		case stopReasonToolCalls:
			return "tool_calls"
		}
		return canonical
	case channelprotocol.ProtocolAnthropicMessages:
		switch canonical {
		case stopReasonStop:
			return "end_turn"
		case stopReasonLength:
			return "max_tokens"
		case stopReasonToolCalls:
			return "tool_use"
		case stopReasonContentFilter:
			rep.Note("stop reason content_filter remapped to end_turn for anthropic target")
			return "end_turn"
		}
	}
	return canonical
}
