# 渠道协议体系 Implementation Plan

> Status: APPROVED
> Source: `.claude/artifacts/designs/channel-protocol.md`
> Mode: --deliberate（自动升级：涉及**鉴权注入**与**数据迁移**两类高风险信号）
> Iterations: 2 / 3
> Author: user
> Last updated: 2026-10-15

## Requirements summary

把现有**单协议** OpenAI 兼容网关扩展为**三基础协议直通**网关：新增 `openai-responses` / `anthropic-messages` 入站端点；引入「渠道协议」实体承载 baseURL / protocols / auth / 特殊请求头；请求头改为**策略化**（none/whitelist/blacklist/all + set + 强制黑名单）；响应头为**基础协议硬编码 + 全局白名单**；由**外挂预设包**首次启动物化渠道协议、后续管理员手动同步。一期不做跨协议转换。

## Acceptance criteria

（继承 spec，二值可验证）

- AC-1 OpenAI Chat 入站命中含 `openai-chat` 的渠道协议 → 上游 `<baseUrl>/chat/completions` 直通，响应原样回传。
- AC-2 Anthropic 入站 `POST /v1/messages` → 上游协议 `path`，按 `auth` 注入鉴权。
- AC-3 OpenAI Responses 入站 `POST /v1/responses` → 上游协议 `path`（默认 `/responses`）。
- AC-4 入站协议 ∉ 模型可达 protocols → 返回**固定错误**，且**不发起上游请求**。
- AC-5 `passthroughMode=none` → 下游自定义头不上游。
- AC-6 `whitelist=["x-opencode-session"]` → 仅该头透传。
- AC-7 `blacklist=["x-foo"]` → 除 `x-foo` 与强制黑名单外透传。
- AC-8 强制黑名单头（`Cookie`/`Host`/`Accept-Encoding`/`Connection` 等）任何模式下都不出现。
- AC-9 `set` 变量替换正确；鉴权头不出现在用户可编辑 `set`。
- AC-10 全局白名单为空 → 响应头仅基础协议硬编码项。
- AC-11 白名单含某头 → 该上游响应头透传。
- AC-12 预设地址环境变量**未设置** → 不拉取、不报错、列表为空；设置且成功 → 有数据；设置但失败 → 正常启动 + 日志 + 列表为空。
- AC-13 同步-未编辑 → 直接覆盖，标记回「同步」。
- AC-14 同步-已编辑 → 展示差异 + 二次确认才覆盖；放弃不改动。
- AC-15 预设条目删除/改名 → 协议**不删除**，标记「来源已失效」，引用渠道仍可用。
- AC-16 三协议 usage 归一化五段；Anthropic 推理 token 并入 output。
- AC-17 usage 缺失/无法解析 → 该次调用报错 + 渠道置 `DISABLED`。
- AC-18 模型 `protocols` 仅含 `anthropic-messages` → 该模型对 openai-chat 返回 AC-4 固定错误。

## RALPLAN-DR

### Principles

1. **直通优先、最小代码**：一期不做跨协议转换；能复用现有 Provider 骨架就不新建抽象层。
2. **外科手术式改动**：复用已有挂载点（`Provider` 接口 / `ProviderFactory` / `serveWith(endpoint)`），不重写网关热路径。
3. **沿用既有工程模式**：迁移、配置、错误码、前端页面全部按仓库现有范式落地。
4. **安全默认**：上游头白名单默认空、强制黑名单不可配置、鉴权头不进用户 `set`。
5. **可验证**：每条 AC 对应单元/集成/e2e 测试或可观测证据（call_log / metric）。

### Decision drivers

- **不破坏现有 openai-compat 主链路**（兼容性风险最高）。
- **与现有 Provider 抽象一致**（维护成本 / 团队熟悉度）。
- **头与鉴权安全**（凭证泄漏风险）。

### Viable options

