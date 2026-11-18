-- 0019_v2_topology：V2 四层拓扑的三张表 + 对外模型绑定列。
--
-- 口径来源：docs/designs/2026-11-15-v2-p1-data-contract.md §2（本迁移是该文档的逐字落地）。
--   四层平级：账号 N:1 供应商 N:N 组/直通 N:1 外部模型。
--   灵枢只存「内部模型 → 外部模型」的绑定与画布坐标、账号的一对一财务扩展；
--   供应商/组/账号/模型的配置本体只在网关，本地是只读镜像。
--
-- 三个表各管一件事（不要混用）：
--   internal_models —— 网关同步的只读镜像（节点）。判活用 last_seen_at + stale，
--                      **不建 deleted_at**：真源在网关，软删列会与同步器语义打架
--                      （同步器每轮整批 upsert，删除判定只认「本轮未见」）。
--   model_links     —— 画布落位（坐标）。**不含绑定语义**：缺行 = 走自动布局，
--                      不影响售卖与计费；绑定真源只有 external_models.bound_internal_ref 一处。
--   subscriptions   —— 账号财务扩展，与网关账号一对一（uq_subscriptions_account 表达），
--                      不落账期累计 token：摊销按窗口现算，避免每请求写同一行的热点。
--
-- auto 组：同步器侧过滤，不入库（V2 不使用网关自动聚合的组）。
-- 配额窗口：不入库（易变状态 + 会变成第三份真源），进程内 TTL 缓存即可。
--
-- 幂等：全部 IF NOT EXISTS / ADD COLUMN IF NOT EXISTS；可回滚（DROP 三表 / DROP 列）。

-- 内部模型镜像（节点）
CREATE TABLE IF NOT EXISTS internal_models (
    ref          TEXT PRIMARY KEY,                                    -- 网关侧引用：provider/model 或 group/名称
    provider_ref TEXT NOT NULL DEFAULT '',                            -- 所属供应商（网关 provider）
    kind         TEXT NOT NULL DEFAULT 'direct' CHECK (kind IN ('group','direct')),
    display_name TEXT NOT NULL DEFAULT '',
    context_len  BIGINT NOT NULL DEFAULT 0,                            -- 网关未给则 0
    caps         JSONB NOT NULL DEFAULT '{}'::jsonb,                   -- vision/tools/thinking… 原样
    list_price   JSONB NOT NULL DEFAULT '{}'::jsonb,                   -- 网关牌价原样；换算只在一处
    pos_x        DOUBLE PRECISION NOT NULL DEFAULT 0,
    pos_y        DOUBLE PRECISION NOT NULL DEFAULT 0,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),                   -- 每轮同步命中即刷新 = 判活依据
    stale        BOOLEAN NOT NULL DEFAULT false,                       -- true=本轮未见（沿用上期快照，展示但标灰）
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_internal_models_provider ON internal_models(provider_ref);
CREATE INDEX IF NOT EXISTS idx_internal_models_kind     ON internal_models(kind);
CREATE INDEX IF NOT EXISTS idx_internal_models_live     ON internal_models(last_seen_at) WHERE stale = false;

-- 绑定真源（唯一）：NULL = 未绑定。
--   ⚠️ 事实校正（2026-11-16）：未绑定的模型**当前仍可被调用**——`/v1/models` 可见性与转发准入
--   的判据仍是 `enabled` + `magpie_model` 非空，**没有**按 bound_internal_ref 过滤。
--   本期只做「登记 + 管理端展示绑定状态（含前端未绑定提醒）」，不做拦截；
--   「未绑定即不售卖」是待产品拍板的售卖规则变更，成文后再改判据，别照这行注释去推断现有行为。
ALTER TABLE external_models ADD COLUMN IF NOT EXISTS bound_internal_ref TEXT;
CREATE INDEX IF NOT EXISTS idx_external_models_bound
    ON external_models(bound_internal_ref) WHERE bound_internal_ref IS NOT NULL;

-- 画布落位（不含绑定语义；缺行 = 自动布局）
-- 不建到 internal_models 的外键：同步器会整批替换镜像，外键会把同步顺序变成约束问题。
CREATE TABLE IF NOT EXISTS model_links (
    internal_ref      TEXT   NOT NULL,
    external_model_id BIGINT NOT NULL REFERENCES external_models(id) ON DELETE CASCADE,
    pos_x             DOUBLE PRECISION NOT NULL DEFAULT 0,
    pos_y             DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (internal_ref, external_model_id)
);

-- 账号财务扩展（与网关账号一对一）
CREATE TABLE IF NOT EXISTS subscriptions (
    id             BIGINT PRIMARY KEY,                                 -- 雪花（应用层 idgen，无自增）
    provider_ref   TEXT NOT NULL,
    account_ref    TEXT NOT NULL,                                      -- 网关账号 id/name，仅作标识
    price_amount   NUMERIC(20,5) NOT NULL DEFAULT 0,                   -- 与 credit_wallets.balance 同型，可直接加减
    price_currency TEXT NOT NULL DEFAULT 'CNY' CHECK (price_currency IN ('CNY','USD')),
    billing_cycle  TEXT NOT NULL DEFAULT 'month' CHECK (billing_cycle IN ('month','year','once')),
    pay_day        SMALLINT NOT NULL DEFAULT 1 CHECK (pay_day BETWEEN 1 AND 28),  -- 29–31 会跨月错位
    share_cost     BOOLEAN NOT NULL DEFAULT true,                      -- 是否参与成本分摊
    cycle_start    DATE NULL,                                          -- 当前账期起点；NULL=未开始摊销
    note           TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_subscriptions_account UNIQUE (provider_ref, account_ref)
);