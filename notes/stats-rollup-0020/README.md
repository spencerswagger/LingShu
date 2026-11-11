# 统计/账单接口性能改造（0020）· 独立复核与压测证据

> 2026-11-11 · 测试（Gate）· 对象分支 `feat/daily-stats-rollup`（2 提交）
> 全部数字为本机独立实测，脚本与原始采样本目录内可复跑。**本文的绝对数字与设计文档不一致，以本文为准**（见 §8）。

## 1. 一句话结论

| | 改造前 | 改造后（冷） | 改造后（热） | 结论 |
|---|---|---|---|---|
| `GET /auth/me/summary` p50 | **3591.3 ms** | **4.4 ms** | **1.4 ms** | 816× / 2565× |
| `GET /admin/cost/stats` p50 | **220 516.7 ms** | **67 129.5 ms** | **0.5 ms** | 3.3× / 44 万× |

- 三档积分与区间合计**确实**落到预聚合表（EXPLAIN：`Bitmap Index Scan on user_daily_stats_pkey`，11 行，0.173 ms）。
- **`cost/stats` 没有走预聚合表**：它仍是 `billing_records` 30 天窗口（10,000,591 行）的明细聚合，只是去掉了逐行 JSONB 正则。任务书里「EXPLAIN 证明 cost/stats 走预聚合表」这条在当前实现下不成立（见 §5、§8-2）。
- 首次启动回填在 3000 万行存量库上是**小时级**（实测 ≈ 6400 s），不是设计文档写的 55.6 s（文档那次 token 阶段跑 0 行，见 §4、§8-3）。

## 2. 数据集与测量方法（同机同库，一台机器一份数据）

| 项 | 取值 |
|---|---|
| 库 | 临时库 `qa_stats30m`（`lingshu_test` 角色自建，跑完未删，便于复核） |
| `billing_records` | **30,000,000 行**，全部 `status='completed'`，`call_time` 覆盖 2026-08-13 ~ 2026-11-11（91 天，30 天窗口内 10,000,591 行） |
| 分布 | 100 用户，单个用户占 40%（贴近「一个人一个月几十万行」的真实痛点）；3 渠道 / 3 密钥 / 3 对外模型；5000 会话 |
| 表体积 | 15 GB（`VACUUM (FULL, ANALYZE)` 后；等价于一次干净装载，避免回填产生的旧版本抬高任何一侧） |
| `user_daily_stats` | 9,100 行（91 天 × 100 用户） |
| 机器 | 8 核 / 96 GB，PG 17，`shared_buffers=16GB`，`work_mem=4MB`，`jit=on` |

**改造前 / 改造后怎么拿**：同一台机器、同一个库、同一份数据，两个真实二进制：
- 改造前 = `git archive origin/main`（`3720ed7`）构建的 `cmd/server`；
- 改造后 = 本分支构建的 `cmd/server`。

两者都用 `GET` 打真实 HTTP（只读接口，参数固定），不 import 被测代码。

**冷 / 热定义**（`bench.py`）：
- 冷：每条样本单独起一次被测进程 → 进程内缓存必然为空（响应头 `x-stats-cache: miss` 逐条核过）；
- 热：一个进程内 1 次预热 + N 次重复（响应头 `x-stats-cache: hit`）；
- 改造前没有缓存，所以「每次都是冷」，采集时按顺序连打即为逐次全量查询。

**p50/p95**：单轮内按 nearest-rank 取分位；**跑 3 轮，取 3 轮 p50 / p95 的中位数**（表里标的就是中位）。

## 3. p50 / p95 对比表（单位 ms）

