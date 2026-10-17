# 渠道协议体系 Spec

> Status: ALIGNED
> Author: user
> Last updated: 2026-10-15

## Background

现有网关是**单协议 OpenAI 兼容网关**：

- 对外仅 `GET /v1/models`、`POST /v1/chat/completions`、`POST /v1/completions`、`POST /v1/embeddings` 四个端点（`backend/internal/domain/gateway/gateway.go` L194-206）；无 `/v1/messages` / `/v1/responses`。
- 上游请求由 `BuildUpstreamRequest` **重建**，只注入 `Content-Type` 与 `Authorization: Bearer <cred>`（`backend/internal/domain/gateway/openai.go` L50-60），下游头除 `Authorization` / `x-session-id` 外全部丢弃。
- 渠道协议是**单值枚举** `openai-compat`（`backend/internal/domain/channel/store.go` L39-42）。
- 凭据在**渠道级** `channel_keys`（多密钥，SM4 密文）；计费已固定为三段 `billing.Usage`（input / output / cache_read，`backend/internal/domain/billing/rates.go`，口径见「usage 归一化」与 AC-16）。
- 已有 `sys_configs`（key/value JSONB）表；已有 `domain/sync` 包（拉 models.dev `/api.json` 做**价格 watchlist 告警**，与本功能无关）。

本 spec 定义「**渠道协议体系**」：把基础协议从 1 个扩到**代码固定的 3 个**（openai-chat / openai-responses / anthropic-messages），引入「渠道协议」实体作为可复用配置模板，支持特殊制式请求头的透传/注入，并由外挂「预设包」物化与同步。

## In scope

- 基础协议 3 个（代码固定）：`openai-chat` / `openai-responses` / `anthropic-messages`。
- 新增入站端点：`POST /v1/responses`（openai-responses）、`POST /v1/messages`（anthropic-messages）；`/v1/chat/completions` 归 openai-chat。
- 新增实体「渠道协议」`channel_protocols`：由预设包 provider 条目物化，**可编辑**，记录来源 ID 与 `edited`。
- 预设包拉取：**首次启动（初始化）自动拉取一次**；后续由管理员在前端点击按钮触发同步。
- 同步语义：未编辑（标记「同步」）→ 直接覆盖；已编辑（标记「已编辑」）→ 展示差异 + 二次确认覆盖；预设条目删除/改名 → **保留**并标记「来源已失效」。
- 渠道引用渠道协议（可选）；未命中渠道协议时在渠道内联配置。
- 渠道 / 渠道-模型级 `protocols` 覆盖（模型级语义为**可达协议子集**）。
- 请求头策略：`passthroughMode`(none/whitelist/blacklist/all，默认 none) + `whitelist`/`blacklist` + `set`(支持变量) + **内置强制黑名单**。
- 鉴权注入：`auth`(header / scheme / query)。
- 响应头：入站基础协议**硬编码** + 全局上游响应头白名单（存 `sys_configs`，**系统菜单**配置，默认空）。
- usage 归一化到三段（输入/缓存读/输出）；计费失败 → **报错并禁用渠道**。
- 路由：入站协议识别 → 选中渠道 + 内部模型 → 直通判定。

## Out of scope

- 一期**不做跨协议转换**（未命中直通时返回固定错误）；二期再实现。
- 不扩展 `/v1/completions`、`/v1/embeddings` 的协议语义（维持现状，归 openai-chat）。
- 不消费 models.dev **v2**（只对齐 v1）。
- 不做「渠道内联配置一键沉淀为渠道协议」（加分项）。
- 不做响应头配置化（硬编码 + 全局白名单除外）。
- 不内置硬编码预设包（全部外挂）。
- 不改动计费引擎公式与三段口径（仅做 usage 归一化，口径定义见「usage 归一化」）。
- 不消费预设包 `env` / `npm`（运行时忽略）。
- 不做外挂仓库的布局 / 加载逻辑（只定 JSON 格式与字段，见 `docs/preset-pack-format.md`）。

## Assumptions

