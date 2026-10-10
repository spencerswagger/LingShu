-- call_logs：调用日志（rc1 问题 2+3）——记录每次网关请求的请求增量、响应内容与平台决策轨迹。
-- 与 billing_records 按 billing_id 1:1 关联；无账单（候选全部被拦截等拒绝场景）时 billing_id 为空字符串。
-- 会话完整记录 = 同一 session_id 的 call_logs 按 created_at 升序拼接（不新增会话消息表）。
CREATE TABLE IF NOT EXISTS call_logs (
    id               BIGINT PRIMARY KEY,
    billing_id       TEXT NOT NULL DEFAULT '' UNIQUE,      -- 关联 billing_records.billing_id（拒绝场景为空串）
    request_id       TEXT NOT NULL DEFAULT '',             -- 网关请求链路 ID（审计/日志关联）
    session_id       TEXT NOT NULL DEFAULT '',             -- 归属会话；空串 = 无会话
    user_id          BIGINT NULL,                          -- 消费用户（拒绝场景可为空）
    channel_id       BIGINT NULL,                          -- 最终命中渠道（可为空）
    channel_key_id   BIGINT NULL,                          -- 最终命中渠道密钥
    internal_model_id TEXT NOT NULL DEFAULT '',            -- 最终命中内部模型
    external_model_id BIGINT NULL,                         -- 最终命中对外模型
    model            TEXT NOT NULL DEFAULT '',             -- 用户请求的对外模型名
    pricing_mode     TEXT NOT NULL DEFAULT '',             -- sale / cost
    status           TEXT NOT NULL DEFAULT 'completed' CHECK (status IN ('completed','failed')),
    req_messages     JSONB NULL,                           -- 请求增量：与上次请求 diff 出的新增消息（role/content/tool_calls…）
    resp_body        TEXT NOT NULL DEFAULT '',             -- 非流式完整响应体；流式为结构化提取的 assistant 增量 JSON
    resp_kind        TEXT NOT NULL DEFAULT 'none' CHECK (resp_kind IN ('non_stream','stream','error','none')),
    decision         JSONB NULL,                           -- 决策轨迹：候选渠道与原因、时段/分档系数、预扣与结算
    error_message    TEXT NOT NULL DEFAULT '',
    duration_ms      BIGINT NULL,
    first_token_ms   BIGINT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_call_logs_session ON call_logs(session_id, created_at ASC);
CREATE INDEX IF NOT EXISTS idx_call_logs_request ON call_logs(request_id);
CREATE INDEX IF NOT EXISTS idx_call_logs_user_time ON call_logs(user_id, created_at DESC);