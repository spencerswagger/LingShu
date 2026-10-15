-- 渠道协议：由外挂预设包 provider 条目物化而来的接入模板
--（baseURL / protocols / auth / 特殊请求头 / 模型清单），独立于渠道存在，可被多个渠道引用。
-- 仅 BaseURL 不同时不新建协议：直接改渠道级 base_url 即可。
CREATE TABLE IF NOT EXISTS channel_protocols (
    id               BIGINT PRIMARY KEY,
    name             TEXT NOT NULL,
    base_url         TEXT NOT NULL DEFAULT '',
    protocols        JSONB NOT NULL DEFAULT '{}',
    auth             JSONB NOT NULL DEFAULT '{}',
    request_headers  JSONB NOT NULL DEFAULT '{}',
    models           JSONB NOT NULL DEFAULT '{}',
    preset_source_id TEXT NOT NULL DEFAULT '',
    -- 是否被管理员编辑过：false=与预设一致（同步），true=已编辑（同步前需二次确认）
    edited           BOOLEAN NOT NULL DEFAULT false,
    -- 预设来源状态：ACTIVE=来源正常；STALE=预设条目已删除/改名（协议保留，标记失效）
    source_status    TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (source_status IN ('ACTIVE','STALE')),
    deleted_at       TIMESTAMPTZ DEFAULT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_channel_protocols_active ON channel_protocols(name) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_channel_protocols_preset ON channel_protocols(preset_source_id);

-- 渠道可引用渠道协议；未引用时走渠道内联配置（channels.protocol + channels.base_url）。
-- 外键不带 ON DELETE：预设条目删除时不级联删协议，避免破坏引用它的渠道（改标记 STALE）。
ALTER TABLE channels ADD COLUMN IF NOT EXISTS channel_protocol_id BIGINT REFERENCES channel_protocols(id);
CREATE INDEX IF NOT EXISTS idx_channels_protocol ON channels(channel_protocol_id);

-- 渠道-模型级可达协议子集：覆盖渠道协议中该模型的定义；
-- NULL = 继承渠道协议中该模型的 protocols。
ALTER TABLE channel_models ADD COLUMN IF NOT EXISTS protocols JSONB;
