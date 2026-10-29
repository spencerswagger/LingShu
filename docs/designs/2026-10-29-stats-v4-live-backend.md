# 统计页 v4 · 实时区口径修正与卡片字段（后端第一棒）

- 日期：2026-10-29
- 范围：`backend/internal/domain/console/live.go`（实时区）、`backend/internal/domain/console/live_test.go`（单测）
- 对应 v4 六条中的两条（后端部分）：**第 4 条 实时区过滤口径**、**第 5 条 卡片化补数据字段**。
  纯前端三条（吞吐量 K/M 缩写、小指标行改缓存命中率+输入输出比、模型广场瘦身去默认 Token 排序）不在本文范围。
- 串行链：Core 后端（本文）→ View 前端 → Gate 验收 → Owl 评审。
- 硬约束：接口向后兼容（老字段不删、类型不变）、不动统计口径（KPI / Usage / ModelPlaza 一行未动）、
  不动 ServiceStatus、不引入新依赖、不做单位换算与删字段。

---

## 1. 口径（第 4 条）

| 区块 | v4 口径 | 实现 |
|---|---|---|
| 我的会话 | 请求中 **或** 最近 5 分钟内有请求 | 注册表 `Inflight > 0 \|\| LastActive >= now-5min`，且 `Closed=false` |
| 在线用户 | 页面右上角时间范围内**所有有过请求的用户**（总计口径） | `billing_records` 按 `[From, To]` 分用户聚合 |
| `UsersTotal` | 与 `Users` 列表**同一查询、同一口径** | `UsersTotal` = 该查询的未截断行数；列表取前 100 行（`UsersTruncated` 标记） |

要点：

- 注册表会把会话保留到 TTL 过期（默认 5 分钟，档位更长），所以「空闲会话」仍在内存里；
  只看 `Inflight>0` 会让刚发完一轮的会话立刻从列表消失，必须叠加 `LastActive` 判定。
  5 分钟窗口常量 `liveActiveSessionLookback`。
- 在线用户**不再由「请求中会话」反推**（旧实现 `UsersTotal = 请求中会话的用户数`），改为直接查 DB。
  否则「范围内活跃但此刻空闲」的用户全部隐身，且总数会随请求瞬时抖动。
- 总数与列表同源是结构保证而非约定：一次 `GROUP BY user_id` 查询既出行也出总数，
  不存在「总数一个口径、列表另一个口径」的可能。
- 在线用户以 `billing_records` 落账为准：首次请求仍在进行中、尚未落账的用户本轮不计入（下轮落账后进入）。

## 2. 新增查询参数（`/stats/live`）

| 参数 | 取值 | 默认 | 说明 |
|---|---|---|---|
| `From` | RFC3339 | `now-24h` | 在线用户筛选范围起点（页面右上角时间范围） |
| `To` | RFC3339 | `now` | 范围终点 |

- 缺省 = 近 24h（对齐页面默认预设）；`From > To` → 400；跨度 > 366 天 → 400；格式非法 → 400。
- 与既有 `Window`（TPM/消耗的滑动窗口，默认 60 分钟）**刻意分开**：
  `Window` 决定「吞吐/消耗算多久」，`From/To` 决定「哪些用户算在线」。
- 响应新增 `RangeFrom` / `RangeTo` 回显实际生效范围（前端窗口标签读它，不要自己拼）。

## 3. 新增响应字段（第 5 条）

顶层（`Data`）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `RangeFrom` / `RangeTo` | string | 在线用户筛选范围（RFC3339） |
| `DailyFrom` / `DailyTo` | string | 每用户日活跃序列覆盖的日期区间（`YYYY-MM-DD`，北京时间日界） |
| `DailyPoints` | int | 日活跃序列点数（= 每个用户 `Daily` 的长度） |

`Sessions[]`（我的会话卡片）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `TotalTokens` | int64 | **新增**：会话累计 Token（三段求和），补卡片「累计」缺项 |
| `CreatedAt` | string | **新增**：会话创建时间 RFC3339（注册表未记录时为空串），卡片可算会话时长 |

`Users[]`（在线用户卡片）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `ActiveSessions` | int | **新增**：所选范围内有过请求的会话数（`count(DISTINCT session_id)`） |
| `Calls` | int64 | **新增**：所选范围内请求数 |
| `RangeTokens` | int64 | **新增**：所选范围内 Token 合计 |
| `RangeCredits` | float64 | **新增**：所选范围内消耗积分合计 |
| `Daily` | `int64[]` | **新增**：日活跃序列（逐日 Token，长度 = `DailyPoints`，无请求日补 0，时间正序），供迷你曲线 |
| `Sessions` / `Tokens` / `TPM` / `SpentCredits` / `Spark` | 原字段 | 见 §4 口径变更 |

排序：`RangeTokens` 倒序 → `Tokens` 倒序 → 用户 ID 升序（刷新时行序稳定）。

