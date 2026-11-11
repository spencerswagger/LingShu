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
