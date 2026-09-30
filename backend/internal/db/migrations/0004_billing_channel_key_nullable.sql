-- 网关拒绝类失败账单（未进入渠道，如零余额/模型未配置/无路由）可能没有 channel_key 维度：
-- channel_key_id 改为可空，写入 NULL（0 不再视为有效外键），SQL 层 Insert/Scan 同步适配。
ALTER TABLE billing_records ALTER COLUMN channel_key_id DROP NOT NULL;