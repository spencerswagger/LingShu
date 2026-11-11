-- seed-30m.sql —— 独立压测数据集（Gate 复核用，2026-11-11）
--
-- 与仓库 backend/scripts/bench-stats/seed.sql 的差异（刻意为之）：
--   1) **不写** 0020 新增的三段 token 实体列 —— 即真实上线时的存量库状态
--      （ADD COLUMN ... NOT NULL DEFAULT 0 是元数据操作，存量行一律为 0），
--      这样启动回填的 token 列阶段才真的跑 3000 万行更新，而不是「0 行」。
--   2) 用户 900000000 提为 ADMIN，且 must_change_password=false —— 压测要直接打管理端只读接口。
--   3) 其余分布与仓库脚本逐条一致（top 用户占 40%、call_time 铺最近 90 天、3 渠道 3 模型），
--      便于与设计文档里的数字对照。
--
-- 用法：psql -d qa_stats30m -v rows=30000000 -f seed-30m.sql

\set ON_ERROR_STOP on

BEGIN;

DELETE FROM billing_records;
DELETE FROM channel_models;
DELETE FROM external_models;
DELETE FROM call_logs;
DELETE FROM tokens WHERE id BETWEEN 900000000 AND 999999999;
DELETE FROM channel_keys WHERE id BETWEEN 900000000 AND 999999999;
DELETE FROM channels WHERE id BETWEEN 900000000 AND 999999999;
DELETE FROM users WHERE id BETWEEN 900000000 AND 999999999;

INSERT INTO users (id, username, password_hash, role, pricing_mode, nickname, must_change_password)
SELECT 900000000 + g, 'bench_user_' || g, 'x',
       CASE WHEN g = 0 THEN 'ADMIN' ELSE 'DEVELOPER' END,
       'sale', 'bench ' || g, false
FROM generate_series(0, 99) g;

INSERT INTO channels (id, name, protocol, base_url)
SELECT 900000000 + g, 'bench-channel-' || g, 'openai-chat', 'https://bench' || g || '.example.com'
FROM generate_series(0, 2) g;

INSERT INTO channel_keys (id, channel_id, name, credential_enc)
SELECT 900000000 + g, 900000000 + g, 'bench-key-' || g, 'x'
FROM generate_series(0, 2) g;

INSERT INTO external_models (id, external_name, enabled)
SELECT 900000000 + g, 'bench-model-' || g, true
FROM generate_series(0, 2) g;

INSERT INTO channel_models (id, channel_id, internal_model_id, external_model_id)
SELECT 900000000 + g, 900000000 + g, 'int-model-' || g, 900000000 + g
FROM generate_series(0, 2) g;

INSERT INTO tokens (id, token_hash, token_display, user_id, display_name, secret_cipher)
SELECT 900000000 + g, 'hash-' || g, 'bench-token-' || g, 900000000 + g, 'bench-token-' || g, 'x'
FROM generate_series(0, 99) g;

INSERT INTO billing_records (
    id, billing_id, user_id, pricing_mode, token_id, external_model_name, internal_model_id,
    channel_key_id, session_id, session_name, call_time, tokens, rates, coefficients, r_value,
    raw_total, credits_consumed, cost_credits, status, duration_ms, first_token_ms)
SELECT i,
       'bench-' || i,
       CASE WHEN (i % 10) < 4 THEN 900000000 ELSE 900000001 + (i % 99) END,
       'sale',
       900000000 + (i % 100),
       'bench-model-' || (i % 3),
       'int-model-' || (i % 3),
       900000000 + (i % 3),
       'sess-' || (i % 5000),
       '',
       now() - ((i % (90 * 24 * 60)) || ' minutes')::interval,
       jsonb_build_object('input', 500 + (i % 700), 'cache_read', i % 50, 'output', 200 + (i % 300)),
       '{"input":1,"output":2,"cache_read":0.1}'::jsonb,
       '{"time":1,"context":1}'::jsonb,
       10000,
       ((i % 1000) / 100.0)::numeric(20, 5),
       ((i % 1000) / 100.0)::numeric(20, 5),
       ((i % 1000) / 120.0)::numeric(20, 5),
       'completed',
       100 + (i % 5000),
       20 + (i % 300)
FROM generate_series(1, :rows) i;

COMMIT;

ANALYZE billing_records;
