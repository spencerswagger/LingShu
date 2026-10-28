# 渠道预设包 JSON 格式约定（v0.2 草案）

- **状态**：草案。待兰台（LanTai）冻结为 JSON Schema 并发布成数据集 URL。
- **修订**：2026-10-28（变更记录见第 14 节）
- **正本**：本文件。机器可读参考实现：`docs/preset-pack-format.schema.json`
  （兰台冻结版以兰台产出为准；**字段口径冲突时以本文件为准**）
- **对齐**：models.dev v1（`https://models.dev/api.json`），只做最小扩展

> 本文只对齐 models.dev **v1**，不消费 v2（`/experimental/v2.0/`，仍为实验态）。

## 0. 一页速览（兰台先看这五条）

1. 顶层是**多 provider 合并对象** `{ "<providerId>": {...} }`，不是数组。
2. 一个 provider 条目 = models.dev v1 形状 + 扩展块 `gateway`。**所有本项目自有字段只能放 `gateway` 内**。
3. **价格一律人民币**：`models.<m>.gateway.costRates`（三段，元/百万 token 倍率）是唯一计费载体；`cost` 是展示位。数据集必须写 `gateway.costCurrency: "CNY"`。
4. 灵枢**忽略一切未知字段**（不会因多字段报错），但**会因为 `passthroughMode` 非法 / provider 名重复 / baseUrl 非 http(s) 而整包同步失败**（第 11 节）。
5. 数据集 URL 只挂 `preset.pack_url`；**绝不能**挂到 `sync.price_source_url`（第 9.4 节）：若数据集同时带 v1 的 `cost`，会被当美元 ×6.8 写进售价；只带 `costRates` 则参考价这条通道静默无效（一条都拉不到，不报错）。

## 1. 目的

约定「渠道预设包」（= 数据集）的 JSON 格式与字段，用于描述一个供应商的接入方式（base URL、特殊请求头、原生协议）及其模型清单（能力、**人民币价格**、对应基础协议、商业优惠）。格式对齐 models.dev v1，只做最小扩展，从而复用其模型能力数据。

## 2. 设计原则

1. **对齐 models.dev v1**：provider / model 的字段名、两级嵌套形状、单位一律沿用 v1，不另造同义字段；v1 字段即使灵枢不消费，也**原样保留**以保证可无损读入 `api.json`。
2. **扩展隔离**：本项目自有字段统一放进 `gateway` 命名空间，避免与 models.dev 现有 / 未来字段冲突。
3. **可覆盖**：包内值是「默认值」，渠道协议 / 渠道 / 渠道-模型可逐层覆盖。
4. **开放枚举**：`modalities`、`type`、`gateway.protocols` 的键在本格式内按开放枚举处理，消费方遇未知值应**透传 / 忽略而非报错**。注意 v1 原文的 `modalities` / `type` 是封闭枚举，开放对待是本项目消费策略，非 v1 语义。
5. **币种单一（v0.2 新增）**：本格式的价格**只有人民币一种口径**，不含美元、不涉及汇率。`gateway.costCurrency` 是这一点的显式声明位。
6. **未知字段必容忍（v0.2 新增）**：消费方对任何不认识的键一律忽略；新增字段**不得**要求消费方改代码才能读取旧数据集。反向也成立：兰台可在 `gateway` 内先行摆字段位（留空），不会影响灵枢。

## 3. 顶层结构

顶层为多 provider 合并对象，键 = provider ID（与条目内 `id` 一致）：

```jsonc
{
  "anthropic": { "id": "anthropic", "name": "Anthropic", "models": { "...": {} }, "gateway": { "...": {} } },
  "zhipu":     { "id": "zhipu",     "name": "智谱",       "models": { "...": {} }, "gateway": { "...": {} } }
}
```

- 顶层**只允许**「providerId → provider 条目」这一层映射；条目里不允许出现 provider 之外的键。
- provider ID 必须**全局唯一**，且同时是灵枢物化后的 `preset_source_id`（用于同步认领、失效标记）。

## 4. Provider 字段

| 字段 | 类型 | 必填性 | 灵枢消费 | 含义 |
|---|---|---|---|---|
| `id` | string | **格式必填**（空则整包同步失败） | 是（`preset_source_id`） | provider 唯一 ID |
| `name` | string | 可选（空则回退 `id`） | 是（渠道协议名） | 展示名，**全局唯一**（重名整包失败） |
| `models` | object | 业务必填（缺省 `{}` 不报错，但协议无模型） | 是 | 模型表（第 5 节） |
| `gateway` | object | 业务必填（缺省不报错，但无上游地址） | 是 | 本项目扩展块（第 6 节） |
| `env` | string[] | 可选 | **否（惰性）** | 该 provider 需要的 API Key 环境变量名 |
| `npm` | string | 可选（v1 要求非空，本格式放宽） | **否（惰性）** | 官方 AI SDK 包名 |
| `api` | string \| null | 可选 | **否（惰性）** | OpenAI 兼容 base URL（v1 无 `${ENV}` 模板；模板是 v2 特性） |
| `doc` | string | 可选 | **否（惰性）** | 文档链接 |

说明：
- 上游地址以 `gateway.baseUrl` / `gateway.protocols[*].baseUrl` 为准；`api` 仅供展示与 v1 对齐。
- `env` 语义为「该供应商读取 API Key 的环境变量名」，可作前端提示，不参与运行时。

