# 自带代理侧（magpie）：容器、端口、凭据与重置

适用：`docker compose up -d --build` 一键部署（compose 文件在仓库根，V2 起自带 magpie）。

**结论先说：不需要手工配任何环境变量。** 首次 `up -d` 会自动把代理侧的网关密钥生成好，
magpie 与 backend 读的是同一份；只有「你想手工接管这把密钥」时才需要填 `MAGPIE_ADMIN_KEY`。

---

## 1 容器与端口

| 服务 | 作用 | 端口 | 说明 |
| --- | --- | --- | --- |
| `web` | nginx：前端静态资源 + `/api`、`/v1` 反代到 backend | 宿主 `${APP_PORT:-80}` | 唯一对外端口 |
| `backend` | 灵枢（facade：鉴权 / 准入 / 改写 model / 计费） | 不发布 | 内网 `:8080` |
| `magpie` | 代理侧网关（选渠道 / failover / 四协议互译） | **容器内 `3425`**，不发布到宿主 | backend 用 `http://magpie:3425` |
| `magpie` 管理面 | 浏览器配置页（provider、网关密钥、用量） | 宿主 `127.0.0.1:${MAGPIE_UI_PORT:-3430}` → 容器 `3430` | 只绑回环，不经 nginx、不对公网 |
| `magpie-init` | 一次性任务：首次生成代理侧网关密钥后退出 | 无 | 先于 magpie 运行 |
| `postgres` | PostgreSQL 16 | 不发布 | 数据在 `pgdata` 卷 |

管理面地址（本机浏览器）：

```
http://127.0.0.1:3430/?k=<MAGPIE_WEB_KEY>
```

- `MAGPIE_WEB_KEY` 没设时，每次重启 magpie 会换一个新口令，链接在 `docker compose logs magpie` 里（搜 `?k=`）。
  建议固定一次：`echo "MAGPIE_WEB_KEY=$(openssl rand -hex 16)" >> .env` 然后 `docker compose up -d magpie`。
- 网关 `3425` 刻意不发布到宿主：外部要调这一层，应走灵枢的 `/v1`（有鉴权与计费）。
  需要在本机临时验证网关时用 `docker compose exec magpie curl -s localhost:3425/v1/models -H "Authorization: Bearer <key>"`。

---

## 2 凭据都在哪

| 凭据 | 用途 | 存放位置 | 能否再取回 |
| --- | --- | --- | --- |
| **代理侧管理密钥** | backend 拉 `/v1/models`、`/v1/magpie/usage/requests`（拓扑 + 账本） | Docker 卷 `magpie_secret` 里的 `admin_key`；容器内 `/run/magpie-secret/admin_key`（backend 只读挂载） | 能。`docker compose exec -u 0 backend cat /run/magpie-secret/admin_key` |
| 管理面口令 `MAGPIE_WEB_KEY` | 浏览器管理面的登录口令 | 你在 `.env` 里写的那一份；未写则由 magpie 每次启动生成 | 写了就能查 `.env`；没写看 `docker compose logs magpie` |
| 租户调用密钥（callerKey，一令牌一把） | backend 转发时逐请求注入给 magpie | magpie 的 `/config` 卷（`magpie_config`）；灵枢侧只存密文于 `magpie_keys` 表 | 不能。magpie 只在创建时显示一次（列表只显示后 4 位） |
| 上游 provider 的 API key | magpie 打真实上游 | `magpie_config` 卷（`providers.json`，**明文**） | 不能 |

因此：`magpie_config` 卷等同于密钥库，**不许**只读挂给别的容器、不许进备份包外传；
`docker compose down -v` 会连它一起删（等于把上游凭据与所有租户密钥一起丢掉）。

### 管理密钥是怎么自举的（为什么不需要你填）

1. `magpie-init` 用**同一个镜像、同一个 `/config` 卷**，在网关启动前跑一次
   `magpie gateway-key add lingshu-backend`；
2. 这个命令打印出的 `sk-magpie-key-…` 被写进共享卷 `magpie_secret:/secret/admin_key`（`chmod 600`）；
3. `backend` 的启动脚本发现 `MAGPIE_ADMIN_KEY` 为空，就从只读挂载的同一份文件里取值，
   写进 `config.yaml` 的 `magpie.admin_key`，并把 `UPSTREAM_ADMIN_KEY`（账本）也对齐成同一把；
4. 之后每次 `up -d`，`magpie-init` 发现文件已存在就直接复用 —— 两边永远是同一把。

所以「值」是 magpie 自己签发的（它的密钥库只存掩码，无法反查），我们不去伪造它 —— 这也是
`MAGPIE_ADMIN_KEY` 保持可选项、而不是必填项的原因。

### 还需要人工做的两件事（一次性）

1. **在管理面加 provider**（上游真实凭据），否则转发会返回上游错误。
2. **给每个租户令牌建一把 magpie 网关密钥**，并绑到灵枢：
   管理面 `Gateway → Gateway keys → Add`（一把令牌一把，命名建议与令牌同名），
   然后 `PUT /api/v1/admin/tokens/{id}/magpie-key`（管理端「令牌」页有对应操作，也可以直接调接口）。
   未绑定的令牌调用 `/v1` 会得到 `403 magpie_key_missing`。

---

## 3 首次启动自检

