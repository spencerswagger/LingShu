# v1-proxy.inc —— /v1 白名单各点位的公共转发段（定案③：nginx 白名单 + 剥头同窗）。
#
# 被frontend/nginx.conf 的 5 个放行 location include（chat/completions、responses、messages
# +count_tokens、Gemini 三方法、/v1/models）；一份 include = 一段剥头，白名单与剥头天然同窗。
#
# 为什么是 .inc 而不是 .conf：容器主配置 include 的是 conf.d/*.conf，.conf 会被 http 顶层
# 二次装载直接报 duplicate location；.inc 只能被显式 include，不会自装载。
#
# 头面口径（与 docs/designs/2026-11-20-nginx-whitelist-and-header-strip-window.md §3 一致）：
#   - 请求方向零改动：Authorization / x-api-key / x-goog-api-key / User-Agent / anthropic-beta
#     / X-Magpie-Session 等原样透传；`?key=` 剔除属应用层职责，nginx 不做 query 改写。
#   - 响应方向逐个枚举剥（proxy_hide_header 不吃通配、容器不带 headers-more）：
#     X-Magpie-* 已知样本 + Server（回填 nginx 自身值）+ X-Powered-By。
#     x-ratelimit-* / Retry-After / x-request-id / 其它未知头一律保留（名单外不猜）。
#     供应商身份类头按 F8 口径在变更窗抓包后逐个判定补枚举（冒烟脚本带自动审计位）。
#   - 剥 X-Magpie-Limit-Reset 后用 Retry-After 镜像限位秒数兜底（变量空值时该头自动省略；
#     值非纯秒数时删除本行——真机核值格式后定，见方案 §11 O2）。

# 响应安全头：add_header 声明后不再继承 server 级，本段显式补全（与 /api/ 段同清单）。
add_header X-Content-Type-Options "nosniff" always;
add_header X-Frame-Options "SAMEORIGIN" always;
add_header Referrer-Policy "strict-origin-when-cross-origin" always;
add_header Permissions-Policy "camera=(), microphone=(), geolocation=()" always;
add_header Cache-Control "no-store" always;

# 转发面：与原 /v1/ 段逐字对齐（SSE 流式关闭缓冲、放宽读写超时）。
# Host 必须原样转发：magpie 的页面/会话判定依赖 Host，改内部名会 403。
proxy_pass http://$magpie_gw$request_uri;
proxy_http_version 1.1;
proxy_set_header Host $http_host;
proxy_set_header X-Real-IP $remote_addr;
proxy_set_header X-Forwarded-For $remote_addr;
proxy_set_header X-Forwarded-Proto $scheme;
proxy_buffering off;
proxy_cache off;
proxy_read_timeout 3600s;
proxy_send_timeout 3600s;

# —— 定案④：痕迹全剥（枚举实现，无通配的现实约束）——
proxy_hide_header Server;                    # 回填 nginx 自身值（server_tokens off，无版本）
proxy_hide_header X-Powered-By;
proxy_hide_header X-Magpie-Left-Out;         # ↓ 现网已剥 4 个
proxy_hide_header X-Magpie-Route;
proxy_hide_header X-Magpie-Request-Id;
proxy_hide_header X-Magpie-Cost;
proxy_hide_header X-Magpie-Provider;         # ↓ 本次新增（已知样本；升版新增头靠发版后断言重跑兜底）
proxy_hide_header X-Magpie-Model;
proxy_hide_header X-Magpie-Limit-Reset;
proxy_hide_header X-Magpie-Upstream;
proxy_hide_header X-Magpie-Session;          # 只剥响应回声；请求方向的透传不受影响（proxy_hide_header 仅作用响应）

# 429 提示兜底：镜像上游限位秒数。$upstream_http_* 变量为空时 nginx 自动省略该头，
# 故只会在 magpie 真发了限位头的响应上出现；真机核值非纯秒数则删本行（方案 §11 O2 默认保留）。
add_header Retry-After $upstream_http_x_magpie_limit_reset always;