## 5. Model 字段

模型表键 = 上游模型 ID（与条目内 `id` 一致）。

| 字段 | 类型 | 必填性 | 灵枢消费 | 含义 |
|---|---|---|---|---|
| `id` | string | **格式必填** | 是（模型键） | 上游模型 ID |
| `name` | string | 格式必填（v1 对齐） | **否（惰性）** | 展示名 |
| `description` | string | 格式必填（v1 对齐） | **否（惰性）** | 描述 |
| `cost` | object | 可选 | **否（惰性，见 9.2）** | 价格：`input/output/reasoning/cache_read/cache_write/input_audio/output_audio/tiers`，人民币/百万 token |
| `limit` | object | 可选 | **否（惰性）** | `context` / `input` / `output`（token） |
| `modalities` | object | 可选 | **否（惰性）** | `{input:[], output:[]}`，取值 text/audio/image/video/pdf（开放枚举） |
| `tool_call` | bool | 可选 | **否（惰性）** | 支持工具调用 |
| `reasoning` | bool | 可选 | **否（惰性）** | 支持推理 |
| `reasoning_options` | array | 可选 | **否（惰性）** | `toggle` / `effort` / `budget_tokens` |
| `interleaved` | `true` \| object | 可选 | **否（惰性）** | 思维链侧信道：`{ "field": "reasoning_content" \| "reasoning_details" }` |
| `attachment` / `structured_output` / `temperature` / `open_weights` | bool | 可选 | **否（惰性）** | v1 能力位 |
| `knowledge` / `release_date` / `last_updated` | string(date) | 可选 | **否（惰性）** | `YYYY-MM` 或 `YYYY-MM-DD`（两种都产出，须容忍） |
| `family` | string | 可选 | **否（惰性）** | 模型家族 slug |
| `status` | string | 可选 | **否（惰性）** | `alpha` / `beta` / `deprecated` |
| `type` | string | 可选 | **否（惰性）** | chat/image/video/embedding/reranking/decision/transcription/speech/realtime（开放枚举） |
| `canonical_model_id` | string | 可选 | **否（惰性）** | 溯源到 lab 模型 |
| `provider` | object | 可选 | **否（惰性）** | v1 模型级端点覆盖（npm/api/shape/body/headers），由本格式 `gateway` 表达 |
| `experimental` | object | 可选 | **否（惰性）** | `{ modes: { <name>: { cost?, provider? } } }` |
| `gateway` | object | 业务必填（见 6.4） | 是 | 模型级扩展覆盖 |

说明：
- **本格式不消费** `provider` 与 `experimental`，但原样保留以保证可无损读入 `api.json`。
- `cost.context_over_200k` 是 v1 生成器的 legacy 输出（单档 ≥200k 的 tier），消费方应忽略；计价统一走 `tiers`。
- ⚠ **模型条目保活规则（重要）**：灵枢物化时，若某模型条目既无 `gateway.protocols`、又无 `gateway.baseUrl`、又无 `gateway.externalModelID > 0`，该条目会被**整条静默丢弃**（含它的价格）。因此**带价格的模型必须至少声明一个协议**。

## 6. `gateway` 扩展块

### 6.1 Provider 级 `gateway`（作为该 provider 下所有模型的默认值）

| 字段 | 类型 | 必填性 | 含义 |
|---|---|---|---|
| `baseUrl` | string | 业务必填 | 默认上游 base URL。**必须 `http://` 或 `https://` 开头，否则整包同步失败** |
| `protocols` | object | 业务必填 | 该 provider 原生支持的上游基础协议集合；键即协议名，键存在 = 支持 |
| `requestHeaders` | object | 可选 | 请求头策略（6.3） |
| `costCurrency` | string | **数据集必填**（6.5） | 价格币种声明，本数据集恒为 `"CNY"` |
| `offers` | array | 可选（v0.1.0 留空 `[]`） | 优惠方式（6.6） |
| `edited` / `sourceStatus` | bool / string | **数据集不要填** | 仅供灵枢 export/import 承载编辑标记与来源状态；sync 路径强制写 `edited=false`、`sourceStatus=ACTIVE` |

`protocols` 每一项可带：

| 子字段 | 类型 | 含义 |
|---|---|---|
| `baseUrl` | string | 该协议专属 base URL（缺省回退 provider 级 `baseUrl`） |
| `path` | string | 该协议端点路径（缺省用基础协议内置默认路径：`/chat/completions`、`/responses`、`/v1/messages`） |
| `default` | bool | 标记首选上游协议；**同一作用域内至多一个**，多于一个则整包同步失败 |

**基础协议仅三个，代码固定、不可由数据集新增**：`openai-chat` / `openai-responses` / `anthropic-messages`。

```jsonc
"protocols": {
  "openai-chat":        { "path": "/chat/completions" },
  "openai-responses":   { "path": "/responses" },
  "anthropic-messages": { "baseUrl": "https://api.anthropic.com", "path": "/v1/messages", "default": true }
}
```

网关按**客户端入站协议**与该集合求交：命中即直通（零转换）；未命中则一期返回固定错误、二期按 `default` 目标做转换。

