#!/bin/bash
# magpie 起来之后的**校验**（不重建）。建 key 与属主修正在启动前那步已完成；
# 这里只用真实请求确认“后端要用的那把”被 magpie 接受，并把结论打清楚。
set -eu
ADDR="${MAGPIE_ADDR:-http://magpie:3425}"
HOST_NAME=$(printf '%s' "$ADDR" | sed -E 's#^[a-z]+://##; s#/.*$##; s#:.*$##')
HOST=$(getent hosts "$HOST_NAME" 2>/dev/null | awk '{print $1; exit}'); [ -n "${HOST:-}" ] || HOST="$HOST_NAME"
PORT=$(printf '%s' "$ADDR" | sed -nE 's#^[a-z]+://[^:/]+:([0-9]+).*#\1#p'); PORT=${PORT:-3425}
KEY_FILE=/secret/admin_key
K=$(cat "$KEY_FILE" 2>/dev/null || true)

probe() { exec 3<>"/dev/tcp/$HOST/$PORT" 2>/dev/null || return 1
  printf 'GET /v1/models HTTP/1.0\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n' "$HOST" "$1" >&3
  IFS= read -r line <&3 || true; exec 3<&- 3>&- 2>/dev/null || true
  printf '%s' "$line" | grep -q ' 200'; }

for i in $(seq 1 20); do (exec 3<>"/dev/tcp/$HOST/$PORT") 2>/dev/null && { exec 3<&- 3>&- 2>/dev/null; break; }; sleep 1; done
if [ -z "$K" ]; then echo "[magpie-init] 没有 /secret/admin_key，转发与同步会 401" >&2; exit 0; fi
if probe "$K"; then echo "[magpie-init] 网关密钥有效（尾号 ${K: -4}）"; exit 0; fi
echo "[magpie-init] 网关密钥仍不被接受（尾号 ${K: -4}）。查 magpie 日志是否 caller-keys.json 权限/迁移问题。" >&2
exit 0
