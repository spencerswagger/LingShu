# 灵枢 V2 阶段一 · 数据层与口径定稿（0019–0023）

> 2026-11-15 · Ark（架构）· 依据 `docs/tasks/v2-2026-11-15.md`（唯一口径）与已批准的
> `docs/v2-impl-plan-2026-11-15.md` 五条开放项决议。
> 本文是阶段一后端实现的**唯一数据层依据**：表结构、字段、枚举、写入时机、证据流程。
> 与已批准计划有出入的地方集中在 §10，均给出理由，其中 3 条需负责人一句话拍板（A / E / F）。

> ## ⚠️ 本文的 SQL 与清单已过期，执行一律以代码为准
>
> 本文写于 2026-11-15、编号还是 `0019–0023`（DROP 文件叫 `0021_v2_drop_legacy`）。之后的裁定是：
> DROP 文件更名 **`0024_v2_drop_legacy`**、`0019_v2_facade` 拆成 **`0019b_v2_passthrough_credentials`**、
> 补了 `0023b` / `0023c` 两个幂等修形迁移。**照本文 §5 / §4 / §14 的 SQL 直接跑会造出坏库**
> （最典型：R-2 的 DELETE 清单里留着不存在的 `0021_v2_drop_legacy.sql`、缺 `0019b/0023b/0023c`，
> 跑完再迁移时 `0019b` 被跳过 → `magpie_keys` 与 `external_models.magpie_model` 永久消失，启动即 42P01/42703）。
>
> **唯一可执行来源**（本文只作设计意图与状态矩阵的解释）：
> 1. `backend/internal/db/migrations/0024_v2_drop_legacy.sql` 文件头的四条探测与处置；
> 2. `backend/scripts/acceptance/v2-preflight/`（`probe-1..4.sql` + `run.sh`，与 `db.V2MigrationFiles()` 同源，有源码级用例守）；
> 3. `backend/docs/plans/2026-11-15-v2-0024-preflight-evidence.md`（三步证据实测）。

---

## 0 结论先行

1. 编号从 **0019** 重新起号（落地后共 **8 个**文件，见 §1 表），**唯一允许出现 DROP 的文件是 `0024_v2_drop_legacy.sql`**（原 `0021` 更名）。
2. 账本先落 **9 列（8 业务字段 + 1 本地主键）**，分页按 **窗口内 offset**，**不持久化游标**；幂等靠 `ledger_id` 唯一键。
3. 归因统一 `upstream_*` 命名，**全仓不出现第三方产品名**；唯一例外是「回程剥头」的匹配前缀（见 §10-F）。
4. 四协议透传落在**入站 handler 与出站 client 之间的适配层**，网关侧零改动；Gemini 现网**没有路由**，必须新增两条（见 §6）。
5. `0024` 的 DROP 绑定「pg_dump → 临时库还原 → 行数比对」，**判定点 A/B/C 任一不过即中止**（实测数据见 `backend/docs/plans/2026-11-15-v2-0024-preflight-evidence.md`）。

---

## 1 编号与职责

主库实测：`origin/main` 最大迁移是 `0018_call_logs_req_params.sql`。所以 0019 起号与主线**不撞号**（旧分支的 `0019_v2_facade.sql` / `0020_reconcile_blocked_status.sql` 未合入，作废、不 cherry-pick）。

> **唯一来源（合并冲突按这条裁）**：0019–0023 五个迁移文件与本文的**唯一来源是本分支 `docs/lingshu-v2-p1-data-contract`**。
> 代码分支 `feat/lingshu-v2-sync-config` 里那份同名副本是早期 cherry-pick 的旧版（文件头探测还是裸条件，会永远误报），
> `git merge-tree` 实测三处 add/add 冲突。合并时以本分支为准、整份丢弃另一份：
> `git checkout origin/docs/lingshu-v2-p1-data-contract -- backend/internal/db/migrations/00{19,20,21,22,23}_*.sql docs/designs/2026-11-15-v2-p1-data-contract.md`

注意区分两件事：**编号不冲突 ≠ 幂等键安全**。`migrate.go` 的幂等键是**文件名**（`applied_migrations.name`），所以真正的风险是：① 同名不同内容 → 静默跳过、schema 缺列不报错；② 同编号不同名（旧分支）→ 两条都执行、DDL 撞车。执行前两条查询必做（§5 步骤 0）。

| 编号 | 文件（`backend/internal/db/migrations/`） | 一句话职责 | 可回滚 | 幂等写法 |
|---|---|---|---|---|
| 0019 | `0019_v2_topology.sql` | 建四层图的三张表（`internal_models` / `model_links` / `subscriptions`）+ `external_models` 加绑定列 | 是（DROP 三表 / DROP 列） | `IF NOT EXISTS` / `ADD COLUMN IF NOT EXISTS` |
| 0020 | `0020_v2_billing_attribution.sql` | `billing_records` 只加归因列与索引（7 列），**纯 ADD、零 DROP、零 DML** | 是 | 同上 |
| ~~0021~~ | ~~`0021_v2_drop_legacy.sql`~~ → **`0024_v2_drop_legacy.sql`** | **唯一的 DROP 文件**：**10 张实际退役表**（另 1 条 `channel_events` 是 0009 已删的空操作）+ 3 个阻塞列 | **否**（必须走 §5 证据流程） | `DROP TABLE/COLUMN IF EXISTS` |
| 0019b | `0019b_v2_passthrough_credentials.sql` | 从已删除的 `0019_v2_facade` 拆出**仍被代码使用**的对象：`magpie_keys` + `external_models.magpie_model` + 部分唯一索引 | 是 | `IF NOT EXISTS` |
| 0023b | `0023b_v2_reconcile_repair.sql` | 老库修形（窗口状态放开 `skipped`、`scroll_offset`、账单成本 CHECK 改 `IS NOT DISTINCT FROM`） | 是（幂等） | 全部幂等写法 |
| 0023c | `0023c_v2_reconcile_counts_backfill.sql` | 把同步写过的「0 占位」计数归 NULL（0 只表示算过且真的没流量） | 是（幂等） | `UPDATE ... WHERE` |
| 0022 | `0022_v2_ledger.sql` | 建 `ledger_entries`（账本最小九字段）与 `credit_holds`（预扣登记，阶段二启用） | 是（DROP 两表） | `IF NOT EXISTS` |
| 0023 | `0023_v2_reconcile.sql` | 建 `reconcile_windows`（60s 对账窗口与告警连续计数） | 是（DROP 表） | `IF NOT EXISTS` |

> 五个 SQL 已落成实体文件（本分支 `docs/lingshu-v2-p1-data-contract`，与 §14 附录逐字一致）：
> Core 与 Gate 直接 `git fetch` 该分支即可，不用再抄文档。落文件时确认过两件事：
> `ledger_entries.ledger_id` 的唯一索引与 `ON CONFLICT (ledger_id) DO NOTHING` 冲突目标逐字一致；
> `credit_holds` 在文件头写明「阶段一只建不写」，避免后来人当漏写。

执行前断言（防撞号）：

```sql
-- 必须返回 0018_call_logs_req_params.sql，否则有人已把旧分支合入 → 停手报主持人
SELECT name FROM applied_migrations ORDER BY name DESC LIMIT 1;
```

---

## 2 0019_v2_topology.sql

### 2.1 `internal_models`（网关同步的只读镜像 · 节点）

| 列 | 类型 | 可空 | 默认 | 说明 |
|---|---|---|---|---|
| `ref` | `TEXT` | NOT NULL | — | **主键**。网关侧模型引用（`provider/model` 或 `group/名称`），全局唯一 |
| `provider_ref` | `TEXT` | NOT NULL | `''` | 所属供应商（网关 provider） |
| `kind` | `TEXT` | NOT NULL | `'direct'` | `CHECK (kind IN ('group','direct'))`；组/直通同层，**不渲染 auto 组**（同步时直接过滤） |
| `display_name` | `TEXT` | NOT NULL | `''` | 展示名 |
| `context_len` | `BIGINT` | NOT NULL | `0` | 上下文长度；网关未给则 0 |
| `caps` | `JSONB` | NOT NULL | `'{}'` | 能力位（vision / tools / thinking …），原样保存 |
| `list_price` | `JSONB` | NOT NULL | `'{}'` | 网关牌价**原样**；换算只在一处（§10-D），不留第二份换算结果 |
| `pos_x` / `pos_y` | `DOUBLE PRECISION` | NOT NULL | `0` | 画布坐标（内部模型节点） |
| `last_seen_at` | `TIMESTAMPTZ` | NOT NULL | `now()` | 每轮同步命中即刷新；**判活在它，不用 deleted_at** |
| `stale` | `BOOLEAN` | NOT NULL | `false` | `true` = 本轮同步未见（网关不可达或该条已消失），仍展示但标灰 |
| `created_at` / `updated_at` | `TIMESTAMPTZ` | NOT NULL | `now()` | — |

索引：`(provider_ref)`、`(kind)`、部分索引 `(last_seen_at) WHERE stale = false`。

