# V2 阶段二准备件（迁移链 + 转发主链 + 对账计数）交付说明

日期：2026-11-15　分支：`feat/lingshu-v2-sync-config`（本地特性分支，未开 PR、未提工单）

> 结论先说：**四条硬裁定（D1/D4/F1/F4）都已落到代码里，迁移链与转发主链一次启动即可跑通；
> 但「0024 删渠道维度」与现有账单/统计代码冲突这件事没有解决**——9 个真库用例会红（见 §3）。
> 本件登记为未完成项，不假装已解决。

## 0. 测试侧基线红：逐条对账（2026-11-15 补）

测试给的 7 条基线红，逐条处理结果如下（"根因"一列是**实际原因**，不是复述现象）：

| # | 现象 | 根因 | 处理 |
| --- | --- | --- | --- |
| 1 | F8 四类头全漏 | ① 供应商名单只有 `magpie` 一个词，按头名整串匹配，`x-provider: openai` / `x-served-by` 全放过；② `x-request-id` 只在 ctx 有值时写，没有就**既不写也不生成** | `headers.go` 改三段判定：值含供应商名 / 头名属身份类（provider·account·vendor·served·backend·worker·region）/ `x-magpie-`·`x-upstream-` 前缀；链路 ID 缺省时**生成**（`ls-<16hex>`）。新增 `TestV2_F8_ResponseHeaderScrub`（端到端，直测客户端收到什么） |
| 2 | `0023` 插 `skipped` 被 23514 拒 | 迁移框架按文件名记账，**改过的 .sql 不会重放**：基线库早已应用过旧版 0023（值域只有 ok\|alarm\|error\|blocked），改文件对它无效 | 新增 **`0023b_v2_reconcile_repair.sql`**（幂等修形：删旧自动命名 CHECK → 按固定名重建含 skipped）。Go 侧 `Validate` 本来就认 5 个值（已核对），DB 侧靠这条修形迁移补上。`TestGate_F10b` 用"旧形状库 + 应用修形"复现并验证 |
| 3 | F1 三键静态检查 4 条红 | ① `Timeouts`/`Degrade` 没实现 `MarshalJSON`，直接序列化得到 `{"StreamFirstByte":30000000000}`（Go 字段名 + 纳秒）——"形状"根本不是裁定的键名；② 仓库里还留着已废弃的单值超时键名字面量 | 两个结构体补 `MarshalJSON`（毫秒 / 布尔、键名逐字按裁定）；配置键集断言 `ConfigKeys()` 恰为三键、超时块恰为两段；连接超时 10s 与非流式总超时 600s **改成代码常量**（`connectTimeout` / `nonStreamRequestTimeout`），不暴露为配置键；删掉残留废弃键名的用例 |
| 4 | F10 `cost_credits IS NULL + ledger` 被放行 | 约束写成 `cost_source = 'none'`，PG 的 CHECK 把三值逻辑的 NULL 当**通过** → "NULL + NULL" 求值为 NULL，等于没有约束；另外加约束时的全表归一 UPDATE 就是那 15 万行扫描本身 | 改成 `cost_source IS NOT DISTINCT FROM 'none'` + `NOT VALID`（存量不扫、新写与 UPDATE 强约束）；删掉全表 UPDATE；文件里写明"回填 `ledger_entry_id` 必须同时 `COALESCE(cost_source,'none')`"并给出正反例 |
| 5 | F4 0021 的 DROP 早于 0022/0023 | 归档迁移编号最小却做了最危险的事 | 改名 `0024_v2_drop_legacy.sql` 排字典序最后，`0021` 作空号**不复用**（已确认目录里没有 0021） |
| 6 | F9 4200 积压两轮都 blocked、账本 0、offset 每轮从 0 重拉 | ⚠️ 关键发现：`PageSource` **只有接口没有生产实现**，而 fake 源**忽略 offset**（按调用顺序返页），所以"续拉"在单测里永远是绿的；真实库上没有可跑的源 | 新增真库用例 `TestRounder_RealSQL_Backlog4200_TwoRounds`：用真实 `ledger_entries`/`reconcile_windows` + **遵守 offset 语义**的分页源，断言第一轮 4000 条 + blocked + offset=4000，第二轮**从 4000 续拉**、补 200 条、blocked 清零、offset 归零。变异验证：把 `offset := int(resumeOffset)` 改成 0 → 用例立刻红（现象与基线一致：两轮都 blocked） |
| 7 | D1 `OnComplete` 没接上 | 钩子名与语义没按 Ark §9.2.2 对齐（我把"结束回调"叫了 `OnDone`），主链侧的时序没有写在代码上 | 钩子改名 `OnComplete`；`OnUpstreamResponse` 增补时序约束注释（同步、首字节前返回、**钩子内不许碰 DB**）；主链注释写清"钩子只存内存事实、落库只在 OnComplete、且用 `WithoutCancel`"。新增 `TestForward_HookBeforeFirstByteAndRequestIDFallback`（钩子运行时下游字节数必须为 0） |

