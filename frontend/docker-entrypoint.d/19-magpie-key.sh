#!/bin/sh
# 把 magpie 管理面的页面口令（MAGPIE_WEB_KEY）编译成 nginx 的 `map`，供 /magpie/ 反代使用。
#
# 为什么要这么绕：magpie 的页面口令只能从「nginx 能读到的运行时数据」来，而 nginx 的
# 官方 envsubst 模板机制会替换配置里的所有 `$xxx`（$host、$uri 这些也在此列），风险大。
# 这里反过来：主配置保持静态，只生成一个 20 行的 map 文件。
#
# 取值优先级（**卷优先**，与 magpie 容器同一口径）：
#   1. /run/magpie-secret/admin_web_key（magpie-init 写的那份；手填的短口令也在那里被补齐到 16 位以上）
#   2. 环境变量 MAGPIE_WEB_KEY（卷里没有时才用；手工接管场景）
#   3. 都没有 → 生成「不注入」的 map：口令仍可手工粘贴（magpie 自己的登录表单），管理面照常可用
#
# 卷优先的理由：magpie 要求口令 ≥16 字符（短了直接拒绝启动），magpie-init 会把不足的口令
# 追加随机 hex 补齐后写回文件。只有读同一份文件，nginx 注入的 ?k= 才与 magpie 真正在用的
# 口令一致；否则会「magpie 用补齐值、nginx 用原短值」→ 401 循环。
set -eu

# 允许自检脚本把输出改到临时目录（scripts/magpie-ui-proxy-selftest.sh），生产不要动它。
OUT=${MAGPIE_KEY_CONF_OUT:-/etc/nginx/conf.d/00-magpie-key.conf}
KEY_FILE=${MAGPIE_WEB_KEY_FILE:-/run/magpie-secret/admin_web_key}

KEY=
KEY_SRC=
if [ -s "$KEY_FILE" ]; then
    KEY=$(cat "$KEY_FILE")
    KEY_SRC=$KEY_FILE
fi
if [ -z "$KEY" ] && [ -n "${MAGPIE_WEB_KEY:-}" ]; then
    KEY=$MAGPIE_WEB_KEY
    KEY_SRC=环境变量
fi

# magpie 的长度硬要求：≥16 字符，短了直接拒绝启动。这种口令无论如何都用不上，
# 当成「没配」—— 否则会拿着一个 magpie 不认的值无限 401，比让管理员手工粘一次更难查。
if [ -n "$KEY" ] && [ "${#KEY}" -lt 16 ]; then
    echo "[nginx] 管理面口令只有 ${#KEY} 个字符（magpie 要求 ≥16，短了起不来），忽略：管理面需手工粘贴口令" >&2
    KEY=
fi

# 口令要进 URL 的查询串，只允许 magpie 自己接受的那套字符（letters digits - . _ ~）。
# 出现其它字符就当成「没配」—— 宁可让管理员手工粘一次，也不要把坏值拼进 URL 造成 401 循环。
case "$KEY" in
    *[!A-Za-z0-9._~-]*)
        echo "[nginx] MAGPIE_WEB_KEY 含非法字符（只允许字母数字与 - . _ ~），忽略：管理面需手工粘贴口令" >&2
        KEY=
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
    echo "[nginx] /magpie/ 入口已注入管理面口令（来源：$KEY_SRC）"
else
    echo "[nginx] 未取到管理面口令：/magpie/ 仍受 ADMIN 票据保护，但打开后需手工粘贴口令" >&2
fi
