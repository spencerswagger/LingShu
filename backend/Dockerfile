# 后端镜像：多阶段构建，产物为静态编译的单个二进制（无 CGO 依赖）。
# 迁移 SQL 通过 go:embed 内嵌在二进制中，启动时自动执行，无需额外拷贝文件。
#
# 基础镜像与软件源统一使用 Docker Hub 官方镜像 / 官方源：
# 镜像构建在 GitHub Actions（海外运行器）执行，官方源比国内镜像更快、更稳定，
# 也避免从国内镜像仓库跨境拉取基础镜像导致的构建缓慢。

FROM golang:1.27-alpine AS build
WORKDIR /src

# 官方 Go 模块代理（与 Go 默认值一致，显式写出以固定口径；末尾 direct 作为兜底）
ENV GOPROXY=https://proxy.golang.org,direct

# 先拷贝依赖清单，利用构建缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/llmgateway ./cmd/server

FROM alpine:3.20
# 安装：openssl（首次启动生成 JWT RSA 密钥对与 SM4 密钥）；tzdata（计费时段系数按 Asia/Shanghai 等时区计算）；
#       su-exec（entrypoint 完成卷属主修复与初始化后，降权到非特权用户 app，uid 10001，运行主进程）
RUN apk add --no-cache ca-certificates tzdata openssl su-exec \
    && adduser -D -u 10001 app

WORKDIR /app
COPY --from=build /out/llmgateway /app/llmgateway
COPY docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh \
    && chown -R app:app /app

# 注意：不设置 USER app。entrypoint 需以 root 启动，先 chown 修复
# 「已存在且由 root 创建」的旧卷，再 exec 降权到 app 运行主进程（先 chown 再降权）。
ENV TZ=Asia/Shanghai
EXPOSE 8080
ENTRYPOINT ["/app/docker-entrypoint.sh"]
