# 代理侧（magpie）随 compose 自举凭据 + 管理面一键跳转

日期：2026-11-18　作者：Core　状态：**待确认（未动代码）**

---

## 0 结论先说

1. **A（自举凭据）可行，且比现状更稳**：改为「先 `openssl rand -hex 32` 造值 → `magpie gateway-key add admin --key -` 从 stdin 注册 → 成功后原子落卷」，
   不再解析 CLI 的打印输出；`magpie gateway-key add <name> --key -` 是官方文档里明写的用法（见 §1.1）。
2. **B 的推荐方案 B1（`/magpie/` 子路径 + auth_request + `MAGPIE_TRUST_PROXY=1`）按原样做不出来**，三个各自独立的硬原因（§3.2）：
   目标端口错、子路径会把页面自身的 `/api` 撞掉、nginx 拿不到灵枢的会话（Bearer 在 localStorage）。
   另外 `MAGPIE_TRUST_PROXY=1` **不解决页面要口令这件事**，它只作用于网关的客户端路由（§1.3）。
3. **可行的替代 = B1′**：把管理面放到**独立端口**（root 挂载，不撞路径），入口用**一次性票据**（灵枢 ADMIN 会话换取），
   nginx 校验后**服务端注入 page key**，浏览器永远看不到任何 key。代价是 web 容器持有 page key（§3.5），
   以及新增约 150 行后端代码、一个前端按钮、一段 nginx server 块。
4. **升级不需要人工加任何变量**（§4）：所有凭据首次 `up -d` 自动生成并落在卷里，已有 `.env` 里的手工值仍然优先。

> 本文中 magpie 侧的所有结论均出自官方文档（`usemagpie.ai/docs/docker`、`usemagpie.ai/docs/remote`、仓库 `docs/reference.md`，对应 release 0.1.4454）。
> 沙盒里没有 docker、CLI 二进制 93 MB 且镜像站限速到几十 KB/s，**magpie 侧没有做真机实测**；落地前必须先在宿主机跑 §1.7 的 3 条命令。

---

## 1 事实核查（每条都给出处）

### 1.1 网关密钥可以先有值再注册

`reference.md`「Gateway keys」：

```sh
magpie gateway-key add "Remote laptop"   # prints the new credential once
magpie gateway-key add Family --key -    # keeps a key clients already send, read from stdin
magpie gateway-key rotate <id> --key -   # gives it a value of your own instead, read from stdin
```

自定值的校验口径（页面上的同一条规则，CLI 同源）：**8–256 个可打印 ASCII、不含空格、不以 `sk-magpie-` 开头、不能与其他网关密钥或 provider key 重复**。
`openssl rand -hex 32` 产出 64 个十六进制字符，满足全部条件。

结论：`openssl rand -hex 32` + `--key -` 是官方支持的姿势，**比现在 grep `sk-magpie-key-[…]` 的解析稳**（打印格式变了就会静默失败）。

### 1.2 两个端口的分工

`usemagpie.ai/docs/docker`：

- `3425` = 网关门（网关 API；容器默认 `magpie serve` 就是它）；
- `3430` = `magpie web` 的浏览器管理面，**它也顺带应答网关的 `/v1` 路由**，「so a reverse proxy needs only this port」。

结论：要反代管理页，目标必须是 **3430**，`http://magpie:3425/` 打开只会是 API 404。**B1 原文里写的 `proxy_pass http://magpie:3425/` 是错的。**

### 1.3 `MAGPIE_TRUST_PROXY=1` 到底管什么

`reference.md`「Providers and the gateway」：

> A request that reaches loopback through a proxy or tunnel … is someone else's … it carries a forwarding header
> (`Forwarded`, `X-Forwarded-For`, `X-Real-IP`, `Cf-Connecting-IP`, `True-Client-IP`, `Tailscale-User-Login`) …
> **A proxy here that signs its clients in itself can be trusted with `MAGPIE_TRUST_PROXY=1`, which keeps what it forwards local, as before.**

同一节关于页面端口：

> On the page's port every one of those routes is answered as a request from another machine, from loopback too …
> The page's sign-in cookie is no gateway key, and **the page and its `/api` still need the page's key**.

结论（三条，都要写进部署文档）：

