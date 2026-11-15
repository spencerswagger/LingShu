# 灵枢 V2 阶段一 · 验收标准（Gate 照单判）

> 2026-11-15 · Spec（产品）· 判据只写「怎样算做完」，不写实现方案。
> 依据：`docs/designs/2026-11-15-v2-p1-data-contract.md`（Ark，分支
> `docs/lingshu-v2-p1-data-contract` @ `7a414e8`，唯一数据层依据）＋ 阶段一第一切片
> （`feat/lingshu-v2-sync-config` @ `9fb813d`）。
> 范围：迁移 0019–0023、同步器（含账本拉取）、四协议透传、归因、账本落库。
> **不做**：Embeddings；不裁定「网关代码改到什么程度」（负责人待确认）；不改既有统计口径与接口契约。

---

## 0 判定原则（Gate 先用这四条筛证据）

1. **证据三件套**：命令（可复制）+ 原始输出（贴原文，不改写）+ 复现步骤。截图只用于界面，不能替代 SQL/日志。
2. **`skip` 不算通过**。迁移集成测试在未设 DSN 时 `t.Skip`（`internal/db/migrate_test.go`），
   阶段一验收必须在有 PG 的环境用 `LLMGATEWAY_TEST_PG_DSN=...` 实跑，否则该项判「未验」= 失败。
3. **判过要能复现失败**：每条 P0 判据必须能说出「改哪里会让它变红」（变异验证思路）。
4. **判不了就退回，不猜**。判据依赖的键名/顺序/枚举若在文档里有互斥写法，按 §7 回报负责人，不许自行择一。

两个**不是 bug** 的预期态（别开错单）：
- `credit_holds` 阶段一只建不写，**恒为 0 行**；
- 0021 是唯一 DROP 文件，迁移重跑时 `DROP … IF EXISTS` 命中「已不存在」不算异常。

---

## 1 迁移 0019–0023（6 条）

| # | 判据 | 通过条件 | 失败条件 | 证据形式 | 级别 |
|---|---|---|---|---|---|
| M1 | 编号合规 + 唯一 DROP 文件 | `SELECT name FROM applied_migrations ORDER BY name DESC LIMIT 1` = `0018_call_logs_req_params.sql`；`migrations/` 下存在 0019/0020/0021/0022/0023 五个文件、**不存在** `0019_v2_facade.sql`、`0020_reconcile_blocked_status.sql`；`grep -nE '^[[:space:]]*DROP|CONCURRENTLY|VACUUM|CREATE DATABASE'` 只在 0021 命中（11 条 DROP TABLE + 3 条 DROP COLUMN） | 撞号 / 五个文件外还有改动 / 非 0021 文件出现 DROP / 出现事务内不可执行语句 | 两条命令原文输出 + `ls migrations \| tail -6` | P0 |
| M2 | 全链一次跑通 + 二次幂等 | 空库 `go test ./internal/db -run TestMigrate_FullChain -v`（带 `LLMGATEWAY_TEST_PG_DSN`）绿，日志「已按序应用 N 个迁移」；已到 0018 的存量库启动一次 → 新增 5 条；再启动 → 新增 0 条（`SELECT count(*) FROM applied_migrations` 前后相同） | 任一步报错 / 二次执行又新增 / 需要手工分次执行才能过 | 测试输出 + 两次 `applied_migrations` 计数 + migrate 返回计数日志 | P0 |
| M3 | 0021 中途失败自动回滚（事务性） | 临时库里先建 `CREATE VIEW v_dep AS SELECT id FROM channels`，再跑 migrate：0021 报 `cannot drop table channels because other objects depend on it`；断言 (a) `applied_migrations` 无 0021；(b) 11 张退役表 `to_regclass` 全非空；(c) `billing_records.channel_key_id`/`sessions.channel_key_id`/`tokens.tag_id` 仍在；(d) 删 view 后重跑 → 0021 成功（证明可重试） | 任一张表/列已消失（= 部分执行、回滚未生效）；或重跑报错 | 一次失败输出 + 四项 SQL 结果 + 重跑输出 | P0 |
| M4 | 三步证据「最小充分」 | ① `pg_dump -Fc` exit 0 且文件 >1 KiB，记录文件名/大小/sha256；② `pg_restore` 到**临时库** exit 0，且 `diff before-all.txt restored-all.txt` 为空（全表行数一致）；③ 迁移后 `diff before-retained.txt after-retained.txt` 为空 + 3d 删干净查询为空输出；④ **还原出的 dump 里 `billing_records.channel_key_id` 非空行数 > 0**（证明唯一永久损失可取证） | dump 缺失/sha256 未记 / restore 失败或行数不一致 / dump 里 channel_key_id 已无数据（= 备份不能取回历史渠道密钥名） | 四类产物原文（dump 元信息、两个 diff 的空输出、3d 空输出、④ 的 count）+ 三条命令退出码 | P0 |
| M5 | 保留表行数不许变少 | 集合 = 0021 **前** public 下所有表 − 11 张退役表（当前 15 张，**含 `watchlist_items`、`price_change_alerts`**）；基线在服务停止时取、复检在 0021 后取；逐表 `after >= before`，停服前提下 `after == before` | 任一表 `after < before`（数据丢失）→ 立即中止；若已 commit，用 M4 的 dump 全量恢复 | `counts-retained.sql` 两次输出 + `diff` 结果 + 取数时的停服说明 | P0 |
| M6 | 删除断言 + 反向断言（防多删） | 3d 查询空输出；`information_schema.columns` 查上面 3 列 = 0 行；`pg_indexes` 无 `idx_billing_key_time`/`idx_sessions_key`；**反向**：`call_logs` 仍有 `channel_id`/`channel_key_id`/`auto_model_name`，`channel_protocols`/`protocol_error_stats`/`watchlist_items`/`price_change_alerts` 仍在 | 有表/列残留（迁移未生效）；或 `call_logs` 三列被误删、保留表被误删 | 两个 SELECT 的输出（前者空、后者为清单）+ `\d call_logs` | P0 |

