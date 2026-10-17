-- 0013_probe_logs_three_segments：把健康探测遥测（probe_logs）统一到三段口径。
--
-- 背景：probe_logs.input_tokens 存的是上游 prompt_tokens（OpenAI 兼容「含」口径，包含 cached_tokens），
-- 与 billing.Usage 的三段口径（input 为纯输入、不含缓存读）不一致，导致渠道探测历史与账单口径不可比。
--
-- 归一化：input_tokens 减去 cached_tokens（input_tokens 是 prompt_tokens 的超集，故减法合法）。
-- total_tokens 不变：旧值 = prompt_tokens + completion_tokens，新口径三段合计
-- = (prompt − cached) + cached + completion，数值相同。
--
-- 注意：本语句依赖迁移运行器的「一次性执行」保证（applied_migrations 记录），不要手工重复执行。
UPDATE probe_logs
SET input_tokens = input_tokens - cached_tokens
WHERE cached_tokens > 0 AND cached_tokens <= input_tokens;
