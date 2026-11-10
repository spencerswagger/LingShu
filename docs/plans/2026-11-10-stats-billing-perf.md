# 统计 / 账单接口性能改造：落地方案（三段串行）

> Date: 2026-11-10
> 范围：后端。明细保留 12 个月、聚合永久；接口保持向后兼容（前端不改也能用）。
> 状态：**方案已出，压测前置条件未满足（见 §0）**。

---

## 0. 前置核实结果（阻塞压测，不阻塞结构设计）

### 0.1 3000 万行真实数据：本机不可得

| 检查项 | 结果 |
|---|---|
| 本机 PG 实例 | 仅 `127.0.0.1:5432`（另有 MySQL 3306、Redis 6379、Meili 7700，与被测表无关） |
| 含 `billing_records` / `call_logs` 的库 | `lingshu_dev`、`lingshu_test` 两个；`lingshu_staging` 无业务表 |
| 实际行数 | 两库均为 **0**（`pg_stat_user_tables.n_live_tup = 0`，`pg_class.reltuples = -1` 表示从未 analyze） |
| 全库体积 | 最大的业务库为另一个项目的 `yanshu_test` 374 MB；灵枢三库均 < 20 MB |
| 现成数据文件 | `/data` 下无 SQL/CSV/dump |

结论：**无法用真实数据出 P50/P95、回填耗时、缓存命中率、`EXPLAIN` 裁剪证据**。
需要二选一：① 提供可读副本的 DSN（或 `pg_dump` 片段）；② 明确同意用合成数据补测，并在 PR 中与真实数据结果分开标注。

### 0.2 现状与任务描述不一致：库里没有分区

任务假设「现有按月分区在 `created_at` 上」。实测：

| 检查项 | 结果 |
|---|---|
| `pg_class.relkind` | `billing_records` = `r`、`call_logs` = `r`（普通表，非分区表） |
| 迁移目录 `PARTITION BY` | 19 个迁移文件中 **0 处** |
| `\d billing_records` | 无分区子表、无 `PARTITION BY` |

即：本仓库 schema 里两张表**都未分区**，不存在「按月分区建在 `created_at` 上」的结构。
若生产库确有此结构（例如由外部 DBA 手工建），需要生产 DDL 原文，才能给出「改造前 `Partitions removed`」的基线；否则第 ③ 段只能给出「从无分区 → 按 `call_time` 分区」的前后对比。

---

## 1. ① 日级预聚合 `user_daily_stats`

### 1.1 表结构（新迁移 `0020_user_daily_stats.sql`）

```sql
CREATE TABLE IF NOT EXISTS user_daily_stats (
    user_id        BIGINT      NOT NULL,
    day            DATE        NOT NULL,             -- 按 Asia/Shanghai 切日
    requests       BIGINT      NOT NULL DEFAULT 0,
    input_tokens   BIGINT      NOT NULL DEFAULT 0,
    output_tokens  BIGINT      NOT NULL DEFAULT 0,
    cache_read     BIGINT      NOT NULL DEFAULT 0,
    credits        NUMERIC(20,5) NOT NULL DEFAULT 0,
    cost_credits   NUMERIC(20,5) NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, day)
);
CREATE INDEX IF NOT EXISTS idx_user_daily_stats_day ON user_daily_stats(day);
```

- 聚合口径与现有看板一致（三段 token + 积分），不引入新计费口径。
- 保留策略：**永久**（不建 TTL/淘汰）。

### 1.2 写入路径：upsert（幂等）

落点是资金写收口 `billing.Service.settleTx`（记账事务内），新增一条
`INSERT … ON CONFLICT (user_id, day) DO UPDATE SET requests = user_daily_stats.requests + EXCLUDED.requests, …`。
与账单写入同事务，避免「有账单无统计」；重复投递同一笔不会重复计数（同一 billing 只 settle 一次）。

### 1.3 历史回填（分批 / 断点 / 幂等）

`backend/scripts/backfill-daily-stats`（与 0019 同一套工程要求）：

- 按 `billing_records.call_time` 的**日边界**推进，不用主键游标（聚合按天天然可续跑）；
- 断点写 `backfill_checkpoints(name='user_daily_stats', last_id=已完成的 day 序号)`；
- 盖章语句：`INSERT … SELECT … GROUP BY user_id, day ON CONFLICT DO UPDATE`（重跑覆盖同一天，不重复累加）；
- 单批 = 若干天（默认 7 天/批），每批独立事务。

