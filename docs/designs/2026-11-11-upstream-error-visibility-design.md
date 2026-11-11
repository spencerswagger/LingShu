# 上游错误透传：对外错误契约与失败留痕（2026-11-11，v2）

> 起因：运营新开一条上游渠道（OpenCode Go）后调用不通，调用方只拿到一句通用中文错误，
> 上游真实报文是「该模型在欧洲/美国以外托管，请到控制台把区域切成 Global」——运营看到这句就能自己解决，
> 但当时既拿不到原文，账单里也查不到这次失败。
>
> v2 修订（取代 v1 的「状态码不变、只补 upstream_status」口径）：**状态码也沿用上游**。
> v1 只把上游原文塞进 `error.message`，但仍把 4xx 改写成 502——调用方看到 502 会判成「网关故障」并放弃重试/自救，
> 而实际上是一个可自查的参数/区域/配额问题。按「凡上游非 2xx，就把上游的错误完整交还调用方」重新定稿。

## 1. 对外错误契约（`/v1/*`）

### 1.1 状态码

**上游返回什么状态码，对外就回什么状态码**（4xx → 4xx，5xx → 5xx）。
唯一例外是运维在协议级错误策略里**显式**配置了 `DownstreamStatus`（那是人工配置的覆盖，不是按内容特判）。

- 候选可转移（failover）时，若全部候选都失败：终结响应取**最后一次**上游失败的状态码与报文。
- 完全没有上游报文（连接失败/超时/构建失败）：502；最后一次是超时 → 504，文案点名原因。
- 连上游都没联系上（候选全被本地拦截：会话满/限流/凭据缺失/协议不支持）：503「无可用渠道」。
- 1xx/3xx 不属错误语义（且 304 禁带响应体）：按 502 输出，保证错误内容可见。

> **状态码有两个，不能混用**：对外 `status`（可能被策略覆盖、可能因 304 改成 502）与
> 响应体顶层 `upstream_status`（**恒为上游真实码**）。策略把 422 覆盖成 418、或上游回 304 时，
> `upstream_status` 仍报 422 / 304，否则字段名就是假的（`writeUpstreamError(status, upstreamStatus, body)`）。
>
> **错误策略里任何与内容相关的匹配（含 `MessageRegex`）只可能来自运维显式配置**，默认不启用；
> 除非被显式配置，网关不按上游文案做任何分类或改写。

### 1.2 响应体：按**形状**透传，零内容特判

判定只看响应体形状，不看文案、关键词、厂商、状态码——不存在「区域限制」「余额不足」这类分支，
所有上游错误同等处理。

| 上游 body 形状 | 对外 body | 说明 |
| --- | --- | --- |
| OpenAI 形状：顶层对象且 `error` 为对象 | **整段原样**（顶层每个键都以原始字节搬走） | 只追加我们自己的**顶层**字段 `request_id` / `upstream_status`；上游 `error` 子对象（含 `details` 等嵌套、未知字段）逐路径不改。上游自带 `request_id` 时不覆盖。 |
| 其它 JSON（顶层数组/标量/`error` 非对象） | 网关错误壳，`error.message` = **原始 JSON 文本** | 结构与嵌套原样保留在文本里，不做字段抽取 |
| 非 JSON（HTML 错误页 / 纯文本） | 网关错误壳，`error.message` = **原文文本** | 绝不替换成中文通用文案 |
| 空体 | 网关错误壳，`error.message` = 「上游返回 N，响应体为空」 | 没有内容可透传，但不返回空 message |

网关错误壳（非 OpenAI 形状时）：

```json
{
  "error": {"message": "…上游原文…", "type": "server_error", "code": "upstream_error"},
  "upstream_status": 503,
  "request_id": "…"
}
```

字段口径：

| 字段 | 来源 | 说明 |
| --- | --- | --- |
| `error.message` | **上游原文** | 见上表；只按 UTF-8 边界截断。**唯一改写**：本次转发实际使用的渠道凭据原值 → `***`（见 §1.5） |
| `error.type` | 网关 | 按对外状态码归类（429 → `rate_limit_error`，5xx → `server_error`，其余 → `invalid_request_error`）。OpenAI 形状整段透传时不生成此字段（上游自己的 `type` 原样保留） |
| `error.code` | 网关 | 固定 `upstream_error`：标识「本次失败由上游拒绝引起」，不随状态码/文案变化 |
| `upstream_status` | 上游真实状态码 | **顶层**字段，两种形状同一位置（OpenAI 形状不允许改 `error` 子对象，故不放里面） |
| `request_id` | 网关 | 与响应头 `x-request-id` 同值；运营在账单页定位一次请求的唯一钥匙 |

