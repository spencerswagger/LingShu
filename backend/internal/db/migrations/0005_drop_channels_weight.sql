-- 渠道权重（channels.weight）已废弃：新会话在同一优先级层内改由「成本最低 + 存活会话数」决定，
-- 不再使用权重加权随机。此处删除该休眠列；0001_init.sql 保持不动（迁移不重写历史）。
-- 使用 IF EXISTS 保证幂等（旧环境可能已手工删除）。
ALTER TABLE channels DROP COLUMN IF EXISTS weight;