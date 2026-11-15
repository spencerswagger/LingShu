# 灵枢 V2 阶段一 · 数据层与口径定稿（0019–0024）

> 2026-11-15 · Ark（架构）· 依据 `docs/tasks/v2-2026-11-15.md`（唯一口径）与已批准的
> `docs/v2-impl-plan-2026-11-15.md` 五条开放项决议。
> 本文是阶段一后端实现的**唯一数据层依据**：表结构、字段、枚举、写入时机、证据流程。
> 与已批准计划有出入的地方集中在 §10，均给出理由，其中 3 条需负责人一句话拍板（A / E / F）。
>
> **本版修订（同日）**：① DROP 文件更名 `0024` 并排最后，字典序 = 执行序，一次启动跑完整链（§1/§4.3/§11）；
> ② 0020 补跨列 CHECK，0023 补 `skipped` 与 `fetch_offset`（§3.1/§7）；
> ③ 账本翻页固定 `occurred_at ASC`、offset 持久化并支持 blocked 补拉（§6.2）；
> ④ 透传落点补钩子契约与包职责边界、三键配置、query/响应头口径、字节相等断言（§9.2/§9.4/§9.5/§9.6）。

---

## 0 结论先行

1. 编号从 **0019** 重新起号，五个文件各管一件事，**唯一允许出现 DROP 的文件是 0024**。文件名编号不是装饰：`migrate.go` 按**字典序**应用（`internal/db/migrate.go:40`），所以 `0019 < 0020 < 0022 < 0023 < 0024` 就是执行序——**DROP 排在最后，一次启动跑完整链**，不靠人工分两次。
2. 账本先落 **9 列（8 业务字段 + 1 本地主键）**，翻页按 **`occurred_at ASC` + offset**；offset **只持久化在 `reconcile_windows.fetch_offset`**，用途仅限 blocked 窗口补拉，**不作为新一轮的游标**；幂等靠 `ledger_id` 唯一键。
3. 归因统一 `upstream_*` 命名，**全仓不出现第三方产品名**；唯一例外是「回程剥头」的匹配前缀（见 §10-F）。
4. 四协议透传落在**入站 handler 与出站 client 之间的适配层**，网关侧零改动；Gemini 现网**没有路由**，必须新增两条（见 §6）。
5. 0024 的 DROP 绑定「pg_dump → 临时库还原 → 保留表行数不得变少」，**判定点 A/B/C 任一不过即中止**。

---

## 1 编号与职责

主库实测：`origin/main` 最大迁移是 `0018_call_logs_req_params.sql`。所以 0019 起号与主线**不撞号**（旧分支的 `0019_v2_facade.sql` / `0020_reconcile_blocked_status.sql` 未合入，作废、不 cherry-pick）。

| 编号 | 文件（`backend/internal/db/migrations/`） | 一句话职责 | 可回滚 | 幂等写法 |
|---|---|---|---|---|
| 0019 | `0019_v2_topology.sql` | 建四层图的三张表（`internal_models` / `model_links` / `subscriptions`）+ `external_models` 加绑定列 | 是（DROP 三表 / DROP 列） | `IF NOT EXISTS` / `ADD COLUMN IF NOT EXISTS` |
| 0020 | `0020_v2_billing_attribution.sql` | `billing_records` 只加归因列与索引（7 列）+ 1 条跨列 CHECK，**纯 ADD、零 DROP、零 DML** | 是 | 同上 |
| 0022 | `0022_v2_ledger.sql` | 建 `ledger_entries`（账本最小九字段）与 `credit_holds`（预扣登记，阶段二启用） | 是（DROP 两表） | `IF NOT EXISTS` |
| 0023 | `0023_v2_reconcile.sql` | 建 `reconcile_windows`（60s 对账窗口、补拉 offset、状态与告警连续计数） | 是（DROP 表） | `IF NOT EXISTS` |
| 0024 | `0024_v2_drop_legacy.sql` | **唯一的 DROP 文件**：11 张退役表 + 3 个阻塞列；**排最后执行**（字典序即执行序） | **否**（必须走 §5 证据流程） | `DROP TABLE/COLUMN IF EXISTS` |

**为什么 DROP 必须改名排最后**：`migrate.go` 对每个文件各开一个事务、按字典序依次应用（`internal/db/migrate.go:18-70`，排序在 `:40`）。DROP 若留在中间编号（旧 0021），「一次启动跑完整链」时会在建新表之前删旧表——只要后面任一文件失败，库就停在「旧表已删、新表没建」的不可逆中间态。把它挪到 0024：前面 0019/0020/0022/0023 任何一步失败都还没删表，可回滚；同时避免「手工分两次执行」——手工顺序会让 `applied_migrations` 的顺序与文档不符，新人重放必然错序。

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
| `cost_source` | `TEXT` | NULL | — | `CHECK IN ('ledger','amortized','none')`：成本从哪来；`cost_credits IS NULL` 时**必为 `none`**（DB 层跨列 CHECK，见 §3.1），不再是「`none` 或 NULL」二选一 |

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

### 3.1 跨列 CHECK：成本为空就必须标 `none`（DB 层强约束）

```sql
-- 不变量：cost_credits 为 NULL ⇒ cost_source 必须是 'none'。
-- 两个坑必须同时处理：
--   ① CHECK 的 NULL 语义：只写 cost_source IN ('none') 时，若 cost_source 为 NULL 整条表达式为 NULL → 约束放行；
--      必须显式写 cost_source IS NOT NULL 才能把它拒掉；
--   ② 历史行豁免：0001_init.sql:205 的 cost_credits 可空，历史行可能是 (NULL, NULL)，
--      0020 又是「零 DML」——直接 ADD CONSTRAINT 会在校验存量数据时失败。
ALTER TABLE billing_records ADD CONSTRAINT ck_billing_cost_source_none
  CHECK (cost_credits IS NOT NULL OR (cost_source IS NOT NULL AND cost_source IN ('none')))
  NOT VALID;
```

- `NOT VALID` 只跳过**存量行扫描**，**不跳过**新写入与 UPDATE：任何对历史行的更新（典型是回填 `ledger_entry_id` / `upstream_*`）都会被这条约束校验。回填语句必须同时补 `cost_source`：
  ```sql
  UPDATE billing_records
     SET ledger_entry_id = $1,
         cost_source     = COALESCE(cost_source, 'none')   -- 不补就撞 23514
   WHERE billing_id = $2;
  ```
- 阶段二完成历史回填后再 `ALTER TABLE billing_records VALIDATE CONSTRAINT ck_billing_cost_source_none;`（本期不做，写进阶段二清单）。
- 幂等：PG 不支持 `ADD CONSTRAINT IF NOT EXISTS`，按 §12 的 `DO $$ … pg_constraint … $$` 包一层。

两个「不做」：

