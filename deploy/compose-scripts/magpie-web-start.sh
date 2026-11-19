#!/bin/bash
# magpie 容器启动脚本：把页面口令送进环境，然后 exec magpie web。
# 由 docker-compose.yml 以 `entrypoint: ["/bin/bash","/opt/lingshu/magpie-web-start.sh"]` 调用。
set -e
if [ -s /config/.magpie_web_key ]; then
  MAGPIE_WEB_KEY=$(cat /config/.magpie_web_key)
  export MAGPIE_WEB_KEY
  echo "[magpie] 管理面口令来自 /config/.magpie_web_key" >&2
elif [ -s /secret/admin_web_key ]; then
  MAGPIE_WEB_KEY=$(cat /secret/admin_web_key)
  export MAGPIE_WEB_KEY
  echo "[magpie] 管理面口令来自 /secret/admin_web_key" >&2
elif [ -n "${MAGPIE_WEB_KEY:-}" ]; then
  echo "[magpie] 管理面口令来自环境变量 MAGPIE_WEB_KEY" >&2
else
  echo "[magpie] 未取到管理面口令：magpie 会自行生成一个随机值（每次重启都会变，/magpie/ 入口需手工粘贴）" >&2
fi
# magpie 要求口令至少 16 个字符，短了它会拒绝启动。这里不改值（改了 nginx 就对不上），
# 只把原因喊出来，免得只看到一行 "web key too short" 不知所措。
if [ -n "${MAGPIE_WEB_KEY:-}" ] && [ "${#MAGPIE_WEB_KEY}" -lt 16 ]; then
  echo "[magpie] 警告：管理面口令只有 ${#MAGPIE_WEB_KEY} 个字符，magpie 要求至少 16，会拒绝启动；清空 .env 的 MAGPIE_WEB_KEY 让 magpie-init 重新生成（或填 >=16 位）" >&2
fi
exec magpie web --addr 0.0.0.0:3430 --no-open
