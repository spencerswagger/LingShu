---
summary: "灵枢 V2 第一期 · magpie 转发链接入（facade）"
read_when:
  - 排查 /v1 转发问题
---

# magpie 转发链接入（灵枢只做 facade）

第一期第二件：把灵枢的自制转发链换成「接代理侧」。灵枢负责**身份与钱**，代理侧负责**转发与协议**。

```
客户端 ──(灵枢令牌)──▶ 灵枢 /v1/* ──(callerKey + 改写后的 model)──▶ magpie ──▶ 上游
                          │
                          ├─ 鉴权（令牌 → 用户，缓存 + 失效钩子）
                          ├─ 准入（资产白名单：enabled + magpie_model + 非订阅池）
                          ├─ 改写（只换 model，其余字节不动）
                          └─ 旁路读 usage → Result（第③件计费接入点）
```

## 1. 端点与协议

| 入站（＝出站同形） | 协议 | model 改写落点 | 计费 |
|---|---|---|---|
| `POST /v1/chat/completions` | openai-chat | body.model | 是 |
| `POST /v1/responses` | openai-responses | body.model | 是 |
| `POST /v1/messages` | anthropic-messages | body.model | 是 |
| `POST /v1/messages/count_tokens` | anthropic-messages | body.model | 否 |
| `POST /v1beta/models/{m}:generateContent` | gemini | **path** 的 `models/{m}` 段 | 是 |
| `POST /v1beta/models/{m}:streamGenerateContent` | gemini | **path** | 是 |
| `POST /v1beta/models/{m}:countTokens` | gemini | **path** | 否 |
| `GET /v1/models` | — | — | — |

只认上表路径；其余路径/方法一律 404/405（不放宽成开放代理）。

## 2. 「只换两样东西」的落地方式

- **凭证头**：`magpie_keys`（UNIQUE(token_id)）→ SM4 解密 → 按 `magpie.auth_header`/`auth_scheme` 注入；
  入站的 `Authorization`/`x-api-key`/`api-key` 一律丢弃（强制黑名单）。
- **model**：`external_models.magpie_model`。用 `json.Decoder.InputOffset` 定位顶层 `model` 的**值区间**，
  只替换这段字节（不重新序列化，键序/空白/数字写法/转义序列原样）。
- **其余**：请求头按强制黑名单剔除后逐值透传，`Accept-Encoding` 固定 `identity`；响应头在 hop-by-hop
  之外按口径②剔除——灵枢/代理内部标识（`x-magpie-*` 前缀）、上游实现暴露（`server`/`x-powered-by`）、
  头名或任一取值含供应商名/身份类特征词的头（`X-Vendor-Trace`/`x-provider: openai` 等，供应商名清单见
  `headers.go` 的 `vendorTokens`，宁可收紧）；`x-request-id` 保留灵枢链路 ID（外层中间件已写入，
  上游同名头不得并成第二值；中间件未挂载时原样透传不误删）；
  响应体（含 SSE 分包、错误体）逐字节原样；**不注入 `include_usage`**。
- **唯一豁免（契约 §8.1）：凭据回显遮蔽**。上游常把我们的 callerKey（或 `Bearer <key>` 整段头）原样回显在
  错误体里；直接透传等于把渠道凭据发给租户。因此转发层只做这一件事：把**本次请求实际用到的那把**凭据命中
  的子串替换成 `<redacted>`，全部状态码生效，**无命中则零改动**（字节与 `Content-Length` 都不变）；
  不做正则泛化（`sk-other-key` 之类原样保留），按子串语义（`caller-secret-2` 会连前缀一起遮，宁可多遮）。
  非流式整段读后重算长度；流式用「尾部暂存」处理 SSE 分包切断，长度交 chunked；上游无视 `identity` 仍
  gzip/deflate 时先解压再遮蔽并丢掉 `Content-Encoding`；stdlib 解不了的编码（如 `br`）只能按下行原字节透传，
  `Result.UnmaskableEncoding` 标记 + 一条告警（这是本豁免的已知边界）。

### 2.1 三条边界（第二轮复审定稿）

- **超 32MB 的非流式响应不截断**：整段读到 32MB 上限后，已读部分先写、**剩余继续流式转发到底**，长度交
  chunked（`Content-Length` 删除）。此前实现读到上限就直接 return，35,651,584 字节的响应实际只送到
  33,554,433（丢 2,097,151），状态仍是 200 且 `Result.Err=nil`，调用方完全看不出被截断。
