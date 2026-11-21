# nginx 白名单 + 剥头 · 变更窗验收（定案③ 2026-11-20）

一条命令（Gate 在变更窗 T0+1min 跑）：

```sh
B=https://<对外入口> TOK='sk-gw-<测试令牌>' MODEL=<chat模型> GEM_MODEL=<gemini模型> \
[PSQL='docker compose exec -T postgres psql -U lingshu -d lingshu -tA'] \
[HEALTH_INTERNAL='docker compose exec -T backend wget -qO- http://127.0.0.1:8080/healthz'] \
sh backend/scripts/acceptance/nginx-window/run.sh
```

- `PSQL` 给了才跑 §7-7（count_tokens 无账本行）与 §7-8（计费链 60s 落账本）；没给打 SKIP。
- `HEALTH_INTERNAL` 给了才跑内网 healthz；外部可达性结果打 AUDIT（供预案 B 裁定）。
- 产出 `headers-audit.txt`：§8-9 抓包核全归档输入，含「疑似供应商身份类头」审计位（F8 口径初筛，
  只报不判——窗内人工定夺后补 `proxy_hide_header` 并重跑）。
- 结论行 `冒烟结论：PASS/FAIL`；FAIL = 触发回滚判定（方案 §9）。

## 判据映射（方案 §8 十条）

| §8 判据 | 落点 |
|---|---|
| 1 放行 6 类 + WS 101 | run.sh §7-1/§7-2（WS 以 Codex CLI 实连为最终判据，握手 101 是代理层面） |
| 2 count_tokens×2 无账本行 | run.sh §7-7（PSQL 前后 `ledger_entries` 计数） |
| 3 白名单外 404 抽样 | run.sh §7-3（11 条抽样 + 错误体无内部信息） |
| 4 healthz 外网不可达 | run.sh §7-6（可达 → AUDIT 提示启用预案 B） |
| 5 响应头零 magpie | run.sh §7-4 全点位（大小写不敏感头名+头值） |
| 6 Server 无版本 | run.sh §7-3/§7-4（`Server: nginx` 精确比对） |
| 7 SSE 流式 | run.sh §7-5（完整结束 + 首字节耗时） |
| 8 四 SDK 初始化 | MANUAL（人工：openai/anthropic/gemini SDK + Codex CLI） |
| 9 剥头清单归档 | `headers-audit.txt` + AUDIT 位 |
| 10 Retry-After 镜像 | AUDIT（429 时核对值格式；iff 行为由离线自检钉死） |

## 离线回归（CI/本地，无真机）

```sh
bash frontend/scripts/v1-whitelist-selftest.sh
```

真 nginx + stub 起本地端口跑全链路：白名单 6 类放行、404 兜底（含 `/v1beta/`）、
零 magpie 断言、Retry-After 镜像 iff、WS 升级头透传、请求头零改动、SSE 完整、
变异自检（上游新增未知 `X-Magpie-*` 头时断言必须报警——防线有效性证明）。
§8 里依赖真机/数据库的条目不在离线范围，由本目录 `run.sh` 在变更窗内闭合。