### 1.2.1 凭据遮蔽（对外同样遮蔽）

上游偶尔会把收到的鉴权头原样回显在错误体里（如 `{"error":{"message":"invalid key sk-xxx"}}`）。
对外透传若原样输出，等于把**渠道密钥**发给下游租户（任何持有自己 token 的用户都能拿到）——
主干的旧实现是「一律替换成中文通用文案」，所以不泄漏；本 PR 改造后必须继续不泄漏。

口径：对外与运营侧**共用同一套规则**（`internal/pkg/errbody.MaskSecrets`）——
只把**本次实际使用的凭据原值**替换成 `***`（含 `Bearer <cred>` 里的 `<cred>` 一段），
不做前缀/高熵/字段白名单清洗；因此除凭据那一处外仍逐字节透传。

- 终结响应（候选全败）用的凭据随 `fwdOutcome.upstreamCred` 一路带过去，保证「最后一次上游报文」同样被遮蔽。
- 截断标注里报的字节数是**遮蔽前**的原文长度（不能让调用方看到对不上账的数字）。

### 1.3 上限与截断

- 读取上限 64KB（`upstreamErrReadBytes`），对外上限 8KB（`upstreamErrOutBytes`）。
  **读取上限必须显著大于对外上限**：读取层就静默截断的话，既拿不到完整内容、也无法诚实标注「已截断」。
- 超限时按 UTF-8 字符边界截断，截断标注与被截原文**落在同一字段**（`error.message` 末尾）：
  `…（已截断：上游原文共 12000 字节，超过 8192 字节对外上限）`——调用方无需另查元数据即可判断完整性。
- 截断后不再是合法 JSON 的 OpenAI 形状，退化为「原文文本进 message + 标注」（内容不丢，只是不能逐字段解析）。

### 1.3.1 传输层失败：可读，但不带渠道地址

连接失败/超时/断连没有上游报文，对外要给「哪一类失败」，但**绝不能把渠道地址写进 message**：
`net/http` 的原始错误形如
`Post "http://internal-gw.corp.local:18080/chat/completions": dial tcp 10.0.3.7:18080: connect: connection refused`，
原样透出等于把内网拓扑交给调用方。实现按 **Go 标准库错误类型与 errno** 分类（不读上游响应体，因此不属于按文案特判）：

| 情形 | 对外文案（示例） | 对外状态码 |
| --- | --- | --- |
| `ECONNREFUSED` | 上游连接失败：连接被拒绝（connection refused） | 502 |
| 超时（`net.Error.Timeout` / context deadline） | 上游请求超时（等不到上游响应） | 504 |
| `ECONNRESET` / `EPIPE` | 上游连接失败：连接被上游重置（connection reset） | 502 |
| `io.EOF` / `io.ErrUnexpectedEOF` | 上游连接失败：连接被上游中断（对端关闭连接） | 502 |
| `net.DNSError` | 上游连接失败：域名解析失败（DNS） | 502 |
| TLS / 证书类错误 | 上游连接失败：TLS 握手失败 | 502 |
| 未归类 | 上游连接失败：<脱敏后的底层错误文本> | 502 |

兜底路径还会把 URL / IPv4(:port) / `[IPv6](:port)` / 带端口主机名统一替换成 `<地址>`（`redactUpstreamAddr`），
保证后续新增的未归类错误也不会把地址漏出去。

### 1.4 流式

流式与非流式共用同一套错误分支代码：`stream=true` 且上游在 SSE 开始前返回非 2xx → 走本契约；
上游已回 200 开始 SSE 后，SSE 帧（含流内 `error` 帧）按原样透传，网关不改写。

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

