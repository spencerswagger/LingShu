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
| `error.message` | **上游原文** | 上游返回非 2xx 时取上游报文里的 `error.message`（非 OpenAI 形状时取整段原文）。只做两件事：**精确遮蔽本次实际使用的渠道凭据**（`errbody.MaskSecrets`，其余字节原样）、按 UTF-8 边界截断到 512 字节。**不做样式清洗**——样式清洗会把 request-id、模型名里的高熵串一并抹掉，调用方读到的就不是原文（清洗只用于日志短字段，见 §1.2.1）。**401/402/403 也照旧透原文**，不按状态码区别对待。上游没给可读文案（空体/纯二进制）时回落网关中文文案，**不返回空 message**。 |
| `error.type` | **上游原文**（待拍板，见文末「待拍板」） | 上游报文里的 `error.type`；缺省按下游状态码归类（429 → `rate_limit_error`，5xx → `server_error`，其余 → `invalid_request_error`）。契约复核建议改为网关归类（理由：上游 401 的 `type=authentication_error` 配对外 503 自相矛盾），与需求方口径冲突，**先按上游原文实现**，分歧见文末「待拍板」。 |
| `error.code` | **网关归类** | 网关按下游状态码归类（`upstream_error` / `rate_limit_exceeded` / `service_unavailable` / `invalid_api_key` …），不透出上游业务码。 |
| `error.upstream_status` | **上游真实状态码** | 整型，**只在「上游确实返回了非 2xx」时出现**；**无上游码时不写该字段**（不是 `null`、也不是 0）——连接失败、已送达超时、网关自身拒绝都没有它。它是「上游转述 vs 网关判定」的唯一判别符（`contract_fields_test.go` 按「字段不存在」断言）。 |
| `request_id` | 网关 | 与响应头 `x-request-id` 同值（客户端带了合法的 `x-request-id` 时沿用）。这是运营在账单页定位一次请求的唯一钥匙。 |

### 1.2.1 凭据遮蔽：三处口径不同，别混用

| 位置 | 口径 | 实现 |
| --- | --- | --- |
| 对外 `error.message` / `error.type` | 原文 + **只遮蔽本次实际使用的渠道凭据** | `errbody.MaskSecrets(text, ureq.Cred)` |
| 运营侧 `call_logs.resp_body` / `error_message` | 同上（原文 + 精确遮蔽） | 同上（读取层已不再做样式清洗） |
| 结构化日志短字段（`err_summary` / `err_type` / `err_code` / `reason`） | 按**样式**清洗（`sk-*` / Bearer / KV / 高熵长串 → `***`）+ 截断 | `sanitizeErrSummary`（`logErr` 内统一执行） |

读取层（`readUpstreamErrBody`）只 `TrimSpace`、不做任何清洗：对外要原文、运营侧也要原文，
清洗放在各自的出口按上表执行。样式清洗的代价是「原文被打成筛子」——它只配用于日志短字段。

HTTP 状态码语义**不变**（本单只做可见性，不动既有映射）。**下表是 `origin/main` 的实测基线**
（2026-11-11 用临时分支跑 12 个上游码 × 单/双候选 × 流式/非流式 = 48 组组合逐项测得，
`contract_status_semantics_test.go` 按这 48 组钉住，改一条就红）：

| 场景 | 对外状态码 | `error.code` |
| --- | --- | --- |
| 上游 4xx 且不可转移（400/409/413/422） | 502 | `upstream_error` |
| 上游 504 / 524（网关类超时，不可转移） | 502 | `upstream_error` |
| 上游可转移失败（401/402/403/404/5xx）但候选耗尽 | 503 | `service_unavailable` |
| 全部候选被上游 429 限流 | 429 | `rate_limit_exceeded` |
| 已送达后超时（无上游状态码） | 502 | `upstream_error` |
| 连接失败 / 构建失败 / 候选全被本地拦截（无上游状态码） | 503 | `service_unavailable` |
| 下游客户端断开 | 499 | — |

命中错误策略时按策略的 `downstreamStatus`。

> 注意：main **不是**「4xx/5xx 一律 502」——可转移失败耗尽走 503、全限流走 429 都是既有行为。
> 若要把它们统一成 502，属**改变既有映射**（会动到客户端重试语义与既有断言），不在本单范围内，
> 需单独裁定。

