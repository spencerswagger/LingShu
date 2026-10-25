-- 上下文分档（context_tiers）演示/测试环境样例配置
--
-- ⚠️ 用途与边界
--   * 这是给**测试库 / 演示环境**用的样例，程序启动不会自动执行它。
--   * 生产库要不要配、配几档、系数多少，由 spencer 拍板，运维（hugo）执行；本文件不参与生产初始化，
--     也不修改 internal/seeding/seed.go 的默认值（默认仍是 `[]` = 不分档），避免新部署无意间改价。
--   * 分档系数直接乘在售价/成本积分上，配错等于改价：执行前先确认金额影响。
--
-- 为什么需要它
--   `billing.context_tiers` 当前是 `[]`、模型级 context_tiers 全为 NULL，分档系数恒 1，
--   定价曲线看不到跳档拐点，也无法验证「超档」。配了 2 档以上才能验出拐点与超档区段。
--
-- 口径（与 billing.ContextTierCoeff 一致）
--   * 查档输入 = 本次请求的「输入 + 缓存读」（= 上下文长度，三段口径下 Input 只是纯输入）。
--   * Min 含端点、Max 含端点，档与档之间用「上档 Max + 1 = 下档 Min」衔接，不留缝、不重叠。
--   * Max 为 null 表示无上限。所有档必须按 Min 升序（billing.ValidateTiers 会校验）。
--   * 落在所有档之外的 token 数（例如档之间留了缝）会让计费失败（"未命中任何上下文分档"），
--     定价曲线会把这些区段标成 OverTier。
--
-- 样例：4 档 3 个拐点（8001 / 32001 / 128001）
--   0      ~ 8000     ×1.0    （短上下文，基准价）
--   8001   ~ 32000    ×1.2
--   32001  ~ 128000   ×1.5
--   128001 ~ 无上限    ×2.0    （长上下文单价翻倍）

UPDATE sys_configs
   SET value = '[
        {"Min": 0,      "Max": 8000,   "Coeff": 1.0},
        {"Min": 8001,   "Max": 32000,  "Coeff": 1.2},
        {"Min": 32001,  "Max": 128000, "Coeff": 1.5},
        {"Min": 128001, "Max": null,   "Coeff": 2.0}
      ]'::jsonb,
       updated_at = now()
 WHERE key = 'billing.context_tiers';

-- 回滚（恢复成「不分档」）：
--   UPDATE sys_configs SET value = '[]'::jsonb, updated_at = now() WHERE key = 'billing.context_tiers';
--
-- 只想给单个对外模型配档（模型级优先于全局，成本线要用渠道内部模型各自的 context_tiers）：
--   UPDATE external_models
--      SET context_tiers = '[{"Min":0,"Max":32000,"Coeff":1.0},{"Min":32001,"Max":null,"Coeff":1.5}]'::jsonb
--    WHERE external_name = '<对外模型名>' AND deleted_at IS NULL;
--   UPDATE channel_models
--      SET context_tiers = '[{"Min":0,"Max":32000,"Coeff":1.0},{"Min":32001,"Max":null,"Coeff":1.5}]'::jsonb
--    WHERE id = <channel_models.id>;
--
-- 生效方式：billing.ConfigProvider 对 context_tiers 有内存缓存，配置更新后需重启服务（或等缓存失效钩子触发）。