---

## 2 同步器与账本拉取（6 条）

| # | 判据 | 通过条件 | 失败条件 | 证据形式 | 级别 |
|---|---|---|---|---|---|
| S1 | 严格串行、不可并发 | 单轮内步骤按注册顺序逐个 await（同 goroutine）；上一轮未结束时新触发返回 `*SkipError`（`ErrBusy`），**不排队**；注入 sleep 3s 的步骤 + 并发触发 10 次 → 上游实际只被调 1 轮 | 出现 2 次并发调用（日志时间重叠 / 计数器 =2）；或积压轮次在网络恢复后一起打上游 | `TestSyncer_SerialSteps`、`TestSyncer_ConcurrentRunRejected`、`TestSyncer_StartNeverOverlaps` + 并发触发日志 | P0 |
| S2 | 退避序列恰 5/20/60s、最多 4 次调用 | 首次立即执行；失败后间隔依次 5s/20s/60s（假时钟精确、真时钟 ±100ms）；第 3 次重试失败后**不再调用、不再退避**，返回可诊断错误（`errors.Is` 可穿透）；零值策略 = 只调 1 次 | 第 5 次调用 / 间隔是 1-2-4s 或 5-10-20s / 退避耗尽后静默成功 / 零值策略偷偷重试 4 次 | `TestDefaultPolicy_Sequence`、`TestDo_RetrySequenceExactly`、`TestDo_StopsAfterThirdRetry`、`TestDo_ZeroPolicyNoRetry` + 生产日志字段 `step/attempts/retries/backoff` | P0 |
| S3 | 60s 窗重叠 2 窗 + 幂等吸收 | 构造 `occurred_at` 迟到 30s、90s 各 1 条 → 均落库；同一 `ledger_id` 出现在相邻两窗 → 只有 1 行；迟到 150s 不承诺落库，但必须被对账窗口以笔数/token 偏差（`detail`）暴露 | 迟到 ≤120s 的条目丢失；同一 ledger_id 出现 2 行；迟到 >120s 完全无痕（对账也看不见） | 两轮运行后的 `SELECT count(*) FROM ledger_entries` + 两行 `reconcile_windows.detail` | P0 |
| S4 | offset 不持久化（不是游标） | 库内无 offset/cursor 列（`ledger_entries` 恰 9 列、`reconcile_windows` 无 offset）；进程重启后首轮日志 `offset=0`，且不读任何「上次位置」 | 新增 cursor/offset 持久化表或列；重启后从中间 offset 续拉 | `information_schema.columns` 清单 + 重启首轮日志 | P1 |
| S5 | 单轮 20 页上限 → blocked，且不漏 | 构造 >20 页（>10000 条）的窗口：本轮 `reconcile_windows.status='blocked'` + 告警 1 条；后续轮次把余量补齐，最终 `ledger_entries` 去重后条数 = 上游条数，**无永久缺口** | 超限后仍写 `ok`（静默丢条目）；或超限部分永久缺失 | 该窗 `status`/`detail` + 最终条数比对（上游条数 vs 去重条数） | P0 |
| S6 | 降级/未配置不清快照 | 上游不可达、`degrade` 打开、base_url 为空三种情形：本轮 `*SkipError`、上游调用 0 次、不退避；`internal_models` 行数不变；**不因不可达就把全部 `stale` 置 true**（`stale=true` 只表示「网关正常应答但本轮未见」） | 快照被清/行数减少；不可达时全表标灰；对着空地址狂重试 | `TestSyncer_GateSkipsWithoutCallingSteps` + 前后 `SELECT count(*), count(*) FILTER (WHERE stale) FROM internal_models` | P1 |