| 字段 | 位置 | 说明 |
| --- | --- | --- |
| `request_id` | `call_logs.request_id` | 与响应体 `request_id` 同源 |
| 上游状态码 | `call_logs.upstream_status`（迁移 0019） | 结构化小整型；`NULL` = 无上游状态码（连接失败/超时/网关拒绝） |
| 上游错误体 | `call_logs.resp_body` + `error_message` | `resp_body` = **上游原文逐字节**（上限 32KB，超限同字段标注）；`error_message` 形如「上游返回 400: {上游报文}」（摘要上限 4KB，超限标注指向 `resp_body`） |
| 渠道/密钥/模型 | `call_logs.channel_id / channel_key_id / internal_model_id / model` | 定位是哪条渠道哪次尝试 |
| 时间 | `call_logs.created_at` | 落库时间 |
| 耗时 | `call_logs.duration_ms` | 本次请求耗时 |
| 金额 | `billing_records`（`status='failed'`、`credits_consumed=0`） | 失败不扣费，但必须有账 |

> **三个视图**（`upstreamErrBody`，混用任何一个都会踩另一条红线）：
>
> | 视图 | 用途 | 口径 |
> | --- | --- | --- |
> | `Raw` | 对外透传 | 原文原样（含首尾空白），不遮蔽 |
> | `Clean` | 日志短字段 / 错误分类 | 按**样式**清洗（`sk-*`/Bearer/KV/高熵长串 → `***`）+ 512B 截断 |
> | `operatorView` | **运营落库**（`call_logs.resp_body`、`error_message`） | 原文逐字节，**只**把本次实际使用的凭据原值换成 `***`；超 32KB 同字段标注；最后过 `errbody.ToValidText` → 非法 UTF-8 字节换成 `�`、`0x00` 换成 `<0x00>` |
>
> 这里刻意推翻了早期「落库走 Clean」的口径：按样式清洗会把上游报文里的 request-id、
> 随机串、模型名一并抹成 `***`，运营读到的根本不是原文（实测一段报文从第 45 字节起整段消失）。
>
> **落库前还有一道「能否写进去」的闸**：上游报文是任意字节（GBK 错误页、截断的多字节字符、`0x00`），
> 而 PG 的 `text` 列会拒绝非法 UTF-8 与 `0x00`，**整行 INSERT 失败**——一次上游报错就把这次请求的
> 账单与调用日志一起丢掉。所以 `call_logs` / `billing_records` / `probe_logs` 的写入口都过
> `errbody.ToValidText`（非法字节 → `U+FFFD`，`0x00` → `<0x00>`，并在同字段标注发生过替换），
> 宁可「可辨认地不完整」，也不能整行消失。
> 精确遮蔽（只知道我们发出去的那把 key）既能保证「存储不落明文凭据」，
> 又能让除凭据外的每个字节保真——它比样式清洗更窄，也更准。

### 本次一并补齐的留痕缺口

以下分支此前会写出错误响应，但**没有**落调用日志（部分连账单也没有），导致用户拿着 `request_id` 在账单页搜不到：

- 请求体过大/不可读、请求体不是合法 JSON 对象、请求体缺少 `model`（400）
- 令牌标签解析失败、Auto 模型解析失败、路由失败（500）
- 预扣金额估算失败（500）
- 预扣被拒（402 余额不足 / 500 预扣异常）、账户余额为 0（402）
- Auto 模型全部成员无可用候选（503）

统一收敛到 `Gateway.rejectAndRespond`：failed 账单 + 调用日志 + 结构化异常日志 + 对外错误，一次写完。

未纳入：**鉴权失败（401，网关自身）**——此时没有可信的用户身份，且该分支是外部可打的公开面，
逐条落库会变成可被利用的写放大面；这类请求仍只有结构化异常日志。

## 3. 运营侧可见性（三个落点，缺一不可）

本节的读者是**运营**，不是下游调用方：判断标准只有一条——后台能看到上游返回的原始错误体。

### 3.1 调用日志 / 账单

失败请求落 `call_logs`（+ 成对 `billing_records`），单条含 `request_id`、`upstream_status`、
`resp_body`（上游原文）、`channel_id / channel_key_id / internal_model_id / model`、`created_at`、`duration_ms`；
运营按 `request_id` 在账单详情查回（`console` 投影直接返回 `RespBody`/`UpstreamStatus`）。

### 3.2 探活单次结果

`POST /api/v1/admin/channels/{id}/keys/{kid}/probe` 与 `.../models/{mid}/probe` 的 `ProbeOutcome` 新增：

