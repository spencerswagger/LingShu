# 成本页改版 · 后端接口与口径说明

- 日期：2026-10-25
- 对应 demo：PC `https://lingshu-preview.pages.dev/cost-demo` / 移动 `.../cost-demo-m`
- 范围：`internal/domain/console/cost.go`（成本统计表改造）、`internal/domain/console/cost_pricing.go`（新增定价曲线）、
  `internal/server/server.go`（路由）、`backend/scripts/demo-context-tiers.sql`（测试/演示分档样例）。
- 与前端的分工：本轮只做后端与配置样例，**没有改任何前端文件**。

---

## 1. 接口一览

| 方法 | 路径 | 状态 |
|---|---|---|
| GET | `/api/v1/admin/cost/stats` | **改造**（按实体聚合 + 探测行剔除 + 新增 4 个字段） |
| GET | `/api/v1/admin/cost/defaults` | **口径调整**（同样剔除探测行） |
| GET | `/api/v1/admin/cost/curves` | 不变（老的「轮次 × 渠道」模拟曲线保留） |
| GET | `/api/v1/admin/cost/session/{id}` | 不变 |
| GET | `/api/v1/admin/cost/pricing-curve` | **新增**（按真实公式的定价曲线） |

---

## 2. `/cost/stats`：按实体聚合

### 2.1 实体与键

实体 = `(解析出的 channel_id, 解析出的 external_model_id, 解析出的 channel_models.id)`。

解析方式（**查询期解析，本轮不给 `billing_records` 加列**）：

- 由 `billing_records.channel_key_id → channel_keys.channel_id` 得到渠道；
- 用 `(channel_id, billing_records.internal_model_id)` 反查 `channel_models`；
- 同一 `(channel_id, internal_model_id)` 存在多条配置（删除后重建）时：**优先未删除，其次 id 最大者**；
- 已被软删的 `channel_models` 仍参与解析 —— 配置下线不会让历史账单失去归属。

`EntityKey`（前端 row-key）两种格式：

| 情况 | 格式 | 例子 |
|---|---|---|
| 解析到 `channel_models` 行 | `cm:<channel_model_id>` | `cm:900` |
| 解析不到（无渠道 / 从未配置过） | `name:<对外模型名原文本>`；若同时有内部模型 ID，追加 `#<internal_model_id>` | `name:orphan-model#orphan-int` |

降级桶**不合并、不丢弃**：不同文本各自成桶；追加内部模型 ID 是为了避免「同一文本 + 不同内部模型」被并成一条。

### 2.2 字段（`Rows[]`）

| 字段 | 类型 | 说明 |
|---|---|---|
| `EntityKey` | string | **新增**。稳定实体键，见上表 |
| `ChannelID` | int64 | 渠道 ID（无渠道为 0） |
| `ChannelName` | string | 渠道名；缺失回退渠道 ID 字符串；无渠道为 `(无渠道)` |
| `ChannelModelID` | int64 | **新增**。解析出的 `channel_models.id`（解析不到为 0） |
| `ExternalModelID` | int64 | **新增**。解析出的 `external_models.id`（解析不到为 0） |
| `InternalModelID` | string | 渠道内部模型 ID |
| `ExternalModelName` | string | **展示名**：解析成功用当前对外模型名，解析不到用账单原始文本 |
| `SnapshotNames` | string[] | **新增**。该实体历史上出现过的所有对外模型文本（字典序去重，供「曾用名」标注） |
| `CostCredits` | float64 | 成本积分（5 位小数） |
| `SaleCredits` | float64 | 售价积分（5 位小数） |
| `Requests` | int64 | 请求数 |
| `InputTokens` / `CacheReadTokens` / `OutputTokens` | int64 | 三段 token（正则守卫，非法按 0） |
| `CacheHitRate` | float64 | 缓存读 ÷ (输入 + 缓存读)，0~1 |
| `InputOutputRatio` | float64 | 输入 ÷ 输出 |

`Total` 结构不变（同一组字段去掉维度字段），累加口径与 `Rows` 完全一致。

排序：`CostCredits` 倒序 → `EntityKey` 升序（稳定，翻页/刷新不会跳行）。

### 2.3 金额口径（硬约束）

- 金额来自 `SUM(billing_records.cost_credits / credits_consumed)`，**只是把行重新分组**，没有重算、没有改公式。
- 账单写入时金额已按 5 位小数落库（`NUMERIC(20,5)`），分组粒度不影响总和；
  逐行 `Round5` 对 ≤5 位小数的值是无操作，因此**改造前后 Amounts 逐位相等**。
