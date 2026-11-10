-- 0019 上线前预检（只读，不写库）：盘点存量与受影响面。
--
-- 对应契约 RULED-v2 §10.3「上线前需要 spencer 给的三个数」，一次跑完拿全。
--
-- 用途：
--   1) 「渠道 / 协议 / 模型」三张表的存量计数（回填影响面的分母；本地 lingshu_test 快照为 0/0/0）；
--   2) 裁定①的影响面：引用协议且内联 base_url / protocol 非空的渠道——迁移会**归档后清空**，
--      这些渠道上线后的生效地址变成协议地址。需要独立地址的必须先显式新建协议再切引用；
--   3) 协议也没配基址的渠道（迁移后会打空地址，高危，必须先处理）；
--   4) 预估被标记 edited=true 的模型行数（首次同步的覆盖面）；
--   5) 种子缺 costRates 但实例成本为空的行（B1 遗留，迁移后由协议调价自动补上）。
--
-- 用法：psql "$DSN" -f scripts/0019-preflight-audit.sql（建议只读账号 / 只读事务）

\echo '=== 1) 存量计数（分母）==='
SELECT 'channels' AS table_name, count(*) AS rows_all, count(*) FILTER (WHERE deleted_at IS NULL) AS rows_live FROM channels
UNION ALL
SELECT 'channel_protocols', count(*), count(*) FILTER (WHERE deleted_at IS NULL) FROM channel_protocols
UNION ALL
SELECT 'channel_models', count(*), count(*) FILTER (WHERE deleted_at IS NULL) FROM channel_models
UNION ALL
SELECT 'channel_keys', count(*), count(*) FILTER (WHERE deleted_at IS NULL) FROM channel_keys;

\echo '=== 1b) 引用协议的渠道分布（按 channel_protocol_id 分组）==='
SELECT coalesce(channel_protocol_id, 0) AS protocol_id, count(*) AS channels
FROM channels WHERE deleted_at IS NULL
GROUP BY 1 ORDER BY 2 DESC;

\echo '=== 2) / 2a) 裁定①影响面：引用「生效中」协议 + 内联 base_url/protocol 非空（迁移会归档并清空，生效地址变协议地址）==='
SELECT c.id,
       c.name,
       c.base_url                                        AS inline_base_url_to_be_archived,
       c.protocol                                        AS inline_protocol_to_be_archived,
       cp.id                                             AS protocol_id,
       cp.name                                           AS protocol_name,
       cp.base_url                                       AS protocol_base_url
FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE c.deleted_at IS NULL
  AND (btrim(c.base_url) <> '' OR btrim(c.protocol) <> '')
ORDER BY c.id;

\echo '=== 2b) 高危：协议自身没有任何可用基址（清空后无地址可打，必须先补协议或改回内联）==='
-- 判定口径：协议级 base_url 为空，且**任意**协议键都没有配 baseUrl（不能只看 openai-chat——
-- 只给 anthropic-messages 配了基址的协议会被误报高危）。
SELECT c.id, c.name, c.base_url AS current_inline_base_url, cp.name AS protocol_name
FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE c.deleted_at IS NULL
  AND btrim(cp.base_url) = ''
  AND NOT EXISTS (
      SELECT 1 FROM jsonb_each(cp.protocols) AS p(name, spec)
      WHERE btrim(COALESCE(p.spec ->> 'baseUrl', '')) <> ''
  )
ORDER BY c.id;

\echo '=== 2c) 信息项：引用「已软删协议」的渠道（迁移不动它们；需人工修复引用）==='
-- S1 口径：协议软删后 lookupChannelProtocol 过滤 deleted_at → rt.Protocol=nil，
-- 网关回落读 channels.base_url/protocol，因此这两个内联值此刻就是生效配置，迁移不清空、不归档。
-- 这类渠道协议已不存在，属既有故障，需人工修复引用（重新激活/换协议，或确认改回内联）。
SELECT c.id,
       c.name,
       c.channel_protocol_id                                  AS referenced_protocol_id,
       btrim(c.base_url)                                      AS inline_base_url_kept,
       btrim(c.protocol)                                      AS inline_protocol_kept,
       cp.name                                                AS protocol_name,
       cp.deleted_at                                           AS protocol_deleted_at
FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NOT NULL
WHERE c.deleted_at IS NULL
ORDER BY c.id;

\echo '=== 3) 候选行数（上界）：种子定义了商业字段的模型行 ==='
SELECT count(*) AS drift_rows_to_mark_edited
FROM channel_models cm
JOIN channels c ON c.id = cm.channel_id AND c.deleted_at IS NULL
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE cm.deleted_at IS NULL
  AND cp.models ? cm.internal_model_id
  AND cp.models -> cm.internal_model_id ?| ARRAY[
        'externalModelID','protocols','costRates','timeConfig','contextTiers'];

\echo '=== 3b) 协议里配了转换的模型数（裁定②：转换仍由协议独占，这里只做需求核对）==='
SELECT count(*) AS protocols_with_convert
FROM channel_protocols cp
WHERE cp.deleted_at IS NULL
  AND EXISTS (
      SELECT 1 FROM jsonb_each(cp.models) AS m(key, value)
      WHERE m.value ?| ARRAY['convertEnabled','convertTargets']);

\echo '=== 4) 种子缺 costRates 但实例成本为空的行（B1 遗留，迁移后由协议调价自动补上）==='
SELECT count(*) AS empty_cost_rows
FROM channel_models cm
JOIN channels c ON c.id = cm.channel_id AND c.deleted_at IS NULL
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE cm.deleted_at IS NULL
  AND cp.models ? cm.internal_model_id
  AND NOT (cp.models -> cm.internal_model_id ? 'costRates')
  AND (cm.cost_rates IS NULL OR cm.cost_rates = '{}'::jsonb);

\echo '=== 5) 量级判断：>5 万行改用 scripts/backfill-0019（分批 + 断点 + 幂等）==='
SELECT CASE WHEN count(*) > 50000
            THEN '使用 scripts/backfill-0019（分批 5000 / 断点续跑）'
            ELSE '直接跑迁移 0019（单语句幂等回填）' END AS recommended_path,
       count(*) AS channel_models_live
FROM channel_models WHERE deleted_at IS NULL;
