# 灵枢 LingShu

> 天有璇玑，灵在其中；众渠归一，枢而控之。

**灵枢（LingShu）** 是一套面向「渠道 / 模型 / 令牌 / 计费」全链路的 AI 网关与资产管理平台。
它以 OpenAI 兼容协议的**统一网关**为入口，将多只上游渠道（供应商）包装成单一的调用平面，
并按**令牌语义标签**智能路由；内置国密加密、JWT 鉴权、五段计费、会话亲和与健康探测，
附完整的管理端 / 开发端前端页面。

## 功能列表

- **身份与令牌**：管理员 / 开发者两种角色；JWT（RS256）鉴权；API Key（明文 HMAC→SM3 落库，只展示一次）
- **渠道管理**：OpenAI-compat 上游对接，凭据 SM4-GCM 加密存储，超时 / 限流 / 健康探测与状态机（HEALTHY / DRAIN_ONLY / UNAVAILABLE）；一个渠道可挂多把密钥
- **模型映射**：对外模型名 → 内部模型 ID 全局单射，含一套五段计价费率（sale / cost 双口径）
- **语义路由**：令牌绑定的语义标签（KV）严格匹配渠道标签 → 会话亲和 → 优先级分层、同层加权轮询
- **计费**：五段 token 计费（input / output / cache_read / cache_write / reasoning）× 时段系数 × 上下文分档 ÷ R；调用前预估预扣、调用后多退少补；账单拆解三步展示，钱包积分乐观锁扣减（余额不足 402）
- **控制台**：管理员数据看板（用户 / 渠道 / 账单 / 会话 / 统计 / 标签 / 汇率同步 watchlist），开发者用量、账单、钱包与令牌自助
- **价格同步**：定时拉取外部价格源（models.dev/api.json）并与 watchlist 比对产生告警

## 架构

后端 Go（net/http，Go 1.22+ 路由 Pattern），分层：`internal/domain/*`（各领域）+
`internal/server`（装配 / 中间件）+ `internal/db`（迁移）+ `internal/pkg/*`
（jwt / logger / resp / crypto / decimalx）。前端 Vue 3 + Element Plus + Vite，代理到后端 `/api`。

```
.
├── Makefile                  # 常用任务：up/down/migrate/run/test/keys/e2e/deploy
├── docker-compose.yml        # 一键部署：web + backend + postgres（对外单端口）
├── backend/                  # Go 后端
│   ├── Dockerfile            # 多阶段构建 → 静态二进制
│   ├── docker-entrypoint.sh  # 自动生成密钥、渲染配置、启动（含自动迁移）
│   ├── cmd/server/           # 入口
│   ├── config.example.yaml   # 配置样例（本地/裸机运行用）
│   └── internal/             # domain / server / db / pkg / seeding
└── frontend/                 # Vue3 前端
    ├── Dockerfile            # Node 构建 → nginx 托管
    └── nginx.conf            # 同端口反代 /api、/v1 到后端
```

## 环境要求

| 依赖 | 版本 |
|------|------|
| Go | 1.22+ |
| Node.js | 18+ |
| PostgreSQL | 16（可用 Docker 或系统 / 本地 PG） |
| OpenSSL | 生成 RSA 密钥对（`make keys` 用到） |

> 本机无 Docker 时，直接用系统 PostgreSQL 建库即可，并把 `TEST_DATABASE_URL` 指向待建的独立测试库（见「e2e 测试」）。

## Docker 一键部署

前置：安装 Docker 与 Docker Compose v2。**无需任何配置文件**，一条命令即可起全栈：

```bash
docker compose up -d --build
```

默认访问 **`http://<主机IP>:80`**，首登 `admin / admin123`。

### 容器编排

| 服务 | 说明 | 对外端口 |
|------|------|---------|
| `web` | nginx：托管前端静态资源，并把 `/api`、`/v1` 反代到后端 | 唯一对外端口（默认 80） |
| `backend` | Go 服务，容器内 `:8080` | 仅内网 |
| `postgres` | PostgreSQL 16，数据持久化在 `pgdata` 卷 | 仅内网 |

前端与后端**共用同一端口**：管理端接口 `/api/v1/*`、OpenAI 兼容网关 `/v1/*` 与前端静态页面都在该端口下，无需额外跨域配置。

### 自动化行为（开箱即用）

- **数据库迁移 + 初始管理员种子**：后端启动时自动执行，无需手工跑迁移。
- **配置渲染**：容器启动脚本按环境变量生成 `config.yaml`（数据库 DSN 默认指向 compose 内的 postgres）。
- **密钥自动生成并持久化**：首次启动生成 JWT RSA 密钥对与强随机 SM4 密钥，存于 `backend_keys` 卷；
  后续重启复用（重建该卷会导致已签发令牌失效、已加密的渠道凭据无法解密）。
- **账单兜底队列**：落库失败记录写入 `backend_data` 卷，不丢账单。

### 可覆盖的环境变量

在项目根目录创建 `.env`（已被 git 忽略）或直接 export 即可，均有默认值：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `APP_PORT` | `80` | 对外唯一端口 |
| `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` | `llmgw` | 数据库账号与库名 |
| `SM4_KEY` | 空（自动生成） | 32 位 hex；如需固定密钥（多环境共用渠道凭据）可显式指定 |
| `TZ` | `Asia/Shanghai` | 计费时段系数按时区计算 |
| `JWT_TTL_MINUTES` | `360` | 登录令牌有效期（分钟） |
| `SYNC_INTERVAL_MINUTES` | `60` | 外部价格源同步周期 |
| `TRUSTED_PROXY_CIDRS` | `172.16.0.0/12`（compose 内设定） | 可信代理网段（逗号分隔 CIDR）。仅当直连对端属于这些网段时才采信 `X-Real-IP`，用于登录限流与审计的客户端 IP。**代码默认只信任回环**；若实际 Docker 网络不在默认段（可用 `docker network inspect <项目>_default` 查看），必须显式配置，否则审计 IP 会退化为 nginx 容器 IP、限流的 IP 维度也会退化。启动日志会打印生效网段 |

