# daily-stats-rollup 交付状态（快照）

> 快照时间：2026-11-12 02:58 CST（UTC+8）· 测试（Gate）
> 目的：让任何人用一次 `git log` / 一次 GitHub API 调用即可核对「PR 存在与否、数字在哪、还差什么」。

## 1. PR 已存在（客观证据，可自行核对）

```
$ gh api repos/spencerswagger/LingShu/pulls/79 --jq '{number,state,draft,created_at,html_url,head:.head.ref,base:.base.ref,commits,changed_files}'
{"base":"main","changed_files":38,"commits":5,"created_at":"2026-11-11T18:37:44Z","draft":false,
 "head":"feat/daily-stats-rollup","html_url":"https://github.com/spencerswagger/LingShu/pull/79",
 "number":79,"state":"open"}
```

- 编号 **#79**，OPEN，非 draft，base `main` ← head `feat/daily-stats-rollup`，5 个提交、38 个文件。
- 创建时刻 `2026-11-11T18:37:44Z` = **北京时间 2026-11-12 02:37:44**。
- `gh api 'repos/spencerswagger/LingShu/pulls?state=open'` 当前返回 5 张：#75 / #76 / #77 / #78 / **#79**。
- 未合入（合入权不在测试），未发起任何「评审通过」结论。

分支关系：`feat/daily-stats-rollup` 原本就坐在 `origin/main`（`3720ed7`）之上，`git rev-list --left-right --count origin/main...origin/feat/daily-stats-rollup` = `0 2`，**无分叉、无需 rebase**，因此不存在 rebase 冲突。

## 2. 数据集（实测，非估算）

```
$ psql -d qa_stats30m -tAc "select count(*) from billing_records"
30000000
$ psql -d qa_stats30m -tAc "select reltuples::bigint from pg_class where relname='billing_records'"
29999936
```

- 精确值 **30,000,000 行**，`reltuples` 估算 29,999,936（与分析过后一致）。
- 全部 `status='completed'`，`call_time` 覆盖 2026-08-13 ~ 2026-11-11（91 天），30 天窗口内 10,000,591 行；100 用户（单人占 40%）；表 15 GB（`VACUUM (FULL, ANALYZE)` 后）。
- 建库耗时：灌数 **2,603 s**（43.4 分钟），回填 ≈6,400 s。当前该库无长事务在跑（`pg_stat_activity` 上 `qa_stats%` 活跃查询仅本条 count，0 秒）。

## 3. 四项交付状态

| 项 | 状态 | 数字 / 证据 |
|---|---|---|
| ① p50/p95 对比表（3000 万行，3 轮取中位） | **已出** | `me/summary` 3591.3 → 冷 4.4 / 热 1.4 ms；`cost/stats` 220 516.7 → 冷 67 129.5 / 热 0.5 ms |
| ② 启动回填耗时 + 峰值内存 | **已出** | ≈6,400 s（20.56M 行@批量5000=2998 s、9.42M 行@批量20000=3369 s、日聚合 91 天=127 s）；峰值 RSS 28.5 MB（VmHWM） |
| ③ EXPLAIN + 10/10 同 key 一致 | **已出** | `user_daily_stats` → Bitmap Index Scan，11 行，0.173 ms；两个接口连打 10 次 sha256 全同（`distinct_digests=1`）。**但 `cost/stats` 不走预聚合表**（见 §4-1） |
| ④ 跨用户缓存反向用例 | **已出** | 3 条用例（顺序 / 并发 singleflight / 区间），变异验证均真红 |

原始采样、脚本、EXPLAIN 全文：`notes/stats-rollup-0020/`（README 第 9 节含复跑命令）。

## 4. 需要产品/架构拍板的三个事实

1. **`cost/stats` 没有走预聚合表**：EXPLAIN 是 `Index Scan using idx_billing_status_time`，30 天窗口 10,000,591 行 + `array_agg(DISTINCT)` 外部归并（temp 1.4 GB）；`user_daily_stats` 只服务 `/auth/me/summary` 三档。收益来自「去 JSONB 正则 + 收敛窗口 + 实体列」，不是预聚合。
2. **设计文档绝对数字复现不了**：文档 `cost/stats` 129 s → 27.9 s；本机 220.5 s → **67.1 s**（相对收益 3.3× 仍成立，但「5 分钟缓存够不够」要按 67 s 判断）。
3. **首启回填是小时级且同步阻塞启动**：实测 ≈107 分钟（`main.go` 里在 `seeding.Seed` 与 `buildApp` 之前调用）。文档 55.6 s 是因为那次 seed 直接写了实体列、token 阶段跑 0 行。

## 5. 还差什么（都不在测试手上）

- 等分级结论（P0/P1/P2 分级由代码审查侧给出），之后按级修复再复验。
- 合入由项目负责人执行，测试不合 main。
- 「回填改后台异步 + 启动期降级」是产品决策项，未获指令前不动实现（属计费/启动路径，超出测试改动范围）。