**未知协议键的容忍方式**：物化时白名单过滤（只保留上述三个基础协议的键），未知键**静默丢弃、不报错**。历史值 `openai-compat` 归一为 `openai-chat`。

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

- `passthroughMode` 是**封闭枚举**（大小写不敏感），非法值 → 400「passthroughMode 只支持 none / whitelist / blacklist / all」，**整包同步回滚**。
- 变量见第 8 节。
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

| 字段 | 类型 | 灵枢消费 | 含义 |
|---|---|---|---|
| `protocols` | object | 是 | **可达协议子集**：只有列出的键是该模型可用的上游协议；未指定时继承 provider 级 |
| `baseUrl` | string | 是 | 模型专属上游地址 |
| `costRates` | object | **是（唯一计费载体）** | 三段成本倍率，人民币元/百万 token（见 9.1） |
| `externalModelID` | string(numeric) | 是 | 绑定的对外模型 ID（雪花 ID，字符串化）。**>0 才参与物化为 `channel_models`** |
| `timeConfig` | object | 是 | 时段系数（高峰/闲时） |
| `contextTiers` | array | 是 | 上下文分档规则 |
| `convertEnabled` | bool | 是 | 是否允许跨协议转换（缺省继承全局开关） |
| `convertTargets` | string[] | 是 | 转换目标协议偏好顺序（空 = 默认偏好） |
| `costCurrency` | string | 否（惰性，6.5） | 模型级币种覆盖（一般不必填，继承 provider） |
| `offers` | array | 否（惰性，6.6） | 模型专属优惠 |

```jsonc
"gateway": { "protocols": { "anthropic-messages": {} } }
```

上例表示该模型**只经 `anthropic-messages`** 访问（典型场景：同一供应商下不同模型被强制走不同端点）。

- 键名一律 camelCase（`costRates` 内部三段键用 PascalCase `Input/Output/CacheRead`，见 9.1）。
- ⚠ **模型级不支持 `requestHeaders`**：`ModelGateway` 结构体没有该字段（旧版本文档曾写"支持三项覆盖"，与代码不符，v0.2 更正）。数据集在模型级写 `requestHeaders` **不会报错，但会被静默忽略**。请求头策略只能配在 provider 级。
- ⚠ `externalModelID` 是**灵枢内部 ID**，兰台无法得知；数据集里**不要**填它（见 12.3）。

### 6.5 `costCurrency`（币种声明，v0.2 新增）

```jsonc
"gateway": { "costCurrency": "CNY" }
```

- 作用域：声明**同一作用域内** `cost` 与 `gateway.costRates` 的币种。provider 级为默认，模型级可覆盖。
- 取值：`"CNY"` | `"USD"`（开放枚举，消费方遇未知值按 `CNY` 处理）。
- **语义默认值 = `USD`**（不写即按 USD 理解）—— 这是 models.dev v1 的原义，用来保证「直接把 `https://models.dev/api.json` 当包读进来」时不会把美元误读成人民币。这是**文档层约定**，不是代码行为（见下条）。
- **本数据集必须显式写 `"CNY"`**；兰台冻结的 JSON Schema 应把 `costCurrency` 设为 **required + `const: "CNY"`**（格式层宽容两种币种，数据集层收紧到只有人民币）。
- **灵枢当前完全没有币种概念，该字段是纯标注**（代码核实，2026-10-28）：
  - 后端全仓 `currency` / `Currency` 出现 **0 次**；`GatewayBlock`（`channelprotocol/source.go`）与 `ModelGateway`（`channelprotocol/model.go`）都没有该键 → 解码时按未知字段**直接丢弃**。
  - 落库（`channel_models.cost_rates` 原值写入）与计费（`billing.ComputeCredits` = `tokens × rate ÷ R`）**都没有任何币种分支**。
  - ⇒ 写不写、写 `"CNY"` 还是 `"USD"`，**落库数值与出账金额完全相同**。「缺 `costCurrency` → 把美元当人民币 → 虚低 6.8 倍」这条因果链在代码里不成立；真正的 6.8 只存在于 `sync.price_source_url` 那条链路（9.4）。
  - ⇒ 因此：它是**消歧标注，不是计费风险点**，也没有任何执法点（不校验、不报错）。真正的风险是**数值币种填错**（把美元数当人民币填进 `costRates`），见 9.1「数值硬约束」。

### 6.6 `offers`（优惠方式，v0.2 新增字段位）

**结论：兼容，v0.1.0 先摆字段位（`"offers": []`），0.2.0 再填内容。** 判定依据见第 13 节。

放置位置：`gateway.offers`（provider 级 = 供应商通用优惠；模型级 = 该模型专属优惠）。
**合并语义 = 数组拼接**（provider 在前、模型在后），不覆盖、不去重；消费方按 `kind` + `label` 自行判重。理由：优惠是「事实集合」，覆盖会丢信息。

