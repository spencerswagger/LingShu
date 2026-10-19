#!/usr/bin/env bash
# 临界点切换测试：
#   轮1 小上下文（<6000）→ 起点择优选「正常A」（原始价高但无分档，小上下文最便宜）
#   轮2 同一会话、上下文增长到 >6000 → 「正常B」分档系数回落 ×1 变最便宜 → 期望中途切换到 B
set -euo pipefail
BASE="http://127.0.0.1:8090/api/v1"
GW="http://127.0.0.1:8090"

T=$(curl -s -X POST "$BASE/auth/login" -H 'Content-Type: application/json' \
  -d '{"Username":"admin","Password":"admin123"}' | jq -r .Data.Token)
AUID=$(curl -s "$BASE/admin/users" -H "Authorization: Bearer $T" | jq -r '.Data.List[]|select(.Username=="admin")|.ID')
TOK=$(curl -s -X POST "$BASE/admin/tokens" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d "{\"UserID\":\"$AUID\",\"DisplayName\":\"cpoint-token\"}" | jq -r .Data.Plain)
echo "token=$TOK"
curl -s -X POST "$BASE/admin/users/$AUID/wallet/set" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d '{"Balance":100000,"Remark":"cp","Password":"admin123"}' >/dev/null

PAD=$(printf 'a%.0s' $(seq 1 40000))

echo "=== 轮1：小上下文（期望落在 正常A）==="
curl -s -o /dev/null -w "  http=%{http_code}\n" -X POST "$GW/v1/chat/completions" \
  -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"model":"demo-chat-pro","stream":false,"messages":[{"role":"user","content":"turn1-hello"},{"role":"assistant","content":"ok"},{"role":"user","content":"turn1-continue"}]}'

sleep 1
echo "  会话归属："
PGPASSWORD=llmgw psql -h 127.0.0.1 -U llmgw -d llmgw -tAc \
"select '    key='||s.channel_key_id||' 渠道='||c.name from sessions s join channel_keys k on k.id=s.channel_key_id join channels c on c.id=k.channel_id order by s.created_at desc limit 1;"

echo "=== 轮2：同会话 + 大上下文（期望触发切换到 正常B）==="
curl -s -o /dev/null -w "  http=%{http_code}\n" -X POST "$GW/v1/chat/completions" \
  -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d "{\"model\":\"demo-chat-pro\",\"stream\":false,\"messages\":[{\"role\":\"user\",\"content\":\"turn1-hello\"},{\"role\":\"assistant\",\"content\":\"ok\"},{\"role\":\"user\",\"content\":\"turn1-continue\"},{\"role\":\"user\",\"content\":\"$PAD\"}]}"

sleep 1
echo "=== 轮2 后 会话归属（期望变为 正常B）==="
PGPASSWORD=llmgw psql -h 127.0.0.1 -U llmgw -d llmgw -tAc \
"select '    key='||s.channel_key_id||' 渠道='||c.name from sessions s join channel_keys k on k.id=s.channel_key_id join channels c on c.id=k.channel_id order by s.created_at desc limit 1;"

echo "=== 会话成本决策事件（/session-cost/events）==="
curl -s "$BASE/admin/session-cost/events?Limit=10" -H "Authorization: Bearer $T" | jq -r '.Data.List[] | "  turn=\(.Turn) switch=\(.Switch) \(.FromChannelKeyID)->\(.ToChannelKeyID) pb=\(.PaybackTurns) rem=\(.RemainingTurns) cross=\(.CrossCacheRead) | \(.Reason)"'