1. `MAGPIE_TRUST_PROXY=1` 的作用面是**网关的客户端路由**（`/v1`、`/mcp`、配额等）：开了之后，代理转发过来的请求按本地处理（不再要求网关密钥）。
2. 它**不会**免掉管理页自己的口令（`MAGPIE_WEB_KEY`，即 `?k=`）。所以「auth_request 过了就不会看到登录页」这个预期不成立。
3. 它对待的是**请求头**：任何带 `X-Forwarded-For`/`Forwarded`/`X-Real-IP` 的请求默认就按「别处的机器」处理（这是 #1022 的判定）。
   我们把 `/v1` 反代给 magpie 时反而**绝对不该**开它——那等于给任意能访问 nginx 的人免掉网关密钥。

**本设计不设 `MAGPIE_TRUST_PROXY`。** 记一条：以后若有人把 `/v1` 也挂到 magpie 上，这条开关就是静默放开鉴权的坑。

### 1.4 页面口令（page key）的语义

- `MAGPIE_WEB_KEY`：16+ 字符（字母数字或 `- . _ ~`），设了之后**跨重启不变**、浏览器登录态 400 天；不设则每次启动换一把，链接打在 `docker compose logs magpie`。
- 页面打开时带 `?k=<key>`，magpie 把它换成 cookie 并从地址里去掉；不带 key 打开会被 401 挡下并给一个输入框。
- 文档反复强调：**任何拿到这个链接的人都能改 magpie、能看到上游 provider 的明文 key**。

### 1.5 唯一「免口令」的官方路径是 LazyCat 平台集成，不能用

`reference.md`「LazyCat」：`MAGPIE_WEB_SIGN_IN=lazycat` 会读平台 ingress 注入的 `X-HC-User-ID` / `X-HC-User-Role` / `X-HC-Source`，
只认 `ADMIN`，并做 `Sec-Fetch-Site` 判定；`magpie web` 在缺 `LAZYCAT_APP_DOMAIN` 时拒绝启动。

不用它的理由：它信任的是**请求头**，任何能直连 3430 的容器都能伪造；而 compose 里所有容器都在同一个网段，
没有「只让 nginx 连、别人连不上」的网络手段（除非再拆一个 network + 只挂 nginx，那属于给平台特性打补丁）。
文档里也写明这是平台专属行为。→ 记为**否决**，免得后面又有人提。

### 1.6 子路径（`/magpie/`）不可行

- `magpie web` 的启动参数只有 `--lan / --addr / --no-open / --gateway / --tls-cert / --tls-key`，**没有 base-path**。
- 页面自己的接口就占根 `/api`（文档：单一公网 URL 下「`/api` goes to `magpie web`'s page」），与灵枢的 `/api` 直接冲突；
  页面静态资源同样是根路径。挂到 `/magpie/` 后，页面资源与 XHR 会全部 404 或打到灵枢后端。
- 官方给出的反代姿势都是「一个域名/端口的根」：`https://magpie.example.com/` + `/v1` 同址。

结论：**只能独立端口或独立域名**。本文选独立端口（单机 compose 里不需要 DNS/TLS 就能用）。

### 1.7 落地前必须在宿主机实测的 3 条（沙盒做不了）

```sh
# ① --key - 能从 stdin 注册、且重复注册会报错（决定 init 的幂等分支怎么写）
printf '%s' "$(openssl rand -hex 32)" | docker compose run --rm --entrypoint sh magpie-init -lc \
  'cat > /tmp/k; magpie gateway-key add probe --key - < /tmp/k; magpie gateway-key list; magpie gateway-key add probe2 --key - < /tmp/k'

# ② 页面端口经代理后仍要口令（预期：无 cookie 时 401，带 ?k= 时 302→cookie）
curl -si http://127.0.0.1:3432/ | head -5
curl -si "http://127.0.0.1:3432/?k=$(docker compose exec -T -u 0 backend cat /run/magpie-secret/admin_web_key)" | head -5

# ③ TRUST_PROXY 的作用面（预期：只影响 3425 的客户端路由，不影响页面口令）
#    开与不开各跑一次，比较 /v1/models 的 401/200 与页面 / 的 401
```

任何一条与 §1 的结论不符 → 退回 **B2**（§3.4），其余设计不变。

---

## 2 【A】compose 自举凭据（改法）

### 2.1 与现状的差异

