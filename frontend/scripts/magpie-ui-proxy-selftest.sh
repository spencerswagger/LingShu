#!/bin/bash
# /magpie/ 子路径反代的自检：把**仓库里真实的 nginx.conf** 起在本地端口上，用忠实复刻
# magpie 管理面行为的 stub 跑一遍关键路径。目的是防两类回归：
#
#   1. 有人把 auth_request 去掉（非管理员也能打开代理侧管理面 = 看得到上游明文 key）；
#   2. 有人删掉 /api/ 改写（页面打开一片 401），或把 Host 改成 magpie:3430（页面所有写请求 403）。
#
# 用法：bash frontend/scripts/magpie-ui-proxy-selftest.sh
# 依赖：nginx、python3、curl；需要用容器内固定端口 3430（magpie 页面端口，cookie 名依赖它）。
set -u

WEB_PORT=${WEB_PORT:-18780}
BE_PORT=${BE_PORT:-18781}
MG_PORT=3430
HERE=$(cd "$(dirname "$0")" && pwd)
CONF=$HERE/../nginx.conf
TMP=$(mktemp -d)
trap 'nginx -c "$TMP/nginx.conf" -s stop 2>/dev/null; kill ${P1:-} ${P2:-} 2>/dev/null; rm -rf "$TMP"' EXIT

KEY=selftest-page-key
fail=0
ck() { # ck <名字> <期望> <实际>
    if [ "$3" = "$2" ]; then printf '  ok   %s\n' "$1"
    else printf '  FAIL %s（期望 %s，实际 %s）\n' "$1" "$2" "$3"; fail=1; fi
}
# ck3 <名字> <实际>：只要求是 3xx 重定向。真机 magpie 换完 cookie 返回的是 **303**（文档里写的
# 302 是旧版行为），所以这里不断言具体码，只断言「是重定向」，并把实测码打印出来。
ck3() {
    case "$2" in
        3??) printf '  ok   %s（实测 %s）\n' "$1" "$2" ;;
        *)   printf '  FAIL %s（期望 3xx，实际 %s）\n' "$1" "$2"; fail=1 ;;
    esac
}
# curl -D 写出的是 CRLF 头，先转成 LF 再比（否则 /magpie/$ 这种锚点永远不匹配）
code() { curl -s -o "$TMP/body" -D "$TMP/hdr.raw" -w '%{http_code}' "$@"; tr -d '\r' <"$TMP/hdr.raw" >"$TMP/hdr"; }

# ---- magpie 管理面 stub：行为照抄 internal/gui/web.go ----
cat >"$TMP/magpie_stub.py" <<'PY'
import json, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

PORT, KEY = int(sys.argv[1]), sys.argv[2]
COOKIE = f"magpie_web_{PORT}"
INDEX = '<!doctype html><link rel=stylesheet href="app.css"><script src="boot.js"></script><body>magpie page</body>'
APPJS = 'async function api(p){const r=await fetch("/api/"+p);return r.json()}\nconst ICON="/api/icons/";'
FORM = '<form method="post" action="/_magpie/sign-in"><input name="key"></form>'


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def send(self, code, body=b"", ctype="text/plain; charset=utf-8", extra=()):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        for k, v in extra:
            self.send_header(k, v)
        self.end_headers()
        if body:
            self.wfile.write(body)

    def handle_one(self):
        u = urlparse(self.path)
        path, q = u.path, parse_qs(u.query)
        cookies = [c.strip() for c in (self.headers.get("Cookie") or "").split(";") if c.strip()]
        if "k" in q:
            if q["k"][0] != KEY:
                return self.send(401, b"wrong key")
            # 真机 magpie 回的是 303（See Other），不是 302 —— stub 必须照抄，否则自检覆盖不到 303
            return self.send(303, extra=[("Location", "/"), ("Set-Cookie", f"{COOKIE}={KEY}; Path=/; HttpOnly")])
        if path in ("/manifest.json", "/icons/icon-512.png"):
            return self.send(200, b"{}", "application/json")
        if f"{COOKIE}={KEY}" not in cookies:
            return self.send(401, FORM.encode(), "text/html; charset=utf-8")
        if path.startswith("/api/"):
            if self.command not in ("GET", "HEAD"):
                origin, host = self.headers.get("Origin"), self.headers.get("Host", "")
                if origin and urlparse(origin).netloc != host:
                    return self.send(403, f"origin {origin} != host {host}".encode())
            return self.send(200, json.dumps({"path": path, "host": self.headers.get("Host")}).encode(),
                             "application/json")
        if path in ("/", "/index.html"):
            return self.send(200, INDEX.encode(), "text/html; charset=utf-8")
        if path == "/app.js":
            return self.send(200, APPJS.encode(), "application/javascript; charset=utf-8")
        return self.send(404, b"stub 404")

    do_GET = do_POST = do_HEAD = handle_one


ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
PY

