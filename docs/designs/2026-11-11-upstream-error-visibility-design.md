# 上游错误可见性：对外错误契约与失败留痕（2026-11-11）

> 起因：运营新开一条上游渠道（OpenCode Go）后调用不通，调用方只拿到一句通用中文错误，
> 上游真实报文是「该模型在欧洲/美国以外托管，请到控制台把区域切成 Global」——运营看到这句就能自己解决，
> 但当时既拿不到原文，账单里也查不到这次失败。

## 1. 对外错误体契约（`/v1/*`）

所有 `/v1` 错误体沿用 OpenAI 标准形状（与内部统一壳 `resp.Body` 解耦）：

```json
{
  "error": {
    "message": "…",
    "type": "…",
    "code": "…",
    "upstream_status": 400
  },
  "request_id": "…"
}
```

字段口径：

| 字段 | 来源 | 说明 |
| --- | --- | --- |
| `error.message` | **上游原文** | 上游返回非 2xx 时取上游报文里的 `error.message`（非 OpenAI 形状时取整段摘要）。做两件事：凭据样式清洗（上游可能在文案里回显它收到的 key）、按 UTF-8 边界截断到 512 字节。上游没给可读文案（空体/纯二进制）时回落到网关中文文案，**不返回空 message**。 |
| `error.type` | **上游原文** | 上游报文里的 `error.type`；缺省按下游状态码归类（429 → `rate_limit_error`，5xx → `server_error`，其余 → `invalid_request_error`）。 |
| `error.code` | **网关归类** | 网关按下游状态码归类（`upstream_error` / `rate_limit_exceeded` / `service_unavailable` / `invalid_api_key` …），不透出上游业务码。 |
| `error.upstream_status` | **上游真实状态码** | 仅「上游返回非 2xx」的失败体带上；网关自身拒绝（鉴权/余额/路由/协议不支持）不带。用于区分「网关拒绝」与「上游拒绝、网关转述」，并可按上游状态码查厂商文档。 |
| `request_id` | 网关 | 与响应头 `x-request-id` 同值（客户端带了合法的 `x-request-id` 时沿用）。这是运营在账单页定位一次请求的唯一钥匙。 |

HTTP 状态码语义**不变**：上游 4xx/5xx 在未命中协议级错误策略时仍统一写 502（`code=upstream_error`），
上游真实状态码体现在 `error.upstream_status` 与调用日志里；命中错误策略时按策略的 `downstreamStatus`。
状态码本身不在本次改动范围内（改动它会影响客户端重试语义，属另一件事）。

### 透传的边界

- 只透传「上游确实返回了非 2xx」的场景。连接失败、响应超时、网关自身拒绝没有上游报文，保持既有中文文案。
- 候选可转移（failover）时，若最终全部候选都失败，终结响应（503 / 429）里带**最后一次**上游报文原文与状态码，
  不再只剩一句「无可用渠道」——否则 5xx 这类可转移失败的上游原因会彻底丢失。
- 流式与非流式共用同一套错误分支代码：`stream=true` 时上游在 SSE 开始前返回非 2xx，走的就是这套透传；
  若上游已经返回 200 才开始 SSE，则 SSE 帧（含流内 `error` 帧）按原样透传给客户端，网关不改写。

**流内 `error` 帧的留痕口径**（响应已 200 下发，无法再改状态码，只能保证记录与原因准确）：

| 情形 | 错误类型 | 计费 | 调用日志 | 渠道健康 |
| --- | --- | --- | --- | --- |
| error 帧 + **无** usage | `UsageMissingError{Reason: error_frame}` | 按可见内容估算（既有口径） | `failed`，`error_message` = 「上游流式响应中断（流内 error 帧，按估算计费）：<上游 error 帧摘要>」 | 计入失败（既有口径） |
| error 帧 + **有** usage | `UpstreamErrorFrameError`（新增，**不**属于 usage 缺失） | 按上游**实际** usage 结算（不估算、不落额外 failed 账单） | `failed`，`error_message` = 「上游流式响应中断（流内 error 帧，按上游 usage 计费）：<上游 error 帧摘要>」 | 保持既有成功回喂（本单不改健康/选路口径） |

两种情形都落结构化标记：`decision.usage_reason=error_frame` / `decision.upstream_error_frame=true`。

> 「error 帧 + 有 usage」此前整条记 `completed`：上游明明用 error 帧宣告失败，
> 却因为顺手给了 usage 而在调用日志/账单上完全消失（main 上既有缺陷，Gate 独立验收复现）。
> 修法是给这类流一个**独立的错误类型**，与 usage 缺失彻底分开——若复用 `ErrUsageMissing`，
> 会连带把计费从「实际 usage」改成「估算」，等于动了计费口径。


## 2. 失败留痕（账单 + 调用日志）

一次失败要能被运营按 `request_id` 查到，**账单与调用日志必须成对写入**：
账单页按 `request_id` 检索是经 `call_logs.request_id` 关联 `billing_records` 的
（见 `internal/domain/billing/query.go` 的 `EXISTS` 子查询），只落一边就搜不到。

上游非 2xx 的失败记录包含：

