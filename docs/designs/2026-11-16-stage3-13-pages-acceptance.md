# 阶段三前端 13 页验收清单（逐页四维：数据来源 / 空态 / 错误态 / 权限）

- 日期：2026-11-16 · 状态：**生效**（spencer 2026-11-16 派工：13 页验收标准写成清单，每页必须有数据来源接口、空态、错误态、权限四段）
- 读者：Gate（照此出验收单，逐条打勾）、View（开工前先看本页四段）、Core（接口字段与错误码以本文为准）
- 与 `2026-11-16-stage3-frontend-13-pages.md` 的关系：那份是**页面规格**（用途/交互/接口清单），本文是**可勾选验收单**。两份冲突时以本文为准（本文第 4 节列了本轮核对出的口径修正与依据）。
- 依据（均已核对代码，非推测）：`backend/internal/server/server.go:98,226`（角色守卫按路径前缀）、`backend/internal/domain/console/ops.go`（pnl / reconcile-windows 实现，在 `feat/lingshu-v2-billing-phase2`）、`backend/internal/domain/console/dev_view.go:51-60`（`devSensitiveKeys` 单一来源）、`0020_v2_billing_attribution.sql`（`ck_billing_cost_presence`）、`0023_v2_reconcile.sql`（`ck_reconcile_status` 五值）、`frontend/src/router/index.ts`（现有路由与守卫）。

---

## 0 怎么用这份清单

1. **一页一张单**：每页四段（数据来源 / 空态 / 错误态 / 权限）+ 一串验收条目，Gate 逐条勾；任一条 `[P0]` 不通过 → 该页判**不可合入**。
2. **mock 不算通过**：接口未就绪时允许壳 + mock，但带 mock 的页面最多只能勾「壳验收」（空态/错误态/权限/禁用词），数据类条目一律不勾（`VITE_MOCK=1` 顶部黄条必须在）。
3. **证据留痕**：每条勾选要能落到「截图 / 接口响应 / SQL / 负向用例名」之一；不接受「我看着对」。
4. **双端各验一遍**：凡标「管理 + 开发」的页面，管理端与开发者端各出一张单；开发者端的负向断言用 `devSensitiveKeys` 那组键，不许自拟。

---

## 1 通用口径（13 页都适用，不重复写进每页）

| # | 项 | 口径 |
|---|---|---|
| G1 | 三态必备 | 每个列表/卡片/图表都必须有**加载态、空态、错误态**；错误态显示 `request_id`（或 `X-Request-Id`）+ 重试按钮，不许只显示「加载失败」。 |
| G2 | 空值语义 | 「未计算 / 未配置 / 历史未知」显示专属文案或「—」，**绝不许显示 0**；真实 0 显示 `0.00000`。依据：`ops.go` 的计数列是 `*int64`，null 有意义。 |
| G3 | 金额单位 | 页面金额一律标「积分」；「元」只出现在订阅录入（并实时折算积分）。 |
| G4 | 筛选可还原 | 筛选条件写进 URL（query），刷新/分享/后退结果一致；后端返回 400 时定位到具体参数名。 |
| G5 | 错误码形状 | 统一用 `resp` 的数字码：`40001` 参数、`40101` 未登录、`40301` 无权限、`40302` 需改密、`40401` 不存在、`40901` 冲突、`42901` 限流、`50001` 内部错。业务细分原因放 `Reason` 字符串，前端按 `Reason` 分支。 |
| G6 | 权限双端 | 前端守卫（`meta.roles` + `/403`）只是体验层，**接口必须另有一道**：`/api/v1/admin/*` 需 ADMIN、`/api/v1/dev/*` 需 DEVELOPER。只验前端 = 不算通过。 |
| G7 | 禁用词 | 13 页不得出现「渠道 / 渠道协议 / 渠道密钥 / 标签 / Auto 模型 / 会话列表」入口或菜单；`frontend/src/router/index.ts` 里的 `channels`、`channel-protocols`、`auto-models`、`tags`、`sessions`、`cost` 路由必须删除（不是隐藏）。 |
| G8 | 不 500 | 任何页面不得引用 0024 已删表/列；本地起服务点一遍全部 13 页，出现 500 即阻断合入。 |

---

## 2 逐页清单

### P01 统计概览（`/admin/dashboard`、`/dev/dashboard`）

