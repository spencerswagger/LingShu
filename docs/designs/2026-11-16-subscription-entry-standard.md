# 订阅录入标准（阶段二 A5 月费分摊的唯一入口）

- 日期：2026-11-16 · 状态：**生效**（spencer 2026-11-16 裁定：管理端订阅录入端点要补，否则 A5 永远摊 0 笔）
- 读者：Core（实现端点 + 分摊细化）、View（录入页与列表页）、Gate（验收）
- 依据（均已在代码里核对）：
  - `backend/internal/db/migrations/0019_v2_topology.sql`（`subscriptions` DDL：`uq_subscriptions_account`、`pay_day 1–28`、`NUMERIC(20,5)`；注意本文件当前只在 `feat/lingshu-v2-sync-config` / `feat/lingshu-v2-billing-phase2` 分支，未进 `v2`）
  - `backend/internal/domain/amortize/amortize.go`（A5 分摊：取数过滤、`AllocateShares` 分配、`Allocate` 直写 `cost_credits`）
  - `backend/cmd/server/main.go`（`amortizer` 装配 + `time.NewTicker(5 * time.Minute)`；**无启动即跑**）
  - `backend/internal/domain/billing/engine.go:12-23`（积分价值 `V = R ÷ 1,000,000` 元/积分；`积分 = 倍率合计 × 系数 ÷ R`）
  - `backend/internal/server/server.go:98,226`（角色守卫：`/api/v1/admin/` 需 ADMIN、`/api/v1/dev/` 需 DEVELOPER，`/api/v1/admin/` 整段套 `WithAudit`）
  - `2026-11-15-v2-p1-data-contract.md` §2.4/§7、`2026-11-15-channel-cost-basis-口径.md`
- 范围：只定**产品口径 + 接口契约 + 前端规范 + 验收条件**，不含实现代码、不改 DDL。

---

## 0 结论先行（八条）

| # | 结论 |
|---|---|
| 1 | 唯一自然键 = `(provider_ref, account_ref)`；一个账号一条订阅，**重复录入一律 409**，不做静默覆盖、不做自动合并。 |
| 2 | `price_amount` 落库单位是**积分**（不是元）：录入端收「元/美元 + 币种」，后端按 `元 × 1,000,000 ÷ R`（USD 先 `× cny_rate`）折算，5 位小数。依据：`amortize.go` 的 `Allocate` 把该值**直接**写进 `billing_records.cost_credits`（中间没有换算）；积分价值 `V = R ÷ 1,000,000` 元/积分出自 `billing/engine.go:12`。R=10000 时 100 元 = 10000 积分。 |
| 3 | 参与分摊是 **6 个条件的 AND**（见 §5）。只满足前 4 个静态条件只能保证「会被扫到」，当期有没有可摊账单取决于归因是否落库——所以录入响应必须回 `WillAmortize` + `BlockReasons[]`，页面必须回显。 |
| 4 | 本期**不新增列**：不做月配额、不做状态列、不做失效时间列、不做软删；「停摊」= `share_cost=false`，「账期过期」由 `cycle_start + billing_cycle` 现算。 |
| 5 | 录入成功 ≠ 已摊出。录入成功 = 行落库 + 唯一键成立 + 静态条件判定返回；**分摊生效 SLA = 0–5 分钟**（`main.go` 装配的 ticker 是 5 分钟一跳且**启动时不跑**，所以录入后最长等 5 分钟，不是「立刻」）。 |
| 6 | **「未摊出」目前没有持久化载体**：`amortize.Result.Unallocated` 只在日志里打一行，不落表、不发告警。因此页面的「未摊出」必须由 S4/S1 **现算**（`PriceCredits − Σ 本期 cost_source='amortized' 的 cost_credits`），**不许**去读日志或等告警表。 |
| 7 | **权限只有 ADMIN**：录入/改价/停摊/结转/手工重算全部挂在 `/api/v1/admin/subscriptions*` 下（`server.go:98` 路径前缀守卫），开发者端**零入口、零接口**；所有写操作走 `WithAudit` 留痕（谁、何时、改了哪几个字段的前后值）。 |
| 8 | 与 `cost_basis` 文档的「订阅摊销 = 月费 ÷ 月配额」**不并存**：V2 成本只允许 `ledger` / `amortized` / `none`（0020 的 CHECK），订阅摊销的唯一实现是 A5 行级分摊。两套同时写 `cost_credits` = 同一笔成本记两次。 |

