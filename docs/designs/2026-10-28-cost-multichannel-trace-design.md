# 成本页「多渠道对比 + 会话渠道溯源」后端设计

- 日期：2026-10-28
- 分支：`feat/cost-multichannel-trace`
- 范围：后端只读接口（不加表、不加列、不加依赖、不改计费公式、不改路由逻辑、不把引擎决策落库）
- 任务书：`docs/pending-tasks.md` 灵枢区「成本页多渠道对比 + 会话渠道溯源」

## 1. 为什么

成本页的定价曲线目前只能**单选一个实体**：一次请求出一条售价线 + 一条成本线。要看「同一个内部模型在
A/B/C 三家渠道上谁更便宜」，只能反复切换下拉、肉眼比对，无法同轴对照；而聊到「这通会话为什么后来
贵了」，运维只能翻账单明细自己数换没换过渠道。

## 2. 做了什么（两个只读能力，共用一个开关）

### 2.1 定价曲线多线对比

`GET /api/v1/admin/cost/pricing-curve` 新增**可选**参数 `ChannelModelIDs=a,b,c`（逗号分隔，最多 10 个）。

- 传了 → 响应多一个 `Lines` 数组，每行一个 `(渠道, 渠道内部模型)`，各带**自己的**成本线与售价线
  （`pricingLine` 结构与现有 `Sale`/`Cost` 完全同构：`Points` / `TierBreaks` / `OverTier` / `Rates` / `Available` / `Reason`）。
- **不传 → 响应与改造前逐字节一致**。这条是硬要求，用 golden 快照锁住，不靠「看起来没变」：
  `internal/domain/console/testdata/pricing_curve_*.golden.json` 是在**改动前的代码**上跑同一组场景直接落盘的响应字节，
  测试 `TestHandleCostPricingCurve_GoldenUnchanged` 逐字节比对（4 个场景：无 DB 无参数 / 渠道内部模型+采样参数 /
  只按对外模型名 / 非法参数 400）。
  实现上靠 `Lines []pricingMultiLine \`json:"Lines,omitempty"\`` —— nil 切片连键都不出现。

几个刻意的取舍：

- **参数合法但模型不存在**（或费率缺失）→ 给一条 `Available=false + Reason` 的线，**不 500、不整单失败**，
  与单线模式「选了不存在的模型给一条不可用线」口径一致；`ChannelModelID` 原样回吐，前端好对号。
- **`ChannelModelIDs` 里出现非法/重复/零/负数 → 400**；超过 10 个也 400（**不静默截断**）。
- **总点数上限仍是 2000**：线多了按线数等比下调横轴上限（`MaxTokens = Step × floor(2000/线数)`），
  保证响应体量与单线模式同一量级；实际用的 `MaxTokens` 在响应里回报，前端不用猜。
- 多线模式下缺省 `MaxTokens` 取**所有线**分档推导值的最大值，避免某条线因别家分档更小被提前截断。
- 单线模式的 `Sale`/`Cost`/`PointCount` 语义**完全不变**；多线模式额外把 `PointCount` 对齐到首条线。

### 2.2 会话渠道轨迹

新增 `GET /api/v1/admin/cost/session/{id}/trace`（只读、只查 `billing_records`）。

数据来源与解析口径（复用既有代码，不新造）：

- 渠道 = `channel_keys JOIN channels`（`channel_key_id` 可空：拒绝类账单没有渠道维度 → `ChannelID=0`）；
- 渠道内部模型 = 按 `(channel_id, internal_model_id)` 解析 `channel_models`，复用 `costEntityResolveBody`
  （与成本统计同一解析式，同一实体在多处的归属不会漂移）；
- 金额直接取账单落库值（`cost_credits` / `credits_consumed`），**不重算**。

响应骨架：

