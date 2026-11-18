-- 0023c_v2_reconcile_counts_backfill：把「同步轮次写过、但从来不会写计数」的窗口计数归 NULL。
--
-- 为什么需要单独一条（Gate 复验指出 0023b 只覆盖了 blocked/error）：
--   D4 的计数列只由**对账服务**写。同步轮次（window.go 的 Rounder）会为每个窗口写 status
--   （ok / blocked / error）以及 detail，但**从不写四列计数**。于是老库里大量 `status='ok'`
--   的行是「0 占位」——它和「对账算过、这一窗真的没流量」的 0 长得一模一样，
--   漏账因此不可见（这正是 D4 要消灭的那种状态）。
--
-- 判定依据（为什么能安全区分、不误伤真 0）：
--   `detail->>'phase'` 是写入方标记：同步轮次写 'sync'，对账服务写 'reconcile'
--   （见 internal/domain/upstream/window_store.go 的 windowDetailPhase / windowDetailPhaseReconcile，
--   对账落库用 `detail = detail || '{phase:reconcile}'` 覆盖该键）。
--   所以：
--     · phase='sync' 且四列全 0  → **从没算过**（同步不写计数）→ 归 NULL；
--     · phase='reconcile'（哪怕数值是 0）→ 算过且确实没流量 → **不动**；
--     · 没有 phase 标记的行 → 无从判断，**不动**（宁可留着可疑值，也不抹掉可能真实的 0）。
--   再加「四列全 0」这个条件：任何一列非 0 都说明有值，绝不覆盖。
--
-- 幂等：条件里带 `COALESCE(col,0) = 0`，重复执行不会再命中已归 NULL 的行（NULL 的 COALESCE 是 0，
-- 但 phase 仍为 'sync' 的会再次被置 NULL —— 结果相同，无副作用）。
--
-- 排序：`0023c` 在 0023 / 0023b 之后、0024 之前（字典序 = 执行序，本批右界 < '0025'）。
-- 可回滚：计数是派生数据，回滚只需把行删掉重算（DROP 表同理）。

UPDATE reconcile_windows
   SET local_count     = NULL,
       upstream_count  = NULL,
       local_tokens    = NULL,
       upstream_tokens = NULL
 WHERE detail->>'phase' = 'sync'
   AND COALESCE(local_count, 0) = 0
   AND COALESCE(upstream_count, 0) = 0
   AND COALESCE(local_tokens, 0) = 0
   AND COALESCE(upstream_tokens, 0) = 0;