---

## 1 要解决的问题（为什么以前必然摊 0 笔）

A5 的取数 SQL 是硬过滤（`amortize.go:241-243`）：

```sql
SELECT provider_ref, account_ref, price_amount, billing_cycle, cycle_start
  FROM subscriptions
 WHERE share_cost = true AND price_amount > 0
```

而 `subscriptions` **没有任何录入入口**，表为空 → 分摊轮次每次 `Subscriptions=0` → 盈亏页成本只剩账本变动成本，月费从未进入成本。录入端点的最低职责不是「能存一行」，而是**让运营一眼看出这行会不会被摊、摊不出去是卡在哪一条**。

---

## 2 字段清单

### 2.1 业务字段 → 数据列映射

| 业务叫法 | API 字段 | 落库列 | 必填 | 录入单位 | 说明 |
|---|---|---|---|---|---|
| 订阅方（供应商） | `Provider` | `provider_ref` | 是 | — | 网关 provider 名 |
| 产品 / 账号 | `Account` | `account_ref` | 是 | — | 网关账号 id/name；与 Provider 组合唯一 |
| 整期费用 | `Price` | `price_amount` | 是 | **元 / 美元**（后端折算积分） | 一期固定费用，不是单价 |
| 币种 | `Currency` | `price_currency` | 是 | — | `CNY` / `USD`，默认 `CNY` |
| 计费周期 | `Cycle` | `billing_cycle` | 是 | — | `month` / `year` / `once`，默认 `month` |
| 付款日 | `PayDay` | `pay_day` | 是 | 日 | 1–28（29–31 跨月会错位，DDL 已限） |
| 是否参与分摊 | `ShareCost` | `share_cost` | 是 | — | 默认 `true`；`false` = 停摊（保留历史） |
| 账期起点 | `CycleStart` | `cycle_start` | **参与分摊必填** | 日期 | `NULL` = 不摊；决定分摊窗口 `[CycleStart, +1 期)` |
| 备注 | `Note` | `note` | 否 | — | ≤500 字，写「哪个套餐/谁批的」 |
| 记录 ID | `ID` | `id` | 只读 | — | 服务端雪花 |
| 创建/更新时间 | `CreatedAt` / `UpdatedAt` | 同名列 | 只读 | — | 乐观锁用 `UpdatedAt` |
| 状态 | `Status` | **不落列** | 只读 | — | 现算：`active`/`pending`/`expired`/`stopped`/`once`（§2.3） |

### 2.2 明确不做的字段（写死，避免「顺手加列」）

| 想加的字段 | 结论 | 理由 |
|---|---|---|
| 月配额 `monthly_quota`（token/月） | **不加** | 那是 `cost_basis`「月费÷月配额」的单价法，V2 不用；分摊基数直接取当期账单的真实三段 token |
| 计量单位 / 计价单位 | **不单独存** | 由 `billing_cycle` 表达（整期固定费用）；没有「按 token 计价」的订阅 |
| 状态列 `status` | **不加** | 全部可由 `share_cost` + `cycle_start` + `billing_cycle` 推出；存一份必然与事实漂移 |
| 生效 / 失效时间列 | **不加** | 生效 = `cycle_start`；失效 = 停摊（`share_cost=false`）。需要留痕写 `note` |
| 软删 `deleted_at` | **不加** | 已摊成本在账单行上，删订阅不会回滚；本期只允许停摊，不提供删除端点 |
| 汇率列 `fx_rate` | **不加** | USD→CNY 用全局 `billing.cny_rate`，录入时快照进积分，币种只作审计 |

### 2.3 派生状态（页面标签口径，优先级从上到下）

