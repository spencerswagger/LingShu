-- 探测①（记录层）：applied_migrations 里出现「本链之外」的 V2 记录就说明库被旧分支污染过。
--
-- 口径三条，缺一条就会误报或漏报：
--   1. **排除式**：不要写 `name >= '0019'` 裸条件 —— 本链跑完后它永远非空，每次都误报；
--   2. **有右界**：`name < V2MigrationRightBound`（当前 0025），后续批新增迁移不会打红本探测；
--   3. **白名单必须等于 `db.V2MigrationFiles()`**：加了迁移文件忘了加白名单 = 每次误报 N 行
--      （实测：0023b / 0023c 落地后，旧白名单在干净库上报 2 行假污染）。
--      这一条由 `internal/db/v2_probe_list_test.go` 用源码级用例守住，别只靠人眼。
--
-- 期望输出：0 行。有输出 → 按状态矩阵判 S1/S2/S3，走 R-1/R-2，不许直接执行 0024。
--
-- <v2-migration-whitelist>
--   0019_v2_topology.sql
--   0019b_v2_passthrough_credentials.sql
--   0020_v2_billing_attribution.sql
--   0022_v2_ledger.sql
--   0023_v2_reconcile.sql
--   0023b_v2_reconcile_repair.sql
--   0023c_v2_reconcile_counts_backfill.sql
--   0024_v2_drop_legacy.sql
-- </v2-migration-whitelist>

SELECT name
FROM applied_migrations
WHERE name >= '0019'
  AND name < '0025'                       -- = db.V2MigrationRightBound
  AND name NOT IN (
    '0019_v2_topology.sql',
    '0019b_v2_passthrough_credentials.sql',
    '0020_v2_billing_attribution.sql',
    '0022_v2_ledger.sql',
    '0023_v2_reconcile.sql',
    '0023b_v2_reconcile_repair.sql',
    '0023c_v2_reconcile_counts_backfill.sql',
    '0024_v2_drop_legacy.sql'
  )
ORDER BY name;