- 预设包 JSON 结构以 `docs/preset-pack-format.md`（对齐 models.dev v1）为**唯一依据**；预设站点地址由**环境变量**配置，**未设置则不拉取**（不报错、不阻塞启动）。
- 凭据仍存渠道级 `channel_keys`（多密钥）；渠道协议只描述鉴权的**注入方式**，不含密钥。
- 基础协议只有代码固定的 3 个；上游制式不在此列时视为「无渠道协议」，走渠道内联配置。
- 计费口径为三段（输入=纯输入、缓存读、输出含推理），定义与各协议字段映射见「usage 归一化」与 AC-16；本次仅新增各协议的 usage 归一化，不改公式本身。
- 现有 `domain/sync`（价格 watchlist）与本预设同步是**两个不同功能**，仅复用「拉取 models.dev 形状 JSON」的解析思路。

## Solution

**实体**：新增 `channel_protocols`（渠道协议模板）；`channels` 增列 `channel_protocol_id`（可空）；`channel_models` 增列 `protocols`（模型级可达协议子集）；`sys_configs` 增 key `gateway.upstream_response_header_whitelist`（默认空）。

**协议适配层**：3 个基础协议各实现一个适配器，统一接口：识别入站端点 / 构建上游请求 / 注入鉴权与请求头 / 解析 usage / 硬编码响应头。入站协议由「路径 → 基础协议」映射识别。

**usage 归一化（三段口径）**：三段为 `input`（纯输入，不含缓存读；上游缓存写 token 并入）/ `cache_read` / `output`（含推理）；费率键同为三个（`input`/`output`/`cache_read`）。各协议字段映射与已知偏差见 AC-16。统计总量、缓存命中率分母、上下文分档依据统一为「总输入 = input + cache_read」。

**上游原始 usage 落库（诊断）**：每次拿到上游响应的请求，其 usage 原文随 `call_logs.upstream_usage`（JSONB，迁移 `0012_call_logs_upstream_usage.sql`）落库，形态为 `{"Protocol":"<基础协议名>","Usage":{...上游原始键值...}}`；流式响应按「同名键后写覆盖」合并多个分片（Anthropic 的 message_start/message_delta）。协议名必须同存——各协议字段名与包含关系不同，脱离协议无法事后判断口径；`billing_records.tokens` 只有归一化后的三段，看不出上游原文。

存量数据由迁移 `0011_unify_usage_three_segments.sql`（启动自动执行）归一化：历史 `billing_records.tokens` 按 `input = (cache_read > input ? input : input − cache_read) + cache_write`、`output = output + reasoning` 换算（对「输入含缓存读」的历史行三段总和不变，历史金额 `credits_consumed` 不重算；`cache_read > input` 的行判定为「输入不含缓存读」口径，不再二次相减）；`external_models.sale_rates`、`channel_models.cost_rates` 删除 `cache_write`/`reasoning` 键（原推理单价按定调移除，不折算进 output）；`routing.cost_sample` 同步为三段。健康探测遥测 `probe_logs` 由迁移 `0013_probe_logs_three_segments.sql` 统一：`input_tokens` 减去 `cached_tokens`（`total_tokens` 数值不变），探测自身的 usage 解析也按三段归一（含 DeepSeek 原生 `prompt_cache_hit_tokens` 兜底）。

**调用日志可读性**：`call_logs.req_messages` 必须是**真实请求增量**（失败请求同样展示，不再被 `error_message` 顶替——前端账单详情/会话详情统一用 `MessageBubbleGroup` 渲染成气泡，并保留「原始 JSON」折叠）；`resp_body` 只在真有上游响应体时有值，网关侧失败（构建/连接/读取失败、已送达但失败）留空，原因只落 `error_message` 与决策轨迹的 `reason`，避免三处重复同一句文案；上游错误响应（429/401/5xx）的原文照实落 `resp_body`。

**请求头引擎**：按 `passthroughMode` 从下游选取头 → 减去**内置强制黑名单**（常量表，不入库、不可配置）→ 叠加 `set`（变量替换）→ 由 `auth` 注入鉴权头 → 由基础协议补齐必需头（Content-Type 等）。鉴权头**不进入**用户可编辑的 `set`。

**路由**：入站协议识别 → 选中渠道 + 内部模型 → 判定「入站协议 ∈（渠道协议 ∩ 渠道 ∩ 模型）的 protocols 子集」→ 命中直通；未命中一期返回固定错误，且不发起上游请求。

**预设同步**：初始化时拉取一次并物化 `channel_protocols`；后续由管理员按钮触发，按 `edited` 标记分流。

## Edge cases & risks

