#!/usr/bin/env bash
# 阈值对比：同一会话，上下文 <6000 保持不切；>6000 触发切换到更便宜的「正常B」。
set -euo pipefail
BASE="http://127.0.0.1:8090/api/v1"; GW="http://127.0.0.1:8090"

T=$(curl -s -X POST "$BASE/auth/login" -H 'Content-Type: application/json' -d '{"Username":"admin","Password":"admin123"}' | jq -r .Data.Token)
AUID=$(curl -s "$BASE/admin/users" -H "Authorization: Bearer $T" | jq -r '.Data.List[]|select(.Username=="admin")|.ID')
TOK=$(curl -s -X POST "$BASE/admin/tokens" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d "{\"UserID\":\"$AUID\",\"DisplayName\":\"cp2-token\"}" | jq -r .Data.Plain)
curl -s -X POST "$BASE/admin/users/$AUID/wallet/set" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' -d '{"Balance":100000,"Remark":"cp","Password":"admin123"}' >/dev/null
PAD=$(printf 'a%.0s' $(seq 1 40000))

cur_channel() {
  PGPASSWORD=llmgw psql -h 127.0.0.1 -U llmgw -d llmgw -tAc \
  "select c.name from sessions s join tokens t on t.id=s.token_id join channel_keys k on k.id=s.channel_key_id join channels c on c.id=k.channel_id where t.display_name='cp2-token' order by s.created_at desc limit 1;"
}
send() { curl -s -o /dev/null -w "%{http_code}" -X POST "$GW/v1/chat/completions" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d "$1"; }

S3='{"role":"user","content":"turn1-hello"},{"role":"assistant","content":"ok"},{"role":"user","content":"turn1-continue"}'

echo "轮1 小上下文：" ; echo "  http=$(send "{\"model\":\"demo-chat-pro\",\"stream\":false,\"messages\":[$S3]}")"; sleep 0.8; echo "  → 渠道=$(cur_channel)"

echo "轮2 同小上下文（阈值下，期望仍为 正常A）："; echo "  http=$(send "{\"model\":\"demo-chat-pro\",\"stream\":false,\"messages\":[$S3]}")"; sleep 0.8; echo "  → 渠道=$(cur_channel)"

echo "轮3 追加 4 万字符（越阈值，期望切到 正常B）："; echo "  http=$(send "{\"model\":\"demo-chat-pro\",\"stream\":false,\"messages\":[$S3,{\"role\":\"user\",\"content\":\"$PAD\"}]}")"; sleep 0.8; echo "  → 渠道=$(cur_channel)"

echo "=== 该令牌的调用日志（每轮实际服务的渠道）==="
PGPASSWORD=llmgw psql -h 127.0.0.1 -U llmgw -d llmgw -c \
"select cl.channel_key_id, c.name as channel, cl.internal_model_id, cl.created_at
 from call_logs cl join tokens t on t.id=cl.token_id left join channel_keys k on k.id=cl.channel_key_id left join channels c on c.id=k.channel_id
 where t.display_name='cp2-token' order by cl.created_at;" 2>&1 | head -n 12