| 状态 | 判定 | 页面文案 | 是否参与分摊 |
|---|---|---|---|
| `stopped` | `share_cost=false` | 已停摊 | 否 |
| `once` | `billing_cycle='once'` | 一次性（不摊） | 否 |
| `missing_cycle` | `share_cost=true` 且 `cycle_start IS NULL` | 缺账期起点 | 否（红色提示「补账期起点，否则不摊」） |
| `pending` | `cycle_start > 今天` | 待生效（M-D 起摊） | 否（当期跳过，不算异常） |
| `expired` | `今天 ≥ cycle_start + 1 期` | 账期已结束，需结转 | 否（**最常见的「摊 0 笔」原因之一**） |
| `active` | 其余，且今天落在 `[cycle_start, cycle_start+1 期)` | 分摊中 | 是 |

---

## 3 校验规则（四层，逐层给可读错误）

### 3.1 L1 语法与范围

| 字段 | 规则 | 失败码 / 文案 |
|---|---|---|
| `Provider` / `Account` | trim；1–128 字符；不允许空串、换行 | 400 `invalid_field`「订阅方/账号不能为空」 |
| `Price` | 数字；`≥ 0`；≤ 1e9（元）；最多 5 位小数；**`=0` 仅允许与 `ShareCost=false` 同时出现** | 400 `invalid_field`「整期费用必须大于 0」/「0 元只能作为停摊占位」 |
| `Currency` | `CNY` / `USD` | 400 `invalid_field` |
| `USD` 录入 | `billing.cny_rate` 必须已配置且 > 0，否则拒绝 | 422 `rate_not_configured`「USD 汇率未配置，暂不能按美元录入」 |
| `Cycle` | `month` / `year` / `once` | 400 `invalid_field` |
| `PayDay` | 1–28 的整数 | 400 `invalid_field` |
| `CycleStart` | `YYYY-MM-DD`；允许未来（预售）；不允许早于 2000-01-01 | 400 `invalid_field` |
| `Note` | ≤500 字 | 400 `invalid_field` |

> `Price` 的 5 位小数是落库精度（`NUMERIC(20,5)`），前端按「元」输入最多 2 位小数即可；折算后的积分允许 5 位。

### 3.2 L2 存在性（只查本地镜像，不查网关实时状态）

| 校验 | 规则 | 失败码 / 文案 |
|---|---|---|
| `Provider` 存在 | 必须出现在 `internal_models.provider_ref` 的去重集合里 | 400 `provider_not_found`「订阅方不在网关镜像中」 |
| 拓扑为空 | 上述集合为空 | 409 `topology_not_synced`「尚未同步网关拓扑，请先在「上游拓扑」页执行一次同步」 |
| `Account` 存在 | **软校验**：命中近 30 天 `billing_records.upstream_account` 或 `ledger_entries.provider_account` 的已知集合则不提示；未命中允许保存，但响应 `Warnings[]` 附 `account_unverified` | 200 + warning「账号未在近期账单/账本中出现，请确认拼写，否则永远摊 0 笔」 |

> 为什么账号只能软校验：V2 没有「网关账号」镜像表（`subscriptions.account_ref` 只作标识），硬校验会逼后端在录入时同步调网关，网关抖动就录不进去。软校验 + 页面高亮风险，比假装严格更可靠。

### 3.3 L3 业务一致性

| 规则 | 处理 |
|---|---|
| `Cycle` 与 `Price` | `once` 允许存（用于台账留档），但状态 `once`、明确不摊、不进「未摊出」告警 |
| `CycleStart` 与 `PayDay` | 不做硬校验（有的订阅按自然月 1 日计费、付款日在 5 号）；不一致时页面给灰字提示，不阻断 |
| `CycleStart` 已过期 | **允许保存**（保留历史账期），但状态 `expired` + 列表置顶提示「需结转」；后端不得自动改 `cycle_start` |
| `ShareCost=false` + `Price=0` | 允许（停摊的零价占位），状态 `stopped` |

### 3.4 L4 唯一性与并发

- 唯一键：`(provider_ref, account_ref)`（DB 已有 `uq_subscriptions_account`）。
- 乐观锁：`PUT` 必须带 `UpdatedAt`（RFC3339）；与库中不一致 → 409 `stale_object` + 返回当前行。
- 值完全相同的 `PUT`：返回 200，**不刷新 `updated_at`**（避免乐观锁自旋）。

---

## 4 幂等与重复录入

