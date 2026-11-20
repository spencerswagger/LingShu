# magpie 透传接口契约定稿（2026-11-20 · spencer 已批准设计的实施口径）

> 状态：已确认。本文与 `magpie-whitelist-final-table.md`（白名单终表）、`docs/pending-tasks.md` ①②③⑤ 互为依据；与报告冲突时以本文为准。

## 1. 字段与命名契约

### 1.1 key 名（存储名 key_name / 逻辑名）

| 项 | 口径 |
|----|------|
| 逻辑名（展示） | `用户名-令牌名` 原文（张三-办公室台式机），UI/账单/导出一律用逻辑名 |
| 存储名 key_name | 用户名转写 + `-` + 令牌名转写；纯 ASCII；≤64 字符；不含 PII（无邮箱/手机号/token_hash 片段） |
| 转写规则 | 非 `[a-z0-9-]` 段做 ASCII 转写；纯中文/无 ASCII 段退化：用户名→`u<uid>` 段、令牌名→`w-`+4hex；空段兜底随机 |
| 重名消歧 | 同用户重名或跨用户转写撞名 → 尾部 `-2/-3` 顺序追加；**分配后固化不回收**；64 封顶时先截转写段再拼尾号，尾号永不截 |
| 唯一性归属 | 归属映射由 `magpie_keys.token_id` 承担；key_name 只是连接键；uid/tokenID **不进** key 名 |
| 迁移 | `magpie_keys` 纯 ADD：`legacy_key_name TEXT`；编号顺现行链取未用号；零 DROP |

### 1.2 契约字段（管理端 API）

| 字段 | 类型 | 出现于 | 说明 |
|------|------|--------|------|
| `key_name` | string | TokenInfo（新增补齐） | 存储名；管理视图 JOIN username/nickname 渲染逻辑名 |
| `legacy_key_name` | string / null | magpie_keys 列 + 管理视图只读 | 改名前旧名；回填后只读冻结 |
| rename 端点 | — | `POST /api/v1/admin/tokens/magpie-keys/rename` | **dry-run 默认开启**（`?dry_run=false` 才执行）；逐令牌 Ensure→Provision→DeleteCallerKey(旧名)；任一步失败停在「新名已建、库未改」，可幂等重跑 |
| 错误码 | — | 409 | key_name UNIQUE 撞名（消歧重试一轮后仍撞 → 409 挂起人工） |

## 2. 行为边界（透传语义）

1. **透传面**：白名单 6 类点位（见终表 §1.1）全部原样透传 magpie——请求体不动、响应体不动、SSE 字节流不动、WS 帧不动；`proxy_buffering off`、读写超时 3600s。
2. **鉴权面**：租户令牌鉴权在灵枢入口维持现状（`?key=` 特判转发前必须剔除——防令牌出境，安全红线不变）。
3. **错误语义**：4xx/5xx **原样透传不改写**（含 magpie 的错误体）；`count_tokens` 两点同口径，不计费不落账本。客户端断开无 usage → 沿用 V2 契约「退预扣不估算」。
4. **限流语义**：剥掉 `X-Magpie-Limit-Reset` 后，对外限流提示由 `Retry-After` / `x-ratelimit-*` 承担（保留透传，见 §3）；无则客户端按既有 SDK 默认退避。
5. **重试约束（全链路统一）**：灵枢/工具侧遇 429/5xx 指数退避 5s→20s→60s，最多 3 次，不换方案。

## 3. 分层分工：哪层拦、哪层校验（nginx vs 应用）

| 职责 | 层 | 手段 |
|------|----|------|
| 路径放行/拒绝（白名单 6 类，其余 404） | **nginx** | `location` 白名单 + `location /v1/ { return 404; }` 兜底 |
| WS 升级（Codex） | **nginx** | `/v1/responses` 补 `Upgrade`/`Connection` 头（http 块 `map $http_upgrade`） |
| 响应头剥除（X-Magpie-* 全族、品牌/版本头） | **nginx** | `proxy_hide_header` 逐个枚举（headers-more 不在镜像、不支持通配）；实现时从 magpie 源码+抓包核全清单并归档 |
| 请求头透传（Authorization/UA/anthropic-beta 等 P0 清单） | **nginx 不动** | 原样透传，不许增删 |
| `?key=` query 剔除 | **应用（灵枢入口）** | nginx 不做 body/query 改写 |
| 令牌鉴权/预扣/结算/归属 | **应用** | magpie_keys.key_name ↔ token_id；`legacy_key_name` 兜底反查 |
| 错误体改写 | **不做** | sub_filter 放弃（Content-Length/SSE 风险） |
| health | **内网** | `/healthz` 反代灵枢 backend:8080，不经 magpie、不进对外白名单 |
| 供应商身份头（F8 名单） | **nginx** | 品牌类剥；`x-ratelimit-*`/`Retry-After` 保留 |

## 4. 交付物与验收索引

- 白名单终表：`docs/designs/magpie-whitelist-final-table.md`（24 条 + count_tokens 两点 + legacy_key_name 反查口径）。
- 正式任务书：`docs/pending-tasks.md` ①②③⑤（本契约定稿时同步登记）。
- 验收断言汇总见终表 §4（WS 101、count_tokens 无账本行、404 抽样、响应头零 magpie、四 SDK 冒烟）。
- 报告联动：`magpie-key-naming-agent-id` §4.7/§4.8 为同口径来源，任务书冲突时以 pending-tasks.md 为准。
