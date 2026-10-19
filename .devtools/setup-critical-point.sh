#!/usr/bin/env bash
# 为「临界点」场景配置定价：
#   正常A：原始价高但无分档 → 小上下文最便宜（会话起点会选它）
#   正常B：原始价低但小上下文分档系数高（5.0）→ 上下文 >6000 后最便宜（中途切换目标）
# 其余渠道更贵，不参与竞争。
set -euo pipefail
BASE="http://127.0.0.1:8090/api/v1"
T=$(curl -s -X POST "$BASE/auth/login" -H 'Content-Type: application/json' \
  -d '{"Username":"admin","Password":"admin123"}' | jq -r .Data.Token)
AUTH=(-H "Authorization: Bearer $T" -H 'Content-Type: application/json')
api() { local m="$1" p="$2" d="${3:-}"; if [ -n "$d" ]; then curl -s -X "$m" "$BASE$p" "${AUTH[@]}" -d "$d"; else curl -s -X "$m" "$BASE$p" "${AUTH[@]}"; fi; }

EXT_NAME="demo-chat-pro"

# 渠道名 → 目标成本与分档
# A：原始价 2/4/0.2，无分档
# B：原始价 1/2/0.1，小上下文分档 ×5（>6000 回落 ×1）
set_cost() {
  local chname="$1" cost="$2" tiers="$3"
  local chid mid cur
  chid=$(api GET /admin/channels | jq -r --arg n "$chname" '.Data[]|select(.Name==$n)|.ID')
  [ -n "$chid" ] && [ "$chid" != "null" ] || { echo "渠道不存在: $chname"; return 1; }
  mid=$(api GET "/admin/channels/$chid/models" | jq -r --arg e "$EXT_NAME" '.Data[]|select(.ExternalModelID!=null)|select(.InternalModelID==$e)|.ID' | head -n1)
  [ -n "$mid" ] && [ "$mid" != "null" ] || { echo "内部模型不存在: $chname/$EXT_NAME"; return 1; }
  cur=$(api GET "/admin/channels/$chid/models" | jq -c --arg id "$mid" '.Data[]|select(.ID==$id)')
  # 以现有行做基底，仅覆盖 CostRates / ContextTiers
  local body
  body=$(echo "$cur" | jq -c --argjson c "$cost" --argjson t "$tiers" '{
      InternalModelID, ExternalModelID,
      CostRates: $c,
      ContextTiers: (if ($t|length)>0 then $t else null end),
      TimeConfig: (.TimeConfig // null),
      RateLimit: .RateLimit,
      Reliability: .Reliability
    }')
  api PUT "/admin/channels/$chid/models/$mid" "$body" >/dev/null
  echo "  已设置 $chname / $EXT_NAME  cost=$cost tiers=$tiers"
}

echo "== 设置定价 =="
set_cost "演示渠道-正常A" '{"input":2.0,"cache_read":0.2,"output":4.0}' '[]'
set_cost "演示渠道-正常B" '{"input":1.0,"cache_read":0.1,"output":2.0}' '[{"Min":0,"Max":6000,"Coeff":5.0},{"Min":6000,"Max":null,"Coeff":1.0}]'
set_cost "演示渠道-慢速" '{"input":3.0,"cache_read":0.3,"output":6.0}' '[]'
set_cost "演示渠道-限流" '{"input":3.5,"cache_read":0.35,"output":7.0}' '[]'
set_cost "演示渠道-故障" '{"input":1.05,"cache_read":0.105,"output":2.1}' '[]'

echo "== 写 routing.session_cost（启用 + 中途切换）=="
PGPASSWORD=llmgw psql -h 127.0.0.1 -U llmgw -d llmgw -v ON_ERROR_STOP=1 -c \
"INSERT INTO sys_configs(key, value) VALUES ('routing.session_cost',
 '{\"enabled\":true,\"remaining_a\":12,\"remaining_b\":1,\"remaining_min\":1,\"remaining_max\":20,\"min_remaining\":1,\"cooldown_sec\":0,\"mid_switch\":true,\"min_improve\":0.1}'::jsonb)
 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;" >/dev/null
echo "  已写入并置 enabled=true / mid_switch=true / min_improve=0.1"