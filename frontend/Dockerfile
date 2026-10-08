# 前端镜像：Node 构建静态产物 → nginx 托管，并与后端共用同一对外端口（/api、/v1 反代）。

FROM registry.cn-hangzhou.aliyuncs.com/spencerswagger/library-node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

FROM registry.cn-hangzhou.aliyuncs.com/spencerswagger/library-nginx:1.27-alpine
COPY --from=build /app/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 80