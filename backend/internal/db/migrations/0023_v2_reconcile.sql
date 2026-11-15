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
-- ── 四个计数列的写入方（D4 裁定，唯一）──────────────────────────────────
--   local_count / upstream_count / local_tokens / upstream_tokens **由对账服务在每窗结束时
--   统一写入**（internal/domain/upstream/reconcile.go 的 Store.SaveCounts），不许恒 0，
--   也不许「先写 0 等以后再补」。detail JSON 只兜底（分组明细、原因、页码），
--   任何**判定用**的数字都必须落在计数列上——查历史时不能再靠解析 detail。
--   同步轮次（window.go 的 Rounder）因此只写 status / detail / checked_at / scroll_offset。
--
-- ── scroll_offset（F9 裁定）──────────────────────────────────────────────
--   翻页显式 `ORDER BY occurred_at ASC, ledger_id ASC` + offset，且 offset **持久化在本列**：
--   blocked 的窗口下次从该 window_start 用本列的 offset 继续拉，直到清零。
--   幂等由 ledger_entries.ledger_id 的 ON CONFLICT 兜住，所以「重拉一段」不会重复计账；
--   持久化 offset 只是不再白拉已确认的那段（4000 条/轮上限下，积压窗口需要多轮才清完）。
--   清零（窗口置 ok）后本列归 0。
--
-- ── status 取值域（D4 裁定）──────────────────────────────────────────────
--   ok      两边对上
--   alarm   token 偏差超阈（alarm_streak 累加，>=3 升级）
--   error   本窗无法判定（拉取失败/时钟偏差超半窗等），重试安全
--   blocked 单轮未拉完（页数/条数上限）或账本侧数据不可得 → 要补拉，不是终态
--   skipped 本窗**不参与比对**（账本侧关闭/降级、窗口为空且无账本数据）——
--           与 blocked 区分：skipped 是「按口径不比」，blocked 是「该比但没拉完」。
--           两者混用会让「漏账」看起来像「正常」，所以值域必须分开。
--
-- alarm_streak：连续告警窗数，>=3 升级。用列累加而不是窗口函数，
-- 因为跨批次的连续判定用 SQL 窗口函数要回溯历史行，成本高且难测。
--
-- 窗口长度由 CHECK 保证：window_end 必须等于 window_start + 60s。
-- 交错/错位窗口不许写同一行起点（ON CONFLICT (window_start) 只接受整一分钟），
-- 「断言进库里」比写在文档里可靠。
--
-- ── 幂等难点：列 CHECK 的约束名 ────────────────────────────────────────
--   status 的取值域原来写成列内联 CHECK（PG 自动命名 `reconcile_windows_status_check`）。
--   现在要追加 'skipped'，而 PG 不支持「改 CHECK」：先 DROP 掉自动命名的那条，再按固定名
--   `ck_reconcile_status` 建新的一条。两条都留着的话，旧那条仍会拒 'skipped' —— 改值域却
--   忘了删旧约束，是本文件最容易踩的坑。
--   scroll_offset 用 ADD COLUMN IF NOT EXISTS 补：本文件是 CREATE TABLE IF NOT EXISTS，
--   对「同名旧表已存在」的环境整段跳过，新列不会自动出现。
--
-- ── 已知边界：本表的 CREATE 是 IF NOT EXISTS（会被同名旧表整段跳过）──────
--    若同名的旧表（旧分支遗留）已存在，CREATE 语句**整段跳过且不报错**：新列 / PK / 60s CHECK 全缺，
--    直到首次写窗口才炸（实测：column "local_count" does not exist；只写两键时
--    ON CONFLICT (window_start) 报 no unique or exclusion constraint matching ...，旧表 PK 是 id）。
--    防线因此放在**执行前的探测**（定稿文档 §5.1 ③ 看形状、④ 按记录反查对象），不在本文件。
--
-- 幂等：IF NOT EXISTS / ADD COLUMN IF NOT EXISTS / DROP CONSTRAINT IF EXISTS + DO 块判存；
-- 写入用 ON CONFLICT (window_start) DO UPDATE。
-- 可回滚：DROP 表（纯派生数据，回滚无损失）。

CREATE TABLE IF NOT EXISTS reconcile_windows (
    window_start    TIMESTAMPTZ PRIMARY KEY,              -- 供 ON CONFLICT (window_start)
    window_end      TIMESTAMPTZ NOT NULL,
    local_count     BIGINT NOT NULL DEFAULT 0,            -- 灵枢账单笔数（对账服务写）
    upstream_count  BIGINT NOT NULL DEFAULT 0,            -- 账本笔数（对账服务写）
    local_tokens    BIGINT NOT NULL DEFAULT 0,            -- 三段合计（对账服务写）
    upstream_tokens BIGINT NOT NULL DEFAULT 0,            -- in+out 合计（对账服务写）
    status          TEXT NOT NULL DEFAULT 'ok',
    alarm_streak    SMALLINT NOT NULL DEFAULT 0,
    scroll_offset   BIGINT NOT NULL DEFAULT 0,            -- 下次补拉该窗的 offset（ok 后归 0）
    detail          JSONB NOT NULL DEFAULT '{}'::jsonb,   -- 分组明细/原因（兜底，不参与判定）
    checked_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_reconcile_window_len CHECK (window_end = window_start + interval '60 seconds')
);

-- 旧表补齐新列（CREATE 被 IF NOT EXISTS 跳过时这一句才是真正生效的那句）。
ALTER TABLE reconcile_windows ADD COLUMN IF NOT EXISTS scroll_offset BIGINT NOT NULL DEFAULT 0;

-- 值域：删掉列内联 CHECK（自动命名）后按固定名重建，追加 'skipped'。
ALTER TABLE reconcile_windows DROP CONSTRAINT IF EXISTS reconcile_windows_status_check;
DO $$
BEGIN
  IF NOT EXISTS (
      SELECT 1 FROM pg_constraint
      WHERE conname = 'ck_reconcile_status'
        AND conrelid = 'reconcile_windows'::regclass
  ) THEN
    ALTER TABLE reconcile_windows ADD CONSTRAINT ck_reconcile_status
      CHECK (status IN ('ok','alarm','error','blocked','skipped'));
  END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_reconcile_status ON reconcile_windows(status, window_start DESC);

-- 补拉扫描：blocked 窗口按起点升序取（重启后「不静默」要靠它）。
CREATE INDEX IF NOT EXISTS idx_reconcile_blocked ON reconcile_windows(window_start) WHERE status = 'blocked';