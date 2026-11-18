# 自带代理侧（magpie）：容器、端口、管理台入口与凭据

适用：`docker compose up -d --build` 一键部署（compose 文件在仓库根，V2 起自带 magpie）。

**结论先说：不需要手工配任何环境变量。** 首次 `up -d` 会自动把代理侧的网关密钥
（`admin_key`）与管理台口令（`admin_web_key`）生成好并落在卷里；magpie、backend、nginx
读的是同一份。只有「你想手工接管这两把」时才需要填 `MAGPIE_ADMIN_KEY` / `MAGPIE_WEB_KEY`。

---

## 1 容器与端口

| 服务 | 作用 | 端口 | 说明 |
| --- | --- | --- | --- |
| `web` | nginx：前端静态资源 + `/api`、`/v1` 反代 + **`/magpie/` 管理台反代** | 宿主 `${APP_PORT:-80}` | 唯一对外端口 |
| `backend` | 灵枢（facade：鉴权 / 准入 / 改写 model / 计费） | 不发布 | 内网 `:8080` |
| `magpie` | 代理侧网关（选渠道 / failover / 四协议互译） | **容器内 `3425`**，不发布 | backend 用 `http://magpie:3425` |
| `magpie` 管理台 | 浏览器配置页（provider、网关密钥、用量） | **容器内 `3430`**，正常不发布 | 对外入口是灵枢的 `/magpie/`（§2.3） |
| `magpie` 管理台（应急） | 同上，绕过 nginx 直连 | 宿主 `127.0.0.1:${MAGPIE_UI_PORT:-3430}` | break-glass，仍需管理台口令 |
| `magpie-init` | 一次性任务：生成网关密钥 + 管理台口令后退出 | 无 | 先于 magpie 运行 |
| `postgres` | PostgreSQL 16 | 不发布 | 数据在 `pgdata` 卷 |

### 两个端口不能混（这是最容易踩的坑）

| 端口 | 谁在听 | 有什么 | 没什么 |
| --- | --- | --- | --- |
| `3425` | 网关 | `/v1/*`（OpenAI/Responses/Messages/Gemini 路由）、`/mcp`、健康检查 | **没有管理页面**：拿它当管理面反代会 404 |
| `3430` | `magpie web` | 管理页面 + 页面自己的 `/api/**` + 登录表单 | 页面自己占了根路径 `/api`，**不能挂在子路径 `/magpie/` 上直接跑**（§2.3 的改写就是为此） |

页面端口也会应答 `/v1`，但那条路由上的请求**永远不被当成「本地请求」**（magpie
`internal/gateway/oneport.go` 的 `viaPagePort`），即使开了 `MAGPIE_TRUST_PROXY` 也一样。

---

## 2 管理台怎么用

### 2.1 日常：灵枢「系统 → 上游（V2）」页的按钮

1. 管理员登录灵枢 → 系统 → 上游（V2） → **打开代理侧管理台**；
2. 前端调 `POST /api/v1/admin/ops/magpie-access`：后端校验会话是 `ADMIN`，
   签一张 8 小时的票据，用 **HttpOnly cookie `magpie_gate`**（`Path=/magpie/`）种到浏览器，
   回同源路径 `/magpie/`；
3. 浏览器打开 `/magpie/`：nginx 先 `auth_request` 回灵枢验签（验不过 403，请求到不了 magpie），
   再把管理台口令**在服务端**拼进上游 URL；magpie 换成它自己的页面 cookie 后重定向（真机实测
   **303**，旧版是 302）回 `/magpie/`，**浏览器地址栏里从来没有口令**。

票据过期（8 小时）或换了 `keys/magpie_jump.key` 之后，页面会变成 403：回灵枢再点一次按钮即可。
非管理员看不到按钮，直接访问 `/magpie/` 也是 403。

### 2.2 应急：break-glass 直连

nginx 挂了、或者要排查「是不是反代改写的问题」时，从**宿主机**（或 `ssh -L`）直连：

```
http://127.0.0.1:${MAGPIE_UI_PORT:-3430}/?k=<管理台口令>
```

口令取值：`docker compose exec -u 0 web cat /run/magpie-secret/admin_web_key`
（它就是 `magpie_secret` 卷里的 `admin_web_key`；`.env` 里手填的 `MAGPIE_WEB_KEY` 会在首次 up 时写进
这个文件，**以文件为准** —— 手填值不足 16 位时会被补齐，按日志里提示的那份读）。
这条路径**没有灵枢的 ADMIN 校验**，只认口令本身 —— 别把链接发给别人，用完即弃。
不需要 break-glass 就从 compose 里删掉那行 `ports`。

