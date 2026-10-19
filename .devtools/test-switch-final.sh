#!/usr/bin/env bash
# 最终测试：同一会话
#   轮1 小上下文（est≈10）→ 起点择优 → 正常A
#   轮2 同 seed + 大填充（est≈9000 ≥6000）→ 期望「中途切换」到 正常B，且事件流出现 switch 事件
set -euo pipefail
BASE="http://127.0.0.1:8090/api/v1"; GW="http://127.0.0.1:8090"
T=$(curl -s -X POST "$BASE/auth/login" -H 'Content-Type: application/json' -d '{"Username":"admin","Password":"admin123"}' | jq -r .Data.Token)
AUID=$(curl -s "$BASE/admin/users" -H "Authorization: Bearer $T" | jq -r '.Data.List[]|select(.Username=="admin")|.ID')
TOK=$(curl -s -X POST "$BASE/admin/tokens" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d "{\"UserID\":\"$AUID\",\"DisplayName\":\"final-token\"}" | jq -r .Data.Plain)
curl -s -X POST "$BASE/admin/users/$AUID/wallet/set" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' -d '{"Balance":100000000,"Remark":"f","Password":"admin123"}' >/dev/null

SENT="The quick brown fox jumps over the lazy dog. "
REPS=$1
PAD=""; for _ in $(seq 1 "$REPS"); do PAD="$PAD$SENT"; done

ch() { PGPASSWORD=llmgw psql -h 127.0.0.1 -U llmgw -d llmgw -tAc \
 "select c.name from sessions s join tokens t on t.id=s.token_id join channel_keys k on k.id=s.channel_key_id join channels c on c.id=k.channel_id where t.display_name='final-token' order by s.created_at desc limit 1;"; }

echo "轮1 小上下文："
printf '%s' '{"model":"demo-chat-pro","stream":false,"messages":[{"role":"user","content":"seed-A"},{"role":"assistant","content":"ok"},{"role":"user","content":"seed-B"}]}' > /tmp/f1.json
echo "  http=$(curl -s -o /dev/null -w '%{http_code}' --data-binary @/tmp/f1.json -X POST "$GW/v1/chat/completions" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json')  渠道=$(ch)"

sleep 1
echo "轮2 同 seed + 大填充（reps=$REPS, est≈$((REPS*45/2)) ≥6000）："
printf '%s' "{\"model\":\"demo-chat-pro\",\"stream\":false,\"messages\":[{\"role\":\"user\",\"content\":\"seed-A\"},{\"role\":\"assistant\",\"content\":\"ok\"},{\"role\":\"user\",\"content\":\"seed-B\"},{\"role\":\"user\",\"content\":\"$PAD\"}]}" > /tmp/f2.json
echo "  http=$(curl -s -o /dev/null -w '%{http_code}' --data-binary @/tmp/f2.json -X POST "$GW/v1/chat/completions" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json')  渠道=$(ch)"

sleep 1.5
echo "=== 会话成本事件流 ==="
curl -s "$BASE/admin/session-cost/events" -H "Authorization: Bearer $T" | jq -r '.Data.List[] | "  turn=\(.Turn) switch=\(.Switch) \(.FromChannelKeyID)->\(.ToChannelKeyID) pb=\(.PaybackTurns) rem=\(.RemainingTurns) | \(.Reason)"'
echo "=== 该会话调用日志（逐轮实际渠道）==="
PGPASSWORD=llmgw psql -h 127.0.0.1 -U llmgw -d llmgw -c "select c.name channel, left(cl.session_id,10) sid, cl.created_at from call_logs cl left join channel_keys k on k.id=cl.channel_key_id left join channels c on c.id=k.channel_id where cl.session_id in (select session_id from sessions s join tokens t on t.id=s.token_id where t.display_name='final-token') order by cl.created_at;"