- **不动 `pricing_mode`**：用户侧仍只有 `sale` / `cost`。订阅摊销是**成本侧**口径，混进用户定价枚举会让报表口径二义（§10-C）。
- **不加 `channel_*` 兼容列**：0024 会删掉 `channel_key_id`，历史「渠道密钥名」维度永久丢失——这是已批准方向，写进 PR 影响面（§10-B）。

---

## 4 0024_v2_drop_legacy.sql（唯一不可回滚文件）

### 4.1 先删列（解除外键/非空阻塞）

```sql
ALTER TABLE billing_records DROP COLUMN IF EXISTS channel_key_id;   -- 连带 idx_billing_key_time
ALTER TABLE sessions        DROP COLUMN IF EXISTS channel_key_id;   -- NOT NULL，必须删；连带 idx_sessions_key
ALTER TABLE tokens          DROP COLUMN IF EXISTS tag_id;           -- 引用 semantic_tags
```

`call_logs.channel_id` / `channel_key_id` / `auto_model_name` **不删**：无外键、不阻塞，且是历史日志快照。删了就没有第二份证据可查（§10-B）。

### 4.2 再删表（子表在前，顺序不可调）

```
channel_tags → channel_model_events → channel_key_events → probe_logs
→ channel_models → channel_keys → channel_events → channels
→ auto_model_members → auto_models → semantic_tags
```

全部 `DROP TABLE IF EXISTS`。**保留不删（15 张，逐张列清，少一张就是数据丢失）**：

```
users · tokens · billing_records · credit_wallets · credit_flows · sessions · call_logs
external_models · sys_configs · announcements · audit_logs
channel_protocols · protocol_error_stats          -- 不在批准清单；无写入方后是孤岛表，等阶段二单独拍板
watchlist_items · price_change_alerts             -- 价格监控，未批准下架（别因为「看起来像渠道衍生」顺手删）
```

> 计数口径：`DROP` 语句 11 条，其中 `channel_events` 早在 `0009_drop_channel_state.sql` 就删了，0024 里那条是**空操作**（保留只为清单完整）→ **实删 10 张**。按「11 张」去找会误以为漏了一张。

### 4.3 为什么 0024 是唯一 DROP 文件、且必须排最后

`migrate.go` 每文件一个事务（`internal/db/migrate.go:18-70`），PG 的 DDL 事务性 → 0024 中途失败会整体回滚，不需要手工 restore。把所有不可逆动作集中在**一个**文件，等于「备份 → 证据 → 执行」只走一次，回滚点只有一个。

但「唯一」还不够，**位置也必须最后**：`migrate.go:40` 按文件名字典序应用，旧编号 0021 会让 DROP 在建表之前执行。一次启动跑完整链时，只要 0022/0023 有一个文件失败（比如 `credit_holds` 的外键写错），库就停在「旧表已删、新表没建」——而 DROP 不可逆。改名 0024 后，链上任何前置文件失败都还留着旧表。

---

## 5 0024 三步证据流程（可直接照抄）

> 执行人：持有宿主 PG 的人（spencer / hugo）。**沙盒内 PG 拒连**（实测 `127.0.0.1:5432` Connection refused），且 `lingshu_dev` 无 `CREATEDB`（2026-11-10 实测），临时库必须由有权限的角色建。

先落两个 SQL 文件（动态精确计数：不写死表名，表多表少都对）：

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
# 保留表清单（15 张，逐张列清）：判据是「行数不得变少」，不是「逐字相同」
cat > counts-retained.sql <<'SQL'
SELECT table_name,
       (xpath('/row/c/text()',
              query_to_xml(format('SELECT count(*) AS c FROM %I.%I', table_schema, table_name),
                           false, true, '')))[1]::text::bigint AS rows
FROM information_schema.tables
WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
  AND table_name = ANY('{users,tokens,billing_records,credit_wallets,credit_flows,
                         sessions,call_logs,external_models,sys_configs,announcements,
                         audit_logs,channel_protocols,protocol_error_stats,
                         watchlist_items,price_change_alerts}')
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

# ── 步骤 0：撞号断言（必须 0018 结尾）─────────────
psql -d $SRC_DB -tAc "SELECT name FROM applied_migrations ORDER BY name DESC LIMIT 1"
# 期望：0018_call_logs_req_params.sql，否则停手

# ── 步骤 1：备份（自定义格式，带 CRC）─────────────
pg_dump -Fc -Z6 -f "$BK" "$SRC_DB"; echo "dump_exit=$?"
ls -l "$BK"; sha256sum "$BK"

# ── 步骤 2：临时库还原 + 全表行数比对 ───────────────
createdb -T template0 "$TMP_DB"; echo "createdb_exit=$?"
pg_restore -d "$TMP_DB" --no-owner --no-privileges -j4 "$BK"; echo "restore_exit=$?"
psql -d $SRC_DB -tA -F'|' -f counts-all.sql | sort > before-all.txt
psql -d $TMP_DB -tA -F'|' -f counts-all.sql | sort > restored-all.txt
diff before-all.txt restored-all.txt && echo "RESTORE-OK"

# ── 步骤 3a：迁移前，先存保留表基线（不停服也成立）─────
psql -d $SRC_DB -tA -F'|' -f counts-retained.sql | sort > before-retained.txt

# ── 步骤 3b：执行迁移（一次启动跑完整链：0019→0020→0022→0023→0024）──
#   ……此处执行 migrate（服务启动或 migrate 命令触发）；不要手工分两次执行……

# ── 步骤 3c：迁移后复核保留表（判据 = 不允许变少，不要求逐字相同）──
psql -d $SRC_DB -tA -F'|' -f counts-retained.sql | sort > after-retained.txt
# ① 表集合必须一致：少一张表 = 误删 → 立即中止
comm -23 <(cut -d'|' -f1 before-retained.txt) <(cut -d'|' -f1 after-retained.txt) > lost-tables.txt
test -s lost-tables.txt && { echo "RETAINED-MISSING:"; cat lost-tables.txt; false; }
# ② 逐表 after >= before；任一表行数变少 → 中止
awk -F'|' 'NR==FNR { b[$1]=$2; next }
           { if (($2+0) < (b[$1]+0)) { printf "LOST %s: %s -> %s\n", $1, b[$1], $2; bad=1 } }
           END { exit bad }' before-retained.txt after-retained.txt && echo "RETAINED-OK(no-loss)"

# ── 步骤 3d：删干净断言（期望空输出）────────────────
psql -d $SRC_DB -tAc "
SELECT string_agg(t, ',') FROM unnest(ARRAY[
 'channels','channel_keys','channel_models','channel_events','probe_logs',
 'semantic_tags','channel_tags','channel_model_events','channel_key_events',
 'auto_models','auto_model_members']) t