---

## 3 四协议透传（6 条）

**「透传成功」的定义（横向，P5 展开）**：请求体除 model 值区间外**逐字节**相同；query 逐字原样；
请求头剔除强制黑名单后逐 name/value 原样；响应头只丢 hop-by-hop；响应体（含 SSE 分包、错误体）逐字节原样。

| # | 判据 | 通过条件 | 失败条件 | 证据形式 | 级别 |
|---|---|---|---|---|---|
| P1 | Chat `/v1/chat/completions`（非流式 + 流式各一次） | 非流式：出站 body 与入站 diff **只有 model 那一行的值**（键序/空白/数字写法/转义不变），未注入 `include_usage`；响应状态/头/体逐字节一致。流式：SSE 分包边界与总字节数一致，末段 usage 原样可读 | 重新序列化（键序变化/空格变化）、改了 `stream`/`stream_options`、响应被改写或补帧 | stub 上游保存的原始 request/response 字节 + `diff` 只命中 model 行 + `TestForward_OpenAIChat_SwapsOnlyCredentialAndModel`、`TestRewriteModelJSON_PreservesAllOtherBytes`、`TestForward_StreamPassthrough` | P0 |
| P2 | Responses `/v1/responses`（非流式 + 流式） | 同上；流式取 `response.completed`/`incomplete` 事件的 `response.usage`，事件帧原样透传（不重排、不补 `completed`） | 事件被重排/补齐；usage 事件被改写 | stub 原始字节 + `TestUsageCollector_Stream`（responses 分支） | P0 |
| P3 | Messages `/v1/messages`（非流式 + 流式） | 同上；`anthropic-version`、`anthropic-beta`（多值）逐值保留；`message_start`/`message_delta` 帧原样；`stop_reason` 不乱改 | 头被改写/丢值；帧被合并或改写 | 请求头 diff（逐 name/value）+ 原始 SSE 字节 + `TestForward_Anthropic_And_Gemini_Rewrite` | P0 |
| P4 | Gemini `/v1beta/models/{m}:generateContent` 与 `:streamGenerateContent` | 只有 path 中 `models/` 之后的 model 段被替换（`:generateContent` 等后缀原样）；body 逐字节不变；query 除鉴权 `?key=` 外逐字原样；`?alt=sse` 保留 | body 被改；后缀/路径被改；`?key=`（租户令牌）被转发到上游 | 出站 URL 原文 + body 字节 diff 为空 + `TestGeminiPathModel`、`TestRewriteModelJSON_PreservesAllOtherBytes` | P0 |
| P5 | 透传三面（body/query/header）+ 身份不泄漏 | 请求头：命中强制黑名单（hop-by-hop、`host`/`content-length`/`content-type`/`content-encoding`/`accept-encoding`、`cookie`/`forwarded`/`x-real-ip`、`authorization`/`x-api-key`/`api-key`）一律不带，其余逐值原样；`Accept-Encoding` 固定 `identity`；出站只注入本次令牌对应的凭据头（一令牌一把）。响应面：**客户端可见面（响应头+响应体+错误体）不出现上游 provider/渠道名/密钥名/上游模型名，也不出现本次 callerKey**（唯一豁免：凭据回显遮蔽后替换为 `<redacted>`，无命中时字节与 `Content-Length` 都不变） | 黑名单头被带去上游；凭据在 query 里转发；客户端能看到上游身份或凭据明文；遮蔽把无关内容也改了（无命中却字节变化） | 头 diff 脚本逐 name/value 输出 + 一次故意回显 key 的上游 stub + `TestMask_NonStream_Miss_ByteIdentical`、`TestMask_NonStream_Hit`、`TestMask_Stream_Miss_ByteIdentical`、`TestMask_DoesNotGeneralize` | P0 |
| P6 | 流式末尾抓不到 usage 的判定 | **判通过**（透传不因此失败）：客户端字节已完整回写（结束标记由上游给，灵枢不许补）；同时必须留下可判定证据 —— 有 usage 则 `upstream_usage` 存原文；无 usage 则 `estimated=true` 且原因可查（`done_no_usage`/`truncated`/`client_cancelled`） | usage 缺失被静默当成 0 计费且无 `estimated` 标记（等于送钱）；截断被记成正常完成且无归因；灵枢自行补 `[DONE]`/`message_stop` | 一次「有空响应但无 usage」的 stub + 账单行字段（`estimated`/`upstream_usage`/`error_message`）+ `TestUsageCollector_Stream` | P1 |

