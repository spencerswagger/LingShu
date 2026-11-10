# 统计/账单性能改造（0020）：日级预聚合 + 进程内缓存 + 去正则

> 2026-11-10 · 分支 `feat/daily-stats-rollup` · 后端（Core）
> 压测数据：本地 PG 17.11，8 核 / 96GB，`shared_buffers=8GB`，`billing_records` **3000 万行**
>（`scripts/bench-stats/seed.sql`，top 用户占 40%，tokens 三段与实体列同值）。

## 1. 做了什么（三段）

| 段 | 内容 | 为什么 |
|---|---|---|
| ① 日级预聚合 `user_daily_stats` | 结算事务内 upsert 增量；启动时按天回填（分批/断点/幂等）；看板三档积分与区间合计改读聚合表 | 明细表上「一个人一个月」的扫描量 ∝ 该用户行数，索引/分区都救不了（见 `notes/pg-scale-bench-2026-11-07.md`） |
| ② 进程内 5 分钟缓存 + singleflight | `internal/pkg/statscache`（~160 行，stdlib only，无新依赖） | 同一份聚合多人同时看，5 分钟滞后可接受；冷启动只让一个请求打 DB |
| ③ 去正则 + 默认 30 天 + 会话索引 | `billing_records` 新增三段 token 实体列；cost/stats 不再逐行 JSONB 正则；默认窗口 30 天；`sessions(user_id, closed, last_active DESC)` | 30M 行下每行 3 次正则 = 主耗时；不带时间窗 = 全表扫且 Total 随数据量漂移 |

## 2. 口径（与改造前逐位一致）

- 切日：`Asia/Shanghai` 自然日（`day` 为 DATE）。
- 只统计 `status='completed'`；`requests` = 行数；`credits` = `SUM(credits_consumed)`；
  `cost_credits` = `SUM(cost_credits)`；三段 token = 实体列之和（缺失/非法一律 0，与原正则守卫同口径）。
- 看板「今日 / 昨日 / 本月」= `day = today` / `day = today-1` / `day >= 本月 1 日`。
- 探针排除（cost/stats）口径不变。

等价性校验（3000 万行，实测通过）：聚合表逐日逐用户 vs 明细直算，`days_compared=9078, mismatch=0`；
`/me/summary` 三档积分与三段 token 与改造前 handler 结果一致（今日 8501.04 / 昨日 661266.48 / 本月 6643311.84）。

## 3. 写入两路，互不重复

- **实时**：`billing.Service.settleTx` 内 `SAVEPOINT daily_stats` + `ON CONFLICT (user_id, day) DO UPDATE`
  **增量累加**。聚合写失败只 `ROLLBACK TO SAVEPOINT`，账单照常提交（派生数据不得反向回滚资金）。
  非事务降级路径（`settlePlain`）用可选能力接口 `dailyStatsUpserter` 探测，失败只记日志。
- **历史**：`internal/db.BackfillStats`。两个阶段各自断点（`stats_backfill_state`）：
  token 列按 **主键游标**（keyset，`id > last_id`）每批 5000~20000 行；
  日聚合按 **自然日** 每批 7 天，`ON CONFLICT DO UPDATE` **覆盖写**（同一天重跑结果不变）。
- **幂等 / 可重入**：断点 `done_at` 盖章后重跑直接跳过；`-reset` 清断点从最早数据重算。
  「不覆盖已冻结的过去」由「今天之前的天由回填覆盖、今天由实时累加」保证；多副本滚动重启时今日那格
  理论上有竞争（见 §7 已知边界）。

## 4. 缓存（`internal/pkg/statscache`）

| 项 | 取值 |
|---|---|
| 实现 | 进程内 `map[string]entry[T]` + `sync.Mutex` + 每键 `inflight` 合并（自己写，无 x/sync） |
| 键 | `/auth/me/summary` → `me-summary:<user_id>`；cost/stats → `cost-stats:<fromUnix>:<toUnix>`（**所有键都含隔离维度**） |
| TTL | 300s（`statscache.DefaultTTL`）；可注入时钟，单测不真等 |
| 并发 | 同键并发只回源一次，其余等这次结果；`Loads` 计数即「打 DB 次数」 |
| 空结果 | **缓存**（真没数据也是合法结果，否则无数据账号每次都打穿） |
| 错误 | **不缓存**（错误不放大，下一次请求重试） |
| 失效 | 过期即删；写路径可 `Invalidate(key)`（`Dev.InvalidateMeSummary`）；`Purge()` 供配置变更/手工清 |
| 观测 | 响应头 `x-stats-cache: hit/miss`（miss 也包含「合并到别人的回源」） |

