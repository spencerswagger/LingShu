-- 渠道无独立状态：渠道=配置模板，可用性由该渠道下各密钥（channel_keys.state）与
-- 各内部模型（channel_models.state）决定。此处删除渠道级休眠列 channels.state
-- 与渠道级批量状态事件表 channel_events（不再有渠道级状态流转）。
-- 使用 IF EXISTS 保证幂等（旧环境可能已手工删除 / 表可能不存在）。
-- 0001_init.sql 保持不动（迁移不重写历史）。
ALTER TABLE channels DROP COLUMN IF EXISTS state;
DROP TABLE IF EXISTS channel_events;