```jsonc
"offers": [
  {
    "kind": "time_limited",                    // 必填，见下方枚举
    "label": "首发五折",                        // 必填，人读短句
    "eligible": "all",                          // 可选：new_user | existing_user | invitee | all（默认 all）
    "window": {                                 // 可选；kind=time_limited / flash_sale 必填 end
      "start": "2026-08-26",
      "end": "2026-09-09T24:00+08:00"           // 带时区；也可只写日期 YYYY-MM-DD
    },
    "effect": {                                 // 必填，下列之一（互斥）
      "priceOverride": { "input": 0.4, "output": 1.4, "cache_read": 0.115 }  // 人民币元/百万 token
      // "discount": { "mode": "percent", "value": 50 }                     // 折让百分比（50 = 五折）
      // "discount": { "mode": "amount",  "value": 5 }                      // 立减金额（元）
      // "quota":    { "unit": "token", "amount": 140000000, "period": "week" }
    },
    "limit": { "quantity": 1000, "remaining": 137 },   // 可选：限量抢购 / 名额上限
    "note": "折扣截止 2026-09-09 24:00（UTC+8），到期回落标价",
    "source": "https://open.bigmodel.cn/pricing",      // 可选，来源 URL
    "asOf": "2026-10-28"                               // 可选，该优惠信息的采信日
  }
]
```

`kind` 枚举与必填搭配：

| `kind` | 中文 | 必填项 | `effect` 允许形态 |
|---|---|---|---|
| `first_purchase` | 首购价 | `eligible: new_user` | `priceOverride` / `discount` |
| `time_limited` | 限时活动 | `window.end` | `priceOverride` / `discount` |
| `renewal` | 续费价 | — | `priceOverride`（分档用 `priceOverrideTiers`） |
| `invite_discount` | 邀请折扣 | `eligible: invitee` | `discount` / `quota` |
| `free_quota` | 免费额度 | — | `quota` |
| `flash_sale` | 限量抢购 | `limit.quantity` + `window.end` | `priceOverride` / `discount` |

`quota.unit` 开放枚举：`token` / `credit` / `afp` / `call`；`quota.period`：`total` / `day` / `week` / `month`。

约束：
- `effect` 内**只允许一种**形态（多给时按 `priceOverride > discount > quota` 优先，其余忽略）。
- `priceOverride` 数值为**人民币元/百万 token**，与 `costRates` 同口径，可被消费方直接替换 `costRates`。
- 未知 `kind` 值：**整条 offers 忽略，不报错**（开放枚举）。
- 时间只表达「有效区间」，**不做自动生效/失效调度**：灵枢不消费该字段，价格切换仍以数据集重新发布为准。

## 7. 继承与覆盖

```
基础协议内置默认  ←  包 / 渠道协议  ←  渠道  ←  渠道-模型
```

包里的 provider 条目被物化为一个**渠道协议**；渠道引用渠道协议，可覆盖 `baseUrl`、每个模型的 `protocols`、成本；渠道-模型可覆盖 `protocols`（未指定则取渠道协议中该模型的定义）。

模型级 `gateway` 覆盖 provider 级同名字段；`offers` 例外（拼接而非覆盖）。

## 8. 变量（用于 `requestHeaders.set`）

| 变量 | 含义 |
|---|---|
| `{api_key}` | 渠道凭据 |
| `{session_id}` | 网关会话 ID |
| `{request_id}` | 网关请求 ID |
| `{model}` | 对外模型名 |
| `{client_header:NAME}` | 下游原始请求头 |

## 9. 定价口径（人民币，v0.2 定稿）

### 9.1 唯一计费载体：`gateway.costRates`

| 项 | 规定 |
|---|---|
| 位置 | `models.<modelKey>.gateway.costRates` |
| 键 | **PascalCase**：`Input` / `Output` / `CacheRead`（snake `input/output/cache_read` 同样接受；兰台统一输出 PascalCase）。**只有这两套键名会被归一化，别的键原样保留、不报错**（见下方「键归一化」） |
| 语义 | **人民币元 / 百万 token 的倍率**（无单位纯数字，照抄官网人民币价） |
| 数值 | **必须是人民币元/百万 token 的字面量**（如 `0.8` / `2.8` / `0.23`）。代码不识别单位、不做任何换算：从 models.dev 照抄美元数进来，就会按人民币出账 → **虚低 6.8 倍**。这是**数值错，不是标注错**，当前无代码执法点 |
| 完整性 | **三段必须齐全**（缺任一 → 不报错，但成本积分静默不记，见 11.2 S2） |
| 取值 | 非负浮点；未知多余键允许（原样透传落库，不参与计费） |
| 汇率 | **不乘任何汇率**，直接落 `channel_models.cost_rates` |

**键归一化（唯一转换点）**：`billing.WireRates.UnmarshalJSON`（`backend/internal/domain/billing/rates.go`）把 `Input` / `Output` / `CacheRead` 归一为内部 `input` / `output` / `cache_read`，snake 键同样接受。预设包解的就是这条路径（`channelprotocol/model.go` 的 `CostRates billing.WireRates`）—— 所以「PascalCase 与 snake 之间没有转换点」是**错的**，按本格式写 PascalCase 不会出问题。
真正的坑是**拼错键**：该函数对未知键「原样保留、不报错」，写 `cache` / `Cache_Read` / `inputTokens` 不会被纠正，只会让 `cache_read` 缺席 → 直接落进 S2。

**为什么「缺段 / 拼错键」在同步阶段完全无感**（代码核实，2026-10-28）：

