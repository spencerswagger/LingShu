-- 0019_call_logs_upstream_status：调用日志落库「上游真实 HTTP 状态码」。
--
-- 目的：运营/调用方拿着 response 里的 request_id 在账单页定位一次失败时，要能直接看出
-- 「上游返回了什么状态码」，而不是从 error_message 的中文说明里推断
-- （历史上 error_message 形如「上游返回 400: {上游错误体}」，状态码只存在于自由文本里，
-- 既不能按列筛选，也会随文案调整而失效）。
--
-- 语义（写上游**真实**状态码，不写网关改写后的码）：
--   - 上游返回非 2xx 的失败路径写入原值（400 / 401 / 429 / 500 …）；
--   - 流式「已回 200、SSE 中途上游发 error 帧」的失败路径写 200——让「上游 HTTP 200 但内容层失败」
--     与「上游 4xx/5xx」「网关自身拒绝」三者可区分；
--   - NULL 表示无上游状态码：连接失败、响应超时、网关自身拒绝（鉴权/余额/路由/协议不支持）等。
--
-- 幂等：ADD COLUMN IF NOT EXISTS；旧数据该列为 NULL（属预期）。
ALTER TABLE call_logs ADD COLUMN IF NOT EXISTS upstream_status SMALLINT NULL;

COMMENT ON COLUMN call_logs.upstream_status IS
    '上游真实 HTTP 状态码（上游非 2xx 写原值；流式已 200 后流内报错写 200；NULL=无上游状态码，如连接失败/超时/网关拒绝）';

-- 回滚路径（DOWN，手工执行；迁移框架只跑 up，每文件一事务，故回滚按下面的逆操作走）：
--
--   1) 只想「停用」而不丢数据（推荐，先回代码再回库）：
--        -- 代码回滚后即不再写入/读取该列，列留着不影响旧版本（旧 INSERT 显式列出列名）。
--        COMMENT ON COLUMN call_logs.upstream_status IS '（已停用）上游真实 HTTP 状态码';
--
--   2) 彻底移除（会丢掉历史状态码；调用方仍可从 error_message 文本里看到「上游返回 N」）：
--        ALTER TABLE call_logs DROP COLUMN IF EXISTS upstream_status;
--        DELETE FROM applied_migrations WHERE name = '0019_call_logs_upstream_status.sql';
--
--   3) 重放幂等：本文件是 ADD COLUMN IF NOT EXISTS，重复执行不报错、不改既有行
--      （旧行保持 NULL = 「当时没有记录」，不会被回填成假值）。
--
-- 为什么**不加索引**：该列只用于「展示/人工筛选」，唯一的按值筛选是运营在账单详情里看单条记录
-- （走主键/`request_id` 已有索引），不存在 `WHERE upstream_status = ?` 的高频查询路径；
-- 而 call_logs 是写入密集表，多一个索引就多一份写放大与维护成本。若后续出现按状态码聚合的
-- 报表需求，应在报表侧走预聚合，而不是给这张表加单列索引。
--
-- 不变量：本迁移只新增一列（可空、无默认值回填），**不修改、不删除、不回填**任何既有列与既有行。