口径：**镜像表不建软删列**。`stale` 由同步器按「本轮未命中」置位，避免与同步器语义打架；删除判定交给网关（它才是真源）。

### 2.2 `external_models` 追加 1 列（绑定真源）

```sql
ALTER TABLE external_models ADD COLUMN IF NOT EXISTS bound_internal_ref TEXT;
CREATE INDEX IF NOT EXISTS idx_external_models_bound
    ON external_models(bound_internal_ref) WHERE bound_internal_ref IS NOT NULL;
```

- `NULL` = 未绑定 = **不售卖**（对外模型列表按 `enabled AND bound_internal_ref IS NOT NULL` 过滤）。
- 「一个内部模型挂多个外部模型」= 多行同值；**绑定真源只此一列**（理由见 §10-G）。

### 2.3 `model_links`（画布落位 · 不是关系真源）

```sql
CREATE TABLE IF NOT EXISTS model_links (
    internal_ref      TEXT   NOT NULL,
    external_model_id BIGINT NOT NULL REFERENCES external_models(id) ON DELETE CASCADE,
    pos_x             DOUBLE PRECISION NOT NULL DEFAULT 0,
    pos_y             DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (internal_ref, external_model_id)
);
```

- 表里**没有绑定语义**：缺行 = 用自动布局，不影响售卖与计费。
- 不建到 `internal_models` 的外键：同步器会整批替换镜像，外键会让同步顺序变成约束问题。

### 2.4 `subscriptions`（账号财务扩展 · 一对一）

| 列 | 类型 | 可空 | 默认 | 说明 |
|---|---|---|---|---|
| `id` | `BIGINT` | NOT NULL | — | 主键（雪花，与全库一致） |
| `provider_ref` | `TEXT` | NOT NULL | — | 供应商 |
| `account_ref` | `TEXT` | NOT NULL | — | 网关账号 id/name（**一对一**靠下面的唯一键表达） |
| `price_amount` | `NUMERIC(20,5)` | NOT NULL | `0` | 账期费用（同 `credit_wallets.balance` 同型，便于直接加减） |
| `price_currency` | `TEXT` | NOT NULL | `'CNY'` | `CHECK IN ('CNY','USD')`；USD 仅作录入，换算只在成本侧一次 |
| `billing_cycle` | `TEXT` | NOT NULL | `'month'` | `CHECK IN ('month','year','once')` |
| `pay_day` | `SMALLINT` | NOT NULL | `1` | `CHECK BETWEEN 1 AND 28`（29–31 会跨月错位） |
| `share_cost` | `BOOLEAN` | NOT NULL | `true` | 是否参与成本分摊 |
| `cycle_start` | `DATE` | NULL | — | 当前账期起点；`NULL` = 未开始摊销 |
| `note` | `TEXT` | NOT NULL | `''` | — |
| `created_at` / `updated_at` | `TIMESTAMPTZ` | NOT NULL | `now()` | — |

```sql
CONSTRAINT uq_subscriptions_account UNIQUE (provider_ref, account_ref)
```

- **不落账期累计 token 列**：摊销按窗口从 `ledger_entries` 现算，避免每请求写同一行（热点 + 与账本不一致）。
- 主键仍是雪花 `id`，一对一由唯一键表达（§10-G 同源裁定）。

### 2.5 0019 明确不建的东西

配额窗口（`/v1/…/quotas`）**不入库**：它是易变状态，进程内 TTL 缓存即可；落表会立刻变成第三份真源。

---

## 3 0020_v2_billing_attribution.sql（纯 ADD）

| 列 | 类型 | 可空 | 默认 | 用途 |
|---|---|---|---|---|
| `request_id` | `TEXT` | NULL | — | 入站生成，串起日志/账单/账本 |
| `upstream_provider` | `TEXT` | NULL | — | 归因：供应商（= 网关渠道） |
| `upstream_account` | `TEXT` | NULL | — | 归因：账号 |
| `upstream_model` | `TEXT` | NULL | — | 归因：上游实际服务模型（**不是**对外模型名） |
| `upstream_protocol` | `TEXT` | NULL | — | `CHECK (IS NULL OR IN ('openai-chat','openai-responses','anthropic-messages','gemini'))` |
| `ledger_entry_id` | `TEXT` | NULL | — | 账本条目幂等键；**部分唯一索引**（历史行全 NULL 不进索引） |
| `cost_source` | `TEXT` | NULL | — | `CHECK IN ('ledger','amortized','none')`：成本从哪来；`cost_credits IS NULL` 时必为 `none` 或 NULL |

索引：

```sql
CREATE UNIQUE INDEX IF NOT EXISTS uq_billing_ledger_entry
    ON billing_records(ledger_entry_id) WHERE ledger_entry_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_billing_upstream_time
    ON billing_records(upstream_provider, upstream_account, call_time DESC)
    WHERE upstream_provider IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_billing_request
    ON billing_records(request_id) WHERE request_id IS NOT NULL;
```

两个「不做」：

- **不动 `pricing_mode`**：用户侧仍只有 `sale` / `cost`。订阅摊销是**成本侧**口径，混进用户定价枚举会让报表口径二义（§10-C）。
- **不加 `channel_*` 兼容列**：0021 会删掉 `channel_key_id`，历史「渠道密钥名」维度永久丢失——这是已批准方向，写进 PR 影响面（§10-B）。

---

> **§4 已过期**：DROP 文件现名 `0024_v2_drop_legacy.sql`，正文以迁移文件为准（含 10 张表 + 3 列的完整清单、删除顺序与外键说明）。
> 本节保留的只是「为什么先删列再删表」的推理，**不要照抄 SQL**。

## 4 ~~0021~~ → 0024_v2_drop_legacy.sql（唯一不可回滚文件）

> 下文提到「0021」的地方一律读作「`0024_v2_drop_legacy.sql`」（仅编号/文件名改了，删表删列的内容与顺序不变）。

### 4.1 先删列（解除外键/非空阻塞）

```sql
ALTER TABLE billing_records DROP COLUMN IF EXISTS channel_key_id;   -- 连带 idx_billing_key_time
ALTER TABLE sessions        DROP COLUMN IF EXISTS channel_key_id;   -- NOT NULL，必须删；连带 idx_sessions_key
ALTER TABLE tokens          DROP COLUMN IF EXISTS tag_id;           -- 引用 semantic_tags
```

`call_logs.channel_id` / `channel_key_id` / `auto_model_name` **不删**：无外键、不阻塞，且是历史日志快照。删了就没有第二份证据可查（§10-B）。

### 4.2 再删表（子表在前，顺序不可调）

**实际删除 10 张**（子表 → 父表）：

```
channel_tags → channel_model_events → channel_key_events → probe_logs
→ channel_models → channel_keys → channels
→ auto_model_members → auto_models → semantic_tags
```

文件里还有第 11 条 `DROP TABLE IF EXISTS channel_events`：**它在 `0009_drop_channel_state.sql` 就已经被删了**（该文件末句就是 `DROP TABLE IF EXISTS channel_events;`），所以永远是空操作。它留在文件里只为「清单读起来完整 + 手工恢复过该表的环境也能收拾干净」，**数表时按 10 张数，不要按 11 张数**（按 11 数会以为漏了一张而去找不存在的表）。

全部 `DROP TABLE IF EXISTS`。**保留不删**：`channel_protocols`、`protocol_error_stats`（不在批准清单；无写入方后是孤岛表，等阶段二后单独拍板清理），`watchlist_items` / `price_change_alerts`（价格监控，未批准下架）。

### 4.3 为什么 DROP 只出现在一个文件（原 0021 → 现 0024）

`migrate.go` 每文件一个事务（`internal/db/migrate.go:16-70`），PG 的 DDL 事务性 → 0021 中途失败会整体回滚，不需要手工 restore（严格结论与 dump 的实际作用见 §12-①）。把所有不可逆动作集中在**一个**文件，等于「备份 → 证据 → 执行」只走一次，回滚点只有一个。

### 4.4 风险点与回滚（实测路径）

两条风险，处置不同：

| 风险 | 兜底 | 实测状态 |
|---|---|---|
| **跑一半**（0021 第 5 句报错） | 单文件事务自动回滚到 0018 状态，`applied_migrations` 不记 0021，重试安全；不需要 dump | 已实测：DDL 末尾注入 `SELECT 1/0` → 整份事务回滚、表与列原样 |
| **已 commit 后发现删错 / 要取回历史渠道密钥名** | 用同一份 dump 整库还原回迁移前 | 已实测：同一 dump 还原后**行数 0 差异** |

整库还原（已实测的命令形态，`$BK` 即步骤 1 的 dump）：

```bash
pg_terminate_backend_all $SRC_DB            # 先踢连接：生产上先把灵枢进程停掉
dropdb --if-exists $SRC_DB                  # 或先 ALTER DATABASE ... RENAME TO ${SRC_DB}_broken 保底
createdb -T template0 $SRC_DB
pg_restore -d $SRC_DB --no-owner --no-privileges -j4 "$BK"   # 行数应与 before-all.txt 逐字相同
```