**Option A（推荐）· 复用并扩展现有 `Provider` 抽象**
- 思路：把 `Provider.BuildUpstreamRequest` 的入参由「baseURL+cred+body」扩展为携带**下游头 + 头策略**的上游请求构造上下文；新增 `openai-responses` / `anthropic-messages` 两个 Provider 实现；`ProviderFactory`（`gateway/gateway.go:86`）解析维度由 `(protocol, endpoint)` 扩展为「渠道协议 + 基础协议 + 端点」。
- 改动文件：`gateway/provider.go`、`gateway/openai.go`、新增 `gateway/responses.go` / `gateway/anthropic.go` / `gateway/headers.go`、`gateway/gateway.go`、新领域包 `domain/channelprotocol/`、新迁移、`config/config.go`、`docker-entrypoint.sh`、前端若干。
- Pros：与现有架构同构；改动可局部化；`ProviderFactory` 已是注入点，单测可用 fake 替换；后续二期转换可在 Provider 层插入。
- Cons：**需改动 `Provider` 接口签名**，触及 `OpenAIProvider`（`gateway/openai.go:26,52`）、handler 调用点（`handler.go:650,779`）与现有单测替身。

**Option B · handler 内按端点硬编码三套分支**
- 思路：`Provider` 接口不动，直接在 `serveWith`/`serveNonStream`/`serveStream` 内按端点分叉，渠道协议仅作数据存储。
- 改动文件：`gateway/handler.go`、`gateway/openai.go`。
- Pros：短期不动接口签名，现有测试大多不破。
- Cons：**与现有挂载点冲突**（`ProviderFactory` 本就按 `protocol+endpoint` 解析，见 `gateway/gateway.go:86` / `handler.go:398`）；热路径继续膨胀，每加一种协议都要改 handler 主体；二期转换无处安放。判定：**不采纳**。

**Option C · 一期即引入跨协议转换层** —— 被 spec `Out of scope` 明确砍除，列此仅作完整性说明（invalidation rationale）。

### Favored

**Option A**。理由：仓库**已经**为多协议预留了 `Provider` 接口与工厂注入点，Option B 反而要绕开它、把逻辑塞回 handler，违背 Principle 2。

## Implementation steps

1. **迁移**：新增 `backend/internal/db/migrations/0007_channel_protocols.sql`（编号续 `0006`）——
   新建 `channel_protocols`（`id, name, base_url, protocols JSONB, auth JSONB, request_headers JSONB, models JSONB, preset_source_id TEXT, edited BOOL DEFAULT false, source_status TEXT, created_at, updated_at`）；
   `ALTER TABLE channels ADD COLUMN IF NOT EXISTS channel_protocol_id BIGINT REFERENCES channel_protocols(id)`；
   `ALTER TABLE channel_models ADD COLUMN IF NOT EXISTS protocols JSONB`。
   依据：迁移通过 `//go:embed migrations/*.sql` 按文件名排序执行（`backend/internal/db/migrate.go:13-18`），沿用 `0005_drop_channels_weight.sql` 的 `IF EXISTS` 幂等风格。

2. **渠道协议领域包**：新增 `backend/internal/domain/channelprotocol/{store,service,handler}.go`——CRUD + 同步 upsert；错误用现成范式 `APIError{HTTPStatus, Code, Message}`（`domain/channel/service.go:17-21`）+ `internal/pkg/resp`（`pkg/resp/resp.go:20`）。

3. **渠道层扩展**：`domain/channel/service.go` 的 `ChannelInput`（`:45-56`）新增 `ChannelProtocolID`；创建/更新校验（`:89-103`）由「protocol 仅 `openai-compat`」改为二分：**有 `channel_protocol_id` → 以其 protocols 为准**；**无 → 要求内联 `base_url` + 协议**。`domain/channel/store.go:39-42` 的协议常量由单值枚举扩为三基础协议常量。

4. **Provider 接口扩展**：`gateway/provider.go:23-31` 将 `BuildUpstreamRequest(ctx, baseURL, cred, body, isStream)` 的入参收敛为一个构造上下文（新增 `UpstreamRequest{BaseURL, Cred, Body, IsStream, DownstreamHeader, HeaderPolicy, Auth}`），或在 `context.Context` 中携带头策略（二者取其一，见 Architect 张力 T1）。`gateway/openai.go:26,52` 同步改写。

