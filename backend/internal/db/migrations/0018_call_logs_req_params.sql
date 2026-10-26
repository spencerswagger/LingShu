-- 0018_call_logs_req_params：调用日志落库「入站请求参数快照」。
--
-- 目的：让一次请求可被完整定位——凭 request_id 找到它之后，能直接看到当初到底发了什么参数
-- （模型、采样参数、工具摘要、端点、上游协议与上游 URL、令牌维度），而不必回捞入站请求体。
-- 与 req_messages 的分工：req_messages 是「消息增量」（会话拼接用，语义不变），
-- req_params 是「消息之外的全部请求参数」——两者互补，不重叠（req_params 不含 messages）。
--
-- 落库形态（见 gateway/reqparams.go）：
--   {"endpoint":"/v1/chat/completions","inbound_protocol":"openai-chat",
--    "upstream_protocol":"anthropic-messages","upstream_url":"https://api.example.com/v1/messages",
--    "token_id":123,"model":"gpt-4o","stream":true,"message_count":7,
--    "tools":{"count":2,"names":["get_weather","search"]},
--    "params":{"temperature":0.7,"max_tokens":1024,"top_p":1,"response_format":{...}},
--    "summarized":["tools"],"dropped":["metadata"]}
--
-- 上限与裁剪：序列化后 ≤8KB；超限时按字段优先级裁剪（先 tools 明细→大块参数→长尾参数），
-- 被裁剪/被摘要的字段名记在 dropped/summarized 里，**不会因为超限整体丢字段**（见 reqparams.go 的预算逻辑）。
-- 脱敏：上游 URL 在写入前即去掉 userinfo 与 query（query 可能带 api-key/api-version 等敏感值）。
--
-- 幂等：ADD COLUMN IF NOT EXISTS；旧数据该列为 NULL（无快照，属预期）。
ALTER TABLE call_logs ADD COLUMN IF NOT EXISTS req_params JSONB NULL;

COMMENT ON COLUMN call_logs.req_params IS
    '入站请求参数快照（不含 messages；≤8KB，超限按优先级裁剪并标记 dropped/summarized）';
