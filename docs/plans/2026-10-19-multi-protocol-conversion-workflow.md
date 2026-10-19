---
intent: 让网关在入站协议与渠道实际支持的上游协议不一致时，对请求体、响应体与流式 SSE 事件做协议转换后转发；协议一致时保持零转换直通。
success_criteria: openai-chat / openai-responses / anthropic-messages 两两 6 个方向的请求体、响应体、流式事件均可转换；直通场景与一期逐字节一致；无法在目标协议表达的字段被丢弃时决策轨迹与调用日志带 lossy 明细。
risk_level: high
auto_approve: false
---

## Steps

- [x] **Step 1: 建立 protocolconv 包骨架（IR + LossyReport）**
action: 新建 `backend/internal/domain/protocolconv/ir.go`，定义 `CanonicalRequest`（Model、System、Messages、Tools、ToolChoice、Temperature、TopP、MaxTokens、Stop、Stream、Metadata、Extra）、`Message`（Role、Blocks）、`ContentBlock`（Kind 枚举 text/image/tool_use/tool_result/thinking + 对应字段）、`CanonicalResponse`（ID、Model、Role、Blocks、FinishReason、Usage、Extra）、`CanonicalEvent`（Type 枚举 message_start/content_block_start/content_block_delta/content_block_stop/message_delta/message_stop/error/ping + Index + TextDelta + ToolArgsDelta + StopReason + Usage + Err）；新建 `backend/internal/domain/protocolconv/lossy.go` 定义 `LossyReport{Dropped []string, Notes []string}` 及 `Add`/`Merge`/`IsEmpty` 方法。本步仅类型与字段，不含协议逻辑。Usage 复用 `billing.Usage`。
loop: false
max_iterations: 3
verify: cd backend && go build ./internal/domain/protocolconv/...
gate: auto

- [x] **Step 2: 实现 openai-chat 请求/响应编解码器（TDD）**
action: 新建 `backend/internal/domain/protocolconv/codec_openai_chat.go` 实现 `ParseRequest`/`SerializeRequest`（messages/system/tools/tool_choice/temperature/max_tokens/stop/stream；content 兼容 string 与 [{type,text,image_url}]）与 `ParseResponse`/`SerializeResponse`（choices[].message.content、tool_calls、finish_reason、usage）。新建 `backend/internal/domain/protocolconv/codec_openai_chat_test.go`，断言 parse→serialize→parse 相等，覆盖纯文本、多模态数组、tools/tool_calls 三类用例。先写测试（失败）再实现至通过。
loop: until cd backend && go test ./internal/domain/protocolconv/ -run TestOpenAIChatCodec passes
max_iterations: 3
verify: cd backend && go test ./internal/domain/protocolconv/ -run TestOpenAIChatCodec -v
gate: auto

- [x] **Step 3: 实现 openai-responses 请求/响应编解码器（TDD）**
action: 新建 `backend/internal/domain/protocolconv/codec_openai_responses.go` 实现请求（input 为 string 或 message 数组、instructions 作 system、tools 为 function 类型、temperature、max_output_tokens、stream）与响应（output[] 的 message.content[].output_text 与 function_call、status、usage.input_tokens/output_tokens）的 Parse/Serialize。新建 `backend/internal/domain/protocolconv/codec_openai_responses_test.go` 做往返断言，覆盖文本、工具调用、system/instructions 映射。先写测试再实现至通过。
loop: until cd backend && go test ./internal/domain/protocolconv/ -run TestOpenAIResponsesCodec passes
max_iterations: 3
verify: cd backend && go test ./internal/domain/protocolconv/ -run TestOpenAIResponsesCodec -v
gate: auto

- [x] **Step 4: 实现 anthropic 请求/响应编解码器（TDD）**
action: 新建 `backend/internal/domain/protocolconv/codec_anthropic.go` 实现请求（system、messages 的 role/content 块 text/image/tool_use/tool_result、tools.input_schema、tool_choice、max_tokens 必填、temperature、stop_sequences、stream）与响应（content[] 的 text/tool_use/thinking、stop_reason、usage 含 cache_creation_input_tokens/cache_read_input_tokens）的 Parse/Serialize。新建 `backend/internal/domain/protocolconv/codec_anthropic_test.go` 做往返断言，覆盖文本、工具调用、cache_control 与 thinking 字段的 lossy 标记。先写测试再实现至通过。
loop: until cd backend && go test ./internal/domain/protocolconv/ -run TestAnthropicCodec passes
max_iterations: 3
verify: cd backend && go test ./internal/domain/protocolconv/ -run TestAnthropicCodec -v
gate: auto

