# 统计页改版（对齐定稿 demo）· 前端设计说明

- 日期：2026-10-25（v3：接上后端真实接口 `e7a8589`；v2 为占位版式，v1 为"删运维块 + 首屏两段式"）
- 范围：前端 `frontend/src/views/StatsDashboard.vue`、`frontend/src/api/stats.ts`
- 视觉基准：`https://lingshu-preview.pages.dev/stats-demo-pc` / `stats-demo`
- 后端契约：`docs/designs/2026-10-25-stats-dashboard-backend.md`（参数、字段、口径以该文档为准）
- 边界：不改后端、不动状态页、不引入新依赖

## 1. 页面结构

```
工具栏    统计范围（近 24h / 7 / 30 / 90 天 / 自定义）+ 刷新 + 「实时区每 30s 自动刷新」
① 大数字  总 Token（窗口=范围） / 总消费积分（窗口=范围） / 吞吐量（忙时口径）
② 小指标  总请求数 / 成功率 / 平均耗时 / 请求中会话 / 请求中用户
③ 图 1    用量趋势：近 24h × 四日对比（今天/昨天/前天/大前天，按 HourOfDay 对齐，缺桶断线）
③ 图 2    模型广场：Token/吞吐（忙时）/请求·失败/首字·全程 + 四维排序 + 复制 + 空闲折叠 + 截断提示
④ 实时区  我的会话 / 在线用户（tab 切换），30s 刷新
⑤ 更多维度 默认收起：Token 构成 / Top 渠道密钥 / 计费模式 / Top 用户
⑥ 最近公告
```

## 2. 请求映射

| 交互 | 请求 |
|---|---|
| 页面加载 / 切范围 / 切模型排序 / 手动刷新 | `GET /admin|/dev/stats/dashboard`，参数 `From`、`To`、`Granularity`、`CompareDays=4`、`ModelSort`、`ModelLimit=50`、`Window=exact` |
| 进页面 + 每 30s | `GET /admin|/dev/stats/live?Window=60&SparkPoints=12&Limit=20` |

- 粒度：近 24h → `hour`；7/30/90 天与自定义 → `day`。`Granularity=hour` 遇长区间由后端自动降级为 `day`，前端按响应的 `Usage.Granularity` 渲染，不报错、不清空图。
- `Window=exact`（后端默认）：首屏 KPI / `Usage.Hourly` / `ModelPlaza` 都按**请求区间的严格滚动窗口**算；`Usage.Daily` 与既有维度聚合仍是日界口径。切 `calendar` 时窗口为北京时间自然日归一。
- `CompareDays=4` 恒定：四日对比序列由后端按小时桶返回，与 `Granularity` 解耦。
- 30s 刷新：`refreshTick()` 先取 `live`，再以**当前筛选条件**静默重取 `dashboard`；该函数只读 `preset`/`customRange`、从不写它们，因此刷新不会重置筛选；`document.visibilityState === 'hidden'` 时跳过；已存在在途请求时本轮跳过（不叠加）。

## 2.1 请求序号守卫

筛选切换、模型排序、手动刷新、30s 轮询可能交叠，`StatsDashboard` 对 dashboard 与 live 各维护一个自增序号：

- 每次发起 `seq = ++xxxSeq`，响应回来若 `seq !== xxxSeq` 直接丢弃（不写 `data`/`live`、不动 loading）；
- `finally` 也只有最新序号才清 loading；
- `refreshTick()` 在 `dashInFlight || liveInFlight || loading` 时整轮跳过，保证同一时刻不叠加重复请求。

## 2.2 窗口标签与未匹配模型

- 首屏两个大数字的窗口标签直接读 `KPI.WindowFrom / WindowTo / WindowMode`：`滚动 24 小时` / `自然日 2 天` 这类文案由窗口时长推导，tooltip 给出精确起止（后端 +08:00 墙钟）。**不再硬编码「近 24h」**。
- `KPI` 口径已是滚动窗口（数值可能比改前小），属预期修正，前端不做补偿、不加额外解释。
- `ModelPlaza.List` 可能出现 `Unmatched=true` 的兜底行（`Model="__unmatched__"`）：前端单独渲染成灰底虚线「未匹配模型」区块（Token / 请求·失败 / 吞吐 / 首字全程），**不参与普通模型排序与空模型折叠**，也不受 `Limit` 截断（后端保证）。

## 3. 三个吞吐口径（文案刻意分开）

| 位置 | 数值来源 | UI 文案 |
|---|---|---|
| 首屏大数字 | `KPI.TPM` | 标签「忙时口径」+ 副标「区间 近 X · 请求 N RPM」，tooltip 说明 ≈60÷同批平均耗时 |
| 模型广场 | `ModelPlaza.List[].TPM` | 标签「按模型忙时」 |
| 实时区 | `Sessions[].TPM` / `Users[].TPM` | 头部「近 60 分钟墙钟平均」 |

`BusyMS=0` 的模型行 TPM 显示 `—`（后端此时值为 0，不代表真零吞吐）。

## 4. 空态与降级

| 场景 | 表现 |
|---|---|
| `Compare` 完全无数据 | 趋势图整块换 `el-empty`（不画四条空线） |
| 四条序列都无桶（`hasTrendData=false`） | 同上 |
| `ModelPlaza.List=[]` | `el-empty`「模型目录为空：还没有对外模型」 |
| 目录内有模型但全部 `Active=false` | 不渲染空卡片网格，提示「当前窗口内没有模型请求，可展开查看模型目录」 |
| `Truncated=true` | 提示「仅显示前 N 个模型（目录共 M 个，窗口内有请求 K 个）」 |
| `Sessions/Users=[]` | 各自 `el-empty`「当前没有请求中的会话 / 在线用户」 |
| `live` 请求失败 | 头部红字提示，保留上一次数据，不打断页面 |
| `null` 数组 | 一律 `?? []` 当空处理（后端约定返回 `[]`，前端不依赖该约定） |

## 5. 后端缺口（前端已按约定降级，不造假）

1. `ContextLimit` 恒 0 → 只显示 `ContextUsed` 绝对值，tooltip 注明「平台未配置模型上限」，不做除法。
2. 首屏积分副标仍用「日均」，没有今日/本月聚合。
3. 模型广场排序只有 `latency`（全程），没有单独「首字」排序。
4. `RoundTokens` 是上一轮已完成请求的值，正在跑的这轮不计入。

## 6. 与定稿 demo 的差异

| 项 | demo | 实现 |
|---|---|---|
| 积分副标 | 今日 / 本月 | 日均（无该聚合） |
| 大数字窗口标签 | 固定「近 24h」 | 前两个跟随筛选范围；吞吐量用口径标签「忙时口径」 |
| 模型广场卡片 | 含 1 空闲 + 「另有 2 个已折叠」 | 活跃优先、空闲折叠（`Active` 由后端给） |
| 实时区 | 双卡并列 | tab 切换（我的会话 / 在线用户），移动端更省纵向空间；在线用户仅 admin 可见 |

## 7. 验收

- `npx vue-tsc --noEmit` 零错、`npm run build` 通过。
- 契约已用真实后端（commit `e7a8589`）在本地跑通：`/stats/dashboard`（hour + CompareDays=4 / day + ModelSort=failed）与 `/stats/live` 的响应字段与前端类型逐字段对得上；空库与造数两种情况都验过。
