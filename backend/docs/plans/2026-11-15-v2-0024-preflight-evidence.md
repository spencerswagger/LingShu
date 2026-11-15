# 0024 执行前证据三步（实测记录）

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