### 2.3 `/magpie/` 子路径反代做了什么（为什么需要这些改写）

magpie 的管理页面是按「自己占着域名根」写的，挂到子路径上要补三件事（实现都在
`frontend/nginx.conf`，自检脚本 `frontend/scripts/magpie-ui-proxy-selftest.sh`）：

| magpie 的原行为 | 不处理的后果 | 我们的处置 |
| --- | --- | --- |
| 页面 JS 里写死 `fetch("/api/"+path)`（绝对路径） | 点任何东西都打到灵枢的 `/api`，一路 401 | `sub_filter` 把 `"/api/` 改写成 `"/magpie/api/`（因此上游必须关 gzip，由 nginx 重新压缩） |
| `?k=` 换完 cookie 后重定向到 `/`（真机返回 **303**，旧版 302） | 一跳就跳出管理台 | `proxy_redirect` 把 `Location: /…` 改回 `/magpie/…`（nginx 对 301/302/303/307/308 一视同仁） |
| 页面自己的写请求校验 `Origin` / `Sec-Fetch-Site` | `Host` 被改成 `magpie:3430` 时所有写请求 403 | `proxy_set_header Host $http_host` 原样转发 |

页面口令的注入是**每次请求动态判断**的：浏览器还没有 magpie 自己的页面 cookie（`magpie_web_3430`）、
且没有手动带 `?k=` 时，才由 nginx 代填一次；有 cookie 就直接走，不会再带口令。

### 2.4 管理台里还要人工做的两件事（一次性）

1. **加 provider**（上游真实凭据），否则转发会返回上游错误；
2. **给每个租户令牌建一把 magpie 网关密钥**，并绑到灵枢：
   管理台 `Gateway → Gateway keys → Add`（一把令牌一把，命名建议与令牌同名），
   然后 `PUT /api/v1/admin/tokens/{id}/magpie-key`（管理端「令牌」页有对应操作，也可以直接调接口）。
   未绑定的令牌调 `/v1` 会得到 `403 magpie_key_missing`。

---

## 3 凭据都在哪

| 凭据 | 用途 | 存放位置 | 能否再取回 |
| --- | --- | --- | --- |
| **代理侧网关密钥** `admin_key` | backend 拉 `/v1/models`、`/v1/magpie/usage/requests`（拓扑 + 账本） | `magpie_secret` 卷 `/secret/admin_key`；backend 只读挂载到 `/run/magpie-secret/` | 能。`docker compose exec -u 0 backend cat /run/magpie-secret/admin_key` |
| **管理台口令** `admin_web_key` | 打开 magpie 管理页面 | 同一卷 `/secret/admin_web_key`；magpie 与 web 都只读挂载 | 能。`docker compose exec -u 0 web cat /run/magpie-secret/admin_web_key` |
| **灵枢管理台票据密钥** `keys/magpie_jump.key` | 签 `/magpie/` 的访问票据（HMAC） | backend 的 `backend_keys` 卷；首次启动自动生成（32 字节 hex，`chmod 600`） | 能，但没必要；删掉即作废所有已发出的票据 |
| 租户调用密钥（callerKey，一令牌一把） | backend 转发时逐请求注入给 magpie | magpie 的 `/config` 卷（`magpie_config`）；灵枢侧只存密文于 `magpie_keys` 表 | 不能。magpie 只在创建时显示一次（列表只显示后 4 位） |
| 上游 provider 的 API key | magpie 打真实上游 | `magpie_config` 卷（`providers.json`，**明文**） | 不能 |

因此：`magpie_config` 卷等同于密钥库，**不许**只读挂给别的容器、不许进备份包外传；
`docker compose down -v` 会连它、`magpie_secret`、`pgdata` 一起删。

> 代价要说清：`web` 容器为了在服务端代填口令，只读挂了 `magpie_secret`。
> 谁能 `exec` 进 web 容器，谁就能读到管理台口令（等于能开代理侧管理台）。

### 3.1 三把东西是怎么自举的

1. `magpie-init`（同镜像、`user: 0:0`、同 `/config` 卷）在网关启动前跑一次：
   - 网关密钥：`magpie gateway-key add lingshu-backend`，从输出里抓 `sk-magpie-key-…` 写 `/secret/admin_key`；
   - 管理台口令：`openssl rand -hex 16` 写 `/secret/admin_web_key`（32 字符，只含 `0-9a-f`）；
     手填（`.env` 的 `MAGPIE_WEB_KEY`）优先，但**不足 16 位会被追加随机 hex 补齐**并打警告——
     magpie 对长度有硬要求，短口令让它根本起不来；
   - 两个文件都 `chown 65532` + `chmod 600`（magpie 以 uid 65532 运行）。