- **声称压缩但字节不是压缩 → 回退原始字节**：`gzip.NewReader`/`zlib.NewReader` 在判定失败前已经从响应体
  读走一段（典型 10 字节，流损坏时更多），直接回空壳等于把整段响应丢光（实测 1,500 字节 → 0 字节）。
  现在解码器坐在「快照 reader」上：`NewReader` 失败、或**读到中途失败**（如声称 gzip 但流被截断）都回放
  「已消费前缀 + 剩余」的原始字节，`Content-Encoding` 原样保留，`Result.UnmaskableEncoding` 标记 + 告警。
  「有内容 + 告警」优先于「空响应」。
- **query 里的 `key` 判定与剔除同源**：键名比较统一走解码口径（`?ke%79=`、`?%6Bey=` 都算 `key`），
  且**头鉴权 + query 同时带 `key`** 也照样剔除（此前只按「本次鉴权是否来自 query」决定剔不剔，这条路漏剔）。
  剔除只按 `&` 切段、其余参数**逐字节原样**（不整体重编码 `RawQuery`，签名/时间戳类参数会被打坏）；
  没有 `key` 参数时 `RawQuery` 一个字节都不动。
- **顶层重复 `model` 只改生效的那一处**：JSON 语义是「重复键后者生效」，因此只替换最后一处；
  其余出现过的 `model` 逐字节保留（此前是把每一处都替换，等于擅自改写了语义上被忽略的字节）。
  不变式不变：除生效的 model 值外，键序/空白/换行/转义**逐字节**不动。
- **准入拒绝的状态码分三类**（矩阵 B-11 / P-10）：
  「模型不存在 / 已停用」（`model.ErrNotFound`）→ `404 model_not_found`，文案「模型不存在或未对租户开放」；
  「已开放但未映射 `magpie_model`」（配置未完成）→ `404 model_not_found`，文案「模型尚未接入转发服务」；
  「订阅池 / 未开通 callerKey」→ `403 model_not_allowed` / `403 magpie_key_missing`（权限口径）。
  停用与不存在对外**不可区分**（不泄漏「这个名字存在但被停用」）；只有真正的服务故障才是
  `500 internal_error`。

## 3. 准入（灵枢唯一的策略点）

三条同时成立才放行：

1. `external_models.enabled = true`；
2. `magpie_model` 非空（未映射 = 不转发）；
3. `magpie_model` 不命中订阅池前缀（`sys_configs['magpie.subscription_prefixes']`，
   默认 `claude/ codex/ copilot/ gemini-code-assist/ antigravity/ grok/`）。

命中第三条 → `403`，`code=model_not_allowed`，文案「模型未对租户开放（订阅池渠道默认不开放）」。
配置读失败时回落代码默认名单：**失败方向是继续拦截**，不是放开。

拒绝口径（状态码 / 归类码 / 文案必须能区分「名字打错」「配置没做完」「你没权限」）：

| 情况 | 状态码 | `error.code` | 文案 |
|---|---|---|---|
| 模型不存在 / 已停用（`model.ErrNotFound`） | 404 | `model_not_found` | 模型不存在或未对租户开放 |
| 已开放但未映射代理模型 | 404 | `model_not_found` | 模型尚未接入转发服务 |
| 订阅池渠道 | 403 | `model_not_allowed` | 模型未对租户开放（订阅池渠道默认不开放） |
| 令牌未开通代理访问 | 403 | `magpie_key_missing` | 该令牌未开通代理访问 |
| 真正的服务故障（DB 等） | 500 | `internal_error` | 服务内部错误 |

「不存在」与「已停用」共用 404 + 同一句话：`model.Service.GetByExternalName` 只返回启用记录，
两者在生产路径上已经不可区分，对外也**不应该**可区分（否则任意租户能枚举出「哪些模型被停用」）。

## 4. 失败与超时口径

| 场景 | 下游 | 备注 |
|---|---|---|
| 连不上 magpie / 建连失败 / DNS | 503 | `Retry-After: 5` |
| 首包超时（收不到响应头） | 504 | `TimedOut=true` |
| 流空闲超时 | 中断连接 | 响应头已发出，无法改状态码 |
| 上游 4xx/5xx | **原样** | 不二次包装（包装就改了上游错误体） |
| 上游 204/304 | 原样状态码、不写 body | `NoContent=true` |
| 客户端中途取消 | 已读到的 usage 保留 | `Canceled=true` |

错误体按**入站协议形状**输出（OpenAI / Anthropic / Gemini 三套），状态码语义按上表。
超时阈值全部可配（`magpie.*_ms`），默认：建连 10s、非流式 600s、流首包 30s、流空闲 120s、计数 30s。

## 5. 与第③件的接缝

`Proxy.Forward` 产出 `Result`（`Status`/`Usage`+`HasUsage`/`Streamed`/`Canceled`/`TimedOut`/`NoContent`/
`Billable`/`CallerKeyName`），由 `OnComplete` 钩子消费。**本期不注入钩子**：
不写 `billing_records`、不碰预扣/结算、不写 `call_logs`（第一期已停写）。