**上游真实状态码**始终体现在 `error.upstream_status` 与 `call_logs.upstream_status` 里；
对外状态码不沿用上游（否则上游 401/402/403 原样透出会被调用方判成「我方 key 无效 / 我方余额不足」，
实际是渠道密钥/余额问题）。

### 透传的边界

- 只透传「上游确实返回了非 2xx」的场景。连接失败、响应超时、网关自身拒绝没有上游报文，保持既有中文文案。
- **对外状态码由失败终结方式决定，不是上游码的函数**：同一个上游 500 可能因为「还有候选可转移」而走到
  503，也可能因为「命中错误策略」而按策略值；调用方要判断「上游说了什么」，看 `error.upstream_status`
  与 `error.message`，不要用对外状态码反推上游码。
- 候选可转移（failover）时，若最终全部候选都失败，终结响应（503 / 429）里带**最后一次**上游报文原文与状态码，
  不再只剩一句「无可用渠道」——否则 5xx 这类可转移失败的上游原因会彻底丢失。
- 流式与非流式共用同一套错误分支代码：`stream=true` 时上游在 SSE 开始前返回非 2xx，走的就是这套透传；
  若上游已经返回 200 才开始 SSE，则 SSE 帧（含流内 `error` 帧）按原样透传给客户端，网关不改写。

**流内 `error` 帧的留痕口径**（响应已 200 下发，无法再改状态码，只能保证记录与原因准确）：

| 情形 | 错误类型 | 计费 | 调用日志 | `upstream_status` | 渠道健康 |
| --- | --- | --- | --- | --- | --- |
| error 帧 + **无** usage | `UsageMissingError{Reason: error_frame}` | 按可见内容估算（既有口径） | `failed`，`error_message` = 「上游流式响应中断（流内 error 帧，按估算计费）：<上游 error 帧摘要>」 | 上游真实码（此路 200） | 计入失败（既有口径） |
| error 帧 + **有** usage | `UpstreamErrorFrameError`（新增，**不**属于 usage 缺失） | 按上游**实际** usage 结算（不估算、不落额外 failed 账单） | `failed`，`error_message` = 「上游流式响应中断（流内 error 帧，按上游 usage 计费）：<上游 error 帧摘要>」 | 上游真实码（此路 200） | 保持既有成功回喂（本单不改健康/选路口径） |

两种情形都落结构化标记：`decision.usage_reason=error_frame` / `decision.upstream_error_frame=true`；
error 帧摘要落 `error_message` 前统一过 `errbody.MaskSecrets(…, ureq.Cred)`（上游可能在报文里回显它收到的凭据）。
`upstream_status` 写上游真实 HTTP 码（流式已 200 → 写 200），使「上游 HTTP 200 但流内报错」与
「上游 4xx/5xx」「网关拒绝」三者可区分。

**账单行不变**（刻意的）：这类请求上游确实产出了内容并给了 usage，`billing_records` 仍是一条按实际
usage 结算的 `completed` 记录，金额与既有口径一致；失败结论落在 `call_logs.status=failed`
（账单详情页直接展示关联的调用日志）。若要求账单行也标 `failed`，等于改 `status` 枚举语义与
汇总口径（影响：账单页成功/失败计数、失败率、对账口径、`credits_consumed` 的「有扣费却记失败」），
需产品确认后再动，本单不做。

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
| 金额 | `billing_records` | 分两类，**不要混读**：① 未产生上游内容的失败（上游 4xx/5xx、连接失败、网关拒绝）→ `status='failed'`、`credits_consumed=0`；② 上游已 200 并产出内容、只在流内报错或结算失败（见 §1.4）→ 账单行仍是按上游实际 usage 结算的 `completed`，失败结论只在 `call_logs.status='failed'`。判「算不算失败」看调用日志，判「扣了多少钱」看账单行 |

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

### 查询口径（按 request_id 定位时怎么读这几个字段）