**这条回滚有窗口期限制**：dump 是 0021 执行「之前」的快照，所以还原会丢掉「迁移后 → 回滚前」写进库的增量（新账单、新账本条目、新用户）。因此回滚只适用于「迁移刚 commit 就发现错」的窗口；超过一个业务周期再回滚，正确做法是从 dump 把被删表的**历史数据单独抽出来**补，而不是整库覆盖。

---

## 5 DROP 文件执行手册（探测 → 状态矩阵 → 证据 → 判定）

> ⚠️ 本节只讲**判定逻辑**（状态矩阵与 R-1/R-2 的取舍）；**可执行的探测 SQL 已移到**
> `backend/scripts/acceptance/v2-preflight/`（与 `db.V2MigrationFiles()` 同源、有源码级用例守漂移），
> 本文不再重复贴 SQL（贴一份就会漂一份）。

> 执行人：持有宿主 PG 的人（spencer / hugo）。**沙盒内 PG 拒连**（实测 `127.0.0.1:5432` Connection refused），且 `lingshu_dev` 无 `CREATEDB`（2026-11-10 实测），临时库必须由有权限的角色建。
>
> **本流程已在演练库完整跑过**（21 项全绿，A/B/C/D 四个判定点都构造出了红态，另加 `SELECT 1/0` 负向对照）。
> **演练环境 ≠ 生产**：演练源库是按迁移脚本重建 + 合成造数的**等价物**，真实 `migrate.go` 触发路径（服务启动时自动迁移）未在演练中验证；
> 正式执行必须在**有真实数据的宿主**上按同一脚本重跑，演练的行数/耗时数字不代表生产。

### 5.1 执行前探测：三条 SQL，缺一不可（**不能只信 `applied_migrations`**）

实测背景（测试环境用真实提交复跑）：三个旧分支的 `0019_v2_facade.sql` 逐字节相同（sha256 `23ddc9d2…`，4930 B）。在一个**跑过旧分支**的库上执行本版 0019–0023，**五个文件逐条 `exit 0`、`applied_migrations` 记到 0023、零报错**，但库是坏的：

- `reconcile_windows` 被 `CREATE TABLE IF NOT EXISTS` 整段跳过（新列 / PK / 60s CHECK 全缺），与干净基线差 **96 行**；
- 旧表 `magpie_keys` + 4 个旧列 + 2 个旧索引遗留；
- 第一次写窗口才炸：`column "local_count" does not exist`；只写两键时 `ON CONFLICT (window_start)` 报 `no unique or exclusion constraint matching the ON CONFLICT specification`（旧表主键是 `id`）。

⇒ 记录表记的是「**文件跑过**」，不是「**对象建对**」。**四条**探测缺一不可：

四个探测**不要手抄**，用入库的可执行版本（与 `db.V2MigrationFiles()` 同源，有 `TestV2Probe_RecordWhitelistMatchesMigrationFiles` 等用例守漂移）：

```bash
cd backend
sh scripts/acceptance/v2-preflight/run.sh "$DSN"     # 依次跑 ①记录 ②对象 ③形状 ④反查，任一非空即停
# 单跑某一项：
psql "$DSN" -tA -f scripts/acceptance/v2-preflight/probe-1-records.sql
psql "$DSN" -tA -f scripts/acceptance/v2-preflight/probe-2-legacy-objects.sql
psql "$DSN" -tA -f scripts/acceptance/v2-preflight/probe-3-window-shape.sql
psql "$DSN" -tA -f scripts/acceptance/v2-preflight/probe-4-objects-by-record.sql
```

口径要点（脚本里逐条写了原因，这里只列结论）：
- ① 记录层**排除式 + 有右界**，白名单必须等于 `db.V2MigrationFiles()`（当前 8 个文件：0019 / 0019b / 0020 / 0022 / 0023 / 0023b / 0023c / 0024）；
- ② 对象层**不再把 `magpie_keys` / `magpie_model` 当残留**——它们是 0019b 收回本链的现役对象；
  只剩「账单三列**有数据**」这一条可验证事实（列存在不算异常，源码零写入由用例守）；
- ③ 形状层：`reconcile_windows` 未建或列齐全（含 `scroll_offset` 与四计数列），缺 `local_count` 即「被跳过」；
- ④ 反查：按已记文件名核对象是否真在（专治 S4）。

**判读口径（照抄实测，别自己简化）**：

- **③ 输出的是「列清单」，不是判定**：实测 S1/S2/S3 给 12 行、S5b 给 11 行（S5a 少于 10 行）。所以**不能只判「③ 空/非空」**——那样会把 S1–S3 读成「还行」。必须拿清单与十列集合**逐项比对**（`window_start, window_end, local_count, upstream_count, local_tokens, upstream_tokens, status, alarm_streak, detail, checked_at`），缺任何一列即形状不对。
- **④ 只 JOIN「本版五个文件名」**：旧分支的记录名（`0019_v2_facade.sql` / `0020_reconcile_blocked_status.sql`）根本匹配不到，所以 **S1/S2 的 ④ 是 0 行**（实测），它们的判定靠 ①②，不靠 ④。
- **④ 有输出 → 记录说建过、对象却不在（S4）**，按 §5.2 走 R-2。
- **四条全净（0 / 0 / 空 / 0）≠ 一定可以执行**：S5b（十列齐、约束不符）实测就是 `0 / 0 / 11 / 0`，见 §5.2 —— 这种库只有 v2 断言能拦。

**为什么必须四条**：S4（记录在、对象缺）这种库里 ①②③ 恰好全是 0 行 —— 旧写法「S4 靠 ①+② 组合判」是错的，这两格在 S4 都给不出任何信号；只有 ④ 能判（实测 S4 上 ④ 输出 9 行，逐条点名缺失对象）。

**键约束确认（S5b 的唯一手工防线 —— 断言未落地时每次执行前都要跑）**：

```sql
SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint
WHERE conrelid = to_regclass('public.reconcile_windows') AND contype IN ('p','u','c');
```
期望看到 `PRIMARY KEY (window_start)` 与含 `60 seconds` 的 CHECK。

### 5.2 宿主状态矩阵（S0–S5b）与处置

| 状态 | ① 记录层 | ② 对象层 | ③ 形状层 | ④ 反查（记录→对象） | 含义 | 处置 |
|---|---|---|---|---|---|---|
| **S0** | 0 行 | 0 行 | 空 / 正形状 | **0 行** | 干净（含全新库） | 直接执行 |
| **S1** | 命中 `0019_v2_facade.sql` | 有残留 | 旧形状（12 行） | **0 行**（旧分支文件名不匹配 ④ 的 JOIN） | 跑过旧 0019 | **不可直接继续** → R-1 或 R-2 |
| **S2** | 同上 + `0020_reconcile_blocked_status.sql` | 有残留 | 旧形状（12 行） | **0 行** | 跑过旧 0019+0020 | 同上 |
| **S3** | 0 行（记录被手工删过） | 有残留 | 旧形状（12 行） | 0 行（没记录就没得反查） | 记录与对象脱节 | **以对象层为准** → R-2 |
| **S4** | 0 行（本版五个文件名被排除） | 0 行 | 0 行 | **有输出（实测 9 行）** | 记录在、对象缺 | **R-2**（实测：清记录后重跑，三个对象全部建出） |
| **S5a** 缺列 | 0 行 | 0 行 | 列清单**缺列** | 0 行 | 记录在、表在但缺列（同名异内容） | **R-2**；③ 可判、v2 断言也能判 |
| **S5b** 约束不符 | 0 行 | 0 行 | **11 行**（十列齐 + 多余列如 `id`） | **0 行** | 记录在、十列齐但 PK 在 `id` / 复合唯一不含 `window_start` | **R-2**；**四条探测全净，只有 v2 断言能拦** |

> **S4 只有 ④ 能给信号**（①②③ 全 0 行）；**S5b 四条全净**（`0 / 0 / 11 / 0`，实测），不落 v2 断言就等于放行 —— 这种库第一次写窗口就报 `ON CONFLICT` 找不到匹配约束。
> **S5b 的人工防线**：§5.1 那条「键约束确认」查询（`pg_get_constraintdef`，期望 `PRIMARY KEY (window_start)` 与含 `60 seconds` 的 CHECK）—— 断言未落地时，**每次执行前都要跑它**。
> **不要徒手补 DDL**：手工建的对象与迁移文件长期脱节，下次真实迁移里 `IF NOT EXISTS` 又会静默跳过。
> 「重跑无效」只对**不清记录的 naive 重跑**成立；R-2 自带 `DELETE`，所以重跑是有效的（实测）。

**R-1（应急：只修窗口表；实测可用，但 schema 不干净）**

```bash
psql -d $DB -c "\copy (SELECT * FROM reconcile_windows) TO 'reconcile_windows_old.csv' CSV HEADER"
psql -d $DB -1 -v ON_ERROR_STOP=1 -c "DROP TABLE IF EXISTS reconcile_windows; DELETE FROM applied_migrations WHERE name = '0023_v2_reconcile.sql';"
# 再重跑迁移（只补 0023）
```
结果：窗口表可写；`magpie_keys` 与 4 个旧列仍在，与干净基线仍有差异。