- 唯一会改变金额的是**探测行剔除**（下一节），属预期修正。
- 已在临时 PG 库上用「旧 SQL 分组 vs 新 SQL 实体分组」对比验证：sale/cost/token/请求数全部相等。

### 2.4 探测行剔除（口径变化，需在 PR 说明）

`pricing_mode='cost'` 的健康探测行不再计入售价与统计口径（spencer 已拍板）。判定**三个条件同时成立**：

1. `pricing_mode = 'cost'`；
2. 账单的 token 属于系统探测身份（`users.is_system = true` 或 `tokens.display_name = 'system-probe'`）；
3. 没有对应 `call_logs`（真实请求必写 call_log；探测走 `billing.Record` 直记，不写 call_log）。

影响：此前探测开销混进统计，把售价与请求数抬高（test 库此前约虚高 14.25%）。剔除后售价数字下降属**预期修正**。
真实用户（含 cost 定价模式的开发者）的账单一定有 call_logs，不会被误剔。

同一口径也应用到 `/cost/defaults`（每轮输入、缓存命中率、输入输出比）。

---

## 3. `/cost/pricing-curve`（新增）

### 3.1 公式（与 `billing.ComputeCredits` 同一份语义，不另立一套）

```
credits = Σ(段 token × 段费率) × timeCoeff × ctxCoeff ÷ R
```

- Σ：三段求和 —— `input×rate.input + cache_read×rate.cache_read + output×rate.output`。
  三段互斥：`input` 是纯输入（不含缓存读）。
- `timeCoeff`：**单值**，取「结算时刻」命中的时段段系数。跨时段请求按结算时刻整笔计一个系数，
  不按分钟切分（spencer 已确认保持现状）。
- `ctxCoeff`：按 **输入 + 缓存读**（= 上下文长度）查上下文分档；上下文随会话累加，
  所以长会话会在某一轮**跳档**。
- `R`：积分换算系数（`billing.r`，默认 10000）。
- 取整：`Round5`。

两条线各用各的配置，互不借用：

| 线 | 费率 | 时段 | 分档 | 来源表 |
|---|---|---|---|---|
| 售价 `Sale` | `sale_rates` | `time_config` | `context_tiers` | `external_models` |
| 成本 `Cost` | `cost_rates` | `time_config` | `context_tiers` | `channel_models` |

模型级配置为空 → 回落全局 `sys_configs`（`billing.time_config` / `billing.context_tiers`），
并回报实际来源（`TimeConfigSource` / `TierSource`）。

### 3.2 参数

| 参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `ChannelModelID` | int64 | 空 | 成本线模型；同时可反查它的对外模型用于售价线 |
| `ExternalModelID` | int64 | 空 | 售价线模型（优先于名称） |
| `ExternalModelName` | string | 空 | 售价线模型名（`ExternalModelID` 为空时生效） |
| `Step` | int | 1000 | 横轴步进（token），上限 100000 |
| `MaxTokens` | int | 按分档推导 | 横轴上限，上限 5000000；点数上限 2000（超出自动截断上限） |
| `HitRate` | float | 0.9 | 缓存读占上下文比例（0~1） |
| `InOutRatio` | float | 10 | 输入 ÷ 输出；决定输出 token 数 |
| `At` | string | now | 结算时刻，`RFC3339` 或 `HH:MM`（按 Asia/Shanghai 当天） |

横轴语义：`L = 上下文长度 = input + cache_read`；
`cache_read = round(L × HitRate)`、`input = L − cache_read`、`output = round(input ÷ InOutRatio)`。

### 3.3 响应字段

顶层：

| 字段 | 类型 | 说明 |
|---|---|---|
| `At` | string | 实际使用的结算时刻（RFC3339） |
| `TimeZone` | string | 平台时区（`Asia/Shanghai`） |
| `R` | int64 | 积分换算系数 |
| `Step` / `MaxTokens` | int64 | 实际生效的采样参数（可能与入参不同：已收敛/截断） |
| `PointCount` | int | 每条线的点数 |
| `HitRate` / `InOutRatio` | float64 | 实际生效的拆分参数 |
| `TimeOptions` | `TimeOption[]` | 时段下拉可选项 |
| `DefaultTimeCoeff` | float64 | 时段配置的兜底系数 |
| `Sale` / `Cost` | `Line` | 两条线，**恒为对象**，缺配置时 `Available=false`+`Reason`，不返回 null |

`TimeOption`：`Name` / `Start` / `End` / `Coeff`（来自全局 `billing.time_config` 的周期段，按 Name+Start+End 去重；
无周期段时给一个 `全天`）。

`Line`：

