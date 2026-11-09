-- 回滚 0019（手工执行，**不要放进 migrations/**，否则会被自动应用）。
--
-- 使用前提：已把后端二进制回滚到 0019 之前的版本（老代码不读 convert_enabled /
-- convert_targets / edited，多出来的列不影响它的显式列清单查询）。
--
-- 行为等价性：
--   * 转换配置本来就在 channel_protocols.models 里，0019 只是复制到实例列，DROP 不丢配置；
--   * 模型级协议子集同理（种子仍在），实例列里多出来的副本对老代码只是「等同于种子值」；
--   * edited 是纯守卫标记，老代码不用它，DROP 无副作用；
--   * channels.base_url 全程未被 0019 修改（引用协议时它只是被运行时忽略），回滚后老代码
--     继续按原值转发，地址与迁移前逐行等值。
--
-- 脚本为纯 SQL（psql 与自动化测试都可直接执行）：核对用 SELECT 只是打印结果，不改数据。

BEGIN;

-- 1) 核对影响面：三列的行数分布（迁移前应能看到 convert_enabled 有值的行数 = 种子里配了转换的模型数）。
SELECT count(*)                                              AS channel_models_total,
       count(*) FILTER (WHERE edited)                        AS edited_rows,
       count(*) FILTER (WHERE convert_enabled IS NOT NULL)   AS convert_enabled_set
FROM channel_models
WHERE deleted_at IS NULL;

-- 2) 删列（老代码不依赖）。
ALTER TABLE channel_models
  DROP COLUMN IF EXISTS convert_enabled,
  DROP COLUMN IF EXISTS convert_targets,
  DROP COLUMN IF EXISTS edited;

-- 3) 迁移记录回退，便于重新应用 0019（表不存在时跳过，纯 SQL 兼容空库/测试库）。
DO $$
BEGIN
    IF to_regclass('public.applied_migrations') IS NOT NULL THEN
        DELETE FROM applied_migrations WHERE name = '0019_channel_field_ownership.sql';
    END IF;
END $$;

-- 4) 回滚后自检：三列应已不存在（期望 0 行）。
SELECT count(*) AS leftover_columns
FROM information_schema.columns
WHERE table_name = 'channel_models'
  AND column_name IN ('convert_enabled', 'convert_targets', 'edited');

COMMIT;
