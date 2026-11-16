# 灵枢 V2 阶段三 · 前端 13 页（页面清单 / 完成度 / 依赖接口 / Spec 对齐记录）

> 2026-11-16 · View（前端）· 分支 `feat/v2-console-ops-pages`（基于 `origin/main` @ `e5b579f`，与 `origin/v2` 同点）
> 依据（按优先级）：
> 1. `docs/designs/2026-11-16-stage3-frontend-13-pages.md`（Spec：13 页清单 + 逐页验收口径，**页面职责的唯一依据**）
> 2. `docs/designs/2026-11-16-subscription-entry-standard.md`（Spec：订阅录入标准，**P04 字段/校验/文案的唯一依据**）
> 3. `docs/designs/2026-11-15-v2-p1-data-contract.md`（表结构 / 枚举 / 空值语义）
> 4. `backend/internal/domain/console/ops.go`（盈亏 / 对账已定型接口的字段与语义）
>
> 本文只写「前端做了什么、和 Spec 差在哪、差在哪个接口」，不重复后端方案，也不实现 Spec §4 的 26 条接口。

---

## 0 一句话

13 页全部落地：**5 页新建**（上游拓扑 / 订阅账本 / 账本流水 / 对账窗口 / 盈亏）+ **6 页按 V2 口径改造**（统计沿用接口未改、模型 / 账单 / 账单详情 / 令牌 / 系统 / 用户）+ **2 页沿用**（公告）；
其中**盈亏、对账**两个接口已定型，合入即取真数据；其余依赖的 20 余条接口未上线 → 页面出「未就绪空态 + 显式演示数据」，接口上线后**不改页面代码**（只删 mock 文件与开关）。

---

## 1 13 页清单（对齐 Spec §2 编号）

| Spec | 页面 | 路由 | 完成度 | 依赖接口 | 状态 |
|---|---|---|---|---|---|
| P01 | 统计概览 | `/admin/dashboard` | 沿用（M1/M2 未合入） | M1 dashboard、M2 live | **待接口**（见 §5-1） |
| P02 | 上游拓扑 | `/admin/topology` | 同步状态条 + 供应商层 + 快照表 + 画布；账号层待 T1 | T1 `/admin/topology`、T3 `/admin/topology/sync`、T2 `/admin/topology/links` | 空态可交付 |
| P03 | 对外模型与绑定 | `/admin/models` | 绑定列 + 未绑定不售卖横幅 + 绑定入口（禁用，待 T5） | M3、T4 `/admin/internal-models`、T5 `/admin/models/{id}/binding` | 已改造 |
| P04 | 订阅账本 | `/admin/subscriptions` | 列表 + 录入表单**按录入标准全量对齐**（见 §4） | S1–S7 | 空态可交付 |
| P05 | 账本流水 | `/admin/ledger` | 流水 + 新鲜度卡（滞后 180s/15min 两级）+ ledger_id 精确查与复制 | L1 `/admin/ledger/entries`、L2 `/admin/ledger/status` | 空态可交付 |
| P06 | 对账窗口 | `/admin/reconcile` | 完整（五状态、null≠0、skip_reason 中文、streak≥3 高亮置顶、行展开 detail、按当前页统计标注） | M4 `/admin/ops/reconcile-windows`、O1 汇总（可延后） | 待接口合入 |
| P07 | 盈亏 | `/admin/pnl` | 完整（变动/固定成本分列、恒等式自检、Uncosted 拆两段、分组排序、Uncosted 跳账单） | M5 `/admin/ops/pnl`、O2 `/admin/ops/pnl/trend` | 待接口合入 |
| P08 | 账单查询 | `/admin/billings` | 仅加「归因 / 成本来源」列（字段缺失不渲染） | M6、M7 | **部分**（筛选/URL 待 M6，见 §5-4） |
| P09 | 账单详情 | `/admin/billings/{id}` | 归因块 + 成本来源 + 账本条目 + **资金轨迹块（预扣/退款/结算差额/钱包流水 + 恒等式）** | M8 | 部分（字段待 M8） |
| P10 | 用户与钱包 | `/admin/users` | 沿用 + **「按成本」按钮禁用并说明口径 D-1** | 既有接口 | 已改造 |
| P11 | 令牌 | `/admin/tokens` | 沿用 + **删除「语义标签」表单项与 TagID 提交** | M9 | 已改造 |
| P12 | 系统设置 | `/admin/system` | 新增「上游（V2）」页签（base_url / 两超时 / degrade 三开关 / 订阅池前缀），**只读** | M10 | **只读**（见 §5-6） |
| P13 | 公告 | `/admin/announcements` | 沿用 | 无变化 | 可用 |

