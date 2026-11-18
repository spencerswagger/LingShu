# 代理侧（magpie）随 compose 自举凭据 + 管理面一键跳转

状态：**已实现**（分支 `feat/compose-bundled-magpie`）。部署侧操作手册见
`docs/deploy/magpie-container.md`，本文只记设计与事实依据。

---

## 0 结论先说

1. **凭据全部自举，不需要人填任何变量**：网关密钥 `admin_key` 与**管理台口令**
   `admin_web_key` 都由 `magpie-init` 首次 `up` 时生成并落在 `magpie_secret` 卷里，
   magpie / backend / nginx 读同一份；重启不换值。
2. **管理台入口 = 灵枢同源的 `/magpie/`，只有 ADMIN 能进**：
   灵枢签发 8 小时票据（HttpOnly cookie），nginx 用 `auth_request` 回灵枢验签，
   再由 **nginx 在服务端代填页面口令**。浏览器地址栏、前端存储里都没有口令。
3. **`MAGPIE_TRUST_PROXY` 我们不用**，而且**绝不能**在「把 `/v1` 反代给 magpie」的链路上开：
   它按请求头判定，开了之后 nginx 转来的请求会被当成「本地」= 免网关密钥（§1.3 有判定表）。
4. 子路径反代**能做，但必须补三处改写**（`/api/` 前缀、`Location`、`Host`），
   而且要把上游 gzip 关掉才能 `sub_filter`（§2.2）。

---

## 1 事实依据（每条都指出处）

### 1.1 两个端口的分工

| 端口 | 谁在听 | 内容 |
| --- | --- | --- |
| `3425` | 网关 | `/v1/*`、`/mcp`、健康检查；**没有管理页面** |
| `3430` | `magpie web` | 管理页面 + 页面自己的 `/api/**` + 登录表单；也会应答 `/v1` 路由 |

出处：`internal/gateway/oneport.go`。页面端口转交的请求**永远不算「本地」**（`viaPagePort`），
`MAGPIE_TRUST_PROXY` 也改变不了这一点。

### 1.2 管理台口令的语义

- 口令由 `MAGPIE_WEB_KEY` 提供（16+ 字符）；不设则每次启动随机生成。
- `?k=<口令>` 校验通过后 magpie **种 cookie 并 303/302 回去掉 `k` 的地址**
  （cookie 名 `magpie_web_<页面端口>`，HttpOnly、`Path=/`）；无 cookie 时回 401 + 粘口令表单
  （表单 POST 到 `/_magpie/sign-in`）；免口令白名单只有 `manifest.json` 与两个 icon。
- **它是长期凭据、且只有「知道 / 不知道」两态**，没有用户维度 —— 所以不能直接给浏览器看，
  必须有一层「是谁」的判定（这就是灵枢票据的唯一理由）。

出处：`internal/gui/web.go` 的 `webGuard`。

### 1.3 `MAGPIE_TRUST_PROXY=1` 到底管什么（这是本轮的关键结论）

它管的是**网关的客户端路由**：`internal/gateway/lan.go` 里

```
local() = 回环地址 && !proxied(r) && !viaPagePort(r)
proxied(r) = trustProxy() ? false : 请求带了那 7 个转发头里的任意一个
```

`Forwarded`、`X-Forwarded-For`、`X-Forwarded-Host`、`X-Real-IP`、`Cf-Connecting-IP`、
`True-Client-IP`、`Tailscale-User-Login`。也就是**只看头、不看来源 IP**。

按这段逻辑复刻的判定表（`generated/trustproxy/sim.py`，可复跑）：

| 场景 | `TRUST_PROXY=0` | `TRUST_PROXY=1` |
| --- | --- | --- |
| 直连网关端口、无转发头 | 本地（免密钥） | 本地（免密钥） |
| nginx 反代，带 `X-Forwarded-For`/`X-Real-IP` | **别处（要网关密钥）** | **本地（免密钥）** |
| nginx 反代，只带 `Forwarded` | 别处（要密钥） | **本地（免密钥）** |
| 同网段容器直连（非回环） | 别处 | 别处 |
| 页面端口 `3430` 转交的 `/v1` | 别处 | 别处 |

