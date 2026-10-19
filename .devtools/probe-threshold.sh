#!/usr/bin/env bash
# 探测：不同 padding 长度下，起点择优落在 正常A（est<6000，B 分档 ×5）还是 正常B（est≥6000）。
# 单次请求、新会话 → 只受起点择优影响，用于标定 tokenizer 的 est 与字符数的关系。
set -euo pipefail
BASE="http://127.0.0.1:8090/api/v1"; GW="http://127.0.0.1:8090"
T=$(curl -s -X POST "$BASE/auth/login" -H 'Content-Type: application/json' -d '{"Username":"admin","Password":"admin123"}' | jq -r .Data.Token)
AUID=$(curl -s "$BASE/admin/users" -H "Authorization: Bearer $T" | jq -r '.Data.List[]|select(.Username=="admin")|.ID')
TOK=$(curl -s -X POST "$BASE/admin/tokens" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d "{\"UserID\":\"$AUID\",\"DisplayName\":\"probe-token\"}" | jq -r .Data.Plain)
curl -s -X POST "$BASE/admin/users/$AUID/wallet/set" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' -d '{"Balance":100000,"Remark":"probe","Password":"admin123"}' >/dev/null

last_channel() {
  PGPASSWORD=llmgw psql -h 127.0.0.1 -U llmgw -d llmgw -tAc \
  "select c.name from sessions s join tokens t on t.id=s.token_id join channel_keys k on k.id=s.channel_key_id join channels c on c.id=k.channel_id where t.display_name='probe-token' order by s.created_at desc limit 1;"
}

for n in 5000 20000 40000 100000 400000; do
  PAD=$(printf 'a%.0s' $(seq 1 "$n"))
  # 每条不同 seed（含长度）→ 各自新会话，避免互相干扰
  curl -s -o /dev/null -X POST "$GW/v1/chat/completions" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
    -d "{\"model\":\"demo-chat-pro\",\"stream\":false,\"messages\":[{\"role\":\"user\",\"content\":\"probe-$n-$PAD\"}]}"
  echo "  chars=$n  →  $(last_channel)"
done