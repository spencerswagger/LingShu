#!/usr/bin/env bash
# 演示数据：4 对外模型 × 5 渠道（优先级全部相同），成本非单调（让「序位」不再等于行显示顺序），
# 并把 RPM/TPM/并发上限调低，便于验证：最便宜优先 / 限流 / 会话均摊。
set -euo pipefail
BASE="http://127.0.0.1:8090/api/v1"

TOKEN=$(curl -s -X POST "$BASE/auth/login" -H 'Content-Type: application/json' \
  -d '{"Username":"admin","Password":"admin123"}' | jq -r '.Data.Token')
[ -n "$TOKEN" ] && [ "$TOKEN" != "null" ] || { echo "登录失败"; exit 1; }
AUTH=(-H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json')

api() { local m="$1" p="$2" d="${3:-}"; if [ -n "$d" ]; then curl -s -X "$m" "$BASE$p" "${AUTH[@]}" -d "$d"; else curl -s -X "$m" "$BASE$p" "${AUTH[@]}"; fi; }

EXT_MODELS=(
  "demo-chat-pro|{\"Input\":1.0,\"Output\":2.0,\"CacheRead\":0.2}"
  "demo-chat-lite|{\"Input\":0.5,\"Output\":1.0,\"CacheRead\":0.1}"
  "demo-chat-reasoner|{\"Input\":2.0,\"Output\":4.0,\"CacheRead\":0.4}"
  "demo-vision|{\"Input\":1.5,\"Output\":3.0,\"CacheRead\":0.3}"
)

echo "== 对外模型 =="
declare -A EXT_ID
for row in "${EXT_MODELS[@]}"; do
  IFS='|' read -r name rates <<<"$row"
  id=$(api POST /admin/models "{\"ExternalName\":\"$name\",\"Description\":\"演示模型\",\"Enabled\":true,\"SaleRates\":$rates}" | jq -r '.Data.ID')
  EXT_ID["$name"]="$id"
  echo "  $name -> $id"
done

# 优先级全部相同（100）；成本按渠道给定（非单调）；限流压到很低。
HP='{"Interval":"*/10 * * * * *","DrainIntervalSeconds":5,"TimeoutMS":5000,"FailThreshold":2,"RecoveryThreshold":1,"ProbeModel":""}'
REL='{"WindowSeconds":60,"MinSamples":3,"ErrorRatePct":10,"Rate429Pct":20,"P99LatencyMS":5000}'
RL='{"RPM":10,"TPM":20000,"BurstMultiplier":1,"OnExceed":"REJECT","QueueSize":0,"QueueTimeoutMS":0,"MaxConcurrent":2}'

# 名称|BaseURL|密钥数|Input成本（越小越便宜）
CHANNELS=(
  "演示渠道-正常A|http://127.0.0.1:9099/v1|2|1.20"
  "演示渠道-正常B|http://127.0.0.1:9099/v1|1|1.00"
  "演示渠道-慢速|http://127.0.0.1:9099/slow/v1|1|1.30"
  "演示渠道-限流|http://127.0.0.1:9099/ratelimited/v1|1|1.40"
  "演示渠道-故障|http://127.0.0.1:9099/dead/v1|1|1.10"
)

for row in "${CHANNELS[@]}"; do
  IFS='|' read -r name url nkeys base_cost <<<"$row"
  chid=$(api POST /admin/channels "{\"Name\":\"$name\",\"Protocol\":\"openai-chat\",\"BaseURL\":\"$url\",\"Priority\":100,\"RateLimit\":$RL,\"HealthProbe\":$HP,\"Reliability\":$REL}" | jq -r '.Data.ID')
  echo "== 渠道 $name -> id=$chid (priority=100, cost=$base_cost) =="

  for k in $(seq 1 "$nkeys"); do
    api POST "/admin/channels/$chid/keys" "{\"Name\":\"演示密钥-$name-$k\",\"Credential\":\"sk-demo-$chid-$k\"}" >/dev/null
  done

  j=0
  for em in "${EXT_MODELS[@]}"; do
    j=$((j+1))
    IFS='|' read -r ename _ <<<"$em"
    cost=$(awk "BEGIN{printf \"%.2f\", $base_cost + ($j-1)*0.01}")
    out=$(awk "BEGIN{printf \"%.2f\", $cost*2}")
    cache=$(awk "BEGIN{printf \"%.2f\", $cost*0.2}")
    api POST "/admin/channels/$chid/models" "{\"InternalModelID\":\"$ename\",\"ExternalModelID\":\"${EXT_ID[$ename]}\",\"CostRates\":{\"Input\":$cost,\"Output\":$out,\"CacheRead\":$cache},\"RateLimit\":$RL,\"Reliability\":$REL}" >/dev/null
  done
done

echo "== 完成 =="
api GET /admin/channels | jq -r '.Data[] | "  #\(.ID) \(.Name) prio=\(.Priority) rpm=\(.RateLimit.RPM) maxConc=\(.RateLimit.MaxConcurrent)"'