```json
{"OK": false, "Error": "probe http status: 400", "UpstreamStatus": 400,
 "ErrorBody": "上游错误体原文（仅遮蔽本次凭据；>32KB 截断并同字段标注）", "State": "DRAIN"}
```

`Error` 仍是短文案（状态机 `LastErr` 也用它，不能塞 32KB）；原文在 `ErrorBody`。

**根因修复**：`channel/probe.go` 的 `doProbeAuth` 历史上在非 2xx 三个分支（429 / 401-403 / 其它）
只拼 `fmt.Sprintf("probe http status: %d", ...)`，**从不读 `resp.Body`**——上游报文被整段丢弃，
运营新建密钥后点探活只能看到「失败」两个字。现在三个分支都读（上限 64KB），
并带上游状态码，探活是运营建渠道后的第一个动作，必须自证。

### 3.3 探活历史

`GET /api/v1/admin/channels/{id}/probe-logs` 的每条 `ProbeLog` 新增 `ErrorBody` / `UpstreamStatus`，
落库列见迁移 `0020_probe_logs_error_body.sql`（`error_body TEXT`、`upstream_status SMALLINT`，
`NULL`=没打到上游）。事后翻历史仍能看到上游原文，而不是只有「probe http status: 400」。

### 3.4 上限（三处一致）

| 位置 | 读取上限 | 保存上限 | 超限行为 |
| --- | --- | --- | --- |
| 对外透传 `error.message` | 64KB | 8KB | 同字段追加「…（已截断：上游原文共 N 字节，超过 8192 字节对外上限）」 |
| `call_logs.resp_body` | 64KB | 32KB | 同字段追加「…（已截断：上游原文共 N 字节，此处只保留前 32768 字节）」；读满 64KB 时如实写「超过读取上限 65536 字节」 |
| `probe_logs.error_body` / `ProbeOutcome.ErrorBody` | 64KB | 32KB | 同上（同一套 `errbody.TruncateUTF8`） |
| **探活历史列表** `GET .../probe-logs` | 64KB（落库时） | **100 行 × 每行 4KB 预览** | 列表只给「这次为什么失败」的线索：超出的行按 4KB 预览 + 同字段标注「完整原文 N 字节见 error_body」；行数上限 100（历史值 500 行 × 32KB ≈ 16MB 单次响应） |

读取上限必须**显著大于**保存上限：读取层静默截断会导致「既拿不到完整内容、也无法诚实标注」。

## 5. 验证

`backend/internal/domain/gateway/upstream_error_passthrough_test.go`：

