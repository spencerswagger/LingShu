-- 0014_channel_protocol_error_policy：渠道协议增加「错误策略」列。
--
-- error_policy 为协议级错误处置策略数组（JSONB）：命中错误签名后决定
-- 「是否计入渠道健康错误率 / 是否 failover / 触发动作(none|drain|disable) / 下游状态码」。
-- 默认 '[]'（未配置）→ 行为与改造前完全一致（默认安全态）。
ALTER TABLE channel_protocols ADD COLUMN IF NOT EXISTS error_policy JSONB NOT NULL DEFAULT '[]'::jsonb;