> 会话 / 渠道 / 渠道协议 / 渠道密钥 / 标签 / Auto 模型 / 服务状态七类页按 Spec §1.2 不入 13 页；V1 成本页由 P07 取代。

---

## 2 三条会改实现的口径

### 2.1 接口未就绪 ≠ 出错 ≠ 0

| 状态 | 触发 | 表现 |
|---|---|---|
| `live` | 200 | 正常渲染 |
| `pending` | 404 / 405 / 501 / 403 | 灰底横幅写明**是哪几个接口**没上线 + 「预览演示数据」按钮；**默认不显示任何数字** |
| `error` | 其它 | `ErrorBubble`（带可复制 requestId） |

实现：`src/api/v2.ts` 的 `v2Get` / `v2Post` 把四类状态码归一成 `pending`（走 `http` 的 `silentStatuses`，不弹全局红条）；
`src/composables/useV2Load.ts` 管四态与演示开关；`src/components/v2/V2Page.vue` 是统一页壳。
写接口（`v2Post`）额外把 400/409/422 当**业务结果**交给页面（字段级定位 / 409 分支），不吞进全局红条。

### 2.2 演示数据默认关，开了就挂黄条

财务页默认显示假数字是事故源。**只有用户点「预览演示数据」才渲染**，渲染时页头挂「演示数据 · 接口待接入」标签 + 横幅
「当前数字不是真实账务结果，不能用于对账、结算或对外汇报」，并提供「退出预览」立刻回到真实请求。

### 2.3 `null` = 还没算过，绝不是 0

- 对账计数四列：`null` → 「未算」；`0` → 「0」（真实 0 笔）。
- 订阅 `PriceCredits / CycleAllocated / CycleUnallocated`：`null` → 「未计算 / 未折算」。
- 盈亏 `MarginPct`：`null` → 「—」（收入 ≤ 0）；`Uncosted` 拆「历史未配置（NULL）」与「明确无口径（none）」两段，不合并。
- 账单 `CostSource`：`NULL` → 「历史（口径未知）」；`none` → 「无成本口径」，颜色与文案都不同。

---

## 3 mock 规则（Spec §5 落地）

| Spec 规则 | 落地 |
|---|---|
| `VITE_MOCK=1` 时由 `src/mock/<page>.ts` 提供数据 | ✅ `src/mock/{topology,subscriptions,ledger,reconcile,pnl}.ts` + `src/mock/index.ts` 的 `MOCK_ENABLED` |
| 一页一文件、字段名与契约逐字一致 | ✅ 每个 mock 文件按 `@/api/v2` 的类型构造，TS 直接约束字段名 |
| 顶部固定黄条「演示数据 · 接口待接入」 | ✅ `V2Page` 的 demo 横幅（同时挂页头标签） |
| 接口就绪后同一 PR 删 mock 与开关 | 接口合入时删对应 `src/mock/<page>.ts` 一个文件即可，页面不用改 |
| 不许按钮点了没反应 | ✅ 未就绪能力一律 `disabled` + tooltip 说明（P02 立即同步、P03 绑定、P04 分摊明细/结转） |

> 与「显式点按钮」并存：`VITE_MOCK=1`（演示环境）默认就是演示数据；未设该变量时仍是空态 + 用户主动点按钮（财务页不许默认显示假数字）。

---

## 4 订阅录入（P04）对齐 Spec《订阅录入标准》

