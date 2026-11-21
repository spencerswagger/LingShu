#!/bin/bash
# /v1 白名单 + 剥头 同窗自检（定案③，方案 docs/designs/2026-11-20-nginx-whitelist-and-header-strip-window.md §7/§8）。
#
# 把仓库里真实的 nginx.conf + v1-proxy.inc 起在本地端口，用忠实复刻 magpie 行为的 stub 跑一遍：
#   1. 白名单 6 类点位放行、白名单外抽样全 404（§8-1/3 的 HTTP 面）；
#   2. 剥头断言：全部点位响应头大小写不敏感「零 magpie 字样」、Server 头无版本（§8-5/6）；
#   3. Retry-After 镜像行为（§8-10）：当且仅当上游发了 X-Magpie-Limit-Reset；
#   4. Codex WS：/v1/responses 升级头透传（101，§8-1 WS 面）；
#   5. 请求头零改动：Authorization / UA / anthropic-beta / X-Magpie-Session / ?key= 原样到达上游；
#   6. SSE 流式不被剥头/缓冲破坏（§8-7 的载体）；
#   7. 变异自检：上游新增未知 X-Magpie-* 头时，「零 magpie」断言必须报警（防线有效性证明，方案 §3 三步闭环）。
#
# §8 里依赖真机/数据库的条目（四 SDK 初始化、Codex CLI 实连、ledger_entries 计数、healthz 外网可达性）
# 不在本脚本范围——真机清单见 backend/scripts/acceptance/nginx-window/run.sh。
#
# 用法：bash frontend/scripts/v1-whitelist-selftest.sh
# 依赖：nginx、python3、curl。
set -u

WEB_PORT=${WEB_PORT:-18790}
MG_PORT=${MG_PORT:-13425}
BE_PORT=${BE_PORT:-18791}
HERE=$(cd "$(dirname "$0")" && pwd)
CONF=$HERE/../nginx.conf
INC=$HERE/../v1-proxy.inc
TMP=$(mktemp -d)
trap 'nginx -c "$TMP/nginx.conf" -s stop 2>/dev/null; kill ${P1:-} ${P2:-} 2>/dev/null; rm -rf "$TMP"' EXIT

fail=0
ck() { if [ "$3" = "$2" ]; then printf '  ok   %s\n' "$1"
       else printf '  FAIL %s（期望 %s，实际 %s）\n' "$1" "$2" "$3"; fail=1; fi; }
# 零 magpie 断言（大小写不敏感，头名+头值全量）：命中即 FAIL（§8-5）
ck_nomagpie() {
    if grep -qi 'magpie' "$TMP/hdr"; then
        printf '  FAIL %s：响应头残留 magpie 字样：\n' "$1"
        grep -i 'magpie' "$TMP/hdr" | sed 's/^/       /'; fail=1
    else
        printf '  ok   %s（响应头零 magpie）\n' "$1"
    fi
}
code() { curl -s -o "$TMP/body" -D "$TMP/hdr.raw" -w '%{http_code}' "$@"; tr -d '\r' <"$TMP/hdr.raw" >"$TMP/hdr"; }

# ---- magpie 网关 stub：3425 的行为面（响应头指纹 / SSE / WS / 请求头回显） ----
cat >"$TMP/mg_stub.py" <<'PY'
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

PORT = int(sys.argv[1])

