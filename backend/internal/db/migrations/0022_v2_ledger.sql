-- 0022_v2_ledger：用量账本落库表 + 预扣登记表。
--
-- 口径来源：docs/designs/2026-11-15-v2-p1-data-contract.md §6。
--
-- ── ledger_entries：最小九字段，POC 之后才补扩展字段 ─────────────────────
--   1 id 2 ledger_id 3 provider 4 provider_account 5 model 6 tokens_in 7 tokens_out
--   8 cost_credits 9 occurred_at
--   本期刻意不加：request_id / status·failed / retries / cache_read_tokens / thinking_tokens /
--   unit_price / currency。上游字段名未真机证实，先加等于让一半列恒为 NULL，比没有更误导。
--
--   幂等：uq_ledger_entries_ledger_id 是**唯一索引**，写入走
--         INSERT ... ON CONFLICT (ledger_id) DO NOTHING
--         —— 冲突目标必须逐字等于该唯一索引的列，否则 PG 会报
--         "there is no unique or exclusion constraint matching the ON CONFLICT specification"。
--         靠它吸收「每轮重叠 2 个窗口（120s）」带来的重复条目。
--   分页：offset 只在**单个窗口内**翻页（0/500/1000…），**不持久化、不进库**；
--         不要拿 offset 或取整时间戳当跨窗游标（换窗会跳行/丢数）。
--   金额：cost_credits 与 billing_records.cost_credits 同型同精度（NUMERIC(20,5)，单位=积分），
--         报表可直接加减；不引第二套金额单位。
--   时间：occurred_at 取网关时钟（网关闭环时间），窗口归属只按它切；两端时钟偏差要求 < 30s（半窗），
--         超阈值时对账置 status='error'，不许把条目静默挪到别的窗口。
--
-- ── credit_holds：阶段一「只建不写」，不是漏了 ──────────────────────────
--   阶段一（迁移 + 同步器 + 透传 + 归因 + 账单落库）**不会有任何代码写这张表**。
--   它是阶段二预扣/结算的登记表：一请求一行，创建时 expires_at = now()+15min，
--   账本匹配到 → settled；15 分钟未匹配 → expired 并全额退回；请求失败 → refunded。
--   阶段二接上之前，表里恒为 0 行属预期，不要拿它当「预扣没生效」的 bug。
--
-- 幂等：IF NOT EXISTS；可回滚（DROP 两表；阶段一没有写入方，回滚无数据损失）。

-- 账本条目（最小九字段）
CREATE TABLE IF NOT EXISTS ledger_entries (
    id               BIGINT PRIMARY KEY,                  -- 雪花（应用层 idgen，无自增）
    ledger_id        TEXT NOT NULL,                       -- 幂等键：上游账本条目 id
    provider         TEXT NOT NULL DEFAULT '',
    provider_account TEXT NOT NULL DEFAULT '',
    model            TEXT NOT NULL DEFAULT '',            -- 上游实际服务模型
    tokens_in        BIGINT NOT NULL DEFAULT 0,           -- 账本原值，不做三段归一化
    tokens_out       BIGINT NOT NULL DEFAULT 0,
    cost_credits     NUMERIC(20,5) NOT NULL DEFAULT 0,    -- 上游实付成本，单位=积分
    occurred_at      TIMESTAMPTZ NOT NULL                 -- 网关记账时间（同机部署，同一时钟）
);
-- ON CONFLICT (ledger_id) 的冲突目标：与下面的唯一索引逐字一致
CREATE UNIQUE INDEX IF NOT EXISTS uq_ledger_entries_ledger_id ON ledger_entries(ledger_id);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_time    ON ledger_entries(occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_account ON ledger_entries(provider, provider_account, occurred_at DESC);

-- 预扣登记（阶段二启用；阶段一只建不写）
CREATE TABLE IF NOT EXISTS credit_holds (
    id              BIGINT PRIMARY KEY,
    billing_id      TEXT NOT NULL,                        -- 一请求一预扣
    user_id         BIGINT NOT NULL REFERENCES users(id),
    token_id        BIGINT NULL REFERENCES tokens(id),
    amount          NUMERIC(20,5) NOT NULL DEFAULT 0,     -- 预扣积分
    status          TEXT NOT NULL DEFAULT 'held' CHECK (status IN ('held','settled','refunded','expired')),
    ledger_entry_id TEXT NULL,                            -- 结算匹配到的账本条目
    expires_at      TIMESTAMPTZ NOT NULL,                 -- 创建时 now()+15min
    closed_at       TIMESTAMPTZ NULL,                     -- 终结时间
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_credit_holds_billing UNIQUE (billing_id)
);
-- 过期扫描器的唯一入口：只扫 held，且按到期时间排序
CREATE INDEX IF NOT EXISTS idx_credit_holds_expiry ON credit_holds(status, expires_at) WHERE status = 'held';