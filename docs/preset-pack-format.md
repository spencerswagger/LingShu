# 渠道预设包 JSON 格式约定（v0.2 草案）

## 1. 目的

约定「渠道预设包」的 JSON 格式与字段，用于描述一个供应商的接入方式（base URL、鉴权、特殊请求头）及其模型清单（能力、价格、对应基础协议）。格式**对齐 models.dev v1**（`https://models.dev/api.json`），只做最小扩展，从而复用其模型能力与价格数据。

> 本格式**只对齐 v1**，不消费 v2（`/experimental/v2.0/`，仍为实验态）。

## 2. 设计原则

1. **对齐 models.dev v1**：provider / model 的字段名、两级嵌套形状、单位一律沿用 v1，不另造同义字段；v1 字段即使本网关不消费，也**原样保留**以保证可无损读入 `api.json`。
2. **扩展隔离**：本网关专有字段统一放进 `gateway` 命名空间，避免与 models.dev 现有 / 未来字段冲突（models.dev schema 是 strict，未知键会校验失败）。
3. **可覆盖**：包内值是「默认值」，渠道协议 / 渠道可逐层覆盖。
4. **开放枚举（本格式自身）**：`modalities`、`type`、`protocols` 的键在本格式内按开放枚举处理，消费方遇未知值应透传 / 忽略而非报错。注意：**v1 原文的 `modalities` / `type` 是封闭枚举**，开放对待是本网关的消费策略，非 v1 语义。

## 3. 顶层结构

一个 provider 的 JSON 对象（与 models.dev v1 `api.json` 中单个 provider 条目的形状一致，外加 `gateway` 块）：

```jsonc
{
  "id": "anthropic",
  "name": "Anthropic",
  "env": ["ANTHROPIC_API_KEY"],
  "npm": "@ai-sdk/anthropic",
  "api": null,
  "doc": "https://docs.anthropic.com/...",
  "models": { "...": {} },
  "gateway": { "...": {} }
}
```

多 provider 合并后形状 = `{ "<providerId>": { ...上例... } }`（与 models.dev v1 `api.json` 一致）。

## 4. Provider 字段

| 字段 | 类型 | 含义 | 来源 |
|---|---|---|---|
| `id` | string | provider 唯一 ID | models.dev v1 |
| `name` | string | 展示名 | models.dev v1 |
| `env` | string[] | 该 provider 需要的 API Key 环境变量名 | models.dev v1 |
| `npm` | string | 官方 AI SDK 包名 | models.dev v1 |
| `api` | string \| null | OpenAI 兼容 base URL | models.dev v1 |
| `doc` | string | 文档链接 | models.dev v1 |
| `models` | map | 模型表（见第 5 节） | models.dev v1 |
| `gateway` | object | **本网关扩展块**（见第 6 节） | 扩展 |

说明：
- `env` / `npm`：**本网关运行时不消费**（忽略），仅为与 models.dev v1 **完整对齐**而保留。`env` 语义为「该供应商读取 API Key 的环境变量名」，可作**前端提示**；`npm` 语义为「官方 AI SDK 包名」，v1 为**必填非空**，本格式原样保留、不参与运行时。
- `api`：v1 为**可选字符串**，**无 `${ENV}` 模板**（模板是 v2 特性，本格式不采用）。本网关实际使用的上游地址以 `gateway.baseUrl` / `gateway.protocols[*].baseUrl` 为准。

## 5. Model 字段

| 字段 | 类型 | 含义 | 来源 |
|---|---|---|---|
| `id` | string | 上游模型 ID | v1 |
| `name` | string | 展示名 | v1 |
| `description` | string | 模型描述（v1 **必填**） | v1 |
| `family` | string | 模型家族 slug | v1 |
| `cost` | object | 价格，USD / 百万 token：`input,output,reasoning,cache_read,cache_write,input_audio,output_audio,tiers` | v1 |
| `limit` | object | `context,input,output` | v1 |
| `modalities` | object | `{input:[], output:[]}`，取值 text / audio / image / video / pdf | v1 |
| `tool_call` | bool | 支持工具调用 | v1 |
| `reasoning` | bool | 支持推理 | v1 |
| `reasoning_options` | array | provider 侧推理控制项：`toggle` / `effort` / `budget_tokens` | v1 |
| `interleaved` | `true` \| object | 思维链侧信道；`{ "field": "reasoning_content" \| "reasoning_details" }` | v1 |
| `attachment` | bool | 支持附件 | v1 |
| `structured_output` | bool | 支持结构化输出 | v1 |
| `temperature` | bool | 支持 temperature | v1 |
| `knowledge` | string(date) | 知识截止 | v1 |
| `release_date` / `last_updated` | string(date) | 发布 / 更新日期 | v1 |
| `open_weights` | bool | 是否开源权重 | v1 |
| `status` | string | `alpha` / `beta` / `deprecated` | v1 |
| `type` | string | chat / image / video / embedding / reranking / decision / transcription / speech / realtime | v1 |
| `canonical_model_id` | string | 溯源到 lab 模型 | v1 |
| `provider` | object | 模型级端点覆盖：`{npm?, api?, shape?: "responses"\|"completions", body?, headers?}` | v1 |
| `experimental` | object | `{ modes: { <name>: { cost?, provider?: { body, headers } } } }` | v1 |
| `gateway` | object | 模型级扩展覆盖（可选，见第 6.4 节） | 扩展 |