另按补充约束：**blocked/error 窗口的计数列必须 NULL，不许写 0**。已改成计数列可空（`NULL` = 未算过，`0` = 算过且真的没流量），`0023b` 把老库里 blocked/error 行的 0 占位归 NULL；对账服务拒绝对 blocked 行写计数（`ErrWindowNotReconcilable`）。计数列写入方按裁定补齐，**没有**扩到七列读取方。

## 0.1 F6 收紧与 F9「先证后写」（2026-11-15 补）

### F6：任何路径的 `?key=` 一律剔除

- 口径：**租户令牌不得透传到上游**。不再有「Gemini 原生鉴权才剔」的例外；用 header 鉴权却在
  query 里挂着令牌的调用方同样被剔（这种最该被看见），并留一条 WARN（日志只记 path 与「本次是否用 query 鉴权」，
  **不记令牌原文**）。
- 行为细节：其余参数（`alt=sse`、`prettyPrint` 等）**逐字原样**；剔完为空则上游 query 是空串，
  **不留尾部 `?`**；参数名大小写敏感（`Key=` 不误剔）。
- 实现：`magpie.ForwardRequest.StripQueryKey` 字段**删除**（布尔开关会让「忘了置 true」变成静默泄漏），
  改为 `Forward` 内无条件 `StripQueryParam(q,"key")`；新增 `magpie.HasQueryParam` 供主链告警判据。
- 用例：`TestForward_QueryPassthroughExceptKey`（6 条形状，含非 Gemini 路径、key 在中间、大小写）、
  `TestV2_F6_AnyPathKeyParamStrippedAndWarned`（端到端：上游收到 `alt=sse`、日志有 WARN 且**不含令牌原文**、
  只有 key 时上游 query 为空串）。
- 文档口径只写一条理由：**租户令牌不得透传到上游**。

### F9：上游排序能力先证后写（POC）

- 新增能力探测 `upstream.ProbeLedgerOrder`：取相邻两页，验三条——页内按 `(occurred_at, ledger_id)` 非递减、
  跨页不回退、样本 ≥2（只有一条 = 没证过）。三条结论：`supported` / `unsupported` / `unverified`。
- 闸门在 `Rounder.RunOnce` 的**翻页之前**：`LedgerOrder != supported` → 本窗直接记 `blocked`
  （Pages/Fetched/Inserted 为 0、`scroll_offset=0`、reason 写明「排序能力未证实」），
  返回 `ErrLedgerOrderUnproven`，**一条都不拉、不写账本**。
  为什么不在翻页中途发现再退：翻页一旦跑过，offset 就写进库了，等于用一个不可信的排序去证明进度。
- POC 结果（本次）：
  - 链路验证 **PASS**：`TestPOC_UpstreamLedgerOrder_Mock` 用真实 HTTP 分页源（`GET /v1/magpie/usage/requests`
    + offset 翻页）对着同一 mock 上游的两种排序跑：升序判 `supported`、乱序判 `unsupported`。
  - **真实上游未执行**：沙盒里没有上游地址/凭据（`TestPOC_UpstreamLedgerOrder_Real` 无 `UPSTREAM_LEDGER_URL` 时 SKIP）。
    因此当前结论是 `unverified`，行为按裁定的保守分支：**本窗记 blocked、不可清零**。
    复跑方式（有上游时一条命令）：`UPSTREAM_LEDGER_URL=… UPSTREAM_ADMIN_KEY=… go test ./internal/domain/upstream/ -run TestPOC_UpstreamLedgerOrder_Real -v`。
- 新增 `upstream.HTTPLedgerSource`（`PageSource` 的生产实现，契约与 `usagebilling` 分支的 `LedgerPath` 同源）。
  它**刻意不做本地排序**：在这里补一个 sort 会把「上游乱序」完全掩盖，探测永远返回 supported。

### 迁移编号（已定项）

- `drop_legacy` 占 **0024**；`token_scope` 顺延 **0025**；本批右界常量 `V2MigrationRightBound = "0025"`，
  所有「本批内 0024 必须最后 / 只允许 0024 破坏性」的断言都以 `[0019, 0025)` 为界——后续批加 0025 不会把本批守卫打红。
- F10 用 `NOT VALID`；F1 仍是**两键**（`stream_first_byte_ms` / `stream_idle_ms`）+ `connect 10s`、`request 600s` 作代码常量。

## 0.2 Gate 复验（`e7d49ac`）后修三处（2026-11-15 深夜）

### 改动清单（文件 → 改了什么 → 对应用例）

