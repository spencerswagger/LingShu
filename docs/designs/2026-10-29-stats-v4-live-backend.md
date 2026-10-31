# 统计页 v4 · 实时区口径修正与卡片字段（后端第一棒）

- 日期：2026-10-29
- 范围：`backend/internal/domain/console/live.go`（实时区）、`backend/internal/domain/console/live_test.go`（单测）
- 对应 v4 六条中的两条（后端部分）：**第 4 条 实时区过滤口径**、**第 5 条 卡片化补数据字段**。
  纯前端三条（吞吐量 K/M 缩写、小指标行改缓存命中率+输入输出比、模型广场瘦身去默认 Token 排序）不在本文范围。
- 串行链：Core 后端（本文）→ View 前端 → Gate 验收 → Owl 评审。
- 硬约束：接口向后兼容（老字段不删、类型不变）、不动统计口径（KPI / Usage / ModelPlaza 一行未动）、
  不动 ServiceStatus、不引入新依赖、不做单位换算与删字段。

---

## 1. 口径（第 4 条，spencer 拍板后修订 rev.2）

| 区块 | 口径 | 实现 |
|---|---|---|
| 我的会话（**准入**） | **仅请求中**（`Inflight > 0`） | 注册表 `Inflight > 0 && Closed=false`；与 `origin/main` 一致 |
| 我的会话（**淡出**） | 刚结束请求的会话最多停留 5 分钟，**只做视觉过渡** | 独立数组 `RecentlyExited[]`（`ExitedAt` + `FadeWindowSeconds`），上限 20 行 |
| 在线用户（**旧字段 `Users`/`UsersTotal`**） | **仅由「请求中会话」反推**（与 `origin/main` 一致） | 注册表内存计数；窗口用量只汇总请求中会话 |
| 范围内活跃用户（**新字段 `RangeUsers`/`RangeUsersTotal`**） | 页面右上角时间范围内**所有有过请求的用户** | `billing_records` 按 `[From, To]` 分用户聚合；列表取前 100 行 |

要点：

- **准入不是「5 分钟活跃」**：`Inflight > 0` 才进 `Sessions[]`、才计入 `SessionsTotal` / `Truncated` /
  会话聚合查询。曾经把 `LastActive >= now-5min` 当第二类准入条件，会同时抬高会话数与用户会话数，
  已按拍板撤回。
- 注册表会把会话保留到 TTL 过期（默认 5 分钟，档位更长），所以「刚结束的会话」仍在内存里；
  这些会话**只**进 `RecentlyExited`（窗口常量 `liveFadeWindow`），供前端把卡片淡出再移除。
  它们**不进主列表、不进任何计数、不进任何金额/Token 求和、不进聚合 SQL**（单测用
  `WithArgs("inflight", …)` 锁死：查询参数里只有请求中的会话 ID）。
- **rev.3（spencer 拍板，向后兼容）**：`Users` / `UsersTotal` / `Users[].Tokens` / `Sessions` / `TPM` /
  `Spark` **回到 `origin/main` 语义**（只由请求中会话反推），逐字节一致由 golden 快照锁住
  （`testdata/live_compat_*.golden.json`，golden 是在 #66 合入前的代码上落盘的）。
  「范围内活跃用户」那套口径**单列**到 `RangeUsers[]` / `RangeUsersTotal`，前端按需取用，两套不混。
- 两套口径各自「总数与列表同源」：旧字段的用户集来自会话注册表，新字段来自一次 `GROUP BY user_id`
  查询（既出行也出总数），各自都不会出现「总数一个口径、列表另一个口径」。
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
| `RecentlyExited` | object[] | **新增（rev.2）**：淡出窗口内已结束请求的会话，仅 `ID` / `Name` / `Model` / `ExitedAt`；**不参与任何计数与聚合** |
| `FadeWindowSeconds` | int | **新增（rev.2）**：淡出窗口长度（秒，当前 300），前端据此排移除定时器 |
| `RangeUsersTotal` | int | **新增（rev.3）**：范围内有过请求的用户数（未截断）；与 `RangeUsers` 同源 |
| `RangeUsers` | object[] | **新增（rev.3）**：范围内活跃用户行（见下表） |

`Sessions[]`（我的会话卡片）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `TotalTokens` | int64 | **新增**：会话累计 Token（三段求和），补卡片「累计」缺项 |
| `CreatedAt` | string | **新增**：会话创建时间 RFC3339（注册表未记录时为空串），卡片可算会话时长 |

`Users[]`（在线用户卡片，**旧字段集与 `origin/main` 完全一致，rev.3 撤回全部新增字段**）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `ID` / `Name` / `Sessions` / `Tokens` / `TPM` / `SpentCredits` / `Spark` | 原字段 | 语义与取值均与 main 逐字节一致（golden 锁定） |

> rev.3：原先塞进 `Users[]` 的范围字段（`ActiveSessions` / `Calls` / `RangeTokens` / `RangeCredits` / `Daily`）
> 已整体迁到新字段 `RangeUsers[]`（`RangeSessions` / `RangeCalls` / `RangeTokens` / `RangeCredits` / `Daily`）。

排序（`RangeUsers`）：`RangeTokens` 倒序 → `WindowTokens` 倒序 → 用户 ID 升序（刷新时行序稳定）。

