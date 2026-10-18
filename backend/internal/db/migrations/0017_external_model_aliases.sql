-- 0017_external_model_aliases：对外模型增加「别名」列。
--
-- aliases 为该对外模型的别名数组（JSONB）：请求命中任一别名时解析到本条对外模型，
-- 沿用既有路由/计费链路；别名仅用于模型名解析，不出现在 GET /v1/models。
-- 默认 '[]'（无别名）→ 行为与改造前逐字节一致（默认安全态）。
-- 编号取当前最大迁移 +1（0016 已被 channel_protocol_features 占用），故为 0017。
ALTER TABLE external_models ADD COLUMN IF NOT EXISTS aliases JSONB NOT NULL DEFAULT '[]'::jsonb;