---
design_type: bugfix
created_at: 2026-10-25
---

# usage 缺失归因：终止标记与内容层收尾证据交叉验证

> Status: IMPLEMENTED（随 PR #59 review follow-up 落地）
> Author: agent
> 背景：PR #59 把「收到 `[DONE]` / `message_stop` / 终止事件」等同于「流正常结束」，据此归因
> `done_no_usage`（不惩罚渠道、调用日志 `completed`、请求级不置 `interrupted`）。代码评审提出：
> 代理类渠道常在上游字节流结束时**无条件补发**一个 `[DONE]`——上游被中途截断时同样会补，于是
> 截断被判定为「正常结束」，渠道健康 / 调用日志 / 计费三个观测面同时看不见这个坏渠道，永不处罚。

## Intent Contract

```
intent: 让「上游被截断但代理补发结束标记」在网关内被识别为异常，恢复渠道健康 / 调用日志 / 计费三个观测面的可见性；同时不误伤「内容完整但忽略 include_usage」的正常上游。
constraints:
  - 不改变计费公式、预扣/结算模型：usage 缺失一律按可见内容估算计费（估算口径不变）。
  - 不改变客户端断开（client-gone）与下游写失败（errDownstreamWrite）两条既有分支。
  - 四条流式路径（openai-chat / anthropic-messages / openai-responses 直通 + 跨协议转换泵）归因口径一致。
  - 不新增每帧反序列化开销：内容层证据复用各路径已有的逐帧解析结果。
  - go vet / go test -race 全绿。
success_criteria:
  - 「截断 + 补发 [DONE]/message_stop」归因 terminator_only，回喂渠道健康失败、调用日志 failed。
  - 「内容层收尾证据齐备但无 usage」仍归因 done_no_usage，不回喂失败（不误伤）。
  - 「流内 error 帧 + [DONE]」仍归因 error_frame（优先级最高，不被收口标记洗白）。
  - 「连接层异常（非 io.EOF 的读错误 / 超时 / 连接被重置）」一律返回原始错误，不计入 usage 缺失。
risk_level: medium   # 触及渠道健康判定口径（可能把协议不达标的兼容上游判为失败）
```

## Decision

判定「流正常结束」必须交叉验证两类**独立**信号，缺一不可：

| 信号 | 含义 | 各协议取值 |
|---|---|---|
| `Terminal`（终止标记） | 「有人宣布流结束了」 | `[DONE]` / `message_stop` / responses 终止事件 |
| `ContentComplete`（内容层收尾证据） | 「上游确实生成完毕了」 | `choices[].finish_reason` / `message_delta.stop_reason` / `response.completed`·`incomplete` 事件本身 |

`ContentComplete` 是**必要条件**。理由：终止标记对中间层零成本可伪造（`if !sent { write("[DONE]") }`），
而 `finish_reason` / `stop_reason` / 终止事件是上游真实产物，代理要伪造成本高得多。

归因优先级（`streamEnd.reason`）：

```
error_frame  >  done_no_usage  >  terminator_only  >  truncated
```

- `error_frame`：流内 error 帧（既有）。
- `done_no_usage`：`ContentComplete` 成立；**不惩罚渠道**（上游忽略 include_usage，内容完整）。
- `terminator_only`（新增）：只有 `Terminal`、无 `ContentComplete`——补发标记掩盖截断；**惩罚渠道**。
- `truncated`：两者皆无——真截断；**惩罚渠道**。

连接层信号（非 `io.EOF` 读错误 / 读超时 / 连接重置）不进入该结构：适配器读到即返回原始错误，
handler 按「流式传输中断」处置并回喂失败，本身就压过任何结束标记。若把它也折进 usage 缺失，
反而会丢掉传输层错误原文。

## 零值守卫（usage 存在但为空/全零）

「`usage` 键存在」不等于「取到 usage」：上游下发 `"usage":{}` 或全零 usage 时，解析结果为
零值且 `err == nil`，若据此置 `sawUsage`，就会按 0 token 结算——钱直接漏掉，而调用日志仍记
`completed`、渠道健康也不亮，金额与异常同时隐身。

口径统一为：**全零 usage 等同于「未取到 usage」**，走缺失归因 + 可见内容估算。四条流式路径与
非流式（`handler.go` 的 `usageZero(usage)`）、openai-chat 流式（`parseStreamData` 的
`!usageZero(u)`）完全一致：

| 路径 | 守卫位置 |
|---|---|
| `openai.go` 流式 | `parseStreamData`（既有，本次补对照用例） |
| `responses.go` 流式 | `response.completed/incomplete` 的 usage 解析（本次补齐，评审 P2） |
| `anthropic.go` 流式 | `message_start` / `message_delta` 的 usage 解析（同类漏配，本次一并补齐） |
| `conversion_stream.go` 泵 | 规范事件 `HasUsage` 合并前（同上） |
| 非流式 | `handler.go` `usageZero(usage)`（既有） |