**R-2（回干净基线；实测 schema 与干净基线 0 差异）**

```bash
# 前置：表存在才导数据（S4 库里这些表可能压根不存在，\copy 会失败）
psql -d $DB -tAc "SELECT to_regclass('public.magpie_keys'), to_regclass('public.reconcile_windows')"
# 存在哪个就导哪个，例如：
psql -d $DB -c "\copy (SELECT * FROM reconcile_windows) TO 'reconcile_windows_old.csv' CSV HEADER"

cat > r2.sql <<'SQL'
-- ⚠️ **不许 DROP `magpie_keys` / `external_models.magpie_model`**：
--    它们是 0019b 收回本链的**现役对象**（转发链与准入在用）。旧版 R-2 让删这两样、
--    同时又把 0019b 的记录漏在 DELETE 清单外，于是重跑时 CREATE ... IF NOT EXISTS 全部跳过、
--    对象永久消失，进程启动即 42P01/42703（已在真库上复现过这条坏库路径）。
DROP TABLE IF EXISTS reconcile_windows;  -- 旧形状或坏形状（纯派生数据，不迁行）
-- 账单三列（magpie_key_name / estimated / upstream_usage）属「阶段二按新形处理」：
-- 本链不建也不删，留着不影响；不许在这里顺手 DROP（阶段二会 ADD COLUMN IF NOT EXISTS 静默跳过）。
--
-- 故意的：只清记录，让本链整套重跑。已建对的走 IF NOT EXISTS 是 no-op；
-- 被 DROP / 从来没建的 reconcile_windows 会按新形状真正建出来（S4 修复的关键）。
-- ⚠️ 必须逐条列名，**不许** name >= '0019'（0024 落地后那条会把 0024 记录一起删掉）。
DELETE FROM applied_migrations WHERE name IN (
    '0019_v2_topology.sql','0019b_v2_passthrough_credentials.sql',
    '0020_v2_billing_attribution.sql','0022_v2_ledger.sql',
    '0023_v2_reconcile.sql','0023b_v2_reconcile_repair.sql',
    '0023c_v2_reconcile_counts_backfill.sql',
    '0019_v2_facade.sql','0020_reconcile_blocked_status.sql'   -- 旧分支记录，一并清
);
SQL
psql -d $DB -X -1 -v ON_ERROR_STOP=1 -f r2.sql
# 再重跑迁移，然后复验 §5.1 **四条**探测 + schema diff（1030 行 0 差异）
```

**R-2 覆盖 S4 的实测依据**：S4 库里执行同一份 `DELETE` 后重跑，`internal_models` / `ledger_entries` / `reconcile_windows` 三个对象全部建出。所以 §5.2 把 S4 的处置从「人工补 DDL」改成 R-2 —— 徒手补 DDL 只在「不想清记录」时才有诱惑，而它会造成手工 DDL 与迁移文件长期不一致。

### 5.3 三步证据（脚本）

先落两个计数 SQL 文件（动态精确计数：不写死表名，表多表少都对）：

```bash
mkdir -p ./backup
cat > counts-all.sql <<'SQL'
SELECT table_name,
       (xpath('/row/c/text()',
              query_to_xml(format('SELECT count(*) AS c FROM %I.%I', table_schema, table_name),
                           false, true, '')))[1]::text::bigint AS rows
FROM information_schema.tables
WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
ORDER BY table_name;
SQL
# 保留表清单：迁移前后都必须逐字相同
cat > counts-retained.sql <<'SQL'
SELECT table_name,
       (xpath('/row/c/text()',
              query_to_xml(format('SELECT count(*) AS c FROM %I.%I', table_schema, table_name),
                           false, true, '')))[1]::text::bigint AS rows
FROM information_schema.tables
WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
  AND table_name = ANY('{users,tokens,billing_records,credit_wallets,credit_flows,
                         sessions,call_logs,external_models,sys_configs,announcements,
                         audit_logs,channel_protocols,protocol_error_stats}')
ORDER BY table_name;
SQL
```

```bash
# ── 变量 ─────────────────────────────────────────
export PGHOST=127.0.0.1 PGPORT=5432
SRC_DB=lingshu_dev
STAMP=$(date +%Y%m%dT%H%M%S)
BK=./backup/${SRC_DB}-${STAMP}.dump
TMP_DB=lingshu_verify_${STAMP}

# ── 步骤 0：执行前探测（**四条**，缺一不可；见 §5.1）──────
#   只信 applied_migrations 会漏：它记的是「文件跑过」，不是「对象建对」。
psql -d $SRC_DB -tA -f probe-1-record.sql   # 期望 0 行（区间有右界，不受将来 0024 影响）
psql -d $SRC_DB -tA -f probe-2-object.sql   # 期望 0 行
psql -d $SRC_DB -tA -f probe-3-shape.sql    # 期望 0 行 或 十列齐全（缺 local_count 即被跳过）
psql -d $SRC_DB -tA -f probe-4-objects-by-record.sql   # 期望 0 行（非空 = S4 或同名异内容）
# 任一条不符 → 按 §5.2 矩阵判状态，S1–S4 一律走 R-1 或 R-2（S4 也走 R-2）；都不许直接执行。

# ── 步骤 1：备份（自定义格式，带 CRC）─────────────
pg_dump -Fc -Z6 -f "$BK" "$SRC_DB"; echo "dump_exit=$?"
ls -l "$BK"; sha256sum "$BK"

# ── 步骤 2：临时库还原 + 全表行数比对 ───────────────
createdb -T template0 "$TMP_DB"; echo "createdb_exit=$?"
pg_restore -d "$TMP_DB" --no-owner --no-privileges -j4 "$BK"; echo "restore_exit=$?"
psql -d $SRC_DB -tA -F'|' -f counts-all.sql | sort > before-all.txt
psql -d $TMP_DB -tA -F'|' -f counts-all.sql | sort > restored-all.txt
diff before-all.txt restored-all.txt && echo "RESTORE-OK"

# ── 步骤 3a：迁移前，先存保留表基线 ─────────────────
psql -d $SRC_DB -tA -F'|' -f counts-retained.sql | sort > before-retained.txt

# ── 步骤 3b：执行迁移（服务启动或 migrate 命令触发）────
#   ……此处执行 migrate……

# ── 步骤 3c：迁移后比保留表（必须逐字相同）───────────
psql -d $SRC_DB -tA -F'|' -f counts-retained.sql | sort > after-retained.txt
diff before-retained.txt after-retained.txt && echo "RETAINED-OK"

# ── 步骤 3d：删干净断言（期望空输出）────────────────
#   10 张实际删除 + channel_events（0009 已删，留作第 11 条只为清单完整；断言同样要求 NULL）
psql -d $SRC_DB -tAc "
SELECT string_agg(t, ',') FROM unnest(ARRAY[
 'channels','channel_keys','channel_models','channel_events','probe_logs',
 'semantic_tags','channel_tags','channel_model_events','channel_key_events',
 'auto_models','auto_model_members']) t
WHERE to_regclass(t) IS NOT NULL"
```

**要留的产物**：`*.dump`（文件名 + 大小 + sha256）、`before-all.txt` / `restored-all.txt` / `before-retained.txt` / `after-retained.txt`、三条命令退出码、3d 的空输出；再加**步骤 0 四条探测**的输出（证明这个库既没被旧分支碰过、对象也确实建对了）+ 判定出的状态码（S0–S5b）。全部贴进 PR 描述。

### 5.4 判定点（任一命中即中止）

| 点 | 条件 | 动作 |
|---|---|---|
| A | `dump_exit≠0`，或 dump 文件 < 1 KiB（空库除外） | **不执行迁移**，重跑备份 |
| B | `createdb_exit≠0` 或 `restore_exit≠0`，或 `diff before-all.txt restored-all.txt` 非空 | **不执行迁移**（备份不可信 = 没备份） |
| C | `RETAINED-OK` 不成立（保留表行数变少） | **立即中止**；若已 commit，用 §4.4 的整库还原 |
| D | 3d 有输出（表还在） | 迁移未生效或部分执行 → 查 `applied_migrations` 后重跑 |
| E | **负向对照**：在 0021 末尾注入 `SELECT 1/0` 后执行 | 整份事务回滚、表与列原样（已实测）→ 证明「跑一半能自动回滚」不是推断 |

### 5.5 执行方式与比对口径（三条实测坑，写进手册别只写「用 psql 执行」）

| 坑 | 事实 | 正确做法 |
|---|---|---|
| `psql -f` 静默成功 | 不加 `ON_ERROR_STOP` 时语句出错仍 **exit 0**，出错前的语句已各自提交 → 半套状态 | 必须带 `-v ON_ERROR_STOP=1` |
| 只加 `ON_ERROR_STOP` 仍半套 | 出错 **exit 3**，但出错前语句已落库（自动提交）→ 仍是半套 | 必须同时带 `-1`（单事务） |
| `-1` + `ON_ERROR_STOP=1` | 与 `migrate.go` 的「每文件一个事务」等价；实测逐文件 `psql -1` 路径 910 ms | `psql -X -1 -v ON_ERROR_STOP=1 -f 0019_v2_topology.sql`（五个文件逐个跑） |