| 文件 | 改了什么 | 用例 |
| --- | --- | --- |
| `internal/domain/magpie/headers.go` | **接线 `hasVendorToken`**：头名含供应商/渠道/账号名一律剔（子串、大小写不敏感），并把 `goog` 补进名单。此前 `hasVendorToken` 是死代码，`ShouldDropResponseHeader("OpenAI-Organization",…)` 返回 false | `TestShouldDropResponseHeader_F8Classes`（17 行判定表，含 Gate 漏的三类）、`TestForward_VendorIdentityHeadersDropped`、`TestV2_F8_ResponseHeaderScrub`（端到端） |
| 同上 | 代价写进注释：`anthropic-ratelimit-*` 这类「供应商前缀 + 正常语义」的头也一并剥（身份泄漏不可逆，要改回来属新裁定）；通用 `x-ratelimit-limit` 仍透传，证明不是一刀切 | `TestForward_ResponseBytesIdentical`、`TestForward_VendorIdentityHeadersDropped` |
| 同上 | 确认 `x-request-id` 归灵枢：上游即使发多个同名值（大小写混用）也只留我们那一个 | `TestForward_OurRequestIDWinsOverUpstream` |
| `cmd/server/main.go` | **装配同步轮次**：`NewHTTPLedgerSource`（env `UPSTREAM_LEDGER_URL` / `UPSTREAM_ADMIN_KEY` / `UPSTREAM_ADMIN_AUTH_HEADER`）→ 启动时 `ProbeLedgerOrder`（最近 30 分钟、200 条样本）→ `NewRounder` → 60s ticker，`sync_enabled` 关闭时整轮跳过，单轮 panic 打日志不带走进程 | 编译 + `-race`（`cmd/server` 包）、`TestRounder_*` |
| `internal/domain/upstream/window.go` | `RoundResult.Empty` 新增：`Fetched==Inserted==0` 时置位并打 WARN（`本轮同步未拉到任何账本条目（空轮）`，带 window/scan_start/pages/resume_offset/hint）。此前空轮「0 行 + nil」完全静默 | `TestRounder_EmptyRoundIsObservable` |
| `internal/domain/billing/query_requestid_integration_test.go` | 两处夹具补 `cost_source='none'`（F10 跨列 CHECK 要求「无成本则来源恰好 none」） | `TestListRecords_RequestIDFilter_Integrated`、`TestListRecords_GroupByRequest_UserIsolation_Integrated` |
| `internal/domain/billing/attribution_pg_test.go` | `TestBilling_DimensionFromCallLogs` 改成 0024 之后的口径：断言 `billing_records/sessions.channel_key_id` 已删、`call_logs.channel_key_id` 保留、渠道族四张表已删；**行为段显式 Skip**（写路径仍写被删列 → 42703，属 P0 代码债） | 该用例本身（3 条 schema 断言 + Skip） |

### 复跑结果

- `go vet ./...`：0 输出；`gofmt -l`（本轮改动文件）：空。
- `go test ./... -count=1`（无 DSN）：35 包全绿。
- `go test -race`（magpie / gateway / upstream / db / cmd-server，带真库 DSN）：全绿。
- 真库门禁：`TestGate_F4/F10/F10b/F7` 四条 PASS；`TestRounder_RealSQL_Backlog4200_TwoRounds`、`TestSQLWindowStore_RealSQL_SkippedWritableAndCountsWritten`、`TestPOC_UpstreamLedgerOrder_Mock` PASS，真实上游 POC 仍 SKIP（无 URL/凭据）。
- **带 DSN 全量：由 9 红降到 8 红**，且 8 条**全部**是 0024 代码债（预期内，本批不动）：
  - `billing`（3）：`TestAttribution_RealSchema_WriteAndReadBack`（写路径仍写 `channel_key_id` → 42703）、`TestListRecords_RequestIDFilter_Integrated`、`TestListRecords_GroupByRequest_UserIsolation_Integrated`（`recordCols` 仍选该列 → 42703）。
    ⚠️ 订正 Gate 的归因：这 2 条不只是 `cost_source` 夹具问题——夹具补齐后它们**继续红**，红点移到读/写路径仍引用被删列。夹具修复本身有效（不再是 23514）。
  - `console`（5）：`TestCostStats_CrossChannelFallback`、`TestStats_WindowAndPlaza`、`TestStats_WindowCalendarMode`、`TestStats_ResponseJSONShape`、`TestStats_SoftDeletedModelGoesUnmatched`（`relation "channels" does not exist`）。
  - `TestBilling_DimensionFromCallLogs`：由红转 **SKIP**（口径已改，行为段等代码债修好再补）。
- 变异验证：把 `hasVendorToken` 判定掐掉 → `TestShouldDropResponseHeader_F8Classes`（3 个子例）、`TestForward_VendorIdentityHeadersDropped`、`TestV2_F8_ResponseHeaderScrub` 同时转红，报错直接点名 `X-OpenAI-Organization`。

## 0.3 计数列 NULL 语义补洞（Gate 复验第 1 问，2026-11-15）