1. 物化路径 `channel/service.go` 的 `materializeFromProtocol` 是 `CostRates: billing.Rates(mg.CostRates)` 直接落库，**从不调用 `billing.ValidateRates`**；`ValidateRates` 只在管理端 API 路径（`channel/model_service.go` 校验成本、`model/service.go` 校验售价）被调用。所以错键会一路进 `channel_models.cost_rates`。
2. 计费时 `ComputeCredits` 会因缺键报错，但**成本侧错误被吞**：`billing/service.go` 里 `if c, cerr := ComputeCredits(...); cerr == nil { rec.CostCredits = c }` —— 既不记日志也不告警，`cost_credits` **静默为 0**，成本/毛利曲线整段为空且没人会发现。
3. 售价侧缺键不静默：结算直接报错。
4. 唯一会「响」的是管理员在渠道模型页手工保存该行 → 400「cost_rates 非法」。

### 9.2 `cost`（v1 位）与 `costRates` 的关系

- `cost` 保留 models.dev v1 形状与字段名，但**语义按本格式改为人民币/百万 token**，并由同作用域 `gateway.costCurrency: "CNY"` 显式声明。
- `cost` 是**展示/对照位**：灵枢当前**完全不解析**它（证据见 13.1），不产生任何计费影响。
- 一致性要求：数据集内 `cost` 的数值应与 `costRates` **一致**（建议由 `costRates` 派生，避免两套数字打架）。

### 9.3 倍率 → 积分（灵枢实际算式）

灵枢计费引擎（`billing.ComputeCredits`）：

```
积分 = (输入token × Input + 输出token × Output + 缓存读token × CacheRead) × 时段系数 × 分档系数 ÷ R
R = billing.r（默认 10000）
```

由此可推出兰台直接可用的两条速算：

- **积分价值** 每 1 积分 = R ÷ 1,000,000 元 = **0.01 元**（R=10000）
- **每百万 token 积分 = 倍率 × 100**

价格落库**不经过** `billing.cny_rate`（那是美元→人民币的折算系数，只服务 9.4 的参考价路径）。

### 9.4 边界：`cost` 绝不能喂给 `sync.price_source_url`

灵枢有两条完全独立的 URL 配置：

| 配置项 | 数据源 | 价格语义 | 是否折算 |
|---|---|---|---|
| `preset.pack_url` | **本数据集** | 人民币倍率 | 否 |
| `sync.price_source_url` | models.dev `api.json` | **美元**/百万 token | 是：× `billing.cny_rate`（默认 6.8）|

`sync.price_source_url` 拉到的 `cost` 会被当作美元，× cny_rate 后成为「参考价」，并可通过「一键应用参考价」写入对外模型的 `sale_rates`（售价倍率）；同一份 ×6.8 的数值也进价格变更告警基线（不落售价）。

链路（代码核实，2026-10-28）：`internal/config` 的 `Sync.PriceSourceURL` → `cmd/server/main.go` 注入 `sync.NewSyncer` → `internal/domain/sync/source.go` 按 models.dev 形状解析 `cost.input/output/cache_read`，**硬编码按美元/百万 token 理解、无任何单位识别**；`priceRefAdapter.ReferencePrice` 里 `p.Pricing.Prompt × cny_rate`（默认 `6.8`，由 `internal/seeding/seed.go` 种下、`billing/service.go` 读配置）→ `model/service.go`（ApplyPrice / SyncPrices）→ `model/store.go` 的 `UPDATE external_models SET sale_rates`。

**挂错 URL 的实际后果分两种，别混为一谈**（`flatten()` 要求 `cost.input` 与 `cost.output` **同时存在**才收录，否则跳过该模型）：

| 形态 | 数据集内容 | 后果 |
|---|---|---|
| a | 只带 `gateway.costRates`（本数据集正本） | 一个带 `cost` 的模型都解析不出来 → 参考价永远查不到、批量同步全 Skipped。**不报错、也不放大**（静默无效，白挂） |
| b | 同时带 v1 的 `cost`（哪怕填的是人民币数） | 被当美元 ×6.8 写进售价 → **虚高 6.8 倍**。这才是真正的放大 |

⇒ 硬红线不变：**本数据集只能挂 `preset.pack_url`，绝不能挂 `sync.price_source_url`**。形态 a 是「白挂」，形态 b 是「错价」，两种都要在发布纪律里挡住。

### 9.5 完整样例：`glm-5.3-flash`

官网人民币价（智谱 BigModel 开放平台，`https://open.bigmodel.cn/pricing`，as_of **2026-10-28**）：

| 项 | 官网人民币价（元/百万 token） | `costRates` | 每百万 token 积分（×100） |
|---|---|---|---|
| 输入 | 0.8 | `Input: 0.8` | **80** |
| 输出 | 2.8 | `Output: 2.8` | **280** |
| 缓存命中（缓存读） | 0.23 | `CacheRead: 0.23` | **23** |
| 缓存存储 | 限时免费 | — | — |

算式：

- 100 万输入 token → `1,000,000 × 0.8 ÷ 10000 = 80` 积分
- 一次调用（10 万输入 + 1 万输出、无缓存命中）→ `(100000 × 0.8 + 10000 × 2.8) ÷ 10000 = 108000 ÷ 10000 = ` **10.8 积分**
- 交叉校验（积分价值 0.01 元）：`10.8 × 0.01 = 0.108 元`，与按量价 `0.08 + 0.028 = 0.108 元` **一致** ✓