| 项 | 现状（分支 `feat/compose-bundled-magpie`，commit 27db85a） | 本次改法 |
| --- | --- | --- |
| 网关密钥来源 | 跑 `magpie gateway-key add <name>`，**grep 打印出的 `sk-magpie-key-…`** | `openssl rand -hex 32` 造值，`gateway-key add <name> --key -` 注册 |
| 落盘时机 | 拿到打印值后写 `admin.key` | 先写 `admin.key.tmp`，**注册成功才 `mv` 成 `admin.key`** |
| 幂等 | 文件在 → 直接复用 | 同（文件在就什么都不做） |
| 失败语义 | 软失败（exit 0 + 指引） | 保持软失败，但**绝不留下「backend 有值、magpie 不认」的状态**：注册失败就删临时文件 |
| 页面口令 | `.env` 里可选，留空则每次重启变 | **也自举**：`admin_web_key`，首次生成、之后复用 |

### 2.2 卷里的文件

```
magpie_secret/
  admin.key          # 网关密钥（backend 的 MAGPIE_ADMIN_KEY / UPSTREAM_ADMIN_KEY、magpie 的调用方）
  admin_web_key      # 管理页口令（magpie 的 MAGPIE_WEB_KEY、web 的 nginx 注入）
```

两个值**分权**：网关密钥只给 backend，页面口令只给 magpie 与 web。互不越界。

### 2.3 优先级（与现有 entrypoint 一致）

```
环境变量（.env）> 卷里已有文件 > 首次生成
```

已有部署手工设过 `MAGPIE_ADMIN_KEY` / `MAGPIE_WEB_KEY` 的，行为不变。

### 2.4 不变的东西

- `MAGPIE_BASE_URL` 默认 `http://magpie:3425`（已是默认）。
- `UPSTREAM_ADMIN_KEY` 继续与 `MAGPIE_ADMIN_KEY` 对齐成同一把（账本/拓扑打的是同一个代理侧）。
- 迁移、计费、转发主链零改动。

---

## 3 【B】管理面一键跳转

### 3.1 暴露面模型（先定这个，再谈方案）

| 凭据 | 能做什么 | 现在谁持有 |
| --- | --- | --- |
| 网关密钥（`admin.key`） | 调网关 API、拉拓扑/账本 | magpie（校验方）+ backend |
| 页面口令（`admin_web_key`） | **完全管理 magpie**：加 provider、看上游明文 key、删网关密钥 | magpie（校验方）+ **操作者浏览器**（URL/cookie） |
| 租户 callerKey（一令牌一把） | 只转发，不能管理 | magpie + backend（密文存 `magpie_keys`） |

页面口令 = 最高权限。**「key 不进浏览器」只对页面口令有意义**，这也是 B1 的出发点。

### 3.2 B1 原样为什么做不出来

1. **端口错**：管理面在 3430，`proxy_pass http://magpie:3425/` 只会拿到 API 404（§1.2）。
2. **子路径撞车**：页面与它的 `/api`、静态资源都在根上，`/magpie/` 前缀会把它们打断（§1.6）。
3. **auth_request 拿不到会话**：灵枢的登录态是 `localStorage` 里的 Bearer JWT（`frontend/src/stores/auth.ts`），
   浏览器**导航**不会带 `Authorization` 头，`auth_request` 子请求必然是 401 → 管理员也被 403。
4. **即使前三条都绕过去，页面仍然要口令**：`MAGPIE_TRUST_PROXY=1` 不免页面口令（§1.3），用户还是会被 magpie 的 401 页挡一次。

### 3.3 方案对比

| 方案 | ADMIN 门禁 | 页面口令进浏览器 | magpie 侧改动 | 暴露面 | 工作量 |
| --- | --- | --- | --- | --- | --- |
| **B0 现状**：`127.0.0.1:3430/?k=…` 手输 | 无（只靠回环 + 口令） | 手输到输入框，不进 URL | 无 | 回环 | 0 |
| **B1 原样** | — | — | — | — | **不可行** |
| **B1′ 推荐**：独立端口 + 一次性票据 + 服务端注入口令 | ✅ auth_request → 灵枢 ADMIN | ❌ 不进（浏览器只见 `?t=<120s 票据>`） | 无（不设 TRUST_PROXY） | 回环（可配 LAN/VPN） | 中 |
| **B2 备选**：系统页给一条 `?k=` 链接 | ✅ 按钮可见性（事后不可控） | ✅ 会进（历史/复制/截图） | 无 | 回环（或 LAN） | 小 |