| 接口 | 口径 | 每轮采样 | 3 轮 p50 | 3 轮 p95 | 中位 p50 | 中位 p95 |
|---|---|---|---|---|---|---|
| `/auth/me/summary` | 改造前 | 5 | 3591.3 / 3580.3 / 3631.4 | 3906.9 / 3691.4 / 3660.1 | **3591.3** | **3691.4** |
| `/auth/me/summary` | 改造后·冷 | 5 | 7.1 / 3.9 / 4.4 | 8.7 / 5.2 / 7.6 | **4.4** | **7.6** |
| `/auth/me/summary` | 改造后·热 | 5 | 1.6 / 1.4 / 1.1 | 1.8 / 1.7 / 1.4 | **1.4** | **1.7** |
| `/admin/cost/stats` | 改造前 | 3 | 220516.7 / 221387.7 / 218105.5 | 231330.3 / 226383.2 / 221914.9 | **220516.7** | **226383.2** |
| `/admin/cost/stats` | 改造后·冷 | 5 | 66665.4 / 67387.1 / 67129.5 | 67022.7 / 67916.7 / 67321.3 | **67129.5** | **67321.3** |
| `/admin/cost/stats` | 改造后·热 | 5 | 0.8 / 0.5 / 0.5 | 1.0 / 0.7 / 0.9 | **0.5** | **0.9** |

原始采样（逐条 ms / 缓存头 / 响应字节数）：`raw/before-cost.jsonl`、`raw/after-cost-cold.jsonl`、`raw/after-cost-hot.jsonl`、`raw/before-summary.jsonl`、`raw/after-summary-cold.jsonl`、`raw/after-summary-hot.jsonl`。

**结论口径**：改造后 `cost/stats` 的冷启动仍有 **67 s**（每 5 分钟一次真回源），热路径 0.5 ms 是缓存兜住的；`me/summary` 冷热都在 10 ms 内。

## 4. 启动回填（30,000,000 行存量行，实体列全 0 = 真实升级起手状态）

测量方法：`startup_measure.py` 起真实二进制，每 1 s 探 `/healthz`，每 5 s 读 `/proc/<pid>/status` 的 `VmHWM`；**`/healthz` 首次 200 即「可服务」**（回填在 `main.go` 里是 `seeding.Seed` 与 `buildApp` 之前**同步**执行的，所以这段时间服务不可用）。

| 阶段 | 处理量 | 批量 | 实测耗时 | 速率 |
|---|---|---|---|---|
| token 列（干净表起手） | 20,560,000 行 | 5000（CLI 默认） | **2 998.0 s** | 6,858 行/秒 |
| token 列（表已含上一轮旧版本） | 9,420,000 行 | 20000（**服务启动默认**） | **3 369.0 s** | 2,796 行/秒 |
| 日聚合 | 91 天 / 9,100 行 | 每批 7 天 | **127 s** | — |
| **合计（等效单进程到底）** | 30,000,000 行 + 91 天 | — | **≈ 6 400 s（≈107 分钟）** | — |
| 峰值内存 | — | — | **28.5 MB**（`VmHWM`，非机器峰值） | — |

- 分段只因为工具单次执行上限 60 分钟，回填本身带断点续跑（`stats_backfill_state`），**两段拼接即单进程到底的等效值**；分段续跑本身也验证了断点可用（第二次启动从 `cursor=20585000` 继续）。
- 按实测速率折算全量：干净表 ≈ 73 分钟（6,858 行/秒）、膨胀表 ≈ 179 分钟（2,796 行/秒）。**量级是「小时」，不是「秒」。**
- 原始日志：`logs/backfill-段A-5000批.log`（5.5 MB 全文另存工作区）、`logs/startup-chunk2.log`。
- 复核命令：`DATABASE_DSN=... go run ./scripts/backfill-daily-stats -reset`（本目录 `README` §9 有完整命令）。

## 5. EXPLAIN（10 条走索引的核对里最关键的三条）

完整输出：`explain.out`。

**A. `/auth/me/summary` 三档 —— 走预聚合表（这是唯一走 `user_daily_stats` 的看板查询）**

```
Aggregate  (actual time=0.059..0.065 rows=1)
  ->  Bitmap Heap Scan on user_daily_stats  (actual time=0.040..0.044 rows=11)
        Recheck Cond: user_id = 900000000 AND day >= '2026-11-01' AND day <= '2026-11-12'
        ->  Bitmap Index Scan on user_daily_stats_pkey  (rows=11)
Execution Time: 0.173 ms
```

**B. 同一份数据用改造前的明细写法（只取「本月」一段代表）**

