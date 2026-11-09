-- 0019_channel_field_ownership：渠道 / 渠道协议字段归属收口（唯一运行时真源）。
--
-- 契约：docs/designs/2026-11-10-channel-field-ownership-contract.md（RULED-v2，四项口径已裁定）。
-- 口径（四层切死）：
--   channel_protocols        = 跨渠道复用的模板（含「物化种子」models）
--   channels                 = 单渠道实例（身份 / 路由 / 运行额度）；基址只有「内联」一种来源
--   channel_models           = 每渠道每模型的唯一运行时真源（网关不再读协议种子）
--   channel_keys             = 凭据
--
-- 与 v1 相比的收敛（按裁定）：新增业务列由 4 个降为 1 个（channel_models.edited），
-- 不加任何渠道级基址覆盖列（裁定①），不把转换配置下沉到实例（裁定②）。
--
-- 本迁移做四件事（全部幂等，可重复执行）：
--   (a) 加列 channel_models.edited + 建留档表 channel_base_url_archive
--   (b) 引用协议的渠道：原内联基址/协议类型先归档再清空（基址随协议，可无损回滚）
--   (c) 存量：把协议种子里的模型级 protocols 复制进实例行（原先运行时回落读种子，
--       降级为种子后必须落库，否则存量「模型级协议子集」行为会丢）
--   (d) 存量：实例行与种子「双方都有值且不一致」时标记 edited=true（物化守卫）
--
-- 回滚：scripts/rollback-0019.sql（**不放在 migrations/ 下**，避免被自动应用）。
-- 回填规模：≤5 万行由本迁移单语句完成；>5 万行改用 scripts/backfill-0019（分批 + 断点 + 幂等），
-- 因迁移框架每文件一个事务、无法断点续跑（见契约 §10）。

-- ---------- (a) 加列 + 留档表 ----------

-- 物化守卫：实例行商业字段被本地编辑过 → 协议同步不再覆盖（重置接口是其唯一出口）。
ALTER TABLE channel_models ADD COLUMN IF NOT EXISTS edited BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN channel_models.edited IS
    '商业字段（绑定/成本/时段/分档/协议子集）是否被本地编辑过；true=协议同步不再覆盖该行（reset-from-protocol 置回 false）';

-- 留档：裁定①要求引用协议的渠道不带内联基址，清空前先原样存一份，使回滚可无损还原。
-- 只做留档，不进任何业务读路径；行数 ≈ 引用协议的渠道数（channels.protocol 有默认值，故按 OR 判定）。
CREATE TABLE IF NOT EXISTS channel_base_url_archive (
    channel_id  BIGINT PRIMARY KEY,
    base_url    TEXT NOT NULL DEFAULT '',
    protocol    TEXT NOT NULL DEFAULT '',
    archived_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE channel_base_url_archive IS
    '0019 留档：引用协议的渠道在「基址随协议」改造中被清空的内联 base_url / protocol（仅供回滚还原，无业务读取）';

-- ---------- (b)(c)(d) 数据段：受行数阈值守卫（方案 A）----------
-- channel_models 未删除行数 > 阈值时：**只做 DDL，跳过数据段**，并显式告警；
-- 数据段交给 scripts/backfill-0019（分批 5000 + backfill_checkpoints 断点续跑 + 幂等）执行。
-- 原因：迁移框架每个 .sql 只包一个事务，>5 万行时无法分批/断点（契约 §10.2）。
-- 阈值是**单一开关**：Go 侧常量 Backfill0019InlineThreshold，经 renderMigration() 替换占位符。
-- 顺序不可调换：(b) 归档清空 → (c) 协议子集下移 → (d) 漂移标记。
-- (c) 必须先于 (d)：否则「实例 protocols 为空」的行会先被判成漂移标 edited，下移被跳过，
-- 模型级协议子集永久丢失且该行被冻结（契约 §6.1 顺序条款）。
DO $$
DECLARE
    n         bigint;
    threshold bigint := {{backfill_threshold}};
BEGIN
    SELECT count(*) INTO n FROM channel_models WHERE deleted_at IS NULL;
    IF n > threshold THEN
        RAISE WARNING '0019: channel_models 现有 % 行（内联回填阈值 %），迁移只做了 DDL，数据回填已跳过；请执行 cd backend && go run ./scripts/backfill-0019 -dsn "$DSN"（可中断重跑、幂等），完成前实例行缺少模型级协议子集。', n, threshold;
    ELSE
    -- ---------- (b) 归档并清空「引用协议」渠道的内联基址与协议类型 ----------

    -- 只处理「引用了仍在生效的协议」的渠道：协议已软删的渠道保持原值不动——
    -- 那种状态下运行时解析不到协议基址，清空只会丢掉唯一可恢复的地址（等人工修复，不是迁移的事）。
    INSERT INTO channel_base_url_archive(channel_id, base_url, protocol)
    SELECT c.id, btrim(c.base_url), btrim(c.protocol)
    FROM channels c
    JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
    WHERE c.deleted_at IS NULL
      AND (btrim(c.base_url) <> '' OR btrim(c.protocol) <> '')
    ON CONFLICT (channel_id) DO NOTHING;

    UPDATE channels c
    SET base_url = '',
        protocol = '',
        updated_at = now()
    FROM channel_protocols cp
    WHERE cp.id = c.channel_protocol_id
      AND cp.deleted_at IS NULL
      AND c.deleted_at IS NULL
      AND (btrim(c.base_url) <> '' OR btrim(c.protocol) <> '');

    -- ---------- (c) 模型级协议子集下沉 ----------
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

    -- ---------- (d) 漂移标记 edited ----------
    -- 判定口径（裁定④「字段级置位」在 SQL 侧的对齐）：
    --   * 只看种子里确实定义了的键（种子没定义的键与该行配置无关，不参与判定，否则会永久冻结同步）；
    --   * 只看「双方都有值且不等」——实例侧为空说明还没被配置过，不算本地编辑（否则 B1 留下的
    --     空成本行会被永久冻结，永远拿不到协议调价）；
    --   * 成本比较必须做键名映射：种子 costRates 是 PascalCase（billing.WireRates），
    --     实例 cost_rates 是 snake（billing.Rates），直接整体 IS DISTINCT FROM 恒为真会误标全表；
    --   * 数值按 numeric 比较（1 与 1.0 不算差异），非数字脏值按「不参与比较」跳过，绝不让迁移中断。
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
            AND (cp.models -> cm.internal_model_id -> 'timeConfig') <> '{}'::jsonb
            AND cm.time_config IS NOT NULL
            AND jsonb_typeof(cm.time_config) = 'object'
            -- 空对象 = 未设置（与 cost_rates / protocols / context_tiers 同口径），不算漂移；
            -- 缺这一条会把 time_config='{}' 的行误标 edited，从而永久冻结协议同步。
            AND cm.time_config <> '{}'::jsonb
            AND cm.time_config IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'timeConfig'))
        OR (cp.models -> cm.internal_model_id ? 'contextTiers'
            AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'contextTiers') = 'array'
            AND jsonb_array_length(cp.models -> cm.internal_model_id -> 'contextTiers') > 0
            AND cm.context_tiers IS NOT NULL
            AND jsonb_typeof(cm.context_tiers) = 'array'
            AND jsonb_array_length(cm.context_tiers) > 0
            AND cm.context_tiers IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'contextTiers'))
      );
    END IF;
END $$;
