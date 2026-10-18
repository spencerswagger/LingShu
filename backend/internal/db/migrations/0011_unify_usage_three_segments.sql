-- 0011_unify_usage_three_segments：把 usage 与定价从「五段」统一为「三段」（输入/缓存读/输出）。
--
-- 三段口径（与 billing.Usage 注释一致）：
--   input     = 纯输入，不含缓存读；上游的「缓存写」token 属于输入 token，计入本段；
--   cache_read= 命中提示词缓存读取的输入；
--   output    = 生成 token，含推理（原 reasoning 段并入）。
--
-- 归一化规则（历史数据只改 tokens/费率构成，credits_consumed 等金额一律不重算）：
--   1) 历史 billing_records.tokens 转换为新口径：
--      input_new  = (CASE WHEN cache_read_old > input_old THEN input_old  -- 该行本就是「输入不含缓存读」口径，保持不变
--                         ELSE input_old - cache_read_old END) + cache_write_old
--      cache_read = cache_read_old
--      output_new = output_old + reasoning_old
--      说明：旧口径下缓存读被当作输入的子集（prompt_tokens 含 cached_tokens），因此
--      「cache_read > input」只可能来自「输入不含缓存读」的上游（如 Anthropic 形状），这类行不再二次相减；
--      对「输入含缓存读」的行，换算后三段总和 = 旧的 input+cache_write+output+reasoning 之和
--      （即旧展示口径下的总量不变），只是构成被拆成新口径。历史歧义（无法判定某个 input 到底含不含缓存读）
--      无法完全还原，此处按上述规则取最合理的一种。
--      取舍：本步骤只处理「仍含 cache_write/reasoning 键」的行（保证重复执行不二次换算）。
--      据此，本就缺这两个键、但 input 仍含缓存读的历史行不会被换算——旧 Go 写入器恒写满五键，
--      仅外部导入 / 手工构造的数据可能落入该情形，此处不为其猜测口径。
--      取值守卫：tokens 中的数值以正则 '^-?[0-9]+$' 判定，非整数字符串（如 "1.0"/"abc"）按 0 处理，绝不抛错中断迁移。
--   2) 费率 JSONB（售价/成本）删除 cache_write / reasoning 两个键（原推理单价按定调移除，不折算进 output）。
--   3) 路由成本样值（routing.cost_sample）删除废弃键，保留既有数值。
--   4) 三处 JSONB 列的 DEFAULT 同步改为三段（新插入行不再写入已废弃键）。
--
-- 幂等：所有 UPDATE 均带「是否仍含废弃键」的前置条件，重复执行不会二次换算。

-- 1) 历史账单 tokens 归一化（非数字键值按 0，绝不抛错）
WITH hist AS (
    SELECT id,
           CASE WHEN tokens->>'input'       ~ '^-?[0-9]+$' THEN (tokens->>'input')::bigint       ELSE 0 END AS input_old,
           CASE WHEN tokens->>'cache_read'  ~ '^-?[0-9]+$' THEN (tokens->>'cache_read')::bigint  ELSE 0 END AS cache_read_old,
           CASE WHEN tokens->>'cache_write' ~ '^-?[0-9]+$' THEN (tokens->>'cache_write')::bigint ELSE 0 END AS cache_write_old,
           CASE WHEN tokens->>'output'      ~ '^-?[0-9]+$' THEN (tokens->>'output')::bigint      ELSE 0 END AS output_old,
           CASE WHEN tokens->>'reasoning'   ~ '^-?[0-9]+$' THEN (tokens->>'reasoning')::bigint   ELSE 0 END AS reasoning_old
    FROM billing_records
    WHERE tokens ? 'cache_write' OR tokens ? 'reasoning'
)
UPDATE billing_records b
SET tokens = jsonb_build_object(
        'input', CASE WHEN h.cache_read_old > h.input_old
                      THEN h.input_old
                      ELSE h.input_old - h.cache_read_old END + h.cache_write_old,
        'cache_read', h.cache_read_old,
        'output', h.output_old + h.reasoning_old
    )
FROM hist h
WHERE b.id = h.id;

-- 2) 对外模型售价：删除废弃键 + 更新默认值
UPDATE external_models
SET sale_rates = (sale_rates - 'cache_write') - 'reasoning'
WHERE sale_rates ? 'cache_write' OR sale_rates ? 'reasoning';

ALTER TABLE external_models
    ALTER COLUMN sale_rates SET DEFAULT '{"input":1.0,"output":2.0,"cache_read":0.1}';

-- 3) 渠道内部模型成本价：删除废弃键 + 更新默认值
UPDATE channel_models
SET cost_rates = (cost_rates - 'cache_write') - 'reasoning'
WHERE cost_rates ? 'cache_write' OR cost_rates ? 'reasoning';

ALTER TABLE channel_models
    ALTER COLUMN cost_rates SET DEFAULT '{"input":0.8,"output":1.6,"cache_read":0.05}';

-- 4) 路由成本比较样值：删除废弃键，保留既有数值
UPDATE sys_configs
SET value = (value - 'cache_write') - 'reasoning'
WHERE key = 'routing.cost_sample'
  AND (value ? 'cache_write' OR value ? 'reasoning');
