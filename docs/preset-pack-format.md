# 渠道预设包格式约定（v0.1 草案）

## 1. 目的

本文件约定「渠道预设包」（Preset Pack）的数据格式。预设包独立成一个仓库维护，本网关只负责：定义格式、加载、把包里的 provider 条目物化为「渠道协议」。

核心目标：**尽量对齐 models.dev 的格式**，只在其上做最小扩展，从而复用其模型能力与价格数据，把人工维护成本降到最低。

## 2. 设计原则

1. **对齐 models.dev**：provider/model 的字段名、两级嵌套形状、单位一律沿用 models.dev，不另造同义字段。
2. **扩展隔离**：本网关专有字段统一放进 `gateway` 命名空间，避免与 models.dev 现有/未来字段冲突（models.dev schema 是 strict，未知键会校验失败）。
3. **可覆盖**：包内值是「默认值」，渠道协议 / 渠道可逐层覆盖。
4. **开放枚举**：`modalities`、`type`、`baseProtocol` 等枚举按需扩展，消费方遇到未知值应透传 / 忽略而非报错。

## 3. 仓库布局（建议）

```
preset-pack/
  manifest.json                  # 包元信息（schemaVersion、包名、版本）
  providers/
    <provider-id>.json           # 每个 provider 一个文件（便于 diff / 审计）
```

- `<provider-id>.json` 的**内容形状**与 models.dev `api.json` 中对应 provider 的条目一致，外加 `gateway` 扩展块。
- 文件亦可用 TOML（与 models.dev 源格式一致），形状不变；推荐 JSON 便于工具链消费。

## 4. 顶层结构

单个 provider 文件：

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

多 provider 合并后的形状 = `{ "<providerId>": { ...上例... } }`（与 models.dev `api.json` 一致）。

## 5. Provider 字段

| 字段 | 类型 | 含义 | 来源 |
|---|---|---|---|
| `id` | string | provider 唯一 ID（与文件名同名） | models.dev |
| `name` | string | 展示名 | models.dev |
| `env` | string[] | 该 provider 需要的 API Key 环境变量名 | models.dev |
| `npm` | string \| null | 官方 AI SDK 包（隐式提示鉴权 / 端点实现） | models.dev |
| `api` | string \| null | OpenAI 兼容 base URL（models.dev 仅对兼容类填充；支持 `${ENV}` 模板） | models.dev |
| `doc` | string | 文档链接 | models.dev |
| `models` | map | 模型表（见第 6 节） | models.dev |
| `gateway` | object | **本网关扩展块**（见第 7 节） | 扩展 |

## 6. Model 字段

| 字段 | 类型 | 含义 | 来源 |
|---|---|---|---|
| `id` | string | 上游模型 ID | models.dev |
| `name` | string | 展示名 | models.dev |
| `cost` | object | 价格，USD / 百万 token：`input,output,reasoning,cache_read,cache_write,input_audio,output_audio,tiers` | models.dev |
| `limit` | object | `context,input,output` | models.dev |
| `modalities` | object | `{input:[], output:[]}`，取值 text/audio/image/video/pdf（开放枚举） | models.dev |
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
| `gateway` | object | 模型级扩展覆盖（可选，见第 7 节） | 扩展 |

## 7. `gateway` 扩展块

### 7.1 Provider 级 `gateway`

作为该 provider 下所有模型的默认值。

| 字段 | 类型 | 含义 | 必填 |
|---|---|---|---|
| `baseProtocol` | string | 默认基础协议：`openai-chat` / `openai-responses` / `anthropic-messages` | 否（可由模型级指定） |
| `baseUrl` | string | 默认上游 base URL（支持 `${ENV}` 模板） | 否 |
| `auth` | object | 凭据注入方式，见 7.2 | 否 |
| `endpoints` | object | 各中继格式的路径覆盖：`chat` / `responses` / `messages` | 否 |
| `requestHeaders` | object | 特殊制式请求头，见 7.3 | 否 |

### 7.2 `auth`

```jsonc
"auth": {
  "header": "Authorization",                       // Authorization | x-api-key | api-key
  "scheme": "bearer",                              // bearer | raw（原样注入）
  "query": { "api-version": "2025-04-01-preview" } // 需放 querystring 的鉴权 / 版本参数（如 Azure）
}
```