| `call_logs.status` | `upstream_status` | 含义 |
| --- | --- | --- |
| `failed` | 400 / 401 / 422 / 500 … | 上游返回非 2xx；对外 HTTP 状态码是网关语义（502/503），**不是**这一列的值 |
| `failed` | **200** | **上游 HTTP 200、但流内下发 error 帧**（或流式中断/结算失败）。单看 200 像「上游成功」，必须结合 `status=failed` 与 `error_message` 一起读；结构化过滤可用 `decision.usage_reason=error_frame` / `decision.upstream_error_frame=true` |
| `failed` | `NULL` | 没有上游状态码：连接失败、响应超时、构建失败，或网关自身拒绝（鉴权/余额/路由/协议不支持） |
| `completed` | `NULL` | 成功；上游未返回 usage 时 `error_message` 标「按估算计费」 |

> 一句话：**`upstream_status` 是上游真实 HTTP 码，不是本次请求的成败结论**；
> 成败看 `call_logs.status`，扣费看 `billing_records`。

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
| `TestCallLog_PgRoundTrip_InvalidUTF8Payload` | 真库：非法 UTF-8 + `0x00` 的错误体仍能落库并读回（清洗后不丢内容） |
| `TestCallLog_PgRoundTrip` / `TestCallLog_PgRoundTrip_InvalidUTF8Payload` | 真库用例改为**自建临时库 + 跑全部迁移**（`internal/testutil/pgtest`），CI 里不再「库里没表就跳过」假绿 |
| `TestServeUpstreamError_DownstreamNeverSeesChannelCredential` | 上游回显渠道凭据时，对外响应与 `resp_body`/`error_message` 都不得出现该凭据，同时除凭据外内容仍可见 |
| `TestServeUpstreamError_OutwardMessageHasNoUpstreamAddress` | 断网时对外文案不含主机名/IP/端口/URL；完整错误（含地址）留在调用日志供运营定位 |
| `TestContract_UpstreamStatusOmittedWhenAbsent` | 连接失败 / 已送达超时 / 网关拒绝三条无上游码路径：`error.upstream_status` **字段不存在**（不是 `null`/0），顶层也没有 |
| `TestContract_MessageExactMaskOnly` | 对外 message 只遮蔽本次凭据：request-id、模型名等高熵内容原样保留；401 也透原文 |
| `TestContract_MessageTruncatedTo512` | 对外 message 按 UTF-8 边界截断到 512 字节 |
`backend/internal/domain/gateway/stream_error_frame_test.go`（流内 `error` 帧的留痕口径）：
| 用例 | 覆盖 |
| --- | --- |
| `TestServeStream_ErrorFrameWithUsageRecordedFailed` | error 帧 + usage（表驱动四种顺序：usage 在 error 帧前/后 × 有无 `[DONE]`）：调用日志记 `failed`（此前误记 `completed`）、`error_message` 带上游原文、`upstream_status=200`；计费仍按上游实际 usage（10/5，不估算、不额外落 failed 账单）；下游 SSE 仍是上游 error 帧原文；请求级结论计为失败 |
| `TestServeStream_ErrorFrameWithoutUsageKeepsUpstreamDetail` | error 帧 + 无 usage：记 `failed`、`error_message` 带出上游 error 帧原文（此前只有归因、没有原因）、`upstream_status=200`、下游仍是原文 |
| `TestStreamConversionErrorFrameWithUsageRecordedFailed` | 转换泵（anthropic 入站 → openai-chat 上游）同一口径 |
| `TestUpstreamErrorFrameError_Semantics` | 新错误类型可 `errors.As` 识别，且**不**被当成 usage 缺失（否则计费会从实际 usage 退化成估算） |

## 待拍板（口径分歧，勿擅自改）

| 项 | 现状（已实现） | 分歧 | 影响面 |
| --- | --- | --- | --- |
| `error.type` | **上游原文**（缺失时按对外状态码归类） | 契约复核建议改为**网关归类**：上游 401 的 `type=authentication_error` 配上对外 503 会自相矛盾（body 说鉴权错、状态码说服务不可用） | 改则客户端看到的错误大类与状态码一致；不改则严格「原文透出」。改一处需同步 `contract_*` 用例与本文档 |
| 账单行是否也标 `failed` | 上游已 200 且产出内容的失败（流内报错/结算失败）账单仍是按实际 usage 结算的 `completed`，失败结论只在 `call_logs.status` | 运营按账单页筛「失败」时看不到这类请求 | 改则要动 `billing_records.status` 枚举语义与汇总口径（失败率 KPI、错误信息 TOP、对账口径、`credits_consumed` 的「有扣费却记失败」） |
