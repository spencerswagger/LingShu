#!/bin/sh
# 把 magpie 管理面的页面口令（MAGPIE_WEB_KEY）编译成 nginx 的 `map`，供 /magpie/ 反代使用。
#
# 为什么要这么绕：magpie 的页面口令只能从「nginx 能读到的运行时数据」来，而 nginx 的
# 官方 envsubst 模板机制会替换配置里的所有 `$xxx`（$host、$uri 这些也在此列），风险大。
# 这里反过来：主配置保持静态，只生成一个 20 行的 map 文件。
#
# 取值优先级（与 backend 的 entrypoint 同口径）：
#   1. 环境变量 MAGPIE_WEB_KEY（.env 里手工固定过的人，行为不变）
#   2. /run/magpie-secret/admin_web_key（magpie-init 首次 up 时自动生成并落在卷里）
#   3. 都没有 → 生成「不注入」的 map：口令仍可手工粘贴（magpie 自己的登录表单），管理面照常可用
set -eu

# 允许自检脚本把输出改到临时目录（scripts/magpie-ui-proxy-selftest.sh），生产不要动它。
OUT=${MAGPIE_KEY_CONF_OUT:-/etc/nginx/conf.d/00-magpie-key.conf}
KEY_FILE=${MAGPIE_WEB_KEY_FILE:-/run/magpie-secret/admin_web_key}
KEY=${MAGPIE_WEB_KEY:-}

if [ -z "$KEY" ] && [ -s "$KEY_FILE" ]; then
    KEY=$(cat "$KEY_FILE")
fi

# 口令要进 URL 的查询串，只允许 magpie 自己接受的那套字符（letters digits - . _ ~）。
# 出现其它字符就当成「没配」——宁可让管理员手工粘一次，也不要把坏值拼进 URL 造成 401 循环。
case "$KEY" in
    *[!A-Za-z0-9._~-]*)
        echo "[nginx] MAGPIE_WEB_KEY 含非法字符（只允许字母数字与 - . _ ~），忽略：管理面需手工粘贴口令" >&2
        KEY=""
        ;;
esac

if [ -n "$KEY" ]; then
    cat >"$OUT" <<EOF
# 由 docker-entrypoint.d/19-magpie-key.sh 生成（容器启动时覆盖镜像默认值，勿手改）
map "\$cookie_magpie_web_3430|\$arg_k" \$magpie_entry_qs {
    "|"     "?k=$KEY";
    default "";
}
EOF
    chmod 644 "$OUT"
    echo "[nginx] /magpie/ 入口已注入管理面口令（来源：$([ -n "${MAGPIE_WEB_KEY:-}" ] && echo 环境变量 || echo "$KEY_FILE")）"
else
    echo "[nginx] 未取到管理面口令：/magpie/ 仍受 ADMIN 票据保护，但打开后需手工粘贴口令" >&2
fi
