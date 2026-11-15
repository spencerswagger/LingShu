-- 0020_v2_billing_attribution：账单表补「归因列」，纯 ADD。
--
-- 口径来源：docs/designs/2026-11-15-v2-p1-data-contract.md §3 / §8。
-- 本文件只有 ADD COLUMN / ADD CONSTRAINT / CREATE INDEX，**零 DROP、零 DML**：
-- 不可逆动作全部集中在 0021，备份与证据流程只走一次（见 0021 文件头）。
--
-- 归因链：request_id（请求）→ upstream_provider（供应商=网关渠道）→ upstream_account（账号）
--        → upstream_protocol（四协议）→ internal_model_id（已有列：组/直通，不另立 group_ref）
--        → external_model_name（请求名）+ upstream_model（实际服务模型）→ ledger_entry_id（账本）
--
-- 两个刻意的不做：
--   1) 不动 pricing_mode：它在现有代码里是「用户售价」口径（users.pricing_mode 同义），
--      订阅摊销是成本侧口径，混进用户定价枚举会让账单页与盈亏页读出两种含义 → 另立 cost_source。
--   2) 不加 channel_* 兼容列：0021 会删 billing_records.channel_key_id，历史「渠道密钥名」
--      维度永久丢失（已批准方向，需写进 PR 影响面）。
--
-- 幂等难点：PostgreSQL 不支持 ADD CONSTRAINT IF NOT EXISTS，故两个枚举约束用 DO 块按
-- （约束名 + 所属表）判存后再加；重复执行不会报 duplicate_object。
-- 可回滚：DROP COLUMN / DROP CONSTRAINT 即可（历史行这些列为 NULL）。

ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS request_id        TEXT;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS upstream_provider TEXT;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS upstream_account  TEXT;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS upstream_model    TEXT;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS upstream_protocol TEXT;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS ledger_entry_id   TEXT;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS cost_source       TEXT;

DO $$
BEGIN
  IF NOT EXISTS (
      SELECT 1 FROM pg_constraint
      WHERE conname = 'ck_billing_upstream_protocol'
        AND conrelid = 'billing_records'::regclass
  ) THEN
    ALTER TABLE billing_records ADD CONSTRAINT ck_billing_upstream_protocol
      CHECK (upstream_protocol IS NULL OR upstream_protocol IN
             ('openai-chat','openai-responses','anthropic-messages','gemini'));
  END IF;

  IF NOT EXISTS (
      SELECT 1 FROM pg_constraint
      WHERE conname = 'ck_billing_cost_source'
        AND conrelid = 'billing_records'::regclass
  ) THEN
    ALTER TABLE billing_records ADD CONSTRAINT ck_billing_cost_source
      CHECK (cost_source IS NULL OR cost_source IN ('ledger','amortized','none'));
  END IF;
END $$;

-- 账本条目与账单一对一：部分唯一索引（历史行 ledger_entry_id 全 NULL，不进索引）。
CREATE UNIQUE INDEX IF NOT EXISTS uq_billing_ledger_entry
    ON billing_records(ledger_entry_id) WHERE ledger_entry_id IS NOT NULL;

-- 对账/盈亏按「供应商 + 账号」聚合同一时间窗；部分索引避免把历史 NULL 行扫进来。
CREATE INDEX IF NOT EXISTS idx_billing_upstream_time
    ON billing_records(upstream_provider, upstream_account, call_time DESC)
    WHERE upstream_provider IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_billing_request
    ON billing_records(request_id) WHERE request_id IS NOT NULL;