跨用户串数据的反向用例：`TestDevSummary_CacheIsolatesUsers`（A/B 两用户各自回源、第二轮命中各自的值、
断言 A≠B 且聚合表不再被查）；`TestCache_KeysAreIsolated`（包级）。

## 5. 实测（3000 万行，iters=5，p50/p95）

| 接口 | 改造前 | 改造后（冷，缓存空） | 改造后（热，命中缓存） |
|---|---|---|---|
| `GET /api/v1/auth/me/summary` | p50 **2289.4ms** / p95 2316.3ms | p50 **0.6ms** / p95 0.9ms | p50 **0.3ms** / p95 0.4ms |
| `GET /api/v1/admin/cost/stats` | p50 **129246ms** / p95 141942ms | p50 **27904ms** / p95 37781ms | p50 **0.1ms** / p95 0.3ms |

采样方式：`PERF_BENCH_DSN=... go test ./internal/domain/console -run TestPerfStatsDashboard`
（同一数据集、同一机器、每轮前 `Purge()` 得到冷启动口径；legacy 用改造前 SQL 原文直打 DB）。

**回填**（3000 万行、91 天、100 用户）：wall **55.6s**，写入 9078 行，进程峰值 RSS **10.5MB**
（采样：回填进程 `/proc/self/status` 的 `VmHWM`，不是机器峰值）。
本次 token 列阶段 0 行——seed 直接写了实体列；真实库上该阶段 ≈ 一次 30M 行分批 UPDATE（每批独立事务，可中断续跑）。

## 6. EXPLAIN（10/10 走索引）

`billing_records` 3000 万行 / `sessions` 50 万行 / `user_daily_stats` 9078 行。

| # | 查询 | 访问路径 | 耗时 |
|---|---|---|---|
| A1 | 本人三档（聚合表） | Bitmap Index Scan `user_daily_stats_pkey` (11 行) | 1.9ms |
| A2 | 本人 30 天区间合计 | Bitmap Index Scan `user_daily_stats_pkey` (30 行) | 2.4ms |
| A3 | 本人近 1 分钟吞吐 | Index Scan `idx_billing_call_time` | 0.02ms |
| A4 | 本人 30 天账单统计栏 | Bitmap Index Scan `idx_billing_user_time` (400 万行) | 3.5s |
| A5 | 账单列表分页 | Index Scan `idx_billing_user_time` | 0.12ms |
| A6 | 会话列表分页（`closed=false`） | Index Scan `idx_sessions_user_closed_active` | 7ms |
| A7 | 会话列表 count | Index **Only** Scan `idx_sessions_user_closed_active`（0 heap fetch） | 5.7ms |
| A8 | 会话列表不按 closed 过滤 | Index Scan `idx_sessions_last_active` | 3.5ms |
| A9 | 全站按天趋势 | Index Scan `idx_user_daily_stats_day` (2978 行) | 2.3ms |
| A10 | 某用户某天账单 | Bitmap Index Scan `idx_billing_user_time` (9.1 万行) | 72ms |

对照：改造前 cost/stats = `Parallel Seq Scan` 3000 万行 + 每行 3 次正则（31.6s，纯聚合部分）；
改造后 = `Bitmap Index Scan on idx_billing_call_time`（1000 万行窗口）+ 实体列，正则与 JSONB 逐行解析全部消失。

完整计划：`explain.out`（本轮归档在工作区 `generated/pgbench/`，重跑命令见 `scripts/bench-stats`）。

## 7. 已知边界 / 未做

1. **cost/stats 冷启动仍慢（≈28s / 1000 万行窗口）**：瓶颈是 30 天窗口的实体 join + `array_agg(DISTINCT)` 排序
   （外部归并 1.4GB），不是正则。要再降一档必须按「实体 × 天」预聚合（0021+），本轮不做。
   热路径由 5 分钟缓存兜住（0.1ms）。
2. **多副本今日竞争**：回填覆盖到「今天」时，若另一副本同时在跑实时累加，今日那格可能少算/多算一次；
   单副本滚动重启无此问题。修法：回填只到「昨天」，今日一律由实时路径维护（需改一处边界，待拍板）。
3. **缓存是进程内的**：多实例之间不一致（各自 5 分钟）。这是「不引入 Redis」的既定代价。
4. `billing/stats.go` 与 `console/admin.go` 里还有几处 `tokens->>` 逐行 JSONB 提取（非正则），
   未在本轮改动范围（口径相同，但与实体列并存 → 建议后续统一）。

## 8. 门禁

`gofmt`（改动文件）、`go vet ./...`、`go test -race`（CI 口径 29 包）全绿；`tests/` e2e 仍按 CI 的
`continue-on-error` 处理（脚手架未装配渠道密钥服务，历史已知）。