---

## 4 归因（5 条）

| # | 判据 | 通过条件 | 失败条件 | 证据形式 | 级别 |
|---|---|---|---|---|---|
| A1 | 上游标识缺失 → 留 NULL，不猜 | 上游不回标识、账本也未回填时：`upstream_provider`/`upstream_account`/`upstream_model` 为 **SQL NULL**（不是 `''`、不是 `'unknown'`、不是请求里的对外模型名）；`upstream_protocol` 必有值（由入站路径定，不依赖上游） | 用空串/占位符/对外模型名兜底；协议字段缺失或为 NULL | `SELECT count(*) … WHERE upstream_provider='' OR upstream_account='' OR upstream_model=''` = 0；一次无标识头请求的账单行 dump | P0 |
| A2 | 协议枚举只在四个取值内 | 值域 = `openai-chat`/`openai-responses`/`anthropic-messages`/`gemini`；写第 5 个值（如 `openai`）被 DB 拒（SQLSTATE 23514） | 出现第五个值；约束不存在；值域靠代码注释保证 | `pg_constraint` 查 `ck_billing_upstream_protocol` + 一次故意写入的报错原文 | P0 |
| A3 | 改价/改绑定后历史可解释 | 请求落库后修改 `external_models.sale_rates` 与 `bound_internal_ref`：历史行的 `external_model_name`/`internal_model_id`/`upstream_model`/rates 快照/金额**逐字段不变**；新请求才用新绑定与新价 | 历史行被回填式改写；或历史行只剩一个模型名（分不清对外名与实际服务名） | 改前/改后两次 SELECT 逐字段 diff 为空 + 新请求行取到新绑定 | P0 |
| A4 | 流式首字节定稿 + 同事务落库（无中间态） | 首字节后归因即固定、中途不改写；usage 缺失/截断时归因仍完整；库里不存在「有归因没金额」或「归因半截」的行 | 流结束后归因与首字节不一致；存在 `upstream_protocol` 非空但 `credits_consumed`/`tokens` 为空的行 | 首字节后断开连接的 stub + `SELECT count(*) WHERE upstream_protocol IS NOT NULL AND (credits_consumed IS NULL OR tokens='{}')` = 0（completed 行） | P0 |
| A5 | `cost_source` 与金额自洽 | `cost_credits IS NULL` 时 `cost_source ∈ {NULL,'none'}`；`cost_source='ledger'` 时 `cost_credits` 非空且等于对应账本条目 `cost_credits` | 出现 `cost_source='ledger'` 但金额为空/与账本不等；出现 NULL 金额却标 `ledger`/`amortized` | 写侧用例 + 反例查询 0 行（**注意**：0020 只有枚举 CHECK，没有跨列约束，这条只能由应用层用例判，见 §7-F8） | P1 |

---

## 5 账本落库与账单联接（5 条）

