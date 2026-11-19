#!/bin/sh
# 容器启动脚本：零配置开箱即用，同时不落公开默认密钥。
#   1) SM4 渠道凭据密钥：优先生效 SM4_KEY 环境变量；未提供则首次启动生成强随机密钥并持久化到 keys 卷
#   2) JWT RSA 密钥对：缺失时生成并持久化（重新生成会使已签发令牌失效）
#   3) 代理侧（magpie）管理密钥：留空则读共享卷里 magpie-init 首次生成的那一份
#   4) 由环境变量渲染 config.yaml（DSN、监听地址等）
#   5) 主进程以非特权用户 app 运行：root 启动时先修复卷属主（兼容旧 root 卷）、完成初始化，
#      再 exec 降权；迁移与初始管理员种子由程序自身在启动时自动完成
set -eu

: "${APP_DIR:=/app}"
: "${KEYS_DIR:=$APP_DIR/keys}"
: "${DATA_DIR:=$APP_DIR/data}"
: "${CONFIG_PATH:=$APP_DIR/config.yaml}"
: "${APP_USER:=app}"
: "${APP_GROUP:=$APP_USER}"
: "${DATABASE_URL:=postgres://llmgw:llmgw@postgres:5432/llmgw?sslmode=disable}"

mkdir -p "$KEYS_DIR" "$DATA_DIR"

# 兼容「卷已存在且由 root 创建」的历史部署：先修正卷属主，后续再降权运行
if [ "$(id -u)" -eq 0 ]; then
  chown "$APP_USER:$APP_GROUP" "$KEYS_DIR" "$DATA_DIR"
fi
chmod 700 "$KEYS_DIR" "$DATA_DIR"

# 1) SM4 密钥（16 字节 = 32 hex）
# 与程序内置回退路径 keys/sm4.key 保持一致：SM4_KEY 未提供时，由 entrypoint 或程序
# 任一方生成的密钥都落在 $KEYS_DIR/sm4.key，重启互相复用。
if [ -z "${SM4_KEY:-}" ]; then
  if [ -f "$KEYS_DIR/sm4.key" ]; then
    SM4_KEY=$(cat "$KEYS_DIR/sm4.key")
  else
    SM4_KEY=$(openssl rand -hex 16)
    printf '%s' "$SM4_KEY" > "$KEYS_DIR/sm4.key"
    chmod 600 "$KEYS_DIR/sm4.key"
    echo "[entrypoint] generated new SM4 key at $KEYS_DIR/sm4.key"
  fi
fi

# 2) JWT RSA 密钥对
if [ ! -f "$KEYS_DIR/jwt_rsa" ] || [ ! -f "$KEYS_DIR/jwt_rsa.pub" ]; then
  openssl genrsa -out "$KEYS_DIR/jwt_rsa" 2048
  openssl rsa -in "$KEYS_DIR/jwt_rsa" -pubout -out "$KEYS_DIR/jwt_rsa.pub"
  echo "[entrypoint] generated new JWT RSA key pair at $KEYS_DIR"
fi

# 密钥文件统一收紧到 600（目录已为 700）
chmod 600 "$KEYS_DIR/jwt_rsa" "$KEYS_DIR/jwt_rsa.pub"

# 3) 代理侧（magpie）接入参数
#    基址由 MAGPIE_BASE_URL 提供（compose 里默认 http://magpie:3425）。
#    管理密钥 MAGPIE_ADMIN_KEY 的口径：
#      - 环境变量显式给出（手工在 magpie 管理面建 key 的场景）→ 用它；
#      - 否则读共享卷里 magpie-init 首次生成的那一份 —— magpie 与 backend 读的是同一把，
#        因此无需任何人工填值（重置方式见 docs/deploy/magpie-container.md）。
#    账本变更流（UPSTREAM_ADMIN_KEY）打的是同一个代理侧，默认复用同一把密钥。
MAGPIE_ADMIN_KEY_FILE="${MAGPIE_ADMIN_KEY_FILE:-/run/magpie-secret/admin_key}"
# 优先级：共享卷里的密钥 > 环境变量。原因：卷里那份由 magpie-init 在启动后**实测 /v1/models 通过**
# 才写入，是当下 magpie 真正认的值；而环境变量可能是旧的/被顶掉的（历史上正是它在覆盖，导致
# 一直报 401 而自愈结果被忽略）。只有卷里没有文件时才用环境变量（手工接管场景）。
if [ -r "$MAGPIE_ADMIN_KEY_FILE" ] && [ -s "$MAGPIE_ADMIN_KEY_FILE" ]; then
  MAGPIE_ADMIN_KEY=$(cat "$MAGPIE_ADMIN_KEY_FILE")