# ---- 灵枢后端 stub：只实现 /internal/magpie-access/verify 的判定口径 ----
cat >"$TMP/backend_stub.py" <<'PY'
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_GET(self):
        u = urlparse(self.path)
        if u.path == "/internal/magpie-access/verify":
            t = (parse_qs(u.query).get("t") or [""])[0]
            if not t:
                for c in (self.headers.get("Cookie") or "").split(";"):
                    c = c.strip()
                    if c.startswith("magpie_gate="):
                        t = c[len("magpie_gate="):]
            code = 204 if t == "good" else 403
        elif u.path.startswith("/api/"):
            code = 401
        else:
            code = 404
        self.send_response(code)
        self.send_header("Content-Length", "0")
        self.end_headers()


ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY

mkdir -p "$TMP/conf.d" "$TMP/tmp" "$TMP/html"
echo '<!doctype html><title>lingshu</title>' >"$TMP/html/index.html"

# ---- 把仓库里的真实配置搬到本地端口上（每处替换都必须命中，否则说明配置改形了）----
mkdir -p "$TMP/conf.d"
sed -e "s#listen 80;#listen $WEB_PORT;#" \
    -e "s#set \$backend_upstream backend:8080;#set \$backend_upstream 127.0.0.1:$BE_PORT;#" \
    -e "s#set \$magpie_page magpie:3430;#set \$magpie_page 127.0.0.1:$MG_PORT;#" \
    -e "s#root /usr/share/nginx/html;#root $TMP/html;#" \
    -e "s#resolver 127.0.0.11 #resolver 127.0.0.11 #" \
    "$CONF" >"$TMP/conf.d/default.conf"
for pat in "listen $WEB_PORT;" "127.0.0.1:$BE_PORT;" "127.0.0.1:$MG_PORT;"; do
    grep -q -- "$pat" "$TMP/conf.d/default.conf" || { echo "配置替换失败（找不到 $pat）：nginx.conf 结构变了？"; exit 1; }
done

# 用真实的 19-magpie-key.sh 生成注入规则（不是测试里另写一份）
# MAGPIE_WEB_KEY_FILE 指到不存在的路径 → 走「环境变量」分支（脚本口径是卷优先，宿主机上可能真有那个文件）
MAGPIE_WEB_KEY=$KEY MAGPIE_WEB_KEY_FILE=$TMP/no-such-key \
    MAGPIE_KEY_CONF_OUT=$TMP/conf.d/00-magpie-key.conf \
    sh "$HERE/../docker-entrypoint.d/19-magpie-key.sh"

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

python3 "$TMP/magpie_stub.py" $MG_PORT "$KEY" >"$TMP/mg.log" 2>&1 &
P1=$!
python3 "$TMP/backend_stub.py" $BE_PORT >"$TMP/be.log" 2>&1 &
P2=$!
sleep 0.8
nginx -c "$TMP/nginx.conf" || { echo "nginx 起不来："; cat "$TMP/error.log"; exit 1; }
sleep 0.4
B=http://127.0.0.1:$WEB_PORT
GATE="magpie_gate=good"
MC="magpie_web_${MG_PORT}=$KEY"

