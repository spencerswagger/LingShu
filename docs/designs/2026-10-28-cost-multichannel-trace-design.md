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
- `Step` / `MaxTokens` **永远回吐实际生效的采样参数**：未显式传时按该模型分档推导、
  再按点数上限收敛（与定价曲线同规则）；连提前返回的分支也带上，前端不会显示「上限 0」。

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

---

## 6. 返修记录（2026-10-30 · 按 Owl 评审）

> 评审来源：PR #65 Owl 独立审查（P2-1 ~ P2-4 必改 + P3 顺手项）。

### 6.1 切换判定只认「成功且有渠道」的点（P2-1）

- 旧实现按「相邻两条记录渠道变没变」判切换 → **一次请求内的候选 failover**（A 429 失败 → B 成功）
  会被标成真实切换；`A(成功) → 0(无渠道拒绝) → A(成功)` 还会算出 A→空→A 两次假切换。
  连带后果：只要出现假切换，`PredictedBreakEven` 被整通会话禁掉（主打能力报废）。
- 现在：切换基准 = **上一个成功且有渠道的点**（`!Failed && ChannelID>0`）；
  失败/无渠道记录既不产生切换，也不参与「从哪个渠道切过来」。
  `Points[].ChannelChanged` 与 `ActualSwitches` **用同一基准**（两处都改，前端标色与切换列表不会再对不上）。
- `Segments` 分段逻辑保持不变（分段键仍是渠道 + 渠道内部模型 + 内部模型）：失败轮/拒绝轮如实占一个点，
  不影响切换结论。
- 边界口径（写进代码注释与用例）：**上一次成功轮用 A、这次成功轮用 B** = 1 次真实切换，且切换点挂在
  成功点（`TurnNo` 是含失败轮的时间线序号）上；失败候选点不带 `ChannelChanged`。
- 补测：请求内 failover 0 切换且预测仍下发；无渠道拒绝夹中间 0 切换且不出空渠道名；成功轮后 failover 只在成功点记 1 次。

### 6.2 预测候选过滤死渠道（P2-2）

`loadSiblingChannelModels` 的 SQL 补两条：候选渠道 `c.deleted_at IS NULL AND c.state <> 'DISABLED'`；
**当前渠道那条始终保留**（`cm.id = $2`）——否则当前渠道已删/停用时会退化成
「配置不存在」的 Reason，把真实原因掩盖成配置缺失。

### 6.3 预测基准取第一条「成功且解析到渠道内部模型」的记录（P2-3）

旧实现取 `points[0]`，而首条常是拒绝/余额不足账单（无 `channel_key_id`、`ChannelModelID=0`）→ 整通会话没有预测。
现在逐点找第一条合格记录作基准（`firstTraceAnchor`）。**一条都没有**时仍返回对象
（`Matched=false`），Reason 说「会话里没有成功且解析到渠道内部模型的记录」——不再误报成「配置缺失」。

### 6.4 开关下发三态，避免被静默关掉（P2-4）

`GET /configs/system` 的 `Console.MultichannelTrace` 现在是三态：

| 库里的情况 | 下发 | 前端行为 |
| --- | --- | --- |
| 读到 `true` / `false` | `true` / `false` | 正常显示，保存时回写 |
| 键不存在（`sql.ErrNoRows`） | `false` | 可信的「默认关」，可回写 |
| 读取出错（DB 抖动 / 未装配） | `null` | 视为未知，**跳过回写** |

服务端门控（`costViewsEnabled`）仍是 fail-closed：读不到就当没开，不放行新能力。

### 6.5 顺手三项（P3）

1. **检查顺序统一**：两个端点都按「先参数校验（400）→ 再判能力开关（403）」。
   关闭 + 非法参数一律 400，不会出现「同一个开关，一个端点 400、另一个 403」。
2. **「能力未启用」的 403 不落异常日志**：新增 `resp.ErrNoLog`（响应壳与 `Err` 完全一致，只是不写 `api error`），
   两个 403 分支改用它，避免 4xx 告警把「开关关着」误报成故障；序列化失败仍按 5xx 落日志。
3. **上限实际生效值回显**：多线时后端会按线数把 `ChannelModelIDs` 的总点数上限等比下调 `MaxTokens`（响应里一直回报实际值）；
   前端在手动填了上限且被收敛时，输入框旁显示「实际生效 N token（M 条线按总点数上限自动收敛）」，不再只显示用户填的值。
4. **截断 + 预测的组合**：`Truncated=true` 时给预测打平点的 `Reason` 追加「本次只加载了前 N 条（共 M 条）」，
   不把截断轨迹的结论说成整通会话的事实。

### 6.6 会话成本路由默认值（spencer 定稿，与 Owl P3-7 描述不同）

- 产品默认改为 **`enabled=true`、`mid_switch=true`、`shadow_enabled=false`**：
  种子 `routing.session_cost` 与代码默认 `defaultSessionCostConfig()` 均已改；配置里少写字段也按此补齐。
- **故障安全态不变**：配置读不到 / 解析失败 / 未装配配置源时仍回退三态全关（`safeSessionCostConfig`），
  不凭默认值改变路由行为。
- **回退方式**：改写 `sys_configs` 的 `routing.session_cost`（`enabled`/`mid_switch` 置 false）即可，
  配置写入有失效钩子 → 热生效，**无需重启/重新部署**。注意种子是 `ON CONFLICT DO NOTHING`，
  存量库已有的旧值不会被覆盖，需要显式改一次。
- 当前 `routing.session_cost` **没有管理端点**（`/configs/system` 只覆盖 Billing / SessionExpiration /
  上游白名单 / Console）：要 UI 或端点开关需另开任务；本轮只在 PR 说明写清「改哪个键、怎么生效」。