| 场景 | 行为 |
|---|---|
| 同一 `(Provider, Account)` 再 POST | **409 `duplicate_subscription`**，响应体带 `ExistingID` 与当前完整行；前端弹「该账号已有订阅，去编辑 / 取消」，**不做**自动 upsert |
| POST 网络超时后重试 | 若首次已落库，重试得到 409 + `ExistingID`，按成功处理（前端跳详情） |
| 同一份 `PUT` 重放 | 200（幂等，见 §3.4） |
| 两个管理员同时改价 | 后提交者 409 `stale_object`，必须刷新后重改；**不许 last-write-wins** |
| 手工触发分摊（N11）重放 | 幂等（写的是 SET 不是 ADD）；响应返回本轮 `Updated/Allocated/Unallocated` |
| 账期结转（N10）重放 | 若 `cycle_start` 已是目标期起点 → 200 `noop=true`，不重复 +1 期 |

> 不引入 `Idempotency-Key` 存储：唯一键 + 乐观锁已覆盖所有重试路径，再加一层只会多一份会过期的状态。

---

## 5 与 A5 分摊的对齐条件（录入成什么样才会计入分摊）

### 5.1 六个条件（全部满足才会被摊到）

| # | 条件 | 谁保证 | 不满足的后果 |
|---|---|---|---|
| C1 | `share_cost = true` | 录入时勾选 | SQL 直接过滤，0 笔 |
| C2 | `price_amount > 0` | 录入价 > 0 且折算后 ≥ 0.00001 | SQL 直接过滤，0 笔 |
| C3 | `billing_cycle ∈ {month, year}` | 录入时选周期 | `once` 被跳过，0 笔 |
| C4 | `cycle_start` 非空，且 `今天 ∈ [cycle_start, cycle_start+1 期)` | 录入/结转 | `NULL` 跳过；过期窗口查不到当期账单，0 笔 |
| C5 | 该 `(provider, account)` 当期有 `billing_records`，且 `upstream_provider/upstream_account` **非空并精确匹配** | 网关归因（0020）+ 账号拼写 | 归因为 NULL 或拼错 → 查不到行，0 笔 |
| C6 | 这些账单 `ledger_entry_id IS NULL` 且 `cost_source ∈ (NULL, 'none', 'amortized')` | 账本拉取时序 | 全部已挂账本 → 无可摊行，`Unallocated = 全额` |

补充规则（来自 `amortize.go`，页面文案要照此解释）：

- 分摊基数 = 当期可摊账单的三段 token 合计；**token 全为 0 时按行均分**（有账单就要把固定成本摊出去）。
- **部分行为 0 token 时，这些行分不到钱**（`total > 0` 分支下 `tokens <= 0` 直接 `continue`）：页面「本期分摊明细」里 0 token 行显示「0（无用量，不背成本）」，不能画成 0 元成本却仍算进分母。
- 金额合计**必须**等于订阅费，分币误差按最大余额法补在 token 最多的行。
- 账本成本迟到会覆盖 `amortized`（`ledgersettle`），下一轮在剩余行上重算——**同一行不会双记**；被账本占用的行在 `Allocate` 里 `RowsAffected=0`，本轮直接跳过。
- 修改价格 / `cycle_start` 会重算**当期**已摊金额（SET 幂等）；**已结束账期不回算**。
- `RunOnce` 里 `Skipped` 的三种情形与页面状态一一对应：`Price<=0`（`price_zero`）、`CycleStart` 为空（`cycle_missing`）、`billing_cycle='once'`（`cycle_once`）。`Unallocated` 只在**有账期、非一次性、价>0 但当期无可摊账单**时累加。
- **`Unallocated` 不落库**（只 `log.Warn` 一行）：它随时会因为下一轮有新账单而消失，落一份快照必然与事实漂移。页面口径 = 现算（见 §0 第 6 条）。

### 5.2 「摊 0 笔」原因码（列表与详情必须逐条显示）