| 标准条目 | 落地 |
|---|---|
| §2.1 字段集合（Provider/Account/Price/Currency/Cycle/PayDay/ShareCost/CycleStart/Note） | ✅ 入参类型 `SubscriptionInput` 与表单字段逐字一致；不再用 `ProviderRef/PriceAmount/BillingCycle` 等旧名 |
| §2.1 单位：录入「元/美元」，`price_amount` 落**积分** | ✅ 单价右侧实时显示「≈ N 积分」（`priceToCredits`：×1e6÷R，USD 先 ×cny_rate；缺 R/汇率显示「—」） |
| §2.2 不做的字段（月配额 / 状态列 / 生效失效时间 / 软删 / 汇率列） | ✅ 表单与列表都没有这些字段；状态由 `share_cost + cycle_start + billing_cycle` 派生 |
| §2.3 派生状态六值 + 优先级 | ✅ `deriveSubscriptionStatus` 与后端同序：stopped → once → missing_cycle → pending → expired → active；列表标签 color 与文案按表 |
| §3.1 L1 语法与范围 | ✅ 订阅方/账号 trim、1–128、禁换行；Price ≥0、≤1e9、2 位小数、`=0` 仅允许停摊；币种列举；PayDay 1–28；CycleStart ≥2000-01-01；Note ≤500（计数） |
| §3.1 `=0` 仅允许与 `ShareCost=false` 同时出现 | ✅ 校验器直接拦：「0 元只能作为停摊占位（先关闭「参与分摊」）」 |
| §3.1 USD 需 `billing.cny_rate` | ✅ 缺汇率时表单红字 + 阻止提交，文案点名 422 `rate_not_configured` 与去「系统设置 → 计费参数」 |
| §3.3 期限一致性不硬校验、过期允许保存 | ✅ 账期起点早于今天只给灰字「已过期，保存后需结转」，不阻断 |
| §3.4 / §4 唯一键 `(provider_ref, account_ref)`、重复 409 不覆盖 | ✅ 提交前本地查重 → 弹窗「该账号已有订阅（ID …），唯一键不允许重复录入，是否改为编辑？」 |
| §5.2 原因码十条（文案 + 处置） | ✅ `REASON_TEXT` 逐条照抄；未摊出列 tooltip 逐条显示「原因 → 处置动作」 |
| §5.3 / §8.1 提交后必须回显 `WillAmortize` + `BlockReasons` | ✅ 提交后面板显示「会/不会参与分摊 + WillAmortize 值 + 逐条原因与处置按钮」；`cycle_missing/cycle_expired` → 填回本月 1 日，`share_off` → 打开开关，`price_zero` → 补价 |
| §5.3 C5/C6 不在录入响应承诺 | ✅ 文案明确「下一轮 ≤5 分钟；当期无账单会进「未摊出」告警（C5/C6 属运行时事实）」 |
| §8.1 控件与默认值 | ✅ 订阅方可搜索下拉（S7 候选；接口未就绪时黄条说明并临时允许手输）、账号下拉 + 手输开关、费用数字 + 币种、周期单选（一次性时隐藏账期起点）、账期起点默认本月 1 日、付款日 1–28、分摊开关（关闭时整表灰化 + 「已摊金额不回滚」）、备注 500 字计数 |
| §8.2 列表列与顶部条 | ✅ 订阅方/账号/整期费用（原币+积分）/周期/账期/状态/本期已摊/未摊出/操作；顶部「本月有 N 条订阅未摊出，合计 M 积分」为 0 时隐藏；`Unallocated>0` 橙字 + 原因 |
| §8.3 错误映射 | ✅ `invalid_field`→字段红字；`provider_not_found`→提示去拓扑页同步；`topology_not_synced`→阻断并指路；`duplicate_subscription`→弹窗改编辑；`stale_object`→刷新重试；`rate_not_configured`→去配汇率（前四类已可触发，后两类随 S2/S3 上线生效） |
| 保存不写库（本轮约束） | ✅ 提交只做前端校验 + 判定回显 + 明示「S2 待 Core，本次未写库」；接线点已留（`createSubscription`） |