- **生产走真实执行器**：`server -migrate-only`（24 个迁移 289 ms，实测）。两条路径的 `pg_dump -s` 均 1030 行、**0 差异**，所以演练用 psql 路径是可信的。
- **schema diff 前必须先过滤 `\restrict`**：`pg_dump -s` 在 17.11 会写随机 `\restrict` / `\unrestrict` 行，直接 `diff` 必误报。
  `pg_dump -s db | grep -vE '^\\(un)?restrict' | sort > schema.txt`，两边同样过滤后再比。

---

## 6 账本：最小九字段 + offset（0022_v2_ledger.sql）

### 6.1 `ledger_entries` —— 9 列，一列不多

| # | 列 | 类型 | 可空 | 默认 | 说明 |
|---|---|---|---|---|---|
| 1 | `id` | `BIGINT` | NOT NULL | — | 本地主键（雪花） |
| 2 | `ledger_id` | `TEXT` | NOT NULL | — | **幂等键**：上游账本条目 id（唯一） |
| 3 | `provider` | `TEXT` | NOT NULL | `''` | 上游 provider |
| 4 | `provider_account` | `TEXT` | NOT NULL | `''` | 上游账号（`providerAccount`） |
| 5 | `model` | `TEXT` | NOT NULL | `''` | 上游实际服务模型 |
| 6 | `tokens_in` | `BIGINT` | NOT NULL | `0` | 输入 token（账本原值，**不做归一化**） |
| 7 | `tokens_out` | `BIGINT` | NOT NULL | `0` | 输出 token |
| 8 | `cost_credits` | `NUMERIC(20,5)` | NOT NULL | `0` | 上游实付成本，单位=积分（与 `billing_records.cost_credits` 同型） |
| 9 | `occurred_at` | `TIMESTAMPTZ` | NOT NULL | — | 账本记账时间（网关时钟，同机部署；漂移约束见 §12-②） |

```sql
CREATE UNIQUE INDEX IF NOT EXISTS uq_ledger_entries_ledger_id ON ledger_entries(ledger_id);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_time ON ledger_entries(occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_account
    ON ledger_entries(provider, provider_account, occurred_at DESC);
```

**POC 后再补的字段（本期一律不加）**：`request_id`、`status/failed`、`retries`、`cache_read_tokens`、`thinking_tokens`、`unit_price`、`currency`。加了就必须先证实字段名，否则一半字段恒为 NULL，比没有更误导。

### 6.2 offset 的语义（关键：offset 不是游标）

- 每 60 秒一轮：拉取区间 `[last_end - 120s, now)`——**重叠 2 个窗口**吸收迟到条目，重复靠 `ledger_id` 唯一键吸收。
- 区间内按 `offset=0, N, 2N…` 翻页（每页 500），直到返回条数 `< N`。**offset 不持久化、不进库**。
- 单轮页数上限 20 页；超限 → 本轮 `reconcile_windows.status='blocked'` + 告警，下轮从新区间重来（重叠窗口已覆盖，不会漏）。
- 写入时机：整轮一个事务，`INSERT … ON CONFLICT (ledger_id) DO NOTHING` 批量落库；随后同事务写窗口行。

> 为什么不用「上次 offset / 上次时间戳」做断点：上游在区间内插入或删除条目会让 offset 跳行（这是我在 0020 统计迁移上踩过的同类坑——**用取整时间做游标会把整天砍成 8 小时**）。重叠窗口 + 唯一键比任何游标都稳。

### 6.3 `credit_holds`（阶段一只建表，阶段二启用）

| 列 | 类型 | 可空 | 默认 | 说明 |
|---|---|---|---|---|
| `id` | `BIGINT` | NOT NULL | — | 主键 |
| `billing_id` | `TEXT` | NOT NULL | — | 一请求一预扣（`UNIQUE`） |
| `user_id` | `BIGINT` | NOT NULL | — | `REFERENCES users(id)` |
| `token_id` | `BIGINT` | NULL | — | `REFERENCES tokens(id)` |
| `amount` | `NUMERIC(20,5)` | NOT NULL | `0` | 预扣积分 |
| `status` | `TEXT` | NOT NULL | `'held'` | `CHECK IN ('held','settled','refunded','expired')` |
| `ledger_entry_id` | `TEXT` | NULL | — | 结算匹配到的账本条目 |
| `expires_at` | `TIMESTAMPTZ` | NOT NULL | — | 创建时 `now()+15min` |
| `closed_at` | `TIMESTAMPTZ` | NULL | — | 终结时间 |
| `created_at` | `TIMESTAMPTZ` | NOT NULL | `now()` | — |

索引：`(status, expires_at) WHERE status='held'`（过期扫描器唯一入口）。

---

## 7 0023_v2_reconcile.sql

| 列 | 类型 | 可空 | 默认 | 说明 |
|---|---|---|---|---|
| `window_start` | `TIMESTAMPTZ` | NOT NULL | — | **主键**（供 `ON CONFLICT (window_start)`） |
| `window_end` | `TIMESTAMPTZ` | NOT NULL | — | 必须 = 起点 + 60s（断言，不靠文档） |
| `local_count` / `upstream_count` | `BIGINT` | NOT NULL | `0` | 灵枢账单笔数 / 账本笔数 |
| `local_tokens` / `upstream_tokens` | `BIGINT` | NOT NULL | `0` | **总 token 合计**（灵枢 input+cache_read+output；账本 in+out） |
| `status` | `TEXT` | NOT NULL | `'ok'` | `CHECK IN ('ok','alarm','error','blocked')` |
| `alarm_streak` | `SMALLINT` | NOT NULL | `0` | 连续告警窗数；≥3 升级 |
| `detail` | `JSONB` | NOT NULL | `'{}'` | 分组明细（key×model 偏差），不参与判定 |
| `checked_at` | `TIMESTAMPTZ` | NOT NULL | `now()` | — |

**只比总 token 与笔数，不比金额**：账本币种/牌价与灵枢积分不等价，比金额必然误报。笔数不一致进 `detail`，不单独告警。

---

## 8 归因字段口径

| 归因维度 | 字段 | 落点 | 取值 / 兜底 |
|---|---|---|---|
| 请求 | `billing_records.request_id` | 0020 | 入站生成；`billing_id` 仍为对外主键 |
| 渠道（供应商） | `billing_records.upstream_provider` | 0020 | 回程头（剥前读取）→ 账本 `provider` 校对；缺失 `NULL` |
| 账号 | `billing_records.upstream_account` | 0020 | 回程头或账本 `provider_account`；缺失 `NULL`（**不猜**） |
| 协议 | `billing_records.upstream_protocol` | 0020 | `openai-chat` / `openai-responses` / `anthropic-messages` / `gemini` |
| 内部模型（组/直通） | `billing_records.internal_model_id`（已有） | 0001 | 组归因 = 该 ref 在 `internal_models.kind='group'`；**不另立 `group_ref`** |
| 对外模型 | `external_model_name`（请求名）+ `upstream_model`（实际服务） | 已有 + 0020 | 两个名字分开留，改价/改绑定后历史可解释 |
| 令牌 / 用户 / 会话 | `token_id` / `user_id` / `session_id`（已有） | 0001 | — |
| 账本 | `ledger_entries.ledger_id` ↔ `billing_records.ledger_entry_id` | 0022 / 0020 | 部分唯一索引保证一对一 |
| 成本来源 | `billing_records.cost_source` | 0020 | `ledger` / `amortized` / `none` |

命名铁律：表、列、配置键、日志字段、界面文案**统一用 `upstream`**，不出现第三方产品名（§10-F 是唯一例外）。

---

## 9 四协议透传落点

### 9.1 结论（一段话）

透传落在**入站 handler 与出站 client 之间的适配层**：`server.go` 的 mux 早已把整个 `/v1/` 挂给 `gateway.Gateway`，所以**入站不需要新层**；改动只发生在 `gateway.gateway.go` 的路由表和 `gateway.handler.go` 的 `serve()` 中段——前半段（令牌鉴权、请求体读取、model 解析、预扣、请求快照）原样保留，**「路由选渠道 + Provider 组包 + 协议互译」整段删除**，替换为 `upstream.Client.Forward(ctx, path, header, body)`：请求体逐字节转发（只改 Gemini path 里的 model 段），回程 `io.Copy` 流式回写、边扫 usage 边剥上游标识头。**这不是旁路**：预扣/账单/归因仍在主链上；也不是配置层：配置只提供基址/超时/降级开关。网关侧零改动，因为它本来就原生暴露这四套协议，灵枢只是把「同名路径 + 同字节」递过去。

### 9.2 涉及的文件位置（实测）

