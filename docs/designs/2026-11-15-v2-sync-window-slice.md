# V2 阶段一 · 第二切片：同步窗口 + 账本九字段 + 归因列

日期：2026-11-15 · 分支 `feat/lingshu-v2-sync-config`
上一片：`docs/designs/2026-11-15-v2-sync-config-slice.md`（同步器骨架 / sys_configs 三项 / 账本占位）
字段对齐清单：`docs/designs/2026-11-15-v2-ledger-attribution.md`

## 1 本切片做了什么

| 件 | 落点 | 一句话 |
|---|---|---|
| 同步窗口 | `backend/internal/domain/upstream/window.go` | 60s 窗口 + 重叠 2 窗；offset 翻页不持久化；单轮 20 页上限，超限置 blocked + 告警；整轮一事务 |
| 账本真实字段 | `backend/internal/domain/upstream/ledger.go` | 九个具名字段 + `ledger_entries` SQL 实现（`ON CONFLICT (ledger_id) DO NOTHING`）+ offset 分页 |
| 归因列对接 | `backend/internal/domain/billing/{attribution.go,service.go}` | `billing_records` 六个归因列进 INSERT（第 26–31 参数）；枚举校验 + 缺失留 NULL |

**没有做**（口径未定，别提前动）：网关路由表、handler 请求处理链、四协议透传、Embeddings、
`billing_records` 读取侧（`recordCols` 不变）、统计口径。

## 2 窗口口径

- 长度 60s（与对账窗口同源）；
- 区间 `[上次成功结束 - 120s, now)`：重叠 2 窗吸收迟到数据；
- 落库水位线按 60s **向下对齐**（`WindowRecord.Watermark()`），下一轮起点只早不晚；
- **offset 不持久化**：每轮从 0 翻。把 offset 存起来当进度，一旦漂移就会「重复算」或「永久漏」；
- 单轮 ≤ 20 页；超限 → 回滚 + 窗口置 `blocked` + ERROR 告警。
  因为 offset 不持久化，积压**不会自愈**（每轮都从 0 重翻）——这正是必须告警的原因；
- **整轮一事务**：所有页同事务写入，成功才提交；失败/超限整轮回滚（不留半条账）；
- 窗口状态**不在**账本事务内写：blocked/error 恰在回滚之后才需要留证据；
- 首轮无水位线只拉 1 窗（没有历史线时空拉 3 窗没意义，且更容易撞页上限）；
- 「空页 + has_more」判定为数据源缺陷，直接报错（否则死循环）。

窗口状态落库（架构侧 0023 的 reconcile 窗口表）尚未落地，暂用 `MemoryWindowStore`：
**重启丢水位线 → 首轮重拉 1 窗，由 `ledger_id` 幂等键兜住，不会重复计数。**

## 3 账本与归因（字段表见对齐清单）

- 九列一字不加、一字不改名；`occurred_at` 必须显式给（不许 now() 兜底，猜时间会把账算错位）；
- 幂等只靠 `ledger_id` 唯一索引 + `ON CONFLICT DO NOTHING`；`RowsAffected` 之和 = 本次真实新增；
- 归因六列：`request_id / upstream_provider / upstream_account / upstream_protocol / upstream_model / cost_source`；
  `upstream_*` 缺失一律 NULL（**不猜**），`cost_source` 空归 `none`；
- 非法协议/成本来源在写库前由 `attributionArgs` 拦下并点名字段（不留到 CHECK 约束里当 PG 错误码）；
- 读取侧暂不接这六列（会连带改 console 十余处行构造与统计口径），属后续切片。

## 4 本切片测点

| 用例 | 断言 |
|---|---|
| `TestRounder_OverlapDedupeByLedgerID` | 第二轮重叠窗口把旧账重拉一遍 → 只插入新的一条，库内总数不翻倍；起点确为水位线−120s |
| `TestRounder_OffsetPagingBoundary` | offset 序列 `0 → 2 → 4`；`has_more=false` 立即停（整页也不多翻） |
| `TestRounder_MaxPagesBlocks` | 恰好翻 20 页停在 blocked；整轮回滚（commits=0、stored=0）；窗口落 blocked 记录；不推进水位线；20 页正好拉完不算 blocked |
| `TestRounder_FetchErrorRollsBack` | 拉取失败 → 回滚 + 窗口记 error + 透传原错误 |
| `TestRounder_EmptyPageWithHasMoreIsError` | 空页 + has_more → 报错不死循环 |
| `TestWindowFor_OverlapTwoWindows` | 窗口起点/终点与水位线对齐口径 |
| `TestLedgerStore_UpsertIdempotent_DuplicateNotCounted`（真实库，自建临时库） | 同一 `ledger_id` 重复写入只算 1 条、库内 3 条；offset 分页边界 |
| `TestBuildLedgerInsert_IdempotentShape` | SQL 含 `ON CONFLICT (ledger_id) DO NOTHING`；两行占 18 参数；ID 自动补 |
| `TestLedgerEntry_Validate` | 幂等键/发生时刻/token 负数/金额 NaN/超 NUMERIC(20,5) |
| `TestAttributionArgs_Normalization` / `TestRecord_InsertsAttributionColumns` | 缺失写 NULL、cost_source 归一 none、非法枚举点名字段报错；归因值真进 INSERT 第 26–31 参数 |

变异验证（改回缺陷实现确认变红）：取消 120s 重叠、放宽页上限、blocked 不回滚、offset 按 1 推进、
归因列序错位、跳过协议校验、去掉 `ON CONFLICT`（真实库）——七项全部变红。

## 5 与架构侧 DDL 的对齐状态

- 远端 22 个分支里**没有任何一条**带 V2 的 0019–0023 迁移（全部 ls-tree 检查过），
  当前仓库最高迁移仍是 `0018`；因此本切片**不写生产迁移**，只按派单给的字段表实现读写，
  并在 `docs/designs/2026-11-15-v2-ledger-attribution.md` 记录对齐清单。
- 需要架构侧确认两点：①账本表名是否就是 `ledger_entries`；②窗口状态表的列名
  （决定了 `WindowStore` 的 SQL 实现与 blocked 落库形态）。这两点一确认就能接线。