2. `magpie` 的启动脚本在 `exec` 之前把 `/secret/admin_web_key` 读进 `MAGPIE_WEB_KEY`
   （magpie 只认环境变量，没有「从文件读口令」的开关），所以**重启不会换口令**。
3. `backend` 启动脚本发现 `MAGPIE_ADMIN_KEY` 为空，就从只读挂载取值，写进 `config.yaml` 的
   `magpie.admin_key`，并把 `UPSTREAM_ADMIN_KEY`（账本）对齐成同一把；
   同时第一次生成 `keys/magpie_jump.key`（管理台票据密钥）。
4. `web` 启动脚本把 `/secret/admin_web_key` 编译成一个 nginx `map`（`/etc/nginx/conf.d/00-magpie-key.conf`），
   供 `/magpie/` 入口代填。

所以「值」是 magpie 自己签发的（它的密钥库只存掩码，无法反查），我们不去伪造它 ——
这也是 `MAGPIE_ADMIN_KEY` 保持可选项的原因。

---

## 4 首次启动自检

```bash
docker compose up -d --build
docker compose ps                        # magpie 应为 healthy；magpie-init 为 exited (0)
docker compose logs magpie-init          # 期望：已生成管理面口令 / 已创建网关密钥
docker compose logs web | head           # 期望：/magpie/ 入口已注入管理面口令
docker compose exec -u 0 backend printenv MAGPIE_BASE_URL     # http://magpie:3425
docker compose exec magpie magpie gateway-key list            # 名字在、状态 enabled（值是掩码）
```

然后打开灵枢「系统 → 上游（V2）」，点「打开代理侧管理台」：应当直接进管理页面，
地址栏是 `<灵枢地址>/magpie/`（**没有** `?k=`）。用开发者账号登灵枢时不该看到这个按钮。

拓扑镜像默认 10 分钟同步一次；首轮若因网关刚起而失败，可手动触发：
`curl -X POST http://<host>/api/v1/admin/topology/sync -H "Authorization: Bearer <管理端 token>"`。

---

## 5 重置 / 更换

### 5.1 管理台口令（`admin_web_key`）

```bash
docker compose run --rm --entrypoint sh magpie-init -c 'rm -f /secret/admin_web_key'
docker compose up -d && docker compose restart magpie web
docker compose logs magpie-init          # 期望：已生成管理面口令
```

三方（magpie 进程 / nginx 的 map / 卷）都从这一份来，所以换完不需要改任何配置。

### 5.2 网关密钥（`admin_key`）

```bash
# ① 管理台 Gateway → Gateway keys：删掉（或改名保留）lingshu-backend
# ② 清掉卷里的旧值
docker compose run --rm --entrypoint sh magpie-init -c 'rm -f /secret/admin_key'
# ③ 重建（init 会新建同名 key，backend 重启后生效）
docker compose up -d && docker compose restart backend
docker compose logs magpie-init          # 期望：已创建网关密钥 lingshu-backend
```

第 ② 步不能省：文件还在时 init 按设计直接复用旧值，不会去碰 magpie 里的 key。

### 5.3 管理台票据密钥（`keys/magpie_jump.key`）

`docker compose exec -u 0 backend rm keys/magpie_jump.key && docker compose restart backend`
—— 所有已发出的票据立即失效（所有人回灵枢再点一次按钮），不影响转发与计费。

### 5.4 手工接管（用你自己指定的值）

管理台 `Gateway → Gateway keys → Add` 有一栏 *Your own key (optional)*（8–256 字符、
不含空格、不以 `sk-magpie-` 开头）。建好后写进 `.env`：

```
MAGPIE_ADMIN_KEY=<你填的那把>
MAGPIE_WEB_KEY=<你自己定的管理台口令>      # 可只填这一项，另一项继续自动；**至少 16 个字符**
```

`docker compose up -d` 之后 `magpie-init` 以环境变量为准（不自动生成），backend / magpie / web
都用同一把。注意两条 magpie 的硬规则：

- **长度 ≥16 字符**（真机实测 13 位直接起不来）。手填不足 16 位时 `magpie-init` 会追加随机 hex
  补齐并打警告，补齐后的值只存在 `/secret/admin_web_key` 里 —— 别按 `.env` 里的原值手输，
  要用 `docker compose exec -u 0 web cat /run/magpie-secret/admin_web_key` 取。
- 只接受字母数字与 `- . _ ~`（其它字符 nginx 会拒绝注入并在 `docker compose logs web` 里报警，
  此时管理台需手工粘口令）。

---

## 6 升级

