#!/usr/bin/env bash
# 模拟真实调用：建令牌 → 充值 → 经网关打多轮 /v1/chat/completions，并打印真实结果。
set -euo pipefail
BASE="http://127.0.0.1:8090/api/v1"
GW="http://127.0.0.1:8090"
N="${1:-6}"

T=$(curl -s -X POST "$BASE/auth/login" -H 'Content-Type: application/json' \
  -d '{"Username":"admin","Password":"admin123"}' | jq -r .Data.Token)
AUID=$(curl -s "$BASE/admin/users" -H "Authorization: Bearer $T" | jq -r '.Data.List[]|select(.Username=="admin")|.ID')
TOK=$(curl -s -X POST "$BASE/admin/tokens" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d "{\"UserID\":\"$AUID\",\"DisplayName\":\"demo-token\"}" | jq -r .Data.Plain)
echo "admin_uid=$AUID token=$TOK"
curl -s -X POST "$BASE/admin/users/$AUID/wallet/set" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d '{"Balance":100000,"Remark":"demo","Password":"admin123"}' | jq -c '{Code,Message}'

echo "=== 发送 $N 次真实请求（model=demo-chat-pro）==="
for i in $(seq 1 "$N"); do
  code=$(curl -s -o "/tmp/sim-$i.json" -w "%{http_code}" -X POST "$GW/v1/chat/completions" \
    -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
    -d "{\"model\":\"demo-chat-pro\",\"messages\":[{\"role\":\"user\",\"content\":\"ping-$i\"}],\"stream\":false}")
  echo "  req$i http=$code"
done

echo "=== 首次响应头（观察上游/路由线索）==="
curl -s -D - -o /dev/null -X POST "$GW/v1/chat/completions" \
  -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"model":"demo-chat-pro","messages":[{"role":"user","content":"hdr-probe"}],"stream":false}' \
  | grep -iE "^(HTTP/|x-|content-type)" | head -n 20

echo "=== 响应体样例 ==="
head -c 260 /tmp/sim-1.json; echo

echo "=== 把令牌写入文件供后续核对 ==="
echo "$TOK" > /tmp/demo-token.txt
