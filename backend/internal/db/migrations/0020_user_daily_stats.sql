-- 0020 统计/账单接口性能：日级预聚合 + token 明细列 + 会话列表索引
--
-- 背景（实测）：billing_records 明细表上的看板查询随行数线性变慢——
--   /api/v1/auth/me/summary @100 万行 ≈178ms、@3000 万行 ≈835ms；
--   /api/v1/admin/cost/stats @100 万行 ≈3.5s（全表扫 + 每行 3 次 JSONB 正则）。
-- 实测对照结论：按月分区更慢（过滤列 call_time ≠ 分区键 created_at，零裁剪）、BRIN 无效、
-- 唯「日级预聚合」达标且随行数不增长。故本迁移只做结构，不做分区，不引入任何新组件。
--
-- 三段：
--   (a) billing_records 增加三段 token 实体列（写入时落库，读时不再逐行解析 JSONB + 正则）；
--       既有行由启动回填补齐（见 internal/db/backfill_stats.go），本迁移不做大表 UPDATE。
--   (b) user_daily_stats：按 (user_id, day) 的日级聚合，day 以 Asia/Shanghai 切日，
--       口径与 billing_records 现有看板一致（status='completed'）。
--   (c) sessions(user_id, closed, last_active DESC)：开发者/管理端会话列表的过滤+排序索引。
--
-- 回滚（手工执行，迁移框架不做 down）：
--   DROP TABLE IF EXISTS user_daily_stats;
--   DROP TABLE IF EXISTS stats_backfill_state;
--   ALTER TABLE billing_records DROP COLUMN IF EXISTS input_tokens,
--                              DROP COLUMN IF EXISTS cache_read_tokens,
--                              DROP COLUMN IF EXISTS output_tokens;
--   DROP INDEX IF EXISTS idx_sessions_user_closed_active;

-- (a) 三段 token 实体列。默认 0 = 「未结算/无 token」，与看板既有口径（缺失按 0）一致。
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS input_tokens      BIGINT NOT NULL DEFAULT 0;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS cache_read_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS output_tokens     BIGINT NOT NULL DEFAULT 0;

-- (b) 日级预聚合。主键 (user_id, day) 即看板查询路径：
--     「某人某天」单点命中；「某人某时间窗」按 (user_id, day) 范围扫描，行数 = 天数（几十行）。
CREATE TABLE IF NOT EXISTS user_daily_stats (
    user_id           BIGINT        NOT NULL,
    day               DATE          NOT NULL,              -- Asia/Shanghai 自然日
    requests          BIGINT        NOT NULL DEFAULT 0,
    input_tokens      BIGINT        NOT NULL DEFAULT 0,
    cache_read_tokens BIGINT        NOT NULL DEFAULT 0,
    output_tokens     BIGINT        NOT NULL DEFAULT 0,
    credits           NUMERIC(20,5) NOT NULL DEFAULT 0,    -- 售价积分（= billing_records.credits_consumed）
    cost_credits      NUMERIC(20,5) NOT NULL DEFAULT 0,    -- 成本积分（= billing_records.cost_credits）
    updated_at        TIMESTAMPTZ   NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, day)
);
-- 全站按天汇总（管理端跨用户看板）用；按用户维度查询走主键，不需要额外索引。
CREATE INDEX IF NOT EXISTS idx_user_daily_stats_day ON user_daily_stats(day);

-- 回填断点：两个阶段各自一行（billing_tokens 用 id 游标，user_daily_stats 用 day 序号）。
CREATE TABLE IF NOT EXISTS stats_backfill_state (
    name       TEXT        PRIMARY KEY,
    last_id    BIGINT      NOT NULL DEFAULT 0,
    done_at    TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- (c) 会话列表：WHERE user_id=$1 [AND NOT closed] ORDER BY last_active DESC
--     既有 idx_sessions_user(user_id, expire_at) 无法支撑 closed 过滤 + last_active 排序，
--     行数涨上来后每页都要排序整段用户会话。
CREATE INDEX IF NOT EXISTS idx_sessions_user_closed_active
    ON sessions(user_id, closed, last_active DESC);
