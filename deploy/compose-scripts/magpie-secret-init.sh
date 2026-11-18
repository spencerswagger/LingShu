#!/bin/bash
# 由 docker-compose.yml 以 `command: ["/bin/bash","/opt/lingshu/magpie-secret-init.sh"]` 调用。
# 逻辑与本文件同源；改这里即可，compose 不再内联 bash。
set -u
WEB=/secret/admin_web_key
mkdir -p /secret /config; chmod 700 /secret || true
gen() { if command -v openssl >/dev/null 2>&1; then openssl rand -hex 16
        else head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n'; fi; }
if [ -n "${MAGPIE_WEB_KEY:-}" ]; then printf '%s' "$MAGPIE_WEB_KEY" > "$WEB"
elif [ ! -s "$WEB" ]; then gen > "$WEB"; fi
cur=$(cat "$WEB"); if [ "${#cur}" -lt 16 ]; then printf '%s%s' "$cur" "$(gen)" > "$WEB"; fi
chown -R 65532:65532 /config 2>/dev/null || true
chown 65532:65532 "$WEB" 2>/dev/null || true; chmod 600 "$WEB" 2>/dev/null || true
echo "[magpie-secret-init] 页面口令就绪"
