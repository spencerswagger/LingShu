# 后端镜像：多阶段构建，产物为静态编译的单个二进制（无 CGO 依赖）。
# 迁移 SQL 通过 go:embed 内嵌在二进制中，启动时自动执行，无需额外拷贝文件。

FROM golang:1.27-alpine AS build
WORKDIR /src

# 先拷贝依赖清单，利用构建缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/llmgateway ./cmd/server

FROM alpine:3.20
# openssl：首次启动生成 JWT RSA 密钥对与 SM4 密钥；tzdata：计费时段系数按 Asia/Shanghai 等时区计算
RUN apk add --no-cache ca-certificates tzdata openssl

WORKDIR /app
COPY --from=build /out/llmgateway /app/llmgateway
COPY docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh

ENV TZ=Asia/Shanghai
EXPOSE 8080
ENTRYPOINT ["/app/docker-entrypoint.sh"]