```bash
docker compose up -d --build
docker compose ps                        # magpie 应为 healthy；magpie-init 为 exited (0)
docker compose logs magpie-init          # 期望：已创建网关密钥 lingshu-backend 并写入 /secret/admin_key
docker compose logs magpie | grep '?k='  # 没固定 MAGPIE_WEB_KEY 时，管理面链接在这里
docker compose exec -u 0 backend printenv MAGPIE_BASE_URL     # http://magpie:3425
docker compose exec -u 0 backend sh -c 'echo ${MAGPIE_ADMIN_KEY:0:12}…'   # 与卷里那份一致
docker compose exec magpie magpie gateway-key list           # 名字在、状态 enabled（值是掩码）
```

拓扑镜像默认 10 分钟同步一次；首轮若因网关刚起而失败，可手动触发：
`curl -X POST http://<host>/api/v1/admin/topology/sync -H "Authorization: Bearer <管理端 token>"`。

---

## 4 重置 / 更换管理密钥

### 4.1 推荐：让 init 重新生成一把

```bash
# ① 管理面 Gateway → Gateway keys：删掉（或改名保留）lingshu-backend
# ② 清掉卷里的旧值
docker compose run --rm --entrypoint sh magpie-init -c 'rm -f /secret/admin_key'
# ③ 重建（init 会新建同名 key，backend 重启后生效）
docker compose up -d && docker compose restart backend
docker compose logs magpie-init          # 期望：已创建网关密钥 lingshu-backend
```

第 ② 步不能省：文件还在时 init 会按设计直接复用旧值，不会去碰 magpie 里的 key。

### 4.2 手工接管：用你自己指定的值

管理面 `Gateway → Gateway keys → Add` 里有一栏 *Your own key (optional)*（8–256 字符、不含空格、
不以 `sk-magpie-` 开头）。建好后写进 `.env`：

```
MAGPIE_ADMIN_KEY=<你填的那把>
```

`docker compose up -d` 之后 `magpie-init` 会以环境变量为准（不自动生成），backend 也用同一把。
注意 `.env` 里的 `MAGPIE_ADMIN_KEY` 同时会传给 `magpie-init` 和 `backend`，两处不会打架。

### 4.3 只是忘了管理面口令

`docker compose logs magpie | grep '?k='` 取当前链接；要固定就按 §1 写 `MAGPIE_WEB_KEY` 后重建 magpie。

---

## 5 升级

| 变更 | 怎么做 |
| --- | --- |
| 只升代理侧 | 改 `MAGPIE_IMAGE`（或 compose 里的默认 tag）→ `docker compose pull magpie magpie-init && docker compose up -d` |
| 只升灵枢 | 改镜像 tag（或 `--build`）→ `docker compose up -d`；**数据库迁移由后端启动时自动执行**，不需要手工跑 |
| 一起升 | `docker compose pull && docker compose up -d --build` |

升级不需要新增或修改任何环境变量；`magpie_secret` 与 `magpie_config` 两个卷保留即可，
密钥在升级后继续有效。如果新版本改了口令/密钥格式导致 401，按 §4.1 重建一把即可。

> magpie 官方 tag：`latest`（最新）/ `stable`（一周一版、先放三天）/ `daily` / 具体版本号（如 `0.1.1150`）。
> 生产建议钉具体版本或 `stable`，避免 `latest` 一天多变。国内网络拉不到 ghcr 时用
> `MAGPIE_IMAGE=<你的镜像源>/yetone/magpie:<tag>` 覆盖。

灵枢侧不需要跟着改：`MAGPIE_BASE_URL` 默认值就是 `http://magpie:3425`，容器名不变就永远对得上。

---

## 6 常见故障

| 现象 | 原因与处置 |
| --- | --- |
| `magpie-init` 日志「自动创建网关密钥失败」 | 该版本 CLI 输出变了或名字重复。按日志里的指引：先删同名 key 重跑 `up -d`，或在管理面建 key 后写 `MAGPIE_ADMIN_KEY` |
| 转发返回 `403 magpie_key_missing` | 该租户令牌还没绑 magpie 网关密钥（§2 人工两件事之二） |
| 转发返回 401 / 上游报错 | 管理面里没加 provider，或 provider 凭据失效 |
| 拓扑页一直空、日志报 401 | 管理密钥与 magpie 里那把不一致。查 `docker compose exec -u 0 backend printenv MAGPIE_ADMIN_KEY` 与 `magpie gateway-key list`；按 §4 重置 |
| `/v1` 一律 410 | `MAGPIE_BASE_URL` 为空（被 `.env` 覆盖成空串？）。`docker compose config` 里确认它渲染成 `http://magpie:3425` |
| 管理面打不开 | 端口只在回环：必须从宿主机浏览器访问 `127.0.0.1:3430`；远程请用 `ssh -L 3430:127.0.0.1:3430 <host>` |

---

## 7 与旧部署的差异（须知）

- 旧 compose 不传 `MAGPIE_*`，`/v1` 一直是 410（转发停用）；现在默认接上自带的 magpie。
- 新增两个卷：`magpie_config`（代理侧全部配置与凭据）、`magpie_secret`（只放一把管理密钥）。
- `docker compose down -v` 的破坏范围随之扩大：数据库、后端密钥、**代理侧 provider 凭据与所有租户密钥**。
