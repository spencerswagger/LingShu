#!/bin/sh
# 容器启动脚本：零配置开箱即用，同时不落公开默认密钥。
#   1) SM4 渠道凭据密钥：优先生效 SM4_KEY 环境变量；未提供则首次启动生成强随机密钥并持久化到 keys 卷
#   2) JWT RSA 密钥对：缺失时生成并持久化（重新生成会使已签发令牌失效）
#   3) 由环境变量渲染 config.yaml（DSN、监听地址等）
#   4) 主进程以非特权用户 app 运行：root 启动时先修复卷属主（兼容旧 root 卷）、完成初始化，
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

# DSN 统一附加时区参数：已有 timezone 则不重复；已有 query 用 & 连接，避免破坏 sslmode 等已有参数
dsn_with_timezone() {
  case "$1" in
    *timezone=*) printf '%s' "$1" ;;
    *\?*)        printf '%s&timezone=Asia/Shanghai' "$1" ;;
    *)           printf '%s?timezone=Asia/Shanghai' "$1" ;;
  esac
}

# 3) 渲染配置
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
sync:
  price_source_url: "${PRICE_SOURCE_URL:-https://models.dev/api.json}"
  interval_minutes: ${SYNC_INTERVAL_MINUTES:-60}
billing:
  retry_queue_path: "$DATA_DIR/billing_retry.jsonl"
  retry_interval_seconds: ${BILLING_RETRY_INTERVAL_SECONDS:-30}
EOF
chmod 600 "$CONFIG_PATH"

# 4) 启动（相对路径的读取需在工作目录下）；root 场景先 chown 生成的密钥/配置，再降权执行
cd "$APP_DIR"
if [ "$(id -u)" -eq 0 ]; then
  chown -R "$APP_USER:$APP_GROUP" "$KEYS_DIR" "$DATA_DIR"
  chown "$APP_USER:$APP_GROUP" "$CONFIG_PATH"
  exec su-exec "$APP_USER:$APP_GROUP" ./llmgateway -config "$CONFIG_PATH"
fi
exec ./llmgateway -config "$CONFIG_PATH"