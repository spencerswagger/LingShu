---
summary: "部署与升级（拉镜像、零构建）"
read_when:
  - 要在服务器上部署/升级灵枢
---

# 部署与升级（拉镜像、零构建）

**结论先说：部署机不构建，只拉镜像。** 前后端镜像由 GitHub Actions
（`.github/workflows/build-images.yml`）在打 tag 时构建并推到阿里云 ACR：

```
registry.cn-hangzhou.aliyuncs.com/spencerswagger/lingshu-backend:<tag>
registry.cn-hangzhou.aliyuncs.com/spencerswagger/lingshu-web:<tag>
```

预发布 tag（`v0.14.0-rc1` 这类）**不打 `latest`**，所以 compose 里钉的是具体版本号。

---

## 1 三步部署

### ① 拿到 `docker-compose.yml`（可选再拿 `.env`）

```bash
wget https://raw.githubusercontent.com/spencerswagger/LingShu/v2/docker-compose.yml
wget https://raw.githubusercontent.com/spencerswagger/LingShu/v2/.env.example -O .env   # 可选
```

私有仓库直连不通时，从任何一份仓库检出里拷这两个文件即可（只要这两个文件，不需要源码目录）。
`.env` 也可以完全不建，直接用环境变量或就用 compose 里的默认值。

### ② （可选）改 `.env`

最常改的两项：`APP_PORT`（对外端口，默认 80）、`POSTGRES_PASSWORD`（数据库口令，默认 `llmgw`）。
镜像 tag 在 `.env` 里的 `LINGSHU_IMAGE_TAG`，默认 `v0.14.0-rc1`。

> ACR 仓库是私有的：登录一次即可，凭据会留在 `~/.docker/config.json`。
> ```bash
> docker login registry.cn-hangzhou.aliyuncs.com
> ```

### ③ 起服务

```bash
docker compose up -d
```

首次启动自动完成：建库 → 跑迁移（`0019`…`0025`）→ 建初始管理员（`admin / admin123`，登录后立刻改密）
→ 生成 JWT/SM4 密钥与代理侧凭据（都落在卷里，重启不变）。

自检：

```bash
docker compose ps                       # 期望：postgres/backend/web/magpie 起，magpie-init 为 exited (0)
curl -s localhost:${APP_PORT:-80}/healthz     # 200
curl -s -o /dev/null -w '%{http_code}\n' localhost:${APP_PORT:-80}/v1/models            # 401（没有令牌）
curl -s -o /dev/null -w '%{http_code}\n' localhost:${APP_PORT:-80}/magpie/              # 403（未登录管理员）
```

### 可选：在部署机上用源码本地构建

默认路径用不到。真要本地构建（改完代码立刻验、或部署机连不上 ACR）：

```bash
docker compose -f docker-compose.yml -f docker-compose.build.yml build
docker compose -f docker-compose.yml -f docker-compose.build.yml up -d
```

构建产物按 compose 里的 `image:` 命名，不推 ACR。

---

## 2 升级 = 改 tag → `up -d`

```bash
# ① 改 .env 里的 LINGSHU_IMAGE_TAG=<新 tag>（或直接改 compose 里 image: 的默认 tag）
# ② 拉新镜像并重建容器
docker compose pull
docker compose up -d
```

- **数据库迁移由后端启动时自动执行**，不需要手工跑迁移脚本。
- tag 变了 `up -d` 自己会拉；`pull` 只是把「同一个 tag 被重新推送」这种情况也覆盖掉。
- 只有 `postgres` / `backend` / `web` / `magpie` 会被重建，卷（数据库、密钥、代理侧配置）不动。
- 回滚**不是**改回 tag 就行：数据库迁移里有不可逆的一步（见下）。

### ⚠️ 升级前必须备份（`0024` 不可逆）

`0024_v2_drop_legacy.sql` 会**删掉 10 张表 + 3 列**（渠道族、标签、探活、自动模型的历史数据）。
删掉的数据只能靠升级前的整库快照回来，没有反向 DDL。所以：