| Category | Notes |
|---|---|
| Boundary conditions | 入站协议 ∉ 模型可达 protocols → 固定错误；`protocols` 为空 → 视为不支持任何协议 |
| Failure modes | 首次拉取失败 → 不阻塞启动、仅日志、列表为空；usage 缺失 → 报错并禁用渠道；同步遇已编辑条目 → 需二次确认 |
| Risks | 头策略误配导致下游头（凭证 / Cookie）泄漏；协议键命名与 models.dev v2 不一致；预设站点不可用导致无可引用协议 |
| Mitigation | 强制黑名单不可配置；鉴权头不进用户 `set`；白名单默认空；预设条目删除不级联删除 |

## Acceptance criteria

- AC-1 OpenAI Chat 入站：客户端 `POST /v1/chat/completions` 命中渠道协议（其 protocols 含 `openai-chat`）→ 上游收到 `<baseUrl>/chat/completions`，零转换，响应体原样回传。
- AC-2 Anthropic 入站：客户端 `POST /v1/messages` 命中 protocols 含 `anthropic-messages` → 上游收到协议 `path`（默认 `/v1/messages`），鉴权按 `auth` 注入（`x-api-key` 或 `Authorization`）。
- AC-3 OpenAI Responses 入站：客户端 `POST /v1/responses` 命中 protocols 含 `openai-responses` → 上游收到协议 `path`（默认 `/responses`）。
- AC-4 直通未命中：入站协议 ∉ 模型可达 protocols → 返回固定错误（HTTP `400` + OpenAI 标准错误体 `{"error":{...}}`，`type=unsupported_protocol`、`code=invalid_request_error`），且**不发起上游请求**。
- AC-5 头策略 `none`：下游自定义头（如 `x-opencode-session`）**不出现**在上游请求中。
- AC-6 头策略 `whitelist`：`whitelist=["x-opencode-session"]` → 该头原样出现在上游请求，其余下游头不出现。
- AC-7 头策略 `blacklist`：`blacklist=["x-foo"]` → 除 `x-foo` 与强制黑名单外，其余下游头透传。
- AC-8 强制黑名单：任何模式下，`Cookie` / `Host` / `Accept-Encoding` / `Connection` 等强制黑名单头均**不出现**在上游请求中。
- AC-9 变量替换：`set` 中 `{api_key}` 替换为当前渠道密钥；`{client_header:NAME}` 取下游原始头；鉴权头**不出现**在用户可编辑 `set`。
- AC-10 响应头默认：全局白名单为空时，客户端收到的响应头仅含基础协议硬编码项，上游其余响应头不透传。
- AC-11 响应头白名单：白名单含某头后，该上游响应头透传给客户端。
- AC-12 初始化拉取：预设站点地址**环境变量未设置** → 不发起拉取、不报错、列表为空；已设置且拉取成功 → `channel_protocols` 有数据；已设置但拉取失败 → 进程正常启动、日志记录、渠道协议列表为空。
- AC-13 同步-未编辑：管理员触发同步，未编辑条目被预设值覆盖，标记回到「同步」。
- AC-14 同步-已编辑：管理员触发同步，已编辑条目先展示差异，二次确认后才覆盖；放弃则不改动。
- AC-15 预设条目删除 / 改名：对应渠道协议**不删除**，标记「来源已失效」；引用它的渠道仍可用。
- AC-16 usage 归一化（三段口径）：三个协议各自响应的 usage 均归一化为三段 `input/output/cache_read`，计费按 `input×单价 + cache_read×单价 + output×单价` 直接求和（无净输入减法）。口径与映射：
  - **input = 纯输入，不含缓存读**；上游的「缓存写」token 属于输入 token，计入 input。
  - **cache_read = 命中提示词缓存读取的输入**。
  - **output = 生成 token，含推理**（不单列推理段）。
  - OpenAI chat/completions：`input = prompt_tokens − prompt_tokens_details.cached_tokens`（负数钳 0）、`cache_read = cached_tokens`、`output = completion_tokens`（含 reasoning）；缓存写 token 天然在 `prompt_tokens` 内，故已落入 input。
  - OpenAI responses：`input = input_tokens − input_tokens_details.cached_tokens`（钳 0）、`cache_read = cached_tokens`、`output = output_tokens`（含 reasoning）。
  - OpenAI chat（DeepSeek 原生风格兜底）：命中数优先取 `prompt_tokens_details.cached_tokens`，缺失时回落顶层 `prompt_cache_hit_tokens`（两者同义，官方文档写明相等）；`prompt_tokens = hit + miss` 属「含」口径，同样按 `input = prompt_tokens − 命中数` 归一。
  - Anthropic messages：`input = input_tokens + cache_creation_input_tokens`（缓存写并入输入）、`cache_read = cache_read_input_tokens`、`output = output_tokens`（thinking 已含其中）。
  - 已知偏差（明示接受）：Anthropic 官方与百炼**显式缓存**对缓存写按输入价 1.25× 计费，本平台把写入 token 并入 input 按 1× 计，少收约 25%；对按「按小时存储费」计价的缓存形态（智谱、火山显式、Gemini 显式）本模型无法表达，不计入。
  - 上下文分档依据为**总输入**（input + cache_read）；缓存命中率分母同为总输入。
