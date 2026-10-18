-- 0015_protocol_error_stats：协议级错误统计表。
--
-- 网关在命中渠道协议错误策略（error_policy）时，按「错误类别」(category) 对当日 (day)
-- 计数 +1。category 为匹配到的错误类别标识，如 status:422 或 code:<errCode>。
-- 主键 (protocol_id, category, day) 支撑直接 upsert 累加与按类别聚合查询。
CREATE TABLE IF NOT EXISTS protocol_error_stats (
    protocol_id BIGINT NOT NULL,
    category    TEXT   NOT NULL,
    day         DATE   NOT NULL,
    count       BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (protocol_id, category, day)
);