| 原因码 | 触发 | 页面文案（给运营看的） | 处置动作 |
|---|---|---|---|
| `share_off` | C1 | 已停摊 | 打开「参与分摊」 |
| `price_zero` | C2 | 费用为 0，不参与分摊 | 改价 |
| `cycle_once` | C3 | 一次性费用不分摊（设计如此） | 无需处理 |
| `cycle_missing` | C4 | 缺账期起点 | 补 `cycle_start` |
| `cycle_pending` | C4 | 账期还没开始（X 起摊） | 无需处理（不是异常） |
| `cycle_expired` | C4 | 账期已结束（截至 X），未结转 | 点「结转到本期」 |
| `no_billing_rows` | C5 | 本期该账号没有账单 | 确认是否有流量 |
| `no_attribution` | C5 | 有账单但缺归因（`upstream_account` 为空） | 找 Core 查归因写入 |
| `account_mismatch` | C5 | 账单账号与订阅账号不一致 | 核对账号拼写 |
| `ledger_taken` | C6 | 可摊账单已被账本成本占用 | 正常（变动成本优先） |
| `unknown` | 兜底 | 未知原因，看接口 `Detail` | 报 Core |

### 5.3 录入页必须回显的判定结果

`POST`/`PUT` 响应都带：

```json
{
  "ID": "…", "Provider": "…", "Account": "…",
  "Price": 100.00, "Currency": "CNY", "PriceCredits": 10000.00000,
  "Cycle": "month", "CycleStart": "2026-11-01", "ShareCost": true,
  "Status": "active",
  "WillAmortize": true,
  "BlockReasons": [],
  "Warnings": [{"Code": "account_unverified", "Message": "账号未在近 30 天账单中出现"}],
  "NextRunHint": "下一轮分摊 ≤5 分钟；无账单会进「未摊出」告警"
}
```

- `WillAmortize` **只判定 C1–C4**（静态条件，服务端可确定）；C5/C6 是运行时事实，由 `GET /subscriptions/{id}/allocations` 回显，不在录入响应里承诺。
- `BlockReasons` 用 §5.2 的原因码；静态条件全过时为空数组（不是 `null`）。

---

## 6 录入成功 / 失败的判定口径

### 6.1 两级判定（不许混为一谈）

| 级别 | 判定 | 证据 |
|---|---|---|
| **录入成功** | 201/200 + 行可按 `(Provider, Account)` 查回 + 唯一键成立 + `WillAmortize/BlockReasons` 有值 | 接口响应 + 列表回显 |
| **分摊生效** | 下一轮（≤5 分钟）后，该账号当期账单出现 `cost_source='amortized'`，且 `Σ cost_credits = PriceCredits`（±0.00001） | `GET /subscriptions/{id}/allocations` + 账单行 |

> 只做到「录入成功」不算功能可用。验收必须看到第二级（§9 用例 A-08）。

### 6.2 HTTP 语义

| 码 | 含义 | 前端动作 |
|---|---|---|
| 201 | 新建成功 | 跳详情/列表，展示 `WillAmortize` 与 `Warnings` |
| 200 | 更新成功 / 幂等重放 / 结转 noop | 同 201 |
| 400 | 字段非法 / Provider 不存在 | 定位到字段，保留用户输入 |
| 404 | `{id}` 不存在 | 提示已被删除/刷新列表 |
| 409 | 重复订阅 / 乐观锁冲突 / 拓扑未同步 | 按 `Code` 分支处理（`duplicate_subscription` 带 `ExistingID`） |
| 422 | 业务不可执行（USD 汇率未配置） | 提示去系统设置配 `billing.cny_rate` |
| 403 | 非 ADMIN | 统一无权限页 |

> **错误体形状不另立一套**：沿用现有 `internal/pkg/resp`（`CodeOK=0`、`CodeBadRequest=40001`、`CodeForbidden=40301`、`CodeNotFound=40401`、`CodeConflict=40901`、`CodeRateLimited=42901`、`CodeInternalError=50001`）。本文档里的字符串码（`invalid_field` / `duplicate_subscription` / `stale_object` / `rate_not_configured` / `topology_not_synced`）落在响应体的 `Reason` 字段，`Code` 用上面那组数字；前端按 `Reason` 分支，`Code` 只用来判定大类。**不要**为订阅单独发明一套错误码体系。

---

### 6.3 权限与审计（写死）