| # | 判据 | 通过条件 | 失败条件 | 证据形式 | 级别 |
|---|---|---|---|---|---|
| L1 | 九字段必须齐 | `ledger_entries` 恰好 9 列，列名/类型/可空与 0022 一致；写入载荷长度 == 9（`LedgerFieldCount`）且顺序与 DDL 一致；少一个/多一个字段被明确拒绝（不落半条） | 列数 ≠ 9；载荷 8 或 10 个值被接受；缺字段静默补零落库 | `SELECT count(*) FROM information_schema.columns WHERE table_name='ledger_entries'` = 9 + `TestLedgerRecord_Validate` + 一次 8 字段写入的报错原文 | P0 |
| L2 | `ledger_id` 幂等（重复写入不重复计数） | 同一 `ledger_id` 写两次 → 1 行、无报错；`ON CONFLICT (ledger_id)` 冲突目标与唯一索引逐字一致（`\d ledger_entries` 可见 `uq_ledger_entries_ledger_id`）；2 万条乱序含 30% 重复 → 落库条数 = 去重后条数；重跑同一窗口 → 新增 0 | 报 `there is no unique or exclusion constraint matching the ON CONFLICT specification`；出现 2 行同 ledger_id；批量写 23505 整批失败（靠吞错掩盖也算失败） | `\d ledger_entries` + 一次性灌入脚本的「灌入数/去重数/落库数」三行 + 重跑新增 0 | P0 |
| L3 | 金额单位与既有列同型，可直接加减 | `ledger_entries.cost_credits` 与 `billing_records.cost_credits`、`credit_wallets.balance` 同为 `NUMERIC(20,5)`；一条 SQL 直接 `l.cost_credits + b.cost_credits` 求和无需 cast；不出现 BIGINT 微元/浮点/其他精度 | 精度/标度不一致；需要 cast 才能相加；出现两套金额单位 | 三行 `information_schema` precision/scale 输出 + 一条跨表求和 SQL 的结果 | P0 |
| L4 | 窗口行自证 + 同事务 | `reconcile_windows.window_end` 恒 = `window_start + 60s`（写错位窗被 DB 拒 23514）；同窗重写走 `ON CONFLICT (window_start) DO UPDATE`，不产生第二行；账本条目与窗口行在同一事务（构造条目写入失败 → 窗口行不落） | 出现 61s/59s 的窗口行；同窗两行；条目落了窗行没落（或反之） | 尝试写错位窗的报错原文 + 同窗重写前后的行数 + 一次注入失败后的两表计数 | P1 |
| L5 | 账单 ↔ 账本一对一，历史行不被索引污染 | `billing_records.ledger_entry_id` 全 NULL 的历史行不进部分唯一索引（回填前后该索引条目数只增对应行）；同一账本条目挂第二行账单 → 23505，不许静默覆盖 | 历史 NULL 行被算进唯一索引导致回填失败；同一 ledger_id 挂多行账单；回填报错被吞 | `\d billing_records` + 回填前后索引计数 + 一次重复回填的报错原文 | P0 |

---

## 6 P0 必过清单（不过不许进阶段二）

```
迁移   M1 M2 M3 M4 M5 M6
同步器 S1 S2 S3 S5
透传   P1 P2 P3 P4 P5
归因   A1 A2 A3 A4
账本   L1 L2 L3 L5
```

P1（建议项，可带缺陷进阶段二但要登记）：`S4`（offset 不持久化）、`S6`（降级不清快照）、
`P6`（usage 末尾失败判定）、`A5`（cost_source 自洽）、`L4`（窗口自证）。

---

## 7 计划自相矛盾 / 判据无法客观化（回报，不自行抹平）