| 维度 | 口径 |
|---|---|
| 数据来源接口 | `GET /api/v1/admin/stats/dashboard`（M1 改造）、`GET /api/v1/admin/stats/live`（M2 改造）；开发者端同结构 `GET /api/v1/dev/stats/dashboard`、`/dev/stats/live`。关键字段：KPI 窗口（`WindowFrom/WindowTo/WindowMode`）、请求数/成功率/三段 token、`Revenue`、`Cost`、`MarginPct`、成本来源四段（`ledger`/`amortized`/`none`/未配置）、近 60 分钟 `RPM/TPM/活跃令牌/在飞请求`。 |
| 空态 | 时间窗内无账单 → 四段成本条显示「暂无账单」+「查看账单」按钮，**不画全 0 饼图**；实时区无流量 → 「近 60 分钟无请求」并保持 30s 轮询。 |
| 错误态 | 40001 → 顶部条「时间范围不合法」并回退到近 24h；50001 → 卡片位显示 `request_id` + 重试（单卡片失败不影响其它卡片）；40101 → 跳登录并带 `redirect`。 |
| 权限 | 管理端看全量；开发者端只看**本人**（后端强制 self）。开发者端不得出现成本/毛利/供应商/账号/内部模型任何字段（`devSensitiveKeys`）；负向断言必做。 |

验收：
- [ ] `[P0]` 页面无「会话 / 渠道 / 密钥 / Auto」字样与入口（G7）。
- [ ] `[P0]` `cost_source IS NULL` 显示「成本未配置」、`none` 显示「无成本口径」，两者文案不同且都不显示 0.00（G2）。
- [ ] `[P0]` 开发者端响应体不含 `Cost`、`CostSource`、`UpstreamProvider`、`UpstreamAccount`、`RValue`、`Rates`（按 `devSensitiveKeys` 断言）。
- [ ] `[P1]` 收入为 0 时毛利率「—」（接口 `MarginPct=null`），不显示 0%/100%。
- [ ] `[P1]` 切时间范围后所有卡片同步刷新，无「部分卡片停旧窗口」。
- [ ] `[P1]` 实时区只有速率类指标，无会话/在线用户列表。

### P02 上游拓扑（`/admin/topology`）

| 维度 | 口径 |
|---|---|
| 数据来源接口 | `GET /api/v1/admin/topology`（T1 **新增**：provider 列表、内部模型含 `stale/kind/context_len`、对外模型绑定、未绑定计数、最后同步时间/结果）；`POST /api/v1/admin/topology/sync`（T3 **新增**）；`PUT /api/v1/admin/topology/links`（T2 可延后）。真源：`internal_models`、`external_models.bound_internal_ref`、`model_links`（坐标）、`sys_configs`（上游三键，仅系统页可读）。 |
| 空态 | 从未同步 → 「尚未同步网关拓扑。同步前录入订阅、绑定模型都会被拒绝」+ 主按钮「立即同步」；对外模型为 0 → 「还没有对外模型，先去模型页新建」。 |
| 错误态 | 同步失败 → 红色条：失败原因 + 上次成功时间 + 「重试」，**不清空**已有快照；T2 未就绪 → 拖拽后提示「坐标暂不保存，刷新按自动布局」；40101/40301 按 G6。 |
| 权限 | **仅 ADMIN**（路由 `/admin/*`、接口 `/api/v1/admin/*`）。开发者端无入口、无接口；网关 `base_url` 与任何凭据**不得**在本页出现（只显示 provider/账号/模型名）。 |

验收：
- [ ] `[P0]` 四层关系与服务端数据一致：未绑定内部模型计数 = 列表条数。
- [ ] `[P0]` `stale=true` 节点灰化 + 「上期快照」标识；同步成功后消失。
- [ ] `[P0]` 同步失败保留旧数据，页面不空白（有失败原因与上次成功时间）。
- [ ] `[P0]` 页面不含 `upstream.base_url`、密钥、token 任何内容。
- [ ] `[P1]` 从 P04 收到 `topology_not_synced` 能一跳回到本页。
- [ ] `[P1]` 移动端 375px 四列视图可切换为列表（不横向滚动）。

### P03 对外模型与绑定（`/admin/models`、`/dev/models`）

