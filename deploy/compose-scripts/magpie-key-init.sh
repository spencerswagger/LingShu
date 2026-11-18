#!/bin/bash
# 由 docker-compose.yml 以 `command: ["/bin/bash","/opt/lingshu/magpie-key-init.sh"]` 调用。
# 逻辑与本文件同源；改这里即可，compose 不再内联 bash。
set -u
NAME="${MAGPIE_ADMIN_KEY_NAME:-lingshu-backend}"
ADDR="${MAGPIE_ADDR:-http://magpie:3425}"
HOST=$(printf '%s' "$ADDR" | sed -E 's#^[a-z]+://##; s#/.*$##; s#:.*$##')
PORT=$(printf '%s' "$ADDR" | sed -nE 's#^[a-z]+://[^:/]+:([0-9]+).*#\1#p'); PORT=${PORT:-3425}
MAGPIE_BIN=$(command -v magpie || echo /magpie)
KEY_FILE=/secret/admin_key
WEB_KEY_FILE=/secret/admin_web_key
fix_owner() {
  chown -R 65532:65532 /config 2>/dev/null || true
  chown 65532:65532 "$KEY_FILE" "$WEB_KEY_FILE" 2>/dev/null || true
  chmod 600 "$KEY_FILE" "$WEB_KEY_FILE" 2>/dev/null || true
}
gen_hex() {
  if command -v openssl >/dev/null 2>&1; then openssl rand -hex 16
  else head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n'; fi
}
mkdir -p /secret /config; chmod 700 /secret || true

# 页面口令：手填优先，其次卷里，最后生成；<16 位补齐（magpie 硬要求）
if [ -n "${MAGPIE_WEB_KEY:-}" ]; then
  printf '%s' "$MAGPIE_WEB_KEY" > "$WEB_KEY_FILE"
elif [ ! -s "$WEB_KEY_FILE" ]; then
  gen_hex > "$WEB_KEY_FILE"
fi
cur=$(cat "$WEB_KEY_FILE")
if [ "${#cur}" -lt 16 ]; then printf '%s%s' "$cur" "$(gen_hex)" > "$WEB_KEY_FILE"; fi

# 期望的网关密钥：环境变量 > 卷里已有的 > 新生成（不依赖 magpie CLI 的格式）
if [ -n "${MAGPIE_ADMIN_KEY:-}" ]; then K="$MAGPIE_ADMIN_KEY"
elif [ -s "$KEY_FILE" ]; then K="$(cat "$KEY_FILE")"
else K="$(gen_hex)"; fi

probe() {
  local key="$1" line
  exec 3<>"/dev/tcp/$HOST/$PORT" 2>/dev/null || return 1
  printf 'GET /v1/models HTTP/1.0\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n' "$HOST" "$key" >&3
  IFS= read -r line <&3 || true
  exec 3<&- 3>&- 2>/dev/null || true
  printf '%s' "$line" | grep -q ' 200'
}
up() { (exec 3<>"/dev/tcp/$HOST/$PORT") 2>/dev/null && { exec 3<&- 3>&- 2>/dev/null; return 0; }; return 1; }

# 让 magpie 就绪（healthcheck 已经等过，这里再兜一层）
for i in $(seq 1 20); do up && break; sleep 1; done

if probe "$K"; then
  printf '%s' "$K" > "$KEY_FILE"; fix_owner
  echo "[magpie-init] 密钥有效，复用：$KEY_FILE"; exit 0
fi

echo "[magpie-init] 现有密钥未被 magpie 接受，开始重建网关密钥 $NAME …" >&2
# 按名字删；有的版本只认 id，再从 list 里按名字找 id 删一次
"$MAGPIE_BIN" gateway-key remove "$NAME" >/dev/null 2>&1 || true
kid=$("$MAGPIE_BIN" gateway-key list 2>/dev/null | grep -F -- "$NAME" | awk '{print $1}' | head -n 1 || true)
if [ -n "$kid" ] && [ "$kid" != "$NAME" ]; then "$MAGPIE_BIN" gateway-key remove "$kid" >/dev/null 2>&1 || true; fi
usable=1; case "$K" in sk-magpie-*) usable=0;; esac; [ "${#K}" -lt 8 ] && usable=0
if [ "$usable" = 1 ]; then out=$(printf '%s' "$K" | "$MAGPIE_BIN" gateway-key add "$NAME" --key - 2>&1) || true; fi
if ! probe "$K"; then
  out=$("$MAGPIE_BIN" gateway-key add "$NAME" 2>&1) || true
  k2=$(printf '%s\n' "$out" | grep -o 'sk-magpie-key-[A-Za-z0-9._-]*' | head -n 1 || true)
  [ -n "$k2" ] && K="$k2"
fi
if ! probe "$K"; then
  N2="$NAME-$(date +%s)"
  out=$("$MAGPIE_BIN" gateway-key add "$N2" 2>&1) || true
  k2=$(printf '%s\n' "$out" | grep -o 'sk-magpie-key-[A-Za-z0-9._-]*' | head -n 1 || true)
  [ -n "$k2" ] && K="$k2"
fi
if probe "$K"; then
  printf '%s' "$K" > "$KEY_FILE"; fix_owner
  echo "[magpie-init] 已重建网关密钥 $NAME 并回写 $KEY_FILE"; exit 0
fi
fix_owner
echo "[magpie-init] 仍然 401：magpie 拒绝这把密钥。" >&2
printf '%s\n' "$out" >&2
echo "[magpie-init] 处置：打开 http://127.0.0.1:${MAGPIE_UI_PORT:-3430}/ 建一把 key，" >&2
echo "  写进 .env 的 MAGPIE_ADMIN_KEY=<key>，再 docker compose up -d。" >&2
# 不让整套编排因为这一把密钥起不来：灵枢管理面照常可用，只是转发/同步暂时 401。
# 真正修好请按上面的处置手工建 key；下次 up 会自动再试一次自愈。
exit 0
