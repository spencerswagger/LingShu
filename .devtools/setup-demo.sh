#!/usr/bin/env bash
# 在平台中配置演示数据：4 个对外模型 × 5 个渠道（每个渠道都绑定全部 4 个内部模型）。
# 渠道 BaseURL 指向 mock 的不同 profile：normal / slow / ratelimited / dead。
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

# 对外模型：名称|三段售价
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

HP='{"Interval":"*/10 * * * * *","DrainIntervalSeconds":5,"TimeoutMS":5000,"FailThreshold":2,"RecoveryThreshold":1,"ProbeModel":""}'
REL='{"WindowSeconds":60,"MinSamples":3,"ErrorRatePct":10,"Rate429Pct":20,"P99LatencyMS":5000}'
RL='{"RPM":0,"TPM":0,"BurstMultiplier":1,"OnExceed":"REJECT","QueueSize":0,"QueueTimeoutMS":0,"MaxConcurrent":0}'

# 渠道：名称|BaseURL|优先级|密钥数
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
  chid=$(api POST /admin/channels "{\"Name\":\"$name\",\"Protocol\":\"openai-chat\",\"BaseURL\":\"$url\",\"Priority\":$prio,\"RateLimit\":$RL,\"HealthProbe\":$HP,\"Reliability\":$REL}" | jq -r '.Data.ID')
  echo "== 渠道 $name -> id=$chid =="

  for k in $(seq 1 "$nkeys"); do
    api POST "/admin/channels/$chid/keys" "{\"Name\":\"演示密钥-$name-$k\",\"Credential\":\"sk-demo-$chid-$k\"}" >/dev/null
  done

  # 每个渠道绑定全部 4 个内部模型（成本随渠道微调，便于观察候选成本差异）
  j=0
  for em in "${EXT_MODELS[@]}"; do
    j=$((j+1))
    IFS='|' read -r ename rates <<<"$em"
    p=$(awk "BEGIN{printf \"%.2f\", ${j} * 1.0 + $i*0.05}")
    api POST "/admin/channels/$chid/models" "{\"InternalModelID\":\"$ename\",\"ExternalModelID\":\"${EXT_ID[$ename]}\",\"CostRates\":{\"Input\":$p,\"Output\":2.0,\"CacheRead\":0.2},\"RateLimit\":$RL,\"Reliability\":$REL}" >/dev/null
  done
done

echo "== 完成 =="
api GET /admin/channels | jq -r '.Data[] | "  #\(.ID) \(.Name) \(.BaseURL) prio=\(.Priority)"'