说明：
- **本格式不消费** `provider` 与 `experimental`（模型级端点/请求体覆盖由本格式的 `gateway` 表达），但**原样保留**以保证可无损读入 `api.json`。
- `cost.context_over_200k`：v1 生成器的 **legacy 输出**（针对单档 ≥200k 的 tier）。消费方应**忽略 / 容忍**，计价统一走 `tiers`。
- `type` / `modalities` 在 v1 为**封闭枚举**，本格式按开放枚举消费（见第 2 节第 4 条）。

## 6. `gateway` 扩展块

### 6.1 Provider 级 `gateway`（作为该 provider 下所有模型的默认值）

| 字段 | 类型 | 含义 |
|---|---|---|
| `baseUrl` | string | 默认上游 base URL |
| `protocols` | object | 该 provider **原生支持的上游基础协议集合**；键即协议名（`openai-chat` / `openai-responses` / `anthropic-messages`），键存在 = 支持。见下方说明。 |
| `requestHeaders` | object | 特殊制式请求头，见 6.3 |

`protocols` 每一项可带：

| 子字段 | 类型 | 含义 |
|---|---|---|
| `baseUrl` | string | 该协议专属 base URL（缺省回退到 provider 级 `baseUrl`） |
| `path` | string | 该协议端点路径（缺省用基础协议内置默认路径，如 `/chat/completions`、`/responses`、`/v1/messages`） |
| `default` | bool | 标记首选上游协议；同一作用域内**至多一个**。用途：二期跨协议转换的目标协议、网关自身内部调用（取第一个协议）。 |

```jsonc
"protocols": {
  "openai-chat":        { "path": "/chat/completions" },
  "openai-responses":   { "path": "/responses" },
  "anthropic-messages": { "baseUrl": "https://api.anthropic.com", "path": "/v1/messages", "default": true }
}
```

网关按**客户端入站协议**与该集合求交：命中即直通（零转换）；未命中则一期返回固定错误、二期再按 `default` 目标做转换。

### 6.2 鉴权（由基础协议隐含，无需配置）

鉴权**不**在预设包中配置：各基础协议隐含各自的凭据注入方式，凭据来自渠道密钥（`channel_keys`）。

| 基础协议 | 注入方式 |
|---|---|
| `openai-chat` / `openai-responses` | `Authorization: Bearer <凭据>` |
| `anthropic-messages` | `x-api-key: <凭据>`（原样） |

若需协议以外的鉴权方式，用 `requestHeaders.set` 显式写入（`set` 在协议隐含注入**之后**应用，会覆盖它）。

### 6.3 `requestHeaders`

```jsonc
"requestHeaders": {
  "passthroughMode": "none",                       // none | whitelist | blacklist | all（默认 none）
  "whitelist": ["x-opencode-session"],
  "blacklist": [],
  "set": { "anthropic-version": "2023-06-01", "x-session-affinity": "{session_id}" }
}
```

- 变量见第 8 节。
- **鉴权由基础协议隐含注入**（见 6.2）；`set` 在其之后应用，因此可显式覆盖鉴权头（用于协议以外的方式）。
- 合并类头（如 `anthropic-beta`：客户端值 + 平台追加值需合并）由**基础协议适配器**处理，不靠 `set` 表达（`set` 是覆盖语义）。

**内置强制黑名单（任何模式下都不透传）**：

| 类别 | 头 |
|---|---|
| hop-by-hop（RFC 7230 §6.1） | `Connection` `Keep-Alive` `TE` `Trailer` `Transfer-Encoding` `Upgrade` `Proxy-Connection` `Proxy-Authorization` |
| 由 URL / body 重新生成（下游值必须丢弃） | `Host`、HTTP/2 伪头 `:authority` `:method` `:scheme` `:path`、`Content-Length`、`Content-Type`、`Content-Encoding`、`Accept-Encoding` |
| 安全 / 链路可信 | `Cookie` `Expect` `Via` `Forwarded` `X-Forwarded-For` `X-Forwarded-Proto` `X-Forwarded-Host` `X-Real-IP` `X-Current-Password` `anthropic-dangerous-direct-browser-access` |
| 凭据（由基础协议隐含注入；透传恒禁止，`set` 可显式覆盖） | `Authorization` `x-api-key` `Proxy-Authorization` |