| 维度 | 口径 |
|---|---|
| 数据来源接口 | 既有 `GET/POST/PUT /api/v1/admin/models`、`/models/{id}/price-reference`、`/apply-price`、`/sync-prices`（M3 改造：去 `magpie_model`/渠道/限流/探活字段、加 `bound_internal_ref`）；绑定候选 `GET /api/v1/admin/internal-models`（T4 **新增**）；绑定写 `PUT /api/v1/admin/models/{id}/binding`（T5 **新增**）；开发者端 `GET /api/v1/dev/models`（只返回已绑定且启用）。 |
| 空态 | 候选为空 → 「先同步拓扑」+ 跳 P02；未绑定 → 行内「不售卖」标签；开发者端无已绑定模型 → 「暂无可用模型，请联系管理员」。 |
| 错误态 | 绑定不存在的 ref → 40001 + 「该内部模型不存在」；绑定 `stale` ref → 40001 + 「该内部模型本轮未在网关出现，请重新选择」；售价字段非法 → 字段级红字并保留输入。 |
| 权限 | 管理端全量读写；开发者端**只读**自己的售卖面：不得返回 `bound_internal_ref`、内部模型名/id、成本、供应商、账号（`devSensitiveKeys`）。开发者端保存按钮不存在，直连接口 → 40301。 |

验收：
- [ ] `[P0]` 绑定保存后 `GET /v1/models` 出现该模型；解绑后消失。
- [ ] `[P0]` 未绑定模型在管理端标「不售卖」，且开发者端不可见。
- [ ] `[P0]` 开发者端响应负向断言：无 `bound_internal_ref`/`internal_model_id`/`magpie_model`/`rate_limit`/`health_probe`。
- [ ] `[P0]` 绑定只接受 T4 返回的 ref；自由文本落库必 40001。
- [ ] `[P1]` 三段售价、上下文分档、时段系数与 `/v1` 实际扣费口径一致（抽一单核对）。

### P04 订阅账本（`/admin/subscriptions`）

| 维度 | 口径 |
|---|---|
| 数据来源接口 | `GET /api/v1/admin/subscriptions`（S1）、`POST`（S2）、`PUT /{id}`（S3）、`GET /{id}/allocations`（S4）、`POST /{id}/roll-cycle`（S5）、`POST /amortize/run`（S6）、`GET /form-options`（S7）——全部**新增**，契约见 `2026-11-16-subscription-entry-standard.md` §7。字段/校验/原因码/回显字段一律以该文档为准。 |
| 空态 | 无订阅 → 「还没有订阅。录入后 0–5 分钟（下一轮）开始分摊；不录 = 盈亏页成本永远少一笔固定支出」+ 新建按钮；`topology_not_synced` → 阻断并跳 P02。 |
| 错误态 | 40001 字段级（保留输入）；`duplicate_subscription` → 40901 弹「已存在（ID …），去编辑／取消」；`stale_object` → 40901 弹「已被他人修改，刷新后重试」并展示差异；`rate_not_configured` → 422/400 提示去系统页配 `billing.cny_rate`；`topology_not_synced` → 40901 阻断；42901 → 「操作太频繁，10 秒后再试」；40301 → 无权限页。 |
| 权限 | **仅 ADMIN**（接口 `/api/v1/admin/subscriptions*`，写操作留审计）。开发者端零入口、零接口，直连 40301 且不返回行内容。 |

验收：
- [ ] `[P0]` 符合 C1–C4 的订阅录入后 `WillAmortize=true`、`BlockReasons=[]`（见订阅文档 §5.3）。
- [ ] `[P0]` ≤5 分钟（或点 S6）后当期账单出现 `cost_source='amortized'`，`Σcost_credits = PriceCredits`（±0.00001）。
- [ ] `[P0]` 同 `(Provider, Account)` 再录 → 40901 `duplicate_subscription`，表内仍 1 行。
- [ ] `[P0]` 「本期已摊 + 未摊出 = 整期费用」可在页面核对；「未摊出」是**现算**（`Unallocated` 只进日志，不落库）。
- [ ] `[P0]` 开发者端直连六个端点全 40301；改价/结转/重算都有审计记录。
- [ ] `[P1]` `cycle_start` 空/过期时状态为「缺账期」「已结束需结转」，不得显示「分摊中」。
- [ ] `[P1]` `expired` 行置顶橙色；「结转到本期」后变 `active` 并能正常摊。
- [ ] `[P1]` 移动端 375px 单列表单可用，无横向滚动。

### P05 账本流水（`/admin/ledger`）

