#!/bin/bash
# 由 docker-compose.yml 以 `command: ["/bin/bash","/opt/lingshu/magpie-key-init.sh"]` 调用。
# 逻辑与本文件同源；改这里即可，compose 不再内联 bash。
set -u
NAME="${MAGPIE_ADMIN_KEY_NAME:-lingshu-backend}"
ADDR="${MAGPIE_ADDR:-http://magpie:3425}"
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

probe() { curl -fsS -o /dev/null -H "Authorization: Bearer $1" "$ADDR/v1/models" 2>/dev/null; }

# 让 magpie 就绪（healthcheck 已经等过，这里再兜一层）
for i in $(seq 1 20); do
  curl -fsS -o /dev/null "$ADDR/healthz" 2>/dev/null && break
  curl -fsS -o /dev/null "$ADDR/v1/models" 2>/dev/null && break
  sleep 1
done

if probe "$K"; then
  printf '%s' "$K" > "$KEY_FILE"; fix_owner
  echo "[magpie-init] 密钥有效，复用：$KEY_FILE"; exit 0
fi

echo "[magpie-init] 现有密钥未被 magpie 接受，开始重建网关密钥 $NAME …" >&2
# 删掉同名旧 key（存在则删），再用我们自己的值新建；都基于运行中的实例
id=$(magpie gateway-key list 2>/dev/null | awk -v n="$NAME" 'index($0,n){print $1; exit}')
[ -n "$id" ] && magpie gateway-key remove "$id" >/dev/null 2>&1 || true
out=$(printf '%s' "$K" | magpie gateway-key add "$NAME" --key - 2>&1) || true
if ! probe "$K"; then
  # 退一步：让 magpie 自己生成一把，抓出它的值
  out=$(magpie gateway-key add "$NAME" 2>&1) || true
  k2=$(printf '%s\n' "$out" | grep -o 'sk-magpie-key-[A-Za-z0-9._-]*' | head -n 1 || true)
  if [ -n "$k2" ]; then K="$k2"; fi
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
exit 1