### 1.4 查询改造

看板按「按天聚合」的查询走 `user_daily_stats`；当前窗口含「今天」时，今天那一格用 `billing_records` 实时补齐
（避免聚合表与实时数据的口径缝隙）。响应结构不变（向后兼容）。

---

## 2. ② 热路径 Redis 5 分钟缓存

| 项 | 设计 |
|---|---|
| 键 | `stats:v1:dashboard:{scope}:{user_or_admin}:{from}:{to}`；`{scope}` ∈ `admin`/`dev` |
| TTL | 300s（5 分钟） |
| 失效 | **写路径主动失效**：`settleTx` 成功后按受影响 `day` 递增版本号 `stats:v1:ver:{day}`，键里带版本（无需扫描删键）；同时 DEL 该用户的当日看板键 |
| 击穿保护 | 单请求内 singleflight（同键并发只回源一次）+ 逻辑过期不允许为空（空结果也缓存短期 30s） |
| 穿透 | 读 miss → 回源 DB → 回填缓存；回源失败不写缓存 |
| 失效面 | 记账写入（用户维度）、渠道/模型配置变更（统计口径不含配置，可不失效）、手动清理入口 |

命中率采集：进程内计数器（`cache_hit` / `cache_miss` / `bypass`），随看板响应头 `x-stats-cache` 输出，压测统计。

---

## 3. ③ 明细表按 `call_time` 分区

> 前置：需确认生产是否已有 `created_at` 分区（见 §0.2）。若没有，本段即「普通表 → 分区表」的在线切换方案。

方案（不删数据）：

1. 新建分区父表 `billing_records_p(…) PARTITION BY RANGE (call_time)`，按月子分区；
2. 建 12 个月（+未来 2 个月）子分区；
3. `INSERT … SELECT` 分批搬历史（按 `call_time` 升序，批 5 千～1 万，断点可续跑）；
4. 校验行数与金额校验和一致后，单事务内 `ALTER TABLE … RENAME` 完成切换（写窗口 < 1s）；
5. 旧表保留为 `billing_records_legacy`，**不 drop**（等 spencer 拍）。

保留策略：只做结构（子分区到 12 个月），删除动作需单独拍板。

验证：同一查询（按 `call_time` 过滤近 7 天）在改造前后的 `EXPLAIN (ANALYZE, BUFFERS)` 中
`Partitions removed` / 扫描分区数对比。

---

## 4. 风险

| 风险 | 说明 | 缓解 |
|---|---|---|
| 回填时长未知 | 依赖行数；本机无数据，无法给真数字 | 分批 + 断点；单批耗时随进度打印；可在只读副本先跑 |
| 锁写窗口 | 切换分区需要短暂排他锁 | 迁移与切换放在低峰；切换步骤单事务、改名而非拷数据 |
| 聚合口径漂移 | 看板同时读聚合表与实时表 | 今天那格实时补齐；回填完成后与直算 SQL 逐日对账 |
| 缓存一致性 | 写路径失效若漏一条写入链路会读到旧值 | 版本号 + 短 TTL 兜底；写路径单一收口（`settleTx`） |
| 结构假设不成立 | 任务描述的分区现状与本仓库 schema 不符（§0.2） | 先要生产 DDL 原文，再定基线 |

---

## 5. 文件清单（预计改动）

| 文件 | 改动 |
|---|---|
| `backend/internal/db/migrations/0020_user_daily_stats.sql` | 新建聚合表 + 索引 |
| `backend/internal/db/migrations/0021_billing_records_partition_by_call_time.sql` | 分区父表与子分区（**待 §3 前置确认**） |
| `backend/internal/domain/billing/*` | `settleTx` 内 upsert 聚合 + 版本号失效钩子 |
| `backend/internal/domain/console/*` | 看板查询改走聚合表；缓存读写与命中统计 |
| `backend/internal/pkg/cache/redis.go`（或既有 Redis 封装） | 封装 get/set/版本号/命中计数 |
| `backend/scripts/backfill-daily-stats/main.go` | 分批断点回填 |
| `backend/scripts/partition-switch/main.go` | 分区搬迁与切换（含校验和） |
| `backend/scripts/bench-stats/*` | 压测与 `EXPLAIN` 采集脚本 |
| `README.md` | 运维入口（回填、切换、压测命令） |
