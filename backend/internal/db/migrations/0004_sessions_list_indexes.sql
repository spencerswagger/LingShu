-- 会话列表改为直接读 sessions 表（含历史会话）后，补充列表与恢复所需索引：
--   - last_active：列表按 last_active DESC 排序 + 分页；
--   - expire_at：启动恢复（LoadActive）与「未过期/已过期」过滤，避免全表扫描。
CREATE INDEX IF NOT EXISTS idx_sessions_last_active ON sessions(last_active DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_expire ON sessions(expire_at);
