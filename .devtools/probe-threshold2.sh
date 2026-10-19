#!/usr/bin/env bash
# 标定：用真实句子填充，找出 est 越过 6000 的字符数（单请求、新会话 → 起点择优落 A 还是 B）。
set -euo pipefail
BASE="http://127.0.0.1:8090/api/v1"; GW="http://127.0.0.1:8090"
T=$(curl -s -X POST "$BASE/auth/login" -H 'Content-Type: application/json' -d '{"Username":"admin","Password":"admin123"}' | jq -r .Data.Token)
AUID=$(curl -s "$BASE/admin/users" -H "Authorization: Bearer $T" | jq -r '.Data.List[]|select(.Username=="admin")|.ID')
TOK=$(curl -s -X POST "$BASE/admin/tokens" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d "{\"UserID\":\"$AUID\",\"DisplayName\":\"probe2-token\"}" | jq -r .Data.Plain)
curl -s -X POST "$BASE/admin/users/$AUID/wallet/set" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' -d '{"Balance":100000,"Remark":"probe2","Password":"admin123"}' >/dev/null

last_channel() {
  PGPASSWORD=llmgw psql -h 127.0.0.1 -U llmgw -d llmgw -tAc \
  "select c.name from sessions s join tokens t on t.id=s.token_id join channel_keys k on k.id=s.channel_key_id join channels c on c.id=k.channel_id where t.display_name='probe2-token' order by s.created_at desc limit 1;"
}
SENT="The quick brown fox jumps over the lazy dog. "

for reps in 100 300 600 1000 1500 2500 4000; do
  PAD=""
  for _ in $(seq 1 "$reps"); do PAD="$PAD$SENT"; done
  printf '%s' "{\"model\":\"demo-chat-pro\",\"stream\":false,\"messages\":[{\"role\":\"user\",\"content\":\"p-$reps $PAD\"}]}" > /tmp/body.json
  code=$(curl -s -o /dev/null -w "%{http_code}" --data-binary @/tmp/body.json -X POST "$GW/v1/chat/completions" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json')
  echo "  reps=$reps (~$((reps*45)) 字符)  http=$code  →  $(last_channel)"
done