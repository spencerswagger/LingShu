-- 会话关闭状态：Close 后不命中路由、释放并发槽，但记录保留（管理列表可见）；ALTER 兼容已应用 0001/0002 的既有库。
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS closed BOOLEAN NOT NULL DEFAULT false;