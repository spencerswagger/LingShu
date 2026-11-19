-- 0027_v3_bypass_billing：V3 旁路计费所需的列。
--
-- 背景：请求直连 magpie，灵枢按账本行计费。
--   ledger_entries 需要保留「请求的模型名(req_model)=对外模型」与「实际服务模型(served_model)」、
--   以及 caller_key_name（=令牌）；internal_models 需要可写的成本价 cost_rates；
--   billing_records 需要 magpie 请求 id 做幂等（同一账本行只能记一次账）。
--
-- 全部 IF NOT EXISTS / 只加列与索引，不改既有数据；回滚 = 删列/删索引（不影响历史账单行）。

ALTER TABLE ledger_entries   ADD COLUMN IF NOT EXISTS req_model        TEXT   NOT NULL DEFAULT '';
ALTER TABLE ledger_entries   ADD COLUMN IF NOT EXISTS served_model     TEXT   NOT NULL DEFAULT '';
ALTER TABLE ledger_entries   ADD COLUMN IF NOT EXISTS caller_key_name  TEXT   NOT NULL DEFAULT '';
ALTER TABLE ledger_entries   ADD COLUMN IF NOT EXISTS cache_read       BIGINT NOT NULL DEFAULT 0;
ALTER TABLE ledger_entries   ADD COLUMN IF NOT EXISTS cache_write      BIGINT NOT NULL DEFAULT 0;

ALTER TABLE internal_models  ADD COLUMN IF NOT EXISTS cost_rates       JSONB  NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE billing_records  ADD COLUMN IF NOT EXISTS magpie_request_id TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS uq_billing_magpie_request
    ON billing_records(magpie_request_id) WHERE magpie_request_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ledger_entries_unbilled ON ledger_entries(occurred_at) ;