| 维度 | 口径 |
|---|---|
| 数据来源接口 | `GET /api/v1/admin/ledger/entries`（L1 **新增**：时间/provider/account/model/`ledger_id` 精确查，返回 total）、`GET /api/v1/admin/ledger/status`（L2 **新增**：`max(occurred_at)` 滞后、最近成功、`blocked`/`error` 窗口数、待补拉 offset、预算是否耗尽）。真源：`ledger_entries`、`reconcile_windows`。 |
| 空态 | `ledger_enabled=false` → 「账本拉取已关闭」+ 去系统页；从未拉到 → 「账本为空，成本只剩订阅摊销」（并把这句话写进盈亏页说明）。 |
| 错误态 | 50001 → 卡片显示 `request_id` + 重试；40001（时间越界/格式错）→ 定位参数；`ledger_id` 查无 → 「无此条目」空结果态，不报错。 |
| 权限 | **仅 ADMIN**（成本真源，开发者端永不下发）。页面不出现金额的币种换算、不出现供应商密钥。 |

验收：
- [ ] `[P0]` 滞后 >180s 橙色「账本不新鲜」，>15 分钟红色「对账不可用」。
- [ ] `[P0]` 列表 total 来自服务端；`Limit`/分页不冒充总数。
- [ ] `[P0]` 开发者端无任何入口；直连 `/api/v1/dev/ledger*` 404/403（不存在也不报 500）。
- [ ] `[P1]` `blocked`/`error` 窗口数 >0 时给出跳 P06 的链接。
- [ ] `[P1]` `ledger_id` 一键复制；同 `ledger_id` 不重复出现。

### P06 对账窗口（`/admin/reconcile`）

| 维度 | 口径 |
|---|---|
| 数据来源接口 | `GET /api/v1/admin/ops/reconcile-windows`（M4 **改造**，已有实现：`From/To/Limit`，`Items[]` 含 `Status/AlarmStreak/LocalCount/UpstreamCount/LocalTokens/UpstreamTokens/DeviationRate/ScrollOffset/CheckedAt`）；`GET /api/v1/admin/ops/reconcile-summary`（O1 可延后，未就绪时前端按当前页聚合并**标注「按当前页统计」**）。状态五值固定：`ok/alarm/error/blocked/skipped`（`ck_reconcile_status`）。 |
| 空态 | 无窗口 → 「同步未运行或账本关闭」+ 原因区分（`ledger_enabled=false` / 同步未启动）；`skipped` 行没有 `skip_reason` 时**不许**显示「已对账」，显示「未参与比对（原因缺失）」并提示报 Core。 |
| 错误态 | 40001（`From` 非 RFC3339 / `To<=From` / `Limit` 非法）→ 字段级提示；50001 → `request_id` + 重试；`Limit` 截断 → 顶部提示「已截断到 N 条」。 |
| 权限 | **仅 ADMIN**。开发者端无入口、无接口（对账是运维面）。 |

验收：
- [ ] `[P0]` 计数列 null 显示「—」+「未计算」，**不显示 0**（接口字段是 `*int64`）。
- [ ] `[P0]` `skipped` 显示四种原因之一的中文文案（`ledger_disabled`/`ledger_unavailable`/`fetch_blocked`/`service_down`）；接口当前只取 `detail->>'deviation_rate'`，未返回 `skip_reason` → 必须补，否则本条不通过。
- [ ] `[P0]` `blocked` 行显示待补拉进度（`ScrollOffset`），`error` 行显示原因。
- [ ] `[P0]` 接口无 total 且默认 500/上限 2000：页面必须显示截断提示，不许把当前页当全量。
- [ ] `[P1]` `alarm_streak ≥ 3` 高亮并可一键筛出。
- [ ] `[P1]` 页面不出现金额对比（只比笔数与 token）。

### P07 盈亏（`/admin/pnl`）

| 维度 | 口径 |
|---|---|
| 数据来源接口 | `GET /api/v1/admin/ops/pnl`（M5 **改造**：`From/To`（默认 24h，≤366 天）、`GroupBy=provider/account/model`；返回 `Requests/Tokens/Revenue/Cost/Margin/MarginPct/LedgerCost/AmortizedCost/Uncosted/Groups[]/Truncated`）；`GET /api/v1/admin/ops/pnl/trend`（O2 **新增**）。 |
| 空态 | 无收入 → 汇总卡显示「本期无收入」，毛利率「—」（`MarginPct=null`）；有收入无成本 → 顶部黄条「成本数据缺失，毛利偏高」+ 成本列显示「无数据」。 |
| 错误态 | 40001（`From` 非 RFC3339、`To<=From`、区间 >366 天、`GroupBy` 非三值）→ 定位参数并回退默认；`Truncated=true` → 提示「分组超过 200，只显示前 200」（这是正常态，不当错误）；50001 → `request_id` + 重试。 |
| 权限 | **仅 ADMIN**：毛利 = 成本侧数据，开发者端无入口无接口。 |