限时五折（官网 2026-08-26 起，至 **2026-09-09 24:00 UTC+8**，现已结束，仅作 `offers` 写法示例）：
输入 0.4 / 输出 1.4 / 缓存命中 0.115 元 → `Input: 0.4` `Output: 1.4` `CacheRead: 0.115` → **40 / 140 / 11.5 积分每百万 token**。

数据集片段：

```jsonc
{
  "zhipu": {
    "id": "zhipu",
    "name": "智谱 BigModel",
    "env": ["ZHIPU_API_KEY"],
    "npm": "@ai-sdk/openai-compatible",
    "api": null,
    "doc": "https://open.bigmodel.cn/pricing",
    "models": {
      "glm-5.3-flash": {
        "id": "glm-5.3-flash",
        "name": "GLM-5.3-Flash",
        "description": "智谱原生多模态模型，1M 上下文，官网人民币计价。",
        "cost": { "input": 0.8, "output": 2.8, "cache_read": 0.23 },
        "limit": { "context": 1000000, "output": 128000 },
        "modalities": { "input": ["text", "image", "video", "pdf"], "output": ["text"] },
        "tool_call": true,
        "reasoning": true,
        "gateway": {
          "protocols": { "openai-chat": {} },
          "costRates": { "Input": 0.8, "Output": 2.8, "CacheRead": 0.23 },
          "offers": []
        }
      }
    },
    "gateway": {
      "baseUrl": "https://open.bigmodel.cn/api/paas/v4",
      "protocols": { "openai-chat": { "path": "/chat/completions", "default": true } },
      "costCurrency": "CNY",
      "offers": []
    }
  }
}
```

## 10. 枚举与容忍规则

| 枚举位置 | 性质 | 灵枢行为 | 未知值后果 |
|---|---|---|---|
| `gateway.protocols` 的键 | 开放 | 白名单过滤（只留 3 个基础协议） | 静默丢弃条目键，**不报错** |
| `models.*.type` / `modalities.*` | 开放（v1 原文封闭） | 不解析 | **不报错** |
| `models.*.status` / `reasoning_options` | 开放 | 不解析 | **不报错** |
| `gateway.offers[].kind` / `quota.unit` | 开放 | 不解析 | **不报错**（整条忽略） |
| `gateway.costCurrency` | 开放 | **完全不解析**（`GatewayBlock` / `ModelGateway` 都无此键） | **不报错**，也不影响任何数值（见 6.5） |
| **`gateway.requestHeaders.passthroughMode`** | **封闭** | 校验 | **400 + 整包同步回滚** |
| **`gateway.sourceStatus`** | **封闭** | 校验（仅 import 路径） | **400**（sync 路径强制写 `ACTIVE`，不受影响） |
| **基础协议名**（`gateway.protocols` 归一后） | **封闭（3 个）** | 校验（物化后） | 已被白名单过滤 → 实际不可达；但若走 CRUD/import 显式写非法协议则 **400** |
| `gateway.errorPolicy[].*` / `features[].key` | 封闭 | pack 路径不解析 | **不报错**（仅 CRUD 路径校验） |

统一策略（对齐第 2 节原则 4/6）：**开放枚举遇未知值一律「丢掉该值、保留其余、继续处理」，不得整个包失败**；封闭枚举（仅 `passthroughMode` / `sourceStatus`）必须严格取值。

## 11. 校验规则

### 11.1 会让整包同步失败（事务整体回滚，一条不留）

| # | 触发条件 | 表现 |
|---|---|---|
| V1 | JSON 语法错误 / 顶层不是对象 | 解析失败，sync 返回错误 |
| V2 | `preset.pack_url` 拉取非 200 或超时 | sync 返回错误 |
| V3 | 某 provider 的 `id` 与 `name` **同时为空** | 400「渠道协议名称不能为空」 |
| V4 | provider 级 `gateway.baseUrl` 非空但非 `http(s)://` 开头 | 400「BaseURL 必须以 http:// 或 https:// 开头」 |
| V5 | `gateway.requestHeaders.passthroughMode` 非法 | 400「passthroughMode 只支持 …」 |
| V6 | 两个 provider 物化出同名（`name` 或 `id` 重复） | 唯一索引冲突，整次回滚 |
| V7 | provider 级 `protocols` 里 `default: true` 多于一个 | 400「至多一个默认协议」 |
| V8 | 把 `gateway.externalModelID` 写成**数字**（该字段是 `json:"externalModelID,string"`） | 整包 JSON 解析失败、sync 报错、全部回滚（`json: cannot unmarshal number into Go struct field ...`）。数据集本来就不该填它，此条防手滑 |

### 11.2 不报错但会静默出问题（验收时必须逐项核对）

