-- seed.sql 统计/账单性能压测数据集（合成数据，只用于沙盒/本地库，绝不进生产）。
--
-- 分布刻意做成「真实世界形状」：单用户热度极度倾斜（top 用户占 40%），
-- 因为看板查询的痛点是「一个人一个月几十万行」，而不是全表均匀分布。
--
-- 用法：
--   psql -d lingshu_dev -v rows=1000000  -f scripts/bench-stats/seed.sql
--   psql -d lingshu_dev -v rows=30000000 -f scripts/bench-stats/seed.sql
-- 可重复执行（先清空 billing_records 与相关维度表）。

\set ON_ERROR_STOP on

BEGIN;

-- 1) 维度：100 个用户 / 3 渠道 / 3 密钥 / 3 对外模型 / 3 条模型配置 / 每用户 1 令牌
DELETE FROM billing_records;
DELETE FROM channel_models;
DELETE FROM external_models;
DELETE FROM tokens WHERE id BETWEEN 900000000 AND 999999999;
DELETE FROM channel_keys WHERE id BETWEEN 900000000 AND 999999999;
DELETE FROM channels WHERE id BETWEEN 900000000 AND 999999999;
DELETE FROM users WHERE id BETWEEN 900000000 AND 999999999;

INSERT INTO users (id, username, password_hash, role, pricing_mode, nickname)
SELECT 900000000 + g, 'bench_user_' || g, 'x', 'DEVELOPER', 'sale', 'bench ' || g
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

-- 2) 账单明细：call_time 铺在最近 90 天（含今天），
--    user_id 命中规则：user 900000000 占 40%（i % 10 < 4），其余 60% 摊给 900000001..900000099。
INSERT INTO billing_records (
    id, billing_id, user_id, pricing_mode, token_id, external_model_name, internal_model_id,
    channel_key_id, session_id, session_name, call_time, tokens, rates, coefficients, r_value,
    raw_total, credits_consumed, cost_credits, status, duration_ms, first_token_ms,
    input_tokens, cache_read_tokens, output_tokens)
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
       20 + (i % 300),
       500 + (i % 700),
       i % 50,
       200 + (i % 300)
FROM generate_series(1, :rows) i;

COMMIT;

ANALYZE billing_records;
ANALYZE user_daily_stats;