验收：
- [ ] `[P0]` `收入 − 成本 = 毛利`、`毛利 ÷ 收入 = 毛利率`，5 位小数后仍成立；`LedgerCost + AmortizedCost = Cost`（分币级）。
- [ ] `[P0]` **`Uncosted` 必须拆两项**：接口现实现是 `count(*) FILTER (WHERE cost_credits IS NULL)`，而 0020 的 `ck_billing_cost_presence` 规定「成本为 NULL ⇒ `cost_source='none'`」，两者混算 → 必须分别返回「未配置（历史 NULL）」与「无成本口径（none）」，页面分别文案。
- [ ] `[P0]` **「模型」维度键必须先定**：接口现按 `internal_model_id` 分组（`pnlGroupExpr`），管理端会看到内部模型 id；开发者端又不允许出现内部模型（`devSensitiveKeys`）→ 需拍板为对外模型名，未定前该维度不得上线。
- [ ] `[P0]` 开发者端无入口；直连返回 40301。
- [ ] `[P1]` `Truncated` 有提示；切分组维度后各维度合计相等（同一时间窗）。
- [ ] `[P1]` 页面不显示渠道/密钥维度（G7）。

### P08 账单查询（`/admin/billings`、`/dev/billings`）

| 维度 | 口径 |
|---|---|
| 数据来源接口 | `GET /api/v1/admin/billings`、`GET /api/v1/admin/billings/stats`（M6/M7 改造：加 `UpstreamProvider/UpstreamAccount/UpstreamProtocol/CostSource/HasLedger` 筛选，去 `ChannelKeyID`）；开发者端 `GET /api/v1/dev/billings`、`/dev/billings/stats`（强制 self）。 |
| 空态 | 筛选无结果 → 保留条件 + 「清除筛选」；首屏无数据 → 「所选时间窗没有账单」。 |
| 错误态 | 40001 → 定位到具体筛选参数并保留其它条件；带旧参数 `ChannelKeyID` 的链接**忽略该参数**不得 500；50001 → `request_id` + 重试。 |
| 权限 | 管理端全量；开发者端只看本人（后端强制 self，越权 id 返回 40401 或空集，不得 200 返回他人数据）。开发者端不出现成本列、他人用户/令牌、内部模型、上游账号（`devSensitiveKeys`）。 |

验收：
- [ ] `[P0]` 筛选写 URL，刷新/分享结果一致（G4）。
- [ ] `[P0]` `cost_source IS NULL`（历史）与 `none`（无成本口径）文案与颜色不同，均不显示 0。
- [ ] `[P0]` 开发者端负向断言：无 `Cost`/`CostSource`/`UpstreamAccount`/`InternalModelID`/`UserID`/`TokenName`。
- [ ] `[P0]` `Total` = 请求数（`Group=0`），且与分页总数一致。
- [ ] `[P1]` 行点击进 P09，返回后筛选条件不丢。
- [ ] `[P1]` 旧链接带 `ChannelKeyID` 不 500。

### P09 账单详情（`/admin/billings/:id`、`/dev/billings/:id`）

| 维度 | 口径 |
|---|---|
| 数据来源接口 | `GET /api/v1/admin/billings/{billing_id}`（M8 改造：管理端补归因七列、`cost_source/cost_credits`、`credit_holds`、`ledger_entry`；移除 `ChannelKeyID/KeyName/ChannelName`）；开发者端 `GET /api/v1/dev/billings/{billing_id}`（白名单重做）。跳转源：P08 行点击、P05 `ledger_entry_id` 反查、P06 排障。 |
| 空态 | 账本条目未匹配 → 「成本待结算」（不是 0）；失败请求无预扣 → 「未预扣 / 已退款」；无归因 → 「无归因（`upstream_account` 为空）」+ 说明这可能造成月费摊不出去。 |
| 错误态 | 40401 → 「账单不存在或已被清理」+ 返回列表；40301 → 无权限页；50001 → `request_id` + 重试。 |
| 权限 | 管理端全量（含成本、预扣、账本）；开发者端只保留请求侧（用量、状态、错误、耗时、`request_id`），成本/上游账号/内部模型/费率一律无。 |