结论：**在「nginx → magpie `/v1`」这种拓扑下一开就是静默放开鉴权**（任何能给 nginx 塞一个
`X-Forwarded-For` 的调用方都变成「本地」）。而且它**不免管理页面口令** —— 页面和它的 `/api`
永远要口令（官方文档原话：「the page and its `/api` still need the page's key」）。

因此本方案 **不设** 这个变量。要免网关密钥请走「给令牌绑 callerKey」这条正规路径。

> 沙盒里没有 docker，**magpie 镜像本体未实测**；上表是按源码判定逻辑的复刻 + nginx 侧实测。
> 宿主机复测命令见 §5.2。

### 1.4 唯一的「免口令」官方路径是 LazyCat 平台集成，不能用

`MAGPIE_WEB_SIGN_IN=lazycat` 信任 `X-HC-User-Role` 这类平台头（缺 `LAZYCAT_APP_DOMAIN`
直接拒绝启动）。同网段任何容器都能伪造这些头 → 否决。

### 1.5 子路径 `/magpie/` 能挂，但有三个硬约束

| 约束 | 出处 / 现象 |
| --- | --- |
| `magpie web` **没有 base-path 参数**，页面自己占根：静态资源是相对路径（`app.css`、`boot.js`），请求是绝对路径 `fetch("/api/"+p)` | `internal/gui/assets/index.html`、`app.js` |
| 换 cookie 后 302 到 `/` | `webGuard` |
| 页面自己的写请求校验 `Origin` vs `Host`、`Sec-Fetch-Site` | `webOwnPage`（`http.CrossOriginProtection`） |

所以「挂上去就完事」不行，必须 §2.2 的三处改写 + 关闭上游 gzip。

---

## 2 落地形态

### 2.1 访问链路

```
浏览器（管理员）
  │ ① 点「打开代理侧管理台」
  ├── POST /api/v1/admin/ops/magpie-access   （Authorization: Bearer <灵枢 JWT>）
  │     后端：会话必须是 ADMIN → 签 8h 票据 → Set-Cookie: magpie_gate=…; Path=/magpie/; HttpOnly; SameSite=Lax
  │     响应：{ Url: "/magpie/", ExpiresAt }
  │ ② window.open('/magpie/')（同源，自动带 cookie）
  ├── nginx: auth_request → GET /internal/magpie-access/verify （验签，2xx 放行）
  │     验不过 → 403，请求根本到不了 magpie
  ├── nginx → magpie:3430 （无 magpie 页面 cookie 时把 ?k=<口令> 拼在上游 URL 上）
  │     magpie: 换 cookie + 302 → Location: /
  ├── nginx: proxy_redirect 把 Location 改成 /magpie/
  └── 页面加载；JS 里的 "/api/…" 已被 sub_filter 改写成 "/magpie/api/…"
```

### 2.2 nginx（`frontend/nginx.conf`）

- `location = /magpie/`（精确）：`auth_request` + `proxy_pass http://$magpie_page/$magpie_entry_qs`
  + `proxy_redirect ~^/(?!magpie/)(.*)$ /magpie/$1`。
- `location /magpie/`（前缀）：`rewrite ^/magpie/(.*)$ /$1 break` 剥前缀，
  `proxy_set_header Host $http_host`（**不能**改成 `magpie:3430`，否则写请求全 403），
  `proxy_set_header Accept-Encoding ""` + `sub_filter '"/api/' '"/magpie/api/'`
  + `sub_filter 'action="/_magpie/sign-in"' …`，并 `proxy_hide_header ETag`（内容已变）。
- `location = /_magpie_gate`（internal）：把子请求转给 backend 的验签端点；cookie 随请求自动带上。
- 口令注入走**生成出来的 `map`**（`/etc/nginx/conf.d/00-magpie-key.conf`），
  不用 nginx 的 envsubst 模板机制（那会连带替换 `$host`/`$uri` 之类）：

