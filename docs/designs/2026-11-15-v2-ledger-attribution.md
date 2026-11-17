# V2 阶段一 · 账本与归因字段对齐（含 0021 冲突清单）

日期：2026-11-15 · 分支 `feat/lingshu-v2-sync-config`
DDL 来源：`docs/lingshu-v2-p1-data-contract`（`7a414e8` + `7b4a86d`），已按原路径取入本分支。
口径原文：`docs/designs/2026-11-15-v2-p1-data-contract.md`（架构侧，本仓库不改它）。

## 1 账本表 `ledger_entries`（0022，九字段，代码已对齐）

| # | 列 | 类型 | 代码侧 |
|---|---|---|---|
| 1 | `id` | `BIGINT` PK | `LedgerEntry.ID`（0 → 存储层补雪花） |
| 2 | `ledger_id` | `TEXT` NOT NULL，唯一索引 `uq_ledger_entries_ledger_id` | 幂等键，`ON CONFLICT (ledger_id) DO NOTHING` 逐字对上 |
| 3 | `provider` | `TEXT ''` | `LedgerEntry.Provider` |
| 4 | `provider_account` | `TEXT ''` | `LedgerEntry.ProviderAccount` |
| 5 | `model` | `TEXT ''` | `LedgerEntry.Model` |
| 6 | `tokens_in` | `BIGINT` | `LedgerEntry.TokensIn` |
| 7 | `tokens_out` | `BIGINT` | `LedgerEntry.TokensOut` |
| 8 | `cost_credits` | `NUMERIC(20,5)` | `LedgerEntry.CostCredits`（写前 `money.Validate`） |
| 9 | `occurred_at` | `TIMESTAMPTZ` | 必须显式给（不许 `now()` 兜底） |

索引（0022 建）：`uq_ledger_entries_ledger_id`、`idx_ledger_entries_time`、`idx_ledger_entries_account`。
`credit_holds` 阶段一只建不写，本仓库代码零引用。

## 2 `billing_records` 归因列（0020，七列，代码已写七列）

`request_id` / `upstream_provider` / `upstream_account` / `upstream_protocol` / `upstream_model` /
`ledger_entry_id` / `cost_source`，写入侧为 INSERT 的第 26–32 个参数。

- `upstream_*` 缺失写 **NULL**（不猜）；`cost_source` 空归 `none`（列可空，但 `none` 是「没有成本口径」的显式值）；
- `ledger_entry_id` 阶段一没有匹配逻辑 → 恒 NULL，字段先留出（0020 建了部分唯一索引
  `uq_billing_ledger_entry ... WHERE ledger_entry_id IS NOT NULL`）；
- 非法 `upstream_protocol` / `cost_source` 在**写库前**报错并点名字段，不留到 CHECK 里当 PG 错误码；
- **读取侧（`recordCols`）暂不改**：加列会连带改 console 十余处 25 列行构造与统计口径，属后续切片。

## 3 `reconcile_windows`（0023）与同步窗口的对应关系

| 0023 列 | 同步轮次怎么用 |
|---|---|
| `window_start` PK | 60s 对齐的窗口起点（`ON CONFLICT (window_start) DO UPDATE`） |
| `window_end` | `window_start + 60s`，库层 `ck_reconcile_window_len` 断言 |
| `status` | `ok` / `blocked` / `error`（同步只写这三个；`alarm` 归对账作业） |
| `detail` JSONB | 写 `{"phase":"sync","pages","fetched","inserted","reason"}`，用 `||` **合并**不覆盖 |
| `checked_at` | 本轮记录时间 |
| `local_count` / `upstream_count` / `local_tokens` / `upstream_tokens` / `alarm_streak` | **同步不写**（对账作业的口径） |

窗口长度冲突与解法：拉取区间是 `[上次成功结束 - 120s, now)`（含重叠、长度可能 >60s），
而库里窗口必须整 60s → 落库时收敛为「拉取终点向下取整」那一格（`savedWindowFor`）。
更早的格子由以前的轮次写过；本轮重拉覆盖到的旧 `blocked` 用 `ClearFrom` 一起置 `ok`（同事务）。

## 4 与 0021 的冲突（**已上报，本切片未改，迁移文件一字未动**）

`0021_v2_drop_legacy.sql` 会 `DROP COLUMN billing_records.channel_key_id`，并 `DROP`
`sessions.channel_key_id`、`tokens.tag_id` 与 10 张渠道/标签/Auto 表。当前代码仍在用这些列/表：

| 位置 | 用法 | 0021 之后 |
|---|---|---|
| `billing/service.go` `insertOn` | INSERT 写 `billing_records.channel_key_id` | 插入报 **42703** `column "channel_key_id" ... does not exist` |
| `billing/service.go` `recordCols` | 查询选 `channel_key_id` | 任何账单查询报 42703 |
| `billing/query.go`、`stats.go` | 按密钥维度分组/索引 | 同上 |
| `console/*`、`gateway/*`、`channel/*`、`router/*` | 渠道名映射、调用日志、会话密钥维度 | 同上 |

非测试代码里 `channel_key_id` 共 **56 处 / 21 个文件**（`grep -rn channel_key_id internal --include=*.go | grep -v _test`），
`tag_id` 还有 5 处。这不属「加归因列」，属**渠道体系下线那一刀**，需要独立切片（连带删
`internal/domain/channel`、console 渠道页、`auto_models` 等），并且要同步改统计/账单页口径。

证据（真实库、全量 0001–0023 迁移后实跑）：

- 写入侧：`ERROR: column "channel_key_id" of relation "billing_records" does not exist (SQLSTATE 42703)`
  —— 用例 `TestAttribution_RealSchema_0021Conflict` 把它钉成确定性失败信息；
- 读取侧：`ListRecords(展开): count billing records: ERROR: column "channel_key_id" does not exist (SQLSTATE 42703)`
  —— 既有集成用例 `TestListRecords_RequestIDFilter_Integrated` / `..._GroupByRequest_UserIsolation_Integrated`
  在真库上会红（未设 `LLMGATEWAY_TEST_PG_DSN` 时仍跳过，所以默认门禁不受影响）。

结论：**0021 不能与当前代码同批执行**。要么先做「渠道体系下线」切片（摘写路径 + 读取列 + 渠道域代码），
要么把 0021 挪到那一刀之后。迁移文件按派单「只搬不改」，本切片未动一字。