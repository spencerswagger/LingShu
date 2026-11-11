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
  AND b.call_time >= now()-interval '30 days' AND b.call_time <= now()
  AND NOT (
        b.pricing_mode = 'cost'
        AND b.token_id IS NOT NULL
        AND EXISTS (
            SELECT 1 FROM tokens t JOIN users u ON u.id = t.user_id
            WHERE t.id = b.token_id AND (u.is_system OR t.display_name = 'system-probe')
        )
        AND NOT EXISTS (SELECT 1 FROM call_logs cl WHERE cl.billing_id = b.billing_id)
    )
GROUP BY 1
ORDER BY COALESCE(SUM(b.cost_credits), 0) DESC NULLS LAST, 1;
