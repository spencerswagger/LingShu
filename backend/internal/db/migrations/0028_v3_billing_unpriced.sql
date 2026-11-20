-- 0028_v3_billing_unpriced：账单状态增 `unpriced`（V3 旁路计费的「未定价」行）。
--
-- 背景：V3 结算按账本 req_model 找对外模型；magpie 的 auto 组（如 auto-flash）
-- 账本记的是用户请求名，而灵枢对外模型的绑定键是组 ref（group/auto-flash）。
-- 解析不到对外模型的账本行，现在也照样落账单：营收 0、成本按账本 cost 兜底、
-- status='unpriced'、Remark 记原始模型名 —— 否则这些行永远结算不出去，
-- 会占满每轮批次头位，把后面的正常账单全部堵死（真机事故，2026-11-19）。
--
-- `unpriced` 与 `failed` 的区别：请求本身成功了（上游已产内容），
-- 只是灵枢侧没有定价口径；运营在拓扑页给该组绑定价后，后续请求即恢复正常计费。
--
-- 幂等：CHECK 先 DROP 再 ADD（PG 没有 ADD CONSTRAINT IF NOT EXISTS）。
-- 可回滚：把 status='unpriced' 的行改回 'failed' 后恢复旧 CHECK（或整库还原）。

ALTER TABLE billing_records DROP CONSTRAINT IF EXISTS ck_billing_status;
ALTER TABLE billing_records ADD CONSTRAINT ck_billing_status
  CHECK (status IN ('completed','failed','upstream_error','unpriced')) NOT VALID;
ALTER TABLE billing_records VALIDATE CONSTRAINT ck_billing_status;

COMMENT ON CONSTRAINT ck_billing_status ON billing_records IS
  'completed=正常计费；failed=无 usage 未计费；upstream_error=上游 4xx/5xx/传输失败；unpriced=请求成功但灵枢侧无定价口径（营收 0，成本按账本 cost 兜底）';
