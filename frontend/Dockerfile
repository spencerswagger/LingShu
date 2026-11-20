# 前端镜像：Node 构建静态产物 → nginx 托管，并与后端共用同一对外端口（/api、/v1 反代）。
#
# 基础镜像与软件源统一使用 Docker Hub 官方镜像 / 官方源：
# 镜像构建在 GitHub Actions（海外运行器）执行，官方源比国内镜像更快、更稳定，
# 也避免从国内镜像仓库跨境拉取基础镜像导致的构建缓慢。

FROM node:22-alpine AS build
WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

FROM nginx:1.27-alpine
COPY --from=build /app/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
# /v1 白名单公共转发段：被 default.conf 的 5 个放行 location include（.inc 后缀不会被
# conf.d/*.conf 顶层装载）。缺这一行 = 容器起不来（include 找不到文件）。
COPY v1-proxy.inc /etc/nginx/conf.d/v1-proxy.inc
# 代理侧管理面（/magpie/）：默认「不注入口令」的 map，容器启动时由 19-magpie-key.sh 覆盖。
COPY 00-magpie-key.conf /etc/nginx/conf.d/00-magpie-key.conf
COPY docker-entrypoint.d/19-magpie-key.sh /docker-entrypoint.d/19-magpie-key.sh
RUN chmod +x /docker-entrypoint.d/19-magpie-key.sh
EXPOSE 80