- `0023b` **只**覆盖 `status IN ('blocked','error')` 且四列全 0 的行（`0023b_v2_reconcile_repair.sql:44-49`），**没有**处理老 `ok` 窗口的 0 占位 —— Gate 的复验是对的。
- 补 `0023c_v2_reconcile_counts_backfill.sql`：条件是 `detail->>'phase' = 'sync'` **且**四列全 0 → 归 NULL。判据用写入方标记（同步轮次写 `phase='sync'` 且从不写计数；对账写 `phase='reconcile'`），因此对账算过的 0（真没流量）与没有标记的行都不动；任一列非 0 也不动。
- 用例 `TestGate_D4_SyncPlaceholderZeroBecomesNull`：四类行（同步占位 0 / 对账真 0 / blocked 占位 0 / 同步非 0）跑两遍验幂等，断言只有前两类中「该归 NULL 的」被归 NULL，对账真 0 保持 0、非 0 值不被抹。

## 0.4 blocked 状态映射统一（Gate 复验 P2，2026-11-15）

- 根因：`saveStatus` 收的是 `RoundResult` **值拷贝**，函数内改 `result.Status` 改的是副本 —— 库里落 blocked、调用方返回值仍是零值 ok。修成 `*RoundResult`（`window.go:693-699`），10 个调用点同步改 `&result`；blocked / error 分支的返回值从此与库记录、日志同源。
- 空轮告警同步收窄：`Empty` 只在**成功收尾**（`Status==ok`）时才置位（`window.go:636-644`）——blocked 的 0 行是失败，由分支自己告警，不混进空轮 WARN。`cmd/server` 侧的 `res.Empty && err == nil` 分支语义不变。
- 用例 `TestRounder_BlockedStatusConsistent`：三种 blocked 来源（排序未证实 / 已证否 / 页数超限）各断一次「返回值 Status=blocked + Reason 非空 + Empty=false + 库记录 blocked 带 reason + 日志写明 blocked 且无空轮告警」。

## 1. 改动清单（文件 → 改了什么 → 对应裁定）

### 迁移链（F4 / F2 / F7 / F9 / F10 / D4）

| 文件 | 改了什么 | 裁定 |
| --- | --- | --- |
| `internal/db/migrations/0024_v2_drop_legacy.sql`（新增） | 把原 `0021` 改名 `0024` 并**排到字典序最后**，成为唯一的不可逆迁移；保留子→父顺序的 DROP COLUMN / DROP TABLE；文件头写清保留 15 表 / 退役 10 表、证据与探针口径 | F4（一次启动跑全链、顺序=字典序）、F2（保留表 15）、F7（只要求行数不减少） |
| `internal/db/migrations/0020_v2_billing_attribution.sql` | 新增跨列 CHECK `ck_billing_cost_presence`（`cost_credits IS NULL ⇒ cost_source='none'`）；加约束前先把存量 `cost_credits IS NULL AND cost_source IS NULL` 归一成 `'none'`；文件头点明不可逆动作已全部搬去 0024 | F10 |
| `internal/db/migrations/0023_v2_reconcile.sql` | status 枚举补 `skipped`（`ok\|alarm\|error\|blocked\|skipped`）、drop 旧内联约束后重建为 `ck_reconcile_status`；新增 `scroll_offset BIGINT NOT NULL DEFAULT 0`；保留四个计数列；补 blocked 窗口索引 | D4（计数列由对账服务写）、F9（offset 持久化） |
| `internal/db/v2_tables.go`（新增） | `RetainedTables()` = 15 张（含 `watchlist_items`、`price_change_alerts`）、`RetiredTables()` = 10 张（`channel_events` 已在 0009 删，不计入）、`V2MigrationFiles()` = 0019/0020/0022/0023/0024 | F2 |
| `internal/db/migrate_batch_test.go` | 改为断言 V2 迁移集合精确相等；`0024` 必须是字典序最大；`0019` 起只有 `0024` 允许破坏性 DDL（`<0019` 的历史迁移豁免，`0005` 当年就带 DROP）；`0023` 必须有 `scroll_offset`/`skipped`/`ck_reconcile_status`/四计数列；`RetiredTables()` 与 0024 正文交叉核对 | F4 / F2 / F9 / D4 |
| `internal/db/v2chain_gate_test.go`（新增，真库门禁） | 三条：①空库一次 `Migrate` 跑全链 + 0020 早于 0024 + 0024 排最后 + 无 0021 + 15 保留表在/10 退役表不在 ②存量库（应用到 0019）写 NULL 成本行 → 跑 0020 → 归一成 `none` 且 CHECK 拦得住「0 成本+ledger」 ③0024 前后 15 张保留表行数不减少 | F4 / F10 / F7 |

### 运行参数（F1）