```
Finalize Aggregate (actual time=2439.841..2462.527)
  ->  Parallel Seq Scan on billing_records  (rows=487200 loops=3)
        Filter: call_time >= ... AND user_id = 900000000 AND status = 'completed'
        Rows Removed by Filter: 9512800
Execution Time: 2462.527 ms     Buffers: read=1664615（≈13 GB 冷读）
```

**C. `/admin/cost/stats` 改造后**

```
Index Scan using idx_billing_status_time on billing_records b  (rows=10 000 591)
  Filter: (pricing_mode <> 'cost') OR ... OR (ANY (billing_id = (hashed SubPlan 3).col1))
Sort Method: external merge  Disk: 1428632kB
Execution Time: 75 462.817 ms
```

→ 明细表 30 天窗口 10,000,591 行 + 外部归并排序；**没有 `user_daily_stats` 参与**，正则已消失（实体列求和）。改造前同一查询（无时间窗、每行 3 次 JSONB 正则）的计划见 `explain.out` 的 D 段：`Parallel Seq Scan on billing_records rows=12499973`。

**10/10 同 key 结果一致**（`consistency.py`，同一进程连打 10 次，去掉每次变化的 `RequestID` 后比对 sha256）：

- `/auth/me/summary`：`distinct_digests=1`，10/10 逐字节相同（首 call miss 8.2 ms，后 9 次 hit 1.5–3.2 ms）。
- `/admin/cost/stats`：`distinct_digests=1`，10/10 逐字节相同（首 call miss 71 967.7 ms，后 9 次 hit 0.5–0.7 ms）。

## 6. 跨用户缓存不串 —— 反向用例（会真失败的测试）

文件：`backend/internal/domain/console/gate_stats_cache_isolation_test.go`（3 个用例，均随本分支提交）。

| 用例 | 守什么 | 去掉防护后 |
|---|---|---|
| `TestGateCache_MeSummaryPerUserIsolated` | A/B 各拿各的值 + 缓存里必须有 **2 把键** + 再查 A 仍命中自己的值 | 把 `meSummaryCacheKey` 的用户维度抹掉 → **FAIL**（`首轮都应是回源(miss)，实际 A="miss" B="hit"`） |
| `TestGateCache_MeSummaryConcurrentCrossUser` | A 的回源故意慢 400 ms，B 在途时进来**不得**挂到 A 的 singleflight 上 | 同上变异 → **FAIL**（`B 拿到 11.5/1200，期望自己的 21.5/2200（挂到了 A 的在途回源上）`） |
| `TestGateCache_CostStatsPerWindowIsolated` | 不同 `From` 各自缓存，返回的 `Range` 与金额必须是自己那份 | 把 `cost-stats:<from>:<to>` 的 from 抹掉 → **FAIL**（`显式区间拿到 from=2026-08-28，期望 2026-09-01（返回了别的区间缓存）`） |

变异是**真的改了实现再跑**（改完立刻还原，`git diff` 干净），不是注释占位。

## 7. 门禁

| 项 | 结果 |
|---|---|
| `gofmt -l`（本次改动文件） | 0 命中 |
| `go vet ./...` | 0 输出 |
| `go test -race`（31 个含测试包，排除 `backend/tests`） | **987 顶层 + 427 子用例 PASS，FAIL 0，SKIP 1**（唯一 SKIP 是 `TestPerfStatsDashboard`，需 `PERF_BENCH_DSN`） |
| 真库集成用例（`LLMGATEWAY_TEST_PG_DSN` 指向临时库） | `TestStats_*` 4 条全 PASS（改夹具前有 3 条红，见 §8-4） |

日志：`logs/gate-race.log`、`logs/gate-race-v.log`。

## 8. 复核发现（需要产品/架构看的三条）

