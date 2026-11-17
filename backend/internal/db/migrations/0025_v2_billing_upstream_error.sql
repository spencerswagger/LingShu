-- 0025_v2_billing_upstream_error：账单状态增 `upstream_error` + 上游错误留痕列。
--
-- 为什么需要这条迁移（不是枚举洁癖，是运营口径）：
--   旧实现把「上游 4xx/5xx」和「上游 2xx 但没有 usage」都落成 `failed` + `error_message='no-usage'`，
--   运营在账单页看到的是「没用量」，实际是上游报错 —— 两类问题混在一起无法分派：
--     · `no-usage`：上游答了 2xx 但没回 usage（内容可能已白送），要查 usage 注入/协议；
--     · `upstream_error`：上游 4xx/5xx 或传输失败，要查上游/凭据/限流。
--   新口径：`upstream_error` 单独成状态；**仍然不退化成收费**（没有可结算 usage 时照样全额退预扣、
--   金额 0），只是把「为什么没计费」说清楚。
--
-- 上游状态码与错误体摘要：
--   `upstream_status` 是上游 HTTP 码（0/NULL = 没收到响应，例如连接失败/超时）；
--   `upstream_error_summary` 是错误体摘要（已遮蔽本次凭据、折叠空白、UTF-8 安全截断到 512B）。
--   两列只在上游失败时有值；成功/无 usage 的 2xx 行留 NULL（不制造「上游 200 也是错误」的误读）。
--
-- 幂等：CHECK 先 DROP 再 ADD（PG 没有 ADD CONSTRAINT IF NOT EXISTS）；ADD COLUMN IF NOT EXISTS。
-- 可回滚：把 status='upstream_error' 的行改回 'failed' 后再恢复旧 CHECK、删两列
-- （回滚走「同批前进」或整库还原；本文件不写 DOWN 段，
--  迁移框架没有 down 通道，回滚走整库还原或前滚；见
--  docs/designs/2026-11-17-magpie-ledger-adapter-and-upstream-error.md）。

-- 0001 的列内联 CHECK 自动命名是 billing_records_status_check；先删它，否则新值域不生效。
ALTER TABLE billing_records DROP CONSTRAINT IF EXISTS billing_records_status_check;
ALTER TABLE billing_records DROP CONSTRAINT IF EXISTS ck_billing_status;
-- NOT VALID + VALIDATE：避免在大表上长时间持 AccessExclusive 锁做全表扫描。
-- 存量值都在旧值域内，VALIDATE 只做一遍较轻的校验（ShareUpdateExclusive）。
ALTER TABLE billing_records ADD CONSTRAINT ck_billing_status
  CHECK (status IN ('completed','failed','upstream_error')) NOT VALID;
ALTER TABLE billing_records VALIDATE CONSTRAINT ck_billing_status;

ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS upstream_status SMALLINT;
ALTER TABLE billing_records ADD COLUMN IF NOT EXISTS upstream_error_summary TEXT;

COMMENT ON COLUMN billing_records.upstream_status IS
  '上游 HTTP 状态码；NULL/0 表示没收到响应（连接失败/超时）';
COMMENT ON COLUMN billing_records.upstream_error_summary IS
  '上游错误体摘要（遮蔽凭据、折叠空白、UTF-8 安全截断 512B）；仅 upstream_error 行有值';