| 项 | 口径 |
|---|---|
| 接口层 | 7 个端点全部在 `/api/v1/admin/subscriptions*`，由 `server.go:98` 的 `WithAuth(..., RoleAdmin)` 统一守；**不得**提供 `/api/v1/dev/*` 同义端点（成本数据一律不下发开发者端） |
| 前端层 | 路由挂 `/admin/*`（`meta.roles=['ADMIN']`）、菜单只在管理端出现；开发者端登录后访问该路由 → `/403`，直连接口 → 403 |
| 越权口径 | 403 一律返回统一错误体（`code=40301`），**不区分**「不存在」与「无权限」，不返回行内容，避免探测 |
| 审计 | 写操作（POST/PUT/roll-cycle/amortize run）必须落审计：操作者、订阅 id、动作、`price_amount`/`cycle_start`/`share_cost` 的前后值；缺审计即判验收不过（改价尤其要能追责） |
| 冷却 | 手工重算 S6 加**单实例限流**（建议 1 次/10 秒，返回 429），防止运营连点点爆数据库；限流不影响定时轮 |
| 余额/他人数据 | 本组接口不返回任何用户余额、令牌、`credit_flows`；订阅只与 `upstream_provider/account` 有关 |

> 为什么不给开发端只读入口：订阅价是灵枢的进货价，等价于成本；一旦下发，开发者可按账号反推毛加价率。产品口径 = 成本侧数据只给 ADMIN。

---

## 7 接口契约草案（给 Core 直接落）

| # | 方法 + 路径 | 用途 | 关键入参 / 出参 |
|---|---|---|---|
| S1 | `GET /api/v1/admin/subscriptions` | 列表 | 筛选 `Provider/Account/Status/ShareCost/Page/Size`；出参含派生 `Status`、`PriceCredits`、`WillAmortize`、`BlockReasons`、`CycleAllocated/CycleUnallocated`（读路径轻量聚合） |
| S2 | `POST /api/v1/admin/subscriptions` | 新建 | 入参见 §2.1；201 出参见 §5.3 |
| S3 | `PUT /api/v1/admin/subscriptions/{id}` | 全量更新 | 必须带 `UpdatedAt`；语义见 §3.4 |
| S4 | `GET /api/v1/admin/subscriptions/{id}/allocations` | 本期分摊明细 | 入参 `CycleStart`（缺省取当前账期）；出参：窗口、`PriceCredits`、`Rows`（账单 ID/时间/token/摊得金额）、`Allocated`、`Unallocated`、`ReasonCode` |
| S5 | `POST /api/v1/admin/subscriptions/{id}/roll-cycle` | 账期结转 | 把 `cycle_start` 推进一个周期（month +1 月 / year +1 年）；可带新 `Price`；幂等 |
| S6 | `POST /api/v1/admin/subscriptions/amortize/run` | 手工重算一轮 | 无入参；出参复用 `amortize.Result`（`Subscriptions/Skipped/Updated/Allocated/Unallocated`） |
| S7 | `GET /api/v1/admin/subscriptions/form-options` | 录入页候选 | Providers（来自 `internal_models`）、Accounts（近 30 天账单/账本 distinct）、`CnyRate` 是否可用、`R` 当前值 |

> 分摊作业本身已有（5 分钟一跳 + `RunOnce`），S6 只是把手动触发接出来；**不要**另写一套分摊逻辑，避免两套口径。

---

## 8 前端录入页规范（View 直接照做）

### 8.1 表单

| 字段 | 控件 | 默认 | 交互要求 |
|---|---|---|---|
| 订阅方 `Provider` | 下拉（S7，可搜索） | 空 | 必选，不允许自由输入；为空时禁用提交 |
| 账号 `Account` | 可搜索下拉 + 「手动输入」开关 | 空 | 手输时实时展示 `account_unverified` 风险提示 |
| 整期费用 `Price` | 数字输入 + 币种选择 | 空 / CNY | 右侧实时显示「≈ N 积分」（按 S7 的 R 与 cny_rate） |
| 计费周期 `Cycle` | 单选：月 / 年 / 一次性 | 月 | 选「一次性」时隐藏账期起点，并提示「不参与分摊」 |
| 账期起点 `CycleStart` | 日期选择 | 本月 1 日 | 「参与分摊」开启时必填；早于今天时提示「已过期，保存后需结转」 |
| 付款日 `PayDay` | 数字 1–28 | 1 | 仅提醒用，不参与计算 |
| 参与分摊 `ShareCost` | 开关 | 开 | 关闭时整表灰化并提示「停摊后不再新增分摊，已摊金额不回滚」 |
| 备注 `Note` | 多行文本 | 空 | 500 字计数 |