- AC-17 usage 缺失（放宽）：上游响应缺失或无法解析 usage 属极端协议违规 —— 因响应正文已产出/透传，**不退预扣**，而是按可见内容（请求 prompt + 响应正文/推理/工具调用）**估算 token 正常结算**，同时将对应渠道置为禁用（`state=DISABLED`）。流式与非流式行为一致：非流式透传上游正文（`200`），流式内容已透传无法回改。
- AC-18 模型级子集：某模型 `protocols` 仅含 `anthropic-messages` 时，即使渠道协议支持 openai-chat，该模型对 openai-chat 入站返回 AC-4 的固定错误。
- AC-19 上游原始 usage 落库：非流式与流式（含计费失败、流式中断、usage 缺失按估算计费三种分支）均把上游 usage 原文写入 `call_logs.upstream_usage`，形态 `{"Protocol":...,"Usage":{...}}`；流式多分片按同名键后写覆盖合并；上游未给 usage 时该列为 `NULL`。账单详情面板展示该原文（`Protocol` + 原始键值），便于与三段归一化结果对照。
- AC-20 探测与账单口径统一：`probe_logs.input_tokens` 为纯输入（不含缓存读）、`total_tokens` 为三段合计（采用上游 `total_tokens`，其定义即 prompt+completion；缺失时按三段求和兜底），探测记账同样走三段 `billing.Usage`。

## Open questions

（无待决项。AC-4 错误形状已定案：HTTP `400` + OpenAI 标准错误体，`type=unsupported_protocol`、`code=invalid_request_error`；网关客户端链路用 OpenAI 形状，不复用管理端 `resp` 包编码。）

## Core entities (ontology)

| Entity | Type | Key fields | Relationship |
|---|---|---|---|
| BaseProtocol | 代码常量（3） | `openai-chat` / `openai-responses` / `anthropic-messages` | 被 ChannelProtocol 引用 |
| ChannelProtocol | 表 `channel_protocols` | id, name, preset_source_id, edited, source_status, base_url, protocols, auth, request_headers, models | 由 PresetPack 物化；被 Channel 引用 |
| Channel | 表 `channels` | channel_protocol_id(新), base_url, state | 引用 ChannelProtocol（可选） |
| ChannelModel | 表 `channel_models` | protocols(新), cost_rates | 属 Channel；可收窄 protocols |
| ChannelKey | 表 `channel_keys` | credential_enc | 属 Channel（凭据来源） |
| PresetPack | 外部站点 | provider 条目（对齐 models.dev v1 + `gateway` 块） | 物化为 ChannelProtocol |
| SysConfig | 表 `sys_configs` | `gateway.upstream_response_header_whitelist` | 全局上游响应头白名单 |

## Interview metadata

- Mode: --spec-only
- Waves: N/A（本轮之前已通过多轮访谈收敛）
- Final ambiguity: ~10%
- Status: PASSED

### Clarity breakdown

| Dimension | Score | Weight | Weighted |
|---|---|---|---|
| Goal | 0.95 | 0.40 | 0.380 |
| Scope | 0.90 | 0.25 | 0.225 |
| AC | 0.90 | 0.25 | 0.225 |
| Context | 0.95 | 0.10 | 0.095 |
| Ambiguity | | | ~9.5% |

### Ontology convergence

本轮首次成文。稳定实体：BaseProtocol / ChannelProtocol / Channel / ChannelModel / ChannelKey / PresetPack / SysConfig。