```bash
# 1) 先停写（否则 pg_dump 跨表不一致）
docker compose stop backend web
# 2) 备份直接写进数据卷（不落宿主机目录）
docker compose exec postgres sh -c 'mkdir -p /var/lib/postgresql/data/pgbackup && \
  pg_dump -Fc -Z6 -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
  -f /var/lib/postgresql/data/pgbackup/pre-upgrade-$(date +%Y%m%d%H%M).dump'
# 3) 校验（能列内容、行数非 0）再升级
docker compose exec postgres sh -c 'ls -l /var/lib/postgresql/data/pgbackup/'
docker compose exec postgres sh -c 'pg_restore -l $(ls -t /var/lib/postgresql/data/pgbackup/*.dump | head -1) | head'
# 4) 升级
docker compose pull && docker compose up -d
```

首次从 V1（迁移到 `0018`）升到 V2 时，这一批迁移会一次性全部执行，`0024` 就在里面 —— **备份不是可选项**。

回滚步骤（代码与数据必须一起退）见
[`docs/plans/2026-11-15-v2-p1-rollback.md`](../plans/2026-11-15-v2-p1-rollback.md)。

---

## 3 不依赖源码目录（只依赖本文件 + 卷）

| 东西 | 在哪 | 说明 |
| --- | --- | --- |
| 后端二进制 | 镜像 `/app/llmgateway` | 多阶段构建产物，`CGO_ENABLED=0` 静态二进制 |
| 迁移 SQL | 镜像内（`go:embed` 进二进制） | 启动时自动执行，不需要拷 `.sql` 文件 |
| 前端静态资源 + nginx 配置 | 镜像 `/usr/share/nginx/html`、`/etc/nginx/conf.d` | 不用挂载前端目录 |
| 启动脚本 | 镜像 `/app/docker-entrypoint.sh`、`/docker-entrypoint.d/19-magpie-key.sh` | 渲染 `config.yaml`、生成密钥、注入管理台口令 |
| 密钥 / 数据 | 卷 `backend_keys`、`backend_data`、`pgdata`、`magpie_config`、`magpie_secret` | 重建容器不影响 |

因此部署机上只需要 `docker-compose.yml`（可选 `.env`）与 docker 本身：**没有任何 `build:`
或源码 bind-mount**，`docker compose up -d` 不会读当前目录以外的任何源码文件。
（只有叠加 `docker-compose.build.yml` 时才需要 `./backend`、`./frontend` 两个源码目录。）

---

## 4 常用运维

```bash
docker compose ps                       # 状态
docker compose logs -f backend          # 后端日志（迁移进度、对账、计费异常都在这）
docker compose logs magpie-init         # 首次启动的代理侧凭据生成结果
docker compose restart backend          # 重启（改 .env 的普通变量后需要）
docker compose down                     # 停止（数据保留）
docker compose down -v                  # ⚠️ 连数据卷一起删（数据库、密钥、代理侧凭据全没）
```

代理侧（magpie）的端口、管理台入口、凭据位置与重置方式：
[`docs/deploy/magpie-container.md`](./magpie-container.md)。

---

## 5 常见问题

| 现象 | 处置 |
| --- | --- |
| `pull` 报 `unauthorized` / `denied` | 没登录 ACR：`docker login registry.cn-hangzhou.aliyuncs.com` 后重试 |
| `manifest unknown` | tag 打错（预发布 tag 不打 `latest`，请用具体版本号如 `v0.14.0-rc1`） |
| `/v1` 一直 410 | `/v1` 由 nginx 直连代理侧网关端口（`magpie:3425`），不经过灵枢后端；直连灵枢端口本来就是 410。确认 nginx 容器已启动、`frontend` 能解析 `magpie` |
| 后端启动即退出、日志有 `42P01` / `42703` | 代码与数据库版本不匹配（升级只换了一半）→ 按回滚说明「代码与数据一起退」 |
| 升级后登录令牌全失效 / 渠道凭据解不开 | `backend_keys` 卷被删过，需要重新登录与重配凭据 |