验收：
- [ ] `[P0]` 金额恒等式：`预扣 − 退款 + 差额结算 = 实扣积分`（或「预扣 − 实扣 = 退回」），页面把等式结果显式写出来。
- [ ] `[P0]` `none` 与 `NULL` 文案不同（「无成本口径」vs「历史未知」）。
- [ ] `[P0]` 账本匹配时显示 `ledger_id`（可跳 P05），未匹配显示「待结算」不显示 0。
- [ ] `[P0]` 开发者端负向断言：无 `Cost`/`CostSource`/`Rates`/`RValue`/`UpstreamAccount`/`InternalModelID`。
- [ ] `[P1]` 复制 `request_id` 能在 P08 精确查回本单。

### P10 用户与钱包（`/admin/users`）

| 维度 | 口径 |
|---|---|
| 数据来源接口 | 既有：`GET/POST /api/v1/admin/users`、`PUT /users/{id}`、`POST /users/batch-delete`、`/users/{id}/reset-password`、`/reset-totp`、`GET /users/{id}`、`GET /users/{id}/wallet`、`POST /wallet/recharge|set|adjust`、`GET /wallet/flows`（**零改造**）。 |
| 空态 | 无用户 → 「还没有用户」+ 新建；钱包无流水 → 「暂无资金变动」。 |
| 错误态 | 负余额 → 40001 并点名字段、保留输入；余额不一致/并发 → 40901；密码策略不符 → 字段级；`42901` → 提示稍后。 |
| 权限 | **仅 ADMIN**。整页含他人余额与流水，开发者端无入口；`/api/v1/dev/users*` 不存在（404，不得 500）。 |

验收：
- [ ] `[P0]` 充值/设为/调整后余额 = 流水末行「余额后」（分币级）。
- [ ] `[P0]` 不允许负余额；错误文案点名字段。
- [ ] `[P0]` `pricing_mode=cost` 在页面上**不可选**（V2 无成本单价真源，口径 D-1），已有 `cost` 用户有明确标注且不影响历史账单展示。
- [ ] `[P1]` 列表与详情余额一致。
- [ ] `[P1]` 403：非 ADMIN 访问 `/admin/users` → `/403`；直连接口 40301。

### P11 令牌（`/admin/tokens`、`/dev/tokens`）

| 维度 | 口径 |
|---|---|
| 数据来源接口 | 管理端 `GET/POST /api/v1/admin/tokens`、`POST /tokens/batch-delete`、`GET /tokens/{id}/secret`（敏感读带审计）；开发者端 `GET/POST /api/v1/dev/tokens`、`POST /{id}/rotate`、`/{id}/toggle`、`GET /{id}/secret`（M9 改造：删 `tag_id` 全套读写）。 |
| 空态 | 无令牌 → 「还没有令牌」+ 新建；开发者端无令牌 → 引导创建并给出示例调用片段。 |
| 错误态 | 密钥查看失败 → 40001/50001 + `request_id`；过期令牌操作 → 40001「令牌已过期」；越权 id → 40401（不区分不存在与无权）；40302 → 强制改密。 |
| 权限 | 管理端全量（含他人令牌，创建时可选用户）；开发者端只能看/操作**本人**令牌，越权 40301/40401。查看密钥需二次确认，且必须留审计（`WithAuditSensitiveRead` 已存在）。 |

验收：
- [ ] `[P0]` 页面与接口均无「标签」概念（列/筛选/表单/DTO）；0024 后列表/创建/轮换不报 42703。
- [ ] `[P0]` 查看密钥有二次确认 + 审计记录可查。
- [ ] `[P0]` 开发者端越权访问他人令牌 → 40301/40401，绝不返回密钥。
- [ ] `[P1]` 过期令牌状态与时间正确，过期后调用 `/v1` 被拒。

### P12 系统设置（`/admin/system`）

