# 阶段三：上游拓扑（T1/T3）与订阅录入（S1/S2/S6/S7）后端落地

日期：2026-11-17　作者：Core　分支：`feat/v2-topology-subscriptions`（base `v2`）

## 0. 范围

| 编号 | 方法/路径 | 状态 |
| --- | --- | --- |
| T1 | `GET /api/v1/admin/topology` | 本批落地 |
| T3 | `POST /api/v1/admin/topology/sync` | 本批落地 |
| S1 | `GET /api/v1/admin/subscriptions` | 本批落地 |
| S7 | `GET /api/v1/admin/subscriptions/form-options` | 本批落地 |
| S2 | `POST /api/v1/admin/subscriptions` | 本批落地 |
| S6 | `POST /api/v1/admin/subscriptions/amortize/run` | 本批落地 |
| T2 | `PUT /admin/topology/links`（画布坐标） | **未做**（前端暂不落坐标） |
| S3/S4/S5 | 编辑 / 分摊明细 / 手工结转 | **未做**（下一批） |

无新增迁移：拓扑同步状态存 `sys_configs['topology.sync_state']`，订阅表结构沿用 `0019_v2_topology.sql`。
`V2MigrationRightBound` 不动（仍是 `0025`），本批不碰任何 `.sql`。

## 1. 拓扑（T1/T3）

### 1.1 数据流

网关是唯一真源：`GET /v1/models`（模型层）+ `GET /api/groups`（组层）。
同步器把结果整体替换进镜像表 `internal_models`（`Replace`：`RETURNING (xmax = 0)` 判新增，
本轮未命中的老节点置 `stale = true`，**不删行**——删了就没法在页面上看到「这个模型已经从网关下线了」）。

基址默认取 `magpie.base_url`，可用 `TOPOLOGY_SOURCE_URL` / `TOPOLOGY_SOURCE_KEY` 覆盖；
路径可用 `TOPOLOGY_MODELS_PATH` / `TOPOLOGY_GROUPS_PATH` 覆盖。密钥只从环境变量来，不进 `sys_configs`。

### 1.2 四条「不静默」口径

| 情形 | 行为 | 为什么 |
| --- | --- | --- |
| 未配基址 | T3 → **503** `topology_source_not_configured` | 这是运维要动的事。报成「同步完成 0 条」会让人以为网关真的空 |
| 拉了但 0 节点 | 记失败 `ErrEmptySnapshot`，**不写镜像** | 0 节点会把存量全部标 stale → 页面集体变灰，比报错更难查 |
| 同步失败（已有快照） | T3 → **200** + `State=failed` + `Message`；`LastSuccessAt` 保持上次成功值 | 业务结果不是 HTTP 错误：页面要继续显示旧快照，同时告诉人原因（Spec P02 验收 3） |
| 同一时刻重复触发 | 第二次 → **409** `sync_in_flight` | 网关一次全量模型清单可能几秒，叠加请求只会把源打慢 |

`State` 取值 `ok|failed|never`（零值/空串一律映射成 `never`），落库在
`sys_configs['topology.sync_state']`（JSON）。**不建新表**：一个几十字节的状态不值得占一个迁移号，
也不该为此把 `V2MigrationRightBound` 往前推。

### 1.3 定时轮

`cmd/server` 里 10 分钟一跳（拓扑变动远慢于账单），带单轮 `recover`。
手动 T3 与定时轮走**同一份** `Syncer.RunOnce`，不存在两套实现。
未配源时不起定时轮，并留一条启动 WARN。

## 2. 订阅（S1/S2/S6/S7）

### 2.1 单位：积分

`price_amount NUMERIC(20,5)` 落的是**积分**，不是元。录入收「元/美元」，折算与 `billing/engine.go` 同源：

```
积分 = 元 × 1,000,000 ÷ R          （R = billing.r，默认 10000）
USD ：先 × billing.cny_rate 折成人民币再套上式
```

USD 而未配 `billing.cny_rate` → **422** `rate_not_configured`（不许悄悄按 0 或按 1 折）。
`billing.r` 非法 → **500** `r_not_configured`（服务端配置问题，不是用户输入错）。

### 2.2 校验顺序（有讲究）

`语法 → 全局参数（R/汇率）→ 拓扑存在性 → 落库`。

反例：若先判拓扑，一个「价格为负 + 拓扑为空」的请求会先报 `topology_not_synced`，
运营照着去同步，回来还是 400 —— Reason 必须指向真正要改的那一处。
（`TestCreate_ValidationBeforeTopology` 守这条，把价格校验删掉即红。）

### 2.3 自然键与冲突

