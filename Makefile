.PHONY: up down logs migrate run test frontend-dev keys e2e build-frontend deploy
# 一键部署全部服务（前端 + 后端 + PostgreSQL），对外仅 web 一个端口（默认 80）
up:
	docker compose up -d --build
down:
	docker compose down
logs:
	docker compose logs -f
# 生成 RSA 密钥对到 backend/keys/（JWT 签发用）
keys:
	cd backend && mkdir -p keys && openssl genrsa -out keys/jwt_rsa 2048 && openssl rsa -in keys/jwt_rsa -pubout -out keys/jwt_rsa.pub
migrate:
	cd backend && [ -f config.yaml ] || cp config.example.yaml config.yaml; go run ./cmd/server -migrate-only -config config.yaml
run:
	# 说明：security.sm4_key 留空即自动生成——启动时从 keys/sm4.key 读取，文件不存在则
	# 生成强随机密钥写入（chmod 600），后续重启复用；不提供公开默认密钥。多环境需共用
	# 同一密钥时，用 openssl rand -hex 16 生成后显式配置。
	cd backend && [ -f config.yaml ] || cp config.example.yaml config.yaml; go run ./cmd/server -config config.yaml
test:
	cd backend && go test ./...
# 守卫式 e2e：未设 TEST_DATABASE_URL 时自动跳过
e2e:
	cd backend && TEST_DATABASE_URL=$${TEST_DATABASE_URL} go test ./tests/... -v
frontend-dev:
	cd frontend && npm run dev
build-frontend:
	cd frontend && npm run build
# 构建产物到 bin/llmgateway + frontend/dist
deploy: keys
	mkdir -p bin
	cd backend && go build -o ../bin/llmgateway ./cmd/server
	$(MAKE) build-frontend
	@echo "构建完成: bin/llmgateway + frontend/dist"