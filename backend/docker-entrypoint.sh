#!/bin/sh
# 容器启动脚本：零配置开箱即用，同时不落公开默认密钥。
#   1) SM4 渠道凭据密钥：优先生效 SM4_KEY 环境变量；未提供则首次启动生成强随机密钥并持久化到 keys 卷
#   2) JWT RSA 密钥对：缺失时生成并持久化（重新生成会使已签发令牌失效）
#   3) 由环境变量渲染 config.yaml（DSN、监听地址等）
#   4) 启动服务；迁移与初始管理员种子由程序自身在启动时自动完成
set -eu

: "${APP_DIR:=/app}"
: "${KEYS_DIR:=$APP_DIR/keys}"
: "${DATA_DIR:=$APP_DIR/data}"
: "${CONFIG_PATH:=$APP_DIR/config.yaml}"
: "${DATABASE_URL:=postgres://llmgw:llmgw@postgres:5432/llmgw?sslmode=disable}"

mkdir -p "$KEYS_DIR" "$DATA_DIR"

# 1) SM4 密钥（16 字节 = 32 hex）
if [ -z "${SM4_KEY:-}" ]; then
  if [ -f "$KEYS_DIR/sm4_key" ]; then
    SM4_KEY=$(cat "$KEYS_DIR/sm4_key")
  else
    SM4_KEY=$(openssl rand -hex 16)
    printf '%s' "$SM4_KEY" > "$KEYS_DIR/sm4_key"
    chmod 600 "$KEYS_DIR/sm4_key"
    echo "[entrypoint] generated new SM4 key at $KEYS_DIR/sm4_key"
  fi
fi

# 2) JWT RSA 密钥对
if [ ! -f "$KEYS_DIR/jwt_rsa" ] || [ ! -f "$KEYS_DIR/jwt_rsa.pub" ]; then
  openssl genrsa -out "$KEYS_DIR/jwt_rsa" 2048
  openssl rsa -in "$KEYS_DIR/jwt_rsa" -pubout -out "$KEYS_DIR/jwt_rsa.pub"
  chmod 600 "$KEYS_DIR/jwt_rsa"
  echo "[entrypoint] generated new JWT RSA key pair at $KEYS_DIR"
fi

# 3) 渲染配置
cat > "$CONFIG_PATH" <<EOF
server:
  addr: "${SERVER_ADDR:-:8080}"
  static_dir: ""
database:
  dsn: "$DATABASE_URL"
jwt:
  private_key_path: "$KEYS_DIR/jwt_rsa"
  public_key_path: "$KEYS_DIR/jwt_rsa.pub"
  ttl_minutes: ${JWT_TTL_MINUTES:-720}
security:
  sm4_key: "$SM4_KEY"
sync:
  price_source_url: "${PRICE_SOURCE_URL:-https://models.dev/api.json}"
  interval_minutes: ${SYNC_INTERVAL_MINUTES:-60}
billing:
  retry_queue_path: "$DATA_DIR/billing_retry.jsonl"
  retry_interval_seconds: ${BILLING_RETRY_INTERVAL_SECONDS:-30}
EOF

# 4) 启动（相对路径的读取需在工作目录下）
cd "$APP_DIR"
exec ./llmgateway -config "$CONFIG_PATH"