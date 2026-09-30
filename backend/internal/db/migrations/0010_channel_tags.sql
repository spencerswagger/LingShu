-- 渠道与语义标签多对多关联：路由以本关联表为准。
CREATE TABLE IF NOT EXISTS channel_tags (
    channel_id BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    tag_id     BIGINT NOT NULL REFERENCES semantic_tags(id) ON DELETE CASCADE,
    PRIMARY KEY (channel_id, tag_id)
);
