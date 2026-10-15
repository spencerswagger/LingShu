# 渠道预设包 JSON 格式约定（v0.1 草案）

## 1. 目的

约定「渠道预设包」的 JSON 格式与字段，用于描述一个供应商的接入方式（base URL、鉴权、特殊请求头）及其模型清单（能力、价格、对应基础协议）。格式**尽量对齐 models.dev**，只做最小扩展，从而复用其模型能力与价格数据。

## 2. 设计原则

1. **对齐 models.dev**：provider / model 的字段名、两级嵌套形状、单位一律沿用 models.dev，不另造同义字段。
2. **扩展隔离**：本网关专有字段统一放进 `gateway` 命名空间，避免与 models.dev 现有 / 未来字段冲突（models.dev schema 是 strict，未知键会校验失败）。
3. **可覆盖**：包内值是「默认值」，渠道协议 / 渠道可逐层覆盖。
4. **开放枚举**：`modalities`、`type`、`protocols` 的键等枚举按需扩展，消费方遇未知值应透传 / 忽略而非报错。

## 3. 顶层结构

一个 provider 的 JSON 对象（与 models.dev `api.json` 中单个 provider 条目的形状一致，外加 `gateway` 块）：

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

多 provider 合并后形状 = `{ "<providerId>": { ...上例... } }`（与 models.dev `api.json` 一致）。

## 4. Provider 字段

| 字段 | 类型 | 含义 | 来源 |
|---|---|---|---|
| `id` | string | provider 唯一 ID | models.dev |
| `name` | string | 展示名 | models.dev |
| `env` | string[] | 该 provider 需要的 API Key 环境变量名 | models.dev |
| `npm` | string \| null | 官方 AI SDK 包（隐式提示鉴权 / 端点实现） | models.dev |
| `api` | string \| null | OpenAI 兼容 base URL（models.dev 仅对兼容类填充；支持 `${ENV}` 模板） | models.dev |
| `doc` | string | 文档链接 | models.dev |
| `models` | map | 模型表（见第 5 节） | models.dev |
| `gateway` | object | **本网关扩展块**（见第 6 节） | 扩展 |

## 5. Model 字段

| 字段 | 类型 | 含义 | 来源 |
|---|---|---|---|
| `id` | string | 上游模型 ID | models.dev |
| `name` | string | 展示名 | models.dev |
| `cost` | object | 价格，USD / 百万 token：`input,output,reasoning,cache_read,cache_write,input_audio,output_audio,tiers` | models.dev |
| `limit` | object | `context,input,output` | models.dev |
| `modalities` | object | `{input:[], output:[]}`，取值 text / audio / image / video / pdf（开放枚举） | models.dev |
| `tool_call` | bool | 支持工具调用 | models.dev |
| `reasoning` | bool | 支持推理 | models.dev |
| `attachment` | bool | 支持附件 | models.dev |
| `structured_output` | bool | 支持结构化输出 | models.dev |
| `temperature` | bool | 支持 temperature | models.dev |
| `knowledge` | string(date) | 知识截止 | models.dev |
| `release_date` / `last_updated` | string(date) | 发布 / 更新日期 | models.dev |
| `open_weights` | bool | 是否开源权重 | models.dev |
| `type` | string | chat / image / video / embedding / ...（开放枚举） | models.dev |
| `canonical_model_id` | string | 溯源到 lab 模型 | models.dev |
| `gateway` | object | 模型级扩展覆盖（可选，见第 6.4 节） | 扩展 |

## 6. `gateway` 扩展块

### 6.1 Provider 级 `gateway`（作为该 provider 下所有模型的默认值）

| 字段 | 类型 | 含义 |
|---|---|---|
| `baseUrl` | string | 默认上游 base URL（支持 `${ENV}` 模板） |
| `protocols` | object | 该 provider **原生支持的上游基础协议集合**；键即协议名（`openai-chat` / `openai-responses` / `anthropic-messages`），键存在 = 支持。见下方说明。 |
| `auth` | object | 凭据注入方式，见 6.2 |
| `requestHeaders` | object | 特殊制式请求头，见 6.3 |