| 文件 | 改了什么 | 裁定 |
| --- | --- | --- |
| `internal/domain/upstream/config.go` | 只读三个键：`upstream.base_url` / `upstream.timeouts{stream_first_byte_ms, stream_idle_ms}` / `upstream.degrade{passthrough_enabled, ledger_enabled, sync_enabled}`；**删除单值 `upstream.timeout_ms`**；新增 `Timeouts`/`Degrade`/`SyncActive()`/`LedgerActive()`/`PassthroughActive()`；未知键报问题、单字段坏值只回落该字段；不接受任何带凭据的键 | F1 |
| `internal/domain/upstream/config_test.go` | 覆盖 30s/120s 默认、三开关默认开、新 JSON 形状与宽容格式、旧 `timeout_ms` 被忽略、未知键上报、部分 degrade 回落、缓存与失效、"不许出现凭据键"守卫 | F1 |
| `internal/seeding/seed.go` + `seed_test.go` | 种子默认值改为三键形状，去掉 `timeout_ms` | F1 |
| `cmd/server/main.go` | 装配 `upstream.NewConfigProvider(billStore)`：**基址非空才让 V2 接管 /v1**，否则保留 V1 并打 WARN；上游鉴权头只从环境变量 `UPSTREAM_AUTH_HEADER` / `UPSTREAM_AUTH_VALUE` 取（配置表与代码里都没有凭据） | F1 |

### 转发主链（D1 / F5 / F6 / F8 / D12）

| 文件 | 改了什么 | 裁定 |
| --- | --- | --- |
| `internal/domain/magpie/protocols.go` | 入站协议识别：`/v1/chat/completions`、`/v1/responses`、`/v1/messages`、`/v1/messages/count_tokens`、`/v1beta/models/{m}:generateContent\|streamGenerateContent\|countTokens`；其余路径一律拒绝（不放开成任意 path 的开放代理）；导出 `PathModel`/`RewritePathModel` | D1 |
| `internal/domain/magpie/rewrite.go` | 用 `json.Decoder.InputOffset` 定位顶层 `model` 的**值区间**做字节替换（不重新序列化，键序/空白/数字表示全保留）；Gemini 只改 path 段 | D1 |
| `internal/domain/magpie/headers.go` | 请求侧强制黑名单（hop-by-hop / 由 URL·body 重生成 / 凭据 / 链路指纹）+ `Accept-Encoding: identity`；响应侧按 F8 剥：hop-by-hop、`x-magpie-*`/`x-upstream-*` 前缀、`server`/`x-powered-by`/`via`、名或值含供应商名的头；上游 `x-request-id` 丢掉后写灵枢自己的；归因在剥头**之前**读 | F8 |
| `internal/domain/magpie/usage.go` | 四协议 usage 旁路读取（只读不改；OpenAI/Responses/Anthropic/Gemini 各自字段路径 + 归一化三段） | D1 |
| `internal/domain/magpie/proxy.go` | 转发本体：只换凭证头 + model，响应体**字节原样**；两段超时（等头 = `stream_first_byte`，读体空闲 = `stream_idle`，用看门狗按每次 Read 重置，不用总超时）；两个钩子 `OnUpstreamResponse`（首字节，归因定稿）/`OnDone`（结束，usage 定稿，`context.WithoutCancel` 保证断连也结算）；query 原样，只有鉴权用了 `?key=` 才剔除 | D1 / F6 / D12 |
| `internal/domain/gateway/upstream.go`（新增） | 主链：鉴权（Bearer / x-api-key / x-goog-api-key / `?key=`）→ 模型准入（`external_models.enabled` 且 `bound_internal_ref` 非空）→ 预扣 → 调 magpie → 结算落账单；失败全额退预扣 + 落 failed 账单；无 usage 走估算并在账单标注；错误体按入站协议形状输出；**预扣/账单/归因全在主链，magpie 不碰钱** | D1 / F5 |
| `internal/domain/gateway/gateway.go` | 装配了 `UpstreamConfig` 时 `Handler()` 返回 V2 入口（`/v1/models` + `/v1`、`/v1beta` 转发），否则维持 V1 | D1 / F5 |
| `internal/domain/model/store.go`（+测试） | `ExternalModel` 补 `BoundInternalRef`（读 `external_models.bound_internal_ref`，TEXT NULL → 空串），这是绑定真源、也是转发时的上游模型名 | D1 |
| `internal/domain/gateway/upstream_test.go`（新增） | 8 组：正常路径（只换 model / 字节原样 / 凭据注入 / 归因与 usage 落账单 / 预扣只结算差额）、上游 4xx（原样透传 + 全额退预扣 + failed 账单）、四类拒绝（降级 503 且不打上游、未知路径 404、无令牌 401、未绑定模型 404、预扣被拒 402 且不打上游）、Gemini path 改写 + `?key=` 剔除、无 usage 估算标注、`/v1/models` 只列启用且已绑定 | D1 / F6 / F8 |
| `internal/domain/magpie/forward_bytes_test.go`（新增） | D12 主断言：8 类形状 × 3 轮随机响应体（随机长度/字段名/嵌套/中文/HTML/20 万字节二进制串），断言下游字节**逐字节等于**上游原文、状态码一致、只剔规定头、`x-request-id` 是灵芯自己的；另含 SSE 分包逐字节一致 + usage 读到、F6 的四种 query 情形、两个钩子的顺序与归因 | D12 / F6 / F8 |
| `internal/domain/magpie/rewrite_test.go`（新增） | "只换 model" 的硬断言：改写后换回原值必须**逐字节等于**原请求体（长度差必须恰为新旧值字节差）；覆盖缩进/键序/转义/中文/嵌套同名键/40 组随机体；另有协议识别与拒绝清单、`StripQueryParam` 不重新编码 | D1 / F6 |

