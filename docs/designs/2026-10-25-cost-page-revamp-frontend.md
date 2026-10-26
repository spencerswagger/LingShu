# 成本页改版 · 前端实现说明

- 日期：2026-10-25
- 对应后端：`feat/cost-page-revamp` / `7a8a841`（契约见 `docs/designs/2026-10-25-cost-page-revamp-backend.md`）
- 对应 demo：PC `lingshu-preview.pages.dev/cost-demo`、移动 `.../cost-demo-m`
- 文件：`frontend/src/views/admin/Cost.vue`、`frontend/src/api/admin.ts`（只加类型与 `getCostPricingCurve`）
- 边界：不改后端、不动状态页与统计页、不引入新依赖

## 1. 版面结构

```
工具栏    说明文案（全部历史口径 / 纯只读）+ 刷新
A 成本统计  桌面：el-table（11 列 + 合计行）；窄屏：数字墙（总成本/总售价/请求）+ 实体行（单列）
B 定价曲线  工具栏（模型 / 步进 / 上限 / 命中率 / 输入输出比 / 时段 / 系数开关 / 线选择）
            图 → HTML 图例 → 参数与来源说明
C 会话成本  会话下拉（名字·用户·模型·时间·N 次调用）+ 合计标签 → 图 → 图例 → 说明
```

窄屏（≤768px）用 CSS 切换 `.desktop-only` / `.mobile-only`，图表高度降为 240/220px，控件宽度 100%，正文 ≥14px、单列不横滚。

## 2. 数据接线

| 区块 | 接口 | 关键字段 |
|---|---|---|
| A 统计表 | `GET /admin/cost/stats` | `EntityKey`（row-key）、`ExternalModelName`（展示名）、`SnapshotNames`（曾用名）、`ChannelModelID`/`ExternalModelID`（喂给曲线） |
| B 定价曲线 | `GET /admin/cost/pricing-curve` | `Cost`（渠道内部模型配置）/`Sale`（对外模型配置）、`Points[].Credits`/`CreditsNoCoeff`、`TierBreaks`、`OverTier`、`TimeOptions` |
| C 会话成本 | `GET /admin/cost/session/{id}` | `Points[].Turn/CostCredits/SaleCredits`、`Totals` |

曲线模型下拉的候选直接取统计表实体（`ChannelModelID` 能直接传给曲线接口）；请求参数：`ChannelModelID`、`ExternalModelID`（缺失时退回 `ExternalModelName`）、`Step`/`MaxTokens`/`HitRate`/`InOutRatio`、`At`。

首次请求不带采样参数，用后端回填的生效值（`Step`/`MaxTokens`/`HitRate`/`InOutRatio`）初始化输入框；用户改动后才带参重取。

## 3. 曲线渲染规则

- **两条线**：成本 `#5a5ce8` 实线、售价 `#e6a23c` 虚线；`Available=false` 的线不画，改在下方红 tag 给 `Reason`。
- **系数开关**：开用 `Credits`、关用 `CreditsNoCoeff`，同一批 `Points` 只切字段，前端不做任何乘法。
- **跳档拐点**：`TierBreaks[].Tokens` → `markLine`（橙色虚线，标签 `×<ToCoeff>`）。
- **超档**：`Points[].OverTier=true` 的点 y 置 `null` + `connectNulls:false` → 断线；`OverTier[]` 区段画红色底 `markArea`，区间起点打红点（scatter 系列）。
- **时段**：下拉来自 `TimeOptions`，选项 key = `Name|Start|End`；用户选择后带 `At=<Start>` 重取，让后端按该时段算系数。默认高亮 = 后端 `At` 回显的时刻落在哪个时段（按字符串切 `HH:MM`，避免浏览器时区错位）。
- **图例**：页面内 HTML 图例（成本/售价/升档/超档），窄屏同样可读；ECharts 自带 legend 关闭。

## 4. 会话成本：竞态、序号与失败轮

- `sessionSeq` 序号守卫：切换会话时**先清空** `sessionPoints`/`sessionTotals`/`sessionMeta`，再取数；响应回来若序号已过期直接丢弃，finally 也只有最新序号才清 loading。
- 下拉标签：`名字 · 用户 · 模型 · 月-日 时:分 · N 次调用`，选项内 `title` 给全文，超长由 el-select 截断。
- **x 轴用 `TurnNo`**（时间线序号，1..M，含失败轮）：`Turn` 是「成功轮序号」（失败轮为 0），拿它当 x 会把失败轮挤到原点，已不再使用。
- **失败轮**：`Failed===true` 或 `Status==='failed'` 的点 →
  - 成本/售价两条线的该点 y 置 `null` + `connectNulls:false`：曲线在该轮断开，不用 0 参与连线（否则会画出「成本骤降」的假曲线）；
  - 另起一条红点散点序列（`scatter`，y=0）叠在图上标注「未计费」；
  - 顶部给「含 N 次失败调用，未计费」标签，避免与合计对不上数。
- **截断**：`Truncated=true` 时在图上方显示「仅显示最早 N 行，共 M 行」，提示 `Limit` 缺省 500 / 最多 2000。
- 合计 `Totals` 仍只累加成功轮（后端口径），前端不做二次求和。

## 5. 与 demo / 后端的已知差异

| 项 | demo | 实现 | 原因 |
|---|---|---|---|
| 定价曲线横轴 | 文案写「轮 1…轮 30」 | 上下文 token（`Points[].Tokens`） | 新接口按 token 出点，轮次轴属旧 `/cost/curves` 口径 |
| 交点标注 | 无 | 去掉（旧曲线的「同指标交点」一并移除） | 新接口只画同一实体的成本/售价两条线，无多模型同图 |
| 会话成本横轴 | 「轮 1..7」 | 调用序号（含失败轮） | 后端暂未提供「按用户轮」的 `TurnKind`，用 `TurnNo` 保证失败轮占位不错位 |
| 移动版 | 独立页面 | 同组件 CSS 切换 | 仓库只有一套路由 |
| 售价数字下降 | — | 不做任何前端补偿或解释性兜底 | 探测记账剔除是已确认的口径修正 |

## 6. 联调验证（本地真实后端）

用本分支后端在沙盒里跑通（造数后已清理）：

- `/cost/stats` → `EntityKey=cm:9103`、`SnapshotNames=["gpt-4o","gpt-4o-pro"]`、`ExternalModelName=gpt-4o`（`gpt-4o-pro` 的账单已归一到同一实体，前端显示「曾用名 1」）。
- `/cost/pricing-curve?ChannelModelID=9103&Step=8000&MaxTokens=40000&HitRate=0.3&InOutRatio=5&At=19:00` → `TimeCoeff=1.5`（高峰）、`Sale/Cost` 的 `Rates`/`TierBreaks`/`Points[].Credits|CreditsNoCoeff` 全部对上；`At=02:00` 时 `TimeCoeff=0.5`。
- 缺费率模型 → `Cost.Available=false`、`Reason=渠道内部模型未配置成本费率`（前端红 tag）。
- `/cost/session/sess-demo-3`（含 1 条 failed 行）→ `Turn` 1,0,2 / `TurnNo` 1,2,3 / `Status` completed,failed,completed / 失败行金额 0、token 保留；`Totals` 只累加成功轮（0.72 / 1.2）。
- `/cost/session/sess-demo-3?Limit=2` → `Truncated=true`、`Total=3`、返回 2 行（`TurnNo` 1,2）。

## 7. 门禁

`npx vue-tsc --noEmit` 零错、`npm run build` 通过。