class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass

    def send(self, code, body=b"", ctype="application/json", extra=()):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        for k, v in extra: self.send_header(k, v)
        self.end_headers()
        if body: self.wfile.write(body)

    def handle_one(self):
        u = urlparse(self.path)
        q = u.query
        # 指纹头全套（含小写变体，验证 hide 的大小写不敏感）：
        fp = [
            ("Server", "magpie/0.1.4454"), ("X-Powered-By", "magpie"),
            ("X-Magpie-Left-Out", "0"), ("X-Magpie-Route", "direct"),
            ("X-Magpie-Request-Id", "req-1"), ("X-Magpie-Cost", "0.001"),
            ("X-Magpie-Provider", "openai"), ("X-Magpie-Model", "gpt-x"),
            ("X-Magpie-Upstream", "up-1"), ("x-magpie-session", "sess-echo"),
        ]
        if "limit_reset" in q:
            fp.append(("X-Magpie-Limit-Reset", "42"))
        if "audit_new" in q:   # 变异位：模拟 magpie 升版新增未知指纹头
            fp.append(("X-Magpie-Future-Header", "leak"))
        extra = [("x-ratelimit-limit-requests", "100"), ("x-request-id", "up-rid-1")]
        if "retry_after" in q:  # 上游自身 Retry-After 必须保留（名单外不剥）
            extra.append(("Retry-After", "7"))

        # WS：回显收到的 Upgrade/Connection（验证 proxy_set_header 透传），101 + 关连接。
        # 真机按 Upgrade 头判定（不按 query 参数）；本地探针两种都认。
        body_txt = ""
        if int(self.headers.get("Content-Length") or 0) > 0:
            body_txt = self.rfile.read(int(self.headers["Content-Length"])).decode("utf-8", "replace")
        if "upgrade" in q or "websocket" in (self.headers.get("Upgrade") or "").lower():
            saw = [("X-Stub-Saw-Upgrade", self.headers.get("Upgrade") or ""),
                   ("X-Stub-Saw-Connection", self.headers.get("Connection") or "")]
            self.send_response(101)
            self.send_header("Upgrade", "websocket")
            for k, v in saw: self.send_header(k, v)
            self.end_headers()
            self.close_connection = True
            return

        # SSE：三 chunk 流式（验证 buffering off 不截断）；按请求体 stream:true 或 ?sse 触发
        if "sse" in q or '"stream":true' in body_txt.replace(" ", ""):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-store")
            for k, v in fp: self.send_header(k, v)
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            for part in (b'data: {"delta":"a"}\n\n', b'data: {"delta":"b"}\n\n', b"data: [DONE]\n\n"):
                self.wfile.write(("%x\r\n" % len(part)).encode() + part + b"\r\n")
                self.wfile.flush(); time.sleep(0.05)
            self.wfile.write(b"0\r\n\r\n")
            self.close_connection = True
            return

        # 其余：200 + 请求回显（验证请求头零改动；带 saw 头验证非升级请求的 Connection 取值）
        echo = {
            "path": u.path, "query": q,
            "authorization": self.headers.get("Authorization") or "",
            "ua": self.headers.get("User-Agent") or "",
            "anthropic_beta": self.headers.get("anthropic-beta") or "",
            "x_magpie_session": self.headers.get("X-Magpie-Session") or "",
            "x_goog_api_key": self.headers.get("x-goog-api-key") or "",
            "host": self.headers.get("Host") or "",
        }
        saw = [("X-Stub-Saw-Upgrade", self.headers.get("Upgrade") or ""),
               ("X-Stub-Saw-Connection", self.headers.get("Connection") or "")]
        self.send(200, json.dumps(echo).encode(), extra=fp + extra + saw)

    do_GET = do_POST = handle_one

ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
PY

# ---- 灵枢后端 stub：/healthz + /internal/magpie-access/verify（/magpie/ 段依赖） ----
cat >"$TMP/be_stub.py" <<'PY'
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse
class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def do_GET(self):
        u = urlparse(self.path)
        if u.path == "/internal/magpie-access/verify":
            t = ""
            for c in (self.headers.get("Cookie") or "").split(";"):
                c = c.strip()
                if c.startswith("magpie_gate="):
                    t = c[len("magpie_gate="):]
            code = 204 if t == "good" else 403
            self.send_response(code)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        body = b'{"status":"ok"}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers(); self.wfile.write(body)
ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY

# ---- 把真实配置搬到本地端口（每处替换必须命中，否则配置改形了）----
mkdir -p "$TMP/conf.d" "$TMP/tmp"
sed -e "s#listen 80;#listen $WEB_PORT;#" \
    -e "s#set \$backend_upstream backend:8080;#set \$backend_upstream 127.0.0.1:$BE_PORT;#" \
    -e "s#set \$magpie_gw magpie:3425;#set \$magpie_gw 127.0.0.1:$MG_PORT;#" \
    -e "s#set \$magpie_page magpie:3430;#set \$magpie_page 127.0.0.1:1;#" \
    -e "s#include /etc/nginx/conf.d/v1-proxy.inc#include $TMP/conf.d/v1-proxy.inc#g" \
    "$CONF" >"$TMP/conf.d/default.conf"
for pat in "listen $WEB_PORT;" "127.0.0.1:$MG_PORT;" "$TMP/conf.d/v1-proxy.inc"; do
    grep -q -- "$pat" "$TMP/conf.d/default.conf" || { echo "配置替换失败（找不到 $pat）：nginx.conf 结构变了？"; exit 1; }
done
cp "$INC" "$TMP/conf.d/v1-proxy.inc"

# 00-magpie-key.conf：/magpie/ 段引用 $magpie_entry_qs，缺它 nginx -t 过不了（解析期未知变量）。
# 本窗不改 /magpie/，这里用默认「不注入」模板，仅为配置可编译。
# 模板不含 nginx 指令本身（只含 map + 注释），即便文件缺失也只影响 /magpie/ 注入行为；
# 为防结构漂移，缺失时生成等价空 map 保住 /magpie/ 段的可编译性。
if [ -f "$HERE/00-magpie-key.conf" ]; then
    cp "$HERE/00-magpie-key.conf" "$TMP/conf.d/00-magpie-key.conf"
else
    printf 'map "$cookie_magpie_web_3430|$arg_k" $magpie_entry_qs { "|" "?k="; default ""; }\n' \
        >"$TMP/conf.d/00-magpie-key.conf"
fi

cat >"$TMP/nginx.conf" <<EOF
worker_processes 1;
error_log $TMP/error.log info;
pid $TMP/nginx.pid;
events { worker_connections 64; }
http {
    access_log $TMP/access.log;
    client_body_temp_path $TMP/tmp;
    proxy_temp_path $TMP/tmp;
    fastcgi_temp_path $TMP/tmp;
    uwsgi_temp_path $TMP/tmp;
    scgi_temp_path $TMP/tmp;
    default_type application/octet-stream;
    include $TMP/conf.d/00-magpie-key.conf;
    include $TMP/conf.d/default.conf;
}
EOF

nginx -t -c "$TMP/nginx.conf" 2>&1 | grep -q 'syntax is ok' || { echo "nginx -t 不过："; nginx -t -c "$TMP/nginx.conf"; exit 1; }
echo "== nginx -t 通过（map/include/location 形状合法）"

python3 "$TMP/mg_stub.py" $MG_PORT >"$TMP/mg.log" 2>&1 & P1=$!
python3 "$TMP/be_stub.py" $BE_PORT >"$TMP/be.log" 2>&1 & P2=$!
sleep 0.8
nginx -c "$TMP/nginx.conf" || { echo "nginx 起不来："; cat "$TMP/error.log"; exit 1; }
sleep 0.4
B=http://127.0.0.1:$WEB_PORT

echo "== 1) 白名单 6 类放行（到达上游 stub）"
ck "chat/completions"        200 "$(code "$B/v1/chat/completions")"
ck "responses"               200 "$(code "$B/v1/responses")"
ck "messages"                200 "$(code "$B/v1/messages")"
ck "messages/count_tokens"   200 "$(code "$B/v1/messages/count_tokens")"
ck "gemini generateContent"  200 "$(code "$B/v1beta/models/gem-x:generateContent")"
ck "gemini stream(变体/v1)"  200 "$(code "$B/v1/models/gem-x:streamGenerateContent?alt=sse")"
ck "gemini countTokens"      200 "$(code "$B/v1beta/models/gem-x:countTokens")"
code "$B/v1/models" >/dev/null
ck "/v1/models"              200 "$(code "$B/v1/models")"
grep -q '"path": *"/v1/models"' "$TMP/body" || { echo "  FAIL /v1/models 未到达上游"; fail=1; }
ck_nomagpie "放行点位响应头（chat）"
code "$B/v1beta/models/gem-x:countTokens" >/dev/null
ck_nomagpie "放行点位响应头（gemini countTokens）"

