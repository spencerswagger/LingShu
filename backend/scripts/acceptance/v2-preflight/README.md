# V2 迁移链 · 执行前探测（0024 前的四条 SQL）

一条命令：

```sh
sh run.sh <库名或 postgres:// DSN>     # PGHOST/PGPORT/PGUSER 走环境变量
```

- 四条输出全空 → 判 **S0**，可以执行 `0024_v2_drop_legacy.sql`（仍要满足三步证据与保留表行数判据）。
- 任一条有输出 → **不许执行**，按数据层契约 §5.2 状态矩阵走 R-1 / R-2。

| 文件 | 层次 | 期望 | 判什么 |
| --- | --- | --- | --- |
| `probe-1-records.sql` | 记录层 | 0 行 | `applied_migrations` 里出现本链之外的 V2 记录（被旧分支污染过） |
| `probe-2-legacy-objects.sql` | 对象层 | 0 行 | 本链**不该建**的旧对象（现只剩 `billing_records` 三列，属阶段二） |
| `probe-3-window-shape.sql` | 形状层 | 0 行 | `reconcile_windows` 缺列 / 多余列 / 主键不是 `window_start` / 缺 60s CHECK / status 值域不全 |
| `probe-4-objects-by-record.sql` | 反查层 | 0 行 | 记录说有、对象却不在（S4）；逐个迁移文件核它该建的对象 |

## 三个不能再犯的坑（都实测过）

1. **白名单漏一个迁移文件 = 干净库误报**。`0023b` / `0023c` 落地后，旧白名单（只列 0019/0020/0022/0023/0024）
   在**全新干净库**上报 2 行「污染」，运维会照矩阵把它判成 S1/S2、对健康库跑 R-2 回基线。
   反向漏一个文件的 ④ 映射，那个文件的对象缺了则永远没人发现（S4 只有 ④ 有信号）。
   ⇒ 现在由 `internal/db/v2_probe_list_test.go` 断言「probe-1 白名单 == `db.V2MigrationFiles()`」
   与「probe-4 覆盖每个迁移文件」。**加迁移文件时，三处（Go 常量 + probe-1 + probe-4）要一起改。**
2. **探测② 不能把仍被用的对象当旧对象**。2026-11-16 裁定 A 之后 `magpie_keys` 表、
   `external_models.magpie_model` 列与 `uq_external_models_magpie_model` 索引由
   `0019b_v2_passthrough_credentials.sql` 正当创建；把它们留在②里 = 干净库上永远非 0 行。
3. **`interval '60 seconds'` 会被 PG 归一成 `'00:01:00'`**：照字面 `LIKE '%60 seconds%'` 匹配
   `pg_get_constraintdef` 永远不中（实测：干净库上误报「缺 60s 窗口 CHECK」）。
   现按 `00:01:00` / `60 seconds` / `1 min` 三种写法同时匹配。

## 与「人工查询」的关系

旧版把「PK 是否在 `window_start`、60s CHECK 在不在、status 值域全不全」留给一条
人工 `pg_get_constraintdef` 查询（S5b 四条探测全净，只有那条能拦）。现在这三条并入探测③，
S5b 不再依赖「记得手动跑一下」。探测③ 因此输出的是**判定行**，不是列清单。

## 覆盖不到的

- 索引、行级安全、列类型：③ 不断言列类型（迁移里类型是 ALTER 出来的，比的是最终态）。
- 数据正确性：0023b/0023c 是纯数据迁移（把 0 占位归 NULL），没有对象可断言，
  ④ 里以 `kind='data'` 显式标注，由它们自己的幂等用例覆盖。
- 记录表内容本身不可信：`applied_migrations` 只有文件名与时间，没有内容指纹，
  「同名异内容」在库层面无法自证 —— 只能靠发布前 `sha256sum` 比对（见 PR 描述的哈希基线）。