| 维度 | 口径 |
|---|---|
| 数据来源接口 | `GET /api/v1/admin/configs/system`、`PUT /api/v1/admin/configs/system`（M10 改造）。三键：`upstream.base_url`（仅 http/https、拒绝 `user:pass@`、去尾斜杠）、`upstream.timeouts{stream_first_byte_ms,stream_idle_ms}`（1000–600000ms）、`upstream.degrade{passthrough_enabled,ledger_enabled,sync_enabled}`（布尔）。旧键 `upstream.timeout_ms` 与布尔版 `upstream.degrade` **出现即 400**。 |
| 空态 | `base_url` 为空 → 「未接入：`/v1` 走 V1 渠道路由（V2 转发未接入）」提示条，不阻断其它块的保存。 |
| 错误态 | 40001 字段级（带凭据的 URL、非 http(s)、超时越界、非布尔）→ **只回滚该字段**，不整块丢弃；50001 → `request_id` + 保留编辑内容。 |
| 权限 | **仅 ADMIN**。页面**不得**出现任何 secret/key；`base_url` 也只在管理端可见（不写日志、不下发 dev）。 |

验收：
- [ ] `[P0]` 三键保存后回读一致；带凭据/非 http(s) 的 `base_url` 返回 40001。
- [ ] `[P0]` 旧键不再出现在页面；提交旧键返回 400。
- [ ] `[P0]` **改完是否立即生效要实测**：已知 `upstream.ConfigProvider.Invalidate()` 在装配层是否挂上 `SetConfigWriteHook` 存疑（历史记录里「无生产调用点、缓存无 TTL」）。不重启生效 = 通过；需重启才生效 = 本条挂红并登记（不许写成「已验证」）。
- [ ] `[P0]` 页面无 secret/key 字段；`base_url` 不出现在开发者端任何响应。
- [ ] `[P1]` `degrade.passthrough_enabled=false` 后 `/v1/*` 返回 503（先解决上一条的生效问题再验）。

### P13 公告（`/admin/announcements`、`/dev/announcements`）

| 维度 | 口径 |
|---|---|
| 数据来源接口 | 管理端 `GET/POST /api/v1/admin/announcements`、`PUT /{id}`、`POST /batch-delete`；开发者端**只有** `GET /api/v1/dev/announcements`（列表，无详情端点）。**当前事实**：`frontend/src/router/index.ts` 的 `/dev` 子路由与 `DevLayout` 菜单里**没有公告入口**，只有 `src/api/dev.ts` 的接口封装 → 开发端「阅读」在页面上不可达。 |
| 空态 | 管理端无公告 → 「还没有公告」+ 新建；开发者端无公告 → 不显示入口（不显示空页面）。 |
| 错误态 | 发布失败 → 保留草稿 + 40001 字段级；50001 → `request_id` + 重试；40401（改/删不存在）→ 提示刷新列表。 |
| 权限 | 管理端读写；开发者端**只读**（无编辑/删除入口，直连 POST/PUT → 40301/404）。未登录访问 → `/login?redirect=`。 |

验收：
- [ ] `[P0]` 发布后开发端可见、下架后不可见（同一份数据源）。
- [ ] `[P0]` 开发端无任何编辑入口（负向断言：无 `POST/PUT/DELETE` 调用、按钮不存在）。
- [ ] `[P0]` **开发端公告入口必须存在**：补 `/dev/announcements` 路由 + 菜单或顶栏通知位（未读计数）；不补则 P13 只能算管理端一页，与「13 页含开发端阅读」口径冲突，需主持人拍板后再定验收。
- [ ] `[P1]` 列表状态与操作结果即时一致（发布/下架不用手动刷新）。
- [ ] `[P1]` 公告内容渲染安全（Markdown/富文本无内联脚本执行）。

---

## 3 权限矩阵（Gate 按此出「越权用例」）

