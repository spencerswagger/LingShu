-- 探测②（对象层 / 数据层）：本期**保留但不许写入**的账单列。期望 0 行。
--
-- ⚠️ 口径变更 1（2026-11-16 裁定 A）：`magpie_keys` 表、`external_models.magpie_model` 列与
--    `uq_external_models_magpie_model` 索引**已改为本链的正当对象**（落在
--    `0019b_v2_passthrough_credentials.sql`），不再是「旧分支独有」。
--    所以它们从本探测的「期望不存在」移到探测④的「记录在 → 对象必须在」。
--    照旧口径把 magpie_keys 留在这里 = 干净库上永远 1 行、每次误报。
--
-- ⚠️ 口径变更 2（2026-11-16 裁定 B，复审指出的互斥）：`billing_records` 的
--    `magpie_key_name / estimated / upstream_usage` **三列本期保留**（0019b 对 billing_records
--    零改动，三列只随阶段二的新形处理），因此「列存在」**不再是异常**，本文件不再按存在性判红。
--    改守数据层事实：这三列**不许有新数据**。
--
--    ⚠️ 只守数据层是不够的：列还在 → 误写会**静默成功**（不报错、账单照出），所以代码级
--       「写入方为 0」由 `internal/domain/billing` 的 TestBilling_LegacyColumnsHaveNoWriter 守
--       （扫 billing/facadebill 非测试源码 + 在「三列存在」的库上插一条账单断言三列仍 NULL）。
--       两者合起来才是 0024 探测② 的完整口径，缺一条就漏。
--
-- 历史库若已被旧分支写过，那些行属历史数据、不算本批缺陷；本探测只保证**本期之后不新增**。
--
-- 期望输出：0 行。

-- 三列可能**整体不存在**（干净库就是这个状态，且是正常的）：所以先判列，再动态查数据，
-- 不能让「列不存在」把探测本身打成 SQL 错误。
-- psql 自动提交模式下每条语句各自成事务，所以**不能**用 ON COMMIT DROP（临时表会立刻消失）；
-- 用 DROP IF EXISTS 保证可重复执行。
DROP TABLE IF EXISTS _probe2;
CREATE TEMP TABLE _probe2(verdict text, n bigint);

DO $probe2$
BEGIN
    IF (SELECT count(*) FROM information_schema.columns
         WHERE table_schema = 'public' AND table_name = 'billing_records'
           AND column_name IN ('magpie_key_name', 'estimated', 'upstream_usage')) = 3 THEN
        EXECUTE $q$INSERT INTO _probe2
                  SELECT 'billing_records 存在本批新增的旧口径数据', count(*)
                    FROM billing_records
                   WHERE magpie_key_name IS NOT NULL
                      OR estimated IS NOT NULL
                      OR upstream_usage IS NOT NULL$q$;
    END IF;
END
$probe2$;

SELECT verdict, n AS rows_with_legacy_values FROM _probe2 WHERE n > 0;
DROP TABLE IF EXISTS _probe2;