| # | 情况 | 后果 |
|---|---|---|
| S1 | 模型条目无 `protocols` 且无 `baseUrl` 且 `externalModelID ≤ 0` | 整条模型（含价格）被丢弃 |
| S2 | `gateway.costRates` 缺三段中任一，或键拼错（`cache` / `Cache_Read` / `inputTokens`） | 键归一化只认 `Input/Output/CacheRead` 与 `input/output/cache_read`，其余键**原样保留、不报错**（`WireRates.UnmarshalJSON`）；物化路径**不调用** `ValidateRates`，所以同步阶段完全无感，缺段/错键一路落进 `channel_models.cost_rates`。计费时 `ComputeCredits` 报错但被 `cerr == nil` 吞掉（不记日志、不告警）→ **成本积分静默为 0，成本/毛利曲线整段为空**。售价侧缺键则在结算时直接报错（非静默）。唯一会响的是管理员在渠道模型页手工保存该行 → 400「cost_rates 非法」 |
| S3 | `costRates` 数值为负 | 同步阶段同 S2 静默；手工保存时 `ValidateRates` 拒绝（400） |
| S4 | `cost` 与 `costRates` 数字不一致 | 无报错，但两处读数打架，排障困难 |
| S5 | 忘记写 `costCurrency` / 写成 `"USD"` | **不影响计费**：后端全仓没有币种字段与换算分支（该键解码即丢弃），落库与出账完全相同。缺省或写错只影响人读/未来消费方。真正的风险是**数值币种填错**（把美元数当人民币写进 `costRates` → 虚低 6.8 倍），见 9.1「数值」行 |
| S6 | provider 未写 `models` | 协议物化成功但零模型 |
| S7 | `gateway` 整块缺失 | 协议物化成功但无 `baseUrl` / `protocols`，请求不可用 |

### 11.3 建议补的代码执法点（本次只登记，未改代码）

上表 S2 / S5 的共同问题：**数据集的错在同步阶段没有声音**。要把它们从「靠人核对」变成「V 类必失败」，需在代码侧补以下执法点（留给 Core 排期，本文档只定口径）：

| # | 位置 | 建议 | 效果 |
|---|---|---|---|
| E1 | `channel/service.go` 的 `materializeFromProtocol` | 落库前调用 `billing.ValidateRates`，失败即 `return error`（外层 `channelprotocol/service.go` 已是 `store.WithTx`，可整包回滚） | 缺段/错键升为 V 类，同步即失败 |
| E2 | `channelprotocol/model.go` 的 `ModelGateway` + `source.go` 的 `GatewayBlock` | 补 `CostCurrency string \`json:"costCurrency"\``，并在 `toChannelProtocol` 校验为空或 `"CNY"` | `costCurrency` 从标注变成执法点 |
| E3 | `billing/rates.go` 的 `WireRates.UnmarshalJSON` | 只在**预设解析入口**加严（拒绝未知键），不动管理端兼容 | 拼错键立刻报错 |
| E4 | `billing/service.go`（`ComputeCredits` 错误被 `cerr == nil` 吞掉） | 至少记一条 warn 日志（带 model / channel） | 成本静默 0 可被发现 |
| E5 | `channelprotocol/source.go`（`toChannelProtocol` 里无协议的模型 `continue`） | 加一条 warn 日志 | 模型被静默丢弃可被发现 |

> 现状口径：E1–E5 全部**未实现**。所以本数据集必须靠「Schema 自检 + 第 4 节验收清单」挡住 S2 / S5，不能指望代码报错。

## 12. 已知限制（数据集设计时必须绕开）

1. **`cost` / `limit` / `modalities` / `type` 等 v1 能力位全部是惰性字段**：灵枢不解析、不入库、不影响路由与计费。数据集写它们只为人读与未来扩展。
2. **模型条目必须「挂得住」**：见 S1，带价格的模型必须至少声明一个 `protocols` 键。
3. **`externalModelID` 兰台不可填**：它是灵枢内部雪花 ID。因此数据集**无法**把价格自动落到 `channel_models`；正确链路是：
   `数据集 → sync 物化渠道协议（价格已在协议 JSON 里）→ 管理员在渠道协议页把模型绑定到对外模型（externalModelID）→ 保存 → 重物化 channel_models.cost_rates`。
4. **不要把 `GET /api/v1/admin/channel-protocols/export` 的结果当数据集源**：export 只回写 `id` + `gateway`，会丢 `models.*.cost/limit/name/description`，且 `name`/`description` 会以空串输出。
5. **`POST /import` 请求体形状**：必须外包一层 `{"Pack": { ... }}`；响应在 `{"Code":0,"Data":{...}}` 信封内。

## 13. 新字段兼容性结论（`offers`，spencer 要求验证）

### 13.1 结论

**兼容。灵枢读到任何未知字段都不会报错。→ `offers` 在 0.1.0 先摆字段位（`"offers": []` 留空），0.2.0 再填内容，无需任何灵枢改造。**

### 13.2 判定依据（代码位置）

| # | 事实 | 位置 |
|---|---|---|
| E1 | 拉包解码用裸 `json.NewDecoder(...).Decode(&pack)`，**未开 `DisallowUnknownFields`** → 未知键静默忽略 | `backend/internal/domain/channelprotocol/source.go`（`fetchPack`） |
| E2 | import / sync 的请求体解码同样未开严格模式 | `backend/internal/domain/channelprotocol/handler.go`（`decodeBody`） |
| E3 | `Model` 结构体只有 `ID / Name / Description / Gateway` 四个字段——**连 v1 的 `cost` / `limit` 都没有**，说明「惰性字段」是既有设计惯例，不是例外 | `backend/internal/domain/channelprotocol/source.go` |
| E4 | `GatewayBlock` 结构体只有 `baseUrl / protocols / requestHeaders / edited / sourceStatus`；新增 `offers` 不在结构体内 → 忽略 | 同上 |
| E5 | `offers` 位于 `gateway` 命名空间内 → 不污染 models.dev v1 顶层/模型级字段，不会与模型供应商未来字段撞名 | 第 2 节原则 2 |
| E6 | `ValidateRates` 只校验 `input/output/cache_read` 三段，多余键「不参与校验、不影响计费」 | `backend/internal/domain/billing/rates.go` |

