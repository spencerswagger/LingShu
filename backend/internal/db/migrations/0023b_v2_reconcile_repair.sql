-- 0023b_v2_reconcile_repair：把 0020 / 0023 的**最终形状**在已应用过老版本的环境上补齐。
--
-- 为什么需要这个文件（不是洁癖，是真实故障）：
--   迁移框架按 `applied_migrations.name` 记账，**文件内容改了不会重放**。只要某个环境
--   曾经应用过早期版本的 0020 / 0023（本分支 2026-11-15 前半段、或其它并行分支），
--   后续把文件改对也不会生效，实测表现为：
--     · 插 status='skipped' 被 23514 拒（老 0023 的 CHECK 值域只有 ok|alarm|error|blocked）；
--     · `cost_credits IS NULL + cost_source='ledger'` 被放行（老 0020 没有跨列 CHECK）；
--     · 计数列还是 NOT NULL DEFAULT 0（blocked 窗与「无流量」不可区分）。
--   所以把这些「最终形状」抽成一条**幂等**的修形迁移，排在 0023 之后、0024 之前：
--   新库上它几乎什么都不做（已经是目标形状），老库上它才是真正生效的那一句。
--
-- 编号说明：`0021` 是一段**作废的空号**（原破坏性迁移已改名 0024，见 F4 裁定），不复用；
-- 这里用 `0023b` 保证字典序在 0023 之后、0024 之前 → 执行顺序不变。
--
-- 幂等：DROP CONSTRAINT IF EXISTS / ADD COLUMN IF NOT EXISTS / ALTER ... DROP NOT NULL 均可重复执行。
-- 可回滚：本文件只把形状推到「最终态」，回滚方式是连同 0020/0023/0024 一起回退。

-- ── 0020：跨列 CHECK 的最终定义（F10 补丁：NOT VALID，不扫存量）──────────────
-- 先 DROP 再建是刻意的：老环境里可能已存在同名但定义不对的约束，
-- 而 PG 没有 `ADD CONSTRAINT IF NOT EXISTS`，DO 判存只会把错定义留在原地。
ALTER TABLE billing_records DROP CONSTRAINT IF EXISTS ck_billing_cost_presence;
ALTER TABLE billing_records ADD CONSTRAINT ck_billing_cost_presence
  CHECK (cost_credits IS NOT NULL OR cost_source IS NOT DISTINCT FROM 'none') NOT VALID;

-- ── 0023：状态值域补 'skipped' ────────────────────────────────────────────────
-- 老表的列内联 CHECK 是自动命名的 `reconcile_windows_status_check`，会继续拒 'skipped'，
-- 所以先删它，再按固定名重建（两个都留着 = 值域实际没变）。
ALTER TABLE reconcile_windows DROP CONSTRAINT IF EXISTS reconcile_windows_status_check;
ALTER TABLE reconcile_windows DROP CONSTRAINT IF EXISTS ck_reconcile_status;
ALTER TABLE reconcile_windows ADD CONSTRAINT ck_reconcile_status
  CHECK (status IN ('ok','alarm','error','blocked','skipped'));

-- ── 0023：断点列 + 计数列可空 ────────────────────────────────────────────────
ALTER TABLE reconcile_windows ADD COLUMN IF NOT EXISTS scroll_offset BIGINT NOT NULL DEFAULT 0;
ALTER TABLE reconcile_windows ALTER COLUMN local_count     DROP NOT NULL;
ALTER TABLE reconcile_windows ALTER COLUMN local_count     DROP DEFAULT;
ALTER TABLE reconcile_windows ALTER COLUMN upstream_count  DROP NOT NULL;
ALTER TABLE reconcile_windows ALTER COLUMN upstream_count  DROP DEFAULT;
ALTER TABLE reconcile_windows ALTER COLUMN local_tokens    DROP NOT NULL;
ALTER TABLE reconcile_windows ALTER COLUMN local_tokens    DROP DEFAULT;
ALTER TABLE reconcile_windows ALTER COLUMN upstream_tokens DROP NOT NULL;
ALTER TABLE reconcile_windows ALTER COLUMN upstream_tokens DROP DEFAULT;

-- 老库里 blocked/error 行的计数是早期「写 0 占位」留下的，一律归 NULL（= 未算过）。
-- 只动「四列全是 0」的行：真的算过且确实没流量的 ok/skipped 行不会被改。
UPDATE reconcile_windows
   SET local_count = NULL, upstream_count = NULL, local_tokens = NULL, upstream_tokens = NULL
 WHERE status IN ('blocked','error')
   AND COALESCE(local_count,0) = 0 AND COALESCE(upstream_count,0) = 0
   AND COALESCE(local_tokens,0) = 0 AND COALESCE(upstream_tokens,0) = 0;