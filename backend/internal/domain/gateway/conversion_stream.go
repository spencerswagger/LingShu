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
	"strconv"

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
// 上游无 usage 时返回 UsageMissingError，归因由 usageMissingReasonOf 统一裁决，
// error 帧优先于任何结束标记（error_frame > done_no_usage > terminator_only > truncated）——上游先报错
// 再补 [DONE]/终止事件时，收口标记不能把失败洗成「正常结束无 usage」，否则渠道惩罚会丢失。
// 「正常结束」另须交叉验证内容层收尾证据（见 streamEnd）：只有 [DONE]/message_stop 而缺
// finish_reason/stop_reason 时，是代理补发标记掩盖截断，同样要计入渠道健康失败。
// 流内 error 帧的探测方式取决于上游协议：
//   - anthropic 由 codec 映射为 EventError，泵内据此置 sawError；
//   - openai-responses 在 codec 中没有对应映射，由泵内 responsesFailedFrame 按原文探测 response.failed；
//   - openai-chat 的 codec DecodeEvent 没有 error 分支（error 帧解码为空事件），由泵内 openAIChatErrorFrame
//     按顶层 error 键探测。
//
// 三种方式都以「帧本身是 error」置位 sawError，空 error 负载（{}）同样算，摘要可能为空串；
// 因此不能靠摘要是否为空判断失败。
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
	sawDone := false               // 是否收到上游终止标记（[DONE] / 终止事件 / message_stop）
	sawContentEnd := false         // 是否收到内容层收尾证据（finish_reason / stop_reason / responses 终止帧）
	sawError := false              // 是否出现流内 error 帧
	var errorFrame string          // error 帧摘要（诊断用）
	var rawParts []json.RawMessage // 上游原始 usage 分片（携带 usage 的事件原文）
	upstreamResponses := channelprotocol.Canonical(upstreamProto) == channelprotocol.ProtocolOpenAIResponses
	upstreamChat := channelprotocol.Canonical(upstreamProto) == channelprotocol.ProtocolOpenAIChat

	var content, reasoning bytes.Buffer
	toolCalls := map[int]*canonicalToolCall{}
	var toolOrder []int

	started := false
	sawToolCall := false
	anthropicDown := downstream == channelprotocol.ProtocolAnthropicMessages

	// 下游 anthropic 的内容块状态：openBlocks 记录已开启的下游块下标。
	// blockIndex 把上游规范下标重映射为「下游连续、从 0 起始」的下标。
	//
	// 上游规范事件里 text 与 thinking 常共用同一上游下标（openai-chat 的 content 与
	// reasoning_content 都用 Index:0；openai-responses 的 output_text.delta 与
	// reasoning_text.delta 共用 output_index）。若按上游下标映射，两者会被并进同一个下游块
	// （首块被标成 text、块内却是 thinking_delta，协议上非法），客户端历史因此没有合法
	// thinking 块。故按「上游下标 × 块类别」分桶映射：text / thinking / tool 各自独立分配
	// 下游下标，保证 thinking 独立成块、text 独立成块、tool 独立成块。
	openBlocks := map[int]bool{}
	blockIndexMap := map[string]int{}
	// blockKeys 记录「下游下标 → blockIndexMap 键」的反向映射：块关闭时据此清掉映射，
	// 使上游再次出现同一「上游下标 × 块类别」时分配**新的**下游下标。
	// 官方流不复用 content_block index；若复用（如交错 thinking → text → thinking 又回到
	// 同一桶），按 index 归并的客户端会覆盖丢字、或把两段签名拼成 sig1sig2。
	blockKeys := map[int]string{}
	nextBlockIndex := 0
	// currentBlock 是当前正在下发的块下标。Anthropic 真实流是**块串行**的
	// （start → delta* → stop 之后才起下一块）；按「当前块」归属 delta 的客户端若遇到
	// 块未关闭就切到另一块，会把内容串到一起。故切块前必须先关旧块。
	currentBlock := -1
	// blockThinkingText 累积每个 thinking 块的**整块全文**：签名按全文生成，
	// 只哈希首个分片会让同一份推理因分片边界不同而得到不同签名（与 signature.go 的
	// 「同一份推理内容 → 同一签名」不符）。
	blockThinkingText := map[int]*bytes.Buffer{}
	// upstreamSignatureEmitted 记录「上游已显式下发 signature_delta」的块，避免重复生成签名。
	// 当前 converted 路径下游为 anthropic 时上游必非 anthropic（否则不是跨协议转换），
	// 故该分支实际不可达；保留作防御，若未来泵被复用于 anthropic→anthropic 仍正确。
	upstreamSignatureEmitted := map[int]bool{}
	blockClass := func(deltaKind string) string {
		switch deltaKind {
		case protocolconv.DeltaThinking, protocolconv.DeltaSignature:
			// signature_delta 属于 thinking 块（Anthropic 在 content_block_stop 前下发），
			// 与 thinking_delta 落入同一桶，避免被误判为 text 块。
			return "thinking"
		case protocolconv.DeltaToolJSON:
			return "tool"
		default:
			return "text"
		}
	}
	// blockClassOfKind 按 content_block_start 的 BlockKind 归类。
	blockClassOfKind := func(kind string) string {
		switch kind {
		case protocolconv.BlockToolUse:
			return "tool"
		case protocolconv.BlockThinking, protocolconv.BlockRedactedThinking:
			return "thinking"
		default:
			return "text"
		}
	}
	blockIndex := func(up int, class string) int {
		if !anthropicDown {
			return up
		}
		key := itoa(up) + "|" + class
		if d, ok := blockIndexMap[key]; ok {
			return d
		}
		d := nextBlockIndex
		blockIndexMap[key] = d
		blockKeys[d] = key
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

	// closeBlock 关闭单个已开启块：thinking 块先补 signature_delta（按整块全文生成，上游未给时），
	// 再 content_block_stop。块串行语义要求「停旧块」与新块 start 之间不交错。
	closeBlock := func(i int) error {
		if !openBlocks[i] {
			return nil
		}
		if !upstreamSignatureEmitted[i] {
			if buf := blockThinkingText[i]; buf != nil {
				sig := protocolconv.OpaqueThinkingSignature(buf.String())
				if err := emit([]channelEvent{{Type: protocolconv.EventContentBlockDelta, Index: i, DeltaKind: protocolconv.DeltaSignature, Text: sig}}); err != nil {
					return err
				}
			}
		}
		if err := emit([]channelEvent{{Type: protocolconv.EventContentBlockStop, Index: i}}); err != nil {
			return err
		}
		delete(openBlocks, i)
		delete(blockThinkingText, i)
		delete(upstreamSignatureEmitted, i)
		// 清掉「上游下标 × 块类别 → 下游下标」映射：官方流不复用 content_block index，
		// 同一上游桶再次出现时必须分配新的下游下标（严格单调递增），否则客户端按 index
		// 归并会覆盖丢字或拼接签名。
		if key, ok := blockKeys[i]; ok {
			delete(blockIndexMap, key)
			delete(blockKeys, i)
		}
		if currentBlock == i {
			currentBlock = -1
		}
		return nil
	}

	// closeOpenBlocks 关闭全部仍开启的块（升序、幂等）：用于整条流收尾。
	closeOpenBlocks := func() error {
		if !anthropicDown {
			return nil
		}
		idxs := make([]int, 0, len(openBlocks))
		for i := range openBlocks {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		for _, i := range idxs {
			if err := closeBlock(i); err != nil {
				return err
			}
		}
		return nil
	}

	// openBlock 开启一个下游块：先停当前块（块串行），再发 content_block_start。
	// startEV 提供块类型与上游携带的元数据（tool id/name、thinking 文本/签名、redacted data），
	// 使流式下 thinking/redacted_thinking 也能原样透传。
	openBlock := func(idx int, kind string, startEV channelEvent) error {
		if openBlocks[idx] && currentBlock == idx {
			return nil // 已开且是当前块：不重复发 start
		}
		if currentBlock >= 0 && currentBlock != idx {
			if err := closeBlock(currentBlock); err != nil {
				return err
			}
		}
		openBlocks[idx] = true
		currentBlock = idx
		startEV.Type = protocolconv.EventContentBlockStart
		startEV.Index = idx
		startEV.BlockKind = kind
		if kind == protocolconv.BlockThinking {
			buf := blockThinkingText[idx]
			if buf == nil {
				buf = &bytes.Buffer{}
				blockThinkingText[idx] = buf
			}
			if startEV.Text != "" {
				buf.WriteString(startEV.Text)
			}
			if startEV.Signature != "" {
				upstreamSignatureEmitted[idx] = true
			}
		}
		return emit([]channelEvent{startEV})
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
			// 上游显式发出的 content_block_start（典型为 tool_use；anthropic 上游还会有
			// thinking / redacted_thinking，携带 thinking 文本/签名、redacted data —— 一并透传）。
			idx := blockIndex(ev.Index, blockClassOfKind(ev.BlockKind))
			return openBlock(idx, ev.BlockKind, channelEvent{
				ToolUseID:    ev.ToolUseID,
				ToolName:     ev.ToolName,
				Text:         ev.Text,
				Signature:    ev.Signature,
				RedactedData: ev.RedactedData,
			})
		case protocolconv.EventContentBlockDelta:
			idx := blockIndex(ev.Index, blockClass(ev.DeltaKind))
			if !openBlocks[idx] || currentBlock != idx {
				// 下游 anthropic 的 content_block_start 必须与后续 delta 的块类型一致：
				// 此前只把 DeltaToolJSON 映射为 tool_use，其余（含 DeltaThinking）一律 text，
				// 导致客户端历史里没有合法 thinking 块（首块被标成 text，块内却是 thinking_delta，
				// 协议上非法组合），下一轮不带 thinking → 上游 400。此处按 delta 类型映射块类型：
				// thinking 增量 → thinking 块；tool_json → tool_use；其余 → text。
				kind := protocolconv.BlockText
				switch blockClass(ev.DeltaKind) {
				case "thinking":
					kind = protocolconv.BlockThinking
				case "tool":
					kind = protocolconv.BlockToolUse
				}
				startEV := channelEvent{}
				if kind == protocolconv.BlockToolUse {
					startEV.ToolUseID = ev.ToolUseID
					startEV.ToolName = ev.ToolName
				}
				if err := openBlock(idx, kind, startEV); err != nil {
					return err
				}
			}
			// thinking 增量：累积整块全文（签名按全文生成）。
			if ev.DeltaKind == protocolconv.DeltaThinking {
				if buf := blockThinkingText[idx]; buf != nil {
					buf.WriteString(ev.Text)
				}
			}
			// 上游显式下发的 signature_delta：标记（关闭时不再重复生成）。
			if ev.DeltaKind == protocolconv.DeltaSignature {
				upstreamSignatureEmitted[idx] = true
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
					// error 帧探测必须放在解码前：畸形 JSON 的 error 帧会让 DecodeStreamEvents 失败并被丢弃，
					// 若探测留在解码成功分支内，随后的 [DONE] 会把失败洗成 done_no_usage（丢失渠道惩罚）。
					// 两个探测函数自带字节预筛，绝大多数正常帧在预筛阶段即返回，成本可忽略。
					if upstreamResponses {
						if failed, detail := responsesFailedFrame(payload); failed {
							// 只要出现 response.failed 就置位；error 负载为空不影响失败判定。
							sawError = true
							// 摘要首个非空优先：后续空负载不得清掉先前的诊断信息。
							if detail != "" && errorFrame == "" {
								errorFrame = detail
							}
						}
					} else if upstreamChat {
						if failed, detail := openAIChatErrorFrame(payload); failed {
							// 顶层 error 键存在且非 null 即为失败帧，{"error":{}} 摘要为空也须置位。
							sawError = true
							if detail != "" && errorFrame == "" {
								errorFrame = detail
							}
						}
					}
					evs, _, derr := protocolconv.DecodeStreamEvents(upstreamProto, payload)
					if derr != nil {
						// 上游畸形帧不得静默丢弃：记入本次损耗诊断（随调用日志决策轨迹落痕）并告警，
						// 但不中断流（单帧坏不终止整条流）。
						lossy.Note("upstream malformed stream frame dropped: protocol=" + upstreamProto + " err=" + derr.Error())
						g.logger.Warn("upstream malformed stream frame dropped", "upstream_protocol", upstreamProto, "err", derr)
					} else {
						carriedUsage := false
						for _, ev := range evs {
							// 零值守卫（口径同三条直通路径）：上游给 "usage":{} 或全零 usage 时
							// 不能当作有效 usage，否则按 0 token 出账且调用日志记 completed
							//（金额与异常同时隐身）；按「未取到 usage」走可见内容估算。
							if ev.HasUsage && !usageZero(ev.Usage) {
								carriedUsage = true
								sawUsage = true
								mergeConvertedUsage(&usage, upstreamProto, ev)
							}
							switch {
							case ev.Type == protocolconv.EventMessageStop:
								sawDone = true
							case upstreamResponses && ev.Type == protocolconv.EventMessageDelta:
								// openai-responses 的终止帧（response.completed/incomplete）解码为
								// message_delta，该协议没有 EventMessageStop：终止帧本身既是标记、
								// 也是内容层收尾证据（上游确实生成完毕才下发）。
								sawDone = true
								sawContentEnd = true
							case ev.Type == protocolconv.EventMessageDelta && ev.StopReason != "":
								// openai-chat / anthropic 的内容层收尾证据（finish_reason / stop_reason）：
								// 只有 [DONE]/message_stop 而没收尾证据时，是代理补发标记掩盖截断
								//（见 streamEnd），不能归因 done_no_usage。
								sawContentEnd = true
							case ev.Type == protocolconv.EventError:
								sawError = true
								if errorFrame == "" {
									errorFrame = ev.ErrMessage
								}
							}
							accumulateConvertedAssistant(&content, &reasoning, toolCalls, &toolOrder, ev)
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
		// 「正常结束」须交叉验证两类独立信号（见 streamEnd）：终止标记（[DONE] / message_stop /
		// responses 终止帧）＋ 内容层收尾证据（finish_reason / stop_reason / responses 终止帧）。
		// 仅有标记时说明标记可能由代理在上游截断后补发，不按正常结束归因（计入渠道健康失败）；
		// error 帧仍最优先，收口标记不得把上游报错洗成正常结束。
		end := streamEnd{Terminal: sawDone, ContentComplete: sawContentEnd}
		return usage, rawUsageJSON(upstreamProto, rawParts), asm(),
			&UsageMissingError{Reason: usageMissingReasonOf(end, sawError), Detail: errorFrame}
	}
	return usage, rawUsageJSON(upstreamProto, rawParts), asm(), nil
}

// responsesFailedFrame 按原文探测 openai-responses 的 response.failed 帧：
// 返回「是否出现该失败帧」与错误摘要。该事件在解码器中没有对应规范事件（视为空事件），
// 故必须按原文探测；只要帧类型为 response.failed 即为 true，error 负载为空（{} / 缺失）也置位，
// 摘要则可能为空串。调用方据此置 sawError，不能依赖摘要是否为空。
// 帧被上游截断成畸形 JSON 时退化为首键形状判定（见 leadingJSONField），避免漏检。
func responsesFailedFrame(payload []byte) (bool, string) {
	// 先按字节探测：绝大多数帧不是 response.failed，避免为每帧多做一次反序列化。
	if !bytes.Contains(payload, []byte("response.failed")) {
		return false, ""
	}
	var probe struct {
		Type     string          `json:"type"`
		Error    json.RawMessage `json:"error"`
		Response struct {
			Error json.RawMessage `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		// 上游可能把失败帧截断成畸形 JSON，此时解码必然失败、摘要也无从解析；
		// 若直接放弃，「error 帧 + [DONE]」会被收口成 done_no_usage。故退化为首键形状判定。
		return leadingResponsesFailed(payload), ""
	}
	if probe.Type != "response.failed" {
		return false, ""
	}
	if d := errorFrameDetail(probe.Response.Error); d != "" {
		return true, d
	}
	return true, errorFrameDetail(probe.Error)
}

// openAIChatErrorFrame 按原文探测 openai-chat 上游的顶层 error 帧：返回「是否出现 error 帧」与错误摘要。
// openai-chat codec 的 DecodeEvent 没有 error 分支，该帧会解码为空事件，若不在此按原文探测，
// 随后的 [DONE] 就会把失败归因成 done_no_usage（不惩罚渠道、调用日志记 completed）。
// 判定只看顶层 error 键是否存在且非 null：{"error":{}} 也算失败帧，此时摘要为空串，
// 调用方不得用摘要是否为空反推失败。帧被截断成畸形 JSON 时同样按首键形状识别，避免漏检。
func openAIChatErrorFrame(payload []byte) (bool, string) {
	// 先按字节预筛：绝大多数正常帧不含 error 键，避免为每帧多做一次反序列化。
	if !bytes.Contains(payload, []byte("\"error\"")) {
		return false, ""
	}
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		// 同 responsesFailedFrame：截断的 error 帧解码必失败，按首键形状兜底识别，
		// 否则失败会被随后的 [DONE] 洗成 done_no_usage（不惩罚渠道）。
		key, _ := leadingJSONField(payload)
		return bytes.Equal(key, []byte("error")), ""
	}
	// isNullRaw 同时覆盖「键缺失」（RawMessage 为 nil）与显式 null。
	if isNullRaw(probe.Error) {
		return false, ""
	}
	return true, errorFrameDetail(probe.Error)
}

// leadingJSONField 从（可能被上游截断的）帧首部取出「首个顶层键」及紧随其后的原始值片段。
// 畸形帧无法整体反序列化，但首部形状仍足以判明帧类型；只认首个顶层键，
// 避免正文（如 content 文本）里出现的同名字面量造成误判。
func leadingJSONField(payload []byte) (key, value []byte) {
	s := bytes.TrimSpace(payload)
	if len(s) < 2 || s[0] != '{' {
		return nil, nil
	}
	s = bytes.TrimSpace(s[1:])
	if len(s) == 0 || s[0] != '"' {
		return nil, nil
	}
	end := bytes.IndexByte(s[1:], '"')
	if end < 0 {
		return nil, nil
	}
	key = s[1 : 1+end]
	s = bytes.TrimSpace(s[1+end+1:])
	if len(s) == 0 || s[0] != ':' {
		return nil, nil
	}
	return key, bytes.TrimSpace(s[1:])
}

// leadingResponsesFailed 判断畸形帧的首个顶层键 type 取值是否为 response.failed。
func leadingResponsesFailed(payload []byte) bool {
	key, val := leadingJSONField(payload)
	return bytes.Equal(key, []byte("type")) && bytes.HasPrefix(val, []byte(`"response.failed"`))
}

// itoa 是 strconv.Itoa 的包内别名（blockIndex 键拼接用）。
func itoa(n int) string { return strconv.Itoa(n) }

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
		// DeltaSignature 不累积进正文/推理（签名是元数据，不是内容）。
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