echo "== 2) 白名单外抽样 404（§7-3 全清单 + 边界）"
for p in /v1/magpie/usage /v1/magpie/usage/requests /v1/magpie/admin/keys /v1/magpie/quotas \
         /v1/embeddings /v1/completions /v1/rerank /v1/audio/transcriptions \
         /v1/responses/x /v1/chat/completions/x /v1/models/x /v1/messages/count_tokens/x \
         /v1beta/models/gem-x:embedContent /v1beta/models/gem-x; do
    c=$(code "$B$p")
    if [ "$c" = "404" ]; then printf '  ok   404 %s\n' "$p"
    else printf '  FAIL %s 期望 404 实际 %s\n' "$p" "$c"; fail=1; fi
done
grep -qi magpie "$TMP/hdr" && { echo "  FAIL 404 响应头含 magpie"; fail=1; } || printf '  ok   404 响应头零 magpie\n'
grep -q 'nginx' "$TMP/body" && printf '  ok   404 错误体为 nginx 自产（无内部信息）\n' || { echo "  FAIL 404 错误体非 nginx 自产"; fail=1; }
ck "Server 头无版本" "Server: nginx" "$(grep -i '^server:' "$TMP/hdr" | tr -d '\r' | sed 's/[[:space:]]*$//')"

# 非 /v1 前缀与裸前缀维持现状（终表 #22）：/v2/*、裸 /v1、裸 /v1beta 落静态资源兜底
# 返 200 HTML，不打到 magpie（location 前缀按带斜杠匹配，裸前缀不在白名单体系内）。
for p in /v2/chat/completions /v1 /v1beta; do
    c=$(code "$B$p")
    if [ "$c" = "200" ] && ! grep -q '"path"' "$TMP/body"; then printf '  ok   %s 维持现状（SPA 兜底，不到上游）\n' "$p"
    else printf '  FAIL %s 行为漂移：code=%s body=%s\n' "$p" "$c" "$(head -c 60 "$TMP/body")"; fail=1; fi
done

echo "== 3) 剥头保留面：名单外头 + 上游 Retry-After 保留"
code "$B/v1/chat/completions?retry_after=1" >/dev/null
ck "上游 Retry-After 保留" "Retry-After: 7" "$(grep -i '^retry-after:' "$TMP/hdr" | tr -d '\r' | head -1)"
ck "x-ratelimit 保留" "x-ratelimit-limit-requests: 100" "$(grep -i '^x-ratelimit-limit-requests:' "$TMP/hdr" | tr -d '\r' | head -1)"
ck "x-request-id 保留" "x-request-id: up-rid-1" "$(grep -i '^x-request-id:' "$TMP/hdr" | tr -d '\r' | head -1)"

echo "== 4) Retry-After 镜像（§8-10：当且仅当上游发限位头）"
code "$B/v1/chat/completions?limit_reset=1" >/dev/null
ck "上游发限位 → 镜像 42" "Retry-After: 42" "$(grep -i '^retry-after:' "$TMP/hdr" | tr -d '\r' | head -1)"
code "$B/v1/chat/completions" >/dev/null
if grep -qi '^retry-after:' "$TMP/hdr"; then printf '  FAIL 上游未发限位头却出现 Retry-After\n'; fail=1
else printf '  ok   上游未发限位 → 无 Retry-After\n'; fi
# 大小写不敏感剥离复验（stub 用小写 x-magpie-session）
grep -qi 'magpie' "$TMP/hdr" && { echo "  FAIL 小写指纹头未被剥"; fail=1; } || printf '  ok   小写指纹头同样被剥\n'

