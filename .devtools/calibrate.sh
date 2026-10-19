#!/usr/bin/env bash
# 先放开限流（避免 429 污染行为测试），再标定 est 越过 6000 的填充量，最后做「阈值下不切 / 越阈值切换」测试。
set -euo pipefail
BASE="http://127.0.0.1:8090/api/v1"; GW="http://127.0.0.1:8090"
T=$(curl -s -X POST "$BASE/auth/login" -H 'Content-Type: application/json' -d '{"Username":"admin","Password":"admin123"}' | jq -r .Data.Token)
AUTH=(-H "Authorization: Bearer $T" -H 'Content-Type: application/json')
api() { local m="$1" p="$2" d="${3:-}"; if [ -n "$d" ]; then curl -s -X "$m" "$BASE$p" "${AUTH[@]}" -d "$d"; else curl -s -X "$m" "$BASE$p" "${AUTH[@]}"; fi; }

RL_HI='{"RPM":1000,"TPM":100000000,"BurstMultiplier":1,"OnExceed":"REJECT","QueueSize":0,"QueueTimeoutMS":0,"MaxConcurrent":100}'

echo "== 放开限流（渠道级 + 模型级）=="
for chname in "演示渠道-正常A" "演示渠道-正常B" "演示渠道-慢速" "演示渠道-限流" "演示渠道-故障"; do
  chid=$(api GET /admin/channels | jq -r --arg n "$chname" '.Data[]|select(.Name==$n)|.ID')
  cur=$(api GET /admin/channels | jq -c --arg id "$chid" '.Data[]|select(.ID==$id)')
  body=$(echo "$cur" | jq -c --argjson rl "$RL_HI" '{Name,Protocol,BaseURL,Priority,TagIDs,ChannelProtocolID,RateLimit:$rl,HealthProbe,Reliability}')
  api PUT "/admin/channels/$chid" "$body" >/dev/null
  for mid in $(api GET "/admin/channels/$chid/models" | jq -r '.Data[].ID'); do
    row=$(api GET "/admin/channels/$chid/models" | jq -c --arg id "$mid" '.Data[]|select(.ID==$id)')
    mbody=$(echo "$row" | jq -c --argjson rl "$RL_HI" '{InternalModelID,ExternalModelID,CostRates,ContextTiers,TimeConfig,RateLimit:$rl,Reliability}')
    api PUT "/admin/channels/$chid/models/$mid" "$mbody" >/dev/null
  done
done
echo "  done"

TOK=$(curl -s -X POST "$BASE/admin/tokens" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d "{\"UserID\":\"$(api GET /admin/users | jq -r '.Data.List[]|select(.Username=="admin")|.ID')\",\"DisplayName\":\"cal-token\"}" | jq -r .Data.Plain)
curl -s -X POST "$BASE/admin/users/$(api GET /admin/users | jq -r '.Data.List[]|select(.Username=="admin")|.ID')/wallet/set" "${AUTH[@]}" -d '{"Balance":100000000,"Remark":"cal","Password":"admin123"}' >/dev/null

SENT="The quick brown fox jumps over the lazy dog. "
echo "== 标定（读决策轨迹的 ctx_coeff：5=est<6000，1=est≥6000）=="
for reps in 200 400 600 800 1000; do
  PAD=""; for _ in $(seq 1 "$reps"); do PAD="$PAD$SENT"; done
  printf '%s' "{\"model\":\"demo-chat-pro\",\"stream\":false,\"messages\":[{\"role\":\"user\",\"content\":\"c-$reps $PAD\"}]}" > /tmp/body.json
  curl -s -o /dev/null --data-binary @/tmp/body.json -X POST "$GW/v1/chat/completions" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json'
  sleep 0.3
  ctx=$(PGPASSWORD=llmgw psql -h 127.0.0.1 -U llmgw -d llmgw -tAc "select (decision->>'ctx_coeff') from call_logs order by created_at desc limit 1;")
  echo "  reps=$reps (~$((reps*45)) 字符)  ctx_coeff=$ctx"
done