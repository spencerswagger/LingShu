#!/usr/bin/env bash
# 并发压测（不登录，复用已有令牌，避免顶掉浏览器会话）：验证限流 / 会话均摊。
set -euo pipefail
GW="http://127.0.0.1:8090"
TOK=$(cat /tmp/demo-token.txt)
N="${1:-30}"
rm -f /tmp/b2.txt
for i in $(seq 1 "$N"); do
  (
    code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 25 -X POST "$GW/v1/chat/completions" \
      -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
      -d "{\"model\":\"demo-chat-pro\",\"messages\":[{\"role\":\"user\",\"content\":\"b2-$i\"}],\"stream\":false}")
    echo "$code" >> /tmp/b2.txt
  ) &
done
wait
echo "=== HTTP 分布 ==="
sort /tmp/b2.txt | uniq -c