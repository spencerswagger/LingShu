# 统计页后端接口（小时趋势 / 四日对比 / 模型广场 / 实时区）· 接口与口径说明

- 日期：2026-10-25（配合前端版式 `feat/stats-dashboard-slim`，commit ef36d52）
- 范围：`internal/domain/billing/stats.go`（聚合）、`internal/domain/console/stats.go`（参数与装配）、
  `internal/domain/console/live.go`（实时区）、`internal/server/server.go`（路由）。
- 不变的东西：既有参数 `From`/`To`、既有响应字段（`KPI` / `Usage.Daily` / `Usage.ByModel` 等）的
  **语义与统计口径一律不改**。新能力全部靠「新增可选参数 + 新增响应字段」承载，不带新参数的调用行为与改动前一致。

---

## 1. 接口清单

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/admin/stats/dashboard` | 管理端全局统计（含运维块 `Ops`） |
| GET | `/api/v1/dev/stats/dashboard` | 开发端本人统计（`Scope=self`，无 `Ops`） |
| GET | `/api/v1/admin/stats/live` | **新增**：管理端实时区（全部请求中会话 + 在线用户） |
| GET | `/api/v1/dev/stats/live` | **新增**：开发端实时区（仅本人会话 + 本人汇总） |

`/stats/dashboard` 负责「按范围重算」的部分；`/stats/live` 负责「每 30s 刷新」的实时部分，
两者刻意分开：实时刷新不应触发整页 9 条历史聚合。

---

## 2. `/stats/dashboard` 新增可选参数

| 参数 | 取值 | 默认 | 说明 |
|---|---|---|---|
| `Granularity` | `auto` \| `hour` \| `day` | 缺省视为 `auto` | 出桶粒度。`auto`：跨度 ≤ 26 小时按小时，否则按天。显式 `hour` 但跨度 > 2 天时**自动降级为 `day`（不报错）**——前端漏传 `From`/`To` 也不会拿到 400。显式 `day` = 老口径。 |
| `Window` | `exact` \| `calendar` | `exact` | **KPI 窗口模式**（见 §2.0）。`exact`：首屏三个大数字 / 小时趋势 / 模型广场严格按 `From`~`To`（近 24h 就是滚动 24 小时）；`calendar`：退回日界归一（改造前行为，给需要旧数字的调用方）。 |
| `CompareDays` | 0~7 | `0`（关闭） | 同期对比天数（含今天）。前端四线图传 `4`。 |
| `ModelSort` | `tokens` \| `tpm` \| `failed` \| `latency` | `tokens` | 模型广场排序键。非法值回落 `tokens`（不报错）。 |
| `ModelLimit` | 1~200 | `50` | 模型广场返回行数上限，超出按上限收敛。 |

参数错误一律 400（`From`/`To` 格式、`Granularity` 非法、`CompareDays`/`ModelLimit` 非数字），
越界数值直接收敛到上下限（不报错）。

### 2.0 窗口口径（KPI / 趋势 / 模型广场同一窗口）

**两套窗口刻意分开**（PR #61 Owl P2 修正）：

| 窗口 | 覆盖范围 | 谁用 |
|---|---|---|
| **KPI 窗口** | 默认 = 请求的 `From`~`To` 原始区间（滚动窗口，不做日界归一） | 首屏三个大数字（`KPI`）、`Usage.Hourly`（趋势）、`ModelPlaza` |
| **日界窗口** | `From`/`To` 按 Asia/Shanghai 归一整天（`00:00:00`~`23:59:59`） | `Usage.Daily` 与既有按天/维度聚合字段（`ByModel`/`ByKey`/…/延迟分位） |

- 改造前首屏数字走日界窗口，标签写「近 24h」却覆盖最长 48 小时（同一请求出现过 36240 vs 23880）。
  现在默认走 KPI 窗口，标签与数字一致；`Window=calendar` 可退回旧行为。
- 自定义区间同理：`From`/`To` 传什么就算什么，不再偷偷扩到自然日边界。
- 响应里新增 `KPI.WindowFrom` / `KPI.WindowTo` / `KPI.WindowMode`，标明这三个数字实际覆盖的区间——
  前端窗口标签请直接读它们，不要自己拼。
- 环比（`Delta*`）也改为「KPI 窗口等长的前一窗口」，与首屏数字同源。
- 不带 `From`/`To` 时默认区间仍是「now 往前 29×24h」（与改造前默认跨度一致）；前端各预设都显式传 `From`/`To`。

### 2.1 `Usage` 块新增字段（老字段不动）

| 字段 | 类型 | 说明 |
|---|---|---|
| `Usage.Granularity` | string | 实际生效粒度 `hour` \| `day` |
| `Usage.Hourly` | `DashHour[]` | 小时桶；`day` 粒度下为 `[]`（不是 `null`） |
| `Usage.Compare` | `DashSeries[]` | 同期对比序列；`CompareDays=0` 时为 `[]` |

`DashHour`：

| 字段 | 类型 | 说明 |
|---|---|---|
| `Hour` | string | 桶起点，`2006-01-02T15:04`（北京时间墙钟），如 `2026-10-25T14:00` |
| `Day` | string | 所属自然日 `2006-01-02`（北京时间） |
| `HourOfDay` | int | 0~23，跨日对齐用 |
| `Calls` / `Failed` | int64 | 请求数 / 失败数 |
| `Credits` | float64 | 积分 |
| `Tokens` | int64 | Token（三段求和：input + cache_read + output） |
| `DurationMS` | int64 | 耗时之和（毫秒） |
| `SuccessRate` | float64 | 0~1（`Calls=0` 时为 0） |

`DashSeries`：

| 字段 | 类型 | 说明 |
|---|---|---|
| `Day` | string | 该序列所属自然日 |
| `Label` | string | `今天` / `昨天` / `前天` / `大前天`；更早回落 `MM-DD` |
| `Offset` | int | 相对今天的回溯天数：0=今天，1=昨天… |
| `Points` | `DashHour[]` | **只含有数据的小时桶**，按 `HourOfDay` 升序；缺失小时**不出行**（前端 `connectNulls:false` 断线） |

> 缺数据不补 0：`Points` 里没有某小时 = 那小时没有请求；有该小时且 `Tokens=0` = 有请求但没产生 Token。
> 两者语义必须区分，前端不要自行 fillna。

### 2.2 新增顶层块 `ModelPlaza`

| 字段 | 类型 | 说明 |
|---|---|---|
| `Sort` | string | 实际生效排序键 |
| `Total` | int | 对外模型目录总数（未截断） |
| `Active` | int | 窗口内有请求的模型数 |
| `Limit` | int | 本次返回上限 |
| `Truncated` | bool | `Total > len(List)`，还有模型没返回 |
| `List` | `DashModelStat[]` | 行数据 |

`DashModelStat`：

| 字段 | 类型 | 说明 |
|---|---|---|
| `Model` / `Label` | string | 对外模型名（两者同值） |
| `Enabled` | bool | 对外模型是否启用 |
| `Calls` / `Failed` / `FailRate` | int64/int64/float64 | 请求数 / 失败数 / 失败率 0~1 |
| `Tokens` | int64 | 窗口内 Token（三段求和） |
| `Credits` | float64 | 窗口内积分 |
| `TPM` | float64 | 忙时口径吞吐（见 §4） |
| `BusyCalls` / `BusyMS` | int64 | 忙时口径的分子（成功且带耗时请求数）/ 分母（同批耗时之和） |
| `AvgMS` / `AvgFirstMS` | float64 | 全程平均耗时 / 首字平均耗时 |
| `Active` | bool | 窗口内是否有请求（`Calls>0`），**前端据此折叠空闲模型** |
| `Unmatched` | bool | **新增**。该行是「解析不到归属」的兜底行（`Model="__unmatched__"`），见下方说明 |

零用量模型（`Calls=0, Active=false`）排在有请求的模型之后。

**归属口径（PR #61 Owl P2 修正）**：用量按**实体**归到对外模型，不再拿账单里的模型名去撞目录名。
账单行先用 `(channel_id, internal_model_id)` 反查 `channel_models` 拿到 `external_model_id`，再按 ID 归属——
模型改名、用别名调用都仍归到对应模型（改造前这部分用量会在广场里整块消失）。

确实解析不到的用量不会被丢掉，而是汇总成**一行显式条目**：

| 字段 | 值 | 说明 |
|---|---|---|
| `Model` | `"__unmatched__"` | 固定哨兵串 |
| `Label` | `"未匹配模型"` | 展示名 |
| `Unmatched` | `true` | **新增字段**，前端据此识别（不必硬编码 Model 串） |
| `Enabled` | `false` | 不对应任何在册模型 |

- **未匹配的判定是「不在册」，不是「没解析到」**：`agg` 里算 `in_catalog`（解析到的对外模型仍 `deleted_at IS NULL`），
  未匹配汇总用 `NOT in_catalog`。所以下面三种形态都有确定归属，没有第三条路：
  - 改名 / 别名 → 归到同一对外模型（走实体解析），进目录行；
  - 换绑（`channel_models.external_model_id` 改了）→ 按当前绑定归属，进目录行；
  - 解析不到（`ext_id=0`）**或解析到的模型已被软删** → 进「未匹配」行。
  只兜 `ext_id=0` 会让「软删模型的历史用量」两头落空（目录 JOIN 用 `deleted_at IS NULL` 把它滤掉、未匹配也不收），
  属静默丢量，PR #61 Gate 复验抓到过。
- 未匹配行**不参与** `Total` / `Active`（那两个是「对外模型目录」口径），也**不受 `Limit` 截断**（否则就是静默丢量）；
  它恒排在 `List` 末尾。
- 守恒：`List` 各行 `Calls` / `Failed` / `Tokens` 之和 == 同窗口总量（含未匹配行）。
  单测 `TestDashboardModels_UnmatchedRowAppended`、集成测试 `TestStats_WindowAndPlaza` 守这条。

---

## 3. `/stats/live`（实时区）

> **2026-10-29 v4 增补**：实时区口径与字段有更新（我的会话 = 请求中 + 5 分钟内活跃；在线用户改为
> 查 DB 范围内活跃用户，`UsersTotal` 与列表同源；新增 `From`/`To` 与卡片字段 `TotalTokens`/`CreatedAt`/
> `ActiveSessions`/`Calls`/`RangeTokens`/`RangeCredits`/`Daily`）。以
> [`2026-10-29-stats-v4-live-backend.md`](./2026-10-29-stats-v4-live-backend.md) 为准，本节以下为改造前口径。

| 参数 | 取值 | 默认 | 说明 |
|---|---|---|---|
| `Window` | 1~1440（分钟） | `60` | TPM 与消耗的统计窗口 |
| `SparkPoints` | 1~60 | `12` | 曲线采样点数 |
| `Limit` | 1~100 | `20` | 返回的会话行数上限 |

响应（`Data`）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `Scope` | string | `global`（admin）/ `self`（dev） |
| `SampledAt` | string | 采样时刻 RFC3339 |
| `WindowMinutes` | int | 实际生效窗口 |
| `SparkPoints` | int | 实际生效采样点数 |
| `SessionsTotal` | int | 请求中会话总数（未截断） |
| `UsersTotal` | int | 在线用户总数 |
| `Truncated` | bool | 会话列表是否被 `Limit` 截断 |
| `UsersTruncated` | bool | 用户列表是否被上限（100）截断 |
| `Sessions` | `LiveSession[]` | 「我的会话」行 |
| `Users` | `LiveUser[]` | 「在线用户」行 |

`LiveSession`：

| 字段 | 类型 | 说明 |
|---|---|---|
| `ID` | string | 会话指纹（**不是**客户端原始 `x-session-id`） |
| `Name` | string | 会话可读名（可能为空，前端用 ID 前缀兜底） |
| `Model` | string | 对外模型名 |
| `Inflight` | int | 进行中请求数（>0 才会出现在列表里） |
| `ContextUsed` | int64 | 上下文长度 = 最近一次请求的输入 Token（input + cache_read） |
| `ContextLimit` | int64 | 上下文上限；平台未配置模型上限时**恒为 0**（=未知，前端显示不设上限/只显示已用） |
| `RoundTokens` | int64 | 本轮 Token = 最近一次**已完成**请求的总 Token |
| `Calls` | int64 | 会话累计请求数 |
| `WindowTokens` | int64 | 近 `Window` 分钟 Token（TPM 分子） |
| `TPM` | float64 | `WindowTokens ÷ Window` 分钟（墙钟平均） |
| `SpentCredits` | float64 | 会话累计消耗积分 |
| `LastActive` | string | 最近活跃 RFC3339 |
| `Spark` | `int64[]` | 最近 `SparkPoints` 次请求的 Token，**时间正序**，可能不足 N 个 |

`LiveUser`：

| 字段 | 类型 | 说明 |
|---|---|---|
| `ID` | string | 用户 ID（字符串，避免 JS 大整数精度丢失） |
| `Name` | string | 昵称优先，空则登录名 |
| `Sessions` | int | 该用户当前请求中的会话数 |
| `Tokens` | int64 | 近 `Window` 分钟 Token（只算该用户「请求中会话」的用量） |
| `TPM` | float64 | `Tokens ÷ Window` 分钟（墙钟平均） |
| `SpentCredits` | float64 | 近 `Window` 分钟消耗积分 |
| `Spark` | `int64[]` | 近 `SparkPoints` 分钟逐分钟 Token，**时间正序**，无请求分钟补 0 |

### 3.1 脱敏（字段层面收口，不靠前端自觉）

实时区 DTO **只有**上面这些字段，没有任何承载会话内容的字段：

- 不含消息 / 提示词 / 模型输入输出 / 工具调用参数；
- 不含客户端原始 `x-session-id`（只有哈希指纹）；
- 不含渠道密钥 / 上游地址；
- `Users` 只有汇总吞吐与消耗，**没有任何会话级字段**。

单测 `TestBuildLiveStats_SanitizedAndAggregates` 用字段白名单 + 禁用字段扫描守住这条线：
任何新增字段都必须显式登记进白名单，否则测试失败。

### 3.2 「请求中」的判定

以 `router.SessionRegistry` 的内存计数 `Inflight > 0` 为准（网关在包住上游转发时 +1/-1）。
DB 的 `sessions` 表是投影、不含 `Inflight`，**不能**用来判「正在请求」。
`SpentCredits` / `Calls` / `ContextUsed` / `Spark` 来自 `billing_records`，即**已完成**的那几轮；
正在进行中的这一轮要等落账后才计入 `RoundTokens`。

---

## 4. 三个吞吐口径（刻意区分，不要混用）

| 位置 | 字段 | 口径 |
|---|---|---|
| 首屏大数字 | `KPI.TPM` | **全窗口忙时口径**：窗口内`成功且带耗时`请求的 Token ÷ 同批请求耗时之和折算的分钟数（`billing.BusyRate`）。样本量看 `KPI.BusyRequests` / `KPI.SumDurationMS`。 |
| 模型广场吞吐列 | `ModelPlaza.List[].TPM` | **按模型的忙时口径**：该模型在窗口内 `成功且带耗时` 请求的 Token ÷ 同批耗时折算分钟数；`BusyMS=0` 时为 0。 |
| 实时会话 / 在线用户 | `Sessions[].TPM`、`Users[].TPM` | **墙钟窗口平均**：近 `Window`（默认 60）分钟 Token ÷ 窗口分钟数。含排队与空转时间，因此天然低于忙时口径。 |

前端展示时必须各自带窗口标签（近 1 分钟 / 近 10 分钟 / 近 60 分钟这类文案由前端决定，
但**数值来源**就是上表三行，不能互相替换）。

---

## 5. 时区与边界

- 统一 **Asia/Shanghai**。日界、小时界都由 `call_time AT TIME ZONE 'Asia/Shanghai'` 切分，
  与既有 `Usage.Daily` 的日界完全同源；请求传 UTC（`Z`）也按北京时间归属。
- 小时桶起点 = 该自然小时的整点。区间首尾不在整点时，首尾桶是**不完整桶**：
  只含区间内那部分记录，刻意不向整点外扩（否则会把区间外数据算进来）。
- `Usage.Daily` 仍按整天归一区间（`00:00:00`~`23:59:59` 北京时间）补零，老口径不变。
- 小时桶 / 同期对比用**请求原始区间**，不做整天归一：否则「近 24h」会被拉长到最多 48 小时，多出半天空桶。
- 同期对比的「今天」= `To`（缺省 now）所在的北京自然日；今天只有到当前时刻为止的桶。
- 会话「最近若干次请求」回溯上限 7 天（`liveRecentLookback`）：请求中的会话上一轮必然很近，
  该上限只用于防止按 `session_id` 扫出整段历史。

---

## 6. 边界处理

| 场景 | 行为 |
|---|---|
| 空窗口 | `Hourly` / `Compare[].Points` / `ModelPlaza.List` / `Sessions` / `Users` 均为 `[]`，不是 `null`；`SuccessRate`/`FailRate`/`TPM` 均为 0 |
| 单用户（dev） | 只统计本人；`Scope=self`；`Users` 只含本人一行 |
| 无在用会话 | `Sessions=[]`、`Users=[]`、`SessionsTotal=0`、`UsersTotal=0`，不查 DB |
| 会话注册表未装配 | 降级为空结果，不报错 |
| DB 未装配 | 会话列表仍返回（带 `Inflight`），数值列为 0 |
| 超大模型数 | `ModelLimit` 上限 200 + `Total`/`Truncated`；零用量模型排后面；「未匹配」行始终返回 |
| 改名 / 别名调用 | 靠实体解析归到同一对外模型，不再丢量 |
| 解析不到的用量 | 汇总成 `Unmatched=true` 的一行，不静默丢弃 |
| 解析到的模型被软删 | 同样进 `Unmatched` 行（判据是 `NOT in_catalog`），不静默丢弃 |
| 极高并发 | 单次参与聚合的请求中会话上限 500、在线用户上限 100（`UsersTruncated` 标记） |
| 参数越界 | 数值收敛到上下限；格式/枚举错误 400 |

---

## 7. 已知缺口（本轮不提供，前端继续占位）

1. `ContextUsed` 的**上限**：平台没有「模型上下文窗口」配置来源，`ContextLimit` 恒为 0。
   如需展示 `12k / 128k`，需要先加模型元数据字段（另开任务）。
2. 首屏大数字的「今日 / 本月」积分聚合：本轮未加，前端仍用「日均」。
3. 模型广场「首字 / 全程」排序：`ModelSort` 目前只有 `latency`（全程），未单独提供按首字排序。
4. 实时区不含「正在请求但尚未落账」的 Token 预估（`RoundTokens` 是上一轮真实值）。

---

## 8. 向后兼容

- 不传任何新参数：`Granularity` 按 `auto` 判定（跨度 ≤26h 会给小时桶，**新增字段**），
  `Compare` 为空数组，`ModelPlaza` 照常返回（新增顶层块）。
- **口径变化（默认行为）**：`KPI`（首屏三个大数字）从「日界归一窗口」改为「请求区间严格窗口」，
  数字可能与改造前不同（尤其区间首尾不在整点/日界时）。`Usage.Daily` 与各维度聚合字段口径不变；
  需要旧数字的调用方传 `Window=calendar`。
- `Granularity=hour` 遇长区间不再 400，改为自动按天（`Usage.Granularity` 会如实回报 `day`）。
  `KPI`、`Usage.Daily`、`Usage.ByModel/ByKey/ByUser/ByMode/Errors/Latency`、`Usage.Total*`、`Ops` 全部保持原语义，
  一条 SQL、一个计算公式都没改。
- 旧前端忽略新字段即可继续工作；`Usage.Hourly`/`Usage.Compare` 在无数据时是 `[]` 而非 `null`，不会打崩渲染。
- 新增的两个 `/stats/live` 路由不影响既有路由表。