补充：整个仓库 `DisallowUnknownFields` 出现 **0 次**（全量 grep 无命中），即「严格解码」在本项目不存在，未知字段容忍是全局一致行为。

### 13.3 为什么判定「不兼容」不成立

唯一的「严格」发生在**物化之后**的 `validate()`：它拒绝非法 `passthroughMode` / 非法 `sourceStatus` / 多于一个 `default`。但这些校验只针对**已被结构体接住的字段**；`offers` 根本进不了结构体，到不了 `validate()`。所以「加字段 → 报错」这条路径在 `offers` 上不存在。

### 13.4 需要注意（不是报错，是语义）

- `offers` 目前**没有任何消费方**（灵枢只把它当噪声丢掉）。因此它的价值是「先冻结契约、兰台先攒数据」，不能指望它影响计费。
- 若未来灵枢要消费优惠价，正确做法是**由消费方在拉取后按 `window` + `effect.priceOverride` 覆盖 `costRates`**，而不是让数据集出现「同一个模型两个价」的歧义。这条等到真要实现时再单开设计。

## 14. 变更记录

| 版本 | 日期 | 变更 |
|---|---|---|
| v0.1 | 2026-10-16 | 首版：v1 对齐 + `gateway{baseUrl,protocols,requestHeaders}` |
| v0.2 草案 | 2026-10-28 | ① 补 `gateway.costRates` 正式口径（此前代码已支持但文档缺失）与 `costCurrency`；② 定价改为**人民币**口径并给倍率→积分算式与样例；③ 新增 `gateway.offers` 优惠字段位（兼容性已验证，见 13）；④ 各字段补**必填性 / 灵枢消费性**两列；⑤ 新增枚举容忍表（10）、校验规则（11）、已知限制（12）；⑥ 明确数据集 URL 与 `sync.price_source_url` 分离（9.4） |
| v0.2 草案 rev.1 | 2026-10-28 | 按 Core 只读核实结果修订三处**事实性错误**：① 9.1 更正「无大小写转换点」——`WireRates.UnmarshalJSON` 就是转换点，真正的坑是拼错键被原样保留 + 物化路径不校验；② 6.5 / S5 更正「缺 `costCurrency` → 虚低 6.8 倍」——代码全无币种概念，该字段纯标注；风险在**数值**填了美元（9.1 新增「数值」硬约束）；③ 9.4 补挂错 URL 的两种后果（静默无效 / 虚高 6.8）；④ 新增 V8（`externalModelID` 必须字符串，否则整包解析失败）与 11.3（5 个建议执法点，未实现） |

**未变更**：v1 字段名与两级嵌套形状、`gateway` 命名空间、协议白名单、鉴权与请求头策略规则。

## 15. 完整示例（多 provider）

```jsonc
{
  "opencode-zen": {
    "id": "opencode-zen",
    "name": "OpenCode Zen",
    "env": ["OPENCODE_API_KEY"],
    "npm": "@ai-sdk/opencode",
    "api": null,
    "doc": "https://opencode.ai/docs/zen",
    "models": {
      "claude-sonnet-4-5": {
        "id": "claude-sonnet-4-5",
        "name": "Claude Sonnet 4.5",
        "description": "Anthropic Claude Sonnet 4.5 via OpenCode Zen.",
        "cost": { "input": 20.4, "output": 102, "cache_read": 2.04, "cache_write": 25.5 },
        "limit": { "context": 200000, "output": 64000 },
        "modalities": { "input": ["text", "image"], "output": ["text"] },
        "tool_call": true,
        "reasoning": true,
        "reasoning_options": ["toggle", "budget_tokens"],
        "gateway": {
          "protocols": { "anthropic-messages": {} },
          "costRates": { "Input": 20.4, "Output": 102, "CacheRead": 2.04 },
          "offers": []
        }
      },
      "gpt-5-codex": {
        "id": "gpt-5-codex",
        "name": "GPT-5 Codex",
        "description": "OpenAI GPT-5 Codex via OpenCode Zen.",
        "cost": { "input": 8.5, "output": 68 },
        "limit": { "context": 400000, "output": 128000 },
        "tool_call": true,
        "reasoning": true,
        "gateway": {
          "protocols": { "openai-responses": {} },
          "costRates": { "Input": 8.5, "Output": 68, "CacheRead": 0 },
          "offers": []
        }
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
      },
      "costCurrency": "CNY",
      "offers": []
    }
  }
}
```

> 示例中的数字是按 1 USD ≈ 6.8 CNY 折算后的**示意值**，只为演示形状；真实数据集必须填**官网人民币价**（9.1）。

## 16. 许可与归属

若直接消费 / 内嵌 models.dev 数据，需保留其 MIT 许可与版权声明（models.dev, MIT, Copyright (c) 2025）。