选 **B1′**：它是 B1 想达到的效果，且不扩大网络暴露（默认仍只绑回环）。B2 作为 B1′ 的前置退路（§1.7 实测不过时用）。

### 3.4 B2（备选，一句话）

`GET /api/v1/admin/ops/magpie-access` 直接返回 `http://<host>:<MAGPIE_UI_PORT>/?k=<admin_web_key>`，前端按钮 `window.open`。
优点：半天能上线、不碰 nginx。缺点：口令进浏览器历史、可被截图/转发；链接**永不过期**。

### 3.5 B1′ 的时序

```
浏览器（管理员）                      nginx(web:3431)                backend                    magpie:3430
   │ 点「打开代理侧管理面」                 │                             │                          │
   │──POST /api/v1/admin/ops/magpie-access─┼────────────────────────────▶│ 校验会话=ADMIN            │
   │◀──────────── {url:"http://h:3430/?t=<entry,120s>"} ─────────────────│                          │
   │──GET /?t=<entry>─────────────────────▶│                             │                          │
   │                                      │─auth_request /internal/…/verify?t=▶│ 验签+换发 gate(8h)   │
   │                                      │◀──── 204 + X-Magpie-Gate ───│                          │
   │◀─ 302 /  + Set-Cookie magpie_gate ────│──GET /?k=<page key>─────────────────────────────────────▶│
   │                                      │                             │        Set-Cookie(自己的) │
   │──GET / (带 magpie_gate + 自己的 cookie)─▶│─verify(cookie)─▶ 204 ──┼──GET / ─────────────────▶│
   │◀──────────────────────── 页面 ────────│                             │◀─────────────────────────│
```

要点：

- **入口只认票据**：`/?t=` 通过后 nginx 才把 page key 拼进**上游** URL；浏览器地址栏里始终是 `?t=`（120 秒后失效）。
- **后续请求靠 nginx 自己发的 `magpie_gate` cookie**（HttpOnly，8 小时），`auth_request` 每个请求都验一次签。
- **页面自身的 cookie** 仍然是最后一道：不经入口、直接访问其它路径的请求，magpie 自己会 401。
- 已知边角：只有 `magpie_gate` 而没有 magpie 自己的 cookie（例：清了站点数据、或 magpie 换了 cookie 签名）时，
  `/` 会被 401 挡住（此时 nginx 不注入口令），需要回灵枢再点一次按钮。这条写进运维文档。

### 3.6 nginx 配置片段（web 镜像，`/etc/nginx/templates/default.conf.template`）

```nginx
# ---- http 上下文：模板里由 envsubst 注入 ${MAGPIE_WEB_KEY}（NGINX_ENVSUBST_FILTER=^MAGPIE_ 限定范围）----
map $arg_t $magpie_entry_uri {
    ""      "/";                              # 没有票据：不注入口令，页面自己会 401（等价于没登录）
    default "/?k=${MAGPIE_WEB_KEY}";           # 有票据：注入页面口令（只出现在上游请求里）
}

map $magpie_gate_value $magpie_gate_cookie {
    ""      "";                               # auth_request 没换发 → 不发 Set-Cookie
    default "magpie_gate=$magpie_gate_value; Path=/; HttpOnly; SameSite=Lax; Max-Age=28800";
}

server {
    listen 3431;                     # 宿主 127.0.0.1:${MAGPIE_UI_PORT:-3430}
    server_name _;
    server_tokens off;

    resolver 127.0.0.11 valid=10s ipv6=off;
    set $magpie_page magpie:3430;        # 用变量：容器 IP 变了不用重启 nginx
    set $backend_upstream backend:8080;  # 每个 server 块各自 set（变量不跨 server 继承）

    set $magpie_ticket $arg_t;
    if ($magpie_ticket = "") { set $magpie_ticket $cookie_magpie_gate; }

    location = /_magpie_gate {        # 只给 auth_request 用
        internal;
        proxy_pass http://$backend_upstream/internal/magpie-access/verify?t=$magpie_ticket;
        proxy_set_header Host $host;
    }

    # 入口 / 页面本身
    location = / {
        auth_request /_magpie_gate;
        auth_request_set $magpie_gate_value $upstream_http_x_magpie_gate;
        add_header Set-Cookie $magpie_gate_cookie always;
        add_header Cache-Control "no-store" always;

        proxy_pass http://$magpie_page$magpie_entry_uri;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;              # 带端口，magpie 生成的跳转才对
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 300s;
    }

    # 页面资源与页面自己的 /api：不注入口令，靠 magpie 自己的 cookie 把关
    location / {
        proxy_pass http://$magpie_page$request_uri;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 300s;
        proxy_buffering off;                           # 管理面有 SSE（用量/会话实时）
    }
}
```