| 变更 | 怎么做 |
| --- | --- |
| 只升代理侧 | 改 `MAGPIE_IMAGE`（或 compose 里的默认 tag）→ `docker compose pull magpie magpie-init && docker compose up -d` |
| 只升灵枢 | 改镜像 tag（或 `--build`）→ `docker compose up -d`；**数据库迁移由后端启动时自动执行**，不需要手工跑 |
| 一起升 | `docker compose pull && docker compose up -d --build` |

升级不需要新增或修改任何环境变量；`magpie_secret` 与 `magpie_config` 两个卷保留即可，
两把密钥升级后继续有效。若新版本改了口令/密钥格式导致 401，按 §5.1 / §5.2 重建。

> magpie 官方 tag：具体版本号（如 `0.1.4458`，**不带 `v` 前缀** —— 写成 `v0.1.4458` 会拉不到镜像）、
> `latest`（最新）/ `stable`（一周一版、先放三天）/ `daily`。compose 默认已钉 `0.1.4458`；
> 想跟新就用 `stable`，别用 `latest`（一天多变）。国内网络拉不到 ghcr 时用
> `MAGPIE_IMAGE=<你的镜像源>/yetone/magpie:<tag>` 覆盖。

灵枢侧不需要跟着改：`MAGPIE_BASE_URL` 默认值就是 `http://magpie:3425`，容器名不变就永远对得上。

---

## 7 常见故障

| 现象 | 原因与处置 |
| --- | --- |
| 点按钮后新页 403 | 票据过期/被轮换：回灵枢再点一次。若一直 403，查 `docker compose logs backend` 有无 `签发代理侧管理面票据` |
| 管理台打开后是 magpie 的登录表单（要口令） | nginx 没代填（`docker compose logs web` 应出现「未取到管理面口令」），或 magpie 页面 cookie 名与配置里的 `magpie_web_3430` 不一致（改了页面端口就要同步改 `frontend/00-magpie-key.conf` 与 `19-magpie-key.sh`） |
| 管理台页面能开，但点任何东西都报错/一片 401 | `sub_filter` 没生效：多半是 nginx 之外又加了一层 gzip/压缩代理，或者 magpie 改了 JS 里的路径写法 → 跑 `bash frontend/scripts/magpie-ui-proxy-selftest.sh` 定位 |
| 管理台写操作全部 403（页面能看不能改） | `Host` 没原样转发（magpie 用 `Origin` vs `Host` 挡跨站写）：确认 `proxy_set_header Host $http_host` |
| 点按钮提示「代理侧还没接入」 | 后端 `MAGPIE_BASE_URL` 为空（`/v1` 也是 410）。`docker compose config` 里确认它渲染成 `http://magpie:3425` |
| `magpie` 起不来、日志说页面口令太短 | `MAGPIE_WEB_KEY` 不足 16 字符（magpie 硬要求）。清空 `.env` 里这一项后 `docker compose up -d`，让 `magpie-init` 重新生成（或直接写 ≥16 位）；日志里出现「已自动补齐」就说明用的是补齐后的值 |
| `magpie-init` 日志「自动创建网关密钥失败」 | 该版本 CLI 输出变了或名字重复。先删同名 key 重跑 `up -d`，或在管理台建 key 后写 `MAGPIE_ADMIN_KEY` |
| 转发返回 `403 magpie_key_missing` | 该租户令牌还没绑 magpie 网关密钥（§2.4 之二） |
| 转发返回 401 / 上游报错 | 管理台里没加 provider，或 provider 凭据失效 |
| 拓扑页一直空、日志报 401 | 管理密钥与 magpie 里那把不一致。查 `docker compose exec -u 0 backend printenv MAGPIE_ADMIN_KEY` 与 `magpie gateway-key list`；按 §5.2 重置 |
| `/v1` 一律 410 | `MAGPIE_BASE_URL` 为空（被 `.env` 覆盖成空串？） |
| 管理台打不开且 nginx 也挂了 | 用 break-glass 直连 §2.2（注意那是**没有** ADMIN 校验的通道） |

---

## 8 与旧部署的差异（须知）

- 旧 compose 不传 `MAGPIE_*`，`/v1` 一直是 410（转发停用）；现在默认接上自带的 magpie。
- 新增两个卷：`magpie_config`（代理侧全部配置与凭据）、`magpie_secret`（两根密钥：网关密钥 + 管理台口令）。
- 新增同源入口 `/magpie/`：**只有 ADMIN** 能从灵枢进去；它反代的是管理页面，不是网关。
- `docker compose down -v` 的破坏范围随之扩大：数据库、后端密钥、**代理侧 provider 凭据与所有租户密钥**。
