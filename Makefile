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
	# 说明：security.sm4_key 必填（缺失会直接退出）。config.yaml 由 example 复制得来，
	# 自带本地开发默认值；生产环境请务必替换为 openssl rand -hex 16 生成的强随机密钥。
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