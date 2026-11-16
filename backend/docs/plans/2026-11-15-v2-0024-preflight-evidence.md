# 0024 执行前证据三步（实测记录）

> 本文只记**本地临时库自测**这一组（B 组：711 MB / 1,625,951 行 / 32 表）。
> 另有一组外部在临时库上的演练结果（A 组：3.0 GB / 422 万行 / 26 表 777,630 行，tip `5bf38b8`），
> 见 `docs/designs/2026-11-15-v2-upgrade.md` §6.3；两组数字不合并、不互相换算。
> 两组都是临时库，不是生产数据。

环境：PostgreSQL 17.11（Debian 17.11-0+deb13u1），单机 127.0.0.1:5432，库 `qa_0024_pre` / `qa_0024_restore`（验证后已删除）。
数据集：空库跑迁移到 0023（23 个文件，字典序到 `0023_v2_reconcile.sql`），再灌样本数据。

样本规模：25 张表共 **1,625,951 行**；32 张表（含 0 行的 `ledger_entries` / `reconcile_windows` / `model_links` / `internal_models` / `subscriptions` / `credit_holds`）。
库大小：迁移到 0023 后 **711 MB**（含索引），0024 执行后 **668 MB**。

## 第 1 步：pg_dump

```
pg_dump -Fc -Z6 -d <qa_0024_pre> -f pre0024.dump
```

- 退出码 0
- 耗时 **4.83 s**
- 产物 **37,152,782 字节**（35.4 MiB，custom 格式 + zlib，仅数据与 DDL，不含索引内容）

## 第 2 步：临时库还原

```
createdb qa_0024_restore
pg_restore --no-owner --no-privileges -j1 -d <qa_0024_restore> pre0024.dump
```

- 退出码 0
- 耗时 **22.74 s**（单线程）
- 还原后：32 张 BASE TABLE、`applied_migrations` 23 行

## 第 3 步：行数比对（dump 前 vs 还原后）

32 张表逐表比对，**差异 0**（`diff` 无输出）。15 张保留表：

| 表 | dump 前 | 还原后 |
| --- | --- | --- |
| users | 20000 | 20000 |
| tokens | 40000 | 40000 |
| billing_records | 500000 | 500000 |
| credit_wallets | 20000 | 20000 |
| credit_flows | 200000 | 200000 |
| sessions | 100000 | 100000 |
| call_logs | 400000 | 400000 |
| external_models | 20000 | 20000 |
| sys_configs | 121 | 121 |
| announcements | 200 | 200 |
| audit_logs | 200000 | 200000 |
| channel_protocols | 40 | 40 |
| protocol_error_stats | 20000 | 20000 |
| watchlist_items | 5000 | 5000 |
| price_change_alerts | 5000 | 5000 |

其余 10 张退役表（`channels` 30 / `channel_keys` 300 / `channel_models` 3000 / `channel_key_events` 20000 /
`channel_model_events` 20000 / `channel_tags` 30 / `semantic_tags` 30 / `probe_logs` 50000 / `auto_models` 200 /
`auto_model_members` 2000）同样两侧一致；空表两侧都是 0。合计 1,625,951 与 1,625,951。

## 附：0024 自身在同库执行（同一证据链）

- `psql -v ON_ERROR_STOP=1 -f 0024_v2_drop_legacy.sql`：退出码 0，耗时 **0.13 s**
- 15 张保留表行数逐表不变
- 10 张退役表残留 **0**；`billing_records.channel_key_id` / `sessions.channel_key_id` / `tokens.tag_id` 残留 **0**
- 表数 32 → 22

## 口径说明

- 数据是合成样本（固定 25 张表规模），用于验证「dump → 还原 → 行数一致」这条链路与耗时量级，
  不代表生产数据分布；生产大文本字段更多，dump 体积与还原耗时按实际 GB 线性外推。
---

# 复跑（迁移链改成 0019b 之后，2026-11-16）

上一组证据取在 `5bf38b8`，那时链上还是 `0019_v2_facade`。之后迁移集合变了两处：
`0019_v2_facade` 拆成 `0019b_v2_passthrough_credentials`（多一张 `magpie_keys` 表、少 `reconcile_windows` 旧形状），
`0023` 增加旧形状自愈、`0023c` 补齐计数归 NULL。**迁移集合变了，0024 执行前的证据必须重跑**（Owl/Gate 都点了这条）。

