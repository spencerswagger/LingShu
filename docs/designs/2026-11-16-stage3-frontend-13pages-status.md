# 阶段三前端 13 页 · 逐页落地状态（第二批）

- 日期：2026-11-16 · 分支 `feat/v2-console-ops-pages` · PR #87（目标 `v2`）
- 对应验收单：`docs/designs/2026-11-16-stage3-13-pages-acceptance.md`（下称「验收单」）
- 读者：Gate（按页出验收单）、Core（哪条接口挡着哪页）

## 0 通用口径（13 页同一套壳，不逐页重复）

| 项 | 实现 |
|---|---|
| 四态 | `components/v2/V2Page.vue`：加载 / 真实数据 / 接口未就绪（灰底说明 + 「预览演示数据」按钮，**默认不显示任何数字**）/ 错误（`ErrorBubble` 带 `request_id` + 重试） |
| 演示态 | 橙条「演示数据 · 接口待接入」+ 可退出；带演示数据不参与任何数据类验收（验收单 §0 第 2 条） |
| 未就绪判定 | `api/v2.ts` 的 `v2Get` 把 404/405/501/403 归一成 `pending`，走 `silentStatuses` 不弹全局红条 |
| 空值语义 | `null`（未计算/未记录）与 `0`（算了是 0）文案与颜色分开；金额 5 位小数、标明积分 |
| 字段兼容 | `pick()` 同时认 PascalCase 与 snake_case，后端改 tag 不会整页空白 |
| 权限 | 路由 `meta.roles` → `/403`；接口另有一道（没做接口校验的页在下面逐页标出） |

## 1 逐页状态

