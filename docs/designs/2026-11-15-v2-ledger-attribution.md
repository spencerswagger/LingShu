# V2 阶段一 · 账本九字段与归因列（对齐清单，不是迁移文件）

日期：2026-11-15 · 状态：**待架构侧 0019–0023 DDL 落地后按此核对**
本文件只记录「Go 代码已经按什么列名/列序读写」，不作为可执行迁移。迁移归架构侧
0019–0023 统一落地；本仓库当前 `internal/db/migrations` 最高仍是 `0018`。

## 1 账本表 `ledger_entries`（九字段，本期一列不加）

| # | 列 | 类型 | 可空 | 默认 | 代码侧 |
|---|---|---|---|---|---|
| 1 | `id` | BIGINT | NOT NULL | — | `LedgerEntry.ID`（写入方留 0 时由存储层生成雪花 ID） |
| 2 | `ledger_id` | TEXT | NOT NULL（**唯一**，幂等键） | — | `LedgerEntry.LedgerID`，`ON CONFLICT (ledger_id) DO NOTHING` |
| 3 | `provider` | TEXT | NOT NULL | `''` | `LedgerEntry.Provider` |
| 4 | `provider_account` | TEXT | NOT NULL | `''` | `LedgerEntry.ProviderAccount` |
| 5 | `model` | TEXT | NOT NULL | `''` | `LedgerEntry.Model` |
| 6 | `tokens_in` | BIGINT | NOT NULL | `0` | `LedgerEntry.TokensIn` |
| 7 | `tokens_out` | BIGINT | NOT NULL | `0` | `LedgerEntry.TokensOut` |
| 8 | `cost_credits` | NUMERIC(20,5) | NOT NULL | `0` | `LedgerEntry.CostCredits`（写前过 `money.Validate`） |
| 9 | `occurred_at` | TIMESTAMPTZ | NOT NULL | — | `LedgerEntry.OccurredAt`（**必须显式给**，不用 now() 兜底） |

索引：`(ledger_id)` 唯一、`(occurred_at DESC)`、`(provider, provider_account, occurred_at DESC)`。

POC 后的扩展列（request_id / status / 缓存读 / 思考 token / 单价 / 币种）**本期不加**。

表名：`ledger_entries`（实施计划 §3 的 0022 口径）。若架构侧 DDL 用了别的表名/列名，
这就是**需要回报的口径冲突**——代码里只有 `upstream.LedgerTable` 与 `ledger.go` 的列清单两处要改。

## 2 `billing_records` 归因列（纯 ADD，无 DROP、无 DML）

| 列 | 类型 | 语义 | 代码侧 |
|---|---|---|---|
| `request_id` | TEXT NULL | 网关请求链路 ID | `Record.RequestID`（空 → NULL） |
| `upstream_provider` | TEXT NULL | 上游供应商（回程头） | `Record.UpstreamProvider`（**缺失留 NULL，不猜**） |
| `upstream_account` | TEXT NULL | 上游账号（回程头） | `Record.UpstreamAccount`（同上） |
| `upstream_protocol` | TEXT NULL + CHECK | 四协议之一 | `upstream.AttributionProtocols()` |
| `upstream_model` | TEXT NULL | 上游真实模型名 | `Record.UpstreamModel` |
| `cost_source` | TEXT NOT NULL DEFAULT `'none'` + CHECK | `ledger` / `amortized` / `none` | `billing.CostSources()` |

`upstream_protocol` 的 CHECK 取值域（逐字一致，别加 Embeddings）：

```
CHECK (upstream_protocol IS NULL OR upstream_protocol IN
       ('openai-chat','openai-responses','anthropic-messages','gemini'))
```

`cost_source` 的 CHECK 取值域：

```
CHECK (cost_source IN ('ledger','amortized','none'))
```

写入侧：`billing.SqlStore.insertRecord` 的 INSERT 追加这 6 列（第 26–31 个参数），
非法枚举在写库前由 `attributionArgs` 拦下并点名字段。
读取侧：`recordCols` **暂不变**（明细展示要连带改 console 的十余处行构造 + 统计口径，
属后续切片），因此这些列当前只写不读——不影响既有统计口径。

## 3 同步窗口与账本写入的耦合口径

- 窗口长度 60s；本轮区间 `[上次成功结束 - 120s, now)`（重叠 2 窗吸收迟到数据）；
- offset 翻页**不持久化**（每轮从 0 翻），单轮 ≤ 20 页；
- **整轮一事务**：所有页写在同一事务，成功才提交；失败/超限整轮回滚；
- 超限 → 窗口置 `blocked` + ERROR 告警（offset 不持久化 ⇒ 积压不会自愈，必须告警）；
- 窗口状态**不在**账本事务内写：blocked/error 恰在回滚之后才需要留证据。

窗口状态表（架构侧 0023 的 reconcile 窗口表）落地前，`upstream.MemoryWindowStore` 占位：
重启丢水位线 → 首轮重拉 1 窗，由 `ledger_id` 幂等键兜住，不会重复计数。