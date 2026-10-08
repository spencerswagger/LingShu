-- ===== 基线：渠道多密钥 + 会话持久化 + 账单维度 + 安全/雪花 ID =====
-- 由原 0001~0013（缺 0012）合并演化的最终基线，为预览版全新库设计，无需兼容旧库。
-- 含破坏性变更：channels 删凭据/enabled；billing/probe 改挂 channel_key_id；全库删 channels.enabled / channel_models.enabled；
-- users 删除 group_id；追加昵称与安全字段（token_version/must_change_password/TOTP）。
-- 全部业务表主键为 BIGINT（应用层 idgen 雪花 ID 生成，无自增默认）。

CREATE TABLE IF NOT EXISTS users (
    id                    BIGINT PRIMARY KEY,
    username              TEXT NOT NULL,
    password_hash         TEXT NOT NULL,
    role                  TEXT NOT NULL CHECK (role IN ('ADMIN','DEVELOPER')),
    status                TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','DISABLED')),
    pricing_mode          TEXT NOT NULL DEFAULT 'sale' CHECK (pricing_mode IN ('sale','cost')),
    nickname              TEXT NOT NULL DEFAULT '',
    is_system             BOOLEAN NOT NULL DEFAULT false,
    must_change_password  BOOLEAN NOT NULL DEFAULT false,
    token_version         INTEGER NOT NULL DEFAULT 0,
    totp_secret_cipher    TEXT,
    totp_enabled          BOOLEAN NOT NULL DEFAULT false,
    totp_recovery_hashes  JSONB,
    totp_last_step        BIGINT NOT NULL DEFAULT 0,
    deleted_at            TIMESTAMPTZ DEFAULT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_users_active ON users(username) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS credit_wallets (
    user_id    BIGINT PRIMARY KEY REFERENCES users(id),
    balance    NUMERIC(20,5) NOT NULL DEFAULT 0,
    version    BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS credit_flows (
    id             BIGINT PRIMARY KEY,
    user_id        BIGINT NOT NULL REFERENCES users(id),
    type           TEXT NOT NULL CONSTRAINT credit_flows_type_check CHECK (type IN ('recharge','consume','adjust','set')),
    amount         NUMERIC(20,5) NOT NULL,
    ref_billing_id TEXT,
    session_id     TEXT,
    remark         TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_credit_flows_user ON credit_flows(user_id, created_at DESC);

-- 渠道 = 配置模板（无凭据、无 enabled，可用性只认 state）
CREATE TABLE IF NOT EXISTS channels (
    id                 BIGINT PRIMARY KEY,
    name               TEXT NOT NULL,
    protocol           TEXT NOT NULL DEFAULT 'openai-compat',
    base_url           TEXT NOT NULL,
    tags               JSONB NOT NULL DEFAULT '{}',
    priority           INT NOT NULL DEFAULT 100,
    weight             INT NOT NULL DEFAULT 1,
    state              TEXT NOT NULL DEFAULT 'NORMAL' CHECK (state IN ('NORMAL','DRAIN','DISABLED')),
    rate_limit         JSONB NOT NULL DEFAULT '{}',
    health_probe       JSONB NOT NULL DEFAULT '{}',
    reliability        JSONB NOT NULL DEFAULT '{}',
    session_ttl_minutes INT NOT NULL DEFAULT 60,
    deleted_at         TIMESTAMPTZ DEFAULT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_channels_active ON channels(name) WHERE deleted_at IS NULL;

-- 渠道密钥 = 运行时实体（每密钥独立状态机/探测/限流/会话计数；共享渠道配置）
CREATE TABLE IF NOT EXISTS channel_keys (
    id            BIGINT PRIMARY KEY,
    channel_id    BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    credential_enc TEXT NOT NULL,          -- SM4 密文（唯一真实凭据）
    state         TEXT NOT NULL DEFAULT 'NORMAL' CHECK (state IN ('NORMAL','DRAIN','DISABLED')),
    last_err      TEXT NOT NULL DEFAULT '',
    deleted_at    TIMESTAMPTZ DEFAULT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_channel_keys_active ON channel_keys(channel_id, name) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_channel_keys_channel ON channel_keys(channel_id);

-- 密钥级状态流转记录
CREATE TABLE IF NOT EXISTS channel_key_events (
    id             BIGINT PRIMARY KEY,
    channel_key_id BIGINT NOT NULL REFERENCES channel_keys(id) ON DELETE CASCADE,
    from_state     TEXT NOT NULL,
    to_state       TEXT NOT NULL,
    reason         TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_channel_key_events_key ON channel_key_events(channel_key_id, created_at DESC);

-- 渠道级批量操作事件（渠道=批量操作层，无 key 维度）
CREATE TABLE IF NOT EXISTS channel_events (
    id         BIGINT PRIMARY KEY,
    channel_id BIGINT NOT NULL REFERENCES channels(id),
    from_state TEXT NOT NULL,
    to_state   TEXT NOT NULL,
    reason     TEXT NOT NULL,
    deleted_at TIMESTAMPTZ DEFAULT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_channel_events_channel ON channel_events(channel_id, created_at DESC);

CREATE TABLE IF NOT EXISTS semantic_tags (
    id          BIGINT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    kv_pairs    JSONB NOT NULL DEFAULT '{}',
    enabled     BOOLEAN NOT NULL DEFAULT true,
    created_by  BIGINT REFERENCES users(id),
    deleted_at  TIMESTAMPTZ DEFAULT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_semantic_tags_active ON semantic_tags(name) WHERE deleted_at IS NULL;

-- 对外模型：售价层（enabled 保留，属对外启停语义）
CREATE TABLE IF NOT EXISTS external_models (
    id            BIGINT PRIMARY KEY,
    external_name TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    enabled       BOOLEAN NOT NULL DEFAULT true,
    sale_rates    JSONB NOT NULL DEFAULT '{"input":1.0,"output":2.0,"cache_read":0.1,"cache_write":0.3,"reasoning":1.0}',
    time_config   JSONB,
    context_tiers JSONB,
    deleted_at    TIMESTAMPTZ DEFAULT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_external_models_active ON external_models(external_name) WHERE deleted_at IS NULL;

-- 渠道内部模型：配置实体（无 enabled；模型级三态 state 保留）
CREATE TABLE IF NOT EXISTS channel_models (
    id                BIGINT PRIMARY KEY,
    channel_id        BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    internal_model_id TEXT NOT NULL,
    external_model_id BIGINT NOT NULL REFERENCES external_models(id),
    cost_rates        JSONB NOT NULL DEFAULT '{"input":0.8,"output":1.6,"cache_read":0.05,"cache_write":0.2,"reasoning":0.8}',
    time_config       JSONB,
    context_tiers     JSONB,
    state             TEXT NOT NULL DEFAULT 'NORMAL' CHECK (state IN ('NORMAL','DRAIN','DISABLED')),
    rate_limit        JSONB NOT NULL DEFAULT '{}',
    health_probe      JSONB NOT NULL DEFAULT '{}',
    reliability       JSONB NOT NULL DEFAULT '{}',
    deleted_at        TIMESTAMPTZ DEFAULT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_channel_models_active ON channel_models(channel_id, internal_model_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_channel_models_ext ON channel_models(external_model_id);

-- 模型级状态流转记录（模型属渠道配置）
CREATE TABLE IF NOT EXISTS channel_model_events (
    id          BIGINT PRIMARY KEY,
    channel_id  BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    model_id    TEXT NOT NULL,
    from_state  TEXT NOT NULL,
    to_state    TEXT NOT NULL,
    reason      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_cm_events_model ON channel_model_events(channel_id, model_id, created_at DESC);

-- 渠道与语义标签多对多关联：路由以本关联表为准。
CREATE TABLE IF NOT EXISTS channel_tags (
    channel_id BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    tag_id     BIGINT NOT NULL REFERENCES semantic_tags(id) ON DELETE CASCADE,
    PRIMARY KEY (channel_id, tag_id)
);

CREATE TABLE IF NOT EXISTS tokens (
    id            BIGINT PRIMARY KEY,
    token_hash    TEXT NOT NULL UNIQUE,
    token_display TEXT NOT NULL,
    user_id       BIGINT NOT NULL REFERENCES users(id),
    display_name  TEXT NOT NULL,
    tag_id        BIGINT REFERENCES semantic_tags(id),
    expires_at    TIMESTAMPTZ,
    last_used_at  TIMESTAMPTZ,
    status        TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','DISABLED')),
    secret_cipher TEXT,
    deleted_at    TIMESTAMPTZ DEFAULT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tokens_user ON tokens(user_id);

-- 账单：维度 = 用户 / 消费者令牌 / 渠道密钥 / 会话
CREATE TABLE IF NOT EXISTS billing_records (
    id                  BIGINT PRIMARY KEY,
    billing_id          TEXT NOT NULL UNIQUE,
    user_id             BIGINT NOT NULL REFERENCES users(id),
    pricing_mode        TEXT NOT NULL,
    token_id            BIGINT REFERENCES tokens(id),
    external_model_name TEXT NOT NULL,
    internal_model_id   TEXT NOT NULL,
    channel_key_id      BIGINT REFERENCES channel_keys(id),  -- 可空：拒绝类账单（未进入渠道）无密钥维度
    session_id          TEXT,
    session_name        TEXT NOT NULL DEFAULT '',
    call_time           TIMESTAMPTZ NOT NULL DEFAULT now(),
    tokens              JSONB NOT NULL DEFAULT '{}',
    rates               JSONB NOT NULL DEFAULT '{}',
    coefficients        JSONB NOT NULL DEFAULT '{}',
    r_value             INT NOT NULL,
    raw_total           NUMERIC(20,5),
    credits_consumed    NUMERIC(20,5),
    cost_credits        NUMERIC(20,5),
    balance_before      NUMERIC(20,5),
    balance_after       NUMERIC(20,5),
    status              TEXT NOT NULL DEFAULT 'completed' CHECK (status IN ('completed','failed')),
    error_message       TEXT,
    duration_ms         BIGINT,
    first_token_ms      BIGINT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_billing_user_time ON billing_records(user_id, call_time DESC);
CREATE INDEX IF NOT EXISTS idx_billing_key_time ON billing_records(channel_key_id, call_time DESC);
CREATE INDEX IF NOT EXISTS idx_billing_session ON billing_records(session_id);
CREATE INDEX IF NOT EXISTS idx_billing_call_time ON billing_records(call_time DESC);
CREATE INDEX IF NOT EXISTS idx_billing_token_time ON billing_records(token_id, call_time DESC);
CREATE INDEX IF NOT EXISTS idx_billing_status_time ON billing_records(status, call_time DESC);

-- 会话持久化（运行时以内存为准，DB 为投影）
CREATE TABLE IF NOT EXISTS sessions (
    session_id        TEXT PRIMARY KEY,
    user_id           BIGINT NOT NULL REFERENCES users(id),
    token_id          BIGINT REFERENCES tokens(id),
    model             TEXT NOT NULL,
    session_raw       TEXT NOT NULL DEFAULT '',
    name              TEXT NOT NULL DEFAULT '',
    closed            BOOLEAN NOT NULL DEFAULT false,
    channel_key_id    BIGINT NOT NULL REFERENCES channel_keys(id),
    internal_model_id TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_active       TIMESTAMPTZ NOT NULL DEFAULT now(),
    expire_at         TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id, expire_at);
CREATE INDEX IF NOT EXISTS idx_sessions_key ON sessions(channel_key_id);
CREATE INDEX IF NOT EXISTS idx_sessions_token ON sessions(token_id);

-- 探测历史（密钥级；model_id 空=密钥默认探测目标）
CREATE TABLE IF NOT EXISTS probe_logs (
    id             BIGINT PRIMARY KEY,
    channel_key_id BIGINT NOT NULL REFERENCES channel_keys(id) ON DELETE CASCADE,
    model_id       TEXT NOT NULL DEFAULT '',
    level          TEXT NOT NULL DEFAULT 'key' CHECK (level IN ('key','model')),
    target         TEXT NOT NULL,
    ok             BOOLEAN NOT NULL,
    error          TEXT NOT NULL DEFAULT '',
    input_tokens   INT NOT NULL DEFAULT 0,
    output_tokens  INT NOT NULL DEFAULT 0,
    cached_tokens  INT NOT NULL DEFAULT 0,
    total_tokens   INT NOT NULL DEFAULT 0,
    duration_ms    INT NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_probe_logs_key ON probe_logs(channel_key_id, created_at DESC);

CREATE TABLE IF NOT EXISTS announcements (
    id         BIGINT PRIMARY KEY,
    title      TEXT NOT NULL,
    content    TEXT NOT NULL,
    level      TEXT NOT NULL DEFAULT 'info' CHECK (level IN ('info','warning','danger')),
    publish_at TIMESTAMPTZ,
    expire_at  TIMESTAMPTZ,
    enabled    BOOLEAN NOT NULL DEFAULT true,
    created_by BIGINT REFERENCES users(id),
    deleted_at TIMESTAMPTZ DEFAULT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sys_configs (
    key        TEXT PRIMARY KEY,
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by BIGINT REFERENCES users(id)
);

-- 审计日志（安全加固；user_id 可空，用户名冗余快照便于审计追责）
CREATE TABLE IF NOT EXISTS audit_logs (
    id          BIGINT PRIMARY KEY,
    user_id     BIGINT NULL,
    -- 审计列用 text：列宽不应成为"能不能被审计"的决定因素
    -- （varchar 超长在 PG 会报错，会让超长用户名/长路径的请求审计静默丢失）。
    username    TEXT NULL,
    action      TEXT NOT NULL,
    target_type TEXT NULL,
    target_id   TEXT NULL,
    detail      JSONB NULL,
    request_id  TEXT NULL,
    ip          TEXT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_logs_created_at ON audit_logs(created_at DESC);
CREATE INDEX idx_audit_logs_user ON audit_logs(user_id);

CREATE TABLE IF NOT EXISTS watchlist_items (
    id                BIGINT PRIMARY KEY,
    external_model_id TEXT NOT NULL,
    local_model_name  TEXT NOT NULL,
    alert_on_change   BOOLEAN NOT NULL DEFAULT true,
    last_synced_at    TIMESTAMPTZ,
    prompt_price      NUMERIC(20,5),
    completion_price  NUMERIC(20,5),
    deleted_at        TIMESTAMPTZ DEFAULT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 部分唯一索引：同一对外模型仅允许一条未删除的关注项（软删除后可重建），
-- 供 UpsertWatchlist 以 ON CONFLICT 原子收敛，避免查询后插入的并发双活。
CREATE UNIQUE INDEX IF NOT EXISTS uq_watchlist_items_active ON watchlist_items(external_model_id) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS price_change_alerts (
    id                BIGINT PRIMARY KEY,
    admin_id          BIGINT REFERENCES users(id),
    watchlist_item_id BIGINT REFERENCES watchlist_items(id),
    external_model_id TEXT NOT NULL,
    local_model_name  TEXT NOT NULL,
    changes           JSONB NOT NULL DEFAULT '{}',
    status            TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','resolved','ignored')),
    detected_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at       TIMESTAMPTZ
);

-- 全局上下文分档默认置空
INSERT INTO sys_configs(key, value) VALUES ('billing.context_tiers', '[]'::jsonb) ON CONFLICT (key) DO UPDATE SET value = '[]'::jsonb;
