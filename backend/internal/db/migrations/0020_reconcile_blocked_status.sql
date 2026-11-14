-- 0020_reconcile_blocked_status：对账窗口新增 `blocked` 状态（契约 §8.2）。
--
-- 为什么需要：账本字段集不满足最小 9 项（或缺时区时间戳、或分页只有 offset 形式）时，
-- 该窗口既不能记通过、也不能记不通过——它只是「端到端未验（pending POC）」。
-- 用 skipped 表达会与「我们自己没接上账本」混淆（两者的排查动作完全不同），故单列一个状态。
--
-- 口径：blocked 窗不计入 alarm_streak，不触发升级审计，也不回写任何账单；
-- 本地比对结果只作为参考留在 detail.local_comparison 里。
--
-- 幂等：DROP CONSTRAINT IF EXISTS + ADD CONSTRAINT（重放安全，不依赖约束名的历史形态）。
-- 回滚：把 CHECK 还原成 ('ok','alarm','error','skipped') 前需先确认没有 status='blocked' 的行
--       （有则先按 §8.2 口径把它们改成 'skipped' 并保留 detail.blocked_reasons）。

ALTER TABLE reconcile_windows
    DROP CONSTRAINT IF EXISTS reconcile_windows_status_check;

ALTER TABLE reconcile_windows
    ADD CONSTRAINT reconcile_windows_status_check
    CHECK (status IN ('ok', 'alarm', 'error', 'skipped', 'blocked'));