提交后：`WillAmortize=false` 时**不许只弹「保存成功」**，必须展开 `BlockReasons` 并给处置按钮（如 `cycle_missing` → 聚焦日期字段）。

### 8.2 列表列

`订阅方` / `账号` / `整期费用（原币 + 积分）` / `周期` / `账期` / `状态` / `本期已摊` / `未摊出` / `操作`

- `本期已摊` / `未摊出` 来自 S4 的聚合；`Unallocated > 0` 用橙色标签，悬停显示原因码文案。
- 状态标签颜色：`active` 绿、`pending` 蓝、`expired` 橙、`stopped` 灰、`once` 灰。
- 空态文案：「还没有订阅。录入后下一轮（≤5 分钟）开始分摊；不录 = 盈亏页成本永远少一笔固定支出。」
- 顶部固定提示：「本月有 N 条订阅未摊出，合计 M 积分」——N/M 为 0 时隐藏。

### 8.3 错误映射

| 接口码 | 前端展示 |
|---|---|
| `invalid_field` | 字段下方红字，保留输入 |
| `provider_not_found` | 下拉标红 + 「去拓扑页同步」链接 |
| `topology_not_synced` | 整页阻断，主按钮「去同步」 |
| `duplicate_subscription` | 弹窗：「已存在（ID …），是否改为编辑？」 |
| `stale_object` | 弹窗：「数据已被他人修改，刷新后重试」+ 展示差异 |
| `rate_not_configured` | 提示去「系统设置 → 计费参数」配汇率 |

---

## 9 验收标准（Gate 逐条执行）

| # | 前置 | 操作 | 期望 | 证据 |
|---|---|---|---|---|
| A-01 | 已同步拓扑，账号有当期账单与归因 | 录入一条月付 CNY 订阅，`cycle_start=本月 1 日` | 201；`Status=active`、`WillAmortize=true`、`BlockReasons=[]` | 响应体 |
| A-02 | A-01 | 等 ≤5 分钟或调 S6 | 该账号当期账单 `cost_source='amortized'`，`Σ cost_credits = PriceCredits ±0.00001` | SQL / S4 |
| A-03 | A-01 | 同 `(Provider, Account)` 再 POST | 409 `duplicate_subscription` + `ExistingID`；表内仍 1 行 | 响应 + `count(*)` |
| A-04 | A-01 | 把 `Price` 改为 2 倍后保存 | 200；下一轮当期已摊合计变为新价（SET 重算） | S4 |
| A-05 | A-01 | 关闭 `ShareCost` | 200；下一轮不再新增分摊；已摊行保留 | S4 + SQL |
| A-06 | 账号当期无账单 | 录入 | 201 + `WillAmortize=true`；下一轮该订阅计入 `Unallocated`，列表橙色告警 | S6 出参 + 列表 |
| A-07 | 账单 `upstream_account` 为空 | 录入匹配 provider | 201；S4 返回 `no_attribution`，页面给出原因码文案 | S4 |
| A-08 | `cycle_start` 设为上月 | 录入 | `Status=expired`；S4 返回 `no_billing_rows`；点「结转」后变 `active` 并正常摊 | 列表 + S4 |
| A-09 | 未同步拓扑 | 录入 | 409 `topology_not_synced`，无脏行落库 | 响应 + `count(*)` |
| A-10 | 汇率未配置 | 选 USD 录入 | 422 `rate_not_configured`，不落库 | 响应 |
| A-11 | 两个管理员同时改同一行 | 先后提交 | 后者 409 `stale_object`，库中为前者版本 | 响应 + SQL |
| A-12 | 已有一轮分摊 | 重放 S6 两次 | 第二次 `Updated` 可为 0，但 `Σ cost_credits` 不变（幂等） | SQL |
| A-13 | 移动端 375px | 打开列表与表单 | 单列、无横向滚动、正文 ≥14px | 截图 |
| A-14 | 开发者端账号 `dev1` | 直连 `GET/POST/PUT/DELETE /api/v1/admin/subscriptions*` | 全 403 + 统一错误体；**不返回**行内容、价格、账号 | 响应 + 负向用例 |
| A-15 | 管理员前端 | 开发者端登录后访问 `/admin/subscriptions` | 跳 `/403`；管理端菜单里没有该项对开发者可见的入口 | 截图 |
| A-16 | ADMIN | 改价 + 结转 + 手工重算各一次 | 审计里三条都在，含操作者与前后值；缺一条判不可合入 | 审计表/SQL |
| A-17 | 某订阅当期无账单 | 列表与详情的「未摊出」 | 金额 = `PriceCredits − Σ摊销`，**现算得出**；日志里没有告警**不影响**页面显示 | S1/S4 + SQL |
| A-18 | 连续点「立即重算」3 次 | 观察接口 | 第 2/3 次 429（限流），数据库连接数不飙升 | 响应 + 监控 |
| A-19 | 当期账单里混有 0 token 行 | 看「本期分摊明细」 | 0 token 行显示「0（无用量，不背成本）」，且分摊合计仍等于订阅价 | S4 |

