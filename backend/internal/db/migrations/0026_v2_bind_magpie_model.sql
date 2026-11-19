-- 0026_v2_bind_magpie_model：把「已绑定内部模型但没写转发模型」的对外模型补齐 MagpieModel。
-- 背景：V2 转发准入以 external_models.magpie_model 为真源；绑定只写 bound_internal_ref 时，
-- 调用会报「模型尚未接入转发服务」。这里对历史行做一次性回填（只补空值，不覆盖人工设置）。
UPDATE external_models
   SET magpie_model = bound_internal_ref,
       updated_at = now()
 WHERE (magpie_model IS NULL OR btrim(magpie_model) = '')
   AND bound_internal_ref IS NOT NULL
   AND btrim(bound_internal_ref) <> '';