| 用例 | 覆盖 |
| --- | --- |
| `TestServeUpstreamError_RandomShapes` | **随机驱动，不绑定任何固定样例文案**：≥10 组随机值 + 8 类形状（OpenAI 形状 / 深嵌套 JSON / 平铺 JSON / 顶层数组 / 纯文本 / HTML / 空体 / 超长）× 流式/非流式。每条断言「对外状态码 = 上游状态码」「形状对应判据下内容完整可见」「`request_id`/`upstream_status` 齐全」「**`resp_body` 与上游原文逐字节一致**（超长则前缀逐字节 + 同字段截断标注）」「`error_message` 原样保留原文」 |
| `TestProbeModelNow_ExposesUpstreamRawBody` / `TestProbeKeyNow_ExposesUpstreamRawBody` | 同一套随机用例跑模型/密钥探测：`ProbeOutcome.ErrorBody` 逐字节等于上游原文，且与 `probe_logs.error_body` 一致 |
| `TestHandleProbeEndpoints_ExposeUpstreamRawBody` | HTTP 级（走 `HandleProbeModel`/`HandleProbeKey` + `resp.Write` 序列化）：响应 `Data.ErrorBody` 逐字节等于上游原文 |
| `TestHandleListProbeLogs_ReturnsStoredRawBody` | HTTP 级：探活历史返回 `probe_logs.error_body` 原文 + 上游状态码 |
| `TestProbeErrorBody_MasksOnlyExactCredential` | 只遮蔽本次凭据：报文里的 request-id / 模型名 / URL query / `<>&` 一律保留 |
| `TestReadUpstreamErrBodyViews` | 三视图口径：Clean 清洗、operatorView 仅遮蔽已知凭据 |
| `TestServeUpstreamError_DownstreamNeverSeesChannelCredential` | 上游回显凭据（OpenAI 形状 / 纯文本终结 / 429 终结）：对外响应与 `resp_body`/`error_message` 都不得出现渠道凭据，同时除凭据外逐字节保留（`***` 只落在凭据那一处） |
| `TestProbeLog_PgRoundTrip` / `TestCallLog_PgRoundTrip` | 真库读写：上游原文（含中文/HTML/换行）逐字节存得进、读得出（无 DSN 跳过） |
| `TestServeUpstreamError_TerminalKeepsLastUpstream` | 可转移状态在上游只有单候选时走到「全候选失败」终结分支：对外仍是上游 500 + 上游报文，不退化成 503 聚合文案 |
| `TestServeUpstreamError_429TerminalKeepsUpstreamBody` | 上游 429 全候选限流：对外 429 + 上游报文 |
| `TestServeUpstreamError_ConnectionFailure` | 断网：502 + 点名「连接失败」原因，不编造上游状态码，账单 + 调用日志留痕 |
| `TestServeUpstreamError_Timeout` | 已送达后超时：504 + 点名超时原因，调用日志留痕且 `upstream_status` 为空 |
| `TestServeUpstreamError_SuccessStaysSuccess` | 反向回归：200 JSON / 200 含厂商扩展字段 / 200 SSE 逐字节透传，`[DONE]` 保留，成功记录不写 `upstream_status` |
| `TestUpstreamErrorPayload_Shapes` | 组装函数边界：截断后非法 JSON、标量 JSON、`error` 为字符串、上游自带 `request_id` 不被覆盖 |
| `TestServeRejectPaths_RecordBillingAndCallLog` | 网关拒绝分支（格式错误/缺 model/余额不足/模型未配置）账单与调用日志成对、`billing_id` 1:1 |
| `TestUpstreamTransportNote_NoAddressLeak` | 传输层失败分类：连接被拒/超时/断连/DNS/未归类各自可读，且文案**不含主机名/IP/端口/URL** |
| `TestServeUpstreamError_ConnectionFailureNoAddress` | HTTP 级：断网时对外 502 + 点名连接失败，message 里没有 `127.0.0.1` / `http://` |
| `TestWriteUpstreamError_UpstreamStatusIsRealCode` | `upstream_status` 恒为上游真实码：上游 304（对外 502）、策略 422→418 两条路径都报 304 / 422 |
| `TestOperatorView_SanitizesInvalidUTF8` | 含非法 UTF-8 / `0x00` 的报文产出合法 UTF-8 并同字段标注；合法原文零改动 |
| `TestCallLog_PgRoundTrip_InvalidUTF8Payload` | 真库：非法 UTF-8 + `0x00` 的错误体仍能落库并读回（清洗后不丢内容） |
| `TestToValidText`（`internal/pkg/errbody`） | 整理规则：非法字节 → `U+FFFD`、`0x00` → `<0x00>`、合法内容逐字节不变 |
| `TestHandleListProbeLogs_BoundsPayload` | 探活历史：`Limit=500` 收敛到 100 行、每行原文 ≤4KB 且同字段标注、整包响应 ≤64KB |
| `TestCallLog_PgRoundTrip` / `TestCallLog_PgRoundTrip_InvalidUTF8Payload` | 真库用例改为**自建临时库 + 跑全部迁移**，CI 里不再「库里没表就跳过」假绿 |

`backend/internal/domain/gateway/stream_error_frame_test.go`（流内 `error` 帧的留痕口径）：

| 用例 | 覆盖 |
| --- | --- |
| `TestServeStream_ErrorFrameWithUsageRecordedFailed` | error 帧 + usage：调用日志记 `failed`（此前误记 `completed`）且 `error_message` 带上游原文；计费仍按上游实际 usage（10/5，不估算、不额外落 failed 账单）；请求级结论计为失败 |
| `TestServeStream_ErrorFrameWithoutUsageKeepsUpstreamDetail` | error 帧 + 无 usage：记 `failed` 且 `error_message` 带出上游 error 帧原文（此前只有归因、没有原因） |
| `TestStreamConversionErrorFrameWithUsageRecordedFailed` | 转换泵（anthropic 入站 → openai-chat 上游）同一口径 |
| `TestUpstreamErrorFrameError_Semantics` | 新错误类型可 `errors.As` 识别，且**不**被当成 usage 缺失（否则计费会从实际 usage 退化成估算） |