| 字段 | 位置 | 说明 |
| --- | --- | --- |
| `request_id` | `call_logs.request_id` | 与响应体 `request_id` 同源 |
| 上游状态码 | `call_logs.upstream_status`（迁移 0019） | 结构化小整型；`NULL` = 无上游状态码（连接失败/超时/网关拒绝） |
| 上游错误体 | `call_logs.resp_body` + `error_message` | 上游响应体截断到 4096 字节后落库（`error_message` 形如「上游返回 400: {上游报文}」） |
| 渠道/密钥/模型 | `call_logs.channel_id / channel_key_id / internal_model_id / model` | 定位是哪条渠道哪次尝试 |
| 时间 | `call_logs.created_at` | 落库时间 |
| 金额 | `billing_records`（`status='failed'`、`credits_consumed=0`） | 失败不扣费，但必须有账 |

调用日志的只读视图（admin `CallLogView` 与 dev `DevCallLogView`）都已输出 `UpstreamStatus`，
账单详情页据此直接显示「上游 400」而不必从中文文案里推断。

### 本次一并补齐的留痕缺口

以下分支此前会写出错误响应，但**没有**落调用日志（部分连账单也没有），导致用户拿着 `request_id` 在账单页搜不到：

- 请求体过大/不可读、请求体不是合法 JSON 对象、请求体缺少 `model`（400）
- 令牌标签解析失败、Auto 模型解析失败、路由失败（500）
- 预扣金额估算失败（500）
- 预扣被拒（402 余额不足 / 500 预扣异常）、账户余额为 0（402）
- Auto 模型全部成员无可用候选（503）

统一收敛到 `Gateway.rejectAndRespond`：failed 账单 + 调用日志 + 结构化异常日志 + 对外错误，一次写完。

未纳入：**鉴权失败（401）**——此时没有可信的用户身份，且该分支是外部可打的公开面，
逐条落库会变成可被利用的写放大面；这类请求仍只有结构化异常日志。

## 3. 验证

`backend/internal/domain/gateway/upstream_error_passthrough_test.go`：

| 用例 | 覆盖 |
| --- | --- |
| `TestServeUpstreamError_PassThroughMessageAndType` | 非流式 + 流式：上游 400 原文出现在调用方错误体里；`type`/`code`/`upstream_status`/`request_id` 齐全；同一次失败在账单与调用日志里都可按 `request_id` 查到 |
| `TestServeUpstreamError_AggregateKeepsLastUpstreamBody` | 非流式 + 流式：上游 500（可转移）全候选耗尽 → 终结 503 仍带上游原文与 `upstream_status=500` |
| `TestServeUpstreamError_TruncatesAndKeepsValidUTF8` | 超长中文上游文案按 UTF-8 边界截断，不出半个字符 |
| `TestServeUpstreamError_FallsBackWhenUpstreamBodyUnusable` | 上游空体 → 回落网关文案，仍记 `upstream_status` |
| `TestServeUpstreamError_ConnectionFailureKeepsGenericMessage` | 断网：保持网关文案、不编造上游状态码，账单 + 调用日志留痕 |
| `TestServeUpstreamError_TimeoutKeepsGenericMessage` | 超时（已送达）：502 + 网关文案 + 调用日志留痕 |
| `TestServeUpstreamError_SuccessStaysSuccess` | 回归守卫：上游 2xx 不会被改成错误 |
| `TestServeRejectPaths_RecordBillingAndCallLog` | 网关拒绝分支（格式错误/缺 model/余额不足/模型未配置）账单与调用日志成对、`billing_id` 1:1 |
| `TestWriteUpstreamError_UpstreamStatusIsRealCode` | `upstream_status` 恒为上游真实码：上游 304（对外 502）、策略 422→418 两条路径都报 304 / 422 |
| `TestOperatorView_SanitizesInvalidUTF8` | 含非法 UTF-8 / `0x00` 的报文产出合法 UTF-8 并同字段标注；合法原文零改动 |
| `TestCallLog_PgRoundTrip_InvalidUTF8Payload` | 真库：非法 UTF-8 + `0x00` 的错误体仍能落库并读回（清洗后不丢内容） |
| `TestToValidText`（`internal/pkg/errbody`） | 整理规则：非法字节 → `U+FFFD`、`0x00` → `<0x00>`、合法内容逐字节不变 |
| `TestCallLog_PgRoundTrip` / `TestCallLog_PgRoundTrip_InvalidUTF8Payload` | 真库用例改为**自建临时库 + 跑全部迁移**，CI 里不再「库里没表就跳过」假绿 |
`backend/internal/domain/gateway/stream_error_frame_test.go`（流内 `error` 帧的留痕口径）：
| 用例 | 覆盖 |
| --- | --- |
| `TestServeStream_ErrorFrameWithUsageRecordedFailed` | error 帧 + usage：调用日志记 `failed`（此前误记 `completed`）且 `error_message` 带上游原文；计费仍按上游实际 usage（10/5，不估算、不额外落 failed 账单）；请求级结论计为失败 |
| `TestServeStream_ErrorFrameWithoutUsageKeepsUpstreamDetail` | error 帧 + 无 usage：记 `failed` 且 `error_message` 带出上游 error 帧原文（此前只有归因、没有原因） |
| `TestStreamConversionErrorFrameWithUsageRecordedFailed` | 转换泵（anthropic 入站 → openai-chat 上游）同一口径 |
| `TestUpstreamErrorFrameError_Semantics` | 新错误类型可 `errors.As` 识别，且**不**被当成 usage 缺失（否则计费会从实际 usage 退化成估算） |
