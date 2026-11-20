# v2 计价目录 M1（models.dev 对齐·四项定案落地）

日期：2026-11-20 · 分支 `feat/v2-billing-catalog-m1`（栈在 `fix/v3-settle-gap` 之上） · PR #173

## 口径（四项定案，2026-11-20 spencer 拍板）

1. **cache_write**：写缓存段独立计价；token 分桶互斥（同一 token 只落
   input / cache_write / cache_read 一个桶）；**空价回落 = 0**（宁少不多收）。
2. **service_tier**：差价只在**成本层**（规则键留位；v3 账本无该字段 → 恒空不命中）。
3. **web_search**：只存元数据不计价（`ext_meta` 留位；价格树照存、不进公式）。
4. **multipliers**：只挂模型层——成本 `internal_models.cost_v2`、
   售价 `external_models.sale_v2`，两层同刻独立求值、规则可以不同。

## M1 交付边界（纯增、无行为变化）

- **迁移 0031**（纯增、幂等、可回滚）：`internal_models.cost_v2 / ext_meta`、
  `external_models.sale_v2`、`billing_records.price_eval` 四列 JSONB。
  旧行 NULL = 回落旧三段。**注意**：`internal_models` 主键是 `ref`（无 id 列），
  `external_models.id` NOT NULL——写探针时别搞反。
- **读价收敛**（v3 结算 + 补收同源 `attachSale/attachCost`）：v2 列存在且
  `ParseV2Cost` 成功 → 四段价 + multipliers，**不叠加**时段/分档（时间与上下文
  条件由规则键 `utc_hours/utc_days/date/context` 吸收）；解析失败 → 回落旧三段
  **并打告警**（静默回落 = 价格漂移不可见，两种失败方向都必须可观测）。
- **settleOneLegacy（kill-switch 关）零改动**：不读新列、语义与 main
  v0.15.0-rc4 逐条一致。
- 写入口（PUT cost-rates、模型编辑）**未切**——照 M1 正本「写入口先不切」。

## 规则引擎（`internal/domain/billing/mult`）要点

- 值域约定：`utc_hours` 的值是 **"HH:MM" 字符串**（内部按分钟比较）；
  `date` 是 "YYYY-MM-DD"；`holiday` 枚举国家码（CN/US/CA）；`region/service_tier`
  只接受字符串操作符（拒绝大小比较）；`context` 拒绝 `in`。
- **一键一操作符**：`{"gte":..,"lt":..}` 同键双操作符在解析期被拒（丢弃+Warning）；
  区间用 `all` 两个半条件表达；**跨午夜窗口必须 `any` 两个半窗**（all 恒假）。
- 未知键整条跳过 + Warning；空 when / 多键 / factor 非正数一律解析期拒绝。
- 因子形状：数字 = 全段；对象 = 点名模态（未知模态键留痕不影响 text 段计价）。
- 命中连乘：`FactorFor(modality) = Global × perMod[modality]`。

## 实现口径决定（写进代码注释的都在这）

- **四桶拆分**（`splitV2Usage`）：`tokens_in - cache_read - cache_write` 为 input；
  数据异常（负数）回退三段口径——cache_write 不拆、input 沿用旧防御链（再负则当纯输入）。
- **幂等不覆盖留痕**：`V2Pricing.Evaluated()` 由 requestCreditsV2 / Record 成本分支
  置位；23505 幂等命中（Record 直接返回既有行）时为 false → price_eval 参数走
  COALESCE 不动首次结算写入的值。
- **priceEvalRaw 用 `any`**：`[]byte(nil)` 经 database/sql 会当空 `[]uint8` 传下去
  （不是 driver nil）——参数形态必须在门口钉死，sqlmock 与 PG 都按 nil 才对。
- **rates 列 = LegacyView**（三段视图）：console 渲染兼容；完整口径看 price_eval。
- **coefficients 列存 1/1**（v2 行）：真因子只在 price_eval 留痕里。
- **raw_total**（v2 行）= 未除 R、先乘因子的四段和。

## 未修项（M2 待办）

1. **facadebill 预扣链仍三段**：`magpie.Usage` 无 CacheWrite（Anthropic 解析把
   cache_creation 折进 Input）——拆段需扩 usage 采集口径，本批不动热路径。
2. **写入切换 + 回填**：旧三段 → v2 形状一次性脚本（cache_write/reasoning 无口径
   不填）；`time_config` → `utc_hours/utc_days`（时区平移）；`context_tiers` →
   `context` 条件。管理端编辑/保存切到 v2 形状。
3. **catalog_models / catalog_match** 不在 M1 正本口径内，未建；如 M2/M3 需要
   对账目录再立迁移（用 0032 起）。
4. **service_tier 账本归因位**：等上游账本提供该字段后把 `CostV2.ServiceTier`
   接上（当前恒空）。

## 验证

- billing / mult / v3billing / db 全绿；v2 专项 9 用例（四桶+留痕逐字节、
  形状不合法回落+告警、异常回退、幂等不覆盖、补收切读）。
- 真库 PG17：`TestMigrate_0031_V2BillingCatalog`（四列可写可读+幂等）+ FullChain PASS。
- 带 DSN 与无 DSN `-race` 全仓 37 包 0 FAIL；`tests` 绿；gofmt/vet/build 干净。
- 变异四组（删四桶拆分 / 删 attach v2 / 删留痕 / 删 output 判存）各自红、还原绿。
- CI run 44729856625 双 job success（Frontend typecheck + Backend with Postgres）。