```
map "$cookie_magpie_web_3430|$arg_k" $magpie_entry_qs {
    "|"     "?k=<口令>";
    default "";
}
```
即：**没有 magpie 自己的 cookie、用户也没手动带 `?k=`** 时才代填；带了就原样交给 magpie 判
（自己填错就该被拒，不能「填错反而进去了」）。
`magpie_web_3430` 里的端口是 magpie 的**页面端口**，改端口要同步改这里的 cookie 名与生成脚本。

### 2.3 后端（`internal/domain/magpieaccess`）

| 端点 | 鉴权 | 行为 |
| --- | --- | --- |
| `POST /api/v1/admin/ops/magpie-access` | `WithAuth(RoleAdmin)` + 处理器内再判一次角色 | 签 8h 票据并种 cookie；未接入（`magpie.base_url` 空）→ 503 + `reason=magpie_admin_unconfigured`（前端把按钮置灰）；响应体**不含**票据 |
| `GET /internal/magpie-access/verify` | **无**（只给 nginx 的 `auth_request`） | 验签 → 204；否则 403；`Cache-Control: no-store`；**不在 `/api/` 前缀下**（否则会被 SPA 兜底/中间件吃掉） |

- 票据 = `v1.<base64url(payload)>.<hmac>`，载荷只有 `{uid, role, iss, exp, nonce}`；
  **不是**灵枢会话 JWT，拿去调 `/api/v1/**` 一律 401。
- 密钥 `keys/magpie_jump.key` 首次启动自动生成（32 字节 hex，`chmod 600`），
  与 SM4/JWT 密钥同法自举；删掉即让所有已发出的票据失效。
- 校验端点**无条件注册**：未装配时也回 403 而不是 404 —— 404 会让 nginx 侧看起来像「后端挂了」。

### 2.4 前端（`views/admin/BillingConfig.vue` 的「上游（V2）」页签）

按钮「打开代理侧管理台」，`v-if="isAdmin"`（路由本身也限管理员，这里再判一次）。
点击先同步 `window.open('about:blank')` 占住用户手势，拿到 `Url` 后再 `location.replace`；
503 时提示「代理侧还没接入」。旁边有醒目提示：**管理台里能看到上游明文凭据**。

### 2.5 compose

- `magpie-init`：`admin_key`（抓 `sk-magpie-key-…`）+ `admin_web_key`（`openssl rand -hex 16`），
  两文件 `chown 65532` + `chmod 600`。
- `magpie`：`entrypoint: bash -c` 在 `exec magpie web …` 之前把 `/secret/admin_web_key` 读进
  `MAGPIE_WEB_KEY`（magpie 只认环境变量）。
- `web`：只读挂 `magpie_secret`，启动时生成 §2.2 的 map。**代价：web 容器持有管理台口令。**
- 宿主机 `127.0.0.1:${MAGPIE_UI_PORT:-3430}` 保留为 break-glass（删掉这行不影响功能）。

---

## 3 为什么不把口令交给浏览器（被否决的两个做法）

| 做法 | 为什么否决 |
| --- | --- |
| 按钮直接 `window.open('/magpie/?k=<口令>')` | 口令进了浏览器地址栏/history/截图，等于把「长期凭据」泄漏给任何能看屏幕或日志的人；且口令只代表「能进」，不区分是谁 |
| 用灵枢 JWT 直接做 `auth_request` 的凭据 | 灵枢登录态在 localStorage，浏览器**导航**不带 `Authorization`，子请求必然 401（这是「直接上 auth_request」做不出来的根本原因） |

---

## 4 残留风险（已在部署文档同步）