唯一键 `(provider_ref, account_ref)`（`uq_subscriptions_account`）。重复录入：
**409 `duplicate_subscription`** + `Data.ExistingID` + 点名现有 id 的文案；不 upsert、不合并、不静默覆盖。
PG `23505` 在 `SQLStore.Create` 里映射成 `ErrDuplicate`，其它错误原样上抛
（否则 500 会被伪装成 409）。

### 2.4 账号软校验

V2 没有网关账号镜像表，硬校验只能实时调网关 → 录入路径不可用。
改为：查近 30 天 `billing_records.upstream_account ∪ ledger_entries.provider_account`，
未命中仍**允许保存**，但响应带 `Warnings[{Code:"account_unverified"}]`。
只查账单侧会误报「账本已到、账单未归因」的账号，所以两侧都要。

### 2.5 派生状态与「会不会摊」

状态**不落列**，按 `share_cost + billing_cycle + cycle_start` 现算，优先级：
`stopped → once → missing_cycle → pending → expired → active`（与前端 `deriveSubscriptionStatus` 同口径）。

`WillAmortize` **只判 C1–C4 静态条件**（`share_off / price_zero / cycle_once / cycle_missing / cycle_expired / cycle_pending`）；
C5/C6（本期有没有可摊账单、是否已被账本占用）是运行时事实，只在 S4 明细里回显。
在录入响应里承诺「会摊」必然变成「录完摊 0 笔」。

### 2.6 本期已摊 / 未摊出

- 录入响应：单条现查。
- 列表（S1）：一条 SQL 用 `LEFT JOIN LATERAL` 按**每行自己的** `cycle_start + cycle` 算区间
  （各订阅账期起点不同，固定区间会把别的账期的钱算进来），不 N+1。
- 读失败或账期缺失 → 两个字段 `null`（前端显示「—」）。**不许用 0 冒充「算过」**。
- 「未摊出」下限 0：价格调低后已摊可能大于当期价，不显示负数。

### 2.7 手工重算（S6）

复用 `amortize.Allocator.RunOnce`，不另写一套分摊逻辑。进程内限流 **1 次/10 秒** → 429 + `Retry-After`；
被限流时**不执行**分摊。限流只针对手工入口，5 分钟的定时轮不受影响。
失败返回 500 并说明「已写入的部分下轮会 `SET` 重算收敛」（幂等，不做补偿事务）。

### 2.8 权限与审计

全部 6 个端点挂在 `/api/v1/admin/` 下，由 `WithAuth(..., RoleAdmin)` 守卫；
**不提供** `/api/v1/dev/*` 同义端点（订阅价与分摊属采购成本，不能给外部租户看）。
`internal/server/routes_stage3_test.go` 两条守卫：
无凭据必须 401；`server.go` 里任何注册这 6 个处理器的行必须以 `/api/v1/admin/` 开头，且总数恰为 6。

写操作写 `audit_logs`：`subscription.create`（含 provider/account/积分/币种/周期/账期/停摊位）、
`subscription.amortize.run`（含本轮统计与错误）。审计失败不回滚业务（数据已落库）但打 ERROR，
不静默丢审计。

## 3. 契约（与 `frontend/src/api/v2.ts` 逐字对齐，PascalCase）

- T1 `Data`：`Items[]`（Ref/ProviderRef/Kind=group|direct/DisplayName/ContextLen/PosX/PosY/LastSeenAt/Stale）、
  `Links[]`（InternalRef/ExternalModelID **字符串**/PosX/PosY）、`StaleCount`、`UnboundExternal`、`SyncedAt`、`Sync`。
- `Sync`：`LastSuccessAt` / `LastRunAt` / `State(ok|failed|never|'')` / `Message` / `Created` / `Stale`；
  空时间一律空串（不出现 `0001-01-01`）。
- T3 `Data`：`Fetched` / `Created` / `Stale` / `State` / `Message`。
- S1 `Data`：`Items[]`（`View`）/ `Total`（过滤后总数）/ `Page` / `Size`。
- S7 `Data`：`Providers[]` / `Accounts[]` / `CnyRate|null` / `R|null`。
- 错误体：沿用 `resp` 的 `Code`；**机器可读原因放 `Body.Reason`**（前端按 Reason 分支，Code 判大类）。

## 4. 已知边界（本批不修）

1. S3（编辑）/ S4（分摊明细）/ S5（手工结转）/ T2（画布坐标）未实现 —— 前端对应入口会是 pending 空态。
2. S6 限流是**单实例**进程内：多副本时每副本各 1 次/10 秒（仍远低于打爆库的量级）。要全局配额需上 Redis。
3. `Providers` 候选来自拓扑镜像，镜像为空时录入一律 409 `topology_not_synced` —— 首次部署必须先同步一次。
4. 拓扑定时轮固定 10 分钟，未做成 `sys_configs` 可配。
5. 供应商名单容错解析（多别名）是按当前网关返回值写的；网关换字段名要同时改 `source.go` 的别名表。