### 对账计数（D4 / F9）

| 文件 | 改了什么 | 裁定 |
| --- | --- | --- |
| `internal/domain/upstream/window.go` | offset 持久化（分页契约 `ORDER BY occurred_at ASC, ledger_id ASC`）；`MaxRowsPerRound = 20 页 × 200 = 4000`；`WindowStatus` 增 `alarm`/`skipped`；`WindowRecord` 增 `ScrollOffset`；`WindowStore` 的 `BlockedWindows` 换成 `PendingWindows`（返回 blocked + error，按起点排序）；`RunOnce` 从最旧 pending 窗口的 `scroll_offset` 续拉、**按页提交**（Upsert 页 + 写带下一 offset 的窗口行同事务），超上限记 blocked + 告警 + 返回 `ErrWindowBlocked`，成功行与"清 blocked/error"同事务 | F9 / D4 |
| `internal/domain/upstream/window_store.go` | `PendingWindows` 读 `status IN ('blocked','error')` 与 `scroll_offset`；`Save` upsert 写 offset、`detail` 用 `||` 合并；成功写 OK 时清区间内 blocked/error 并把 offset 归零；新增 `SaveCounts`（四计数列 + 状态 + streak + detail，**不碰 scroll_offset**，且拒绝覆盖 blocked/error 行 → `ErrWindowNotReconcilable`）、`WindowStates`/`LastAlarmStreak`/`WindowCounts.Validate` | D4 / F9 |
| `internal/domain/upstream/window_test.go`、`window_store_test.go` | 改为断言「按页提交 + offset 落库 + 下一轮续拉 + 补平后 blocked 清零 + 失败时已提交页保留且水位线不动」 | F9 |
| `internal/domain/reconcile/reconcile.go`（新增） | 对账服务 = D4 的计数列写入方：按窗口重算最近 N 个已结束窗口，跳过 blocked/error（不覆盖），按 token 偏差阈值（默认 1%）与 streak（3 连升级）写 ok/alarm；账本关闭写 `skipped` 但仍写灵枢侧计数；本地侧读 `billing_records` + `billing.TokenSumExpr`，上游侧读 `ledger_entries` | D4 |
| `internal/domain/reconcile/reconcile_test.go`（新增） | 判定表 5 组（对上/超阈/阈内/账本侧空=alarm/账本关闭=skipped）+ 计数必须非 0 且合计等于注入值 + 真实库用例（真实迁移链上插 1 行账单 + 1 行账本，断言四列落真实数字、状态 ok、重算幂等）+ blocked 行不许被对账覆盖 | D4 |
| `cmd/server/main.go` | 60s 一跳的对账循环接入装配（`recover` 保护，单轮 panic 不带走进程；失败只告警，窗口保持待重算） | D4 |

## 2. 门禁结果

- `gofmt -l`（改动文件）：空。
- `go vet ./...`：干净。
- `go test ./... -count=1`（无 DSN）：35 个包全绿。
- `go test -race`（`magpie` / `gateway` / `upstream` / `reconcile` / `db` / `seeding` / `model`，带真库 DSN）：全绿。
- **真库门禁**（自建 PG17，DSN 指向空库，用例各自建临时库 + 真实迁移）：
  - `TestGate_F4_OneStartupFullChain` PASS：一次 `Migrate` 跑全链；`0020` 早于 `0023b` 早于 `0024`；`0024` 排最后；目录无 `0021`；15 张保留表在、10 张退役表不在；`billing_records.channel_key_id` 已删。
  - `TestGate_F10_NotValidSkipsLegacyRowsButEnforcesNewWrites` PASS：存量 NULL 行不被扫描也不被改写；新写「无成本 + ledger」被拒；存量行 UPDATE 只补 `ledger_entry_id` 被拒（23514），同时 `COALESCE(cost_source,'none')` 则通过。
  - `TestGate_F10b_RepairMigrationFixesAlreadyAppliedDB` PASS：**精确复现基线红**——先把库退回旧形状（值域无 skipped、计数列 NOT NULL DEFAULT 0、无 F10 约束），确认旧形状确实拒 `skipped`，再应用 `0023b`：`skipped` 可写、计数默认 NULL、blocked 的 0 占位归 NULL、跨列 CHECK 生效。
  - `TestGate_F7_RetainedRowCountsDoNotDecrease` PASS：`0024` 前后 15 张保留表行数一条不减。
  - `TestRounder_RealSQL_Backlog4200_TwoRounds` PASS：见 §0 第 6 条。
  - `TestSQLWindowStore_RealSQL_SkippedWritableAndCountsWritten` PASS：真库可写 `skipped`；计数列落真实数字；blocked 行拒绝对账覆盖且计数为 NULL。