elif [ -n "${MAGPIE_ADMIN_KEY:-}" ]; then
  echo "[entrypoint] 警告：没有 $MAGPIE_ADMIN_KEY_FILE，使用环境变量 MAGPIE_ADMIN_KEY"
fi
if [ -z "${UPSTREAM_ADMIN_KEY:-}" ] && [ -n "${MAGPIE_ADMIN_KEY:-}" ]; then
  UPSTREAM_ADMIN_KEY="$MAGPIE_ADMIN_KEY"
fi
export MAGPIE_ADMIN_KEY UPSTREAM_ADMIN_KEY

# 代理侧页面口令（/api/* 用 ?k= 登录 magpie）：这里以 root 身份读出并 export，
# 后续进程即使降权也能用——避免 /secret 里 600 root 文件对非 root 不可读导致 topology 401。
MAGPIE_WEB_KEY_FILE="${MAGPIE_WEB_KEY_FILE:-/run/magpie-secret/admin_web_key}"
if [ -z "${MAGPIE_WEB_KEY:-}" ] && [ -r "$MAGPIE_WEB_KEY_FILE" ]; then
  MAGPIE_WEB_KEY=$(cat "$MAGPIE_WEB_KEY_FILE")
  export MAGPIE_WEB_KEY
  echo "[entrypoint] magpie 页面口令已加载（${#MAGPIE_WEB_KEY} 字符）"
else
  echo "[entrypoint] 警告：未读到 $MAGPIE_WEB_KEY_FILE，拓扑 /api/groups 会 401"
fi

# DSN 统一附加时区参数：已有 timezone 则不重复；已有 query 用 & 连接，避免破坏 sslmode 等已有参数
dsn_with_timezone() {
  case "$1" in
    *timezone=*) printf '%s' "$1" ;;
    *\?*)        printf '%s&timezone=Asia/Shanghai' "$1" ;;
    *)           printf '%s?timezone=Asia/Shanghai' "$1" ;;
  esac
}

# 4) 渲染配置
cat > "$CONFIG_PATH" <<EOF
server:
  addr: "${SERVER_ADDR:-:8080}"
  static_dir: ""
database:
  dsn: "$(dsn_with_timezone "$DATABASE_URL")"
jwt:
  private_key_path: "$KEYS_DIR/jwt_rsa"
  public_key_path: "$KEYS_DIR/jwt_rsa.pub"
  ttl_minutes: ${JWT_TTL_MINUTES:-360}
security:
  sm4_key: "$SM4_KEY"
magpie:
  # 代理侧基址：拓扑源回落地址（TOPOLOGY_SOURCE_URL 未配置时使用）；默认由 compose 的 MAGPIE_BASE_URL 提供
  base_url: "${MAGPIE_BASE_URL:-}"
  # 管理/用量/拓扑用的网关密钥：与 magpie 容器内的那把是同一个值
  admin_key: "${MAGPIE_ADMIN_KEY:-}"
sync:
  price_source_url: "${PRICE_SOURCE_URL:-https://models.dev/api.json}"
  interval_minutes: ${SYNC_INTERVAL_MINUTES:-60}
billing:
  retry_queue_path: "$DATA_DIR/billing_retry.jsonl"
  retry_interval_seconds: ${BILLING_RETRY_INTERVAL_SECONDS:-30}
preset:
  # 外挂渠道预设包地址；留空表示未配置（首次启动不拉取，AC-12）。
  pack_url: "${PRESET_PACK_URL:-}"
EOF
chmod 600 "$CONFIG_PATH"

# 5) 启动（相对路径的读取需在工作目录下）；root 场景先 chown 生成的密钥/配置，再降权执行
cd "$APP_DIR"
if [ "$(id -u)" -eq 0 ]; then
  chown -R "$APP_USER:$APP_GROUP" "$KEYS_DIR" "$DATA_DIR"
  chown "$APP_USER:$APP_GROUP" "$CONFIG_PATH"
  exec su-exec "$APP_USER:$APP_GROUP" ./llmgateway -config "$CONFIG_PATH"
fi
exec ./llmgateway -config "$CONFIG_PATH"