## 4. 老字段口径变更（**字段名与类型不变**，语义按 v4 要求更新）

| 字段 | 旧口径 | 新口径 |
|---|---|---|
| `SessionsTotal` | 请求中会话数 | 请求中 **+** 最近 5 分钟内有请求的会话数（`Sessions[].Inflight` 可能为 0） |
| `UsersTotal` | 有请求中会话的用户数 | 所选范围内有过请求的用户数（与 `Users` 同源未截断） |
| `Users[].Tokens` / `SpentCredits` / `Spark` | 只算该用户「请求中会话」的近 `Window` 用量 | 该用户**全部会话**的近 `Window` 用量（人群放宽，窗口口径不变） |
| `Users[].Sessions` | 该用户请求中会话数 | 不变（仍只数 `Inflight>0` 的会话；范围内活跃但空闲的用户为 0） |

> 前端注意：`UsersTotal` / `SessionsTotal` 会**变大**（口径变宽），不是回归。
> 卡片上「请求中」相关文案请优先用 `Sessions[].Inflight`、`Users[].Sessions`，不要用总数兜。

## 5. 性能与上限

| 项 | 上限 | 说明 |
|---|---|---|
| 会话参与聚合 | 500 条（`liveMaxSessions`） | 沿用既有防护 |
| 返回会话行 | 100 条（`Limit`） | 沿用 |
| 返回用户行 | 100 条（`liveMaxUsers`） | `UsersTruncated` 标记；曲线/序列只为返回的行计算 |
| 日活跃序列点数 | 92 天（`liveMaxDailyPoints`） | 自定义超长区间只回最近 92 天，`DailyFrom` 如实回报起点 |
| 范围跨度 | 366 天 | 与 dashboard 同口径 |

单次请求的 DB 查询数：会话聚合 ≤2 + 用户聚合 1 + 名称 1 + 分钟曲线 1 + 日序列 1（后三条仅在用户列表非空时执行）。
用户聚合只在 `call_time` 上过滤（走 `idx_billing_call_time`），30s 刷新压力与改动前同量级。

## 6. 边界与降级

| 场景 | 行为 |
|---|---|
| 没有活跃会话 | `Sessions=[]`，但**仍查**在线用户（用户不再依赖会话） |
| 范围内没有用户 | `Users=[]`、`UsersTotal=0`，不再执行名称/曲线/日序列查询 |
| 会话注册表未装配 | 降级为空结果（沿用）；`DailyPoints` / 范围仍回显 |
| DB 未装配 | 会话列表仍返回（含 `Inflight`），数值列 0；在线用户为空 |
| dev 端 | 只统计本人：`Scope=self`，`Users` 只有本人一行（范围内无请求则为空） |
| 日序列无数据 | 全 0 定长序列（迷你曲线语义：0 = 没用量，不是断线） |

## 7. 单测（`live_test.go`）

| 用例 | 守什么 |
|---|---|
| `TestParseLiveParams` | `From/To` 缺省近 24h、宽松 `+` 解析、倒序/超 366 天/格式错 400 |
| `TestBuildLiveStats_SessionRecencyWindow` | 5 分钟窗口：请求中保留、4 分钟前保留、6 分钟前剔除、已关闭剔除 |
| `TestBuildLiveStats_UsersFromDBRange` | 会话为空也有在线用户；`UsersTotal == len(Users)`；`Daily` 长度/补零/归位 |
| `TestBuildLiveStats_UsersTruncated` | 101 用户 → `UsersTotal=101`、列表 100、`UsersTruncated=true` |
| `TestBuildLiveStats_SanitizedAndAggregates` | 字段白名单（新增字段必须登记）+ 内容面字段零泄露 + 聚合值落位 |
| `TestBuildLiveStats_DevScopeIsolation` | dev 端只看本人 |
| `TestHandleStatsLive_HTTP` | 两端点 `{Code,Data}`、scope、范围回显、`Window` 与 `From/To` 非法值 400 |

## 8. 交给前端（View）的注意点

1. 请求在线用户时要带页面右上角的 `From`/`To`（RFC3339），否则默认近 24h；标签请读 `RangeFrom`/`RangeTo`。
2. 用户卡迷你曲线读 `Users[].Daily`，横轴日期由 `DailyFrom` + `DailyPoints` 推（逐日，无请求日 = 0）。
3. 用户卡「当前请求中」用 `Users[].Sessions`；「范围请求数/范围花费」用 `Calls`/`RangeCredits`/`RangeTokens`。
4. 会话卡「累计 Token」用 `TotalTokens`（累计积分仍是 `SpentCredits`）；`CreatedAt` 可算会话时长。
5. `ContextLimit` 仍恒为 0（平台无模型上下文窗口配置来源，属已知缺口，另开任务）。
6. `frontend/src/api/stats.ts` 的 `StatsLive*` 类型需补本节新字段（纯前端改动，归 View）。