1. **设计文档 §5 的绝对数字在本机不可复现**：文档写 `cost/stats` 129 s → 27.9 s、`me/summary` 2289 ms → 0.6 ms；本机实测 220.5 s → 67.1 s、3591 ms → 4.4 ms。改造方向与相对收益成立（3.3× / 816×），但**文档里的绝对数字建议按本表改写**——尤其 `cost/stats` 冷启动是 **67 s 不是 28 s**，这决定「5 分钟缓存 TTL 够不够」的产品判断。
2. **`cost/stats` 没有走预聚合表**（任务书假设不成立）：它仍是明细表 30 天窗口聚合，瓶颈是 1000 万行的实体 join + `array_agg(DISTINCT)` 外部归并（temp 1.4 GB），不是正则。文档 §7-1 自己也写了这点。所以「cost/stats 走预聚合表」这条验收标准要么改成「去正则 + 收敛窗口」，要么等 0021（实体 × 天预聚合）。
3. **首次启动回填是小时级且同步阻塞启动**：3000 万行存量库实测 ≈ 107 分钟（峰值内存仅 28.5 MB，不是内存问题）。文档 §5 的 55.6 s 是因为那次 seed 直接写了实体列、token 阶段跑 0 行。建议：回填改为启动后异步（或在 `seeding` 前先探测断点），否则一次版本升级会把服务启动卡住 1.5 小时以上；批量 5000（CLI 默认）比 20000（服务默认）慢一倍，两者都远超「秒」。
4. **集成夹具漏改（本分支已修）**：`stats_window_integration_test.go` 只写 `tokens` JSONB，0020 之后统计读实体列 → `TestStats_WindowAndPlaza` / `TestStats_WindowCalendarMode` / `TestStats_SoftDeletedModelGoesUnmatched` 报 `tokens=0` 全红。CI 没设 `LLMGATEWAY_TEST_PG_DSN`（整条 `t.Skip`）所以一直是绿的。本 PR 第一个提交补写实体列。
5. 观察（非阻断）：`cost/stats` 缓存键是 `cost-stats:<fromUnix>:<toUnix>`，所有管理员共享同一把键。数据本身是全站口径、无串数据风险，但「5 分钟滞后」是全局的（任何人触发回源后所有人都看到同一份 5 分钟内快照）。

## 9. 复跑命令

```bash
# 0) 建库（用 lingshu_test 角色，不要动 lingshu_dev / lingshu_test）
psql -d postgres -c 'CREATE DATABASE qa_stats30m'
DATABASE_DSN='postgres://.../qa_stats30m?sslmode=disable' go run ./scripts/backfill-daily-stats  # 只跑迁移

# 1) 造 3000 万行（本目录 seed-30m.sql：刻意不写 0020 实体列 = 真实存量库状态）
psql -d qa_stats30m -v rows=30000000 -f seed-30m.sql

# 2) 回填（-reset 清断点从最早数据重算）
DATABASE_DSN='...' go run ./scripts/backfill-daily-stats -reset -rows 20000 -days 7

# 3) 压测（先起两个二进制：main 树与分支树各 build 一次 cmd/server）
python3 bench.py --binary ./bin/server-main   --config config-main.yaml   --port 18390 \
        --endpoint cost --mode hot  --samples 3 --campaign 1 --out before-cost.jsonl
python3 bench.py --binary ./bin/server-branch --config config-branch.yaml --port 18391 \
        --endpoint cost --mode cold --samples 5 --campaign 1 --out after-cost-cold.jsonl

# 4) 一致性与计划
python3 consistency.py ./bin/server-branch config-branch.yaml 18391 admin.jwt cost 10
psql -d qa_stats30m -f explain.sql
```

> 鉴权说明：压测用 `mintjwt`（用仓库 `internal/pkg/jwtx` 对 `backend/keys/jwt_rsa` 签，`uid=900000000`、`role=ADMIN`、`ver=0`，与库里的 `token_version` 一致），不落任何口令。

## 10. 已知局限

- 单机单库、`shared_buffers=16GB`；没有覆盖多副本缓存不一致（设计文档 §7-3 已声明是进程内缓存的既定代价）。
- `cost/stats` 的冷样本靠「重启进程」制造，重启本身约 2 s 不计入请求耗时；热样本在单进程内连打，受 OS/PG 页缓存预热影响（对改造前后是对称的）。
- 本次未覆盖前端 `/me` 5 分钟 TTL 缓存的浏览器侧行为（该部分已在 `origin/main` 的 #74 合入，本分支不含前端改动）。
