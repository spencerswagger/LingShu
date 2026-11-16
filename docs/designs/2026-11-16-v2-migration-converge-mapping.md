# V2 迁移链收敛 · 对象级映射表（施工用）

> 2026-11-16 · 架构（Ark）· 依据：spencer 2026-11-16 裁定（唯一链 = `feat/lingshu-v2-sync-config` 那一套，其余分支迁移文件删除、独有 schema 并入）
> 上游文档：`docs/designs/2026-11-15-v2-p1-data-contract.md`（下称「数据层契约」，本分支版本为准）、`docs/designs/2026-11-15-v2-upgrade.md` §0.4（裁定入档）
> 受众：Core（施工方）。**本文是施工清单，不是讨论稿**；每一行都能直接落到 `git rm` / `git mv` / 改断言 / 不动的判定。

---

## 0 一句话结论

旧分支那 9 个对象（表 2 / 列 4 / 索引 2 / 约束 1）**零并入**；其中 `reconcile_windows` 不是"并入"，是**同一个表名在唯一链里已有正形定义**（`0023_v2_reconcile.sql`），旧定义整体作废、逐列按下表重新落位。
**不新增 `0023d`**（理由见 §5）；**不另起 converge 分支**（理由见 §6）。

---

## 1 施工前必须先对齐的三个事实（口径校正）

| # | 你消息里的表述 | 实测事实 | 对施工的影响 |
|---|---|---|---|
| 1 | 「两套 0019」 | **只有一套内容、三份副本**：`feat/lingshu-v2-p1-slim-migrate` / `-usage-billing` / `-magpie-passthrough` 的 `0019_v2_facade.sql` sha256 **全部 = `23ddc9d2…`（4930 B，逐字节相同）**。另外你只点了两条分支，**第三条 `-magpie-passthrough`（= PR #81）也有同一份** | 不存在"两套 0019 定义打架"。删除动作要覆盖 **3 条**分支，不是 2 条 |
| 2 | 「独有 schema 并入这一套」 | 逐对象核完：**没有一项需要并入**（§2/§3）。唯一需要"并入"的是 `reconcile_windows` 的**语义**，而它在 0023 里已经是更强形状 | 施工量在"删 + 改断言 + 改代码"，不在"补 schema" |
| 3 | 「sync-config 目录没有 0021 空号」 | 正确。0024 文件头写着「0021 是作废空号」只是**编号沿革说明**，目录里不该再有 0021 —— 门禁 `TestGate_F4_OneStartupFullChain` 会断言 `0021` 前缀不存在 | 不用补 0021，也不许复用 0021 |

另外两条口径提醒（会直接导致误判）：

- **幂等键是完整文件名**（`applied_migrations.name`），不是版本号。所以「两个不同名的 0019 会都执行」（`0019_v2_facade` 字典序在 `0019_v2_topology` 之前，先建出错形窗口表），随后 `0023` 的 `CREATE TABLE IF NOT EXISTS` 被跳过、`ALTER COLUMN local_count DROP NOT NULL` 直接 **42703 硬失败**。这与「同号同名异内容静默跳过」是两种完全不同的故障，别混。
- 契约 §5.1 里仍是旧编号（`0021_v2_drop_legacy.sql`），而目录里已是 `0024` —— **探测 SQL 以 0024 文件头那份为准**，契约 §5.1 待同步（见 §4-4）。

---

## 2 映射表 A：`0019_v2_facade.sql`（8 个对象）

### 2.1 表：`magpie_keys`

