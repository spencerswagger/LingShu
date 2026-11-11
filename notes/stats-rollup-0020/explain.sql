-- explain.sql —— 证明「本人摘要三档」走预聚合表；并给出 cost/stats 真实计划
-- 参数口径：今天 = 2026-11-12（Asia/Shanghai），本月 1 日 = 2026-11-01，昨日 = 2026-11-11，30 天窗口 = 2026-10-13..2026-11-12
\echo '=== A. /auth/me/summary 三档（UserDayTotals，改造后）==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT COALESCE(SUM(credits) FILTER (WHERE day = DATE '2026-11-12'), 0),
       COALESCE(SUM(credits) FILTER (WHERE day = DATE '2026-11-11'), 0),
       COALESCE(SUM(credits) FILTER (WHERE day >= DATE '2026-11-01'), 0),
       COALESCE(SUM(input_tokens) FILTER (WHERE day >= DATE '2026-11-01'), 0),
       COALESCE(SUM(cache_read_tokens) FILTER (WHERE day >= DATE '2026-11-01'), 0)
FROM user_daily_stats
WHERE user_id = 900000000 AND day BETWEEN LEAST(DATE '2026-11-11', DATE '2026-11-01') AND DATE '2026-11-12';

\echo '=== B. 同一份数据用旧写法（改造前 handler 的三次明细扫描，只取本月一段代表）==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT COALESCE(SUM(credits_consumed), 0)
FROM billing_records
WHERE user_id = 900000000 AND status = 'completed'
  AND call_time >= TIMESTAMPTZ '2026-11-01 00:00:00+08' AND call_time <= TIMESTAMPTZ '2026-11-12 23:59:59+08';

\echo '=== C. cost/stats（改造后：30 天窗口 + 实体列）==='
EXPLAIN (ANALYZE, BUFFERS)
WITH res AS (SELECT DISTINCT ON (cm.channel_id, cm.internal_model_id)
           cm.channel_id, cm.internal_model_id, cm.id AS cm_id, cm.external_model_id
    FROM channel_models cm
    ORDER BY cm.channel_id, cm.internal_model_id, (cm.deleted_at IS NULL) DESC, cm.id DESC)
SELECT CASE
           WHEN r.cm_id IS NOT NULL THEN 'cm:' || r.cm_id::text
           WHEN b.internal_model_id <> ''
               THEN 'name:' || b.external_model_name || '#' || b.internal_model_id || '@' || COALESCE(k.channel_id, 0)::text
           ELSE 'name:' || b.external_model_name || '@' || COALESCE(k.channel_id, 0)::text
       END AS entity_key,
       MAX(k.channel_id)                    AS channel_id,
       MAX(COALESCE(c.name, ''))            AS channel_name,
       MAX(COALESCE(r.cm_id, 0))            AS channel_model_id,
       MAX(COALESCE(r.external_model_id, 0)) AS external_model_id,
       MAX(b.internal_model_id)             AS internal_model_id,
       MAX(COALESCE(em.external_name, b.external_model_name)) AS external_model_name,
       COALESCE(SUM(b.cost_credits), 0)     AS cost_credits,
       COALESCE(SUM(b.credits_consumed), 0) AS sale_credits,
       COUNT(*)                             AS requests,
       COALESCE(SUM(b.input_tokens), 0) AS input_tokens,
       COALESCE(SUM(b.cache_read_tokens), 0) AS cache_read_tokens,
       COALESCE(SUM(b.output_tokens), 0) AS output_tokens,
       to_json(array_agg(DISTINCT b.external_model_name))::text AS snapshot_names
FROM billing_records b
LEFT JOIN channel_keys k ON k.id = b.channel_key_id
LEFT JOIN (SELECT DISTINCT ON (cm.channel_id, cm.internal_model_id)
           cm.channel_id, cm.internal_model_id, cm.id AS cm_id, cm.external_model_id
    FROM channel_models cm
    ORDER BY cm.channel_id, cm.internal_model_id, (cm.deleted_at IS NULL) DESC, cm.id DESC) r
       ON r.channel_id = k.channel_id AND r.internal_model_id = b.internal_model_id
LEFT JOIN channels c ON c.id = k.channel_id
LEFT JOIN external_models em ON em.id = r.external_model_id
WHERE b.status = 'completed'
  AND b.call_time >= TIMESTAMPTZ '2026-10-13 00:00:00+08' AND b.call_time <= TIMESTAMPTZ '2026-11-12 23:59:59+08'
  AND NOT (
        b.pricing_mode = 'cost'
        AND b.token_id IS NOT NULL
        AND b.token_id IN (
            SELECT t.id FROM tokens t JOIN users u ON u.id = t.user_id
            WHERE u.is_system OR t.display_name = 'system-probe'
        )
        AND NOT EXISTS (SELECT 1 FROM call_logs cl WHERE cl.billing_id = b.billing_id)
    )
GROUP BY 1
ORDER BY COALESCE(SUM(b.cost_credits), 0) DESC NULLS LAST, 1;

\echo '=== D. cost/stats 旧写法（改造前：无时间窗 + 每行 3 次 JSONB 正则，取计划不 ANALYZE 以免跑几分钟）==='
EXPLAIN
SELECT CASE
           WHEN r.cm_id IS NOT NULL THEN 'cm:' || r.cm_id::text
           WHEN b.internal_model_id <> ''
               THEN 'name:' || b.external_model_name || '#' || b.internal_model_id || '@' || COALESCE(k.channel_id, 0)::text
           ELSE 'name:' || b.external_model_name || '@' || COALESCE(k.channel_id, 0)::text
       END AS entity_key,
       COALESCE(SUM(b.cost_credits), 0),
       COUNT(*),
       COALESCE(SUM(CASE WHEN (b.tokens->>'input') ~ '^-?[0-9]{1,18}$' THEN (b.tokens->>'input')::bigint ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN (b.tokens->>'cache_read') ~ '^-?[0-9]{1,18}$' THEN (b.tokens->>'cache_read')::bigint ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN (b.tokens->>'output') ~ '^-?[0-9]{1,18}$' THEN (b.tokens->>'output')::bigint ELSE 0 END), 0)
FROM billing_records b
LEFT JOIN channel_keys k ON k.id = b.channel_key_id
LEFT JOIN (SELECT DISTINCT ON (cm.channel_id, cm.internal_model_id)
           cm.channel_id, cm.internal_model_id, cm.id AS cm_id, cm.external_model_id
    FROM channel_models cm
    ORDER BY cm.channel_id, cm.internal_model_id, (cm.deleted_at IS NULL) DESC, cm.id DESC) r
       ON r.channel_id = k.channel_id AND r.internal_model_id = b.internal_model_id
LEFT JOIN channels c ON c.id = k.channel_id
LEFT JOIN external_models em ON em.id = r.external_model_id
WHERE b.status = 'completed'
GROUP BY 1
ORDER BY 2 DESC NULLS LAST, 1;
