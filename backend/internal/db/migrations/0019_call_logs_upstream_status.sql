-- 0019_call_logs_upstream_status：调用日志落库「上游真实 HTTP 状态码」。
--
-- 目的：运营/调用方拿着 response 里的 request_id 在账单页定位一次失败时，要能直接看出
-- 「上游返回了什么状态码」，而不是从 error_message 的中文说明里推断
-- （历史上 error_message 形如「上游返回 400: {上游错误体}」，状态码只存在于自由文本里，
-- 既不能按列筛选，也会随文案调整而失效）。
--
-- 语义：
--   - 仅「上游返回非 2xx」的失败路径写入（4xx/5xx 原值，如 400 / 401 / 429 / 500）；
--   - NULL 表示无上游状态码：连接失败、响应超时、网关自身拒绝（鉴权/余额/路由/协议不支持）等。
--
-- 幂等：ADD COLUMN IF NOT EXISTS；旧数据该列为 NULL（属预期）。
ALTER TABLE call_logs ADD COLUMN IF NOT EXISTS upstream_status SMALLINT NULL;

COMMENT ON COLUMN call_logs.upstream_status IS
    '上游真实 HTTP 状态码（仅上游非 2xx 的失败路径写入；NULL=无上游状态码，如连接失败/超时/网关拒绝）';