- [x] **Step 5: 实现三协议流式事件编解码（SSE ↔ canonical）**
action: 新建 `backend/internal/domain/protocolconv/stream.go`，为三协议各实现 `DecodeSSE(data []byte) ([]CanonicalEvent, error)` 与 `EncodeEvent(CanonicalEvent) ([][]byte, error)`：anthropic 的 message_start/content_block_start/content_block_delta/text_delta/input_json_delta/message_delta/message_stop；openai-chat 的 choices[].delta（content/tool_calls）与尾帧 usage；openai-responses 的 response.output_text.delta 与 response.completed（usage）。新建 `backend/internal/domain/protocolconv/stream_test.go`，断言三协议各自 SSE 均能解码为同一 canonical 序列并编码回各自形状（含 usage 与 stop_reason）。
loop: until cd backend && go test ./internal/domain/protocolconv/ -run TestStream passes
max_iterations: 3
verify: cd backend && go test ./internal/domain/protocolconv/ -run TestStream -v
gate: auto

- [x] **Step 6: 实现转换编排 convert.go（六方向矩阵）**
action: 新建 `backend/internal/domain/protocolconv/convert.go`，实现 `SelectUpstreamProtocol(inbound string, supported channelprotocol.Protocols, override []string) (string, bool)`（固定优先序常量 openai-chat → anthropic-messages → openai-responses，override 有序列表优先）、`ConvertRequest(from, to string, body []byte) ([]byte, *LossyReport, error)`、`ConvertResponse(from, to string, body []byte) ([]byte, *LossyReport, error)`、`ConvertStreamEvent(from, to string, data []byte) ([][]byte, *LossyReport, error)`。新建 `backend/internal/domain/protocolconv/convert_test.go`，参数化覆盖 6 个有序方向 × 请求/响应/事件三类往返。
loop: until cd backend && go test ./internal/domain/protocolconv/ -run TestMatrix passes
max_iterations: 3
verify: cd backend && go test ./internal/domain/protocolconv/ -run TestMatrix -v
gate: auto

- [x] **Step 7: lossy 报告接入决策轨迹与调用日志**
action: 在 `backend/internal/domain/gateway/protocol.go` 与 `backend/internal/domain/gateway/handler.go` 的候选 attempt 装配处新增 `lossy` 与 `inbound_protocol`/`upstream_protocol` 字段，经既有 `g.callLogDecision(attempts, ...)` 进入 `call_log` decision；网关侧汇总 `protocolconv.LossyReport` 写入轨迹。新增 `backend/internal/domain/gateway/conversion_test.go` 断言：发生有损时 decision.attempts 含 `lossy` 非空且协议字段正确；无有损时不含该键（不改既有键名）。
loop: until cd backend && go test ./internal/domain/gateway/ -run TestLossyDecision passes
max_iterations: 3
verify: cd backend && go test ./internal/domain/gateway/ -run TestLossyDecision -v
gate: auto

- [x] **Step 8: gateway 有效上游协议选择（直通优先 + 优先序 + 覆盖）**
action: 新建 `backend/internal/domain/gateway/conversion.go`，实现 `resolveUpstreamProtocol(rt *channel.RuntimeChannel, mr *channel.ModelRuntime, baseProtocol string) (upstream string, converted bool, ok bool)`：baseProtocol ∈ `effectiveProtocols(rt, mr)` → 直通（upstream=baseProtocol，converted=false）；否则若转换启用（渠道协议/模型 `ConvertEnabled` 默认 true，受 sys_config 总开关约束）→ 用 `protocolconv.SelectUpstreamProtocol` 在 native 集合内选目标；两者皆不可 → ok=false（沿用 AC-4）。保留 `effectiveProtocols` 语义不变。新建 `backend/internal/domain/gateway/conversion_test.go`（同文件追加）覆盖直通/转换/关闭三态。
loop: until cd backend && go test ./internal/domain/gateway/ -run TestResolveUpstreamProtocol passes
max_iterations: 3
verify: cd backend && go test ./internal/domain/gateway/ -run TestResolveUpstreamProtocol -v
gate: auto

- [x] **Step 9: gateway 非流式转换集成**
action: 在 `backend/internal/domain/gateway/handler.go` 候选循环中，用 `resolveUpstreamProtocol` 的结果替换既有「`eff.Has(baseProtocol)` 否则 skip」判定：直通保持现状（`ureq.Protocol=baseProtocol`，原样 body）；转换时以 `ConvertRequest` 生成上游体、`ureq.Protocol=upstream`、`providerFactory(upstream, endpoint)`，并在 `serveNonStream`（`backend/internal/domain/gateway/handler.go`）取到 2xx 体后经 `ConvertResponse` 回转为入站协议体再计费/下发（usage 仍用上游 Provider 解析）。AC-4 分支仅在 `ok=false` 时置 `sawUnsupported`。测试用假上游断言上游收到目标协议体、下游收到入站协议形状且 usage 口径不变。
loop: until cd backend && go test ./internal/domain/gateway/ -run TestNonStreamConversion passes
max_iterations: 3
verify: cd backend && go test ./internal/domain/gateway/ -run TestNonStreamConversion -v
gate: auto

