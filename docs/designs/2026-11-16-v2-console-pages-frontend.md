# 灵枢 V2 阶段三 · 前端 13 页（页面清单 / 完成度 / 依赖接口）

> 2026-11-16 · View（前端）· 分支 `feat/v2-console-ops-pages`（基于 `origin/main` @ `e5b579f`，与 `origin/v2` 同点）
> 依据：`docs/designs/2026-11-15-v2-p1-data-contract.md`（表结构/枚举，下称「数据层契约」）、
> `docs/designs/2026-11-15-v2-upgrade.md` §4.3/§0.4（渠道族摘除、V2 前端 13 页无会话页）、
> `backend/internal/domain/console/ops.go`（盈亏 / 对账两个已定型接口的字段与语义）。
> 本文只写「前端做了什么、还差什么、差在哪个接口」，不重复后端方案。

---

## 0 一句话

13 页里 **5 页新建（内部模型与拓扑 / 订阅 / 账本 / 对账 / 盈亏）+ 4 页按 V2 口径改造（模型 / 账单 / 账单详情 / 系统）+ 4 页沿用（统计 / 用户 / 令牌 / 公告）**；
新建的 5 页中 **盈亏、对账** 两个接口已定型（字段与语义照抄 `ops.go`，合入即可取真数据），
**内部模型、账本、订阅** 三个接口未上线，页面按「空态 + 显式演示数据」处理，接口上线后**不改页面代码**。

---

## 1 13 页清单

| # | 页面 | 路由 | 完成度 | 依赖接口 | 状态 |
|---|---|---|---|---|---|
| 1 | 统计 | `/admin/dashboard` | 沿用（V2 无新增字段） | `GET /admin/stats/dashboard`、`/stats/live` | 可用 |
| 2 | 内部模型与拓扑 | `/admin/topology` | 快照 + 画布**已实现**（读接口未上线） | `GET /admin/internal-models`（待 Core）、`PUT /admin/model-links`（待 Core） | 空态可交付 |
| 3 | 模型 | `/admin/models` | 加「内部模型绑定」列 + 未绑定红色横幅（**字段缺失时整列不渲染**） | `GET/PUT /admin/models`（绑定列随 0019 上线） | 已改造 |
| 4 | 订阅 | `/admin/subscriptions` | 列表 + **录入表单（字段/校验/错误位）**已完成；保存按裁定先不写库 | `GET/POST /admin/subscriptions`（待 Core）、录入标准（待 Spec） | 壳可交付 |
| 5 | 账本 | `/admin/ledger` | 流水 + 预扣两个页签已实现（读接口未上线） | `GET /admin/ledger`、`GET /admin/credit-holds`（待 Core） | 空态可交付 |
| 6 | 对账 | `/admin/reconcile` | 完整（含 null≠0、blocked 补拉进度、五状态语义说明） | `GET /admin/ops/reconcile-windows`（形状已冻结） | 待接口合入 |
| 7 | 盈亏 | `/admin/pnl` | 完整（含 Uncosted 毛利虚高警示、分组图 + 明细表） | `GET /admin/ops/pnl`（形状已冻结） | 待接口合入 |
| 8 | 账单 | `/admin/billings` | 加「归因 / 成本来源」列（**字段缺失时整列不渲染**） | `GET /admin/billings`（归因列随 0020 上线） | 已改造 |
| 9 | 账单详情 | `/admin/billings/{id}` | 加「上游供应商 / 账号 / 协议 / 成本来源 / 账本条目」块（字段缺失时不渲染） | `GET /admin/billings/{id}`（同上） | 已改造 |
| 10 | 用户 | `/admin/users` | 沿用 | 无变化 | 可用 |
| 11 | 令牌 | `/admin/tokens` | 沿用；**callerKey 绑定列待 #81 的管理端绑定端点合入**再补 | `POST /admin/magpie/keys`（#81） | 待接口 |
| 12 | 公告 | `/admin/announcements` | 沿用 | 无变化 | 可用 |
| 13 | 系统 | `/admin/system` | 新增「上游（V2）」页签：base_url / 两个超时 / degrade 三开关 / 订阅池前缀，**当前只读** | `GET /admin/configs/system`（该块待 Core 返回）、写入白名单待开放 | 只读可交付 |

> 「13 页」的拆法说明：按 `v2-upgrade.md`「V2 前端 13 页无会话页」+ 渠道族在 0024 摘除，
> 会话 / 渠道 / 渠道协议 / Auto 模型 / 标签五类页不入清单；「成本」页的盈亏职能由第 7 页接管（`cost_source` 口径分段），
> 旧成本页暂留（只读，等 V2 成本口径稳定后下线）。该拆法若与 Spec 的页面清单不一致，以 Spec 为准改路由标题，不影响页面实现。

---

## 2 为什么长这样（三条会改实现的口径）

### 2.1 接口未就绪 ≠ 出错 ≠ 0

一个页面在三种状态下必须长得不一样，否则运营分不清「真没有数据」和「接口没上线」：

| 状态 | 触发 | 表现 |
|---|---|---|
| `live` | 200 | 正常渲染 |
| `pending` | 404 / 405 / 501 / 403 | 灰底横幅写明**是哪几个接口**没上线 + 「预览演示数据」按钮；**默认不显示任何数字** |
| `error` | 其它 | `ErrorBubble`（带可复制 requestId） |

