-- 0016_channel_protocol_features：渠道协议增加「协议特性」列。
--
-- features 为协议级特性数组（JSONB）：每项 { key, params }，key 为代码实现并注册的
-- 特性标识（如 thinking-roundtrip / tool-schema-normalize），params 为该特性的参数。
-- 默认 '[]'（未挂载任何特性）→ 行为与改造前完全一致（默认安全态）。
ALTER TABLE channel_protocols ADD COLUMN IF NOT EXISTS features JSONB NOT NULL DEFAULT '[]'::jsonb;
