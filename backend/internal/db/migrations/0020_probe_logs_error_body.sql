-- 0020_probe_logs_error_body：探活历史落库「上游原始错误报文」。
--
-- 目的：运营新建渠道密钥后的第一个动作就是点探活。历史上非 2xx 只落一句
-- 「probe http status: 400」——上游到底拒绝了什么（区域未开通 / 模型名不对 / 余额不足 /
-- 请求参数不被接受）运营完全看不到，只能去翻上游控制台。
--
-- 语义：
--   - error_body      上游错误响应体原文（运营可见）：逐字节保真，只把本次实际使用的凭据原值
--                     替换成 ***（不做样式清洗/白名单/字段裁剪，否则 request-id、模型名会被抹掉）；
--                     超过 32KB 时按 UTF-8 边界截断，截断标注与被截原文写在同一个字段。
--                     空串 = 上游没有返回错误体（连接失败/超时/未打上游）。
--   - upstream_status 本次探测拿到的上游 HTTP 状态码；NULL = 没打到上游（连接失败/超时/无可用模型或密钥）。
--
-- 幂等：ADD COLUMN IF NOT EXISTS；旧数据按默认值（'' / NULL）读，属预期。
ALTER TABLE probe_logs ADD COLUMN IF NOT EXISTS error_body TEXT NOT NULL DEFAULT '';
ALTER TABLE probe_logs ADD COLUMN IF NOT EXISTS upstream_status SMALLINT NULL;

COMMENT ON COLUMN probe_logs.error_body IS
    '上游错误响应体原文（运营可见；仅遮蔽本次凭据值；>32KB 按 UTF-8 边界截断并同字段标注；空=上游无错误体）';
COMMENT ON COLUMN probe_logs.upstream_status IS
    '本次探测的上游 HTTP 状态码；NULL=没打到上游（连接失败/超时/无可用模型或密钥）';

-- 回滚路径（DOWN，手工执行；迁移框架只跑 up，每文件一事务，故回滚按下面的逆操作走）：
--
--   1) 只想「停用」而不丢数据（推荐，先回代码再回库）：
--        ALTER TABLE probe_logs ALTER COLUMN error_body SET DEFAULT '';
--        -- 代码回滚后即不再写入/读取这两列，列留着不影响旧版本（旧 INSERT 显式列出列名）。
--
--   2) 彻底移除（会丢掉历史原文，DROP 前请按需导出）：
--        ALTER TABLE probe_logs DROP COLUMN IF EXISTS error_body;
--        ALTER TABLE probe_logs DROP COLUMN IF EXISTS upstream_status;
--        DELETE FROM applied_migrations WHERE name = '0020_probe_logs_error_body.sql';
--
--   3) 重放幂等：本文件全部是 ADD COLUMN IF NOT EXISTS，重复执行不报错、不改既有行
--      （旧行保持 '' / NULL，新行写原文），所以「回滚后重放」不会破坏已有数据。
--
-- 不变量：本迁移只新增两列（带默认值），**不修改、不删除、不回填**任何既有列与既有行，
-- 因此对既有数据语义零影响（旧行的 error_body=''、upstream_status=NULL 即为「当时没有记录」）。
--
-- 为什么**不加索引**：两列都只用于「取某条/某批探测记录时一并读出」，唯一的检索维度是
-- `probe_logs.channel_key_id`（已有索引）+ 主键 `id`，不存在按 `upstream_status` 或 `error_body`
-- 过滤的查询；`error_body` 是大文本，建索引既无收益又会让写入放大（TOAST 写入 + 索引维护）。
-- 列表接口已按「行数上限 100 + 每行 4KB 预览」收口响应体积（完整原文走单条详情端点），
-- 因此也不需要为列表另建覆盖索引。