| 字段 | 含义 |
| --- | --- |
| `Points[]` | 逐条账单（按 `call_time, id` 升序）。`Turn` 只编成功轮、`TurnNo` 是含失败轮的时间线序号（与 `/cost/session/{id}` 完全一致的口径）；`CumulativeCostCredits` 是累计成本；`ChannelChanged` 标记**真实切换点**（本点渠道 ≠ 上一点渠道） |
| `Segments[]` | 连续同渠道同模型的段（分段键 = 渠道 ID + 渠道内部模型 ID + 内部模型 ID），带起止时刻、点数、成功点数、三段 token、成本/售价合计 |
| `ActualSwitches[]` | **真实切换点**，`Kind="actual"`。判定只看 `channel_id` 变化；同渠道换内部模型会开新段但**不算**渠道切换，两端渠道内部模型 ID 仍写进字段里 |
| `PredictedBreakEven` | **预测打平点**，`Kind="predicted"`、`Predicted=true`。仅在「从未真实切换过渠道」时给 |
| `Total` / `Limit` / `Truncated` | 分页语义与 `/cost/session/{id}` 一致：`Limit` 缺省不截断、显式上限 2000 |
| `SwitchKindActual` / `SwitchKindPredicted` | 两个枚举字面量下发，前端不用硬编码字符串 |

**两类切换点为什么不会混淆**：字段名不同（`ActualSwitches` vs `PredictedBreakEven`）+ 各自 `Kind` 枚举 +
预测点额外 `Predicted=true`，三重区分；且真实切换过就**不再**给预测点（`PredictedBreakEven=null`）。

**预测打平点怎么算**：同一 `internal_model_id` 在其它**未删除**渠道下的成本配置各画一条成本曲线
（`buildPricingLine`，与定价曲线同一公式、同一采样参数 `Step/MaxTokens/HitRate/InOutRatio/At`），
在共同横轴上找「替代渠道单次成本**首次严格低于**实际渠道」的位置，相邻采样点之间线性插值；
多个替代渠道都能反超时取**最早**的那个。

- 严格小于才算反超：相等继续往后找，避免把打平前的重合段当切换点；
- 任一侧 `OverTier`（落在分档之外，真实计费会失败）一律跳过，并在该点打断插值；
- 算不出来（费率缺失 / 只有一个渠道配置 / 采样范围内没交叉 / 账单没解析到渠道内部模型）→
  仍返回对象，`Matched=false` + `Reason` 说明，**不编造数字**。

## 3. 功能开关

- 键：`console.cost_multichannel_trace`，**默认 `false`**（`internal/seeding/seed.go` 种子）。
- 读：`GET /api/v1/admin/configs/system` 的响应新增 `Console.MultichannelTrace`（键缺失/值非法/未装配 DB 一律 false）。
- 写：`PUT /api/v1/admin/configs/system` 新增**可选** `Console` 块；**不传该块 = 不写该键**，
  老客户端的三组全量 PUT 行为与写入顺序完全不变（有测试锁）。
- 关闭时的表现：`pricing-curve` 传 `ChannelModelIDs` → **403**（`40301`，文案直接点出开关键名）；
  `.../trace` → **403**。不传新参数的旧请求**完全不受影响**。
- 为什么用 403 而不是 400：这是「能力未启用」而非「参数写错」，单独一个状态码，前端可据此隐藏入口。

## 4. 明确不做

- 不落引擎决策（哪个会话在何时因为什么被切走，本轮不写库、不新增表/列）；
- 不改计费公式（`billing.ComputeCredits` 一行未动）；
- 不改路由逻辑（`router` 一行未动）；
- 不动统计页/状态页（`cost/stats`、`service-status` 未改）；
- 不引入新依赖。

## 5. 测试

| 文件 | 覆盖 |
| --- | --- |
| `cost_pricing_golden_test.go` | **逐字节兼容**（4 个 golden 场景）+ `Lines` 键在单线模式下不出现 |
| `cost_trace_test.go` | `ChannelModelIDs` 解析（去重保序/非法/超限）、`parseConfigBool`、`firstCrossTokens`（插值/起点即便宜/无交叉/超档跳过）、分段与真实切换点（含同渠道换模型不算切换）、多线接口（两条线标识与序列、点数按线数收敛、不存在 ID 给不可用线）、轨迹接口（真实切换不预测 / 预测打平点 / 无交叉 / 单渠道无对比对象 / 截断 / 无 DB 403）、开关读写（GET 下发、PUT 带与不带 Console 块） |

门禁：`gofmt` / `go vet ./...` / `go test ./...` / `go test -race`（console、billing、server、router）全绿。

## 6. 给前端的接口契约要点

见 PR 描述里的「接口契约摘要」一节（字段名/类型/语义/多线数据结构），前端直接照抄，不用猜。
