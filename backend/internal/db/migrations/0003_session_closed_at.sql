-- 会话关闭时间列：用于「已关闭会话保留期」判定（关闭的会话在保留期内仍出现在管理列表，
-- 超过保留期后才随清扫清理）。仅内存已有 ClosedAt，此处补 DB 侧快照。
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS closed_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_sessions_closed ON sessions(closed_at);