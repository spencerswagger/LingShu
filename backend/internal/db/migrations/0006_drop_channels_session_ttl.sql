-- 渠道级会话过期时长（channels.session_ttl_minutes）已废弃：会话过期改由系统配置
-- session.expiration 的「上下文长度 + 用户对话次数」双维度分档计算（取较大者，最低兜底 5 分钟）。
-- 此处删除该列；0001_init.sql 保持不动（迁移不重写历史）。
-- 使用 IF EXISTS 保证幂等（旧环境可能已手工删除）。注意：DROP COLUMN 不可逆，原值不可恢复。
ALTER TABLE channels DROP COLUMN IF EXISTS session_ttl_minutes;