`RangeUsers[]`（范围内活跃用户卡片，rev.3 单列）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `ID` / `Name` | string | 同上 |
| `RangeSessions` | int | 范围内有过请求的会话数（去重） |
| `RangeCalls` | int64 | 范围内请求数 |
| `RangeTokens` | int64 | 范围内 Token 合计 |
| `RangeCredits` | float64 | 范围内消耗积分合计 |
| `WindowTokens` / `SpentCredits` / `TPM` | int64 / float64 / float64 | 该用户**近 Window 分钟**用量（覆盖其全部会话） |
| `Spark` | int64[] | 逐分钟 Token（长度 = `SparkPoints`，无用量分钟补 0） |
| `Daily` | int64[] | 日活跃序列（长度 = `DailyPoints`，无请求日补 0） |

## 4. 老字段口径变更（**字段名与类型不变**，语义按 v4 要求更新）

| 字段 | 旧口径 | 新口径 |
|---|---|---|
| `SessionsTotal` | 请求中会话数 | **保持不变**（仅 `Inflight>0`）；淡出项不计入 |
| `UsersTotal` | 有请求中会话的用户数 | **保持不变**（rev.3 撤回范围口径，范围数据迁到 `RangeUsersTotal`） |
| `Users[].Tokens` / `SpentCredits` / `Spark` | 只算该用户「请求中会话」的近 `Window` 用量 | **保持不变**（rev.3 撤回） |
| `Users[].Sessions` | 该用户请求中会话数 | 不变 |
| `Sessions[].TotalTokens` / `CreatedAt` | — | 纯新增字段（rev.1），不影响旧字段取值 |

> 前端注意（rev.3）：旧字段**全部回到 main 语义**，本次是纯增量——新增 `RangeUsersTotal` / `RangeUsers[]`
> 承载「范围内活跃用户」，新增 `RecentlyExited[]` 承载淡出。老页面不改也照常工作。

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
| 范围内没有用户 | `RangeUsers=[]`、`RangeUsersTotal=0`，不再执行名称/曲线/日序列查询 |
| 没有请求中会话 | `Users=[]`、`UsersTotal=0`（与 main 一致），但 `RangeUsers` 照常返回 |
| 会话注册表未装配 | 降级为空结果（沿用）；`DailyPoints` / 范围仍回显 |
| DB 未装配 | 会话列表仍返回（含 `Inflight`），数值列 0；在线用户为空 |
| dev 端 | 只统计本人：`Scope=self`，`Users` 只有本人一行（范围内无请求则为空） |
| 日序列无数据 | 全 0 定长序列（迷你曲线语义：0 = 没用量，不是断线） |

## 7. 单测（`live_test.go`）

| 用例 | 守什么 |
|---|---|
| `TestParseLiveParams` | `From/To` 缺省近 24h、宽松 `+` 解析、倒序/超 366 天/格式错 400 |
| `TestBuildLiveStats_SessionRecencyWindow` | 5 分钟窗口：请求中保留、4 分钟前保留、6 分钟前剔除、已关闭剔除 |
| `TestBuildLiveStats_SessionRecencyWindow`（rev.2 重写） | 准入只认 `Inflight>0`：4 分钟前结束的会话**不进**主列表，只进 `RecentlyExited`；6 分钟前结束的两边都不进；`SessionsTotal == len(Sessions)` |
| `TestBuildLiveStats_FadingNotAdmittedAndNotCounted`（rev.2 新增） | 淡出项绝不参与计数：会话聚合/最近请求两条 SQL 的 `WithArgs` 只接受 `"inflight"`；`RecentlyExited` 按最近活跃倒序、只含窗口内条目；`Users[].Sessions` 只数 inflight |
| `TestLiveCompatGolden`（rev.3 新增） | **旧字段逐字节向后兼容**：三组场景（三个请求中会话 / 一个在途+两个刚结束 / 全空闲）的 `SessionsTotal`/`UsersTotal`/`Truncated`/`UsersTruncated`/`Users` 与 `Sessions`（投影到旧字段集）与 #66 合入前一致；golden 由改动前代码落盘 |
| `TestBuildLiveStats_RangeUsersFromDBRange`（rev.3 改名） | 没有请求中会话时旧 `Users` 恒空/0；`RangeUsers` 来自 DB 范围、`RangeUsersTotal == len(RangeUsers)`；`Daily` 长度/补零/归位 |
| `TestBuildLiveStats_RangeUsersTruncated`（rev.3 改名） | 101 个范围用户 → `RangeUsersTotal=101`、列表 100；旧 `Users` 不受影响 |
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
7. **两套用户口径（rev.3）**：`Users[]` 是「此刻有请求中会话」的用户（旧字段，语义不变）；
   `RangeUsers[]` 是「所选范围内活跃过」的用户（新字段）。要展示「范围内活跃」请读后者，
   两者人数不同是正常的，别互相兜底。
8. **淡出（rev.2）**：`Sessions[]` 只含请求中的会话；收到 `RecentlyExited[]` 时把这些卡片的节点
   标记为「离开中」并开始淡出动画，`ExitedAt + FadeWindowSeconds` 之后把它从 DOM 移除
   （服务端下一轮就不会再返回它）。**不要**把 `RecentlyExited` 计入会话数/用户数，也不要拿它做排序。