`protocols` 每一项可带：

| 子字段 | 类型 | 含义 |
|---|---|---|
| `baseUrl` | string | 该协议专属 base URL（缺省回退到 provider 级 `baseUrl`） |
| `path` | string | 该协议端点路径（缺省用基础协议内置默认路径，如 `/chat/completions`、`/responses`、`/v1/messages`） |
| `default` | bool | 标记首选上游协议；同一作用域内**至多一个**。用途：二期跨协议转换的目标协议、网关自身发起调用时的协议。 |

```jsonc
"protocols": {
  "openai-chat":        { "path": "/chat/completions" },
  "openai-responses":   { "path": "/responses" },
  "anthropic-messages": { "baseUrl": "https://api.anthropic.com", "path": "/v1/messages", "default": true }
}
```

网关按**客户端入站协议**与该集合求交：命中即直通（零转换）；未命中则一期返回固定错误、二期再按 `default` 目标做转换。

### 6.2 `auth`

```jsonc
"auth": {
  "header": "Authorization",                       // Authorization | x-api-key | api-key
  "scheme": "bearer",                              // bearer | raw（原样注入）
  "query": { "api-version": "2025-04-01-preview" } // 需放 querystring 的鉴权 / 版本参数（如 Azure）
}
```

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
- **内置强制黑名单**（任何模式下都不透传）：凭证类、hop-by-hop、`cookie/host/content-length/accept-encoding`、WS 握手头。
- **鉴权头由 `auth` 注入**，不进入用户可编辑的 `set`（防止误配把渠道凭据冲掉）。
- 合并类头（如 `anthropic-beta`：客户端值 + 平台追加值需合并）由**基础协议适配器**处理，不靠 `set` 表达（`set` 是覆盖语义）。

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
- 日期：`YYYY-MM-DD`。
- 枚举（`modalities` / `type` / `protocols` 的键）**开放**：消费方遇未知值透传 / 忽略，不报错。
- models.dev 两代字段形状：v1 用扁平能力布尔 + `cost`；v2（实验）用 `capabilities` + `api[protocol].base_url` + `compatibility`。消费方可按特征探测（存在 `capabilities` 即 v2）。

## 10. 响应头（不在本格式内）

响应头由**入站基础协议硬编码**决定（`Content-Type`、SSE 形状、网关固定运维头如 `X-Request-Id` / `Retry-After`），另加一份**全局上游头白名单**（默认空、零配置）。上游头默认不透传（维持渠道透明）。因此格式中**不设**响应头字段。

## 11. 完整示例

```jsonc
{
  "id": "opencode-zen",
  "name": "OpenCode Zen",
  "env": ["OPENCODE_API_KEY"],
  "npm": null,
  "api": null,
  "doc": "https://opencode.ai/docs/zen",
  "models": {
    "claude-sonnet-4-5": {
      "id": "claude-sonnet-4-5",
      "name": "Claude Sonnet 4.5",
      "cost": { "input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75 },
      "limit": { "context": 200000, "output": 64000 },
      "modalities": { "input": ["text", "image"], "output": ["text"] },
      "tool_call": true,
      "reasoning": true,
      "gateway": { "protocols": { "anthropic-messages": {} } }
    },
    "gpt-5-codex": {
      "id": "gpt-5-codex",
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
    "auth": { "header": "Authorization", "scheme": "bearer" },
    "requestHeaders": {
      "passthroughMode": "whitelist",
      "whitelist": ["x-opencode-session"]
    }
  }
}
```

## 12. 许可与归属

若直接消费 / 内嵌 models.dev 数据，需保留其 MIT 许可与版权声明（models.dev, MIT, Copyright (c) 2025）。