---

## 5 与 Spec 13 页文档的**不一致清单**

### 5.1 已在本次改掉（9 项）

| # | 页 | 原不一致 | 处理 |
|---|---|---|---|
| 1 | P04 | 字段名与 Spec 不同（ProviderRef/PriceAmount/BillingCycle）、无派生状态、无已摊/未摊出、无原因码 | 按《订阅录入标准》重写（见 §4） |
| 2 | P04 | 「保存不写库」被写成待定，未回显判定 | 提交即出 `WillAmortize`/`BlockReasons` 判定面板 |
| 3 | P05 | 把「预扣登记」当账本页的页签 | 移除（Spec §1.2：预扣属 P09 资金轨迹），改出新鲜度卡 |
| 4 | P05 | 无新鲜度、无 ledger_id 精确查、ledger_id 不可复制 | 补 L2 新鲜度卡（180s 橙 / 15min 红）、ledger_id 精确查 + 一键复制 |
| 5 | P06 | `skipped` 只显示状态词、无 skip_reason 文案、无筛选/排序/展开 | 补中文原因、状态筛选、`alarm_streak` 降序置顶 + `≥3` 行高亮、行展开 detail、「按当前页统计」标注 |
| 6 | P07 | 成本未拆变动/固定、无恒等式自检、无 Uncosted 两段、无排序/跳转 | 补四项；趋势块按 O2 未就绪出空态（**不造前端假折线**） |
| 7 | P02 | 缺同步状态条与手动同步入口、无供应商层 | 补状态条（最后成功/结果/新增/失效）+ 供应商层 + 「立即同步」禁用 |
| 8 | P09 | 只有归因块，无资金轨迹 | 补「资金轨迹」（预扣/退款/差额/钱包流水）+ 恒等式，未就绪时说明三种读法 |
| 9 | P10 / P11 | 用户页仍可切「按成本」；令牌页仍有「语义标签」 | 按成本按钮禁用 + 口径 D-1 说明；删除标签表单项与 TagID 提交（0024 前先拆 UI） |

### 5.2 改不了，卡在接口上（6 项）

| # | 页 | 差什么 | 卡在 |
|---|---|---|---|
| 1 | P01 统计 | 成本来源四段条、`cost_source IS NULL` 文案、去会话/渠道口径、实时区改速率 | M1、M2 |
| 2 | P02 拓扑 | 「供应商 → **账号** → 内部模型 → 对外模型」的账号层；画布拖拽落位持久化 | T1 的 account 聚合、T2 |
| 3 | P03 模型 | 绑定下拉（候选 T4、`stale` 候选禁用）、保存走 T5、`/v1/models` 预览 | T4、T5 |
| 4 | P08 账单 | `upstream_provider/account/protocol/cost_source/HasLedger` 筛选、筛选写 URL、dev 负向断言 | M6、M7 |
| 5 | P09 账单详情 | 归因七列 / 预扣 / 结算差额 / 钱包流水的**真实字段**（页面结构已就位，等 M8 返回） | M8 |
| 6 | P12 系统 | 「上游（V2）」三键**可编辑 + 字段级校验 + 保存回读**（当前只读） | M10 写入白名单 |

> 另有 1 项产品口径待定：P04 的「结转到本期 / 立即重算 / 分摊明细」按钮已按 Spec §5 禁用并写清原因，等 S4/S5/S6 上线即可启用，**不属前端未完成**。

---

## 6 验证（DOM 断言，不靠截图）

| 脚本 | 覆盖 | 结果 |
|---|---|---|
| `/data/pv/v2-align-check.mjs` | Spec 对齐项：P04 列表/表单/409/判定回显、P05 新鲜度与 ledger_id、P06 skip_reason 与筛选/展开/高亮、P07 恒等式与两段 Uncosted、P02 状态条与供应商层、P03 绑定入口禁用、P11 标签已删 | 38 项全绿 |
| `/data/pv/v2-pages-check.mjs` | 13 页基线：真数据渲染、未就绪空态、演示数据、老后端回归（新列/新块不出现）、390px 无横向溢出 | 全绿 |
| `/data/pv/m375.mjs` | Spec §7 移动端口径 375px：五个新页 `scrollWidth == clientWidth`、正文 16px | 全绿 |

