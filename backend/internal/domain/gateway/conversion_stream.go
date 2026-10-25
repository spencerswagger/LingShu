package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
	"github.com/team/llmgateway/internal/domain/protocolconv"
)

// canonicalToolCall 流式拼装中的单个工具调用（规范块下标 → 累积状态）。
type canonicalToolCall struct {
	id        string
	typ       string
	name      string
	arguments string
}

// pumpConvertedStream 是跨协议流式转换泵：把上游 SSE 逐帧解码为规范事件，按入站协议重新编码后下发。
// 与各 Provider.HandleStream 的返回契约一致（usage、上游原始 usage JSONB、assistant 消息 JSON、error）：
// 上游无 usage 时返回 UsageMissingError，并按流结束形态区分原因（[DONE]/终止事件 = 正常结束无 usage；
// 未收到终止标记即 EOF = 截断）。流内 error 帧能否归因为 error_frame 取决于上游协议解码器：
// anthropic / openai-responses 有 error 映射；openai-chat 无 error 映射，error 帧会落入 truncated
// （两类原因都计入渠道健康惩罚，仅归因文案不同）。
func (g *Gateway) pumpConvertedStream(ctx context.Context, upstream *http.Response, inboundProto, upstreamProto string, lossy *protocolconv.LossyReport, w http.ResponseWriter) (billing.Usage, []byte, []byte, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	br := bufio.NewReader(upstream.Body)

	downstream := channelprotocol.Canonical(inboundProto)

	var usage billing.Usage
	sawUsage := false
	sawDone := false               // 是否收到上游终止标记（[DONE] / 终止事件）
	sawError := false              // 是否出现流内 error 帧
	var errorFrame string          // error 帧摘要（诊断用）
	var rawParts []json.RawMessage // 上游原始 usage 分片（携带 usage 的事件原文）
	upstreamResponses := channelprotocol.Canonical(upstreamProto) == channelprotocol.ProtocolOpenAIResponses

	var content, reasoning bytes.Buffer
	toolCalls := map[int]*canonicalToolCall{}
	var toolOrder []int

	started := false
	sawToolCall := false
	anthropicDown := downstream == channelprotocol.ProtocolAnthropicMessages

	// 下游 anthropic 的内容块状态：openBlocks 记录已开启的下游块下标；
	// blockIndex 把上游规范下标重映射为「下游连续、从 0 起始」的下标。
	openBlocks := map[int]bool{}
	blockIndexMap := map[int]int{}
	nextBlockIndex := 0
	blockIndex := func(up int) int {
		if !anthropicDown {
			return up
		}
		if d, ok := blockIndexMap[up]; ok {
			return d
		}
		d := nextBlockIndex
		blockIndexMap[up] = d
		nextBlockIndex++
		return d
	}

	// 待下发的终止事件：合并 stop_reason 与 usage，保证每次流仅下发一个终止帧
	// （openai include_usage 会把 finish_reason 与 usage 拆到两帧，需合并）。
	var pendingTerminal *protocolconv.CanonicalEvent
	bufferTerminal := func(ev protocolconv.CanonicalEvent) {
		if pendingTerminal == nil {
			cp := ev
			pendingTerminal = &cp
			return
		}
		if ev.StopReason != "" {
			pendingTerminal.StopReason = ev.StopReason
		}
		if ev.HasUsage {
			pendingTerminal.Usage = ev.Usage
			pendingTerminal.HasUsage = true
		}
	}

	// closeOpenBlocks / flushTerminal 依赖 emit，定义在 emit 之后。

	asm := func() []byte {
		return marshalConvertedAssistant(content.String(), reasoning.String(), toolCalls, toolOrder)
	}
	fail := func(err error) (billing.Usage, []byte, []byte, error) {
		return usage, rawUsageJSON(upstreamProto, rawParts), asm(), err
	}

	writeFrames := func(frames [][]byte) error {
		for _, fr := range frames {
			if len(fr) == 0 {
				continue
			}
			if _, werr := w.Write(fr); werr != nil {
				return wrapDownstreamWrite(werr)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		return nil
	}

	emit := func(evs []channelEvent) error {
		frames, _, eerr := protocolconv.EncodeStreamEvents(downstream, evs)
		if eerr != nil {
			return eerr
		}
		return writeFrames(frames)
	}

	// closeOpenBlocks 为下游 anthropic 补出所有已开启块的 content_block_stop（升序、幂等）。
	closeOpenBlocks := func() error {
		if !anthropicDown || len(openBlocks) == 0 {
			return nil
		}
		idxs := make([]int, 0, len(openBlocks))
		for i := range openBlocks {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		for _, i := range idxs {
			if err := emit([]channelEvent{{Type: protocolconv.EventContentBlockStop, Index: i}}); err != nil {
				return err
			}
		}
		openBlocks = map[int]bool{}
		return nil
	}

	// flushTerminal 下发（若有）缓冲的终止事件；下游 anthropic 先关闭所有开启块再发 message_delta。
	flushTerminal := func() error {
		if pendingTerminal == nil {
			return nil
		}
		ev := *pendingTerminal
		pendingTerminal = nil
		if err := closeOpenBlocks(); err != nil {
			return err
		}
		return emit([]channelEvent{ev})
	}

	// forward 处理单个规范事件：先补流式状态（message_start / content_block_start），再编码下发；
	// 终止事件（message_delta）先缓冲，待流末尾或遇非终止事件前合并下发一次。
	forward := func(ev protocolconv.CanonicalEvent) error {
		// 工具调用标记：出现工具块/工具增量即置位。
		if (ev.Type == protocolconv.EventContentBlockStart && ev.BlockKind == protocolconv.BlockToolUse) ||
			(ev.Type == protocolconv.EventContentBlockDelta && ev.DeltaKind == protocolconv.DeltaToolJSON) {
			sawToolCall = true
		}

		if ev.Type == protocolconv.EventMessageDelta {
			if anthropicDown && !started {
				started = true
				if err := emit([]channelEvent{{Type: protocolconv.EventMessageStart}}); err != nil {
					return err
				}
			}
			if !anthropicDown {
				started = true
			}
			// 下游 openai-responses：命中工具调用时把最终 completed 的停止原因定为 tool_calls。
			if downstream == channelprotocol.ProtocolOpenAIResponses && sawToolCall {
				ev.StopReason = "tool_calls"
			}
			// 最终 usage 帧携带累积用量（含 anthropic message_start 的输入/缓存段，避免丢失）。
			if sawUsage {
				ev.Usage = usage
				ev.HasUsage = true
			}
			bufferTerminal(ev)
			return nil
		}

		// 非终止事件：先冲刷缓冲的终止帧，保证帧序（如 anthropic message_stop 前先出 message_delta）。
		if err := flushTerminal(); err != nil {
			return err
		}

		if !anthropicDown {
			started = true
			return emit([]channelEvent{ev})
		}

		// 下游 anthropic：确保 message_start，并按需补 content_block_start。
		if !started {
			started = true
			if ev.Type != protocolconv.EventMessageStart {
				if err := emit([]channelEvent{{Type: protocolconv.EventMessageStart}}); err != nil {
					return err
				}
			}
		}
		switch ev.Type {
		case protocolconv.EventMessageStart:
			return emit([]channelEvent{ev})
		case protocolconv.EventContentBlockStart:
			idx := blockIndex(ev.Index)
			openBlocks[idx] = true
			ev.Index = idx
			return emit([]channelEvent{ev})
		case protocolconv.EventContentBlockDelta:
			idx := blockIndex(ev.Index)
			if !openBlocks[idx] {
				kind := protocolconv.BlockText
				if ev.DeltaKind == protocolconv.DeltaToolJSON {
					kind = protocolconv.BlockToolUse
				}
				openBlocks[idx] = true
				if err := emit([]channelEvent{{Type: protocolconv.EventContentBlockStart, Index: idx, BlockKind: kind}}); err != nil {
					return err
				}
			}
			ev.Index = idx
			return emit([]channelEvent{ev})
		default:
			return emit([]channelEvent{ev})
		}
	}

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(trimmed[len("data:"):])
				if len(payload) > 0 && bytes.Equal(payload, []byte("[DONE]")) {
					sawDone = true
				} else if len(payload) > 0 {
					evs, _, derr := protocolconv.DecodeStreamEvents(upstreamProto, payload)
					if derr != nil {
						// 上游畸形帧不得静默丢弃：记入本次损耗诊断（随调用日志决策轨迹落痕）并告警，
						// 但不中断流（单帧坏不终止整条流）。
						lossy.Note("upstream malformed stream frame dropped: protocol=" + upstreamProto + " err=" + derr.Error())
						g.logger.Warn("upstream malformed stream frame dropped", "upstream_protocol", upstreamProto, "err", derr)
					} else {
						carriedUsage := false
						for _, ev := range evs {
							if ev.HasUsage {
								carriedUsage = true
								sawUsage = true
								mergeConvertedUsage(&usage, upstreamProto, ev)
							}
							switch {
							case ev.Type == protocolconv.EventMessageStop:
								sawDone = true
							case upstreamResponses && ev.Type == protocolconv.EventMessageDelta:
								// openai-responses 的终止帧（response.completed/incomplete）解码为
								// message_delta，该协议没有 EventMessageStop，故以此判定「正常结束」。
								sawDone = true
							case ev.Type == protocolconv.EventError:
								sawError = true
								if errorFrame == "" {
									errorFrame = ev.ErrMessage
								}
							}
							accumulateConvertedAssistant(&content, &reasoning, toolCalls, &toolOrder, ev)
						}
						if upstreamResponses {
							if detail := responsesFailedFrame(payload); detail != "" {
								sawError = true
								errorFrame = detail
							}
						}
						if carriedUsage {
							if part := streamUsagePart(upstreamProto, payload); len(part) > 0 {
								rawParts = append(rawParts, part)
							}
						}
						for _, ev := range evs {
							if ferr := forward(ev); ferr != nil {
								return fail(ferr)
							}
						}
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fail(err)
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		default:
		}
	}

	// EOF：先冲刷缓冲的终止帧（合并 stop_reason + usage，仅下发一次）；
	// 下游为 anthropic 且已开始流式 → 先关闭所有开启块，再补末尾 message_stop。
	if ferr := flushTerminal(); ferr != nil {
		return fail(ferr)
	}
	if anthropicDown && started {
		if ferr := closeOpenBlocks(); ferr != nil {
			return fail(ferr)
		}
		if ferr := emit([]channelEvent{{Type: protocolconv.EventMessageStop}}); ferr != nil {
			return fail(ferr)
		}
	}

	if !sawUsage {
		return usage, rawUsageJSON(upstreamProto, rawParts), asm(),
			&UsageMissingError{Reason: usageMissingReasonOf(sawDone, sawError), Detail: errorFrame}
	}
	return usage, rawUsageJSON(upstreamProto, rawParts), asm(), nil
}

// responsesFailedFrame 从 openai-responses 上游帧原文提取 response.failed 的错误摘要；
// 该事件在解码器中没有对应规范事件（视为空事件），故按原文探测以便归因到 error_frame。
func responsesFailedFrame(payload []byte) string {
	// 先按字节探测：绝大多数帧不是 response.failed，避免为每帧多做一次反序列化。
	if !bytes.Contains(payload, []byte("response.failed")) {
		return ""
	}
	var probe struct {
		Type     string          `json:"type"`
		Error    json.RawMessage `json:"error"`
		Response struct {
			Error json.RawMessage `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &probe) != nil || probe.Type != "response.failed" {
		return ""
	}
	if d := errorFrameDetail(probe.Response.Error); d != "" {
		return d
	}
	return errorFrameDetail(probe.Error)
}

// channelEvent 是 protocolconv.CanonicalEvent 的别名，便于在泵内构造事件字面量。
type channelEvent = protocolconv.CanonicalEvent

// mergeConvertedUsage 按上游物理协议的 provider 语义合并用量分片：
// openai-chat / openai-responses 每次携带 usage 的分片整体覆盖（对齐 openai.go parseStreamData
// 与 responses.go 的 `usage = u`）；anthropic 分源于 message_start（输入/缓存）与 message_delta
// （输出），按字段取源（对齐 anthropic.go 的 usage 合并），而非逐字段非零覆盖。
func mergeConvertedUsage(dst *billing.Usage, upstreamProto string, ev protocolconv.CanonicalEvent) {
	switch channelprotocol.Canonical(upstreamProto) {
	case channelprotocol.ProtocolAnthropicMessages:
		if ev.Type == protocolconv.EventMessageStart {
			dst.Input = ev.Usage.Input
			dst.CacheRead = ev.Usage.CacheRead
			return
		}
		dst.Output = ev.Usage.Output
	default:
		*dst = ev.Usage
	}
}

// accumulateConvertedAssistant 把单个规范事件累积进 assistant 拼装状态（正文/推理/工具调用）。
func accumulateConvertedAssistant(content, reasoning *bytes.Buffer, toolCalls map[int]*canonicalToolCall, order *[]int, ev protocolconv.CanonicalEvent) {
	switch ev.Type {
	case protocolconv.EventContentBlockDelta:
		switch ev.DeltaKind {
		case protocolconv.DeltaText:
			content.WriteString(ev.Text)
		case protocolconv.DeltaThinking:
			reasoning.WriteString(ev.Text)
		case protocolconv.DeltaToolJSON:
			ensureCanonicalToolCall(toolCalls, order, ev.Index).arguments += ev.Text
		}
	case protocolconv.EventContentBlockStart:
		if ev.BlockKind == protocolconv.BlockToolUse {
			tc := ensureCanonicalToolCall(toolCalls, order, ev.Index)
			if ev.ToolUseID != "" {
				tc.id = ev.ToolUseID
			}
			if ev.ToolName != "" {
				tc.name = ev.ToolName
			}
			if tc.typ == "" {
				tc.typ = "function"
			}
		}
	}
}

// ensureCanonicalToolCall 取（或创建）指定规范块下标的工具调用，并记录首次出现顺序。
func ensureCanonicalToolCall(toolCalls map[int]*canonicalToolCall, order *[]int, idx int) *canonicalToolCall {
	tc, ok := toolCalls[idx]
	if !ok {
		tc = &canonicalToolCall{}
		toolCalls[idx] = tc
		*order = append(*order, idx)
	}
	return tc
}

// marshalConvertedAssistant 按本包既有约定拼装 assistant 消息 JSON：
// {"role":"assistant","content":...,"reasoning_content":...,"tool_calls":[{"index","id","type","function":{"name","arguments"}}]}，
// 空字段省略，tool_calls 按块下标升序（供 assistantParts / call_log 复用）。
func marshalConvertedAssistant(content, reasoning string, toolCalls map[int]*canonicalToolCall, order []int) []byte {
	msg := map[string]any{"role": "assistant"}
	if content != "" {
		msg["content"] = content
	}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		idx := append([]int(nil), order...)
		sort.Ints(idx)
		tcs := make([]map[string]any, 0, len(idx))
		for _, i := range idx {
			tc := toolCalls[i]
			m := map[string]any{"index": i}
			if tc.id != "" {
				m["id"] = tc.id
			}
			if tc.typ != "" {
				m["type"] = tc.typ
			}
			fn := map[string]any{}
			if tc.name != "" {
				fn["name"] = tc.name
			}
			if tc.arguments != "" {
				fn["arguments"] = tc.arguments
			}
			if len(fn) > 0 {
				m["function"] = fn
			}
			tcs = append(tcs, m)
		}
		msg["tool_calls"] = tcs
	}
	b, _ := json.Marshal(msg)
	return b
}

// streamUsagePart 从上游单帧原文中提取 usage 子对象（供 rawUsageJSON 合并，诊断渠道口径）。
// 三协议位置：anthropic → message.usage / usage；openai-responses → response.usage；openai-chat → choices[].usage / usage。
func streamUsagePart(protocol string, payload []byte) json.RawMessage {
	switch channelprotocol.Canonical(protocol) {
	case channelprotocol.ProtocolAnthropicMessages:
		var top struct {
			Message struct {
				Usage json.RawMessage `json:"usage"`
			} `json:"message"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(payload, &top) != nil {
			return nil
		}
		if !isNullRaw(top.Usage) {
			return top.Usage
		}
		if !isNullRaw(top.Message.Usage) {
			return top.Message.Usage
		}
	case channelprotocol.ProtocolOpenAIResponses:
		var top struct {
			Response struct {
				Usage json.RawMessage `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(payload, &top) != nil {
			return nil
		}
		if !isNullRaw(top.Response.Usage) {
			return top.Response.Usage
		}
	case channelprotocol.ProtocolOpenAIChat:
		var top struct {
			Choices []struct {
				Usage json.RawMessage `json:"usage"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(payload, &top) != nil {
			return nil
		}
		for _, c := range top.Choices {
			if !isNullRaw(c.Usage) {
				return c.Usage
			}
		}
		if !isNullRaw(top.Usage) {
			return top.Usage
		}
	}
	return nil
}

// isNullRaw 判断 JSON RawMessage 是否为空或 null。
func isNullRaw(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null"
}