| # | 页面 | 路由 | 数据来源接口 | 本批做了什么 | 卡在哪 |
|---|---|---|---|---|---|
| P01 | 统计概览 | `/admin/dashboard`、`/dev/dashboard` | `stats/dashboard`、`stats/live` | 新增「成本与毛利」条：后端返回 `Revenue/Cost/MarginPct/LedgerCost/AmortizedCost/Uncosted` 就显示真数字，**没返回就只出一句说明并指路盈亏页**，不拿消费积分反推成本；开发者端整块不渲染 | 后端 M1 未改造：`/stats/dashboard` 不返回收入/成本/毛利率，成本来源四段无从取数。前端已按可选字段建模，后端补上即自动显示 |
| P02 | 上游拓扑 | `/admin/topology` | `GET /admin/topology`（T1）、`POST /admin/topology/sync`（T3） | `@vue-flow/core` 四层画布（账号 → 供应商大卡含实际模型行 → 路由组/对外模型），stale 灰化 + 虚线；同步 404 时如实写「接口未上线」，不清空快照、不假成功 | T1/T3 新增接口未上线；T2（坐标保存）延后，拖拽后提示「坐标暂不保存」 |
| P03 | 对外模型与绑定 | `/admin/models`、`/dev/models` | `admin/models*`、T4、T5 | 绑定列（后端返回 `BoundInternalRef` 才出现）；未绑定统一写「未绑定 + 绑定能力尚未上线，当前不拦截」（不写「不售卖/请求 404」）；空态分管理端（先同步拓扑）/开发者端（暂无可用模型）；开发者端无任何写入口 | T4（候选）与 T5（写绑定）未上线，绑定按钮禁用并说明 |
| P04 | 订阅账本 | `/admin/subscriptions` | S1–S7 | 元/美元录入实时折算积分、`(Provider,Account)` 重复 409 不覆盖、C1–C4 判定 + 十条原因码中文文案、派生六态（stopped→once→missing_cycle→pending→expired→active） | S1–S7 全未上线：列表空态 + 表单只给判定结果 |
| P05 | 账本流水 | `/admin/ledger` | L1 `ledger/entries`、L2 `ledger/status` | 新鲜度卡（>180s 橙「账本不新鲜」、>15min 红「对账不可用」）；`blocked/error>0` 给对账跳转；`ledger_id` 一键复制；**`?ledger_id=` 直达**（P09 账本条目点过来）并把时间窗自动放大到 30 天；空态三分（拉取关闭 / 从未拉到 / 区间无数据） | L1/L2 未上线 |
| P06 | 对账窗口 | `/admin/reconcile` | `ops/reconcile-windows` | 计数列 `null` → 「未计算」，绝不落 0；`skipped` 四种原因中文；`blocked` 显示 `ScrollOffset` 待补进度；`Limit` 截断提示；`alarm_streak≥3` 高亮 + 一键筛 | `skip_reason` 建议由 M4 独立字段返回（当前兼容从 `Detail.skip_reason` 读） |
| P07 | 盈亏 | `/admin/pnl` | `ops/pnl` | 只认后端单一 `Uncosted`（不虚构「历史未配置/none」两段拆分：`ck_billing_cost_presence` 让后者不存在）；`MarginPct=null` → 「—」；`Truncated` 提示；计数用精确整数 | `GroupBy=model` 后端按 `internal_model_id` 分组，维度键待产品拍板（A-2） |
| P08 | 账单查询 | `/admin/billings`、`/dev/billings` | `admin/billings*`、`dev/billings*` | **筛选写回 URL（G4）**：刷新/分享/后退一致，旧参数 `channel_key_id` 不读不写不 500；成本来源 `null`（历史未知，灰）与 `none`（无成本口径，红）文案与颜色分开；开发者端整列不渲染归因/成本来源 | 后端 M6/M7 的 `UpstreamProvider/HasLedger` 筛选项待接口确认，暂按字符串直传（不存在的参数后端忽略即可） |
| P09 | 账单详情 | `/admin/billings/:id`、`/dev/billings/:id` | 同上 `/{id}` | 资金轨迹等式显式写出「预扣 − 退款 + 差额 = 实扣 / 实扣」，不成立时直接标出差额；账本条目匹配 → 可跳 `/admin/ledger?ledger_id=`，未匹配 → 「待结算」；40401 → 「账单不存在或已被清理」+ 返回列表；开发者端不渲染资金轨迹与归因块 | 无（字段缺就整块不渲染） |
| P10 | 用户与钱包 | `/admin/users` | `admin/users*`、`wallet/*` | 新建页「按成本」禁选 + 说明（口径 D-1）；详情页同样只能从 cost 切走；列表已有 cost 用户保留标注 | 无 |
| P11 | 令牌 | `/admin/tokens`、`/dev/tokens` | `admin/tokens*`、`dev/tokens*` | 标签概念清零（列/筛选/表单/DTO）；查看密钥二次口令确认 + 明示「本次查看已记入审计日志」 | 无 |
| P12 | 系统设置 | `/admin/system` | `configs/system` | 「上游（V2）」只读页签：三键 + 订阅池前缀，写入口未开放时如实写「写入白名单还没开放」；无 secret/key 字段 | 后端**没有** `upstream.base_url` 的管理端写入口（代码注释明写），验收单 A-6「保存即生效实测」在接口开放前无法验 |
| P13 | 公告 | `/admin/announcements`、`/dev/announcements` | `admin/announcements*`、`dev/announcements` | **补开发者端入口（A-5）**：新增 `/dev/announcements` 只读页 + 顶栏铃铛（有公告才出现、带未读数，本地水位线），页面不发起任何写请求 | 无 |

## 2 本批另做的清理

- 删 `views/admin/SessionList.vue`、`SessionDetail.vue`、`Cost.vue`（验收单 A-7：路由已 redirect，文件不该留）。
- 删开发者端菜单里的死项，补公告铃铛；管理端/开发者端菜单均无「渠道 / 标签 / Auto / 会话 / 成本」。

## 3 与验收单不同、需要拍板的两处

1. **P01 实时区「请求中会话」卡片保留**。验收单 P01 写「实时区只有速率类指标，无会话/在线用户列表」，但 2026-10-29 spencer 拍板的统计页 v4 明确要求会话与用户卡片化。菜单里已无 `/admin/sessions` 入口，所以按 v4 保留卡片、只保证无「会话列表页」入口。
2. **P08 账单页保留「渠道 / 渠道密钥（已下线）」列**，只删筛选控件与候选加载。理由：历史账单行仍有 `channel_name`，删列会让老账单丢失信息；已标「已下线」且 tooltip 指向归因列。

## 4 自测（如何复跑）

- 类型与构建：`cd frontend && npx vue-tsc --noEmit && npm run build`。
- DOM 走查：`vite dev`（5199）+ puppeteer 拦 `/api/v1/**` 回桩，跑真实路由（管理端 12 条 + 开发端 7 条），断言 49 项：未上线文案 / `null` 不落 0 / 成本来源两文案不同 / 开发者端无成本列 / 「按成本」禁选 / 菜单禁用词 / 390px 无横向溢出。
- 脚本为一次性，不入仓库（沙盒内位置：`/data/pv/stage3-check.mjs`）。