---

## 10 与既有口径的关系（防双记，必须遵守）

| 文档 / 机制 | 关系 |
|---|---|
| `2026-11-15-channel-cost-basis-口径.md` 的「订阅摊销 = 月费 ÷ 月配额」 | **V2 不启用**。该方法算的是每-token 单价，A5 算的是行级固定成本分摊；两者都写 `cost_credits` 就会双记。V2 的 `cost_source` 只有 `ledger/amortized/none`（0020 CHECK），没有「月配额单价」的位置 |
| `cost_basis` 的「牌价折算 / 手工指定」 | 随渠道族下线（0024 删 `channel_models`），V2 无落点；如要恢复，另立任务书，且必须先解决与账本成本的去重 |
| `pricing_mode=cost` 的用户 | 本期**停用**（无成本单价真源）：用户页只允许 `sale`；已有 `cost` 用户的**新请求直接拒绝**并给可读错误「成本模式已停用」，不静默按售价扣费。登记为口径 D-1 |
| 盈亏页「成本未配置」 | 由 `cost_source IS NULL OR cost_credits IS NULL` 表达（历史行）；`none` 是「明确没有成本口径」，两者在页面分开显示，不合并成 0 |

### 10.1 登记项（不在本文件解决，但不许沉默）

| 编号 | 事项 | 归属 | 影响 |
|---|---|---|---|
| D-1 | `pricing_mode=cost` 无单价来源 | 产品/Core | 本期停用；恢复需另立任务书 |
| D-2 | 账期结转是否做定时任务 | Core | 本期手动（S5）；不结转 = 下月摊 0 笔，列表必须有 `expired` 提醒 |
| D-3 | `price_amount` 的积分口径 | 架构确认 | 若架构坚持「存元」，必须同时改 `amortize` 折算，否则成本放大 100 倍（R=10000 时）。本文件按「存积分」定稿 |
| D-4 | USD 汇率的生效时点 | Core | 按录入时快照（`billing.cny_rate` 当时值），改汇率不回算已录订阅 |
| D-5 | 「未摊出」无持久化载体 | Core | `amortize.Result.Unallocated` 只进日志；本期页面现算，若要「历史未摊出趋势/告警」需另立表（不在本期） |
| D-6 | 账期结转无定时任务（同 D-2） | Core | 到期不结转 = 下月摊 0 笔；列表 `expired` 橙色置顶是唯一防线，必须做 |

---

## 11 一句话给三类读者

- **Core**：照 §7 出 7 个端点；`WillAmortize` 只判 C1–C4；不要另写分摊逻辑，S6 复用 `Allocator.RunOnce`；「未摊出」现算不落库。
- **View**：照 §8 做表单与列表；「保存成功」必须带 `WillAmortize/BlockReasons`，否则等于把「摊 0 笔」藏起来；开发者端不许有入口。
- **Gate**：§9 的 A-01/A-02/A-06/A-08 是 P0，A-14/A-16/A-17 是安全与口径 P0，缺一条就判不可合入。
