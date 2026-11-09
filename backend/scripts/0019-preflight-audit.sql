-- 0019 上线前预检（只读，不写库）：盘点存量与受影响面。
--
-- 用途：
--   1) 给出「渠道 / 协议 / 模型」三张表的存量计数（评估回填影响面的分母）；
--   2) 列出 0019 会改变「有效上游基址」的渠道——按拍板「引用协议不覆盖基址」，
--      引用协议且 channels.base_url 非空的渠道，上线后走的是**协议地址**而不是
--      channels.base_url。这些渠道必须在上线前逐个确认：要么接受协议地址，
--      要么为它显式新建一条协议（基址写正确），改引新协议后再上线；
--   3) 预估 0019 会标记 edited=true 的行数（漂移行，同步不再覆盖）。
--
-- 用法：psql "$DSN" -f scripts/0019-preflight-audit.sql
-- 建议在只读账号 / 只读事务下执行（全部语句是 SELECT）。

\echo '=== 1) 存量计数（分母）==='
SELECT 'channels' AS table_name, count(*) AS rows_live FROM channels WHERE deleted_at IS NULL
UNION ALL
SELECT 'channel_protocols', count(*) FROM channel_protocols WHERE deleted_at IS NULL
UNION ALL
SELECT 'channel_models', count(*) FROM channel_models WHERE deleted_at IS NULL
UNION ALL
SELECT 'channel_keys', count(*) FROM channel_keys WHERE deleted_at IS NULL;

\echo '=== 2) 引用协议且内联 base_url 非空（上线后有效基址会变）==='
SELECT c.id,
       c.name,
       c.base_url                              AS inline_base_url_ignored_after_0019,
       cp.id                                   AS protocol_id,
       cp.name                                 AS protocol_name,
       cp.base_url                             AS protocol_base_url,
       (cp.protocols -> 'openai-chat' ->> 'baseUrl') AS protocol_openai_chat_base_url
FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE c.deleted_at IS NULL
  AND btrim(c.base_url) <> ''
ORDER BY c.id;

\echo '=== 2b) 其中「协议也没配基址」的渠道（上线后会打空地址，必须先处理）==='
SELECT c.id, c.name, c.base_url
FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE c.deleted_at IS NULL
  AND btrim(c.base_url) <> ''
  AND btrim(cp.base_url) = ''
  AND COALESCE(btrim(cp.protocols -> 'openai-chat' ->> 'baseUrl'), '') = ''
ORDER BY c.id;

\echo '=== 3) 预估被标记 edited=true 的模型行（种子与实例双方都有值且不同）==='
SELECT count(*) AS drift_rows_to_mark_edited
FROM channel_models cm
JOIN channels c ON c.id = cm.channel_id AND c.deleted_at IS NULL
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE cm.deleted_at IS NULL
  AND cp.models ? cm.internal_model_id
  AND cp.models -> cm.internal_model_id ?| ARRAY[
        'externalModelID','protocols','costRates','timeConfig','contextTiers'];

\echo '=== 4) 种子缺 costRates 但实例成本为空的行（B1 遗留，0019 后由协议调价自动补上）==='
SELECT count(*) AS empty_cost_rows
FROM channel_models cm
JOIN channels c ON c.id = cm.channel_id AND c.deleted_at IS NULL
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE cm.deleted_at IS NULL
  AND cp.models ? cm.internal_model_id
  AND NOT (cp.models -> cm.internal_model_id ? 'costRates')
  AND (cm.cost_rates IS NULL OR cm.cost_rates = '{}'::jsonb);
