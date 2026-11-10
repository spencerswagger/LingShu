-- 回滚 0019（手工执行，**不要放进 migrations/**，否则会被自动应用）。
--
-- 使用前提：已把后端二进制回滚到 0019 之前的版本。
-- 老代码读 channels / channel_models 用的是显式列清单，多一个 edited 列不影响查询；
-- 被清空的内联基址/协议类型由 channel_base_url_archive 原样还原，因此迁移**有损点为零**。
--
-- 脚本为纯 SQL（psql 与自动化测试都可直接执行）：核对用 SELECT 只打印结果，不改数据。

BEGIN;

-- 1) 还原被清空的内联基址与协议类型（裁定①的直接后果，回滚必须可逆）。
UPDATE channels c
SET base_url   = a.base_url,
    protocol   = a.protocol,
    updated_at = now()
FROM channel_base_url_archive a
WHERE c.id = a.channel_id
  AND c.deleted_at IS NULL
  AND c.channel_protocol_id IS NOT NULL
  AND btrim(c.base_url) = '';

-- 2) 核对：还原后仍为空基址的「引用协议」渠道数（期望 0，除非迁移期间手工清过）。
SELECT count(*) AS unrestored_channels
FROM channels c
JOIN channel_base_url_archive a ON a.channel_id = c.id
WHERE c.deleted_at IS NULL
  AND btrim(c.base_url) = ''
  AND btrim(a.base_url) <> '';

-- 3) 删列与留档表（留档表只在 0019 语境下有意义：回滚后老代码不读它）。
ALTER TABLE channel_models DROP COLUMN IF EXISTS edited;
DROP TABLE IF EXISTS channel_base_url_archive;

-- 4) 迁移记录回退，便于重新应用 0019（表不存在时跳过，兼容空库/测试库）。
DO $$
BEGIN
    IF to_regclass('public.applied_migrations') IS NOT NULL THEN
        DELETE FROM applied_migrations WHERE name = '0019_channel_field_ownership.sql';
    END IF;
END $$;

-- 5) 回滚后自检：列与表应已不存在（期望 0 / false）。
SELECT count(*) AS leftover_columns
FROM information_schema.columns
WHERE table_name = 'channel_models' AND column_name = 'edited';

SELECT to_regclass('public.channel_base_url_archive') IS NULL AS archive_table_dropped;

COMMIT;