echo "== 5) Codex WS：/v1/responses 升级头透传"
wsup=$(curl -s -o "$TMP/body" -D "$TMP/hdr.raw" -w '%{http_code}' \
    -H 'Upgrade: websocket' -H 'Connection: Upgrade' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
    -H 'Sec-WebSocket-Version: 13' "$B/v1/responses?upgrade=1")
tr -d '\r' <"$TMP/hdr.raw" >"$TMP/hdr"
ck "WS 握手 101" 101 "$wsup"
ck "上游收到 Upgrade" "X-Stub-Saw-Upgrade: websocket" "$(grep -i '^x-stub-saw-upgrade:' "$TMP/hdr" | tr -d '\r')"
ck "上游收到 Connection: upgrade" "X-Stub-Saw-Connection: upgrade" "$(grep -i '^x-stub-saw-connection:' "$TMP/hdr" | tr -d '\r')"
code "$B/v1/responses" >/dev/null
ck "非升级请求 Connection=close" "X-Stub-Saw-Connection: close" "$(grep -i '^x-stub-saw-connection:' "$TMP/hdr" | tr -d '\r')"
ck_nomagpie "WS 响应头"

echo "== 6) 请求头零改动（回显断言）"
code >/dev/null -X POST -H 'Authorization: Bearer sk-gw-test' -H 'User-Agent: codex-cli/1.0' \
     -H 'anthropic-beta: interleaved-thinking' -H 'X-Magpie-Session: sess-1' \
     -H 'x-goog-api-key: gkey' "$B/v1/chat/completions?key=plain-key"
python3 - "$TMP/body" <<'PY' || fail=1
import json, sys
d = json.load(open(sys.argv[1]))
checks = {
    "authorization 原样": d["authorization"] == "Bearer sk-gw-test",
    "User-Agent 原样": d["ua"] == "codex-cli/1.0",
    "anthropic-beta 原样": d["anthropic_beta"] == "interleaved-thinking",
    "X-Magpie-Session 请求方向透传": d["x_magpie_session"] == "sess-1",
    "x-goog-api-key 透传": d["x_goog_api_key"] == "gkey",
    "?key= 不改写（应用层职责）": "key=plain-key" in d["query"],
}
bad = [k for k, v in checks.items() if not v]
for k, v in checks.items(): print(("  ok   " if v else "  FAIL ") + k)
sys.exit(1 if bad else 0)
PY

echo "== 7) SSE 流式完整（剥头不影响流）"
code >/dev/null --no-buffer "$B/v1/chat/completions?sse=1"
if grep -q 'data: \[DONE\]' "$TMP/body" && grep -q '"delta":"a"' "$TMP/body"; then
    printf '  ok   SSE 三 chunk 完整到达\n'
else
    printf '  FAIL SSE 不完整：\n'; sed 's/^/       /' "$TMP/body"; fail=1
fi
ck_nomagpie "SSE 响应头"
ck "SSE Content-Type 透传" "Content-Type: text/event-stream" "$(grep -i '^content-type:' "$TMP/hdr" | tr -d '\r' | head -1)"

echo "== 8) healthz（内网面，本窗不改）"
ck "内网 healthz 200" 200 "$(code "$B/healthz")"

echo "== 9) 变异自检：未知 X-Magpie-* 头必须被零 magpie 断言抓住（防线有效性）"
code "$B/v1/chat/completions?audit_new=1" >/dev/null
if grep -qi 'magpie' "$TMP/hdr"; then
    printf '  ok   新增指纹头被断言抓到（X-Magpie-Future-Header）→ 窗内补枚举流程生效\n'
else
    printf '  FAIL 防线失效：新增 magpie 头未被抓到\n'; fail=1
fi

echo "== 10) /magpie/ 管理面不受影响（鉴权仍在）"
ck "/magpie/ 无票据 403" 403 "$(code "$B/magpie/")"

echo
if [ "$fail" = "0" ]; then echo "SELFTEST PASS"; else echo "SELFTEST FAIL"; exit 1; fi
