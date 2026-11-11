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
| `error.message` | **上游原文** | 见上表；不做凭据清洗（原文原样还给**同一个**调用方），只按 UTF-8 边界截断 |
| `error.type` | 网关 | 按对外状态码归类（429 → `rate_limit_error`，5xx → `server_error`，其余 → `invalid_request_error`）。OpenAI 形状整段透传时不生成此字段（上游自己的 `type` 原样保留） |
| `error.code` | 网关 | 固定 `upstream_error`：标识「本次失败由上游拒绝引起」，不随状态码/文案变化 |
| `upstream_status` | 上游真实状态码 | **顶层**字段，两种形状同一位置（OpenAI 形状不允许改 `error` 子对象，故不放里面） |
| `request_id` | 网关 | 与响应头 `x-request-id` 同值；运营在账单页定位一次请求的唯一钥匙 |

### 1.3 上限与截断

- 读取上限 64KB（`upstreamErrReadBytes`），对外上限 8KB（`upstreamErrOutBytes`）。
  **读取上限必须显著大于对外上限**：读取层就静默截断的话，既拿不到完整内容、也无法诚实标注「已截断」。
- 超限时按 UTF-8 字符边界截断，截断标注与被截原文**落在同一字段**（`error.message` 末尾）：
  `…（已截断：上游原文共 12000 字节，超过 8192 字节对外上限）`——调用方无需另查元数据即可判断完整性。
- 截断后不再是合法 JSON 的 OpenAI 形状，退化为「原文文本进 message + 标注」（内容不丢，只是不能逐字段解析）。

### 1.4 流式

流式与非流式共用同一套错误分支代码：`stream=true` 且上游在 SSE 开始前返回非 2xx → 走本契约；
上游已回 200 开始 SSE 后，SSE 帧（含流内 `error` 帧）按原样透传，网关不改写。

## 2. 失败留痕（账单 + 调用日志）

一次失败要能被运营按 `request_id` 查到，**账单与调用日志必须成对写入**：
账单页按 `request_id` 检索是经 `call_logs.request_id` 关联 `billing_records` 的
（见 `internal/domain/billing/query.go` 的 `EXISTS` 子查询），只落一边就搜不到。

| 字段 | 位置 | 说明 |
| --- | --- | --- |
| `request_id` | `call_logs.request_id` | 与响应体 `request_id` 同源 |
| 上游状态码 | `call_logs.upstream_status`（迁移 0019） | 结构化小整型；`NULL` = 无上游状态码（连接失败/超时/网关拒绝） |
| 上游错误体 | `call_logs.resp_body` + `error_message` | 上游响应体（读取上限 64KB，落库前做凭据样式清洗）记录在 `resp_body`；`error_message` 形如「上游返回 400: {上游报文}」 |
| 渠道/密钥/模型 | `call_logs.channel_id / channel_key_id / internal_model_id / model` | 定位是哪条渠道哪次尝试 |
| 时间 | `call_logs.created_at` | 落库时间 |
| 耗时 | `call_logs.duration_ms` | 本次请求耗时 |
| 金额 | `billing_records`（`status='failed'`、`credits_consumed=0`） | 失败不扣费，但必须有账 |

> 落库/日志用 `upstreamErrBody.Clean`（凭据样式清洗后），对外透传用 `upstreamErrBody.Raw`（原文）。
> 两个视图分开是刻意的：「对外原文原样」与「存储不落明文凭据」要求相反，混用任何一个都会踩另一条红线。

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

## 3. 验证

`backend/internal/domain/gateway/upstream_error_passthrough_test.go`：

| 用例 | 覆盖 |
| --- | --- |
| `TestServeUpstreamError_ShapeTable` | **10 种形状 × 流式/非流式 20 条**：①OpenAI 标准 ②嵌套 details ③只有 message ④非 JSON 纯文本 ⑤HTML 错误页 ⑥空体 ⑦超长 12KB ⑧5xx ⑨401 鉴权错 ⑩顶层数组。每条断言「对外状态码 = 上游状态码」「形状对应判据下内容完整可见」「`request_id`/`upstream_status` 齐全」「账单 + 调用日志按 `request_id` 可查且带上游状态码/错误体/渠道/模型/耗时」 |
| `TestServeUpstreamError_TerminalKeepsLastUpstream` | 可转移状态在上游只有单候选时走到「全候选失败」终结分支：对外仍是上游 500 + 上游报文，不退化成 503 聚合文案 |
| `TestServeUpstreamError_429TerminalKeepsUpstreamBody` | 上游 429 全候选限流：对外 429 + 上游报文 |
| `TestServeUpstreamError_ConnectionFailure` | 断网：502 + 点名「连接失败」原因，不编造上游状态码，账单 + 调用日志留痕 |
| `TestServeUpstreamError_Timeout` | 已送达后超时：504 + 点名超时原因，调用日志留痕且 `upstream_status` 为空 |
| `TestServeUpstreamError_SuccessStaysSuccess` | 反向回归：200 JSON / 200 含厂商扩展字段 / 200 SSE 逐字节透传，`[DONE]` 保留，成功记录不写 `upstream_status` |
| `TestUpstreamErrorPayload_Shapes` | 组装函数边界：截断后非法 JSON、标量 JSON、`error` 为字符串、上游自带 `request_id` 不被覆盖 |
| `TestServeRejectPaths_RecordBillingAndCallLog` | 网关拒绝分支（格式错误/缺 model/余额不足/模型未配置）账单与调用日志成对、`billing_id` 1:1 |
