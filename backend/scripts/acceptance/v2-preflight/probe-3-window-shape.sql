-- 探测③（形状层）：`reconcile_windows` 的列与关键约束。期望 0 行。
--
-- 两个升级点（相对数据层契约 §5.1 的旧版 ③）：
--   1. 旧版 ③ 只输出「列清单」，判读要靠人拿清单与十列集合逐项比 —— 12 行是旧形、11 行是
--      「十列齐 + 多余列 id」，光看行数会把 S5b 读成「还行」。这里直接输出**判定行**：
--      缺列 / 多余列 / 约束不符各一条，空输出即正形状。
--   2. S5b（列齐但 PK 在 `id`、或 60s CHECK 缺失）旧版四条探测全净，只能靠人工跑
--      `pg_get_constraintdef` 兜。这里把「PK = window_start」「60s 窗口 CHECK」「status 五值」
--      三条约束断言并入 ③ —— S5b 不再依赖「记得手动跑一下」。
--
-- 结构断言不覆盖的：索引、行级安全策略、列类型。列类型若要断言，加 `data_type` 比对，
-- 但迁移里类型变更是 ALTER 出来的，比的是最终态，别写成「创建时的类型」。

WITH col(col) AS (
  VALUES ('window_start'), ('window_end'), ('local_count'), ('upstream_count'),
         ('local_tokens'), ('upstream_tokens'), ('status'), ('alarm_streak'),
         ('detail'), ('checked_at'), ('scroll_offset')
), t AS (SELECT to_regclass('public.reconcile_windows') AS rel)
-- 表还没建：本探测不判（0023 之前执行探测属正常中间态）
SELECT 'reconcile_windows 缺列: ' || col AS finding
FROM col, t
WHERE t.rel IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM information_schema.columns c
    WHERE c.table_schema = 'public' AND c.table_name = 'reconcile_windows'
      AND c.column_name = col.col)

UNION ALL
-- 多余列：旧形（0019_v2_facade）会带 id / pulled_at / req_count_ls / dev_pct / tokens_ls
SELECT 'reconcile_windows 多余列: ' || c.column_name
FROM information_schema.columns c, t
WHERE t.rel IS NOT NULL
  AND c.table_schema = 'public' AND c.table_name = 'reconcile_windows'
  AND c.column_name NOT IN (
    'window_start', 'window_end', 'local_count', 'upstream_count',
    'local_tokens', 'upstream_tokens', 'status', 'alarm_streak',
    'detail', 'checked_at', 'scroll_offset')

UNION ALL
-- 主键必须是单列 window_start（旧形主键是 id → ON CONFLICT (window_start) 直接报无匹配约束）
SELECT 'reconcile_windows 主键不符: 期望 PRIMARY KEY (window_start)'
FROM t
WHERE t.rel IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM pg_constraint pc
    WHERE pc.conrelid = t.rel AND pc.contype = 'p'
      AND pg_get_constraintdef(pc.oid) = 'PRIMARY KEY (window_start)')

UNION ALL
-- 窗口长度必须由库断言（window_end = window_start + 60s）
-- ⚠️ 不能写 `LIKE '%60 seconds%'`：PG 会把 `interval '60 seconds'` 归一成 `'00:01:00'::interval`
--    （实测：迁移文件写 60 seconds，pg_get_constraintdef 出来是 00:01:00），照字面匹配会永远误报。
SELECT 'reconcile_windows 缺 60s 窗口 CHECK'
FROM t
WHERE t.rel IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM pg_constraint pc
    WHERE pc.conrelid = t.rel AND pc.contype = 'c'
      AND pg_get_constraintdef(pc.oid) LIKE '%window_end%'
      AND pg_get_constraintdef(pc.oid) LIKE '%window_start%'
      AND (pg_get_constraintdef(pc.oid) LIKE '%00:01:00%'
        OR pg_get_constraintdef(pc.oid) LIKE '%60 seconds%'
        OR pg_get_constraintdef(pc.oid) LIKE '%1 min%'))

UNION ALL
-- status 值域必须是五值（旧形的列内联 CHECK 只认 ok|alarm|error|blocked，会拒 skipped）
SELECT 'reconcile_windows status 值域不完整（期望 ok,alarm,error,blocked,skipped）'
FROM t
WHERE t.rel IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM pg_constraint pc
    WHERE pc.conrelid = t.rel AND pc.contype = 'c'
      AND pg_get_constraintdef(pc.oid) LIKE '%skipped%'
      AND pg_get_constraintdef(pc.oid) LIKE '%blocked%')

ORDER BY finding;