| 位置 | 现状 | V2 动作 |
|---|---|---|
| `backend/internal/server/server.go:44`（`Gateway http.Handler`）+ `routes()` | 已把 `/v1/` 整体挂载 | 不改 |
| `backend/internal/domain/gateway/gateway.go:313 Handler()` | 注册 6 条：models + chat/completions + **completions** + **embeddings** + responses + messages | 保留 `GET /v1/models` + 4 条；**下线 completions / embeddings**（不注册即 404） |
| Gemini 路由 | **不存在** | 新增 `POST /v1beta/models/` 前缀 + 手写解析 `models/{model}:{generateContent\|streamGenerateContent}`。**不能用 `{model}` 通配**：Go 1.27 实测 `mux.HandleFunc("POST /v1beta/models/{model}:generateContent", …)` 直接 panic —— `at offset 20: bad wildcard segment (must end with '}')` |
| `backend/internal/domain/gateway/handler.go:220 serve()` | 鉴权→读体→解析 model→标签→路由→预扣→上游→计费→落库 | 保留鉴权/读体/解析/预扣/计费；中段换成 `upstream.Client`；删 tagKV、会话成本路由、failover 回喂 |
| 新建 `backend/internal/domain/upstream/` | — | `client.go`（转发/剥头/usage 扫描）、`ledger.go`（60s 拉取 + offset 翻页）、`models.go`（模型/组/供应商/配额查询）、`protocol.go`（四协议常量，**不往 `channelprotocol` 加**）、`config.go`（读 sys_configs） |
| 新建 `backend/internal/pkg/backoff/` | — | 5/20/60s、≤3 次，唯一实现 |

### 9.3 数据如何流经（网关不改）

```
客户端 → 灵枢 /v1/...（四协议同名路径）
  → 令牌鉴权（现有）→ model 解析（现有）→ 预扣（现有）
  → upstream.Client.Forward：同名路径、query 原样、header 剔除黑名单后原样、body 逐字节
      （Gemini 仅改 path 段 models/{对外名} → models/{上游名}）
  → 网关 127.0.0.1:3425 做协议互译/渠道/密钥/failover
  → 回程：先读上游标识头（归因）→ 从响应头剔除全部上游标识头 → 边流式回写边扫 usage
  → 账单落库（含 upstream_* 归因、ledger_entry_id 待账本回填）
  → 客户端（永远看不到上游标识头）

归因与 usage 的定稿/落库时序见 §12-③：**流式响应的 usage 在最后一段才出现，但归因字段不等 usage**。
```

### 9.4 sys_configs 新增键（只放非敏感项）

| 键 | 形态 | 说明 |
|---|---|---|
| `upstream.base_url` | `"http://127.0.0.1:3425"` | 网关基址 |
| `upstream.timeouts` | `{"connect_ms":10000,"request_ms":600000,"stream_first_byte_ms":30000,"stream_idle_ms":120000}` | 超时 |
| `upstream.degrade` | `{"passthrough_enabled":true,"ledger_enabled":true,"sync_enabled":true}` | 降级开关（关 = 面板只读空态 + 告警，不把错误抛给前端） |

**严禁**任何凭据/密钥进这三块（`sys_configs` 是管理员明文可读的配置页）。写入沿用现有「只写出现的键」语义（`console/admin.go:560-884`），老客户端整块提交不会覆盖新块。

---

## 10 与已批准计划的差异（逐条理由）

| # | 冲突点 | 我的裁定 | 为什么 |
|---|---|---|---|
| A | 「旧分支关掉」 | **已关掉（2026-11-16）**：`feat/lingshu-v2-p1-usage-billing` / `-magpie-passthrough` / `docs/lingshu-v2-p1-data-contract` 等含 `0019_v2_facade.sql` 的分支已删除（本分支的迁移副本一并作废），PR #82 / #83 / #85 均已关闭，内容并入 #84 | 旧 0019/0020 永久作废；`0019b` 只保留仍被代码使用的对象 |
| B | 计划写「0020 摘 channel 维度」 | 摘列移到 0021，0020 纯 ADD | 不可回滚动作只应出现在一个文件，备份/证据流程只走一次 |
| C | 计划写「`pricing_mode` 扩 sale·subscription·usage」 | **不扩**，用户侧仍 `sale`/`cost`；成本来源另立 `cost_source` | `pricing_mode` 在现有代码里是**用户售价**口径（`users.pricing_mode` 同义）；把上游成本口径塞进去，盈亏页与账单页会读出两种含义 |
| D | 旧契约金额用「BIGINT 微元」 | 账本金额用 `NUMERIC(20,5)` 积分 | 与 `billing_records.cost_credits` / `credit_wallets.balance` 同型同精度，报表可直接加减；不引第二套金额单位（旧契约已作废） |
| E | 「一令牌一把网关凭据」 | **0019–0023 没有它的落点**，属计划缺口 | 本期用服务端单把 key（环境变量注入，**不进 sys_configs**）；一令牌一把需要新表，建议放 0024 或阶段二，**需负责人拍板** |
| F | 红线「配置与代码里不出现产品名」 | 唯一例外：**剥除回程头的匹配前缀**必须按上游真实头名写常量 | 不匹配就剥不掉，等于泄漏上游身份；该常量只在剥头白名单与注释里，界面/配置/日志回显仍不出现。**请负责人确认这条例外** |
| G | 任务书写 `external_models.bound_internal_model_ref` 且同时建 `model_links` | 绑定真源唯一的放在 `external_models`，`model_links` 只存画布坐标 | 同一关系两处可写，必然漂移（谁后写谁生效），拓扑与售卖会对不上 |
| H | 「四协议透传」当成现成能力 | 现网**没有 Gemini 路由**，且注册了 embeddings/completions | 必须新增 Gemini 前缀路由 + 下线两条端点；这是阶段一唯一的真新增路由 |
| I | 「迁移前 pg_dump 并校验」 | 沙盒 PG 拒连、`lingshu_dev` 无 `CREATEDB` | 证据流程必须在宿主由有权限的角色执行（临时库谁建需确认），沙盒里只能审 SQL 不能跑 |

本轮（交付轮）**未新增冲突**：A / E / F 三条仍待负责人拍板；Core 的 `feat/lingshu-v2-sync-config` 已把基址/超时/降级放进 `sys_configs`、密钥一律不进 —— 与 E 的处置一致，口径无偏离。

按测试实测**修订五条**（原表述作废，以本节后文 §4/§5 为准）：

- **退役表是 10 张，不是 11 张**：`channel_events` 在 `0009_drop_channel_state.sql` 已被删除，0021 里那条是空操作。
- **撞号的真实风险是「文件名幂等键」，不是 DDL 冲突**：同名异内容会被静默跳过（缺列不报错），同编号异名会两条都执行。执行前必须核对 `applied_migrations`（§5.3 步骤 0）。
- **同编号异名在真实旧库上是「静默损坏」，不是报错**：实测五个文件逐条 exit 0、记录记到 0023，而 `reconcile_windows` 被 `CREATE TABLE IF NOT EXISTS` 整段跳过（缺列/PK/CHECK），直到首次写窗口才炸。⇒ 执行前探测改成**记录层 + 对象层 + 形状层 + 反查四条**（§5.1），宿主状态按 **S0–S5b** 处置（§5.2）；手工执行必须 `psql -1 -v ON_ERROR_STOP=1`（§5.5）。迁移文件本身不改（`IF NOT EXISTS` 语义如此），防线在探测。
- **S4（记录在、对象缺）用前三条判不出来，且原处置是错的**：这种库里 ①②③ **全是 0 行**，会被判成 S0，执行后五个文件逐条 exit 0、零报错，而 `internal_models`/`ledger_entries`/`reconcile_windows` 一个都没建。修正：加**第 ④ 条探测「按已记文件名反查其对象」**（§5.1，对「同名异内容」同样有效）；S4 处置改为 **R-2** —— 实测同一库 `DELETE` 记录后重跑，三个对象全部建出，「重跑无效」只对不清记录的 naive 重跑成立。手册里的「徒手补 DDL」已**删除**（会造成手工 DDL 与迁移文件长期不一致）。
- **区间必须带右界、唯一来源只能一份**：探测① 与 R-2 的 `>= '0019'` 在 0024 落地后会误报/误删记录 → 改成 `name < '0024'` 与**逐条列名**；0024 已定稿（§16）。另：迁移与本文的**唯一来源是本分支**，`feat/lingshu-v2-sync-config` 里那份同名副本是旧版（探测还是裸条件），合并时整份丢弃（见 §0 声明）。

---

## 11 阶段一执行顺序（数据层）

1. `git fetch origin` → 从 `origin/main` 开分支 → **先跑 §1 撞号断言**。
2. 0019 → 0020 → 0022 → 0023 逐个应用，空库与存量库各一遍，二次执行新增 0 条。
3. 0021 **最后**，执行前完成 §5 三步证据并留产物。
4. 自测：账本 2 万条乱序灌入（含 30% 重复 `ledger_id`）→ 落库条数 = 去重后条数；重跑同一窗口 → 新增 0；窗口断言 `window_end = window_start + 60s`。

---

## 12 三条硬结论（一句话各一条，Gate 可直接写成断言）

