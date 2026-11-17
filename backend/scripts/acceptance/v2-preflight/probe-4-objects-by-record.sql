-- 探测④（反查层）：按「已记文件名 → 它该建的对象」核对对象**真的在**。期望 0 行。
--
-- 专治 S4（记录在、对象缺）：这种库里 ①②③ 全 0 行，判不出任何异常；
-- 对「同名不同内容」也有效：0019/0019b/0020/0022 的对象/列在不在，①②③ 完全没有信号。
--
-- 清单必须覆盖 `db.V2MigrationFiles()` 的**每一个**文件 —— 漏一个文件就等于那个文件的
-- 对象缺了没人管（本轮补的正是这条：旧清单里 0023b / 0023c 整段缺失）。
-- 由 `internal/db/v2_probe_list_test.go` 守住「每个迁移文件都在本清单出现至少一次」。
--
-- ⚠️ 只 JOIN 本链文件名：旧分支记录名（0019_v2_facade.sql / 0020_reconcile_blocked_status.sql）
--    匹配不到，所以 S1/S2 的 ④ 是 0 行（判定靠 ①）。这是设计如此，不是漏。

WITH want(file, kind, obj) AS (VALUES
  ('0019_v2_topology.sql',               'table',      'internal_models'),
  ('0019_v2_topology.sql',               'table',      'model_links'),
  ('0019_v2_topology.sql',               'table',      'subscriptions'),
  ('0019_v2_topology.sql',               'column',     'external_models.bound_internal_ref'),
  ('0019b_v2_passthrough_credentials.sql','table',     'magpie_keys'),
  ('0019b_v2_passthrough_credentials.sql','column',    'external_models.magpie_model'),
  ('0019b_v2_passthrough_credentials.sql','index',     'uq_external_models_magpie_model'),
  ('0020_v2_billing_attribution.sql',    'column',     'billing_records.upstream_provider'),
  ('0020_v2_billing_attribution.sql',    'column',     'billing_records.cost_source'),
  ('0022_v2_ledger.sql',                 'table',      'ledger_entries'),
  ('0022_v2_ledger.sql',                 'table',      'credit_holds'),
  ('0023_v2_reconcile.sql',              'table',      'reconcile_windows'),
  ('0023b_v2_reconcile_repair.sql',      'constraint', 'reconcile_windows.ck_reconcile_status'),
  ('0023b_v2_reconcile_repair.sql',      'constraint', 'billing_records.ck_billing_cost_presence'),
  ('0023b_v2_reconcile_repair.sql',      'column',     'reconcile_windows.scroll_offset'),
  ('0023c_v2_reconcile_counts_backfill.sql','data',    '（数据型迁移，无对象可断言）'),
  ('0024_v2_drop_legacy.sql',            'gone',       'channels'),
  ('0024_v2_drop_legacy.sql',            'gone',       'channel_keys'),
  ('0024_v2_drop_legacy.sql',            'gone',       'semantic_tags')
), rec AS (
  SELECT name FROM applied_migrations WHERE name >= '0019'
)
SELECT w.file || ' → ' || w.obj || '（' || w.kind || '）' AS suspicious
FROM want w
JOIN rec ON rec.name = w.file
WHERE CASE w.kind
        WHEN 'table' THEN to_regclass('public.' || w.obj) IS NULL
        WHEN 'gone'  THEN to_regclass('public.' || w.obj) IS NOT NULL
        WHEN 'data'  THEN false            -- 数据型迁移没有对象断言，靠 0023c 自身的幂等用例
        WHEN 'index' THEN to_regclass('public.' || w.obj) IS NULL
        WHEN 'constraint' THEN NOT EXISTS (
            SELECT 1 FROM pg_constraint pc
            WHERE pc.conrelid = to_regclass('public.' || split_part(w.obj, '.', 1))
              AND pc.conname = split_part(w.obj, '.', 2))
        ELSE NOT EXISTS (                  -- 'column'
            SELECT 1 FROM information_schema.columns ic
            WHERE ic.table_schema = 'public'
              AND ic.table_name   = split_part(w.obj, '.', 1)
              AND ic.column_name  = split_part(w.obj, '.', 2))
      END
ORDER BY 1;
