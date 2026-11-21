#!/bin/bash
# nginx 白名单 + 剥头 · 变更窗冒烟（定案③ 2026-11-20）
# 方案：docs/designs/2026-11-20-nginx-whitelist-and-header-strip-window.md §7（验证步骤）/ §8（验收判据）。
#
# 用法（Gate 在变更窗 T0+1min 执行）：
#   B=https://<对外入口>  TOK='sk-gw-<测试令牌>'  MODEL=<chat 模型>  GEM_MODEL=<gemini 模型> \
#   [PSQL='docker compose exec -T postgres psql -U lingshu -d lingshu -tA'] \
#   [HEALTH_INTERNAL='docker compose exec -T backend wget -qO- http://127.0.0.1:8080/healthz'] \
#   sh backend/scripts/acceptance/nginx-window/run.sh
#
#   PSQL 给了才跑 §7-7/§8-2（count_tokens 无账本行）与 §7-8（计费链）；没给则这两条打 SKIP。
#   HEALTH_INTERNAL 给了才跑内网 healthz；没给只测外部可达性并打 AUDIT（供预案 B 裁定）。
#   产出 $OUT/headers-audit.txt = §8-9 要求的抓包核全归档输入（含未知头审计位）。
set -u

B=${B:?需要对外入口，如 B=https://gw.example.com}
TOK=${TOK:?需要测试令牌}
MODEL=${MODEL:-}
GEM_MODEL=${GEM_MODEL:-}
OUT=${OUT:-$(mktemp -d)}
mkdir -p "$OUT"
UA="nginx-window-smoke/1.0"
fail=0; skip=0
AUTH="Authorization: Bearer $TOK"
CT="Content-Type: application/json"

ck() { if [ "$3" = "$2" ]; then printf '  PASS %-58s %s\n' "$1" "$3"
       else printf '  FAIL %-58s 期望=%s 实际=%s\n' "$1" "$2" "$3"; fail=1; fi; }
# 请求 → 落盘（body/hdr/audit 三件）
hit() { # hit <名字> <路径> [curl 额外参数...] → 打印状态码
    local name="$1" path="$2"; shift 2
    curl -s -o "$OUT/$name.body" -D "$OUT/$name.hdr" -A "$UA" "$@" "$B$path"
    tr -d '\r' <"$OUT/$name.hdr" >"$OUT/$name.h2"
    { printf '\n== [%s] 响应头归档\n' "$name"; cat "$OUT/$name.h2"; } >>"$OUT/headers-audit.txt"
    set -- $(head -1 "$OUT/$name.h2"); echo "${2:-}"
}
# 零 magpie 断言（§8-5：头名+头值，大小写不敏感）
nomagpie() { if [ ! -s "$OUT/$1.h2" ]; then printf '  SKIP  %-57s（无响应头可判）\n' "$2"; return; fi
    if grep -qi magpie "$OUT/$1.h2"; then
        printf '  FAIL %-58s 残留：%s\n' "$2" "$(grep -i magpie "$OUT/$1.h2" | head -1)"; fail=1
    else printf '  PASS %-58s\n' "$2"; fi; }
# 未知头审计位（§8-9 归档 + F8 口径初筛；只报不判，供应商身份类头窗内人工定夺）
audit_headers() { python3 - "$OUT/$1.h2" "$1" <<'PY' || true
import re, sys
KEPT = re.compile(r'^(date|content-type|content-length|transfer-encoding|connection|'
                  r'server|x-request-id|retry-after|x-content-type-options|x-frame-options|'
                  r'referrer-policy|permissions-policy|cache-control|x-ratelimit[^:]*)$', re.I)
VENDOR = r'openai|anthropic|google|goog|gemini|claude|azure|bedrock|aws|deepseek|qwen|zhipu|glm|moonshot|kimi|mistral|groq|xai|perplexity|cohere'
for line in open(sys.argv[1]):
    if ':' not in line: continue
    name, val = line.split(':', 1)
    if KEPT.match(name.strip()): continue
    if re.search(VENDOR, name, re.I) or re.search(VENDOR, val.strip(), re.I):
        print('  AUDIT %s：疑似供应商身份类头 → F8 口径判定（剥/留）：%s: %s' % (sys.argv[2], name.strip(), val.strip()))
PY
}
lcount() { # lcount → 打印 ledger_entries 行数（PSQL 未配置时打 SKIP）
    if [ -z "${PSQL:-}" ]; then echo "SKIP"; return; fi
    $PSQL -c "SELECT count(*) FROM ledger_entries"
}

echo "== §7-1 / §8-1 放行 6 类点位"
[ -n "$MODEL" ] && { ck "chat/completions 2xx" 2 "$(hit chat /v1/chat/completions -XPOST -H "$AUTH" -H "$CT" -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}" | cut -c1)"; nomagpie chat "chat 响应头零 magpie"; audit_headers chat; } \
  || { echo "  SKIP chat/completions（未给 MODEL）"; skip=1; }
[ -n "$MODEL" ] && { ck "responses 2xx" 2 "$(hit resp /v1/responses -XPOST -H "$AUTH" -H "$CT" -d "{\"model\":\"$MODEL\",\"input\":\"ping\"}" | cut -c1)"; nomagpie resp "responses 响应头零 magpie"; audit_headers resp; } \
  || { echo "  SKIP responses（未给 MODEL）"; skip=1; }
[ -n "$MODEL" ] && { ck "messages 2xx" 2 "$(hit msg /v1/messages -XPOST -H "$AUTH" -H "$CT" -d "{\"model\":\"$MODEL\",\"max_tokens\":8,\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}" | cut -c1)"; nomagpie msg "messages 响应头零 magpie"; audit_headers msg; } \
  || { echo "  SKIP messages（未给 MODEL）"; skip=1; }
[ -n "$MODEL" ] && { ck "messages/count_tokens 2xx" 2 "$(hit ct /v1/messages/count_tokens -XPOST -H "$AUTH" -H "$CT" -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}" | cut -c1)"; nomagpie ct "count_tokens(A) 响应头零 magpie"; } \
  || { echo "  SKIP count_tokens A（未给 MODEL）"; skip=1; }
[ -n "$GEM_MODEL" ] && { ck "gemini generateContent 2xx" 2 "$(hit gemg "/v1beta/models/$GEM_MODEL:generateContent" -XPOST -H "$AUTH" -H "$CT" -d "{\"contents\":[{\"parts\":[{\"text\":\"ping\"}]}]}" | cut -c1)"; nomagpie gemg "gemini 响应头零 magpie"; audit_headers gemg; } \
  || { echo "  SKIP gemini（未给 GEM_MODEL）"; skip=1; }
[ -n "$GEM_MODEL" ] && ck "gemini countTokens 2xx" 2 "$(hit gemc "/v1beta/models/$GEM_MODEL:countTokens" -XPOST -H "$AUTH" -H "$CT" -d "{\"contents\":[{\"parts\":[{\"text\":\"ping\"}]}]}" | cut -c1)"
ck "/v1/models 2xx" 2 "$(hit models /v1/models -H "$AUTH" | cut -c1)"; nomagpie models "models 响应头零 magpie"

echo "== §7-2 / §8-1 Codex WS"
code=$(curl -s -o "$OUT/ws.body" -D "$OUT/ws.hdr" -A "$UA" -H "$AUTH" \
    -H 'Upgrade: websocket' -H 'Connection: Upgrade' -H 'Sec-WebSocket-Version: 13' \
    -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' "$B/v1/responses" -w '%{http_code}' --max-time 10)
tr -d '\r' <"$OUT/ws.hdr" >"$OUT/ws.h2"
ck "WS 握手 101（Codex CLI 实连为最终判据）" 101 "$code"
grep -qi 'magpie' "$OUT/ws.h2" && { printf '  FAIL WS 响应头残留 magpie\n'; fail=1; }

echo "== §7-3 / §8-3 白名单外 404 抽样"
for p in /v1/magpie/usage /v1/magpie/usage/requests /v1/magpie/admin/keys /v1/magpie/quotas \
         /v1/embeddings /v1/completions /v1/rerank /v1/audio/transcriptions \
         /v1/responses/x /v1/chat/completions/x /v1beta/models/x:embedContent; do
    c=$(hit "nf$(echo "$p" | tr '/:' '__')" "$p")
    ck "404 $p" 404 "$c"
done
grep -qi 'magpie' "$OUT/nf_v1_magpie_usage.h2" && { printf '  FAIL 404 错误体/头含 magpie\n'; fail=1; } || printf '  PASS 404 无内部信息\n'
ck "Server 头无版本（§8-6）" "Server: nginx" "$(grep -i '^server:' "$OUT/nf_v1_magpie_usage.h2" | sed 's/[[:space:]]*$//')"

echo "== §7-4 / §8-5/6 头断言全点位"
for f in chat resp msg ct gemg gemc models; do
    [ -s "$OUT/$f.h2" ] && nomagpie "$f" "$f 响应头零 magpie"
done
grep -i '^server:' "$OUT/models.h2" | grep -q 'nginx/' && { printf '  FAIL Server 带版本\n'; fail=1; } || printf '  PASS Server: nginx（无版本）\n'
if [ -s "$OUT/chat.h2" ] && grep -qiE '^(retry-after|x-ratelimit)' "$OUT/chat.h2"; then
    printf '  AUDIT chat 有限位头镜像/保留（§8-10，值格式人工核对）\n'; fi

echo "== §7-5 / §8-7 SSE 流式"
if [ -n "$MODEL" ]; then
    t0=$(date +%s%N 2>/dev/null || date +%s)
    curl -s --no-buffer -A "$UA" -H "$AUTH" -H "$CT" -XPOST \
        -d "{\"model\":\"$MODEL\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"数到3\"}]}" \
        "$B/v1/chat/completions" >"$OUT/sse.body" --max-time 60
    t1=$(date +%s%N 2>/dev/null || date +%s)
    if grep -q '\[DONE\]' "$OUT/sse.body" && grep -q 'data:' "$OUT/sse.body"; then
        printf '  PASS SSE 完整结束（首字节 %sms）\n' $(( (t1 - t0) / 1000000 ))
    else printf '  FAIL SSE 不完整：见 $OUT/sse.body\n'; fail=1; fi
else echo "  SKIP SSE（未给 MODEL）"; skip=1; fi

echo "== §7-6 / §8-4 healthz"
hc=$(curl -s -o /dev/null -w '%{http_code}' -A "$UA" "$B/healthz" --max-time 10)
case "$hc" in
    200) printf '  AUDIT 外网 /healthz 返回 200 → 可达，按方案 §5.3 启用预案 B（allow/deny）\n' ;;
    404|403) printf '  PASS 外网 /healthz %s（不可达）\n' "$hc" ;;
    *) printf '  AUDIT 外网 /healthz 实测 %s → 供预案 B 裁定\n' "$hc" ;;
esac
if [ -n "${HEALTH_INTERNAL:-}" ]; then
    $HEALTH_INTERNAL >/dev/null 2>&1 && printf '  PASS 内网 healthz 200\n' || { printf '  FAIL 内网 healthz\n'; fail=1; }
else echo "  SKIP 内网 healthz（未给 HEALTH_INTERNAL）"; skip=1; fi

echo "== §7-7 / §8-2 count_tokens 无账本行"
before=$(lcount)
if [ "$before" = "SKIP" ]; then echo "  SKIP ledger 计数（未给 PSQL）"; skip=1
else
    [ -n "$MODEL" ] && curl -s -o /dev/null -A "$UA" -H "$AUTH" -H "$CT" -XPOST \
        -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}" "$B/v1/messages/count_tokens"
    [ -n "$GEM_MODEL" ] && curl -s -o /dev/null -A "$UA" -H "$AUTH" -H "$CT" -XPOST \
        -d "{\"contents\":[{\"parts\":[{\"text\":\"ping\"}]}]}" "$B/v1beta/models/$GEM_MODEL:countTokens"
    after=$(lcount)
    ck "count_tokens×2 前后 ledger_entries 不变" "$before" "$after"
fi

echo "== §7-8 计费链（真推理 60s 内落账本）"
if [ -n "${PSQL:-}" ] && [ -n "$MODEL" ]; then
    before=$(lcount)
    curl -s -o /dev/null -A "$UA" -H "$AUTH" -H "$CT" -XPOST \
        -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"计费冒烟\"}]}" "$B/v1/chat/completions"
    echo "  等待 60s（poller 拉账本）……"; sleep 60
    after=$(lcount)
    if [ "$after" -gt "$before" ]; then printf '  PASS 账本落行（%s → %s）\n' "$before" "$after"
    else printf '  FAIL 账本未增（%s → %s），查 UPSTREAM_LEDGER_URL/poller 日志\n' "$before" "$after"; fail=1; fi
else echo "  SKIP 计费链（需 PSQL+MODEL）"; skip=1; fi

echo "== §8-8/9/10 人工位"
cat <<'MANUAL'
  MANUAL §8-8 四家官方 SDK 初始化各跑一次零报错（openai/anthropic/gemini SDK + Codex CLI 实连）
  MANUAL §8-9 本轮 headers-audit.txt（$OUT/headers-audit.txt）交归档：未知/供应商身份类头逐个判定后补 proxy_hide_header
  MANUAL §8-10 Retry-After 镜像值格式（429 时核对是否纯秒数；非秒数 → 删镜像行）
MANUAL

echo
printf '归档：%s/headers-audit.txt\n' "$OUT"
if [ "$fail" = "0" ]; then printf '冒烟结论：PASS（FAIL=0 SKIP=%s；AUDIT/MANUAL 项见上）\n' "$skip"
else printf '冒烟结论：FAIL —— 触发回滚判定（方案 §9）\n'; exit 1; fi