| 字段 | 类型 | 说明 |
|---|---|---|
| `Available` | bool | 该线是否可用（缺费率/模型不存在 → false，并给 `Reason`） |
| `Source` | string | `external_model`（售价线）/ `channel_model`（成本线） |
| `Reason` | string | 不可用原因（可用时为空串） |
| `Name` | string | 售价线 = 对外模型名；成本线 = 内部模型 ID |
| `ExternalModelID` / `ChannelModelID` / `ChannelID` | int64 | 维度 ID |
| `ChannelName` / `InternalModelID` | string | 渠道名 / 内部模型 ID |
| `Rates` | object | `{Input, CacheRead, Output}`（PascalCase，与其它接口一致） |
| `TimeZone` / `TimeCoeff` | string/float64 | 实际时区与该时刻的时段系数 |
| `TimeConfigSource` | string | `model` / `global` |
| `TierSource` | string | `model` / `global` / `none` |
| `Tiers` | `Tier[]` | 生效的分档 `{Min, Max(可 null), Coeff}` |
| `Points` | `Point[]` | 逐 token 曲线点 |
| `TierBreaks` | `TierBreak[]` | 跳档拐点（供 markLine） |
| `OverTier` | `Range[]` | 超档区段（供断线红标） |

`Point`：

| 字段 | 类型 | 说明 |
|---|---|---|
| `Tokens` | int64 | 上下文长度（横轴） |
| `InputTokens` / `CacheReadTokens` / `OutputTokens` | int64 | 本点的三段 token 拆分 |
| `CtxCoeff` | float64 | 该点命中的分档系数（超档为 0） |
| `Credits` | float64 | **含系数**序列：`Σ × timeCoeff × ctxCoeff ÷ R` |
| `CreditsNoCoeff` | float64 | **不含系数**序列：`Σ ÷ R`（前端「关掉系数」对比线） |
| `OverTier` | bool | 落在分档之外：真实计费会失败，前端断线并标红（此时 `Credits=0`，不编造金额） |

`TierBreak`：`Tokens`（下一档起算点，= 下一档 `Min`）/ `FromCoeff` / `ToCoeff`。
`Range`：`FromTokens` / `ToTokens`。

### 3.4 边界行为

| 场景 | 行为 |
|---|---|
| 两个模型都没指定 | `Sale`/`Cost` 均 `Available=false` + `Reason`，仍返回 `TimeOptions`（不报错、不返 null） |
| 装不到 DB | 同上，200 响应 |
| 模型无费率（三段全 0） | `Available=false`，`Reason=未配置费率` |
| 未配分档 | `Tiers=[]`、`TierSource=none`、`ctxCoeff≡1`，无拐点无超档 |
| 分档留缝 | 缝内点 `OverTier=true`，并汇总成 `OverTier` 区段 |
| 最后一档无上限 | 横轴内无超档 |
| 时段配置非法（时区不可加载） | 500 + 明确文案（配置问题，不静默出错数据） |
| 点数过多 | 自动截断到 2000 点，`MaxTokens` 回报实际生效值 |

---

## 4. 分档配置样例（测试/演示环境）

`billing.context_tiers` 当前是 `[]`（= 不分档），模型级 `context_tiers` 全为 NULL，
所以**看不出跳档拐点、也验不出超档**。样例见：

```
backend/scripts/demo-context-tiers.sql
```

内容：4 档 3 拐点

| 区间（输入+缓存读） | 系数 |
|---|---|
| 0 ~ 8000 | 1.0 |
| 8001 ~ 32000 | 1.2 |
| 32001 ~ 128000 | 1.5 |
| 128001 ~ 无上限 | 2.0 |

- 文件里同时给出「回滚成不分档」与「只给单个模型配档」的写法。
- **不会自动执行**：程序启动不跑它，`internal/seeding/seed.go` 的默认值仍是 `[]`。
- **生产要不要配、配几档、系数多少由 spencer 拍板，运维执行**；配错等于改价。
- 生效方式：`billing.ConfigProvider` 对分档有内存缓存，改完需重启服务（或触发缓存失效）。

---

## 5. 时区与兼容

- 统一 **Asia/Shanghai**：`At=HH:MM` 按当天北京时间解释；RFC3339 输入会先换算到北京时间再算时段系数；
  日界/时段边界与计费链路同源（`billing.TimeCoeffConfig.CoeffFor`）。
- 既有接口参数与字段语义不变（`/cost/stats` 的老字段全部保留，只是行按实体合并；
  `/cost/curves`、`/cost/session` 一行没动）。
- 新增内容全部为新增字段 / 新增端点；老调用方不传新参数时行为可预期（见 §2、§3 的默认值）。
- 本轮**不做**（spencer 已定后置）：`billing_records` 加 ID 列与全量回填、会话成本按 user 轮重新定义。
