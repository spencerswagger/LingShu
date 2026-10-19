#!/usr/bin/env bash
# 并发压测：验证「最便宜优先 / 限流(429) / 会话均摊」。
set -euo pipefail
BASE="http://127.0.0.1:8090/api/v1"
GW="http://127.0.0.1:8090"
N="${1:-40}"

T=$(curl -s -X POST "$BASE/auth/login" -H 'Content-Type: application/json' \
  -d '{"Username":"admin","Password":"admin123"}' | jq -r .Data.Token)
AUID=$(curl -s "$BASE/admin/users" -H "Authorization: Bearer $T" | jq -r '.Data.List[]|select(.Username=="admin")|.ID')
TOK=$(curl -s -X POST "$BASE/admin/tokens" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d "{\"UserID\":\"$AUID\",\"DisplayName\":\"burst-token\"}" | jq -r .Data.Plain)
echo "$TOK" > /tmp/demo-token.txt
curl -s -X POST "$BASE/admin/users/$AUID/wallet/set" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d '{"Balance":100000,"Remark":"demo","Password":"admin123"}' >/dev/null
echo "token=$TOK"

rm -f /tmp/burst-codes.txt
for i in $(seq 1 "$N"); do
  (
    code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 25 -X POST "$GW/v1/chat/completions" \
      -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
      -d "{\"model\":\"demo-chat-pro\",\"messages\":[{\"role\":\"user\",\"content\":\"burst-$i\"}],\"stream\":false}")
    echo "$code" >> /tmp/burst-codes.txt
  ) &
done
wait

echo "=== HTTP 状态码分布 ==="
sort /tmp/burst-codes.txt | uniq -c

echo "=== 实际命中：call_logs 按 渠道/密钥（近 3 分钟）==="
PGPASSWORD=llmgw psql -h 127.0.0.1 -U llmgw -d llmgw -c \
"select c.name as channel, cl.channel_key_id, count(*) as hits
 from call_logs cl left join channels c on c.id = cl.channel_id
 where cl.created_at > now() - interval '3 minutes'
 group by 1,2 order by 3 desc;"

echo "=== 存活会话按密钥 ==="
PGPASSWORD=llmgw psql -h 127.0.0.1 -U llmgw -d llmgw -c \
"select c.name as channel, s.channel_key_id, count(*) as sessions
 from sessions s left join channels c on c.id = (select channel_id from channel_keys k where k.id = s.channel_key_id)
 group by 1,2 order by 3 desc;"
