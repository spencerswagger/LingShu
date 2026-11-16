-- 0019_v2_facade.sql
--
-- 灵枢 V2 第一期（加壳不换芯）唯一迁移：**纯 DDL、零 DML、零回填、可重复执行**。
--
-- 本期口径：
--   * 新增 2 张表：magpie_keys（灵枢令牌 ↔ 代理侧 gateway key 的一对一映射）、
--                  reconcile_windows（每 60 秒对账窗口结果，只告警、绝不作账单依据）；
--   * 新增 4 列：external_models.magpie_model（对外名→代理模型名映射）、
--                billing_records.magpie_key_name（本次调用的 callerKey，对账分组维度）、
--                billing_records.estimated（无上游 usage → 按请求体估算）、
--                billing_records.upstream_usage（上游 usage 原文；调用日志停写后的落点）；
--   * 新增 2 个部分索引（历史 NULL 行不进索引）。
--
-- 本文件**不含任何 DROP / RENAME / UPDATE / DELETE**：旧表（channels / channel_keys /
-- channel_models / channel_events / probe_logs / channel_protocols / protocol_error_stats /
-- call_logs / sessions / auto_models / auto_model_members 等）与数据零改动，仅停止读写。
--
-- 回滚：见 docs/plans/2026-11-15-v2-p1-rollback.md（删两新表 + 四列 + 两索引 + revert 代码）。
--
-- 注意：迁移框架每文件一个事务（internal/db/migrate.go），因此本文件禁止写入任何业务回填语句。

-- ① 灵枢令牌 ↔ 代理侧 gateway key：一令牌一把，绝不复用。
--    token_id 不加 ON DELETE CASCADE：令牌走 deleted_at 软删，物理删除不在本期口径内。
CREATE TABLE IF NOT EXISTS magpie_keys (
    id             BIGINT PRIMARY KEY,                     -- 雪花 id，应用侧 idgen 生成，无 DEFAULT
    token_id       BIGINT NOT NULL REFERENCES tokens(id),
    key_name       TEXT   NOT NULL UNIQUE,                 -- 代理侧 callerKey，也是对账分组维度
    key_id         TEXT,                                   -- 代理侧内部 key id，可空（POC 未证实字段名）
    key_cipher     TEXT   NOT NULL,                        -- 密文，口径同 tokens.secret_cipher
    state          TEXT   NOT NULL DEFAULT 'PENDING'
                   CHECK (state IN ('ACTIVE','PENDING','DISABLED')),
    last_synced_at TIMESTAMPTZ,
    last_error     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (token_id)                                      -- 「一令牌一把」落约束，不靠约定
);

-- ② 60 秒对账结果：只告警，绝不回写账单。
--    dev_pct 是**窗口整体**偏差（分组偏差进 detail，一列只挂一种语义）；
--    alarm_streak 写时计算（读上一窗 +1），不在查询里用窗口函数现算。
CREATE TABLE IF NOT EXISTS reconcile_windows (
    id               BIGINT PRIMARY KEY,
    window_start     TIMESTAMPTZ NOT NULL,
    window_end       TIMESTAMPTZ NOT NULL,
    pulled_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    req_count_ls     INT NOT NULL DEFAULT 0,
    req_count_magpie INT NOT NULL DEFAULT 0,
    tokens_ls        JSONB NOT NULL DEFAULT '{}',          -- {input,output,cache_read}
    tokens_magpie    JSONB NOT NULL DEFAULT '{}',
    dev_pct          NUMERIC(6,3),                         -- 窗口整体偏差（按 token 加权）；error/skipped 时 NULL
    status           TEXT NOT NULL DEFAULT 'ok'
                     CHECK (status IN ('ok','alarm','error','skipped')),
    alarm_streak     SMALLINT NOT NULL DEFAULT 0,           -- 连续 alarm 窗数，3 → 升级
    detail           JSONB,                                 -- 分组明细 + 判据快照
    UNIQUE (window_start, window_end)                       -- ON CONFLICT DO UPDATE 幂等；该索引即满足「最近 N 窗」查询
);

-- ③ 对外模型 → 代理模型名映射。NULL = 未映射/不可用（不转发）。
ALTER TABLE external_models ADD COLUMN IF NOT EXISTS magpie_model TEXT;

-- ④ 账单侧三列。历史行 magpie_key_name 恒 NULL，所有按 key 聚合/对账的查询必须带
--    WHERE magpie_key_name IS NOT NULL（部分索引同样只覆盖非 NULL 行）。
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS magpie_key_name TEXT;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS estimated BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS upstream_usage JSONB;

-- ⑤ 索引：两个对外名不得打同一代理模型（否则对账按模型聚合会合并 → 误报）。
CREATE UNIQUE INDEX IF NOT EXISTS uq_external_models_magpie_model
    ON external_models (magpie_model) WHERE magpie_model IS NOT NULL;

-- 账单按 callerKey + 时间检索（对账窗口取数）：部分索引，历史 NULL 行不进。
CREATE INDEX IF NOT EXISTS idx_billing_magpie_key_time
    ON billing_records (magpie_key_name, call_time DESC) WHERE magpie_key_name IS NOT NULL;