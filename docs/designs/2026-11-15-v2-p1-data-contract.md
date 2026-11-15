# 灵枢 V2 阶段一 · 数据层与口径定稿（0019–0023）

> 2026-11-15 · Ark（架构）· 依据 `docs/tasks/v2-2026-11-15.md`（唯一口径）与已批准的
> `docs/v2-impl-plan-2026-11-15.md` 五条开放项决议。
> 本文是阶段一后端实现的**唯一数据层依据**：表结构、字段、枚举、写入时机、证据流程。
> 与已批准计划有出入的地方集中在 §10，均给出理由，其中 3 条需负责人一句话拍板（A / E / F）。

---

## 0 结论先行

1. 编号从 **0019** 重新起号，五个文件各管一件事，**唯一允许出现 DROP 的文件是 0021**。
2. 账本先落 **9 列（8 业务字段 + 1 本地主键）**，分页按 **窗口内 offset**，**不持久化游标**；幂等靠 `ledger_id` 唯一键。
3. 归因统一 `upstream_*` 命名，**全仓不出现第三方产品名**；唯一例外是「回程剥头」的匹配前缀（见 §10-F）。
4. 四协议透传落在**入站 handler 与出站 client 之间的适配层**，网关侧零改动；Gemini 现网**没有路由**，必须新增两条（见 §6）。
5. 0021 的 DROP 绑定「pg_dump → 临时库还原 → 三份行数比对」，**判定点 A/B/C 任一不过即中止**。

---

## 1 编号与职责

主库实测：`origin/main` 最大迁移是 `0018_call_logs_req_params.sql`。所以 0019 起号与主线**不撞号**（旧分支的 `0019_v2_facade.sql` / `0020_reconcile_blocked_status.sql` 未合入，作废、不 cherry-pick）。

| 编号 | 文件（`backend/internal/db/migrations/`） | 一句话职责 | 可回滚 | 幂等写法 |
|---|---|---|---|---|
| 0019 | `0019_v2_topology.sql` | 建四层图的三张表（`internal_models` / `model_links` / `subscriptions`）+ `external_models` 加绑定列 | 是（DROP 三表 / DROP 列） | `IF NOT EXISTS` / `ADD COLUMN IF NOT EXISTS` |
| 0020 | `0020_v2_billing_attribution.sql` | `billing_records` 只加归因列与索引（7 列），**纯 ADD、零 DROP、零 DML** | 是 | 同上 |
| 0021 | `0021_v2_drop_legacy.sql` | **唯一的 DROP 文件**：11 张退役表 + 3 个阻塞列 | **否**（必须走 §5 证据流程） | `DROP TABLE/COLUMN IF EXISTS` |
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

## 4 0021_v2_drop_legacy.sql（唯一不可回滚文件）

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

全部 `DROP TABLE IF EXISTS`。**保留不删**：`channel_protocols`、`protocol_error_stats`（不在批准清单；无写入方后是孤岛表，等阶段二后单独拍板清理），`watchlist_items` / `price_change_alerts`（价格监控，未批准下架）。

### 4.3 为什么 0021 是唯一 DROP 文件

`migrate.go` 每文件一个事务（`internal/db/migrate.go:16-70`），PG 的 DDL 事务性 → 0021 中途失败会整体回滚，不需要手工 restore（严格结论与 dump 的实际作用见 §12-①）。把所有不可逆动作集中在**一个**文件，等于「备份 → 证据 → 执行」只走一次，回滚点只有一个。

---

## 5 0021 三步证据流程（可直接照抄）

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

# ── 步骤 3a：迁移前，先存保留表基线 ─────────────────
psql -d $SRC_DB -tA -F'|' -f counts-retained.sql | sort > before-retained.txt

# ── 步骤 3b：执行迁移（服务启动或 migrate 命令触发）────
#   ……此处执行 migrate……