| 页面 | 管理端 ADMIN | 开发端 DEVELOPER | 后端接口前缀 | 越权期望 |
|---|---|---|---|---|
| P01 统计 | 全量 | 本人 | `/api/v1/admin/stats/*`、`/api/v1/dev/stats/*` | dev 直连 admin → 40301 |
| P02 拓扑 | 读写 | 无 | `/api/v1/admin/topology*` | dev 无路由、接口 40301 |
| P03 模型 | 读写 | 只读（白名单） | `/api/v1/admin/models*`、`/api/v1/dev/models` | dev 写 → 40301 |
| P04 订阅 | 读写 | 无 | `/api/v1/admin/subscriptions*` | dev 全套 40301 |
| P05 账本 | 读写 | 无 | `/api/v1/admin/ledger*` | dev 40301 |
| P06 对账 | 读写 | 无 | `/api/v1/admin/ops/reconcile*` | dev 40301 |
| P07 盈亏 | 只读 | 无 | `/api/v1/admin/ops/pnl*` | dev 40301 |
| P08 账单 | 全量 | 本人 | `/api/v1/admin/billings*`、`/api/v1/dev/billings*` | dev 拿他人 id → 40401/空集 |
| P09 详情 | 全量 | 请求侧白名单 | 同上 `/{id}` | dev 响应无成本键 |
| P10 用户 | 读写 | 无 | `/api/v1/admin/users*` | dev 40301 |
| P11 令牌 | 全量 | 本人 | `/api/v1/admin/tokens*`、`/api/v1/dev/tokens*` | dev 他人 id → 40401 |
| P12 系统 | 读写 | 无 | `/api/v1/admin/configs/system` | dev 40301 |
| P13 公告 | 读写 | 只读 | `/api/v1/admin/announcements*`、`/api/v1/dev/announcements` | dev 写 → 40301/405 |

前端守卫事实（供 Gate 设计用例，`router/index.ts`）：未登录 → `/login?redirect=…`；已登录访问登录页 → 按角色回首页；角色不符 → `/403`；`mustChangePassword` → 强制改密页。**只审计前端守卫不算权限通过。**

---

## 4 本轮核对出的口径修正与阻断项（都有代码依据）

| # | 事项 | 依据 | 处置 |
|---|---|---|---|
| A-1 | P07 `Uncosted` 把「未配置」与「无成本口径」混算 | `ops.go`：`count(*) FILTER (WHERE cost_credits IS NULL)`；`0020` 的 `ck_billing_cost_presence` 规定成本 NULL ⇒ `cost_source='none'` | Core 拆两列（M5 必须做）；页面分别文案 |
| A-2 | P07 `GroupBy=model` 用内部模型 id 分组 | `ops.go` 的 `pnlGroupExpr`：`COALESCE(internal_model_id,'(unknown)')` | 产品拍板维度键（建议对外模型名）；未定前该维度不上线 |
| A-3 | P06 `skipped` 无法解释 | `HandleReconcileWindows` 只取 `detail->>'deviation_rate'`，无 `skip_reason` | M4 补 `skip_reason`（独立字段或从 `detail` 取出），否则 `skipped` 行只能显示「原因缺失」 |
| A-4 | P06 无 total，默认 500/上限 2000 | 同上（`Limit` 默认 500、`>2000` 截断） | 页面显示截断提示；要「近 24h 全量」需补 total 或分页游标 |
| A-5 | P13 开发端公告页不存在 | `router/index.ts` 无 `/dev/announcements`，`DevLayout` 5 项菜单无公告；仅 `api/dev.ts` 有封装 | 补路由 + 入口，或把 P13 口径改为「管理端 1 页」（需主持人拍板） |
| A-6 | P12 配置「保存即生效」不可假定 | `sync-config` 分支历史记录：`ConfigProvider.Invalidate()` 曾无生产调用点、缓存无 TTL | 验收必须实测；不生效即挂红，不许写「已验证」 |
| A-7 | 13 页与现有路由冲突 | `router/index.ts` 仍含 `channels`/`channel-protocols`/`auto-models`/`tags`/`sessions`/`cost` | 删除（不是隐藏）；`/admin/cost` 页面 0024 后必 500，属阻断项 |
| A-8 | dev 负向断言不要自拟 | `console/dev_view.go:51-60` 的 `devSensitiveKeys` 是单一来源 | Gate 直接断言这组键不出现在 dev 响应里 |

---

## 5 阻断项汇总（出现任一条 → 该页不可合入）

1. 页面出现「渠道 / 渠道密钥 / 标签 / Auto 模型 / 会话」入口，或残留 `/admin/cost`（A-7）。
2. 空值/未计算被显示成 0（G2）；盈亏 `Uncosted` 未拆两项（A-1）时 P07 直接阻断。
3. 开发者端出现成本、上游账号/供应商、内部模型、他人用户/令牌（按 `devSensitiveKeys` 断言）。
4. 只有前端守卫、接口无角色校验（G6）；或越权返回行内容而不是 40301/40401。
5. 页面引用 0024 已删表/列导致 500（G8）。
6. 带 mock 数据通过数据类验收（§0 第 2 条）。
7. P12 配置保存后需重启才生效却写成「已验证」（A-6）。
