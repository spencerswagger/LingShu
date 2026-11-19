#!/bin/bash
# 启动前一次性准备（root）：页面口令 + 网关密钥 + /config 属主。
# 关键：magpie 只以 uid 65532 读 /config/magpie/caller-keys.json，root 建的 600 文件它读不到
# → 会把所有网关密钥当成不存在（表现为“key … removed or mistyped”）。所以任何 CLI 写完后
# 必须把 /config 交回 65532；权限归一化只去掉 group/other，**保留 owner 执行位**
# （/config/cache 下有 magpie 自己解包的 ELF，递归 600 会让它 fork/exec 失败）。
set -eu

# 权限归一化（与回归测试共用同一实现）
HERE=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib/perms.sh disable=SC1091
. "$HERE/lib/perms.sh"

WEB_FILE=/secret/admin_web_key
KEY_FILE=/secret/admin_key
NAME="${MAGPIE_ADMIN_KEY_NAME:-lingshu-backend}"
MAGPIE_BIN=$(command -v magpie || echo /magpie)

gen() { if command -v openssl >/dev/null 2>&1; then openssl rand -hex 16
        else head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n'; fi; }

mkdir -p /secret /config/magpie
# /secret 由 root 拥有：711 允许 magpie(65532) 穿越读取属主为自己的 600 文件，但列不出目录
chmod 711 /secret 2>/dev/null || true
chmod 700 /config 2>/dev/null || true

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

# 3) 把属主交回 magpie 运行用户，并归一化权限
#    （只去 group/other；保留 owner 执行位；/config/cache 整棵树不动）
chown -R 65532:65532 /config 2>/dev/null || true
normalize_config_perms /config
chown 65532:65532 "$WEB_FILE" "$KEY_FILE" 2>/dev/null || true
# /secret 归 root(711)：这两个文件给 magpie 属主 600（可读可写，不可执行）
chmod u+rw,go-rwx "$WEB_FILE" "$KEY_FILE" 2>/dev/null || true
# magpie 以 65532 运行，自己在 /config 下有读写权：给它一份同值口令（owner rw，group/other 无），
# 避免 /secret(711 root) 跨用户读不到导致它自造随机口令（那会让 /magpie/ 与 /api 全部 401）。
printf '%s' "$(cat "$WEB_FILE")" > /config/.magpie_web_key
chown 65532:65532 /config/.magpie_web_key 2>/dev/null || true
chmod u+rw,go-rwx /config/.magpie_web_key 2>/dev/null || true
echo "[magpie-secret-init] 准备完成（key 尾号 ${want: -4}）"
