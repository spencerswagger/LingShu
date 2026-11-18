#!/bin/bash
# 启动前一次性准备（root）：页面口令 + 网关密钥 + /config 属主。
# 关键：magpie 只以 uid 65532 读 /config/magpie/caller-keys.json，root 建的 600 文件它读不到
# → 会把所有网关密钥当成不存在（表现为“key … removed or mistyped”）。所以任何 CLI 写完后
# 必须把 /config 交回 65532 并收紧到 600。
set -eu

WEB_FILE=/secret/admin_web_key
KEY_FILE=/secret/admin_key
NAME="${MAGPIE_ADMIN_KEY_NAME:-lingshu-backend}"
MAGPIE_BIN=$(command -v magpie || echo /magpie)

gen() { if command -v openssl >/dev/null 2>&1; then openssl rand -hex 16
        else head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n'; fi; }

mkdir -p /secret /config/magpie
chmod 700 /secret /config 2>/dev/null || true

# 1) 页面口令：环境变量 > 已有 > 生成；不足 16 位补齐（magpie 硬要求）
if [ -n "${MAGPIE_WEB_KEY:-}" ]; then printf '%s' "$MAGPIE_WEB_KEY" > "$WEB_FILE"
elif [ ! -s "$WEB_FILE" ]; then gen > "$WEB_FILE"; fi
cur=$(cat "$WEB_FILE" 2>/dev/null || true)
[ "${#cur}" -ge 16 ] || printf '%s%s' "$cur" "$(gen)" > "$WEB_FILE"

# 2) 网关密钥：如果环境变量显式给了且不为空，就用它；否则**每次启动重建一把**
#    （重建比“复用旧值”安全：旧值可能已被 magpie 迁移/导入顶掉）
want="${MAGPIE_ADMIN_KEY:-}"

# 先把钥匙库和旧文件收回 65532，避免上一次 root 写入留下的 600 root 文件
chown -R 65532:65532 /config 2>/dev/null || true

if [ -z "$want" ]; then
  # 删掉同名旧 key（按名字；再按 list 里的 id 删一次，兼容只认 id 的版本）
  "$MAGPIE_BIN" gateway-key remove "$NAME" >/dev/null 2>&1 || true
  kid=$("$MAGPIE_BIN" gateway-key list 2>/dev/null | grep -F -- "$NAME" | awk '{print $1; exit}' || true)
  if [ -n "${kid:-}" ] && [ "$kid" != "$NAME" ]; then "$MAGPIE_BIN" gateway-key remove "$kid" >/dev/null 2>&1 || true; fi
  out=$("$MAGPIE_BIN" gateway-key add "$NAME" 2>&1) || true
  want=$(printf '%s\n' "$out" | grep -o 'sk-magpie-key-[A-Za-z0-9._-]*' | head -n 1 || true)
  if [ -z "$want" ]; then
    out=$("$MAGPIE_BIN" gateway-key add "$NAME-$(date +%s)" 2>&1) || true
    want=$(printf '%s\n' "$out" | grep -o 'sk-magpie-key-[A-Za-z0-9._-]*' | head -n 1 || true)
  fi
  [ -n "$want" ] || { echo "[magpie-secret-init] 无法创建网关密钥，见下：" >&2; printf '%s\n' "$out" >&2; }
fi

if [ -n "$want" ]; then printf '%s' "$want" > "$KEY_FILE"; fi

# 3) 把属主交回 magpie 运行用户，并收紧权限（这一步是整套能否读到的关键）
chown -R 65532:65532 /config 2>/dev/null || true
find /config -type f -exec chmod 600 {} \; 2>/dev/null || true
chown 65532:65532 "$WEB_FILE" "$KEY_FILE" 2>/dev/null || true
chmod 600 "$KEY_FILE" 2>/dev/null || true
# 页面口令要给 backend 读（它用 ?k= 拉 /api/groups），backend 以非 root 运行，600 读不到 -> 0644
chmod 644 "$WEB_FILE" 2>/dev/null || true
echo "[magpie-secret-init] 准备完成（key 尾号 ${want: -4}）"
