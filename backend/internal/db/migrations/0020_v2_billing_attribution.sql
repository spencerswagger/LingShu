-- 0020_v2_billing_attribution：账单表补「归因列」，纯 ADD。
--
-- 口径来源：docs/designs/2026-11-15-v2-p1-data-contract.md §3 / §8。
-- 本文件只有 ADD COLUMN / ADD CONSTRAINT / CREATE INDEX + 一条**列级归一 UPDATE**
-- （新加跨列 CHECK ck_billing_cost_presence 前置条件，见文件末尾）：
-- 不可逆动作全部集中在 0024，备份与证据流程只走一次（见 0024 文件头）。
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

-- ── F10：跨列 CHECK（成本缺失必须标明「没有成本口径」）────────────────────
--
-- 口径：`cost_credits IS NULL` ⇒ `cost_source` 必须**恰好**是 'none'。写成「列组合不许自相矛盾」
-- 而不是「cost_source 必填」：存量行硬填 NOT NULL 会逼所有人编一个假的成本来源；
-- 要的是**可判别**——空成本的行不许再声称成本来自账本/摊销。
--
-- 为什么用 `IS NOT DISTINCT FROM` 而不是 `=`：SQL 的 CHECK 把**三值逻辑的 NULL 当成通过**，
-- 写成 `cost_source = 'none'` 时「成本 NULL + 来源 NULL」求值为 NULL → 约束形同不存在
-- （实测就是这样被放行的）。`IS NOT DISTINCT FROM` 返回布尔值，NULL 也判 false → 真正拦住。
--
-- 为什么必须在 DB 层而不是只在 Go 里（attributionArgs 已经归一）：写入口不止一个
-- （账单快速失败通道、重试队列、运维补单），任何一条漏归一都会让「0 成本 + ledger 来源」
-- 这种自相矛盾的行进库，报表按 cost_source 聚合直接失真。约束是最后一道闸。
--
-- 顺序与形态（F10 补丁，2026-11-15）：**不做存量扫描的 UPDATE**，直接把约束建成
-- `NOT VALID`。
--   · 生产库里 15 万量级的存量行是 cost_credits NULL + cost_source NULL（0020 之前写的），
--     加约束时若做全表校验要扫一遍大表，把一个纯 ADD 的迁移变成停机风险点；
--   · `NOT VALID` 的语义刚好够用：**已存在的行不校验**（跳过扫描），
--     **新写入与任何 UPDATE 一律强约束**。想补齐历史数据可以后面单独跑运维脚本（V2 之后再说）。
--   · 因此这里不需要「先归一再加约束」的 UPDATE：加了它反而是那一次 15 万行扫描本身。
--
-- 重放/修形：约束名固定，先 DROP 再建，保证「老库上已存在一条定义不对的同名约束」也能被纠正
-- （`ADD CONSTRAINT IF NOT EXISTS` 在 PG 里不存在；DO 判存会把错定义留在原地 → 见 0023b 的说明）。
ALTER TABLE billing_records DROP CONSTRAINT IF EXISTS ck_billing_cost_presence;
ALTER TABLE billing_records ADD CONSTRAINT ck_billing_cost_presence
  CHECK (cost_credits IS NOT NULL OR cost_source IS NOT DISTINCT FROM 'none') NOT VALID;

-- ── 回填 ledger_entry_id 时必须一起给成本来源（否则 23514）──────────────────
--
-- `NOT VALID` 只放过**已存在**的行；任何 UPDATE 都会重新校验整行。所以给历史行补
-- ledger_entry_id 时，必须同一句里把 cost_source 定下来，默认按「没有成本口径」写：
--
--     UPDATE billing_records
--        SET ledger_entry_id = $1,
--            cost_source     = COALESCE(cost_source, 'none')
--      WHERE billing_id = $2;
--
-- 只有确认该行确实记了成本（cost_credits IS NOT NULL）时，才允许写 'ledger'：
--
--     UPDATE billing_records
--        SET ledger_entry_id = $1,
--            cost_source     = COALESCE(cost_source, 'ledger')
--      WHERE billing_id = $2 AND cost_credits IS NOT NULL;
--
-- 反例（会炸 23514）：只 SET ledger_entry_id，而该行 cost_credits 为 NULL、cost_source 还是 NULL
--   → `NULL IS NOT NULL` 为假、`NULL = 'none'` 为 NULL → 整行被判违反。