> `Accept-Encoding` 必须挡：Go `net/http` Transport 会自动补 `gzip` 并透明解压，透传下游的 `br` / `zstd` 会与之冲突，导致 SSE 字节与 token 口径不一致。

**建议透传（白名单候选）**：`anthropic-version` `anthropic-beta` `OpenAI-Organization` `OpenAI-Project` `OpenAI-Beta` `traceparent` `tracestate` `x-request-id` `x-session-id` `x-client-request-id` `x-opencode-session`

### 6.4 模型级 `gateway`（可选，覆盖 provider 级）

支持 `baseUrl`、`protocols`、`requestHeaders` 三项覆盖。

模型级 `protocols` 与 provider 级**同形状**，但语义为**可达协议子集**：只有列出的键才是该模型可用的上游协议，值可再覆盖 `baseUrl` / `path` / `default`；未指定时继承 provider 级 `protocols`。

```jsonc
"gateway": { "protocols": { "anthropic-messages": {} } }
```

上例表示该模型**只经 `anthropic-messages`** 访问（典型场景：同一供应商下不同模型被强制走不同端点）。

## 7. 继承与覆盖

```
基础协议内置默认  ←  包 / 渠道协议  ←  渠道  ←  渠道-模型
```

包里的 provider 条目被物化为一个**渠道协议**；渠道引用渠道协议，可覆盖 `baseUrl`、每个模型的 `protocols`、成本；渠道-模型可覆盖 `protocols`（未指定则取渠道协议中该模型的定义）。

## 8. 变量（用于 `requestHeaders.set`）

| 变量 | 含义 |
|---|---|
| `{api_key}` | 渠道凭据 |
| `{session_id}` | 网关会话 ID |
| `{request_id}` | 网关请求 ID |
| `{model}` | 对外模型名 |
| `{client_header:NAME}` | 下游原始请求头 |

## 9. 单位与枚举约定

- 价格：**USD / 百万 token**（与 models.dev 一致）。若从 litellm（USD / token）导入，必须 ×1e6 换算。
- 上下文 / 输出上限：整数 token。
- 日期：`YYYY-MM` **或** `YYYY-MM-DD`（models.dev v1 两种都产出，消费方须容忍）。
- 枚举（`modalities` / `type` / `protocols` 的键）在本格式内**开放**：消费方遇未知值透传 / 忽略，不报错。注意 v1 原文的 `modalities` / `type` 实为**封闭枚举**，只有 v2 才开放。
- **版本对齐**：本格式只对齐 models.dev **v1**（`/api.json`）；**不消费** v2（`/experimental/v2.0/`）。若未来 v2 转正，再单独评估迁移。

## 10. 响应头（不在本格式内）

响应头由**入站基础协议硬编码**决定（`Content-Type`、SSE 形状、网关固定运维头如 `X-Request-Id` / `Retry-After`），另加一份**全局上游头白名单**（默认空、在系统菜单配置）。上游头默认不透传（维持渠道透明）。因此格式中**不设**响应头字段。

## 11. 完整示例

```jsonc
{
  "id": "opencode-zen",
  "name": "OpenCode Zen",
  "env": ["OPENCODE_API_KEY"],
  "npm": "@ai-sdk/opencode",     // 示意值；本项目不消费 npm，仅为对齐 v1 保留
  "api": null,
  "doc": "https://opencode.ai/docs/zen",
  "models": {
    "claude-sonnet-4-5": {
      "id": "claude-sonnet-4-5",
      "name": "Claude Sonnet 4.5",
      "description": "Anthropic Claude Sonnet 4.5 via OpenCode Zen.",
      "cost": { "input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75 },
      "limit": { "context": 200000, "output": 64000 },
      "modalities": { "input": ["text", "image"], "output": ["text"] },
      "tool_call": true,
      "reasoning": true,
      "reasoning_options": ["toggle", "budget_tokens"],
      "gateway": { "protocols": { "anthropic-messages": {} } }
    },
    "gpt-5-codex": {
      "id": "gpt-5-codex",
      "name": "GPT-5 Codex",
      "description": "OpenAI GPT-5 Codex via OpenCode Zen.",
      "cost": { "input": 1.25, "output": 10 },
      "limit": { "context": 400000, "output": 128000 },
      "tool_call": true,
      "reasoning": true,
      "gateway": { "protocols": { "openai-responses": {} } }
    }
  },
  "gateway": {
    "baseUrl": "https://opencode.ai/zen/v1",
    "protocols": {
      "anthropic-messages": { "path": "/messages" },
      "openai-responses":   { "path": "/responses" }
    },
    "requestHeaders": {
      "passthroughMode": "whitelist",
      "whitelist": ["x-opencode-session"]
    }
  }
}
```

## 12. 许可与归属

若直接消费 / 内嵌 models.dev 数据，需保留其 MIT 许可与版权声明（models.dev, MIT, Copyright (c) 2025）。