- [x] **Step 10: gateway 流式转换 pump 集成**
action: 在 `backend/internal/domain/gateway/handler.go`（或新建 `backend/internal/domain/gateway/conversion_stream.go`）新增 `pumpConvertedStream(ctx, upstream body, upstreamProto, inboundProto, w) (billing.Usage, []byte, []byte, error)`：读上游 SSE 行 → 上游 codec `DecodeSSE` 为 canonical 事件 → 入站 codec `EncodeEvent` 写回并 Flush；累计三段 usage、拼装 assistant 消息（供 call_log）。`serveStream` 在 converted 时走该 pump，直通仍走既有 `prov.HandleStream`（逐字节透传）。测试用假上游断言下游收到入站协议事件序列、顺序保真、usage 正确。
loop: until cd backend && go test ./internal/domain/gateway/ -run TestStreamConversion passes
max_iterations: 3
verify: cd backend && go test ./internal/domain/gateway/ -run TestStreamConversion -v
gate: human

- [x] **Step 11: 配置面（转换开关 + 目标优先序 + 总开关）**
action: 在 `backend/internal/domain/channelprotocol/model.go` 的 `ModelGateway` 追加 `ConvertEnabled *bool json:"convertEnabled,omitempty"` 与 `ConvertTargets []string json:"convertTargets,omitempty"`；在 `backend/internal/domain/channel/model_store.go` 的 `ChannelModel` 同步这两字段；在 `backend/internal/domain/channel/manager.go` 构建 `ModelRuntime` 时透传；新增 sys_config 键 `gateway.protocol_conversion.enabled`（默认 true）并在 `backend/cmd/server/main.go` 装配为网关可读函数。测试断言字段 JSONB 往返与运行时透传、总开关关闭时不转换。
loop: until cd backend && go test ./internal/domain/channel/... ./internal/domain/channelprotocol/... passes
max_iterations: 3
verify:
  - type: shell
    command: cd backend && go test ./internal/domain/channel/... ./internal/domain/channelprotocol/...
  - type: shell
    command: cd backend && go build ./cmd/server/
gate: auto

- [x] **Step 12: 前端转换控件**
action: 在 `frontend/src/views/admin/ChannelProtocolEdit.vue` 的模型清单中，为每个模型项增加「允许转换」开关（默认继承=开）与「目标协议优先序」有序列表（复用 `frontend/src/components/StringListEditor.vue`，候选项为三个基础协议名）；在 `frontend/src/api/admin.ts` 的 `ChannelProtocolModelOverride` 增 `convertEnabled?: boolean` 与 `convertTargets?: string[]`。保存路径沿用既有模型清单提交逻辑。
loop: false
max_iterations: 3
verify:
  - type: shell
    command: cd frontend && npm run build
  - type: artifact
    path: frontend/src/api/admin.ts
    assert:
      kind: exists
gate: auto

- [x] **Step 13: 矩阵集成测试（假上游 × 6 方向 × 流式/非流式）**
action: 新建 `backend/tests/protocol_conversion_test.go`：起三个协议的假上游（openai-chat / openai-responses / anthropic-messages），对 6 个有序方向分别以入站协议 A 发请求、配置渠道仅支持上游协议 B，断言非流式与流式两条路径：上游收到的请求体为 B 形状、下游收到 A 形状、usage 按 B 解析、直通方向字节级不变（AC-P2-1~AC-P2-4、AC-P2-8）。
loop: until cd backend && go test ./tests/ -run TestProtocolConversion passes
max_iterations: 3
verify: cd backend && go test ./tests/ -run TestProtocolConversion -v
gate: auto

- [x] **Step 14: 人工评审（路由语义 + 流式 pump）**
action: 人工复核两项高风险变更：① 路由语义——直通优先、默认开启转换、一期 AC-18 被取代、`ConvertEnabled=false` 可回退到 AC-4（对照设计文档决策 D2 与治理契约）；② 流式转换 pump——事件顺序保真、无丢帧、usage 累计与直通路径互不影响。记录评审结论后再继续。
loop: false
max_iterations: 1
verify:
  type: human-review
  check: 路由语义（直通优先/默认开启/AC-18 取代/关闭回退）与流式转换 pump 正确性经人工复核通过
gate: human

- [x] **Step 15: 全量测试与构建**
action: 运行后端全量测试、后端构建与前端构建，确保无回归（含既有 go test ./... 全绿与直通逐字节不回退）。
loop: false
max_iterations: 3
verify:
  - type: shell
    command: cd backend && go test ./...
  - type: shell
    command: cd backend && go build ./...
  - type: shell
    command: cd frontend && npm run build
gate: auto