类型/构建：`npx vue-tsc --noEmit` 零错；`npx vite build` 通过。

---

## 7 阻塞点（等谁、等什么）

| # | 阻塞 | 等谁 | 解除条件 |
|---|---|---|---|
| 1 | 订阅 S1–S7 | Core | 按标准 §7 开七个端点；`WillAmortize` 只判 C1–C4，S6 复用 `Allocator.RunOnce` |
| 2 | 账本 L1/L2、拓扑 T1/T2/T3、内部模型 T4、绑定 T5 | Core | 接口就绪后删对应 `src/mock/<page>.ts`，页面零改动 |
| 3 | 账单 M6/M7/M8（筛选、dev 负向、资金轨迹字段） | Core | M8 返回 `Hold/Flows` 后 P09 的资金轨迹块自动生效 |
| 4 | 系统 M10（上游三键写入白名单） | Core | 放开后把「上游（V2）」页签从只读改可编辑（`isConfigTab` 放开 `upstream` 即可） |
| 5 | 盈亏 O2 趋势、对账 O1 汇总 | Core（可延后） | O2 就绪后 P07 趋势块接真数据；O1 就绪后 P06 汇总改全量口径并去掉「按当前页统计」标注 |
| 6 | P01 统计口径改造 | Core（M1/M2） | 合入后按 Spec P01 五条验收逐条过（成本来源四段 / 无会话词 / 收入 0 毛利率「—」） |

> 本分支只推送自己的分支，未向 `main` / `v2` 发起任何 PR/MR；v2 收敛 PR 由 Core 开。

---

## 8 渠道族 / 标签接口摘除（2026-11-16，同批修，不延到阶段三）

后端在 v2 收敛里把渠道族（渠道 / 渠道协议 / 渠道密钥 / 渠道模型）与标签接口整体下线，
前端不再是「点进去一串 410」，而是**物理删除 + 依赖渠道维度的展示降级**。

| 项 | 数量 | 明细 |
|---|---|---|
| 请求封装删除 | **38 处** | 渠道 22（含 cron-preview / probe-logs / model-events / 密钥与模型子路径）· 渠道协议 10 · 标签 5（管理端 4 + 开发端 1） |
| 类型定义删除 | 渠道族与标签相关全部 DTO | `AdminChannel / ChannelInput / ChannelKey / ChannelKeyState / ChannelModel / ChannelModelInput / ChannelModelEvent / ChannelProtocolView / ChannelProtocolInput / ChannelProtocolPack / AdminTag / TagInput / DevTag / ProbeLog / ProbeOutcome / ServiceStatusView / MachineMetrics / ChannelRuntimeView / ModelRuntimeView` 等；令牌 DTO 去掉 `TagID` |
| 页面删除 | **11 个文件** | `ChannelList / ChannelCreate / ChannelEdit / ChannelProtocolList / ChannelProtocolEdit / ChannelsPage / TagList / TagCreate / TagEdit / ServiceStatus` + 组件 `ChannelModels.vue` |
| 路由节点 | 10 条页面路由删除 | 渠道 3 / 渠道协议 3 / 标签 3 / 状态 1；替换为 **4 条老书签兜底**（`channels/*`、`channel-protocols/*`、`tags/*`、`service-status` → `/admin/topology`），无页面、无接口调用 |
| 菜单入口 | 3 个删除 | 渠道 / 标签 / 状态（含 activeMenu 高亮映射与图标 import） |

**依赖渠道维度的展示（按「结构不删、渠道维度恒空」降级）**