- 环境：PostgreSQL 17.11，临时集群 `127.0.0.1:54591`，库 `ev_pre` / `ev_restore`（验证后删除）。
- 链状态：26 个迁移应用到 `0023c_v2_reconcile_counts_backfill.sql`（`applied_migrations` 26 行）。
- 规模：33 张 BASE TABLE / **1,611,057 行** / 380 MB。

## 第 1 步：pg_dump

- `pg_dump -Fc -Z6 -d ev_pre -f pre0024.dump`：退出码 0，耗时 **4.09 s**，产物 **23,342,377 字节**。

## 第 2 步：临时库还原

- `pg_restore --no-owner --no-privileges -j1 -d ev_restore pre0024.dump`：退出码 0，耗时 **15.43 s**。
- 还原后 33 张 BASE TABLE、`applied_migrations` 26 行。

## 第 3 步：行数比对

- 33 张表逐表比对，**差异 0**（`diff` 无输出）；两侧合计均 1,611,057 行。

| 表 | dump 前 | 还原后 | 0024 执行后 |
| --- | --- | --- | --- |
| billing_records | 500000 | 500000 | 500000 |
| call_logs | 400000 | 400000 | 400000 |
| audit_logs | 200000 | 200000 | 200000 |
| credit_flows | 200000 | 200000 | 200000 |
| sessions | 100000 | 100000 | 100000 |
| tokens | 40000 | 40000 | 40000 |
| users | 20000 | 20000 | 20000 |
| credit_wallets | 20000 | 20000 | 20000 |
| external_models | 20000 | 20000 | 20000 |
| watchlist_items | 5000 | 5000 | 5000 |
| price_change_alerts | 5000 | 5000 | 5000 |
| model_links | 3000 | 3000 | 3000 |
| internal_models | 2000 | 2000 | 2000 |
| subscriptions | 200 | 200 | 200 |
| announcements | 200 | 200 | 200 |
| channel_protocols | 40 | 40 | 40 |
| sys_configs | 1 | 1 | 1 |

退役 10 张（0024 前）：`channels` 30 / `channel_keys` 300 / `channel_models` 3000 / `channel_key_events` 20000 /
`channel_model_events` 20000 / `channel_tags` 30 / `semantic_tags` 30 / `probe_logs` 50000 / `auto_models` 200 /
`auto_model_members` 2000 = 95,590 行；0024 后全部为 0（表不存在）。1,611,057 − 95,590 = 1,515,467，与实测一致。

## 0024 自身执行（同库）

- `psql -v ON_ERROR_STOP=1 -f 0024_v2_drop_legacy.sql`：退出码 0，耗时 **0.10 s**，日志里 `error|warning` 0 行。
- 保留表逐表行数**不变**（脚本逐表比较，PASS）；表数 **33 → 23**（删 10 张）。
- 三列残留 0：`billing_records.channel_key_id` / `sessions.channel_key_id` / `tokens.tag_id`。
- 0019b 的两个对象**仍在**（这是与旧证据最大的差别）：`magpie_keys` 1 张表、`external_models.magpie_model` 1 列。
- 保留对象真查询可用：`billing_records` / `sessions` / `external_models` / `magpie_keys` 均能直接 SELECT。

## 执行前四条探测（同一库、0024 之前）

`scripts/acceptance/v2-preflight/probe-1..4.sql` 全部输出 **0 行**（探测① 记录层、② 旧对象层、③ 窗口形状、④ 按记录反查对象）。
注：探测① 的白名单此前缺 `0019b`，会在干净库上误报 1 行；已由 `ca35a69` 补上（`TestV2Probe_RecordWhitelistMatchesMigrationFiles` 守着）。

## 启动冒烟（同一终态库，真二进制）

- `-migrate-only`：rc=0，`applied_migrations` 27 行，最后一条 `0024_v2_drop_legacy.sql`。
- 起服务：`/healthz` 200，日志 `42P01|42703|panic` 计数 **0**（旧版本这里是「启动即 exit 1」）。
- 原来查 `channels` / `channel_keys` 的读写面全部 200：成本统计、成本线、账单列表/统计、统计看板/实时、
  会话列表、盈亏、对账窗口、模型列表、令牌列表、系统配置。
- 已摘接口按 410 返回（渠道族 / Auto 模型 / 标签），不是 404/500。