1. **web 容器持有管理台口令**：能 `exec` 进 web 的人等于能开管理台。它只跑 nginx、不挂别的卷。
2. **票据不可单独吊销**：8 小时内有效，只能靠轮换 `keys/magpie_jump.key` 全量作废（会让所有人重开一次）。
3. **口令以 URL 形式进入 nginx→magpie 这一跳**（`?k=` 在容器内网、不到浏览器）。这是 magpie 唯一的入口形态，不是可选项。
4. **`sub_filter` 依赖 magpie 页面里字面量 `"/api/`**：magpie 改写法（例如改成 `fetch(location.origin + "/api/")` 或构建期注入前缀）会让改写失效 → 页面 401。
   自检脚本 `frontend/scripts/magpie-ui-proxy-selftest.sh` 用忠实 stub 覆盖这条；**升级 magpie 后应在宿主机复跑一次**（§5.2）。
5. **`cookie magpie_web_3430` 里的端口与 compose 的 `--addr 0.0.0.0:3430` 绑定**：改端口要同步改 `frontend/00-magpie-key.conf` 与 `19-magpie-key.sh`。
6. 页面自身仍是「单口令」模型：管理台内部没有审计/角色。谁能用管理台，就是全权。

---

## 5 验收

### 5.1 仓库里可复跑的（沙盒已跑通）

```bash
# 后端：票据签验 + 两条路由口径
go test ./internal/domain/magpieaccess/ ./internal/server/ -run 'MagpieAccess|Ticket|Verify|Issue'
# nginx：真实配置 + 忠实 stub（需要 nginx、python3、端口 3430 空闲）
bash frontend/scripts/magpie-ui-proxy-selftest.sh
```

自检覆盖：无票据/伪票据 403、票据换口令 302 且 `Location` 改回 `/magpie/`、页面 200、
JS 里 `/api/` 全量改写、页面 API 前缀剥净且 `Host` 原样、同源 POST 200 / 外站 POST 403、
灵枢自己的 `/api/` 不受影响。
**变异验证**（做过的四条，改回即红）：删 `auth_request`、删 `sub_filter`、
`Host` 换成 `magpie:3430`、删 `proxy_redirect`。

### 5.2 宿主机需要补的三条（沙盒无 docker）

```bash
# ① 管理台口令与端口分工：3425 打开应当 404（没有页面），3430 才是页面
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:3425/
curl -s -o /dev/null -w '%{http_code}\n' "http://127.0.0.1:3430/?k=$(docker compose exec -u 0 web cat /run/magpie-secret/admin_web_key)"

# ② TRUST_PROXY 的作用面：开与不开各跑一次，比较 /v1/models 的 401/200 与页面 / 的 401
#    （结论应为：只影响 3425 的客户端路由，页面口令不受影响）
TRUST_PROXY=0 docker compose up -d magpie && curl -s -o /dev/null -w 'v1=%{http_code} ' \
  -H 'X-Forwarded-For: 10.0.0.7' http://magpie:3425/v1/models
TRUST_PROXY=1 docker compose up -d magpie && curl -s -o /dev/null -w 'v1=%{http_code} ' \
  -H 'X-Forwarded-For: 10.0.0.7' http://magpie:3425/v1/models

# ③ magpie 升级后复跑子路径自检 + 手工点一次按钮
bash frontend/scripts/magpie-ui-proxy-selftest.sh
```

---

## 6 回滚

- 纯前端/编排改动 + 一个新包：回滚 = revert 分支上的提交（`/magpie/` 入口消失、
  按钮消失），**没有数据库迁移**，不影响转发与计费。
- 只想临时关掉入口：删 compose 里 `web` 的 `magpie_secret` 挂载 → `/magpie/` 会停在
  「要口令」的登录表单（仍受 ADMIN 票据保护）；或直接删 `nginx.conf` 里两个 `/magpie/` location。
- 票据密钥损坏：删 `keys/magpie_jump.key` 重启 backend。

---

## 7 待拍板 / 后续

1. 要不要在管理台入口处加二次确认（当前只有页面上的文字提示「能看到明文凭据」）。
2. 要不要把票据有效期做成可配（`MAGPIE_JUMP_TTL`），当前固定 8h。
3. 多副本时票据密钥怎么共享（现在跟 `keys/sm4.key` 一样是单副本落盘；多副本需要外部密钥源）。
