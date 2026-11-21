# magpie 入口白名单终表（定案③ · 2026-11-20 spencer 已批准）

> 状态：已确认（设计批准，2026-11-20 09:30）。本文是 nginx 白名单的唯一实施口径，正式任务书 = `docs/pending-tasks.md` ⑤。
> 设计依据：`magpie-key-naming-agent-id` 报告 §4.7/§4.8（lingshu-preview.pages.dev 定案版）。冲突时以本文为准。

## 0. 一句话结论

白名单 = **四协议推理端点 + /v1/models + Codex WS（含 count_tokens×2）共 6 类点位**；health 仅内网（不经 magpie，维持现状）；usage feed 走内网不进外部白名单；embeddings / rerank / audio / legacy completions 维持排除；白名单外 `/v1/*` 一律 404。

## 1. 白名单终表（放行 6 类 / 9 条 + 拒绝 + 内网，共 24 条）

### 1.1 放行点位（白名单 · 必须全部落地）

| # | 点位 | 方法 | 结论 | 行为要点 |
|---|------|------|------|----------|
| 1 | `/v1/chat/completions` | POST | 白名单 | OpenAI Chat 主力流量；HTTP/SSE |
| 2 | `/v1/responses` | POST/GET | 白名单 + WS | OpenAI Responses；需补 `Upgrade`/`Connection` 头（现配置缺失，Codex WS 必断） |
| 3 | `/v1/messages` | POST | 白名单 | Anthropic Messages；HTTP/SSE |
| 4 | `/v1/messages/count_tokens` | POST | **白名单（count_tokens 点位 A）** | 口径见 §2 |
| 5 | `/v1beta/models/{model}:generateContent` | POST | 白名单 | Gemini 推理；`/v1/models/...` 变体同段放行（正则 `/v1(?:beta)?`） |
| 6 | `/v1beta/models/{model}:streamGenerateContent` | POST | 白名单 | Gemini SSE 流式 |
| 7 | `/v1beta/models/{model}:countTokens` | POST | **白名单（count_tokens 点位 B）** | 口径见 §2；`/v1` 变体同段 |
| 8 | `/v1/models` | GET | 白名单 | SDK 启动探测/拉清单；只列可用模型，无管理信息泄露面 |
| 9 | `/v1/responses`（Upgrade 握手） | GET(WS) | 白名单 | Codex WS 实连 101 为验收项；非升级请求走同段安全（map 关闭连接头） |

nginx location 对照（报告 §4.7 原样）：
- `location ~ ^/v1/messages(/count_tokens)?$`（#3+#4 同段）
- `location ~ ^/v1(?:beta)?/models/[^/]+:(generateContent|streamGenerateContent|countTokens)$`（#5–#7 同段）
- `location = /v1/chat/completions`、`location ~ ^/v1/responses$`、`location = /v1/models`
- 兜底：`location /v1/ { return 404; }`（白名单外全 404）

### 1.2 显式拒绝点位（404）

| # | 点位 | 结论 | 理由 |
|---|------|------|------|
| 10 | `/v1/magpie/usage`、`/v1/magpie/usage/requests` | 拒绝 404 | Stats 管理面；灵枢后端 compose 内网直连 magpie:3425 拉账本，不经 nginx；对外暴露 = 任一租户令牌可读全站聚合用量（安全缺口） |
| 11 | `/v1/magpie/quotas|limit|concurrency|keys|admin/*` | 拒绝 404 | magpie 运维/管理点位；灵枢管理走页面 `/magpie/`（票据 auth_request），不经 `/v1/` |
| 12 | `/v1/embeddings` | 拒绝 404 | 不在售卖清单（只做四协议推理） |
| 13 | `/v1/rerank` | 拒绝 404 | 同上 |
| 14 | `/v1/audio/*` | 拒绝 404 | 同上 |
| 15 | `/v1/completions`（legacy） | 拒绝 404（可再评估） | legacy 补全端点；有租户反馈再单独加，默认关 |
| 16 | 其余一切 `/v1/*` | 兜底 404 | 默认拒绝；错误体不带任何内部信息 |

