-- 0019_channel_field_ownership：渠道 / 渠道协议字段归属收口（唯一运行时真源）。
--
-- 契约：docs/designs/2026-11-10-channel-field-ownership-contract.md（DRAFT-v1 + 拍板结论）。
-- 口径（四层切死）：
--   channel_protocols        = 跨渠道复用的模板（含「物化种子」models）
--   channels                 = 单渠道实例（身份 / 路由 / 运行额度）
--   channel_models           = 每渠道每模型的唯一运行时真源（网关不再读协议种子）
--   channel_keys             = 凭据
--
-- 本迁移做四件事（全部幂等，可重复执行）：
--   (a) 加列：channel_models.convert_enabled / convert_targets / edited
--   (b) 存量：把协议种子里的模型级 protocols 复制进实例行（原先运行时回落读种子，
--       降级为种子后必须落库，否则存量「模型级协议子集」行为会丢）
--   (c) 存量：把协议种子里的 convertEnabled / convertTargets 下沉到实例列（原先运行时读种子）
--   (d) 存量：实例行与种子「双方都有值且不一致」时标记 edited=true（物化守卫，防止下次同步静默覆盖）
--
-- 为什么没有 base_url_override 列：按拍板「引用协议不覆盖基址」——引用协议的渠道基址只跟随
-- 被引用协议，需要不同基址就显式新建协议。存量 channels.base_url 保持原值不动（运行时忽略，
-- 回滚时老代码仍按原值工作，零数据破坏）；上线前的影响面用 scripts/0019-preflight-audit.sql 盘点。
--
-- 回滚：scripts/rollback-0019.sql（**不放在 migrations/ 下**，避免被自动应用）。

-- ---------- (a) 加列 ----------

-- 跨协议转换：按拍板「转换按协议定义、不按渠道差异化」，本列是协议种子的物化副本，
-- 网关运行时读它（唯一真源），渠道层不提供差异化写入。
ALTER TABLE channel_models ADD COLUMN IF NOT EXISTS convert_enabled BOOLEAN;                -- NULL = 继承全局开关
ALTER TABLE channel_models ADD COLUMN IF NOT EXISTS convert_targets JSONB NOT NULL DEFAULT '[]'::jsonb;

-- 物化守卫：实例行商业字段被本地编辑过 → 协议同步不再覆盖（重置接口是其唯一出口）。
ALTER TABLE channel_models ADD COLUMN IF NOT EXISTS edited BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN channel_models.convert_enabled IS
    '是否允许跨协议转换（协议种子物化副本；NULL=继承全局开关）';
COMMENT ON COLUMN channel_models.convert_targets IS
    '跨协议转换目标协议偏好顺序（协议种子物化副本；空=按默认偏好）';
COMMENT ON COLUMN channel_models.edited IS
    '商业字段是否被本地编辑过；true=协议同步不再覆盖该行（reset-from-protocol 置回 false）';

-- ---------- (b) 模型级协议子集下沉 ----------
-- 语义等价：旧运行时是「实例 protocols 为空 → 回落种子 models[m].protocols → 回落协议 protocols」，
-- 新运行时不读种子，因此这里把种子里有、实例里空的那一档复制过来。
-- 只写「实例为空」的行：实例已有的值就是本地显式配置，绝不覆盖。
UPDATE channel_models cm
SET protocols = cp.models -> cm.internal_model_id -> 'protocols',
    updated_at = now()
FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE cm.channel_id = c.id
  AND cm.deleted_at IS NULL
  AND cp.models ? cm.internal_model_id
  AND cp.models -> cm.internal_model_id ? 'protocols'
  AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'protocols') = 'object'
  AND (cp.models -> cm.internal_model_id -> 'protocols') <> '{}'::jsonb
  AND (cm.protocols IS NULL OR cm.protocols = '{}'::jsonb OR jsonb_typeof(cm.protocols) <> 'object');

-- ---------- (c) 转换配置下沉 ----------
-- 只写种子里存在的键；缺省键保留实例现值（convert_targets NOT NULL，不能落 NULL）。
UPDATE channel_models cm
SET convert_enabled = CASE WHEN cp.models -> cm.internal_model_id ? 'convertEnabled'
                           THEN (cp.models -> cm.internal_model_id ->> 'convertEnabled')::boolean
                           ELSE cm.convert_enabled END,
    convert_targets = CASE WHEN cp.models -> cm.internal_model_id ? 'convertTargets'
                           THEN (cp.models -> cm.internal_model_id -> 'convertTargets')
                           ELSE cm.convert_targets END,
    updated_at = now()
FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE cm.channel_id = c.id
  AND cm.deleted_at IS NULL
  AND cp.models ? cm.internal_model_id
  AND cp.models -> cm.internal_model_id ?| ARRAY['convertEnabled','convertTargets'];

