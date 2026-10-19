#!/usr/bin/env bash
# 在平台中配置演示数据：2 个对外模型 + 5 个渠道（各自密钥/内部模型，指向 mock 的不同 profile）。
set -euo pipefail
BASE="http://127.0.0.1:8090/api/v1"

TOKEN=$(curl -s -X POST "$BASE/auth/login" -H 'Content-Type: application/json' \
  -d '{"Username":"admin","Password":"admin123"}' | jq -r '.Data.Token')
[ -n "$TOKEN" ] && [ "$TOKEN" != "null" ] || { echo "登录失败"; exit 1; }
AUTH=(-H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json')

api() { # api METHOD PATH JSON
  local m="$1" p="$2" d="${3:-}"
  if [ -n "$d" ]; then
    curl -s -X "$m" "$BASE$p" "${AUTH[@]}" -d "$d"
  else
    curl -s -X "$m" "$BASE$p" "${AUTH[@]}"
  fi
}

echo "== 对外模型 =="
create_ext() {
  local name="$1" rates="$2"
  local id
  id=$(api POST /admin/models "{\"ExternalName\":\"$name\",\"Description\":\"演示模型\",\"Enabled\":true,\"SaleRates\":$rates}" | jq -r '.Data.ID')
  echo "  $name -> $id"
  echo "$id"
}
EXT_PRO=$(create_ext demo-chat-pro '{"Input":1.0,"Output":2.0,"CacheRead":0.2}' | tail -n1)
EXT_LITE=$(create_ext demo-chat-lite '{"Input":0.5,"Output":1.0,"CacheRead":0.1}' | tail -n1)

HP='{"Interval":"*/10 * * * * *","DrainIntervalSeconds":5,"TimeoutMS":5000,"FailThreshold":2,"RecoveryThreshold":1,"ProbeModel":""}'
REL='{"WindowSeconds":60,"MinSamples":3,"ErrorRatePct":10,"Rate429Pct":20,"P99LatencyMS":5000}'
RL='{"RPM":0,"TPM":0,"BurstMultiplier":1,"OnExceed":"REJECT","QueueSize":0,"QueueTimeoutMS":0,"MaxConcurrent":0}'

# name|baseurl|priority|keys(count)|profile
CHANNELS=(
  "演示渠道-正常A|http://127.0.0.1:9099/v1|100|2"
  "演示渠道-正常B|http://127.0.0.1:9099/v1|99|1"
  "演示渠道-慢速|http://127.0.0.1:9099/slow/v1|98|1"
  "演示渠道-限流|http://127.0.0.1:9099/ratelimited/v1|97|1"
  "演示渠道-故障|http://127.0.0.1:9099/dead/v1|96|1"
)

i=0
for row in "${CHANNELS[@]}"; do
  i=$((i+1))
  IFS='|' read -r name url prio nkeys <<<"$row"
  ch=$(api POST /admin/channels "{\"Name\":\"$name\",\"Protocol\":\"openai-chat\",\"BaseURL\":\"$url\",\"Priority\":$prio,\"RateLimit\":$RL,\"HealthProbe\":$HP,\"Reliability\":$REL}")
  chid=$(echo "$ch" | jq -r '.Data.ID')
  echo "== 渠道 $name -> id=$chid (base=$url) =="

  for k in $(seq 1 "$nkeys"); do
    api POST "/admin/channels/$chid/keys" "{\"Name\":\"演示密钥-$name-$k\",\"Credential\":\"sk-demo-$chid-$k\"}" >/dev/null
  done

  # 成本随渠道微调，便于观察候选成本差异
  p1=$(awk "BEGIN{printf \"%.2f\", 1.0+$i*0.05}")
  l1=$(awk "BEGIN{printf \"%.2f\", 0.5+$i*0.03}")
  api POST "/admin/channels/$chid/models" "{\"InternalModelID\":\"demo-chat-pro\",\"ExternalModelID\":\"$EXT_PRO\",\"CostRates\":{\"Input\":$p1,\"Output\":2.0,\"CacheRead\":0.2},\"RateLimit\":$RL,\"Reliability\":$REL}" >/dev/null
  api POST "/admin/channels/$chid/models" "{\"InternalModelID\":\"demo-chat-lite\",\"ExternalModelID\":\"$EXT_LITE\",\"CostRates\":{\"Input\":$l1,\"Output\":1.0,\"CacheRead\":0.1},\"RateLimit\":$RL,\"Reliability\":$REL}" >/dev/null
done

echo "== 完成 =="
api GET /admin/channels | jq -r '.Data[] | "  #\(.ID) \(.Name) \(.BaseURL) prio=\(.Priority)"'