**① 0021 中途失败能自动回滚到迁移前，但只在文件的事务内（已实测，非推断）。**
`migrate.go:16-70` 把整份文件放进一个 `tx` 执行，PG 的 DDL 是事务性的（`DROP TABLE` / `DROP COLUMN` 可回滚），任一句报错 → `tx.Rollback()` → 库回到 0018，且 `applied_migrations` 不记 0021、重试安全；**dump 保护的不是「跑一半」，而是「文件已 commit 后发现删错/要取回历史渠道密钥名」这类事务之外的误判**——所以三步证据仍是必须的；反过来说，这五个文件里**不得出现** `CREATE INDEX CONCURRENTLY` / `VACUUM` / `CREATE DATABASE` 等不能在事务内执行的语句，否则迁移会直接失败（那才是真回滚不了）。实证：在 0021 末尾注入 `SELECT 1/0`，整份事务回滚、表与列原样——「跑一半」由事务兜住；同一份 dump 整库还原后行数 0 差异——「已 commit 后删错」由 dump 兜住。两者不是一回事，别拿其中一个当另一个。

**② 窗口归属只按网关 `occurred_at` 切，两端时钟偏差必须 < 半窗（30s）。**
账本条目一律落它自己的 `occurred_at`，不按灵枢落库时刻补正；一旦换机或漂移超过 30s，对账窗口映射就会把条目归到相邻窗，因此约束是「同机部署 + NTP 同源，偏差 ≥ 30s 时对账置 `status='error'` 并告警」——**宁可整窗报错，不许静默错窗**。

**③ 流式场景：归因在首字节定稿、usage 在流结束定稿，两者同一次写入落库。**
回程头一到就取 `upstream_provider` / `upstream_account` / `upstream_model` / `upstream_protocol` 并与 `request_id` 一起存进内存结构（流式中途不改写）；`usage` 取到流末尾（或客户端断开/超时）才定稿；账单与归因在**同一事务**写入，所以库里不存在「有归因、没 usage」或「归因是半截」的中间态。

## 13 给 Core 的两句口径确认（不扩范围）

1. **重试次数**：按「首次调用 + 最多 3 次重试」= **最多 4 次调用**，退避 5s/20s/60s（与计划书「退避序列恰为 5/20/60s 且 ≤3 次重试」一致）。若产品口径要的是「总调用 ≤3 次」，改一个常量并同步 §11 自测断言即可，迁移与表结构不受影响。
2. **降级**：网关不可达/未配置时同步器只跳过并告警，**沿用上期快照、不清空**——这与 `internal_models.stale` 的语义一致（`stale=true` 是「本轮未见」，不是「已删除」）；只有网关明确回了删除才允许硬删本地行。

## 14 附录：五个迁移文件的权威 DDL（实体文件已落 `backend/internal/db/migrations/`，与本文逐字一致）

> 注意：PG 不支持 `ADD CONSTRAINT IF NOT EXISTS`，所以 0020 的枚举约束用 `DO $$ … pg_constraint …$$` 包一层保证幂等；
> 其余全部 `IF NOT EXISTS` / `ADD COLUMN IF NOT EXISTS`。
> 本附录与仓库里的五个实体文件一致；§15 的「形状断言」尚未落地，未批准前不要手工加进来。

### 0019_v2_topology.sql

```sql
-- 内部模型镜像（网关同步的只读节点）：组与直通同层，kind 区分；auto 组在同步器侧过滤，不入库。
CREATE TABLE IF NOT EXISTS internal_models (
    ref          TEXT PRIMARY KEY,
    provider_ref TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL DEFAULT 'direct' CHECK (kind IN ('group','direct')),
    display_name TEXT NOT NULL DEFAULT '',
    context_len  BIGINT NOT NULL DEFAULT 0,
    caps         JSONB NOT NULL DEFAULT '{}'::jsonb,
    list_price   JSONB NOT NULL DEFAULT '{}'::jsonb,
    pos_x        DOUBLE PRECISION NOT NULL DEFAULT 0,
    pos_y        DOUBLE PRECISION NOT NULL DEFAULT 0,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    stale        BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_internal_models_provider ON internal_models(provider_ref);
CREATE INDEX IF NOT EXISTS idx_internal_models_kind     ON internal_models(kind);
CREATE INDEX IF NOT EXISTS idx_internal_models_live     ON internal_models(last_seen_at) WHERE stale = false;

-- 绑定真源（唯一）：NULL = 未绑定 = 不售卖。
ALTER TABLE external_models ADD COLUMN IF NOT EXISTS bound_internal_ref TEXT;
CREATE INDEX IF NOT EXISTS idx_external_models_bound
    ON external_models(bound_internal_ref) WHERE bound_internal_ref IS NOT NULL;

-- 画布落位（不含绑定语义；缺行 = 自动布局）。
CREATE TABLE IF NOT EXISTS model_links (
    internal_ref      TEXT   NOT NULL,
    external_model_id BIGINT NOT NULL REFERENCES external_models(id) ON DELETE CASCADE,
    pos_x             DOUBLE PRECISION NOT NULL DEFAULT 0,
    pos_y             DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (internal_ref, external_model_id)
);

-- 账号财务扩展（与网关账号一对一，靠唯一键表达）。
CREATE TABLE IF NOT EXISTS subscriptions (
    id             BIGINT PRIMARY KEY,
    provider_ref   TEXT NOT NULL,
    account_ref    TEXT NOT NULL,
    price_amount   NUMERIC(20,5) NOT NULL DEFAULT 0,
    price_currency TEXT NOT NULL DEFAULT 'CNY' CHECK (price_currency IN ('CNY','USD')),
    billing_cycle  TEXT NOT NULL DEFAULT 'month' CHECK (billing_cycle IN ('month','year','once')),
    pay_day        SMALLINT NOT NULL DEFAULT 1 CHECK (pay_day BETWEEN 1 AND 28),
    share_cost     BOOLEAN NOT NULL DEFAULT true,
    cycle_start    DATE NULL,
    note           TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_subscriptions_account UNIQUE (provider_ref, account_ref)
);
```

### 0020_v2_billing_attribution.sql

```sql
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS request_id        TEXT;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS upstream_provider TEXT;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS upstream_account  TEXT;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS upstream_model    TEXT;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS upstream_protocol TEXT;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS ledger_entry_id   TEXT;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS cost_source       TEXT;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_billing_upstream_protocol') THEN
    ALTER TABLE billing_records ADD CONSTRAINT ck_billing_upstream_protocol
      CHECK (upstream_protocol IS NULL OR upstream_protocol IN
             ('openai-chat','openai-responses','anthropic-messages','gemini'));
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_billing_cost_source') THEN
    ALTER TABLE billing_records ADD CONSTRAINT ck_billing_cost_source
      CHECK (cost_source IS NULL OR cost_source IN ('ledger','amortized','none'));
  END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_billing_ledger_entry
    ON billing_records(ledger_entry_id) WHERE ledger_entry_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_billing_upstream_time
    ON billing_records(upstream_provider, upstream_account, call_time DESC)
    WHERE upstream_provider IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_billing_request
    ON billing_records(request_id) WHERE request_id IS NOT NULL;
```

### ~~0021~~ → 0024_v2_drop_legacy.sql

```sql
-- ① 先删阻塞列（顺序不可调）
ALTER TABLE billing_records DROP COLUMN IF EXISTS channel_key_id;
ALTER TABLE sessions        DROP COLUMN IF EXISTS channel_key_id;
ALTER TABLE tokens          DROP COLUMN IF EXISTS tag_id;

-- ② 再删表（子表在前）
DROP TABLE IF EXISTS channel_tags;
DROP TABLE IF EXISTS channel_model_events;
DROP TABLE IF EXISTS channel_key_events;
DROP TABLE IF EXISTS probe_logs;
DROP TABLE IF EXISTS channel_models;
DROP TABLE IF EXISTS channel_keys;
DROP TABLE IF EXISTS channel_events;   -- 0009 已删 → 空操作（保留只为清单完整）
DROP TABLE IF EXISTS channels;
DROP TABLE IF EXISTS auto_model_members;
DROP TABLE IF EXISTS auto_models;
DROP TABLE IF EXISTS semantic_tags;
```

### 0022_v2_ledger.sql

