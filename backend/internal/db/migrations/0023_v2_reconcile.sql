-- 0023_v2_reconcile：60 秒对账窗口。
--
-- 口径来源：docs/designs/2026-11-15-v2-p1-data-contract.md §7。
--
-- 只比「总 token 合计」与「请求笔数」，**不比金额**：账本币种/牌价与灵枢积分不等价，
-- 比金额必然误报。笔数不一致写进 detail，不单独告警；告警只看 token 偏差。
--   灵枢侧合计 = billing_records.tokens 三段（input + cache_read + output）
--   账本侧合计 = ledger_entries.tokens_in + tokens_out
--   这样绕开「账本的 tokens_in 含不含缓存读」的口径未知问题（求和不受该差异影响）。
--
-- alarm_streak：连续告警窗数，>=3 升级。用列累加而不是窗口函数，
-- 因为跨批次的连续判定用 SQL 窗口函数要回溯历史行，成本高且难测。
--
-- 窗口长度由 CHECK 保证：window_end 必须等于 window_start + 60s。
-- 交错/错位窗口不许写同一行起点（ON CONFLICT (window_start) 只接受整一分钟），
-- 「断言进库里」比写在文档里可靠。
--
-- 幂等：IF NOT EXISTS；写入用 ON CONFLICT (window_start) DO UPDATE。
-- 可回滚：DROP 表（纯派生数据，回滚无损失）。

CREATE TABLE IF NOT EXISTS reconcile_windows (
    window_start    TIMESTAMPTZ PRIMARY KEY,              -- 供 ON CONFLICT (window_start)
    window_end      TIMESTAMPTZ NOT NULL,
    local_count     BIGINT NOT NULL DEFAULT 0,            -- 灵枢账单笔数
    upstream_count  BIGINT NOT NULL DEFAULT 0,            -- 账本笔数
    local_tokens    BIGINT NOT NULL DEFAULT 0,            -- 三段合计
    upstream_tokens BIGINT NOT NULL DEFAULT 0,            -- in+out 合计
    status          TEXT NOT NULL DEFAULT 'ok' CHECK (status IN ('ok','alarm','error','blocked')),
    alarm_streak    SMALLINT NOT NULL DEFAULT 0,
    detail          JSONB NOT NULL DEFAULT '{}'::jsonb,   -- 分组明细（key×model 偏差），不参与判定
    checked_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_reconcile_window_len CHECK (window_end = window_start + interval '60 seconds')
);
CREATE INDEX IF NOT EXISTS idx_reconcile_status ON reconcile_windows(status, window_start DESC);