echo "== 鉴权"
ck "无票据 → 403" 403 "$(code "$B/magpie/")"
grep -qi 'magpie page' "$TMP/body" && { echo "  FAIL 未鉴权却拿到页面"; fail=1; }
ck "伪票据 → 403" 403 "$(code -H 'Cookie: magpie_gate=bogus' "$B/magpie/")"
ck "伪票据 + API → 403" 403 "$(code -H 'Cookie: magpie_gate=bogus' "$B/magpie/api/x")"

echo "== 票换口令（服务端注入）"
REDIR=$(code -H "Cookie: $GATE" "$B/magpie/")
ck3 "票据 + 无页面 cookie → 3xx（真机 magpie 实测 303）" "$REDIR"
# 303 的 Location 必须同样被 proxy_redirect 改写（302 时代已验证，303 是新增断言）
if grep -qi "^location: /magpie/$" "$TMP/hdr"; then
    printf '  ok   3xx 的 Location 已改写为 /magpie/\n'
else
    echo "  FAIL 3xx 的 Location 没改回 /magpie/，实际：$(grep -i '^location:' "$TMP/hdr" | tr -d '\r')"; fail=1
fi
ck "票据 + 页面 cookie → 200" 200 "$(code -H "Cookie: $GATE; $MC" "$B/magpie/")"
grep -qi 'magpie page' "$TMP/body" || { echo "  FAIL 没拿到页面"; fail=1; }
ck "/magpie（无斜杠）→ 301" 301 "$(code -H "Cookie: $GATE" "$B/magpie")"

echo "== 子路径改写"
ck "页面 JS → 200" 200 "$(code -H "Cookie: $GATE; $MC" "$B/magpie/app.js")"
grep -q '"/magpie/api/"' "$TMP/body" || { echo "  FAIL JS 里的 /api/ 没被改写"; fail=1; }
grep -q '"/api/"' "$TMP/body" && { echo "  FAIL JS 里残留裸 /api/"; fail=1; }
ck "页面 API → 200" 200 "$(code -H "Cookie: $GATE; $MC" "$B/magpie/api/state")"
grep -q '"path": "/api/state"' "$TMP/body" || { echo "  FAIL 前缀没剥干净"; fail=1; }
grep -q "\"host\": \"127.0.0.1:$WEB_PORT\"" "$TMP/body" || { echo "  FAIL Host 没原样转发（magpie 的跨站防护会挡掉写请求）"; fail=1; }
ck "同源 POST → 200" 200 "$(code -X POST -H "Cookie: $GATE; $MC" -H "Origin: $B" "$B/magpie/api/write")"
ck "外站 POST → 403" 403 "$(code -X POST -H "Cookie: $GATE; $MC" -H "Origin: http://evil.example" "$B/magpie/api/write")"

echo "== 灵枢自己的前缀不受影响"
ck "/api/ 仍归后端 → 401" 401 "$(code "$B/api/v1/auth/me")"

echo "== 口令不落前端"
ck "响应头里没有明文口令的 Set-Cookie（只有 magpie 自己的）" 0 "$(grep -ci "set-cookie: .*magpie_gate" "$TMP/hdr")"

echo "== 口令长度守卫（真机 magpie 要求 MAGPIE_WEB_KEY ≥16 字符，短了直接起不来）"
SHORT_LOG=$(MAGPIE_WEB_KEY=short12345678 MAGPIE_WEB_KEY_FILE=$TMP/no-such-key \
    MAGPIE_KEY_CONF_OUT=$TMP/short.conf \
    sh "$HERE/../docker-entrypoint.d/19-magpie-key.sh" 2>&1 || true)
case "$SHORT_LOG" in
    *"要求 ≥16"*) printf '  ok   13 位口令 → 告警且不注入\n' ;;
    *) printf '  FAIL 13 位口令没有告警，实际输出：%s\n' "$SHORT_LOG"; fail=1 ;;
esac
if [ -f "$TMP/short.conf" ] && grep -q '?k=' "$TMP/short.conf"; then
    echo "  FAIL 不足 16 位的口令仍被注入进 map"; fail=1
fi

echo
if [ "$fail" -ne 0 ]; then echo "自检失败"; exit 1; fi
echo "自检通过"
