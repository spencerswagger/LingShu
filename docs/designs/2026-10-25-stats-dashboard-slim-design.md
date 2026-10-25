# 统计页瘦身 + 首屏动静分区（设计说明）

- 日期：2026-10-25
- 范围：前端 `frontend/src/views/StatsDashboard.vue`（admin `/admin/dashboard` 与 dev `/dev/dashboard` 共用组件）
- 需求来源：spencer 反馈「统计页分运维概览 / 用量分析两块，运维用不上（大家看状态页）；首屏百分比指标糊在一起；按天趋势看不出东西」
- 本轮边界：只做「删运维块 + 首屏动静分区」；按小时聚合需后端 `billing/stats.go` 增小时粒度，接口就绪后再做

## 1. 删除「运维概览」整块

移除内容（管理端 `isAdmin` 块整体删除）：

| 区块 | 数据来源 | 处理 |
|---|---|---|
| 渠道密钥 / 对外模型 / 渠道内部模型 计数卡 | `Ops.Keys`、`Ops.Models` | 删除 |
| 成功率趋势图 | `Usage.Daily[].SuccessRate` | 删除 |
| 耗时分布图（P50/P90/P95 副标） | `Usage.Latency` + `Usage.P50MS/P90MS/P95MS` | 删除 |
| 吞吐量趋势图（Token 柱 + 请求数线） | `Usage.Daily` | 删除 |
| 错误 Top 表 | `Usage.Errors` | 随块删除（见下"取舍"） |

连带清理：`ops` / `opsCounts` / `successOption` / `latencyOption` / `throughputOption` 计算属性，`KpiCard.tipTarget` 字段，图标 import（`Cpu` / `Key` / `PieChart` / `Timer` / `TrendCharts` / `CircleCheckFilled`），色板里失效的 `C.success / C.warning / C.danger`，以及 `.section-bar--ops`、`.ops-count-*`、`.ops-dot`、`.block-note`、`.inline-switch` 等样式。

**取舍说明**：`错误 Top` 表虽然读的是 `Usage.Errors`（用量数据），但它位于「运维概览」块内，按"整块移除"处理；失败次数仍体现在「总请求数」卡片副标。若产品/运维仍需要按错误信息看 Top，可在「用量分析」里单开一张卡（本轮不做，避免改动用量分析既有内容）。

**未动**：`frontend/src/api/stats.ts` 的类型定义（`StatsOps` 等）保持原样——后端仍在返回 `Ops` 字段，接口契约与统计口径不变，前端只是不再渲染。

## 2. 首屏动静分区

```
① lead-grid   静态总量（主视觉）  总 Token / 总消费积分 / 总请求数
              大数字 30px + 迷你趋势线 + 环比 delta，不随粒度变化
② mini-strip  次级小指标（弱化）  缓存命中率 / 成功率 / 平均耗时 / 吞吐量(TPM·RPM)
              16px 单行内联，无图标无趋势线，口径说明走 tooltip
③ 用量分析    动态图表区（原样保留）趋势 / Token 构成 / Top 模型 / Top 渠道密钥 / 计费模式 / Top 用户
```

- 视觉分层：`lead-card` 有卡片 + 左侧色条 + sparkline；`mini-strip` 只有上下细线分隔的裸文字指标，刻意不与总量抢主位。
- 响应式：`lead-grid` 用 `auto-fit minmax(260px,1fr)`（宽屏三列、窄屏自动折行）；`mini-strip` < 1000px 竖排。
- 数据口径零改动：`leadCards` / `miniMetrics` 只是把原 `kpiCards` 的 7 张卡拆成两组，取数表达式与原实现逐项一致（含吞吐量忙时口径 `BusyRequests` / `SumDurationMS` 就绪判定与"样本较少"灰字）。

## 3. 趋势图聚合粒度占位

「用量趋势」标题右侧新增 `按天 / 近 24 小时` 切换，整组 `disabled` + tooltip「按小时聚合待后端接口就绪后开放，当前为按天聚合」。

本轮**只渲染按天数据**，`granularity` 仅供占位（`'day' | 'hour'`）。后端小时聚合就绪后，接线点集中在两处：

1. `load()` 里按 `granularity` 追加查询参数（建议 `Granularity=hour` 之类可选参数，缺省按天，保证向后兼容）；
2. `trendOption` 的 x 轴 label 走 `Date.slice(5)`，小时粒度需要改成 `HH:00` 或 `MM-DD HH:00`（按后端返回的 `Date` 格式确定）。

## 4. 验收口径

- `npx vue-tsc --noEmit` 零错、`npm run build` 通过。
- admin 统计页仅剩「首屏两段 + 用量分析 + 最近公告」；dev 端行为不变（本来就没有运维块，`Top 用户` 依旧仅管理端）。
- 状态页 `ServiceStatus.vue` 未改动。