-- ---------- (d) 漂移标记 edited ----------
-- 判定口径（与拍板「字段级置位」一致）：
--   * 只看种子里确实定义了的键（种子没定义的键与该行配置无关，不参与判定，否则会永久冻结同步）；
--   * 只看「双方都有值且不等」——实例侧为空说明还没被配置过，不算本地编辑（否则 B1 留下的
--     空成本行会被永久冻结，永远拿不到协议调价）；
--   * 成本比较必须做键名映射：种子 costRates 是 PascalCase（billing.WireRates），
--     实例 cost_rates 是 snake（billing.Rates），直接整体 IS DISTINCT FROM 恒为真会误标全表；
--   * 数值按 numeric 比较（1 与 1.0 不算差异），非数字脏值按「不相等」跳过，绝不让迁移中断。
UPDATE channel_models cm
SET edited = true
FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE cm.channel_id = c.id
  AND cm.deleted_at IS NULL
  AND cp.models ? cm.internal_model_id
  AND cp.models -> cm.internal_model_id ?| ARRAY[
        'externalModelID','protocols','costRates','timeConfig','contextTiers']
  AND (
    -- 对外模型绑定：种子有值、实例有值且不同。
    (cp.models -> cm.internal_model_id ? 'externalModelID'
     AND NULLIF(btrim(cp.models -> cm.internal_model_id ->> 'externalModelID'), '') IS NOT NULL
     AND cm.external_model_id IS DISTINCT FROM
         NULLIF(btrim(cp.models -> cm.internal_model_id ->> 'externalModelID'), '')::bigint)
    -- 模型级协议子集：双方都有内容且不同（键序无关，jsonb 已归一）。
    OR (cp.models -> cm.internal_model_id ? 'protocols'
        AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'protocols') = 'object'
        AND (cp.models -> cm.internal_model_id -> 'protocols') <> '{}'::jsonb
        AND cm.protocols IS NOT NULL
        AND jsonb_typeof(cm.protocols) = 'object'
        AND cm.protocols <> '{}'::jsonb
        AND cm.protocols IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'protocols'))
    -- 成本三段：逐键按 numeric 比较（仅比较种子定义了的键）。
    OR (cp.models -> cm.internal_model_id ? 'costRates'
        AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'costRates') = 'object'
        AND cm.cost_rates IS NOT NULL
        AND jsonb_typeof(cm.cost_rates) = 'object'
        AND cm.cost_rates <> '{}'::jsonb
        AND (
             (cp.models -> cm.internal_model_id -> 'costRates' ? 'Input'
              AND (cp.models -> cm.internal_model_id -> 'costRates' ->> 'Input') ~ '^-?[0-9]+(\.[0-9]+)?$'
              AND COALESCE((cm.cost_rates ->> 'input') ~ '^-?[0-9]+(\.[0-9]+)?$', false)
              AND (cm.cost_rates ->> 'input')::numeric
                  IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'costRates' ->> 'Input')::numeric)
          OR (cp.models -> cm.internal_model_id -> 'costRates' ? 'Output'
              AND (cp.models -> cm.internal_model_id -> 'costRates' ->> 'Output') ~ '^-?[0-9]+(\.[0-9]+)?$'
              AND COALESCE((cm.cost_rates ->> 'output') ~ '^-?[0-9]+(\.[0-9]+)?$', false)
              AND (cm.cost_rates ->> 'output')::numeric
                  IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'costRates' ->> 'Output')::numeric)
          OR (cp.models -> cm.internal_model_id -> 'costRates' ? 'CacheRead'
              AND (cp.models -> cm.internal_model_id -> 'costRates' ->> 'CacheRead') ~ '^-?[0-9]+(\.[0-9]+)?$'
              AND COALESCE((cm.cost_rates ->> 'cache_read') ~ '^-?[0-9]+(\.[0-9]+)?$', false)
              AND (cm.cost_rates ->> 'cache_read')::numeric
                  IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'costRates' ->> 'CacheRead')::numeric)
        ))
    -- 时段系数 / 上下文分档：双方都有内容且不同。
    OR (cp.models -> cm.internal_model_id ? 'timeConfig'
        AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'timeConfig') = 'object'
        AND cm.time_config IS NOT NULL
        AND jsonb_typeof(cm.time_config) = 'object'
        AND cm.time_config IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'timeConfig'))
    OR (cp.models -> cm.internal_model_id ? 'contextTiers'
        AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'contextTiers') = 'array'
        AND jsonb_array_length(cp.models -> cm.internal_model_id -> 'contextTiers') > 0
        AND cm.context_tiers IS NOT NULL
        AND jsonb_typeof(cm.context_tiers) = 'array'
        AND jsonb_array_length(cm.context_tiers) > 0
        AND cm.context_tiers IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'contextTiers'))
  );