# ── 步骤 3c：迁移后比保留表（必须逐字相同）───────────
psql -d $SRC_DB -tA -F'|' -f counts-retained.sql | sort > after-retained.txt
diff before-retained.txt after-retained.txt && echo "RETAINED-OK"

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
| C | `RETAINED-OK` 不成立（保留表行数变少） | **立即中止**；若已 commit，用 dump 全量恢复 |
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
| A | 「旧分支关掉」 | **尚未关掉**：远端仍有 3 个 V2 分支（`feat/lingshu-v2-p1-usage-billing`、`-slim-migrate`、`-magpie-passthrough`），三个都含 `0019_v2_facade.sql`，usage-billing 另含 `0020_reconcile_blocked_status.sql` | main 只到 0018，编号**不冲突**；但「关掉」这个动作没人做。需要 hugo/spencer 删远端分支，并明确旧 0019/0020 永久作废。我不动远端分支 |
| B | 计划写「0020 摘 channel 维度」 | 摘列移到 0021，0020 纯 ADD | 不可回滚动作只应出现在一个文件，备份/证据流程只走一次 |
| C | 计划写「`pricing_mode` 扩 sale·subscription·usage」 | **不扩**，用户侧仍 `sale`/`cost`；成本来源另立 `cost_source` | `pricing_mode` 在现有代码里是**用户售价**口径（`users.pricing_mode` 同义）；把上游成本口径塞进去，盈亏页与账单页会读出两种含义 |
| D | 旧契约金额用「BIGINT 微元」 | 账本金额用 `NUMERIC(20,5)` 积分 | 与 `billing_records.cost_credits` / `credit_wallets.balance` 同型同精度，报表可直接加减；不引第二套金额单位（旧契约已作废） |
| E | 「一令牌一把网关凭据」 | **0019–0023 没有它的落点**，属计划缺口 | 本期用服务端单把 key（环境变量注入，**不进 sys_configs**）；一令牌一把需要新表，建议放 0024 或阶段二，**需负责人拍板** |
| F | 红线「配置与代码里不出现产品名」 | 唯一例外：**剥除回程头的匹配前缀**必须按上游真实头名写常量 | 不匹配就剥不掉，等于泄漏上游身份；该常量只在剥头白名单与注释里，界面/配置/日志回显仍不出现。**请负责人确认这条例外** |
| G | 任务书写 `external_models.bound_internal_model_ref` 且同时建 `model_links` | 绑定真源唯一的放在 `external_models`，`model_links` 只存画布坐标 | 同一关系两处可写，必然漂移（谁后写谁生效），拓扑与售卖会对不上 |
| H | 「四协议透传」当成现成能力 | 现网**没有 Gemini 路由**，且注册了 embeddings/completions | 必须新增 Gemini 前缀路由 + 下线两条端点；这是阶段一唯一的真新增路由 |
| I | 「迁移前 pg_dump 并校验」 | 沙盒 PG 拒连、`lingshu_dev` 无 `CREATEDB` | 证据流程必须在宿主由有权限的角色执行（临时库谁建需确认），沙盒里只能审 SQL 不能跑 |

本轮（交付轮）**未新增冲突**：A / E / F 三条仍待负责人拍板；Core 的 `feat/lingshu-v2-sync-config` 已把基址/超时/降级放进 `sys_configs`、密钥一律不进 —— 与 E 的处置一致，口径无偏离。

---

## 11 阶段一执行顺序（数据层）

1. `git fetch origin` → 从 `origin/main` 开分支 → **先跑 §1 撞号断言**。
2. 0019 → 0020 → 0022 → 0023 逐个应用，空库与存量库各一遍，二次执行新增 0 条。
3. 0021 **最后**，执行前完成 §5 三步证据并留产物。
4. 自测：账本 2 万条乱序灌入（含 30% 重复 `ledger_id`）→ 落库条数 = 去重后条数；重跑同一窗口 → 新增 0；窗口断言 `window_end = window_start + 60s`。

---

## 12 三条硬结论（一句话各一条，Gate 可直接写成断言）

**① 0021 中途失败能自动回滚到迁移前，但只在文件的事务内。**
`migrate.go:16-70` 把整份文件放进一个 `tx` 执行，PG 的 DDL 是事务性的（`DROP TABLE` / `DROP COLUMN` 可回滚），任一句报错 → `tx.Rollback()` → 库回到 0018，且 `applied_migrations` 不记 0021、重试安全；**dump 保护的不是「跑一半」，而是「文件已 commit 后发现删错/要取回历史渠道密钥名」这类事务之外的误判**——所以三步证据仍是必须的；反过来说，这五个文件里**不得出现** `CREATE INDEX CONCURRENTLY` / `VACUUM` / `CREATE DATABASE` 等不能在事务内执行的语句，否则迁移会直接失败（那才是真回滚不了）。

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

### 0021_v2_drop_legacy.sql

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