5. **新增两个 Provider 实现**：`gateway/responses.go`、`gateway/anthropic.go`，分别实现 `Name/BuildUpstreamRequest/ExtractUsage/HandleStream`；Anthropic 的 `ExtractUsage` 把推理 token **并入 output**（AC-16）。参照现有 `openai.go:139-199` 的 SSE 透传与 usage 解析写法。

6. **头策略引擎**：新增 `gateway/headers.go`——常量**强制黑名单**表（不可配置）；`ApplyPolicy(mode, whitelist, blacklist, set, vars)`；变量替换 `{api_key}/{session_id}/{request_id}/{model}/{client_header:NAME}`。在 `openai.go:50-60`（及新 Provider）的请求构造处接入，替换现有「仅注入 `Content-Type` + `Authorization`」。

7. **鉴权注入**：在头策略之后由 `auth`(header/scheme/query) 注入鉴权头，**鉴权头禁止进入用户可编辑 `set`**（AC-9）。

8. **入站端点与识别**：`gateway/gateway.go:203-205` 旁新增 `POST /v1/responses`、`POST /v1/messages` 注册；在 handler 建立「路径 → 基础协议」映射（`chat/completions→openai-chat`、`responses→openai-responses`、`v1/messages→anthropic-messages`）。

9. **路由与直通判定**：`handler.go:398` 的 `g.providerFactory(rt.Channel.Protocol, endpoint)` 改为先算**有效 protocols 交集**（渠道协议 ∩ 渠道 ∩ 模型级子集），命中才建 Provider；未命中走**固定错误**，复用 OpenAI 错误体结构 `openAIErrorBody`（`handler.go:92-96`）。有效 baseURL 由 `rt.Channel.BaseURL`（`handler.go:650,779`）改为「协议覆盖优先」。

10. **响应头**：在各 Provider 内**硬编码**入站响应头；全局上游响应头白名单从 `sys_configs`（表结构见 `migrations/0001_init.sql:271-276`）读 key `gateway.upstream_response_header_whitelist`，在写回处过滤上游头（非流式 `handler.go:640-733`、流式 `handler.go:769-779`）。

11. **usage 归一化与计费接线**：各 Provider 的 `ExtractUsage`/`HandleStream` 归一到 `billing.Usage` 五段（`domain/billing/rates.go`）；`handler.go:694` 处 usage 缺失/解析失败 → 返回错误 + 置渠道 `state=DISABLED`（AC-17）。

12. **预设同步**：新增 `backend/internal/domain/preset/{source,syncer,store}.go`，`fetchSource` 参照 `domain/sync/source.go:82-103` 的拉取/解析写法（**不复用价格 watchlist 语义**）；初始化拉取一次 + 管理员按钮触发；物化到 `channel_protocols`；实现「同步/已编辑/来源已失效」三态（AC-12~AC-15）。

13. **配置**：`config/config.go:14-21` 新增 `Preset PresetConfig { PackURL string }`；`backend/docker-entrypoint.sh:73` 旁新增 `PRESET_PACK_URL` → yaml 映射（沿用 `PRICE_SOURCE_URL` 的既有写法）；未设置则不拉取（AC-12）。

14. **前端**：
    - 新增 admin 页面「渠道协议」列表/编辑，挂载于 `frontend/src/router/index.ts:19-61`，菜单登记于 `frontend/src/layouts/AdminLayout.vue:23-45`。
    - `views/admin/ChannelCreate.vue:40-64` 与 `ChannelEdit.vue:58-82` 增加「渠道协议」选择 + 内联协议/头策略字段。
    - 系统菜单新增「预设同步」入口与「全局响应头白名单」配置（参照 `views/admin/BillingConfig.vue` 页面模式）。
    - `frontend/src/api/admin.ts:140-195` 扩展类型与请求函数。

15. **测试**：见「Expanded test plan」。

## Workspace setup

- 实施前运行 `git status --short` 与 `git branch --show-current`。
- 本 plan 会修改代码 / 迁移 / 配置 / 测试，**先确认 working tree 是否干净**：
  - 干净且当前在 `main` / `master` / `release/*` → **默认推荐开 worktree**：`git worktree add -b codex/channel-protocol ../<repo>-channel-protocol`。
  - 已 dirty → 先保护现有改动，不要把本 plan 混入，需用户确认后再动。