> 生产环境请修改 `POSTGRES_PASSWORD` 与 `APP_PORT`，并在首次登录后立即改掉默认口令。

### 常用运维命令

```bash
docker compose logs -f backend   # 查看后端日志
docker compose ps                # 查看容器状态
docker compose restart backend   # 重启后端
docker compose down              # 停止（数据保留）
docker compose down -v           # 停止并删除数据卷（含数据库与密钥，慎用）
```

## 本地开发（裸机运行）

1. **起数据库**：本机安装 PostgreSQL 后建库与用户，例如：

   ```bash
   createdb llmgw
   createdb llmgw_test   # 供 e2e 使用
   ```

   也可直接用 `docker run -d --name llmgw-pg -p 5432:5432 \
   -e POSTGRES_USER=llmgw -e POSTGRES_PASSWORD=llmgw -e POSTGRES_DB=llmgw \
   postgres:16` 起一个独立的 PostgreSQL 容器。

2. **生成 RSA 密钥**到 `backend/keys/`：

   ```bash
   make keys
   ```

3. **准备配置**（`make migrate` / `make run` 会自动从 `config.example.yaml` 复制 `config.yaml`，按需改 DSN）：

   ```bash
   cp backend/config.example.yaml backend/config.yaml
   # 按需编辑 backend/config.yaml 的 database.dsn
   ```

   > **`security.sm4_key` 渠道凭据加密密钥**（32 位 hex，16 字节）：**留空即自动生成**——启动时从
   > `keys/sm4.key` 读取；文件不存在则生成强随机密钥并写入该文件（`chmod 600`），后续重启复用。
   > 因此**不提供公开默认密钥**，也无需为本地开发手工填值。
   > 若多环境需共用同一密钥（例如共享已加密的渠道凭据），用 `openssl rand -hex 16` 生成后显式配置。

4. **执行迁移**：`make migrate`

5. **起后端**（`:8080`）：`make run`

6. **起前端**（`:5173`，proxy 到 8080）：

   ```bash
   cd frontend && npm install && npm run dev
   ```

7. **首次登录**：`admin / admin123`。**首次登录后务必立即修改默认口令。**

## 默认账号与安全说明

- 初始管理员：`admin / admin123`（首次登录后请立即改密）
- 口令采用 **SM3 加盐哈希** 存储
- JWT 使用 **RS256**（非对称，私钥仅服务端持有）
- 渠道供应商凭据采用 **SM4-GCM** 加密落库，前端仅展示 `****`
- API Key 明文只返回一次，库中仅存 SM3 哈希

## e2e 测试（守卫式）

`backend/tests/e2e_test.go` 为端到端冒烟：覆盖登录 → 建开发者用户 → 充值 → 建渠道（httptest 假上游）→
建模型映射 / 语义标签 → 建令牌 → 网关转发 → 计费扣积分 → 账单一致，含 503（标签不匹配）与 402（余额不足）两个负路径。

- **默认跳过**：`go test ./...` 在未设置 `TEST_DATABASE_URL` 时自动 skip，保证无 DB 环境全绿。
- **启用运行**（建议指向**独立测试库**，会 `DROP SCHEMA` 重建，切勿指向业务库）：

  ```bash
  cd backend
  TEST_DATABASE_URL='postgres://llmgw:llmgw@127.0.0.1:5432/llmgw_test?sslmode=disable' \
      go test ./tests/ -v
  ```

- 也可用 Makefile：`make e2e`（复用 `TEST_DATABASE_URL` 环境变量）。

## 目录 / 模块说明

| 模块 | 说明 |
|------|------|
| `backend/internal/domain/identity` | 用户、钱包 / 积分流水、令牌、公告 |
| `backend/internal/domain/channel` | 渠道 CRUD、密钥管理、凭据加解密、运行时管理、状态机、限流器 |
| `backend/internal/domain/tag` | 语义标签 KV 与渠道绑定 |
| `backend/internal/domain/model` | 模型映射（对外名 → 内部名）与费率 |
| `backend/internal/domain/router` | 模型解析 → 标签匹配 → 会话亲和 → 优先级 / 加权路由 |
| `backend/internal/domain/billing` | 五段计费、预估预扣、上下文分档、时段系数、账单记账、失败重试与统计 |
| `backend/internal/domain/gateway` | 网关热路径：认证 → 路由 → 转发 → 计费（openai-compat 上游适配） |
| `backend/internal/domain/console` | 管理端 / 开发端控制台查询、看板聚合与账单拆解 |
| `backend/internal/domain/sync` | 外部价格源拉取与 watchlist 告警 |
| `backend/internal/server` | HTTP 路由、鉴权 / 日志 / 恢复 / request-id 中间件 |
| `backend/internal/db` | PG 连接池与 `go:embed` 迁移 |
| `backend/internal/seeding` | 初始管理员 + 默认计费配置（幂等） |
| `frontend/` | Vue3 + Element Plus 管理 / 开发双端界面 |

## License

[MIT](./LICENSE)
