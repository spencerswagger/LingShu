-- 0031_v2_billing_catalog_m1：计费目录对齐 models.dev v2 · 第一步 M1「纯增」（无行为变化）。
--
-- 背景（2026-11-20 spencer 四项定案，口径正本 = modelsdev-v2-billing-plan 设计报告 §问c-M1）：
--   1. cache_write 启用：magpie 四价（input/output/cache_read/cache_write）逐档对齐，
--      token 分桶互斥（同一 token 只落输入/缓存写/缓存读一个桶）、写缓存段独立计价；
--      空价回落 = cache_write 段价缺省按 0 计（运营逐模型填价后才按写价计，宁少不多收）。
--   2. service_tier 差价只在成本层（internal_models 的 multipliers 可配该条件因子），售卖层不做。
--   3. web_search 不做独立计价段（预留 MCP/工具调用计费），目录元数据照存。
--   4. multipliers 规则引擎按模型挂两层：成本挂 internal_models、售价挂 external_models，不开放用户组层。
--
-- 本迁移只加列（全部可空、无默认值、无约束），读价收敛在代码侧完成：
--   - internal_models.cost_v2  ：v2 成本价形状（JSONB，人民币/百万 token，与 cost_rates 同单位；
--     目录 USD 价须经 ×billing.cny_rate 折算后才能进本列，运行时无币种分支）。
--   - internal_models.ext_meta ：v2 目录元数据（capabilities/limit/modalities 原样存，
--     镜像真源永远是 magpie /v1/models，本列只做缺位补齐；M1 只建列不写入）。
--   - external_models.sale_v2  ：v2 售价形状（同单位口径）。
--   - billing_records.price_eval：v2 计价行的求值留痕（求值时刻/四价/命中因子/因子连乘/R，可空；
--     旧行为 NULL，对账可复算）。
--
-- 读价口径（代码侧 Resolver）：cost_v2/sale_v2 存在且可解析 → 走 v2 引擎（四价 + multipliers 连乘）；
-- 否则回落旧三段 + time_config/context_tiers（现行为逐字节不变）。
-- 写入口（PUT cost-rates、模型编辑）本批不切 —— 上述列在 M2 前不会有生产写入方，
-- 因此本迁移落地后生产行为零变化。
--
-- 幂等：ADD COLUMN IF NOT EXISTS。可回滚：列全部可空，DROP COLUMN 无数据损失
-- （v2 价未投产前回滚 = 删列 + 代码回退）。

ALTER TABLE internal_models  ADD COLUMN IF NOT EXISTS cost_v2 jsonb;
ALTER TABLE internal_models  ADD COLUMN IF NOT EXISTS ext_meta jsonb;
ALTER TABLE external_models ADD COLUMN IF NOT EXISTS sale_v2 jsonb;
ALTER TABLE billing_records  ADD COLUMN IF NOT EXISTS price_eval jsonb;

COMMENT ON COLUMN internal_models.cost_v2 IS
  'v2 成本价形状（models.dev v2 cost 树，人民币/百万 token；含 multipliers 规则）。存在且可解析时结算走 v2 引擎，否则回落 cost_rates 三段';
COMMENT ON COLUMN internal_models.ext_meta IS
  'v2 目录元数据补齐（capabilities/limit/modalities 原样 JSONB；镜像真源是 magpie /v1/models，本列只补缺，不覆盖镜像字段）';
COMMENT ON COLUMN external_models.sale_v2 IS
  'v2 售价形状（models.dev v2 cost 树，人民币/百万 token；含 multipliers 规则）。存在且可解析时计价走 v2 引擎，否则回落 sale_rates 三段';
COMMENT ON COLUMN billing_records.price_eval IS
  'v2 计价行的求值留痕：求值时刻(UTC)/四价/命中因子明细/因子连乘/R。NULL = 非计价行或旧三段口径';