| 项 | 内容 |
|---|---|
| 旧定义 | `id BIGINT PK` / `token_id BIGINT NOT NULL REFERENCES tokens(id)` / `key_name TEXT NOT NULL UNIQUE` / `key_id TEXT` / `key_cipher TEXT NOT NULL` / `state TEXT CHECK('ACTIVE','PENDING','DISABLED')` / `last_synced_at` / `last_error` / `created_at` / `updated_at` / `UNIQUE(token_id)` |
| 唯一链落位 | **无此对象** |
| 动作 | **丢弃**（不建、不并入、不新建等价表） |
| 理由 | ① 数据层契约 §10-E 已裁定：本期「服务端单把 key，环境变量注入」，一令牌一把属**计划缺口**、明确留待阶段二拍板；② 密钥归网关侧（契约 §9.3「网关做协议互译/渠道/密钥/failover」）；③ 计费与对账都不需要它：账单归属在 `billing_records.user_id/token_id`，上游归因在 `upstream_provider/upstream_account`（0020），对账只比总量与笔数（0023 口径），**按租户切分对账不是需求**；④ 0024 的探测② 已把 `to_regclass('public.magpie_keys')` 列为「期望不存在」，并入它等于同时改探测、改 R-2 清理清单、改契约，冲突面白扩一圈 |
| 若某库已建过 | 属 S1/S2，走 R-2：`DROP TABLE IF EXISTS magpie_keys`（R-2 脚本里已有这一句） |

> **给 #81 的连带责任（P0 级，不是收敛动作）**：`backend/internal/domain/magpie/credentials.go:60,76,93,110` 与 `keyadmin.go` 全部直接读写 `magpie_keys`。只删迁移文件 = 运行期 `relation "magpie_keys" does not exist`。这两处必须在**同一分支**改掉（凭据改走服务端单把 key 的配置来源），否则 #81 不能合。

### 2.2 表：`reconcile_windows` —— 唯一需要逐列过一遍的对象

