-- 探测②（对象层）：本链**不该建**的旧分支对象。期望 0 行。
--
-- ⚠️ 口径变更（2026-11-16 裁定 A）：`magpie_keys` 表、`external_models.magpie_model` 列与
--    `uq_external_models_magpie_model` 索引**已改为本链的正当对象**（落在
--    `0019b_v2_passthrough_credentials.sql`），不再是「旧分支独有」。
--    所以它们从本探测的「期望不存在」移到探测④的「记录在 → 对象必须在」。
--    照旧口径把 magpie_keys 留在这里 = 干净库上永远 1 行、每次误报。
--
-- 仍属「本链不建」的只有 `billing_records` 这三列：它们随阶段二（≥ 0025）落地，
-- 那时本探测要同步退役（否则又会误报）。判据不是本文档，而是「本链有没有写入方」。
--
-- 期望输出：0 行。

SELECT 'billing_records.magpie_key_name' AS legacy_object
WHERE EXISTS (SELECT 1 FROM information_schema.columns
              WHERE table_schema = 'public' AND table_name = 'billing_records'
                AND column_name = 'magpie_key_name')
UNION ALL
SELECT 'billing_records.estimated'
WHERE EXISTS (SELECT 1 FROM information_schema.columns
              WHERE table_schema = 'public' AND table_name = 'billing_records'
                AND column_name = 'estimated')
UNION ALL
SELECT 'billing_records.upstream_usage'
WHERE EXISTS (SELECT 1 FROM information_schema.columns
              WHERE table_schema = 'public' AND table_name = 'billing_records'
                AND column_name = 'upstream_usage');