注意：**不要**在这一层写 `X-Forwarded-For` 之外的转发头（`Forwarded`/`True-Client-IP` 等），也不要设 `MAGPIE_TRUST_PROXY`（§1.3）。

### 3.7 compose 片段

```yaml
  magpie:
    # 管理面不再发布到宿主：只经 web 的 3431 进入（下面 break-glass 端口除外）
    ports:
      - "${MAGPIE_UI_DIRECT_BIND:-127.0.0.1:3432}:3430"   # break-glass：直连页面（仍要口令），排障用
    environment:
      MAGPIE_WEB_KEY: ${MAGPIE_WEB_KEY:-}   # 空 → 由 deploy/magpie/entrypoint.sh 从卷里读
    volumes:
      - magpie_config:/config
      - magpie_secret:/secret:ro            # 只读：页面口令
    entrypoint: ["/bin/bash", "/usr/local/bin/magpie-entrypoint"]
    # command 仍由脚本 exec：magpie web --addr 0.0.0.0:3430 --no-open

  web:
    ports:
      - "${APP_PORT:-80}:80"
      - "127.0.0.1:${MAGPIE_UI_PORT:-3430}:3431"          # 正式入口（票据 + 服务端注入口令）
    environment:
      MAGPIE_WEB_KEY_FILE: /run/magpie-secret/admin_web_key
      NGINX_ENVSUBST_FILTER: "^MAGPIE_"
    volumes:
      - magpie_secret:/run/magpie-secret:ro

  backend:
    environment:
      MAGPIE_ADMIN_PUBLIC_URL: ${MAGPIE_ADMIN_PUBLIC_URL:-}  # 空 → 用请求 Host + MAGPIE_UI_PORT 拼
      MAGPIE_UI_PORT: ${MAGPIE_UI_PORT:-3430}
```

> `web` 需要读一次卷里的口令渲染模板（`docker-entrypoint.d` 里一个 40 行小脚本），因此 web 容器**持有页面口令** —— 这是 B1′ 的固有代价（§3.8）。

### 3.8 后端接口契约（新增 2 个）

```
POST /api/v1/admin/ops/magpie-access          （WithAuth, RoleAdmin）
  → 200 {"url":"http://<host>:3430/?t=<entry>","expires_at":"2026-11-18T09:12:00Z"}
  → 503 {"code":50301,"reason":"magpie_admin_url_unconfigured"}   # 既没 env 也拼不出来

GET  /internal/magpie-access/verify?t=<token> （**无鉴权**，只给 nginx 的 auth_request 调）
  → 204 + 头 X-Magpie-Gate: <gate>   # 验过的是「入口票据」→ 换发 8h 的门票
  → 204                              # 验过的是「门票」→ 不再换发
  → 403                              # 其余一律
```

票据：`HMAC-SHA256(secret, payload)`，payload = `{v:1, uid, role, aud:"magpie-entry"|"magpie-gate", iat, exp}`；
密钥 `magpie_jump.key` 与 `sm4.key` 同法自动生成并持久化在 `backend_keys` 卷。

安全口径：

- 票据**不含**会话 JWT，不能被拿去调灵枢接口（`aud` 独立）。
- 入口票据 120 秒、**短时内可重放**（无状态，不做单次消费；消费需要存储，本期不做）——知道这条，别在别处写「一次性」。
- 门票 8 小时、不可吊销；轮换 = 换 `magpie_jump.key`（重启 backend 即全员重进）。
- `verify` 无鉴权是有意的（它只回答「签是不是我签的」），但它**不在** `/api/` 前缀下，外网经 nginx 只会落到 SPA 兜底。

### 3.9 前端

`系统 → 上游（V2）` 页签顶部加一块「代理侧（magpie）管理面」：

- 按钮「打开代理侧管理面」→ 调上面的接口 → `window.open(url, '_blank', 'noopener')`；
- 接口 503 / 网络失败 → 按钮置灰 + 原因（与页面既有 `v2Get` 的 pending 归一一致，不新增死链）；
- 文案要写明：**这是代理侧的完整管理面，能看上游 provider 的明文 key**。