| 旧 `0019` 列 | 旧类型/约束 | 唯一链落位（`0023_v2_reconcile.sql`） | 动作 | 理由 |
|---|---|---|---|---|
| `id` | `BIGINT PRIMARY KEY` | 无 | **丢弃** | 雪花 id 无业务用途；主键改 `window_start`，`ON CONFLICT` 目标随之变成单列（旧复合唯一无法支撑 `ON CONFLICT (window_start)`） |
| `window_start` | `TIMESTAMPTZ NOT NULL` | `window_start TIMESTAMPTZ PRIMARY KEY` | **保留·升主键** | 一窗一行，"窗口起点"本身就是业务主键 |
| `window_end` | `TIMESTAMPTZ NOT NULL` | 同名 + `CONSTRAINT ck_reconcile_window_len CHECK (window_end = window_start + interval '60 seconds')` | **保留·加约束** | 窗口长度「断言进库里」比写在文档里可靠（0023 文件头原话） |
| `pulled_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | `checked_at TIMESTAMPTZ NOT NULL DEFAULT now()` | **改名为 `checked_at`** | 同一语义（本行写入时刻）；不改名会多一列同义字段 |
| `req_count_ls` | `INT NOT NULL DEFAULT 0` | `local_count BIGINT`（**可空**） | **保留·改型 + 改 NULL 语义** | `NOT NULL DEFAULT 0` 让「没算过」和「真的没流量」都变成 `0` → 漏账不可见（D4 裁定）。**这是本次收敛最重要的一条语义修正** |
| `req_count_magpie` | `INT NOT NULL DEFAULT 0` | `upstream_count BIGINT`（可空） | 同上 | 同上 |
| `tokens_ls` | `JSONB`（三段 `{input,output,cache_read}`） | `local_tokens BIGINT`（三段**合计**） | **降级·丢分段** | 0023 口径：只比总 token 合计。理由是不确定账本 `tokens_in` 含不含 cache_read，**求和不受该差异影响**；分段口径未证实就落列，只会让一半列恒 NULL |
| `tokens_magpie` | `JSONB` | `upstream_tokens BIGINT`（`tokens_in + tokens_out`） | **降级·丢分段** | 同上 |
| `dev_pct` | `NUMERIC(6,3)` | 无列；落 `detail.dev_pct` | **丢弃** | 派生量（由两列 Token 算出），判定用的数字必须落在计数列上（0023 原文），派生量进 detail 兜底即可 |
| `status` | `TEXT` + **列内联** `CHECK('ok','alarm','error','skipped')`（自动命名 `reconcile_windows_status_check`） | `TEXT` + 固定名 `ck_reconcile_status`，值域 `ok,alarm,error,blocked,skipped` | **保留·换约束名 + 扩域** | 值域扩到 5 值（`blocked` 来自旧 `0020`，`skipped` 来自 0023）；列内联 CHECK 必须删掉，否则它继续拒新值 |
| `alarm_streak` | `SMALLINT NOT NULL DEFAULT 0` | 同名同型 | **保留** | 一致 |
| `detail` | `JSONB`（可空） | `JSONB NOT NULL DEFAULT '{}'::jsonb` | **保留·收紧** | 可空会让 `detail->>'phase'` 判据（0023c 依赖）出现 NULL 分支 |
| `UNIQUE (window_start, window_end)` | 复合唯一 | 无 | **丢弃** | `window_start` 已是 PK，且 60s CHECK 排除了「同起点不同终点」，复合唯一纯冗余 |
| （无） | — | `scroll_offset BIGINT NOT NULL DEFAULT 0` | **新链独有** | 补拉断点持久化（F9 裁定） |

**判定：`reconcile_windows` 以 0023 定稿十列为准，旧定义整体作废。** 没有任何一列需要"从旧表搬过来"——旧表若已存在，是被 `CREATE TABLE IF NOT EXISTS` 整段跳过的那一类，属于**坏库**，不是"待合并的 schema"。

### 2.3 列：`external_models.magpie_model`

| 项 | 内容 |
|---|---|
| 动作 | **丢弃** |
| 唯一链替代物 | `external_models.bound_internal_ref`（`0019_v2_topology.sql`）+ 部分索引 `idx_external_models_bound` |
| 理由 | 语义同一件事（对外名 → 网关侧模型引用），但**承载关系不同**：`magpie_model` 指向"代理模型名字符串"，`bound_internal_ref` 指向 `internal_models.ref`（`provider/model` 或 `group/名称`，四层拓扑里的节点）。 |
| **额外一条硬理由（别忽略）** | 旧定义带 `uq_external_models_magpie_model`（部分**唯一**：一个代理模型只能被一个对外模型占用）。新口径下**多个对外模型可以绑同一个内部模型**（同模型对不同租户不同价是正常业务），这条唯一性在新口径里是**错的**——若为了"少改一点"把旧列搬过来，会直接卡死售卖配置 |
| 若某库已建过 | R-2：`ALTER TABLE external_models DROP COLUMN IF EXISTS magpie_model`（脚本已有） |

### 2.4 列：`billing_records.magpie_key_name`

| 项 | 内容 |
|---|---|
| 动作 | **丢弃** |
| 唯一链替代物 | `billing_records.upstream_provider` + `upstream_account`（`0020_v2_billing_attribution.sql`），检索索引 `idx_billing_upstream_time` |
| 理由 | 旧口径的"分组维度"是**一把 callerKey**；新口径的归因链是 `request_id → upstream_provider → upstream_account → upstream_protocol → internal_model_id → external_model_name/upstream_model → ledger_entry_id`，粒度更细且**不依赖灵枢自己维护密钥映射**（§2.1 已废）。`magpie_key_name` 在唯一链里既无写入方也无读取方 |

### 2.5 列：`billing_records.estimated`

| 项 | 内容 |
|---|---|
| 动作 | **丢弃**（不在收敛里建） |
| 唯一链替代物 | **无直接替代**；成本侧provenance 用 `billing_records.cost_source IN ('ledger','amortized','none')`（0020） |
| 理由 | ① 两者是不同轴：`estimated` 说的是"售价侧的 token 是估出来的"，`cost_source` 说的是"成本从哪来"，**不能互相顶替**；② V2 口径下"无上游 usage"的处理是**不计费/退预扣**（不是估算后计费），估算只发生在预扣阶段（内存量 + `credit_holds.amount`），不产生账单事实；③ 0024 探测② 已把它列为「期望不存在」 |
| 什么时候才该建 | 阶段二若确实要落"估算账单"（把估算值写成账单事实），那是**新语义**，按 0025+ 单独立项，不进本次收敛（收敛只做"去重去冲突"，不夹带新语义） |

### 2.6 列：`billing_records.upstream_usage`

| 项 | 内容 |
|---|---|
| 动作 | **丢弃** |
| 唯一链替代物 | `call_logs`（0024 的 15 张**保留表**之一，且 0018 已补 `req_params`）+ `billing_records.tokens` 三段 JSONB + `ledger_entries`（0022） |
| 理由 | 这列的存在前提是"`call_logs` 停写，原始 usage 没地方放"。**唯一链把 `call_logs` 列为保留表**（`db.RetainedTables()`，门禁 F7 断言行数不减）→ 前提消失。原样落两份 usage 反而制造第三份真源 |

### 2.7 / 2.8 索引

| 旧索引 | 动作 | 理由 |
|---|---|---|
| `uq_external_models_magpie_model`（部分唯一） | **丢弃** | 随 §2.3，且新口径下该唯一性是错的 |
| `idx_billing_magpie_key_time`（部分） | **丢弃** | 随 §2.4；新链的等价物是 `idx_billing_upstream_time`（provider+account+time）与 `idx_billing_request` |

### 2.9 回填 / 种子

| 项 | 内容 |
|---|---|
| 回填 | **旧 `0019` 零 DML、零回填**（文件头自述"纯 DDL、零 DML、零回填"）→ 无回填需迁移 |
| 种子 | **无**（两个旧文件都不插任何行） |
| 唯一链的回填 | 只有 `0023b`（blocked/error 行的 0 占位归 NULL）与 `0023c`（`detail.phase='sync'` 的 0 占位归 NULL），**均为唯一链自有，与旧分支无关**，保留不动 |

---

## 3 映射表 B：`0020_reconcile_blocked_status.sql`（1 个对象）

| 项 | 内容 |
|---|---|
| 对象 | 约束 `reconcile_windows_status_check`：值域加 `'blocked'`（做法 = `DROP CONSTRAINT IF EXISTS` 后 `ADD CONSTRAINT` 重建） |
| 唯一链落位 | `0023_v2_reconcile.sql` 末尾：`DROP CONSTRAINT IF EXISTS reconcile_windows_status_check` → 建**固定名** `ck_reconcile_status`，值域 `ok,alarm,error,blocked,skipped`；`0023b` 再做一遍（幂等，防旧库同名错定义） |
| 动作 | **丢弃文件**（内容 100% 已被 0023 absorbed） |
| 兼容红利（无需额外施工） | 旧 `0020` 用的约束名恰好是 `reconcile_windows_status_check`，而 0023/0023b 的第一句就是 `DROP CONSTRAINT IF EXISTS` **同一个名字** → 任何跑过旧 0020 的库，只要 0023b 能执行到，旧约束会被自动清掉，不需要单独处理 |
| 注意 | 这个红利**只在 0023 能跑到 0023b 时成立**。若库是"旧形状窗口表"（S1/S2），0023 在 `ALTER COLUMN local_count` 就 42703 崩了，走 R-2 |

### 3.1 旧 0020 的 `blocked` 语义有没有丢？

没有。0023 文件头的状态值域表已经写明：`blocked` = 单轮未拉完（页数/条数上限）或账本侧数据不可得 → **要补拉，不是终态**，并配了 `scroll_offset` 断点列与 `idx_reconcile_blocked` 偏索引。旧 0020 只加了值域、没有断点能力和补拉扫描，0023 是它的超集。

---

## 4 连带必须改的地方（删文件不删这些 = 门禁红 / 自检假阳性）

| # | 位置 | 要做什么 | 为什么 |
|---|---|---|---|
| 1 | `backend/internal/db/migrate_test.go` → `TestMigrate_0019_V2Facade`（slim 与 usage-billing **各一份，行号一致**：291–366） | **整函数删除** | 它断言 `magpie_keys` / `reconcile_windows` 存在、四个列存在（350–355）、两个部分索引齐全（359–364）。文件删了这函数必红 |
| 2 | 同上文件 `:109` / `:144` 提到 `call_logs.upstream_usage` | 保留 | 那是 **`call_logs`** 的列（0012 建的），与被删的 `billing_records.upstream_usage` 同名不同表，**别一起删** |
| 3 | `backend/internal/db/v2_tables.go` | **不动** | `V2MigrationFiles()` 那 7 个文件名与目录一致；`RetainedTables()`/`RetiredTables()` 与旧分支无关 |
| 4 | `0024_v2_drop_legacy.sql` 文件头「探测①」的排除清单 | **补两个文件名**：`0023b_v2_reconcile_repair.sql`、`0023c_v2_reconcile_counts_backfill.sql` | 现在的清单只排除 5 个（0019/0020/0022/0023/0024）。链跑完后 `applied_migrations` 里 0023b/0023c 会被判成"旧分支污染" → **每次自检 2 条假阳性**。这是实打实的缺陷（P1），不改就等着执行时误判 S1 |
| 5 | 数据层契约 §5.1（`probe-1` / `probe-4` / §5.2 状态矩阵 / R-2 的 `DELETE` 清单） | 编号 `0021_v2_drop_legacy.sql` → `0024_v2_drop_legacy.sql`，并补 0023b/0023c | 文档与目录已漂移；R-2 的 `DELETE` 逐条列名里写的是 0021，照抄会漏删 0024 记录（幂等但记录语义乱） |
| 6 | 契约 §5.1 标题「三条 SQL」 | → 「四条」 | 正文自己写着"四条探测缺一不可" |
| 7 | `docs/designs/2026-11-15-v2-upgrade.md` §6A 表「迁移」行 | 已作废标注（§0.4-2 已加），核对无残留 | 避免后来人按 slim 路径施工 |

---

## 5 裁定：要不要新增 `0023d` 幂等修形？——**不新增**

你的直觉（"不改已冻结文件"）本身是对的，但在这件事上**这条路走不通**，三条理由任何一条都足够：

1. **排序上救不了。** 字典序 `0023 < 0023d`。旧形状窗口表上，`0023` 自己会在 `ALTER TABLE reconcile_windows ALTER COLUMN local_count DROP NOT NULL` 处 **42703 硬失败**（旧表没有 `local_count` 这一列），整文件事务回滚、链停住 —— `0023d` 根本轮不到执行。**修形必须发生在 0023 之前或之内，不能在其后。**
2. **职责重叠。** 唯一链里"修形"这个位置已经有两个文件：`0023b`（0020 早期形状 → 最终态）、`0023c`（同步轮次 0 占位 → NULL）。它们修的是"**本链早期版本**"，不是"**旧分支形状**"。再加一个 `0023d` 会让三份修形逻辑叠在一起，后来人分不清哪份对哪种库生效 —— 这正是 0023b 文件头抱怨过的坑。
3. **旧形状库不值得为它永久留一条迁移。** 生产库没跑过旧分支（main 只到 0018）；0024 也还没上任何环境。可能处于旧形状的只有 dev/test —— 而 `R-2`（回干净基线）实测 schema 与干净基线 **0 差异**，成本≈0。为几台 dev 库永久背一条迁移，收益为负。

**唯一例外（先说清触发条件，免得来回问）**：如果将来出现"某环境**有真实数据不能重来**且处于旧形状"，那正确做法也**不是**加 `0023d`，而是**改 0023 内部**补 4 句 `ADD COLUMN IF NOT EXISTS`（它已经对 `scroll_offset` 用了这个模式，属于同一设计意图的补全，不是改语义）。但即便如此 **PK（`id` → `window_start`）与 60s CHECK 仍然自愈不了**（`CREATE TABLE IF NOT EXISTS` 整段跳过，这两样根本没建），最终还是回到 R-2。所以这条例外**现在不要做，也不许提前做**。

**所以：三套 `reconcile_windows` 定义以哪套为准？** 以 **`0023_v2_reconcile.sql` 的十列定稿形状**为准（`window_start` PK / 四计数列可空 / `scroll_offset` / `ck_reconcile_status` 五值 / `ck_reconcile_window_len` 60s）。`magpie_keys` 不是"哪套为准"的问题，是**两类分支都不该建**。

---

## 6 裁定：分支怎么走？——**不另起 `feat/lingshu-v2-migration-converge`**

理由：所谓"收敛"拆开看是四件性质不同的事，**没有一件需要一条新的收敛分支**：

| 动作 | 落点 | 说明 |
|---|---|---|
| ① 删旧 `0019`/`0020` | **各自分支内部** | slim-migrate 若无代码要合 → 整条分支作废（由 hugo/spencer 删远端），删文件是白做工；#81（usage-billing / magpie-passthrough）要合 → **在该分支上一次提交 `git rm` 两个迁移文件 + 改 §2.1 那批读写方**，这与既有裁定 §7-17「同一分支改完再合」一致 |
| ② 链侧收尾（§4 的 4/5/6 项） | **`feat/lingshu-v2-sync-config`** | 全是文档/注释/探测清单层面，1–2 个 commit 收尾；`v2_tables.go` 与目录本来就在这条分支上，换分支只会造成分叉 |
| ③ 代码侧摘除（0024 的 P0，见 §7） | **`feat/lingshu-v2-sync-config`**，与 0024 同批 | 关键判断：0024 与"摘掉读渠道族的代码"是**同一件事的两半**。拆成两条分支反而制造"只合其中一半"的窗口 —— 那正是 P0 的成因。同一 PR 里评审，Owl 看的是 PR 不是分支 |
| ④ 阶段二计费（A1 预扣） | **单独分支、单独 PR** | 新功能 + 可独立回滚，不该和**不可逆**的 0024 混在一个 diff。你说"表未到之前先做计费"——对，但**别塞进 0024 那个 PR** |

一句话给你执行：**继续在 `feat/lingshu-v2-sync-config` 上做链侧收尾与代码摘除；阶段二计费另开分支；旧分支的文件删除在各分支内部完成，不另起 converge 分支。**

---

## 7 P0 定性：0024 与代码硬冲突 —— 这不是迁移问题，是**上线排序问题**

先把机制钉死（避免"改迁移文件"这种错解）：

`backend/cmd/server/main.go` 启动序：`db.Migrate`（:146）→ `buildApp` → `app.cpSvc.InitialSync` → **`app.mgr.SyncFromDB(ctx)`（:184）失败即 `os.Exit(1)`**。而 `SyncFromDB`（`internal/domain/channel/manager.go:484`）第一步就是 `m.store.List`（查 `channels`）与 `keyStore.ListAll`（查 `channel_keys`）—— 0024 把这两张表删了，所以**只要 0024 与代码同批上线，服务必然起不来**。这是 0024 文件头「已知缺口」自己写明的（"上线前必须先摘代码"），不是漏掉的 bug。

**定性：0024 不许改**（它是裁定过的终态），**要改的是代码**；且"摘代码"是 0024 的**前置闸门**，不是并行任务。0024 文件头「已知缺口」列的四处，精确落点：

| 读/写方 | 文件:行 | 0024 之后的后果 |
|---|---|---|
| 启动装载渠道运行态 | `cmd/server/main.go:184` → `domain/channel/manager.go:484,491` | **启动即 exit=1**（P0 的"启动即死"就是这一条） |
| 会话/账单展示的渠道密钥名反查 | `cmd/server/main.go:948`（`keyNameResolver`，`SELECT id,name FROM channel_keys`） | 打开会话/账单列表 500 |
| 账单按密钥聚合、渠道名展示 | `domain/billing/stats.go:340,341`（`JOIN channel_keys`/`JOIN channels`）、`:537`（`FROM channel_models`）、`:806`（`LEFT JOIN channel_keys`） | 统计/成本页 500 |
| 账单查询与写入 | `domain/billing/query.go:45,48,53`、`service.go:1091`（INSERT 含 `channel_key_id`）、`:1148,1152` | **每笔账单落库失败**（42703） |
| 探测记账 | `cmd/server/main.go:476,506`（`ProbeRecorder` 首参 `channel_key_id`） | 探测链路 500 |
| 令牌标签解析 | `cmd/server/main.go:920`（`tagKVResolver`，依赖 `tokens.tag_id` → `semantic_tags`） | 令牌标签空/报错 |
| 渠道域整包 | `domain/channel/*`、`domain/automodel/*`（整包读写 `auto_models`/`auto_model_members`） | 管理端渠道/自动模型页全废 |
| 历史渠道密钥名（**别误删**） | `billing/query.go:45`、`service.go:1146` 的注释「随 0021 同批删除」 | 这两处注释就是施工路标；注意 `call_logs.channel_key_id` **是保留列**（0024 注释明确"历史日志快照不删"），读路径要从 `billing_records.channel_key_id` 切到 `call_logs.channel_key_id`，不是全删 |

⚠️ 一处**旧口径陷阱**：`docs/designs/2026-11-10-channel-field-ownership-contract.md` 与更早的一期结论写着「账单历史页对 `channel_keys` 取密钥名的查询要留，改 `LEFT JOIN` + `COALESCE`」。那是**一期零 DROP** 口径下的说法；0024 把表删了，`LEFT JOIN` 到被删表一样报错。**按 0024 落地**，别再照一期口径施工。

**验收判据（Gate 可直接写成断言）**：在"链跑完（含 0024）"的库上，服务能起来且以下三条 SQL 不报错：`SyncFromDB`、账单列表、统计聚合。等价于「`git grep -E 'FROM|JOIN' -- '*(channels|channel_keys|channel_models|auto_models|semantic_tags|probe_logs)'` 在非测试代码里 0 命中」。

---

## 8 施工顺序（建议）

1. **链侧收尾**（sync-config，1 个 commit）：§4 的 4/5/6 三项（探测清单补 0023b/0023c、契约编号 0021→0024、标题三条→四条）。
2. **代码摘除**（sync-config，与 0024 同 PR）：§7 清单逐项摘，跑门禁；空库一次启动跑完整链 + 服务起得来 = P0 关闭。
3. **旧分支清理**：#81 分支内 `git rm 0019_v2_facade.sql 0020_reconcile_blocked_status.sql` + 删 `TestMigrate_0019_V2Facade` + 改 `magpie/credentials.go|keyadmin.go|handler.go`（§2.1 的连带责任）；slim-migrate 的交由 spencer/hugo 决定"作废分支"还是"只摘迁移文件"。
4. **阶段二计费**（A1 预扣）：单独分支，`credit_holds` 表 0022 已建好可直接用（"只建不写"就是给这一步留的）。
5. 环境处置：现有 dev/test 库先按契约 §5.1 跑**四条探测**；命中 S1/S2/S3/S5a/S5b 一律 **R-2 回干净基线**再跑链，别徒手补 DDL。

---

## 9 一句话给 Gate（可直接转成用例）

> 旧 `0019`/`0020` 的 9 个对象**一个都不许存在于链跑完后的库**（`magpie_keys`、`external_models.magpie_model`、`billing_records.magpie_key_name/estimated/upstream_usage`、两个部分索引全无）；`reconcile_windows` 必须**恰好**十列 + `PRIMARY KEY (window_start)` + 60s CHECK + 五值 `ck_reconcile_status`；且 `applied_migrations` 里除 7 个唯一链文件名外 0 条。