| 位置 | 处理 |
|---|---|
| 账单查询「渠道 / 密钥」列 | 列保留、表头改「渠道 / 密钥（已下线）」并附 tooltip；**筛选控件与候选加载全部删除**（`listChannels` / `listChannelKeys` 不再调用）；历史行有值就显示，新行 `-` |
| 账单详情 / 调用日志 / 会话详情 | `ChannelName / KeyName / channel_*` 仍按后端返回显示（历史行可能非空），无值显示 `-`，**不做任何推导或补数据** |
| 系统页「运维」页签 | 整块删除（渠道协议备份/恢复），页面不再请求 `/admin/channel-protocols/export|import` |
| 模型编辑「从内部模型同步定价」 | 删除（原本读 `channel_models.CostRates`，该表 0024 已删）；售价维护保留「手填 / 从 models.dev 取参考价」两条路 |
| 开发端令牌「路由 / 语义标签」 | 列表列、筛选项、创建表单项、详情块全部删除；`TagID` 不再出现在任何请求体 |

**保持现状（不属渠道族，别顺手删）**：模型页「内部模型绑定」列与未绑定横幅、令牌页 callerKey 相关能力、模型页绑定入口（禁用待 T5）。

**自查**：`grep -rn "/admin/channels\|/admin/channel-protocols\|/admin/tags\|/dev/tags" frontend/src --include=*.ts --include=*.vue` → **0 命中**。
DOM 走查：`/data/pv/v2-channel-removal-check.mjs`（菜单 18 项、老路径 4 条兜底、账单列降级、模型/系统/令牌残留、全程零渠道接口请求）全绿；`vue-tsc` 零错、`vite build` 通过。

### 8.1 Auto 模型族同批摘除（2026-11-16 追加，Gate 复核提出）

Auto 模型（`auto_models` / `auto_model_members`）与渠道族是同一批退役的表（0024 删表、接口 410），
前端原先仍有 10 处调用（5 个封装 × 管理端/开发者端两套 base），且「新建 / 编辑 Auto 模型」两页能点进去、保存必报错。

| 项 | 处理 |
|---|---|
| 请求封装 | 删 5 个（`listAutoModels` / `getAutoModel` / `createAutoModel` / `updateAutoModel` / `deleteAutoModel`）= **10 处端点调用**（`/admin/auto-models*` 与 `/dev/auto-models*`） |
| DTO | 删 `AutoModel` / `AutoModelMember` / `AutoModelInput` |
| 页面 | 删 4 个：`AutoModelList.vue` / `AutoModelCreate.vue` / `AutoModelEdit.vue` / `ModelsPage.vue`（原「模型 / Auto 模型」页内切换壳），组件删 `AutoModelMembers.vue` |
| 路由 | 删 6 条节点（管理端 3 + 开发者端 3），按既有规则留 **2 条老书签兜底** redirect：`auto-models/:pathMatch(.*)*` → `/admin/models` 与 `/dev/models`；模型页直接挂 `ModelList.vue`（不再有页内切换） |
| 菜单高亮 | 两个 Layout 里「auto 归入模型高亮」的映射删除（路径已被 redirect 吃掉，映射成死代码） |
| 账单页 | 模型筛选不再合并 Auto 名（不再请求 `/admin/auto-models`）；账单行上的 `AutoModelName` 属响应字段，**结构保留、照后端返回显示** |

**为什么不退成只读**：Auto 模型表与成员表都已删除，没有任何可展示的数据来源，「只读页」会是一片空壳；因此整体下线 + 兜底跳转，与渠道族处理一致。

**自查**：`grep -rn "auto-models\|AutoModel\|ModelsPage" frontend/src --include=*.ts --include=*.vue` → 仅剩 2 条 redirect（`AutoModelName` 是账单响应字段，不算残留）。
DOM 走查 `/data/pv/v2-auto-removal-check.mjs`（模型页无 Auto 切换、6 条老路径兜底且无保存按钮、账单不请求 auto-models、零运行时错误）16 项全绿。

**未动（明确不属本轮）**：令牌 `callerKey`（前端本无绑定入口，不新做）、模型「内部模型绑定」列（后端列表接口未返回该字段，属 Core 侧契约缺口，前端不造假数据）。