### 3.10 残留风险（必须写进部署文档）

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| web 容器持有页面口令 | nginx 必须能拼 `?k=`；web 被拿下 = 管理面被拿下 | 接受；web 容器只跑 nginx，不挂别的卷 |
| 门票 8h 不可吊销 | 管理员角色被撤后，已发的门票仍有效至多 8h | 缩短 TTL / 换密钥；写进文档 |
| `magpie_gate` cookie 按主机不按端口 | 会随请求发到灵枢 80 端口（HttpOnly，灵枢不读） | 接受（magpie 自己的 cookie 同理，文档已提示） |
| break-glass 端口 | `127.0.0.1:3432` 直连 magpie，只要口令 | 只绑回环；不需要时从 compose 删一行 |

---

## 4 升级是否还需要人手工加任何变量？——**不需要**

| 项目 | 首次 `up -d` 之后 | 需要人工？ |
| --- | --- | --- |
| 网关密钥（`admin.key`） | 自动生成 + 注册进 magpie + backend 读取 | 否 |
| 页面口令（`admin_web_key`） | 自动生成，magpie 与 nginx 共用 | 否 |
| `MAGPIE_BASE_URL` | 默认 `http://magpie:3425` | 否 |
| 跳转入口 | 默认 `http://<灵枢访问地址>:3430/`，`MAGPIE_ADMIN_PUBLIC_URL` 可覆盖 | 否 |
| SM4 / JWT / 数据库口令 | 既有自举逻辑不变 | 否 |

仍然需要人做的**只有两件业务动作**（与变量无关）：在管理面加上游 provider、给每个租户令牌绑 callerKey。

升级已有部署（`.env` 里已有 `MAGPIE_ADMIN_KEY` / `MAGPIE_WEB_KEY`）时：**env 优先，卷里不再生成**，行为与现在一致；
只有两者都没有时才走自举。迁移不需要动（本次无新迁移）。

---

## 5 实施顺序与验收

1. A 改造 `magpie-init` + 新增 `deploy/magpie/entrypoint.sh`（读 `/secret/admin_web_key`）。
2. 后端：`magpieaccess` 包（票据签发/校验）+ 两个路由 + 单测（签验、过期、aud 混用、非 ADMIN）。
3. nginx：模板化 + 3431 server 块；web 启动脚本读卷口令。
4. compose：端口/卷/env 重排（magpie 不再发布 3430）。
5. 前端：一个按钮 + 失败态。
6. 文档：`docs/deploy/magpie-container.md` 重写 §1/§2/§4，并补 §3.10 的风险表。

验收（真机，宿主机）：

- `up -d` 后 `magpie-init` 退出 0，`admin.key` / `admin_web_key` 都在卷里；`docker compose logs magpie-init` 不再出现解析失败；
- 灵枢非管理员调 `POST /api/v1/admin/ops/magpie-access` → 403；管理员 → 200 且 `url` 可用；
- 浏览器打开 `url`：**地址栏里没有 `k=`**，页面正常，`document.cookie` 里没有页面口令（HttpOnly）；
- 直接访问 `http://127.0.0.1:3430/`（不带票据、不带 cookie）→ 403（nginx 挡）；
- 清掉 `magpie_gate` cookie 后刷新 → 403；再点按钮 → 正常（票据 120 秒过期后同样 403）；
- `docker compose down -v && up -d` → 两把 key 重新生成，全程零人工变量。

## 6 回滚

无迁移、无数据结构变更。回滚 = 还原 `docker-compose.yml` / `frontend/nginx.conf`（模板）/ 新目录 + revert 后端与前端各一个提交；
卷里多出来的 `admin_web_key` 无害（旧的 magpie 服务不再读它）。
若只想暂时关掉入口：把 `web` 的 `127.0.0.1:3430:3431` 映射删掉，改回 magpie 的 `127.0.0.1:3430:3430` + 手输口令即可。

## 7 待拍板

1. **B1′ 还是 B2**（本文推荐 B1′）。若选 B2，§3.8/§3.9 全部砍掉，只留一个返回链接的接口。
2. 门票 TTL：默认 **8 小时**（可配），是否接受「撤管理员后 8 小时内仍能进」。
3. 管理面入口默认只绑 `127.0.0.1`（现状口径）。若运营要从别的机器直接用浏览器打开，需要改成 LAN/VPN 绑定 + 反代加 TLS —— 这是另一个决定。
