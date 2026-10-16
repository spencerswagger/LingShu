-- Auto 模型（对外模型之上的有序别名组）：
-- auto_models 定义别名实体（全局 scope='global' / 私有 scope='private'），
-- auto_model_members 定义该别名的有序对外模型成员（position 升序即优先级）。
--
-- 设计要点（见 docs/designs/2026-10-16-auto-model-alias-design.md）：
--   * Auto 不直接绑定渠道，成员才是绑定实体；因此不触碰 channel_models 唯一约束。
--   * Auto 名与 external_models.external_name 不重名、Auto 不可嵌套 —— 应用层双向校验
--     （Auto 侧在 automodel.Service，对外模型侧在 model.Service）。
--   * 单一扁平命名空间：任意两个 Auto 不得同名（不论 scope、不论 owner），
--     由 uq_auto_models_name 一条部分唯一索引物理保证（跨作用域也唯一）。
--   * 账单/调用日志新增 auto_model_name 记录「请求的 Auto 名」（非 Auto 请求为 NULL）；
--     既有 external_model_name 语义保持为「实际命中的对外模型名」，统计口径不变。
--
-- 全部语句使用 IF NOT EXISTS / ADD COLUMN IF NOT EXISTS 保证幂等；不修改历史迁移、不改既有列语义。

CREATE TABLE IF NOT EXISTS auto_models (
    id            BIGINT PRIMARY KEY,
    name          TEXT NOT NULL,
    scope         TEXT NOT NULL CHECK (scope IN ('global','private')),
    owner_user_id BIGINT NULL REFERENCES users(id),   -- global 为 NULL；private 必填
    enabled       BOOLEAN NOT NULL DEFAULT true,
    description   TEXT NOT NULL DEFAULT '',
    deleted_at    TIMESTAMPTZ DEFAULT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- private 必须归属某用户；global 必须不归属任何用户。
    CONSTRAINT ck_auto_models_owner CHECK (
        (scope = 'global' AND owner_user_id IS NULL) OR
        (scope = 'private' AND owner_user_id IS NOT NULL)
    )
);
-- 单一扁平命名空间：名称全局唯一（跨作用域、跨 owner），一条索引彻底堵住同名共存。
CREATE UNIQUE INDEX IF NOT EXISTS uq_auto_models_name
    ON auto_models(name) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS auto_model_members (
    id                BIGINT PRIMARY KEY,
    auto_model_id     BIGINT NOT NULL REFERENCES auto_models(id) ON DELETE CASCADE,
    external_model_id BIGINT NOT NULL REFERENCES external_models(id),
    position          INT NOT NULL,                    -- 升序即优先级：越小越先被尝试
    deleted_at        TIMESTAMPTZ DEFAULT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 同一 Auto 内排序位唯一（应用层已校验，此处 DB 兜底并发）。
CREATE UNIQUE INDEX IF NOT EXISTS uq_auto_members_position
    ON auto_model_members(auto_model_id, position) WHERE deleted_at IS NULL;

-- 账单：记录请求的 Auto 名（非 Auto 请求为 NULL）。与既有 external_model_name（命中成员名）并存。
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS auto_model_name TEXT NULL;
-- 调用日志：同步记录请求的 Auto 名，保持与账单可关联的决策轨迹可读。
ALTER TABLE call_logs ADD COLUMN IF NOT EXISTS auto_model_name TEXT NULL;