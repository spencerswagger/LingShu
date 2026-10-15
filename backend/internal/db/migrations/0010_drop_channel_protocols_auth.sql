-- 鉴权改由基础协议隐含（ProtocolAuth）：openai-chat/openai-responses → Authorization Bearer，
-- anthropic-messages → x-api-key（raw）；若需协议以外的鉴权方式，用请求头策略的「覆盖(set)」显式写入。
-- 渠道协议不再单独保存鉴权配置，删除 channel_protocols.auth 列。
-- 使用 IF EXISTS 保证幂等（旧环境可能已手工删除）。
-- 0007_channel_protocols.sql 保持不动（迁移不重写历史）。
ALTER TABLE channel_protocols DROP COLUMN IF EXISTS auth;