- 迁移为**单向新增**，回滚需手工 `DROP`；不在本 plan 自动执行。

## Risks & mitigations

| Risk | Mitigation |
|---|---|
| 改 `Provider` 接口签名破坏现有 openai-compat 与单测替身 | 保留 `BuildUpstreamRequest` 旧签名作为薄封装，新增带策略的重载；灰度改调用点（step 4），并保证 `handler_test.go` 全绿 |
| 头策略误配导致下游凭证/Cookie 泄漏上游 | 强制黑名单为**常量**、不可配置；鉴权头不进用户 `set`；白名单默认空（Principle 4） |
| 预设拉取阻塞启动 | 未设置环境变量直接跳过；失败仅日志、不 panic（AC-12） |
| 同步覆盖掉管理员手工编辑 | `edited` 标记分流 + 已编辑条目**展示差异 + 二次确认**（AC-13/14） |
| 删除预设条目引发级联删渠道 | 外键**不级联**（`ON DELETE` 缺省 RESTRICT/NO ACTION），改标记「来源已失效」（AC-15） |
| usage 解析分支写错导致计费口径漂移 | 三协议 usage 归一化统一走 `billing.Usage` 五段；单测逐协议断言（AC-16） |
| 有效 baseURL/protocols 继承链多层覆盖算错 | 单测覆盖「渠道协议 ← 渠道 ← 模型级子集」三级优先级 |

## Verification steps

- **AC-1/2/3**：集成测试起三端点 + fake 上游，断言请求落点与响应原样。
- **AC-4/18**：单元测试直通判定函数，断言返回固定错误码且**未调用** fake 上游。
- **AC-5/6/7/8**：`gateway/headers_test.go` 表驱动，断言上游 `http.Request.Header` 集合。
- **AC-9**：固定输入断言 `{api_key}`/`{client_header:X}` 替换结果，并断言鉴权头不在用户 `set` 生效面。
- **AC-10/11**：构造带额外响应头的 fake 上游，断言客户端可见头集合。
- **AC-12**：`PRESET_PACK_URL` 空 → 断言无出网请求；非法地址 → 断言进程起来且列表空。
- **AC-13/14/15**：同步器单测，覆盖三态与二次确认分支。
- **AC-16**：三协议各给样例响应体，断言 `billing.Usage` 五段数值。
- **AC-17**：给缺 usage 的响应体，断言返回错误且渠道 state 变 `DISABLED`。
- 回归：`go test ./backend/...` 全绿（尤其 `gateway/handler_test.go`）。

## Pre-mortem (deliberate)

1. **Scenario**：上线后 openai-compat 老渠道全部 502。
   **Trigger**：`Provider` 接口改动后，`handler.go:650` 调用点仍按旧签名传参或工厂返回 `false`。
   **Mitigation**：step 4 分两步提交（先加新重载保留旧签名，再切调用点）；`handler_test.go` 作为回归门禁。

2. **Scenario**：某管理员把 `passthroughMode` 设为 `all`，客户端 `Cookie` 被转发上游。
   **Trigger**：头策略实现漏掉强制黑名单减法。
   **Mitigation**：强制黑名单在 `ApplyPolicy` 内**先减后加**且不可绕过；AC-8 表驱动测试兜底。

3. **Scenario**：预设站点返回畸形 JSON，首次启动 panic。
   **Trigger**：`preset/source.go` 解析未包错。
   **Mitigation**：直接复用 `sync/source.go:99-101` 的 decode 包错模式；失败只日志。

## Expanded test plan (deliberate)

- **Unit**：`headers_test`（策略×黑名单）、变量替换、直通判定、三协议 usage 归一化、同步三态分流。
- **Integration**：三端点端到端（fake 上游）+ 渠道协议 CRUD + 渠道引用协议创建 + 有效 baseURL 继承。
- **E2E**：扩展 `backend/tests/e2e_test.go`（已有 sync 装配，`:137`/`:213`），覆盖「设置 `PRESET_PACK_URL` → 初始化物化 → 管理员同步」。
- **Observability**：call_log 记录协议/上游落点（复用现有 call_log 写入路径 `handler.go` 多处）；渠道 `state=DISABLED` 可查；同步结果计数入日志。

