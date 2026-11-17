# V2 阶段一 · 第三切片：DDL 合流 + 窗口状态持久化

日期：2026-11-15 · 分支 `feat/lingshu-v2-sync-config`
上一片：`docs/designs/2026-11-15-v2-sync-window-slice.md`（窗口/账本/归因的代码侧）
本片加：架构侧 0019–0023 迁移取入 + `reconcile_windows` 持久实现 + 真实迁移下的写入侧验证

## 1 DDL 取入方式

`git cherry-pick 7a414e8 7b4a86d`（架构侧 `docs/lingshu-v2-p1-data-contract` 的两次提交），**无冲突**。
取入 5 个迁移文件（`backend/internal/db/migrations/0019_v2_topology.sql` … `0023_v2_reconcile.sql`）
与 `docs/designs/2026-11-15-v2-p1-data-contract.md`，**文件路径与内容一字未改**。

## 2 窗口状态：`MemoryWindowStore` → `SQLWindowStore`

| 能力 | 实现 |
|---|---|
| 水位线 | `SELECT max(window_end) … WHERE status IN ('ok','alarm')`（blocked/error 不推进） |
| blocked 落库 | `INSERT … ON CONFLICT (window_start) DO UPDATE`，`detail` 用 `||` 合并 |
| 重启不丢 | 状态在库里；`MemoryWindowStore` 降级为单测替身 |
| 重启不静默 | 每轮先读 `blocked` 窗口：`BlockedPending>0` 时 WARN 告警，并把拉取起点**前移到最旧 blocked 窗口**（否则没有水位线时永远补不到它） |
| 成功补平 | 成功轮的窗口行与账本**同事务**写；`ClearFrom` 区间内的旧 blocked 一并置 `ok`（同事务，避免假告警留存） |
| 60s 断言 | 代码侧 `WindowRecord.Validate()`；库侧 `ck_reconcile_window_len`（两层都验，见测试） |

同步**不写** `local_count/upstream_count/*_tokens/alarm_streak`：那是对账作业的口径，同步写会污染比对。

## 3 真实迁移下的验证（临时库 + `db.Migrate` 全量 0001–0023）

| 用例 | 断言 |
|---|---|
| `TestLedgerStore_UpsertIdempotent_DuplicateNotCounted`（真实表，不再手搓 DDL） | 双写同一 `ledger_id` 只计 1 条；库内 3 条；offset 分页 0/2/3 边界 |
| `TestSQLWindowStore_BlockedSurvivesRestart` | blocked 落 `reconcile_windows`；新 store 实例仍读到（含 detail 里的原因）；库内窗口长度 = 60s；blocked 期间无水位线；补平后 blocked 清 ok、水位线 = 成功窗口 `window_end`、老行 detail 保留 |
| `TestSQLWindowStore_RejectsMisalignedWindow` | 代码侧拦非整 60s；**绕过代码直接写 SQL 也被 `ck_reconcile_window_len` 拒绝**（SQLSTATE 23514） |
| `TestAttribution_RealSchema_WriteAndReadBack` | 七列在真实 schema 上可写可反查；缺失 NULL / `cost_source` 归一 `none`；**改价后归因列不变**（历史行自解释）；非法枚举写库前报错 |
| `TestAttribution_RealSchema_0021Conflict` | 全量迁移（含 0021 删列）后写入必报 42703 —— 把冲突钉成事实、当地雷用 |

变异验证（本轮至少三条必验，全部变红）：

1. blocked 不落库 → `TestRounder_BlockedSurvivesRestartAndIsReloaded` 红（重启后看不到积压）；
2. 落库窗口不做 60s 对齐 → `TestSavedWindowFor_AlignedToSixtySeconds` 与重叠去重用例红；
3. 成功补平后不清理 blocked → 内存用例与**真实库** `TestSQLWindowStore_BlockedSurvivesRestart` 双红；
4. 跳过代码侧 60s 校验 → 真实库被 `ck_reconcile_window_len` 拒（证明断言在库里，不只是文档）；
5. 归因列序错位（`upstream_model` ↔ `ledger_entry_id`）→ 参数断言与真实库用例红。

## 4 遗留与冲突

- **0021 与当前代码冲突**（详见 `2026-11-15-v2-ledger-attribution.md` §4）：`channel_key_id`（56 处 / 21 文件）
  与 `tag_id` 仍在用；0021 一旦执行，账单写入与查询立刻 42703。本切片按红线**未改迁移、未摘写路径**，等口径。
- `credit_holds` 阶段一无写入方（0022 文件头已声明），本仓库代码零引用。
- 读取侧 `recordCols` 仍不含七列；统一读取口径属后续切片。
- 网关路由表、handler 请求处理链、四协议透传：本切片零改动（等口径）。