实现：`src/api/v2.ts` 的 `v2Get` 把四类状态码归一成 `pending`（走 `http` 的 `silentStatuses`，不弹全局红条）；
`src/composables/useV2Load.ts` 管 `live/pending/error/demo` 四态与演示开关；`src/components/v2/V2Page.vue` 是统一页壳。

### 2.2 演示数据默认关，开了就挂橙条

财务页默认显示假数字是事故源。本期的做法：**只有用户点「预览演示数据」才渲染**，
渲染时页头挂 `演示数据` 标签 + 橙色横幅「不是真实账务结果，不能用于对账、结算或对外汇报」，
并提供「退出预览」立刻回到真实请求。演示数据集中在 `src/api/v2.mock.ts`，接口上线后整个文件删掉即可，页面不用改。

### 2.3 对账的「null = 还没算过」必须原样显示

数据层契约 §7 的核心是漏账可见：四个计数列 `null` 表示对账作业还没写（阶段二才补写入方），
`0` 表示算了是 0 笔。前端把 `null` 显示成「未算」、`0` 显示成 `0`，`blocked` 显示 `ScrollOffset` 补拉进度，
`skipped` 明确标「无结论」——不许出现「都显示 0，看起来对上了」。

---

## 3 改了哪些「老页面」（全部可回退，字段缺失即不渲染）

| 页面 | 改动 | 老后端下的表现 |
|---|---|---|
| 模型 | `ExternalModel.BoundInternalRef` 可选字段 → 有该键才渲染「内部模型绑定」列；启用且未绑定的行显示「未绑定 · 不售卖」，并在表格上方出红色横幅（附「未绑定 = `/v1/models` 不可见、请求 404」） | 没有该键 → 列与横幅都不出现，页面与改前像素一致 |
| 账单 | `AdminBillingItem` 加 `UpstreamProvider/Account/Protocol/Model`、`CostSource` 可选字段 → 有键才渲染「归因 / 成本来源」列（`CostSource` 用统一 `StatusPill`：账本实付 / 订阅摊销 / 无成本口径） | 同上 |
| 账单详情 | `BillingDetail` 加同组字段 + `LedgerEntryID` → 有键才展示归因块，历史行显示「未归因」并注明「历史行无成本口径，不要与新区间同图比较」 | 同上 |
| 系统 | 新增「上游（V2）」页签：只读展示 `Upstream` 块（未返回时出空态「后端该块未上线」） | 页签出现、内容为空态；其它页签行为不变 |

历史账单的处理口径：`upstream_*` 为 `null` 时显示「未归因」，**不填 0、不推断**（契约 §1.2/§4.3）。

---

## 4 验证（DOM 断言，不靠截图）

脚本：`/data/pv/v2-pages-check.mjs`（`vite preview` + puppeteer-core，拦截 `/api/v1/**` 回桩）。24 项全绿，覆盖：

1. 盈亏：`收入 2,400.00` 渲染、`毛利偏高` 警示出现、明细 3 行、无错误条；
2. 对账：五种状态文案齐全、`null` 显示「未算」、`blocked` 显示补拉 4000 条、5 行；
3. 账本：404 → 空态横幅 → 点「预览演示数据」→ 橙色演示横幅 + 表格有行；
4. 拓扑：演示态快照 5 行、切页签后画布 canvas 已渲染（`LinesChart` 已在 `VChart.vue` 注册，缺了连线会被 ECharts 静默吞掉）；
5. 订阅：空表单提交出「必须填供应商 / 必须填账号」、填 `acme/main`（已存在）出「已存在同一 (供应商, 账号)」；
6. 移动端 390px：五个新页 `scrollWidth <= clientWidth`（无横向溢出）；
7. **老后端回归**：模型页无绑定列、账单页无归因列、系统页出现上游页签，且页面不崩。

类型/构建：`npx vue-tsc --noEmit` 零错；`npx vite build` 通过。

---

## 5 阻塞点（等谁、等什么）

| # | 阻塞 | 等谁 | 解除条件 |
|---|---|---|---|
| 1 | 内部模型 / 拓扑 / 账本 / 预扣 四个读接口不存在 | Core | 按数据层契约字段开 `GET /admin/internal-models`、`GET /admin/ledger`、`GET /admin/credit-holds`；`PUT /admin/model-links` 用于画布落位 |
| 2 | 订阅「录入标准」（必填项、默认值、可否改 `provider_ref`、错误码） | Spec | 定稿后只替换 `Subscriptions.vue` 的 `submit()` 实现 + 字段规则，不动表单结构 |
| 3 | 系统页「上游（V2）」写入白名单 | Core | `PUT /admin/configs/system` 接受 `Upstream` 块后，本页从只读切到可编辑（`isConfigTab` 放开 `upstream` 即可） |
| 4 | 令牌页 callerKey 绑定列 | #81 | 管理端绑定端点合入 v2 后补一列（`magpie_keys` 按裁定保留，不进 0024） |
| 5 | 盈亏 / 对账的 handler 只在阶段二分支 | Core | 合并进 v2 即自动切真数据；形状已冻结，前端无需改动 |

> 本分支只推送自己的分支，未向 `main`/`v2` 发起任何 PR/MR；v2 收敛 PR 由 Core 开。