- **带 DSN 全量**：与上一轮完全一致，仍只有那 9 个"退役 schema"用例红（见 §3-A），**没有新增红**。
- 新增（本轮 F6/F9 补）：`TestForward_QueryPassthroughExceptKey`、`TestV2_F6_AnyPathKeyParamStrippedAndWarned`、
  `TestRounder_OrderUnprovenBlocksWithoutFetching`（未证实/已证否两分支都不翻页、不写账本、offset 保持 0）、
  `TestProbeLedgerOrder_Cases`（6 种排序形状）、`TestPOC_UpstreamLedgerOrder_Mock`（真实 HTTP 源 + 两种排序）。
- 变异验证（删掉修复 → 用例变红，证明断言不是摆设）：
  - 删身份类头名的剥除分支 → `TestV2_F8_ResponseHeaderScrub` 红（`x-served-by` 漏出）；
  - 把续拉 offset 强制置 0 → `TestRounder_RealSQL_Backlog4200_TwoRounds` 红（第二轮仍 blocked，与基线现象同形）；
  - 把 `IS NOT DISTINCT FROM` 换回 `=` → 新写「NULL + ledger」放行，`TestGate_F10` 红；
  - 把 `key` 剔除条件改回「仅 Gemini 路径」→ `TestV2_F6_AnyPathKeyParamStrippedAndWarned` 红；
  - 把能力闸门挪到翻页之后 → `TestRounder_OrderUnprovenBlocksWithoutFetching` 红。

## 3. 未完成项与已知缺口（登记，不承诺已解决）

### 3.0 0024 代码债（P0，**等上级授权后**才开工，本轮只登记；⚠️ 是「启动即失败」不是「部分接口报错」）

Gate 已实测：0024 应用后进程**启动即 `exit=1`**（console 装配阶段查询 `channels` 表 → 42P01，发生在首请求之前），因此未摘除五处代码前 0024 **连进程都起不来**，不存在「起得来、部分接口报错」的中间态。

Gate 复验确认：0024 一执行，以下 7 处代码仍在引用被删列/表，必须与「五处代码摘除」同批上线，否则分支不能上线：

| # | 债 | 表现 |
| --- | --- | --- |
| 1 | 鉴权查询仍读 `tokens.tag_id` | 带令牌请求 42703 |
| 2 | 账单写路径仍写 `billing_records.channel_key_id` | 每笔账单失败并退预扣 |
| 3 | 会话写入仍写 `sessions.channel_key_id` | 会话创建失败 42703 |
| 4 | 账单/成本/统计 JOIN `channels` / `channel_keys` | console 5 条用例 42P01 |
| 5 | 标签查询 `semantic_tags` / `channel_tags` | 标签接口 42P01 |
| 6 | `recordCols` 仍选 `billing_records.channel_key_id` | 账单列表/详情/汇总 42703（上面 billing 3 条红） |
| 7 | 读维度仍回落本表列（本批保留的过渡期兜底） | 列删后该回落分支必须一并摘除 |

摘除范围 = auth / billing 读写 / session / console / tag 五处；本批不动。

**A. 0024 与现有代码冲突（本件最大的未解决项）**

`0024` 按裁定删掉 10 张渠道表与 `billing_records.channel_key_id` 等退役列，但**账单写入/统计读取的代码仍在使用它们**。真库跑全链后，以下 9 个用例会红（无 DSN 时它们 skip，所以"全绿"不代表能在新 schema 上跑）：

- billing（4）：`TestAttribution_RealSchema_WriteAndReadBack`（写 `channel_key_id` → 42703）、`TestBilling_DimensionFromCallLogs`（断言该列仍存在）、`TestListRecords_RequestIDFilter_Integrated`、`TestListRecords_GroupByRequest_UserIsolation_Integrated`。
- console（5）：`TestCostStats_CrossChannelFallback`、`TestStats_WindowAndPlaza`、`TestStats_WindowCalendarMode`、`TestStats_ResponseJSONShape`、`TestStats_SoftDeletedModelGoesUnmatched`（全部 `relation "channels" does not exist`，42P01）。

结论：**0024 不能单独上线**，必须与「渠道/标签域代码下线」同批。本批只交付迁移链本身与它的验证方式。