## ADR

- **Decision**：复用并扩展现有 `Provider` 抽象（Option A），引入「渠道协议」实体与策略化请求头，由环境变量驱动的外挂预设包物化。
- **Drivers**：兼容性风险（不破坏主链路）> 与现有抽象一致性 > 头/鉴权安全。
- **Alternatives considered**：
  - Option A —— **chosen**（与既有 `Provider`/`ProviderFactory` 同构）。
  - Option B（handler 内硬编码三分支）—— rejected：绕开现有挂载点，热路径膨胀，二期转换无落点。
  - Option C（一期跨协议转换）—— rejected：spec `Out of scope`。
- **Why chosen**：仓库已为多协议预留接口与工厂注入点，A 顺势扩展即可，改动可局部化、可单测。
- **Consequences**：`Provider` 接口签名变更（正向：能力更强；负向：触及现有实现与测试，需分步提交）。新增一张表与两列，迁移单向。头策略集中到 `gateway/headers.go`，成为安全关键点。
- **Follow-ups**（明确本次不做）：二期跨协议转换（按 `default` 目标协议映射）；「渠道内联配置一键沉淀为渠道协议」；models.dev v2 兼容。

## Review trail

- Planner draft v1：Option A/B/C 三选，favored A，12→15 步。
- Architect challenge v1：指出 T1（头策略随接口签名 vs 随 ctx 传递）与 T2（同步分布式锁缺失），并要求补 pre-mortem。
- Critic verdict v1：REVISE —— 「AC-17 的『置渠道 DISABLED』与现有渠道状态机一致？」未答；verification 缺回归命令。
- Planner draft v2：补 AC-17 与现有 `state` 枚举（`channels.state ∈ NORMAL/DRAIN/DISABLED`，`migrations/0001_init.sql:55`）对齐；verification 补 `go test ./backend/...`；补 pre-mortem/expanded test plan。
- Architect challenge v2：接受 T1 折中（重载保留旧签名），T2 判为非阻塞（单实例部署，跟随 config 模式）。
- Critic verdict v2：APPROVED，2 处改进并入（回归门禁、状态枚举对齐）。
- Final iterations: 2 / 3

## Critic verdict（最终）

| 维度 | 状态 | 备注 |
|---|---|---|
| Principle consistency | ✓ | A 与 Principle 2「复用挂载点」一致 |
| Alternative exploration | ✓ | A/B 真候选，C 有 invalidation rationale |
| Risk mitigation clarity | ✓ | 每 risk 一行 mitigation |
| AC testability | ✓ | 18 条均二值可验证 |
| Verification concreteness | ✓ | 逐 AC 给测试类型 + 回归命令 |
| File/line coverage | ✓ | 步骤 1-15 均 cite 具体文件/行 |
| Pre-mortem present | ✓ | 3 场景 + trigger + mitigation |
| Expanded test plan | ✓ | unit/integration/e2e/observability 齐 |

**Verdict: APPROVED**

### Reservations（必填）

- 步骤 4 的 `Provider` 接口改动，我仍保留一条担心：**若现网存在第三方 we 的 Provider 实现**（非本仓库），签名的破坏性会外溢。Mitigation 段只写了「保留旧签名」，没写「确认无外部实现者」这一步——建议实施前先 `grep` 全仓确认 `Provider` 实现清单。
- 步骤 8 新增端点后，**入站鉴权中间件**是否对 `/v1/messages` 的 `x-api-key` 生效未在步骤里体现（仅落在 AC-2）。若中间件只认 `Authorization`，AC-2 会失败——建议把「入站鉴权兼容 `x-api-key`」显式并入步骤 8。

## Open questions

- AC-4 固定错误的具体 HTTP 状态码与 `resp` 编码取值：实现时按现有约定选定（默认倾向 `400` + `resp.CodeBadRequest`，或新增专用码）。