| # | 冲突点 | 两处原文 | 影响 | 需要谁拍板 |
|---|---|---|---|---|
| F1 | 上游运行参数有**三套**写法 | ① Ark §9.4：`upstream.base_url` + `upstream.timeouts`（JSON，含 `stream_first_byte_ms`/`stream_idle_ms`）+ `upstream.degrade`（JSON 三开关）；② 第一切片已实现：`upstream.base_url` + `upstream.timeout_ms`（单值 30000）+ `upstream.degrade`（bool）；③ 转发链实现从**应用配置** `magpie.*`（`base_url`/`auth_header`/`*_ms`/`subscription_prefixes`）读，不在 `sys_configs` | 验收无法钉死键名与形态；单值 `timeout_ms` 表达不了「流首包 30s / 流空闲 120s」两条超时，P6 与流式超时判据缺参数 | 负责人（一句话定以哪套为准） |
| F2 | 保留表清单少 2 张 | Ark §5 的 `counts-retained.sql` 列 13 张；同文 §4.2 明确 `watchlist_items`/`price_change_alerts` 保留不删。0021 前实际保留表 15 张 | 按现清单判 M5 时，这两张表丢行不会被发现 | Ark（补清单或改判据为动态集合） |
| F3 | 对账状态缺 `skipped` | 0023 `CHECK (status IN ('ok','alarm','error','blocked'))`；`docs/designs/2026-11-15-usage-billing-loop.md` §5 要求「未配 admin_key → 每窗记 `skipped`」 | 按现口径写 `skipped` 直接被 CHECK 拒（23514），对账任务每窗报错 | Ark + Core（改枚举或改口径） |
| F4 | 0021「最后执行」与迁移器顺序矛盾 | §11 要求 0019→0020→0022→0023 先、0021 最后；`migrate.go:16-70` 按**文件名字典序**应用（0021 必然在 0022/0023 之前） | 「全新库一次启动」跑完整链时无法满足该顺序；若靠手工分两次执行，`applied_migrations` 顺序事实与文档不符 | 负责人（定「一次启动」还是「分两次」，判据随之改） |
| F5 | 透传落点两套，账本观察点不同 | Ark §9.2：改 `gateway/handler.go` 中段、新建 `domain/upstream/client.go`，账单与归因留在主链；已实现分支 `feat/lingshu-v2-p1-magpie-passthrough`：`domain/magpie/` 整包接管 `/v1`+`/v1beta`，`handler.go` 明写「这一层不做计费扣费」，**本期不注入 `OnComplete` 钩子**（不写 `billing_records`） | 决定「账单落库」判据的观察点，也决定 §4 归因由谁产出 | 负责人（「不改网关代码到什么程度」的同一问题） |
| F6 | 「query 原样」与 Gemini 原生鉴权互斥 | Ark §9.3「query 原样」；实现 `magpie/proxy.go:129` 对 Gemini 原生 `?key=` **必须剔除**（那是租户令牌） | 判据若写「query 原样」，实现必然违规；若写「可剔 key」，需写清只剔这一个参数 | Ark（补例外条款，与 §10-F 同性质） |
| F7 | 「保留表行数逐字相同」在不停服时不可判 | Ark §5 判点 C 要求 `diff` 逐字相同 | 生产有并发写入，基线与复检之间必然漂移 → 该条要么改为「不许变少」，要么要求停服取证 | 负责人（定停机窗口，否则判据降级为 `>=`） |
| F8 | 响应面口径互斥 | Ark §9.3「从响应头剔除全部上游标识头，客户端永远看不到」；magpie 契约 §2「响应头只丢 hop-by-hop，其余原样」 | P5「身份不泄漏」按哪个判：留 `x-request-id`/`server` 之类是否算泄漏 | Ark（二选一，判据跟着改） |
| F9 | 20 页上限「不会漏」缺前提 | Ark §6.2「超限 → blocked，下轮从新区间重来（重叠窗口已覆盖，不会漏）」；未写 offset 翻页**排序方向** | 若上游按 `occurred_at DESC` 翻页，超出 20 页的最老条目落在下轮区间之外 → **永久漏账**；只有升序翻页该结论才成立 | Ark + Core（把翻页序写进契约，或改为「blocked 后从该 window_start 重拉」） |
| F10 | `cost_credits IS NULL → cost_source ∈ {none,NULL}` 只有文字 | Ark §3 表头写了该不变量；0020 DDL 只有两个枚举 CHECK，**没有跨列 CHECK** | 该条无法由 DB 判，只能由写侧用例判（已按此写 A5） | Ark（确认是否补 CHECK，或接受仅用例保障） |

---

## 8 切片报告

阶段一验收标准已出：5 块 28 条（P0 23 / P1 5），每条带通过/失败条件与证据形式。
P0 卡在四件事：迁移可回滚与删干净、同步器串行与退避、四协议字节级透传、账本九字段与幂等。
发现 10 处口径冲突（配置键三套、对账缺 `skipped`、0021 执行顺序、透传落点两套、20 页上限漏账前提等），需负责人拍板，我不自行择一。