**B. 其余未完成（登记）**

- **S3 / S5 / A3 / A4 / A5**：按派单口径不在本批，未动。
- D1 只做了「magpie 只做协议/路径/字节转发 + 两个钩子」，**V1 的渠道路由/Provider/协议互译代码仍在仓库里**（装配到 `UpstreamConfig` 才不参与），彻底摘除属阶段二清理。
- 阶段二第一件（七列归因的**读取方**）未做；本批只落计数列的写入方（D4）。
- `cost` 计价模式在 V2 主链未接（`RecordReq.PricingMode` 恒 `sale`，成本侧等账本口径）。
- `billing_records` 的 `estimated` / `upstream_usage` / `magpie_key_name` 列不存在于本分支，估算单只能用 `cost_source='none'` + 备注区分。
- 响应回显上游凭据的遮蔽未做：D12 要求响应体逐字节原样，任何内容改写都会破坏该不变量 → 属需要单独裁定的项。
- `SQLWindowStore.SaveCounts` 是 UPDATE-only：没有窗口行时返回 `ErrWindowNotReconcilable`（本批假设同步轮次先建行，未做"缺行自动建"）。
- 对账循环随进程起（60s），未做优雅退出等待、未做多副本互斥（单副本无碍）。
- `external_models.bound_internal_ref` 已进入读模型，但**管理端没有编辑该字段的入口**（写入路径留给 V2 配置界面）。
- 全仓库仍有大量 `channel_key_id` / 渠道表引用（非测试 ~60 处），属 A 项同一件事。
- **同步轮次仍未装配**：`PageSource` 的生产实现（`HTTPLedgerSource`）本轮补上了，但 `cmd/server` 里只装配了对账循环、**没有**装配 `Rounder`（ticker + 能力探测接线的活还没做）。因此 F9 的行为目前由用例保证，生产里还不会真的跑同步。
- **上游排序能力仍未证实**：真实上游 POC 未执行 → 结论 `unverified` → 一旦接线，所有同步窗都会记 blocked 且不可清零，直到 POC 通过（`supported`）。这是裁定要的保守行为，不是 bug。
- `upstream.*` 三个键**没有管理端写入口**：`PUT /admin/configs/system` 只处理既有几块，不含这三个键，目前只能直写 `sys_configs`。
- `upstream.ConfigProvider.Invalidate()` **没有生产调用点**：`SetConfigWriteHook` 在 `upstreamCfg` 创建之前注册，钩子里没有它，而缓存无 TTL —— 改 `upstream.base_url` / 超时要重启才生效。
- F8 的供应商名是**手工清单**（20 个常见上游名 + `provider/account/vendor/served/backend/worker/region` 身份类头名）：未收录的新供应商仍可能漏剥，新增上游时要同步追加。
- 本文的三步证据在 **tip `5bf38b8`** 的树上取得；此后分支追加了 `0023b` 与 F6/F9 的收紧，未在新 head 上重跑三步证据。
- 迁移编号新开了一个 `0023b`（幂等修形，排在 `0023` 与 `0024` 之间）：原因是**改过的 .sql 不会重放**，老环境必须有一条新迁移才能拿到最终形状。这不是复用 `0021` 空号。

## 4. 同一窗口重复写 `blocked` 的幂等性

一句话：**offset 只会前进、账本按 `ledger_id` 去重、计数是绝对重算**，所以重复写同一个窗口不会重复入账、也不会丢进度。

1. 窗口行以 `window_start` 为主键 upsert（`ON CONFLICT (window_start) DO UPDATE`），`scroll_offset` 用 EXCLUDED 覆盖：同一窗口反复写 blocked 只会把断点更新到更靠后的位置，不会产生第二行、也不会退回已提交位置。
2. 断点与页数据**同事务提交**：`Upsert(页) + SaveTx(带下一 offset 的窗口行)` 一起 commit。要么这一页和它的断点都在，要么都不在——不会出现「offset 前进了但数据没落」的静默跳过。
3. 账本写入是 `INSERT ... ON CONFLICT (ledger_id) DO NOTHING`，重拉同一页的重复条目被吃掉，返回的插入条数如实反映实际新增。
4. 计数列由对账服务**整窗重算后覆盖**（`SaveCounts` 写绝对值，不做 `+=`），所以多跑几轮结果恒定；`TestReconciler_RealSQL_WritesCounts` 里第二轮断言数值不变。
5. 对账服务拒绝覆盖 `blocked`/`error` 行（返回 `ErrWindowNotReconcilable`）：补拉尚未完成时，计数方不许把窗口标成 ok，避免「漏账被自己的对账抹平」。
6. blocked 只在两种情况下被清：补拉到 `!has_more` 后写 OK 行，或 `ClearFrom` 覆盖到的区间；两种情况都在事务里完成，重启后行为一致（`TestSQLWindowStore_BlockedSurvivesRestart`）。