### 7.3 `requestHeaders`

```jsonc
"requestHeaders": {
  "passthroughMode": "none",                       // none | whitelist | blacklist | all（默认 none）
  "whitelist": ["x-opencode-session"],
  "blacklist": [],
  "set": { "anthropic-version": "2023-06-01", "x-session-affinity": "{session_id}" }
}
```

- 变量见第 9 节。
- **内置强制黑名单**（任何模式下都不透传）：凭证类、hop-by-hop、`cookie/host/content-length/accept-encoding`、WS 握手头。
- **鉴权头由 `auth` 注入**，不进入用户可编辑的 `set`（防止误配把渠道凭据冲掉）。
- 合并类头（如 `anthropic-beta`：客户端值 + 平台追加值需合并）由**基础协议适配器**处理，不靠 `set` 表达（`set` 是覆盖语义）。

### 7.4 模型级 `gateway`（可选，覆盖 provider 级）

支持 `baseProtocol`、`baseUrl`、`requestHeaders` 三项覆盖。

## 8. 继承与覆盖

```
基础协议内置默认  ←  包 / 渠道协议  ←  渠道  ←  渠道-模型
```

- 包里的 provider 条目会被物化为一个**渠道协议**；
- 渠道引用渠道协议，可覆盖 `baseUrl`、每个模型的基础协议、成本；
- 渠道-模型可覆盖基础协议（未选则取渠道协议中该模型的定义）。

## 9. 变量

请求头 `set` 可用变量（网关在向上游发起请求前解析）：

| 变量 | 含义 |
|---|---|
| `{api_key}` | 渠道凭据 |
| `{session_id}` | 网关会话 ID |
| `{request_id}` | 网关请求 ID |
| `{model}` | 对外模型名 |
| `{client_header:NAME}` | 下游原始请求头 |

## 10. 单位与枚举约定

- 价格：**USD / 百万 token**（与 models.dev 一致）。若从 litellm（USD / token）导入，必须 ×1e6 换算。
- 上下文 / 输出上限：整数 token。
- 日期：`YYYY-MM-DD`。
- 枚举（`modalities` / `type` / `baseProtocol`）**开放**：消费方遇未知值透传 / 忽略，不报错。

## 11. 版本与兼容

- `manifest.json` 必含 `schemaVersion`（如 `0.1`）。
- 兼容 models.dev 两代：v1（扁平能力布尔 + `cost`）与 v2（实验，`capabilities` + `api[protocol].base_url` + `compatibility`）。消费方按特征探测（存在 `capabilities` 即 v2）。
- 本网关扩展字段只存在于 `gateway` 块，不随 models.dev 版本变化。

## 12. 响应头（不在包内）

响应头由**入站基础协议硬编码**决定（`Content-Type`、SSE 形状、网关固定运维头如 `X-Request-Id` / `Retry-After`），另加一份**全局上游头白名单**（默认空、零配置）。因此包内**不设**响应头字段；上游头默认不透传（维持渠道透明）。

## 13. 校验与加载

- 加载时机：网关启动读取本地包目录；支持管理员导入 / URL 同步。
- 校验：`id` 唯一、`baseProtocol` 合法、`requestHeaders` 头名合法且不撞强制黑名单、变量语法合法。
- 可选：用 models.dev 的 schema 校验 JSON 部分（`gateway` 块跳过）。

## 14. 完整示例

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
      "gateway": { "baseProtocol": "anthropic-messages" }
    },
    "gpt-5-codex": {
      "id": "gpt-5-codex",
      "cost": { "input": 1.25, "output": 10 },
      "limit": { "context": 400000, "output": 128000 },
      "tool_call": true,
      "reasoning": true,
      "gateway": { "baseProtocol": "openai-responses" }
    }
  },
  "gateway": {
    "baseUrl": "https://opencode.ai/zen/v1",
    "auth": { "header": "Authorization", "scheme": "bearer" },
    "requestHeaders": {
      "passthroughMode": "whitelist",
      "whitelist": ["x-opencode-session"]
    }
  }
}
```

## 15. 许可与归属

若直接消费 / 内嵌 models.dev 数据，需保留其 MIT 许可与版权声明（models.dev, MIT, Copyright (c) 2025）。建议在包仓库 `NOTICE` / `README` 中注明数据来源与版本。