## 结构化归因字段

`terminator_only` 若只能靠中文文案识别，看板/告警没法按归因聚合。故两处落结构化字段：

- 异常日志：`errInfo.usageReason` → 日志属性 `usage_reason`（取值见 `UsageMissing*` 常量）；
- 调用日志：`call_logs.decision->>'usage_reason'`（仅 usage 缺失时写入，其余请求键集不变）。

## 各路径实现要点

| 路径 | `Terminal` | `ContentComplete` |
|---|---|---|
| `openai.go`（直通） | `[DONE]` | `sawFinishReason`（来自 `streamAssembler.apply` 的回报，复用逐帧解析，不新增解码） |
| `anthropic.go`（直通） | `message_stop` | `message_delta.stop_reason` 非空 |
| `responses.go`（直通） | `response.completed/incomplete` 或 `[DONE]` | 仅 `response.completed/incomplete`（`[DONE]` 非本协议终止符） |
| `conversion_stream.go`（转换泵） | `[DONE]` / `EventMessageStop` / responses 终止帧 | `EventMessageDelta.StopReason` 非空，或 responses 终止帧 |

## 取舍

1. **responses 的 `[DONE]` 不再单独算正常结束**（评审建议 ①）：该协议官方无此终止符，纯 `[DONE]`
   收尾者必然违反契约；判为失败是诚实结论。代价：纯 `[DONE]` 收尾的兼容上游会持续被惩罚，
   若线上确有此类渠道，属渠道侧需要修正的协议偏差。
2. **保留 `done_no_usage` 不惩罚**（不从「惩罚一切 usage 缺失」反向走）：上游忽略 include_usage 属
   协议偏差但内容完整，惩罚会把可用渠道熔断，代价远大于收益。
3. **新增 `terminator_only` 而不是复用 `truncated`**：两者都惩罚，但语义不同（有没有收到标记），
   分开后调用日志与排障能直接区分「代理补发标记」与「干断了」。
4. **不把「仅 finish_reason、不补 `[DONE]`」判失败**：连接干净结束（`io.EOF`）＋ 上游真实收尾证据
   已足够证明内容完整；终止标记只是形式约定。
5. **转换泵删除冗余 `sawTerminal`**（评审 P3）：其职责被 `ContentComplete` 完整吸收，
   两份状态并存只会让「正常结束」有两个真相来源。
6. **健康判定状态不放在 call-log 装配器里**（评审 P3）：`streamAssembler` 只回报本帧是否带
   `finish_reason`，`sawFinishReason` 由 `HandleStream` 自己累积——否则日后为 call_log 调整装配逻辑
   会静默翻转 `terminator_only` 判定，后果是误 DRAIN 健康渠道。

## Verification

```
cd backend && go vet ./...
cd backend && go test -race $(go list ./... | grep -v '/tests')
```

覆盖用例（四路一致性）：
- 单元：`TestHandleStreamUsageMissingReason`（各协议 × 正常/仅标记/截断/error 帧）、
  `TestStreamEndReasonMatrix`（归因矩阵）、`TestHandleStreamConnectionErrorBeatsTerminator`（连接层优先）。
- 转换泵：`TestStreamConversionUsageMissingEndShape`（含「截断 + 补发 [DONE]」「仅 finish_reason 无 [DONE]」「usage 全零帧」）。
- 零值守卫：`TestServeZeroUsageEstimatesInsteadOfZeroBilling`（responses 空对象/全零、anthropic 空对象，
  断言账单 token > 0 且 `usage_reason=done_no_usage`）、`TestStreamConversionZeroUsageEstimatesInsteadOfZeroBilling`（泵）。
- 端到端：`TestServeStreamDoneOnlyWithoutFinishReasonPenalized`、`TestServeResponsesDoneOnlyNoUsagePenalized`、
  `TestServeResponsesCompletedNoUsageNoPenalty`、`TestServeStreamNonOpenAINoUsageEndShape`、
  `TestStreamConversionDoneOnlyPenalized`（健康回喂 + 调用日志 + 估算计费三面同时断言）。

## 已知限制

- responses 兼容上游若只发 `response.output_text.delta` + `[DONE]`，将长期被判 `terminator_only`。
- `terminator_only` 与 `truncated` 都计入健康失败，看板上按 kind 聚合仍为「流式中断」（`kind` 未细化）。
- 客户端 ctx 已结束（`Canceled`/`DeadlineExceeded`）时先按 client-gone 处置、不惩罚渠道（既有口径未改）。
