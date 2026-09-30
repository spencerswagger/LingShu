-- 会话可读名称：网关首条消息摘要自动生成，支持手动改名；ALTER 兼容已应用 0001 的既有库。
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS name TEXT NOT NULL DEFAULT '';