```sql
-- 账本最小九字段 + 幂等键。POC 后再补扩展字段（request_id/status/缓存读/思考 token/单价/币种）。
CREATE TABLE IF NOT EXISTS ledger_entries (
    id               BIGINT PRIMARY KEY,
    ledger_id        TEXT NOT NULL,
    provider         TEXT NOT NULL DEFAULT '',
    provider_account TEXT NOT NULL DEFAULT '',
    model            TEXT NOT NULL DEFAULT '',
    tokens_in        BIGINT NOT NULL DEFAULT 0,
    tokens_out       BIGINT NOT NULL DEFAULT 0,
    cost_credits     NUMERIC(20,5) NOT NULL DEFAULT 0,
    occurred_at      TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_ledger_entries_ledger_id ON ledger_entries(ledger_id);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_time    ON ledger_entries(occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_account ON ledger_entries(provider, provider_account, occurred_at DESC);

-- 预扣登记：阶段一只建表，阶段二启用（15 分钟未匹配 → expired + 全额退回）。
CREATE TABLE IF NOT EXISTS credit_holds (
    id              BIGINT PRIMARY KEY,
    billing_id      TEXT NOT NULL,
    user_id         BIGINT NOT NULL REFERENCES users(id),
    token_id        BIGINT NULL REFERENCES tokens(id),
    amount          NUMERIC(20,5) NOT NULL DEFAULT 0,
    status          TEXT NOT NULL DEFAULT 'held' CHECK (status IN ('held','settled','refunded','expired')),
    ledger_entry_id TEXT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    closed_at       TIMESTAMPTZ NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_credit_holds_billing UNIQUE (billing_id)
);
CREATE INDEX IF NOT EXISTS idx_credit_holds_expiry ON credit_holds(status, expires_at) WHERE status = 'held';
```

### 0023_v2_reconcile.sql

```sql
CREATE TABLE IF NOT EXISTS reconcile_windows (
    window_start    TIMESTAMPTZ PRIMARY KEY,
    window_end      TIMESTAMPTZ NOT NULL,
    local_count     BIGINT NOT NULL DEFAULT 0,
    upstream_count  BIGINT NOT NULL DEFAULT 0,
    local_tokens    BIGINT NOT NULL DEFAULT 0,
    upstream_tokens BIGINT NOT NULL DEFAULT 0,
    status          TEXT NOT NULL DEFAULT 'ok' CHECK (status IN ('ok','alarm','error','blocked')),
    alarm_streak    SMALLINT NOT NULL DEFAULT 0,
    detail          JSONB NOT NULL DEFAULT '{}'::jsonb,
    checked_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 窗口必须是整一分钟：错位窗口不许写同一行起点，靠约束而非文档保证
    CONSTRAINT ck_reconcile_window_len CHECK (window_end = window_start + interval '60 seconds')
);
CREATE INDEX IF NOT EXISTS idx_reconcile_status ON reconcile_windows(status, window_start DESC);
```

---

## 15 待负责人确认：0023 是否加「形状断言」（评估 + DDL 草案）

**结论：建议加；只加断言，不动任何 `CREATE` 语义。**

理由（三条，都对着实测）：

1. 被跳过的失败点落在「首次写窗口」，可能离迁移几小时到几天，报错还是 `column "local_count" does not exist` 这种与病因无关的文案；断言把它提前到迁移当场，并直接点出处置入口。
2. 断言只读元数据（`to_regclass` + `information_schema.columns` + `pg_constraint`），**不依赖 `applied_migrations` 是否存在** → `TestMigrate_FullChain`（直接按文件名执行 SQL、可能不建追踪表）路径安全：全新库里表不存在 → 直接 `RETURN` → 正常建表。
3. 幂等重跑不误伤：在册文件不会重跑；即便手工重跑，判定用「**必要列是超集**」而非「列集合完全相等」，将来 0024 给窗口表加列也不会打红。

两个刻意的不做：

- **不校验 60s CHECK**：`pg_constraint` 只能靠 `pg_get_constraintdef` 文本匹配，换个写法就误报；「十列齐全 + `window_start` 单列唯一约束」已足以识别旧形状（旧表主键是 `id`）。
- **不探测 `magpie_keys` 等旧对象**：那属于「schema 不干净」而非功能故障；把旧对象名写进迁移代码不值得，探测留在执行手册 §5.1 ②。

**结论强化（实测后）**：**形状断言从此不是可选项。** S5b（十列齐、约束不符，实测 ④ 也是 0 行）是四条探测的**覆盖空洞** —— 探测全净、断言缺失，等于允许这种库直接执行，故障要等到首次写窗口才爆（`ON CONFLICT` 找不到匹配约束）。这条比原先的「失败点太远」论证更硬：不落断言不是省事，是留了一个已知的洞。

残余风险（说清）：只能拦「0023 执行这一刻表已存在但形状不对」（S1/S2/S3/S5a/S5b）；
**S4（记录在、对象缺）它拦不住**——表不存在时断言按设计直接放行（否则全新库装不了）。
S4 的防线是 §5.1 **第 ④ 条探测**（按已记文件名反查对象），不是这条断言。

**DDL 草案 v2**（插在 0023 的 `CREATE TABLE IF NOT EXISTS reconcile_windows` **之前**）。

v1 草案有一个实测误报：列检查用 `current_schema()`、PK 检查用 `'reconcile_windows'::regclass`，**两套 schema 解析口径**。
当 `search_path=app,public` 而表只在 `public` 时，`current_schema()` 返回 `app` → 列查询 0 行 → 误报「缺十列」。
v2 改成：**开头只解析一次 `v_oid regclass`，后面列检查与约束检查全部挂在 `v_oid` 上**，不再出现任何 schema 字符串。

```sql
DO $$
DECLARE
    v_oid   regclass := to_regclass('reconcile_windows');  -- 唯一一次 schema 解析（跟着 search_path 走）
    missing text;
BEGIN
    -- 表不存在 = 干净库（含全新安装 / 迁移测试），直接放行
    IF v_oid IS NULL THEN
        RETURN;
    END IF;

    -- 必要列（超集判定：多余列不算错，缺列才算）；用 pg_attribute 而不是 information_schema
    SELECT string_agg(t.col, ', ' ORDER BY t.col) INTO missing
    FROM unnest(ARRAY['window_start','window_end','local_count','upstream_count',
                      'local_tokens','upstream_tokens','status','alarm_streak',
                      'detail','checked_at']) AS t(col)
    WHERE NOT EXISTS (
        SELECT 1 FROM pg_attribute a
        WHERE a.attrelid = v_oid
          AND a.attname  = t.col
          AND a.attnum > 0
          AND NOT a.attisdropped
    );
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION '0023 形状校验失败：reconcile_windows 已存在但缺列 [%]。'
            '这是「同名旧表 + CREATE TABLE IF NOT EXISTS 整段跳过」造成的，不是本次迁移写错；'
            '停手，按设计文档 §5.2 的 R-2 处置后再执行。', missing;
    END IF;

    -- ON CONFLICT (window_start) 的前置条件：window_start 上有单列主键/唯一约束
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint p
        WHERE p.conrelid = v_oid
          AND p.contype IN ('p','u')
          AND array_length(p.conkey, 1) = 1
          AND p.conkey[1] = (SELECT a.attnum FROM pg_attribute a
                             WHERE a.attrelid = v_oid
                               AND a.attname  = 'window_start'
                               AND NOT a.attisdropped)
    ) THEN
        RAISE EXCEPTION '0023 形状校验失败：reconcile_windows 的 window_start 上没有主键/唯一约束，'
            '首次写入会报 no unique or exclusion constraint matching the ON CONFLICT specification。'
            '按设计文档 §5.2 的 R-2 处置后再执行。';
    END IF;
END $$;
```

已实测的五种场景（v2 口径）：**全新库放行 / 正形状放行 / 缺列报错 / PK 在 `id` 上报错 / 复合唯一不含 `window_start` 报错**；
`search_path=app,public` + 表只在 `public` 的场景不再误报。

若负责人点头，落地要求：

- 只动 `0023_v2_reconcile.sql`（文件头说明 + 这段 `DO` 块），**`CREATE` / `INDEX` 语句逐字不动** → 不算语义变更。
- 必须补两条用例：① 先按旧形状建表（缺 `local_count`、PK 在 `id`）→ 执行 0023 必须报错且消息含「形状校验失败」；② 全新库执行 0023 → 正常建表（`TestMigrate_FullChain` 已覆盖这条路径）。
- 报错文案不出现第三方产品名：只说「同名旧表」，具体是谁由执行手册 §5.1 ② 定位。
- **断言不能替代 §5.1 第 ④ 条探测**：S4（记录在、对象缺）时表不存在，断言按设计放行。两者互补，缺一不可。

---

## 16 令牌模型范围与会话归属（0024 预告，全文见另一文档）

被 0021 删掉的两列决定路由行为，不能只删；替代模型已单独定稿：
`docs/designs/2026-11-15-v2-token-scope-session-owner.md`。结论三行：

1. **准入**：`tokens.model_scope ∈ {ALL, LIST}` + 新表 `token_models(token_id, external_model_id)`，`LIST` 空表 = 全禁（fail-closed）；旧 tag 的 KV 匹配语义不保留（能力收缩，待点头）。
2. **归属**：`sessions` **不补** provider/account 列；会话归属一律按 `billing_records` 的 `upstream_*`（`idx_billing_session` 已在 0001），`sessions.internal_model_id` 降级为绑定结果快照，禁止用于归属聚合。
3. **落位**：新开 **`0024_v2_token_scope.sql`**（纯 ADD），**不改 0019–0023** —— 已执行过的迁移文件改内容会命中「同名异内容静默跳过」（§5.1）；0024 只引用 `tokens` / `external_models`，与 0021 无依赖，排第 6 位安全。本文件编号 0024 为预留，**尚未落地**（等派工）。