### 1.3 内网 / 不变点位

| # | 点位 | 结论 | 说明 |
|---|------|------|------|
| 17 | `/healthz` | 仅内网 · 白名单不含 | 现网 `location = /healthz` 反代灵枢 backend:8080，不在 `/v1/` 前缀内、本就到不了 magpie → 维持现状即满足 |
| 18 | `/magpie/` 管理页 | 不变 | 票据 auth_request 把关，不经本白名单体系 |
| 19 | magpie usage feed | 不变 | 后端内网直连 magpie:3425，不经 nginx |
| 20 | `MAGPIE_TRUST_PROXY` | 不设 | 维持约束，不给伪造 XFF 开门 |
| 21 | Host 头 | 不改 | `proxy_set_header Host $http_host`，改内部名 Origin 判定会 403 |
| 22 | 非 `/v1/` 外部路径 | 不变 | 维持现网行为，本次只动 `/v1/` 体系 |
| 23 | 请求头透传 | 契约文档 §2 | Authorization/UA/anthropic-beta 等 P0 清单不许动 |
| 24 | 错误体改写 | 不做 | sub_sub_filter 类改写放弃（Content-Length/SSE 风险），错误体原样透传 |

## 2. count_tokens 两点最终口径（逐条写死）

**点位 A：`POST /v1/messages/count_tokens`**
1. **纳入白名单**——Anthropic SDK（Claude Code 长上下文预算）必需点位，不加则 SDK 侧 404/路径不可用。
2. **契约行为**：原样透传 magpie，无 usage 账本行、不计费、不产生预扣；4xx/5xx 照常透传，对账零影响；响应头照常过剥头层。

**点位 B：`POST /v1beta|/v1/models/{model}:countTokens`**
1. **纳入白名单**——gemini-cli 等 SDK 常调点位，与 generateContent 同段正则放行。
2. **契约行为**：同点位 A——不计费、无账本行、错误透传；唯一额外约束是该端点不得承载任何管理操作（magpie 侧本就不提供，nginx 层无需再加判别）。

> 两点均写进 §1.1 location 正则（`(/count_tokens)?` 与 `:countTokens`），不是单独 location；验收见 §4。

## 3. legacy_key_name 反查口径（回填阶段的旧键映射，不留含糊）

1. **列定义**：`magpie_keys` 加 `legacy_key_name TEXT`（纯 ADD 迁移，编号顺现行链取未用号），存改名前旧名。
2. **写入时机**：只在 M2 回填改名时写一次（旧名）；M4 起冻结只读，不再有写入方。
3. **反查顺序**：`v3billing/poller.settleOne` 反查 = 先按 `key_name`（且 `state='ACTIVE'`）查，查不到再按 `legacy_key_name` 查；两段都查不到才 Warn + 跳过该账本行。
4. **唯一性语义**：改名期间新旧名不得同时 ACTIVE（防止同名命中错行——poller 是 LIMIT 1）；`legacy_key_name` 不加 UNIQUE（多个历史别名理论可能，实际一令牌一旧名）。
5. **回滚语义**：回滚 = 按 `legacy_key_name` 复原旧名（`Ensure(旧名, 同一明文)` → `Provision` → 删新名），三步与正向镜像、可幂等重跑。
6. **结算语义**：反查命中的旧账本行按现行 0011/v2 分段口径正常结算，不重算、不估算——「分段口径与归属键口径分离」。

## 4. 验收断言（可量化，转任务书 ⑤）

1. 白名单 6 类点位四协议冒烟全 200；`/v1/responses` WS 握手 101（Codex 实连）。
2. `POST /v1/messages/count_tokens` 与 `POST /v1beta/models/{m}:countTokens` 各实测 200，且**不产生**账本行（前后 `ledger_entries` 计数不变）。
3. 白名单外抽样全 404：`/v1/magpie/usage`、`/v1/magpie/admin/*`、`/v1/embeddings`、`/v1/completions`。
4. `/healthz` 外网不可达、内网正常。
5. 对外响应头零「magpie」字样（大小写不敏感全量断言）。
6. 四家官方 SDK 初始化各跑一次零报错。