WHERE to_regclass(t) IS NOT NULL"
```

**要留的产物**：`*.dump`（文件名 + 大小 + sha256）、`before-all.txt` / `restored-all.txt` / `before-retained.txt` / `after-retained.txt`、三条命令退出码、3d 的空输出。全部贴进 PR 描述。

**失败即中止的判定点**：

| 点 | 条件 | 动作 |
|---|---|---|
| A | `dump_exit≠0`，或 dump 文件 < 1 KiB（空库除外） | **不执行迁移**，重跑备份 |
| B | `createdb_exit≠0` 或 `restore_exit≠0`，或 `diff before-all.txt restored-all.txt` 非空 | **不执行迁移**（备份不可信 = 没备份） |
| C | 3c 不成立：表集合少了任一张，或任一保留表 `after < before`（**判据只到「不变少」**，不要求逐字相同） | **立即中止**；若已 commit，用 dump 全量恢复 |
| D | 3d 有输出（表还在） | 迁移未生效或部分执行 → 查 `applied_migrations` 后重跑 |

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
| 9 | `occurred_at` | `TIMESTAMPTZ` | NOT NULL | — | 账本记账时间（网关时钟，同机部署） |

```sql
CREATE UNIQUE INDEX IF NOT EXISTS uq_ledger_entries_ledger_id ON ledger_entries(ledger_id);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_time ON ledger_entries(occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_account
    ON ledger_entries(provider, provider_account, occurred_at DESC);
```

**POC 后再补的字段（本期一律不加）**：`request_id`、`status/failed`、`retries`、`cache_read_tokens`、`thinking_tokens`、`unit_price`、`currency`。加了就必须先证实字段名，否则一半字段恒为 NULL，比没有更误导。

### 6.2 翻页与补拉：`occurred_at ASC` + 持久化 offset

- 每 60 秒一轮：拉取区间 `[last_end - 120s, now)`——**重叠 2 个窗口**吸收迟到条目，重复靠 `ledger_id` 唯一键吸收。
- **翻页顺序必须显式升序**：`ORDER BY occurred_at ASC`（同 `occurred_at` 再按 `ledger_id ASC` 定序，保证翻页稳定）。**禁止用上游默认顺序**：默认通常是 DESC，从最新往回翻时「超出本轮预算的最老条目」会落在下一轮区间之外 → **永久漏账**，而且没有任何报错。
  - 上游接口不支持升序 → **本窗直接 `blocked` + 告警**，不许「先按 DESC 拉、事后排序」蒙过去（DESC 分页的 offset 一旦跨页，最老条目就被跳过）。
- 每页 200 条；**单轮预算 4000 条**（= 20 页）；预算按「先补 blocked 旧窗（`window_start` 升序）、再拉新区间」分配。
- **offset 持久化在 `reconcile_windows.fetch_offset`**：语义 = 该窗**已处理条数**（下一页起点），每页成功后与落库同事务 `fetch_offset = fetch_offset + 本页返回条数`。只服务 blocked 窗口补拉，**不作为新一轮的游标**（新窗口每次从 0 开始，靠重叠窗口 + 唯一键吸收重复）。
- **blocked 后补拉至清零**：每轮开始先取「未补平的 blocked 窗口」，从各自 `fetch_offset` 续拉；补平（拉完）时同语句写 `status='ok'`、`fetch_offset=0`；仍未拉完则保留 `blocked` + `fetch_offset` 进度。**不引入人工介入**，靠后续轮次自然补平。
- **写入时机：每页一个事务**（`INSERT … ON CONFLICT (ledger_id) DO NOTHING` + `UPDATE reconcile_windows SET fetch_offset = …` 同事务提交）。这是「能补拉」的前提：整轮一个事务 + 回滚，offset 就无处可落，只能每轮从 0 重翻，积压永远不会自愈。
  - 中途失败：已提交的页保留、offset 与已处理条数一致，下一轮从该 offset 续——因为幂等键，重拉同一段只增加 0 行。
- 预算耗尽（还有未拉完的窗口）：本轮不再开新窗，水位线不推进，下一轮自动重来；记一条告警（同一个 blocked 窗口连续告警按 §7 的 `alarm_streak` 升级）。

> 为什么「新窗口」仍然不用持久化 offset 做游标：上游在区间内插入/删除条目会让 offset 跳行（同类坑：**用取整时间做游标会把整天砍成 8 小时**）。新窗口靠「重叠 2 窗 + `ledger_id` 唯一键」吸收重复；持久化的 offset 只在**同一个窗口内部**有意义——窗口边界固定，顺序固定，续拉才可靠。

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
| `window_end` | `TIMESTAMPTZ` | NOT NULL | — | 必须 = 起点 + 60s（DB CHECK，不靠文档） |
| `local_count` / `upstream_count` | `BIGINT` | NOT NULL | `0` | 灵枢账单笔数 / 账本笔数（**由对账作业每窗结束写**，见下） |
| `local_tokens` / `upstream_tokens` | `BIGINT` | NOT NULL | `0` | **总 token 合计**（灵枢 input+cache_read+output；账本 in+out；同上由对账作业写） |
| `status` | `TEXT` | NOT NULL | `'ok'` | `CHECK IN ('ok','alarm','error','blocked','skipped')` |
| `alarm_streak` | `SMALLINT` | NOT NULL | `0` | 连续告警窗数；≥3 升级 |
| `fetch_offset` | `BIGINT` | NOT NULL | `0` | **补拉进度**：该窗已处理条数（§6.2）；无补拉任务时必须为 0 |
| `detail` | `JSONB` | NOT NULL | `'{}'` | 排障明细（分组偏差、页数、原因），**仅兜底，不参与判定** |
| `checked_at` | `TIMESTAMPTZ` | NOT NULL | `now()` | 最后一次写入时间 |

**只比总 token 与笔数，不比金额**：账本币种/牌价与灵枢积分不等价，比金额必然误报。

### 7.1 状态取值域（每个值只有一种含义）

| 值 | 含义 | 谁能写 |
|---|---|---|
| `ok` | 本窗已拉完，且已对账、偏差在阈值内 | 同步链（拉完）→ 对账作业（判定） |
| `alarm` | 已对账、偏差超阈值 | 对账作业 |
| `error` | 拉取或落库失败（本窗没拉完） | 同步链 |
| `blocked` | 单轮 4000 条预算用尽（本窗没拉完，待补拉） | 同步链 |
| `skipped` | **没有对账结论**：账本侧关闭 / 不可用 / 本窗仍 `blocked` | 对账作业 |

`skipped` 的硬约束：
- 必须带 `detail.skip_reason`，取值域 `ledger_disabled` / `ledger_unavailable` / `fetch_blocked` / `service_down`；
- **禁止**把「两边都 0」写成 `skipped`——那是 `ok`；
- `skipped` 不推进 `alarm_streak`（既不 +1 也不清零），也不写四个计数列（保持 0），因为它没有结论。

### 7.2 四个计数列的写入责任方：对账作业，每窗结束统一写

- **写入方 = 对账服务（`reconcile`）在每窗结束时的一条 UPDATE**，四列必须同时写齐：
  ```sql
  UPDATE reconcile_windows
     SET local_count     = $2,
         upstream_count  = $3,
         local_tokens    = $4,
         upstream_tokens = $5,
         -- ok → 清零；alarm → +1；skipped → 保持原值（既不 +1 也不清零）
         alarm_streak    = CASE $6 WHEN 'alarm' THEN alarm_streak + 1
                                   WHEN 'ok'    THEN 0
                                   ELSE alarm_streak END,
         status          = $6,
         checked_at      = now()
   WHERE window_start = $1
     AND status NOT IN ('blocked','error');   -- 未拉完的窗不许判定，防整行覆盖把 blocked 抹成 ok
  ```
- **同步链一律不写这四列**（同步轮次拿它当计数会污染对账口径）：同步链只写 `status ∈ {ok,blocked,error}`、`fetch_offset`、`detail.sync.*`、`checked_at`。
- **列写分区（同一个表两个写入方，必须只 SET 自己的列）**：双方都用 `ON CONFLICT (window_start) DO UPDATE`，且**禁止整行覆盖**——整行覆盖会把对方刚写的列抹成默认值，这是本表唯一的「幂等」实现要求。
- **冲突优先**：`status ∈ {blocked,error}` 的窗口，对账作业**不得**改 `status`、不得写计数列，只允许追加 `detail.reconcile.skip_reason`；必须先补拉成功（`status='ok'`）才判定。
- 口径定义：
  - `local_count` = 该窗 `billing_records` 中 `status='completed'` 的行数；
  - `local_tokens` = 这些行 `tokens` JSONB 的三段合计（键名与 `billing/query.go:253` 一致）：
    ```sql
    COALESCE(NULLIF(tokens->>'input','')::bigint,0)
  + COALESCE(NULLIF(tokens->>'cache_read','')::bigint,0)
  + COALESCE(NULLIF(tokens->>'output','')::bigint,0)
    ```
  - 失败/拒绝行不计入（它们在账本侧一定没有对应条目，计进去就是每窗恒告警）。
- **`detail` 只兜底**：偏差的分组明细（key×model）、页数、失败原因可以进 `detail`，但「漏账可见」的证据**只能**由四个计数列 + `status` 承担。四个计数列没写齐之前，任何「漏账已被暴露 / 已对账」的结论都不成立。

---

## 8 归因字段口径

| 归因维度 | 字段 | 落点 | 取值 / 兜底 |
|---|---|---|---|
| 请求 | `billing_records.request_id` | 0020 | 入站生成；`billing_id` 仍为对外主键 |
| 渠道（供应商） | `billing_records.upstream_provider` | 0020 | 回程头（**剥前**在 `OnUpstreamResponse` 里读，见 §9.2.2）→ 账本 `provider` 校对；缺失 `NULL` |
| 账号 | `billing_records.upstream_account` | 0020 | 回程头或账本 `provider_account`；缺失 `NULL`（**不猜**） |
| 协议 | `billing_records.upstream_protocol` | 0020 | `openai-chat` / `openai-responses` / `anthropic-messages` / `gemini` |
| 内部模型（组/直通） | `billing_records.internal_model_id`（已有） | 0001 | 组归因 = 该 ref 在 `internal_models.kind='group'`；**不另立 `group_ref`** |
| 对外模型 | `external_model_name`（请求名）+ `upstream_model`（实际服务） | 已有 + 0020 | 两个名字分开留，改价/改绑定后历史可解释 |
| 令牌 / 用户 / 会话 | `token_id` / `user_id` / `session_id`（已有） | 0001 | — |
| 账本 | `ledger_entries.ledger_id` ↔ `billing_records.ledger_entry_id` | 0022 / 0020 | 部分唯一索引保证一对一 |
| 成本来源 | `billing_records.cost_source` | 0020 | `ledger` / `amortized` / `none` |

命名铁律：表、列、配置键、日志字段、界面文案**统一用 `upstream`**，不出现第三方产品名（§10-F 是唯一例外）。

归因的**读取时机只有一处**：上游响应头到达、还没向下游写出任何字节的那一刻（`OnUpstreamResponse`，§9.2.2）。此时头还没被剔除，是唯一能读到上游标识的窗口；一旦开始回写，头已剥、字节已流出，再想补归因只能靠猜。因此**归因属于主链**，不属于转发包（§9.2）。

---

## 9 四协议透传落点

### 9.1 结论（一段话）

透传落在**入站 handler 与出站 client 之间的适配层**：`server.go` 的 mux 早已把整个 `/v1/` 挂给 `gateway.Gateway`，所以**入站不需要新层**；改动只发生在 `gateway.gateway.go` 的路由表和 `gateway.handler.go` 的 `serve()` 中段——前半段（令牌鉴权、请求体读取、model 解析、预扣、请求快照）原样保留，**「路由选渠道 + Provider 组包 + 协议互译」整段删除**，替换为 `upstream.Client.Forward(ctx, prepared, onResp)`：请求体逐字节转发（只改 Gemini path 里的 model 段），回程 `io.Copy` 流式回写。转发包**只**做协议/路径解析与字节转发（§9.2.1），并把「响应头已到、字节未写」的那一刻通过 `OnUpstreamResponse` 交回主链（§9.2.2）——预扣、归因读取、`billing_records` 落库全在主链。**这不是旁路**：账单与归因仍在主链；也不是配置层：配置只提供基址/超时/降级开关（§9.4）。网关侧零改动，因为它本来就原生暴露这四套协议，灵枢只是把「同名路径 + 同字节」递过去。

### 9.2 涉及的文件位置与包职责边界（实测）

| 位置 | 现状 | V2 动作 |
|---|---|---|
| `backend/internal/server/server.go:44`（`Gateway http.Handler`）+ `routes()` | 已把 `/v1/` 整体挂载 | 不改 |
| `backend/internal/domain/gateway/gateway.go:313 Handler()` | 注册 6 条：models + chat/completions + **completions** + **embeddings** + responses + messages | 保留 `GET /v1/models` + 4 条；**下线 completions / embeddings**（不注册即 404） |
| Gemini 路由 | **不存在** | 新增 `POST /v1beta/models/` 前缀 + 手写解析 `models/{model}:{generateContent\|streamGenerateContent}`。**不能用 `{model}` 通配**：Go 1.27 实测 `mux.HandleFunc("POST /v1beta/models/{model}:generateContent", …)` 直接 panic —— `at offset 20: bad wildcard segment (must end with '}')` |
| `backend/internal/domain/gateway/handler.go:220 serve()` | 鉴权→读体→解析 model→标签→路由→预扣→上游→计费→落库 | 保留鉴权/读体/解析/预扣/计费；中段换成 `upstream.Client`；删 tagKV、会话成本路由、failover 回喂 |
| 新建 `backend/internal/domain/upstream/` | — | `client.go`（转发/剥头/usage 扫描）、`ledger.go`（60s 拉取 + offset 翻页）、`models.go`（模型/组/供应商/配额查询）、`protocol.go`（四协议常量，**不往 `channelprotocol` 加**）、`config.go`（读 sys_configs） |
| 新建 `backend/internal/pkg/backoff/` | — | 5/20/60s、≤3 次，唯一实现 |

#### 9.2.1 职责边界：只做协议/路径解析与字节转发

转发包（`domain/upstream`，实现时可按需拆子包）**只允许做四件事**：

1. **path 解析与唯一改写**：识别四协议端点；Gemini 把 `models/{对外名}` 换成 `models/{上游名}`，`:generateContent` / `:streamGenerateContent` 后缀原样。
2. **query 处理**：逐字拼接；唯一剔除「本次鉴权取自 query 的 `key`」（§9.5）。
3. **字节转发**：请求体逐字节发出；响应体逐字节流式回写；**不做任何 JSON 重编码**（反序列化再序列化会改键序、数字写法与转义）。
4. **头处理**：请求头按强制黑名单剔除后原样 + 注入本次令牌的凭据头；响应头按 §9.5 清单剔除后交给下游。

**不做的（越界即视为违反契约）**：

- 不做预扣 / 结算 / 退款；
- 不写 `billing_records` / `credit_holds` / `credit_flows`；
- 不做归因读取与落库（`upstream_provider/account/model/protocol` 四列）；
- 不做计费口径的 usage 归一化（只允许旁路扫描，把结果交回调）；
- 不选渠道、不做 failover、不做协议互译（这些是上游网关的职责，本仓不做）；
- 不自己读配置文件/环境变量：基址、超时、开关由装配层从 `sys_configs` 解析后注入（§9.4）。

#### 9.2.2 `OnUpstreamResponse`：签名、时序、主链消费

转发包必须暴露下面这个钩子；**不注入 = 不带计费能力的纯转发**，只允许出现在自测里，生产装配必须注入。

```go
// UpstreamResponse 是「上游响应头已到、下游还没收到任何字节」这一刻的只读事实。
// 三个数据字段全部只读：钩子不得改 Header、不得换 StatusCode、不得整段缓冲 Body。
type UpstreamResponse struct {
	StatusCode int         // 上游状态码（原样回给客户端）
	Header     http.Header // 上游原始响应头（剥前读取归因；下发前按 §9.5 剔除）
	Body       io.Reader   // 上游原始响应体（未解码、未重编码）
	Protocol   string      // 入站协议：openai-chat / openai-responses / anthropic-messages / gemini
	RequestID  string      // 灵枢入站生成的 request_id（不是上游的）
	Streamed   bool        // 上游是否 SSE（按 Content-Type 判定）
}

// OnUpstreamResponse 在「收到上游响应头之后、向下游写出任何字节之前」同步调用且只调用一次。
// 返回的 io.Reader 由转发链原样流式写给客户端：钩子只能包装 up.Body（tee / 计数器），
// 不得替换成另一段字节、不得提前读完（否则流式变缓冲）。
// 返回 error = 主链拒绝继续：转发链不写任何响应头与字节，由主链写脱敏错误响应（502/504）。
type OnUpstreamResponse func(ctx context.Context, up *UpstreamResponse) (io.Reader, error)
```

调用时序（谁在哪个阶段做什么，顺序不可调）：

| # | 执行者 | 动作 | 失败/边界 |
|---|---|---|---|
| 1 | 主链 `handler.serve()`（`:220`） | 令牌鉴权 → 读请求体 → 解析对外模型 → **预扣**（`CreditService.Consume`）→ 组装 `Prepared{TokenID,UserID,ExternalModel,Body,UpstreamPath,StripQueryKey}` | 预扣失败即拒，不进转发 |
| 2 | 转发包 | path 改写（Gemini 段）→ query 拼装（剔除鉴权 `key`）→ 请求头黑名单 + 注入凭据头 → body 逐字节发出 | 建连失败 503、首包超时 504 |
| 3 | 转发包 | 收到上游响应头 → 构造 `UpstreamResponse` → **同步调用 `OnUpstreamResponse` 一次** | 钩子返回 error → 不发下游、回脱敏错误 |
| 4 | 主链（在钩子内） | ① **读归因**：从 `up.Header` 读上游标识头（剥前），连同 `up.Protocol` 定稿 `upstream_*` 快照；读不到 → NULL，不猜。② **包装 body**：返回 `io.TeeReader(up.Body, usageScanner)` 或等价包装，扫描旁路、不阻塞、不改字节。③ 定稿 `x-request-id` 的对外值 = `up.RequestID` | 钩子内**只做内存动作**：不查库、不等待账本、不读配置 |
| 5 | 转发包 | 按 §9.5 剔除响应头 → 写状态码 → 逐字节流式回写钩子返回的 reader → 每段 flush | 流空闲超时/客户端断开 → 中断上游，保留已读 usage |
| 6 | 转发包 | 结束（EOF / 断开 / 超时）→ 回填 `Result{Status, Usage, HasUsage, Streamed, Canceled, TimedOut, NoContent, Billable, Err}` | — |
| 7 | 主链（在 `OnComplete` 里） | 差额结算（`Settle`；请求失败 → 退预扣）→ **写 `billing_records`**（归因七列 + `request_id` + `ledger_entry_id` 留空待回填）→ 记 `credit_flows` | 结算失败必须留 ERROR 日志，不许静默 |

主链消费的三件事与落点：

| 事项 | 发生时机 | 落点 | 数据来源 |
|---|---|---|---|
| 预扣 | 钩子**之前**（请求进入时） | `credit_wallets` + `credit_flows`（type=consume） | 对外模型售价 × 估算输入 token |
| 归因 | 钩子**内**（剥头前，唯一窗口） | `billing_records.upstream_*` | `up.Header` + `up.Protocol` |
| 结算与落账 | 钩子**之后**（`OnComplete`，流结束、事务边界清晰） | `billing_records` + `credit_flows`（adjust/recharge） | `Result.Usage` + 步骤 4 的归因快照 |

三条硬约束：

1. **钩子同步、必须在下发首字节前返回**。钩子里等账本（60s 一轮）等于把转发变成轮询；`ledger_entry_id` 由后续回填，不在请求内等待。
2. **钩子失败 = 本请求失败**，不许「先回客户端成功、账单后补」——客户端拿到 200 而账单缺失，对账就是永久缺口。
3. **钩子内不碰 DB、不读配置**：流式路径上持有连接会把慢查询传导成上游超时；所有写入放在 `OnComplete`。

#### 9.2.3 为什么否掉「整包接管且本期不写 billing_records」

四条理由，任何一条单独成立：

1. **预扣在请求进入时**：预扣要做余额校验与冻结，整包接管意味着包必须依赖 `billing`/`credit` 域 → 计费域反向依赖转发域；而「预扣—结算—落账」必须在一个可证的边界内守恒，搬出去就证不了。
2. **不写 `billing_records` 等于对账没有 local 侧**：`reconcile_windows.local_count/local_tokens` 只能从 `billing_records` 取（§7.2）。本期不写 → 每窗恒 0 → 「漏账可见」永久不可验，直接与本切片唯一的对账判据冲突。
3. **回归面最大、收益为零**：整包接管等于重写 `handler.go` 已有四段（鉴权、读体、预扣、请求快照，`:220 serve()` 起）。四协议透传真正要换的只有「路由选渠道 + 组包 + 互译」这一段；把不动的代码重写一遍，只会把已有 2000+ 行行为拖进风险。
4. **可测性**：钩子签名能让 Gate 用假上游直接单测（随机字节 → 断言下发字节相等 + 落库字段正确）；整包接管只能端到端，失败时分不清是转发错还是计费错。

### 9.3 数据如何流经（网关不改）

```
客户端 → 灵枢 /v1/...（四协议同名路径）
  → [主链] 令牌鉴权 → model 解析 → 预扣 → 组装 Prepared
  → [转发包] path 改写（Gemini 段）+ query 逐字 + 请求头黑名单 + 注入凭据头 + body 逐字节
  → 网关 127.0.0.1:3425 做协议互译/渠道/密钥/failover
  → [转发包] 收到响应头 → 调 OnUpstreamResponse 一次（此刻下游还没收到任何字节）
      → [主链] 读上游标识头（归因，剥前）→ 包装 body（旁路扫 usage）
  → [转发包] 按 §9.5 剔除响应头 → 写状态码 → 逐字节流式回写 → flush
  → [主链] OnComplete：差额结算 / 退预扣 → 写 billing_records（含 upstream_* 归因、
      request_id；ledger_entry_id 留空待账本回填）
  → 客户端（响应体逐字节原样；响应头里永远看不到上游身份）
```

### 9.4 sys_configs 三键（最终形态）

转发链、同步器、账本拉取**共用同一组**运行参数，只有三个键。逻辑形态：

```yaml
upstream:
  base_url: "http://127.0.0.1:3425"
  timeouts:
    stream_first_byte_ms: 30000
    stream_idle_ms: 120000
  degrade:
    passthrough_enabled: true
    ledger_enabled: true
    sync_enabled: true
```

落到 `sys_configs` 是三行 `key → jsonb`：

| 键 | 值 | 默认 | 校验 | 谁读 |
|---|---|---|---|---|
| `upstream.base_url` | 字符串 | `""`（= 未接入：`/v1/*` 保持 410/503，同步不启动） | 只收 `http`/`https`；**拒绝 URL 内带凭据**（`user:pass@`）；去空白与尾斜杠 | 转发链 + 同步器 + 账本拉取（三处同一个值，不许各读一份） |
| `upstream.timeouts` | 对象，**恰好两个字段** | `{"stream_first_byte_ms":30000,"stream_idle_ms":120000}` | 两个都是整数毫秒，1000–600000；缺字段取默认；**多出来的子键忽略并记 WARN**（不因多一个键整块失效） | 转发链（首包超时 / 流空闲超时） |
| `upstream.degrade` | 对象，**恰好三个开关** | 三个都 `true` | 三个都必须是布尔；非法值沿用上期成功值 | 转发链（`passthrough_enabled`）/ 账本拉取（`ledger_enabled`）/ 同步器（`sync_enabled`） |

三开关的语义（**`true` = 功能开启**，与旧键相反，见下面的映射）：

- `passthrough_enabled=false` → `/v1/*` 与 `/v1beta/*` 一律回脱敏 503，不发上游；面板只读空态 + 告警。
- `ledger_enabled=false` → 不拉账本；对账作业对**有本地账单的窗口**一律记 `skipped`（`skip_reason='ledger_disabled'`，§7.1），窗口里的计数列保持 0。
- `sync_enabled=false` → 同步器不跑，沿用上期快照（不清数据、不标灰）。

**删掉的字段与它们的去处**：

| 旧字段 | 处理 | 理由 |
|---|---|---|
| `upstream.timeout_ms`（单值 30000） | **删除**，改由 `upstream.timeouts` 两字段表达 | 单值表达不了「首包 30s / 流空闲 120s」两条流式超时；一个值兜两种语义，出问题无法调 |
| `timeouts.connect_ms`（10000） | 降为**代码常量**（10s），不再暴露 | 建连超时是环境属性，不是运营参数；可配置只会配错 |
| `timeouts.request_ms`（600000） | 降为**代码常量**（600s），不再暴露 | 非流式总超时同理；流式由两字段夹住，不需要第三个值 |
| `degrade` 单布尔 | 拆成三开关（对象） | 一个开关同时停转发/停对账/停同步，粒度太粗：停同步不该顺带停转发 |

**旧键 → 新键迁移映射**（一次性派生写，不需要新迁移文件；执行时机 = 0024 之后、新版本启动之前）：

| 旧键 / 旧形态 | 新落点 | 转换规则 | 风险点 |
|---|---|---|---|
| `upstream.base_url`（字符串） | 同键 | 只归一化（trim、去尾斜杠、拒凭据 URL） | 无 |
| `upstream.timeout_ms`（整数，默认 30000，1s–10min） | `upstream.timeouts.stream_first_byte_ms` | 取旧值并 clamp 到 1000–600000 | 语义从「单次调用总超时」收窄为「流式首包上限」：旧值调很大的人，非流式总超时从可配变成固定 600s |
| （无） | `upstream.timeouts.stream_idle_ms` | 固定 `120000` | 无 |
| `upstream.degrade`（**布尔**） | `upstream.degrade.{passthrough_enabled,ledger_enabled,sync_enabled}` | **极性相反**：旧 `true`（= 降级停摆）→ 三个开关全 `false`；旧 `false` → 全 `true` | ⚠ 直接换形态会静默翻转：旧值 `true` 若被当成「true = 开启」，等于打开全部功能而本该停摆。**必须**按下述 SQL 派生，并在读侧兼容旧类型 |

```sql
-- ① 旧布尔 degrade → 三开关（极性取反）
UPDATE sys_configs
   SET value = jsonb_build_object(
         'passthrough_enabled', NOT (value #>> '{}')::boolean,
         'ledger_enabled',      NOT (value #>> '{}')::boolean,
         'sync_enabled',        NOT (value #>> '{}')::boolean)
 WHERE key = 'upstream.degrade' AND jsonb_typeof(value) = 'boolean';

-- ② 旧单值 timeout_ms → timeouts.stream_first_byte_ms（幂等：已有对象就不覆盖）
INSERT INTO sys_configs(key, value)
SELECT 'upstream.timeouts',
       jsonb_build_object(
         'stream_first_byte_ms', LEAST(GREATEST((value #>> '{}')::int, 1000), 600000),
         'stream_idle_ms', 120000)
  FROM sys_configs
 WHERE key = 'upstream.timeout_ms'
ON CONFLICT (key) DO NOTHING;

-- ③ 删旧键（派生完成后旧键不再有任何读取方）
DELETE FROM sys_configs WHERE key = 'upstream.timeout_ms';
```

**只读 `sys_configs`、不读 `magpie.*` 应用配置**（硬约束）：

- 运行期唯一数据源 = 上面三个键。`internal/config` 里的上游节（`base_url` / `auth_header` / `*_ms` / `subscription_prefixes`）**不得**出现在转发链、同步器、账本拉取的调用路径上；`magpie.FromApp(...)` / `ResolvedMagpie()` 只允许存在于装配层一次性接线，且接线后不得再把值传进运行期读取函数。
- 守卫（可写成用例/CI grep，不靠自觉）：
  ```bash
  git grep -n "ResolvedMagpie\|FromApp(" -- backend/internal/domain/upstream backend/internal/domain/gateway | grep -v _test.go
  # 期望：空输出
  ```
- **凭据不进这三个键**：`sys_configs` 是管理员明文可读页面。上游鉴权头名是代码常量，头值由装配层逐请求注入；任何 key/token/secret 都不落表（沿用既有红线）。
- **没有第二套兜底**：取值顺序固定为「`sys_configs` → 代码默认」。不读环境变量、不读应用配置文件——否则又变成两套真源，改配置不生效的锅没人认。
- 读失败/值非法：沿用上期成功值 + WARN，**不把已有配置抹空**；写钩子触发缓存失效（与 `billing` 既有做法一致）。

**严禁**任何凭据/密钥进这三块。写入沿用现有「只写出现的键」语义（`console/admin.go:560-884`），老客户端整块提交不会覆盖新块。

### 9.5 query 原样与响应头剔除（接口面口径）

**请求 query**：

- **逐字原样**：参数顺序、重复键、空值、百分号编码全部保持原样；**禁止** `url.Values.Encode()`（它会重排键、重编码值）。拼 URL 时只做字符串拼接。
- **唯一例外**：Gemini 原生鉴权参数 `key`。`/v1beta/models/{m}:generateContent?key=<租户令牌>` 的 `key` 是**灵枢的租户令牌**，转发前必须剔除；剔除后若 query 为空，不留尾部 `?`。
- `alt=sse`、`prettyPrint`、`v1beta` 的其它参数一律保留。
- 判定条件写死为「本次鉴权取自 query 的 `key`」，不是「路径里有没有 `v1beta`」——凭据来源决定是否剔除，避免按路径特判漏掉变体。

**响应头（下发前剔除清单，顺序执行）**：

| # | 剔除对象 | 匹配方式 | 理由 |
|---|---|---|---|
| 1 | `x-magpie-*` | 头名前缀，大小写不敏感 | 上游身份标识（provider / model / account 都在这组头里）；归因只在剥前读进内存 |
| 2 | `server` | 精确名 | 暴露上游软件栈与版本；本仓也**不补写**自己的 `Server` 头 |
| 3 | 任何**头名或头值包含已知供应商/渠道/账号名**的头 | 子串匹配（大小写不敏感），名单来自 `internal_models.provider_ref` 与账本 `provider` / `provider_account` | 上游会自造 `x-<provider>-*` 一类头，枚举不全；按名字集合兜底。名单为空时只执行 1/2/4 |
| 4 | 连接级 hop-by-hop：`connection` / `keep-alive` / `proxy-connection` / `te` / `trailer` / `transfer-encoding` / `upgrade` | 精确名 | 不能跨代理复用 |

**保留**：

- `x-request-id`：值是**灵枢自己的** `request_id`（用 `up.RequestID` 覆盖上游同名值）。客户端拿它报障能直接对上本地账单；上游自己的那个 id 不回给客户端。
- 其余头原样（`content-type`、`content-length`、`anthropic-*` 自定义头、`x-ratelimit-*` 等）。
- 头剔除**不影响响应体字节**：断言只看 body（§9.6）。

### 9.6 透传断言：字节相等，不做任何内容判断

对四协议 × 流式/非流式，断言 **客户端收到的响应体字节序列 == 上游发出的响应体字节序列**：

- **样本生成**：8 类形状 × 随机参数，每类 ≥20 例，禁止用固定样本。
  - 8 类形状：标准 JSON / 深嵌套 JSON / 缺字段 / 非 JSON 纯文本 / HTML / 空 body / 超长 body（多 MiB）/ 5xx 错误体；
  - 随机化维度：字段名长度 1–64（含非 ASCII 与转义符）、嵌套深度 1–8、总长度 0–4 MiB、SSE 分片边界随机。
- **断言形式**：`sha256(下发字节) == sha256(上游发出字节)`；流式还要求事件切分一致（不许合并帧、补帧、重排）。
- **三条禁止**（出现任一条即判失败）：
  1. 按 content-type 或内容分支裁剪（"JSON 才透传、HTML 换文案"这类）；
  2. JSON 反序列化再序列化（键序/数字写法/转义会变）；
  3. 错误文案映射或补齐（把上游 5xx 体换自家文案、补 `[DONE]`、补 `message_stop`）。
- **唯一豁免**：本次请求的凭据回显遮蔽（把命中的 callerKey 子串替换成 `<redacted>`，未命中时字节与 `Content-Length` 零变化）。豁免样本必须单独标注，**不得混进「字节相等」的通过率统计**。

---

## 10 与已批准计划的差异（逐条理由）

| # | 冲突点 | 我的裁定 | 为什么 |
|---|---|---|---|
| A | 「旧分支关掉」 | **尚未关掉**：远端仍有 3 个 V2 分支（`feat/lingshu-v2-p1-usage-billing`、`-slim-migrate`、`-magpie-passthrough`），三个都含 `0019_v2_facade.sql`，usage-billing 另含 `0020_reconcile_blocked_status.sql` | main 只到 0018，编号**不冲突**；但「关掉」这个动作没人做。需要 hugo/spencer 删远端分支，并明确旧 0019/0020 永久作废。我不动远端分支 |
| B | 计划写「0020 摘 channel 维度」 | 摘列移到 0024，0020 纯 ADD | 不可回滚动作只应出现在一个文件，备份/证据流程只走一次 |
| C | 计划写「`pricing_mode` 扩 sale·subscription·usage」 | **不扩**，用户侧仍 `sale`/`cost`；成本来源另立 `cost_source` | `pricing_mode` 在现有代码里是**用户售价**口径（`users.pricing_mode` 同义）；把上游成本口径塞进去，盈亏页与账单页会读出两种含义 |
| D | 旧契约金额用「BIGINT 微元」 | 账本金额用 `NUMERIC(20,5)` 积分 | 与 `billing_records.cost_credits` / `credit_wallets.balance` 同型同精度，报表可直接加减；不引第二套金额单位（旧契约已作废） |
| E | 「一令牌一把网关凭据」 | **0019–0024 没有它的落点**，属计划缺口 | 本期用服务端单把 key（环境变量注入，**不进 sys_configs**）；一令牌一把需要新表，建议放 0025 或阶段二，**需负责人拍板** |
| F | 红线「配置与代码里不出现产品名」 | 唯一例外：**剥除回程头的匹配前缀**必须按上游真实头名写常量 | 不匹配就剥不掉，等于泄漏上游身份；该常量只在剥头白名单与注释里，界面/配置/日志回显仍不出现。**请负责人确认这条例外** |
| G | 任务书写 `external_models.bound_internal_model_ref` 且同时建 `model_links` | 绑定真源唯一的放在 `external_models`，`model_links` 只存画布坐标 | 同一关系两处可写，必然漂移（谁后写谁生效），拓扑与售卖会对不上 |
| H | 「四协议透传」当成现成能力 | 现网**没有 Gemini 路由**，且注册了 embeddings/completions | 必须新增 Gemini 前缀路由 + 下线两条端点；这是阶段一唯一的真新增路由 |
| I | 「迁移前 pg_dump 并校验」 | 沙盒 PG 拒连、`lingshu_dev` 无 `CREATEDB` | 证据流程必须在宿主由有权限的角色执行（临时库谁建需确认），沙盒里只能审 SQL 不能跑 |
| J | §11 写「0021 最后执行」，但迁移器按字典序（`migrate.go:40`），0021 必然排在 0022/0023 之前 | **0021 更名 0024 排最后**；一次启动跑完整链，不许手工分两次 | 字典序即执行序。DROP 排在链尾，前面任何一步失败都还留着旧表；手工分两次会让 `applied_migrations` 顺序与文档不符 |
| K | 上游运行参数存在三套写法：本文三键 / 第一切片已实现 `upstream.timeout_ms`+布尔 `degrade` / 转发链读 `magpie.*` 应用配置 | 统一为 §9.4 三键；删 `timeout_ms`；转发链**只读 `sys_configs`** | 单值超时表达不了「流首包 / 流空闲」两条；两套真源必然出现「改了配置不生效」 |
| L | 0023 `status` 缺 `skipped`；四个对账计数列**全仓没有写入方**（窗口里恒 0） | 0023 加 `skipped` 与 `fetch_offset`（§6.2/§7）；计数列写入方 = 对账作业每窗结束一条 UPDATE | 没有 `skipped` 只能把「没对账」写成 `ok`/`error`，语义污染；计数列没有写入方 = 漏账永远不可见 |
| M | 0020 只有两个枚举 CHECK，「`cost_credits IS NULL` → `cost_source='none'`」只写在文档里 | 补跨列 CHECK（`NOT VALID`），DB 层强约束（§3.1） | 只靠写侧用例，任何新增写入方都能绕过去 |

---

## 11 阶段一执行顺序（数据层）

1. `git fetch origin` → 从 `origin/main` 开分支 → **先跑 §1 撞号断言**（必须 `0018_call_logs_req_params.sql`）。
2. **一次启动跑完整链**：`0019 → 0020 → 0022 → 0023 → 0024`。字典序就是执行序（`migrate.go:40`），所以不需要、也**不允许**手工分两次执行；空库与存量库各跑一遍，再启动一次新增 0 条。
3. **证据时机**：备份（§5 步骤 1–2）在整链**之前**；保留表基线 `before-retained.txt` 在整链**之前**；`3c/3d` 在整链**之后**。因为 0024 排在最后，中间任一文件失败都还没删表，可以用 dump 直接恢复。
4. 自测（每条都要能构造出反例）：
   - 账本 2 万条乱序灌入（含 30% 重复 `ledger_id`）→ 落库条数 = 去重后条数；同窗重跑新增 0；
   - 窗口断言 `window_end = window_start + 60s`（写 59s/61s 被 DB 拒 23514）；
   - `status` 写 `skipped` 通过、写 `unknown` 被拒；`skipped` 行带 `detail.skip_reason`；
   - `billing_records` 写 `(cost_credits NULL, cost_source NULL)` 被拒；写 `(NULL,'none')` 通过；历史行 `UPDATE` 时不同时补 `cost_source` 会撞 23514；
   - 账本拉取构造 >4000 条（>20 页）→ `status='blocked'`、`fetch_offset=4000`、告警 1 条；下一轮从 4000 续拉直至补平 → `status='ok'`、`fetch_offset=0`；
   - 每页事务中途失败 → 已提交页保留、`fetch_offset` == 已处理条数，重跑该窗只增加 0 行。
---

## 12 附录：五个迁移文件的权威 DDL（Core 逐字落文件，不要改写）

> 注意：PG 不支持 `ADD CONSTRAINT IF NOT EXISTS`，所以 0020 的枚举约束用 `DO $$ … pg_constraint …$$` 包一层保证幂等；
> 其余全部 `IF NOT EXISTS` / `ADD COLUMN IF NOT EXISTS`。

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
  -- 成本为空必须标 none：显式写 IS NOT NULL 才拦得住 NULL 放行（CHECK 为 NULL 视为通过）。
  -- NOT VALID：历史行可能是 (cost_credits NULL, cost_source NULL)，零 DML 迁移不能扫描它们；
  -- 新写入与 UPDATE 仍受约束（回填 ledger_entry_id 时必须同时补 cost_source='none'）。
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_billing_cost_source_none') THEN
    ALTER TABLE billing_records ADD CONSTRAINT ck_billing_cost_source_none
      CHECK (cost_credits IS NOT NULL OR (cost_source IS NOT NULL AND cost_source IN ('none')))
      NOT VALID;
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
    status          TEXT NOT NULL DEFAULT 'ok'
                    CHECK (status IN ('ok','alarm','error','blocked','skipped')),
    alarm_streak    SMALLINT NOT NULL DEFAULT 0,
    -- 补拉进度：该窗已处理条数（下一页 offset）；无补拉任务时必须是 0，补平后清 0。
    fetch_offset    BIGINT NOT NULL DEFAULT 0,
    detail          JSONB NOT NULL DEFAULT '{}'::jsonb,
    checked_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 窗口必须是整一分钟：错位窗口不许写同一行起点，靠约束而非文档保证
    CONSTRAINT ck_reconcile_window_len CHECK (window_end = window_start + interval '60 seconds'),
    -- 补拉进度不许为负
    CONSTRAINT ck_reconcile_fetch_offset CHECK (fetch_offset >= 0)
);
CREATE INDEX IF NOT EXISTS idx_reconcile_status ON reconcile_windows(status, window_start DESC);
-- 补拉入口：每轮先取未补平的 blocked 窗口（按起点升序）
CREATE INDEX IF NOT EXISTS idx_reconcile_blocked ON reconcile_windows(window_start) WHERE status = 'blocked';
```

### 0024_v2_drop_legacy.sql

> 编号最大 = 字典序最后 = **执行序最后**。文件名不许再改（改号会改变执行顺序）。

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
DROP TABLE IF EXISTS channel_events;
DROP TABLE IF EXISTS channels;
DROP TABLE IF EXISTS auto_model_members;
DROP TABLE IF EXISTS auto_models;
DROP TABLE IF EXISTS semantic_tags;
```
