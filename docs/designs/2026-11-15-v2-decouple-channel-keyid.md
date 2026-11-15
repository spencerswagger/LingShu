# V2 阶段一 · 第四切片：渠道域摘除的「不用点头」那一半

日期：2026-11-15　作者：Core（后端）　状态：Step 1 已实现并验证，Step 2 待拍板

## 1. 目的与边界

0021_v2_drop_legacy 会删掉三列（`billing_records.channel_key_id`、`sessions.channel_key_id`、
`tokens.tag_id`）并 DROP 渠道/标签域 10 张表。本切片**只做不用拍板的那一半**：
把内部链路从「依赖被删列」改成「不再依赖」。**不做**：不动对外契约、不改返回结构、
不改统计口径、不动迁移文件、不整包删渠道域。

## 2. 结论先行：56 处不是「读写 / 接口」三分，而是三类

按「摘掉之后会不会改行为」分，得到的分界线比按代码位置分更硬：

| 类别 | 性质 | 能否本轮摘 | 理由 |
| --- | --- | --- | --- |
| **A 数据面**（账单的渠道密钥维度） | 只是「这条账单当时命中哪把密钥」的落库与展示 | ✅ 能 | 该维度在 `call_logs.channel_key_id` 有**1:1 保留快照**（`call_logs.billing_id` 是 UNIQUE），换供数来源值不变 |
| **B 控制面**（会话粘性 / 令牌标签路由） | 参与路由决策与恢复的**状态** | ❌ 不能 | 摘掉 = 改路由行为（重启后粘性丢失、令牌默认路由失效），属「渠道/标签域下线」那一刀，要点头 |
| **C 展示面**（渠道名/密钥名/按渠道聚合） | 名字来自被 DROP 的表 | ❌ 不能 | 名字的**唯一来源已被删**，只能选「恒空 / 下线字段 / 换供数」，是产品决定 |

「56 处」按这三类落位：

| 类别 | 处数（非测试） | 处置 | 影响对外 |
| --- | --- | --- | --- |
| A · 账单写路径（列清单 / INSERT / NULL 归一 / 字段注释） | 4 | **本轮摘除**：不再写该列 | 无（值本来也只在展示层出现） |
| A · 账单读路径（`recordCols` / 扫描 / 列表 / 详情） | 3 | **本轮摘除**：改由 `call_logs` 快照回填 | 无（字段仍在、值不变） |
| A · 账单按密钥过滤（`recordWhereOn`） | 1 | **本轮改判**：`EXISTS(call_logs … channel_key_id=$2)` | 无（命中结果不变） |
| B · 会话（`sessionCols` / INSERT / ON CONFLICT / 会话列表列与 WHERE） | 5 | 未摘，**Step 2 前置** | 摘掉会改会话粘性/切换判定 |
| B · 令牌 `tag_id`（列清单 / INSERT / 扫描 / 请求装配） | 5 | 未摘，**Step 2 前置** | 摘掉会改令牌默认路由；`TagID` 还在令牌 API 里 |
| C · 统计/成本页 JOIN `channel_keys`/`channels` | 6 | 未摘（表已删，随域下线） | 会：这几个页面的渠道维度要定方案 |
| C · 名称映射（`keyInfos` / `channelNameByKeyID` / `channelNamesByKeyIDs` / dev 白名单） | 6 | 未摘（同上） | 会：渠道名/密钥名恒空的兜底要定方案 |
| 日志面 · `call_logs` 自有列（网关 6 处 + 调用日志装配 2 处） | 8 | **保留不动** | 无：0021 明确保留 `call_logs.channel_id/channel_key_id` |
| 渠道域自身（`channel/*` 8 处、`tag/store.go` 等） | 18 | 随域整包下线 | 接口整体 410（上一刀已定） |

## 2.5 本批边界（2026-11-15 预审后收敛）

**0021_v2_drop_legacy 摘出本批**，本批迁移只含 `0019 / 0020 / 0022 / 0023`（并且这四个文件与
`docs/designs/2026-11-15-v2-p1-data-contract.md` 一律取自架构分支，本分支不留副本）。
新增 `internal/db/migrate_batch_test.go::TestMigrationBatch_V2Phase1` 守住这条边界：
以后再有人把 0021 塞回来，包级用例立刻红，而不是等有 DSN 的环境才炸。

连带口径修正（**采样方法：`grep -rn` 全仓 .go**）：

| 度量 | 实测 |
| --- | --- |
| `channel_key_id` | 135 处 / 48 文件（**非测试 61 处 / 22 文件**） |
| `tag_id` | 37 处 / 11 文件（**非测试 20 处 / 6 文件**） |

上一轮报的「56 处 / 21 文件」是把「列名 + 少量上下文」当成了全量，低估了一半以上。
0021 一执行等于**渠道域整体功能下线**（仪表盘、成本页、会话、调用日志、渠道域全挂），
不是「schema 清理」——这也正是它必须单独成批、且要同时改代码的原因。

**写路径恢复**（重要）：本批不含 0021，`billing_records.channel_key_id` 还在。若这轮就把写入摘掉，
既有统计/成本页会在这批新数据上丢渠道维度（`billing/stats.go` 的按密钥 TOP 是 INNER JOIN
`channel_keys`，新行会直接消失；`console/cost.go` 的实体解析会退化成 `channel_id=0`）——那是**改统计口径**。
所以：**读/过滤**切到 `call_logs` 快照（已证等价），**写**保留；摘写路径与 0021 同批进行。

## 3. Step 1 改了什么（可合入、行为不变）

1. **写路径**：本批**保留**写 `channel_key_id`（见 §2.5）；摘它随 0021 同批（该改动已在历史提交里备好）。
2. **读路径**：`recordCols` 去掉该列（25 → 24 列），`sqlScan` 同步；新增 `fillChannelKeys`，
   在 `ListRecords` / 归并列表 / `GetByBillingID` 之后用**一条**批量查询
   `SELECT billing_id, channel_key_id FROM call_logs WHERE billing_id IN (…)` 回填。
   - 为什么不用 `recordCols` 里的标量子查询：`recordCols` 被三条查询复用（直查 / 归并 CTE / 详情），
     子查询要么处处给外层表起别名，要么在归并里落到错误的表上；批量回填只有一次往返。
   - 缺 `call_logs` 行（拒绝类账单）→ 留 0，与历史「未进入渠道」语义一致。
3. **过滤**：`recordWhereOn` 的 `channel_key_id = $2` 改成
   `EXISTS (SELECT 1 FROM call_logs clk WHERE clk.billing_id = <外层>.billing_id AND clk.channel_key_id = $2)`。
   - **关联列必须限定到外层表**：写裸 `billing_id` 会解析成 `clk.billing_id = clk.billing_id`（恒真，
     PR #63 踩过同类坑）。无别名时固定用 `billing_records.billing_id`。
   - 参数位一个不平移（仍是 9 个），既有 SQL 断言只改谓词形状。
4. **写路径的幂等预检查**不付这次额外查询：`GetByBillingID` 拆成「裸扫描 `scanByBillingID`」+「回填」，
   `Service.recordByBillingID` 优先走裸扫描（接口可选实现，其他 `Store` 实现回落 `GetByBillingID`）。

## 4. 证据

### 4.1 真库（自建 PG17 + 0001–0023 全量迁移）

| 用例 | 迁移前 | 本轮 |
| --- | --- | --- |
| `TestListRecords_RequestIDFilter_Integrated` | ❌ 42703 `column "channel_key_id" does not exist` | ✅ PASS |
| `TestListRecords_GroupByRequest_UserIsolation_Integrated` | ❌ 42703 | ✅ PASS |
| `TestAttribution_RealSchema_0021Conflict`（冲突地雷） | ❌ 固定失败 | 已改判为 `TestBilling_Post0021Schema_DimensionFromCallLogs` ✅ PASS |
| `TestAttribution_RealSchema_WriteAndReadBack` | 靠 `ADD COLUMN` hack 才能跑 | 删掉 hack ✅ PASS |
| `TestPost0021Schema_RemainingBlockers` | — | 断言 schema 事实后 SKIP（明示剩余两处控制面） |

新用例断言的不变量：插入成功 / 列表回填 42 与缺日志行回填 0 / 详情回填 / **按该维度过滤双向**
（命中 1 条、不存在的密钥 0 条）/ 汇总可跑。

### 4.2 变异验证（删掉修复必红）

| 变异 | 结果 |
| --- | --- |
| `EXISTS` 关联列写成裸 `billing_id`（恒真） | ✅ 红（「不存在的密钥不应命中」失败） |
| 列表不回填维度 | ✅ 红（「应回填密钥 42，got 0」） |

### 4.3 迁移后全仓跑分（`LLMGATEWAY_TEST_PG_DSN` 指向已迁 0021 的临时库）

`billing` / `router` / `identity` / `gateway` / `upstream` / `tag` / `channel` 等全部 ok；
**只剩 console 的 5 个用例失败，且全部是 `relation "channels" does not exist`（42P01）**——
即 §2 的 C 类（表已删的展示面），不是列级问题。

### 4.35 止血批（2026-11-15 预审 P0×1 + P1×2 + P2×1）

| 项 | 修复前实测（我自己复跑，不是转述） | 修复后 |
| --- | --- | --- |
| P0 0021 在本批 | 带 DSN 实跑：`billing` 2 条红（42703 列不存在 / 写路径撞已删列）、`console` **5 条红（`relation "channels" does not exist` 42P01）**；不设 DSN 时这 7 条全 skip、包级显示 ok（**假绿来源**） | 0021 摘出本批：两个包 + `db` 带 DSN `-count=1` 全绿 |
| P1 归因枚举报错 → 扣钱不落账 | 变异实测：`plain consume=1`（非事务路径**又扣一次**），事务内也扣过 1 次 → 钱扣了、账单丢了 | 归因非法值**归一**（协议 → NULL、cost_source → none）并 WARN，绝不抛错；`isDeterministicWriteError` 让约束类失败**不回退**非事务路径（断言 `txCalls=1 / plain calls=0`） |
| P1 跳过被当故障 | 变异实测：`calls=4`（5s/20s/60s 空转 85s），错误被 `Join` 吞成 nil | `backoff.Stop` 终止态：`calls=1`、零退避、整轮全跳过时返回 `*SkipError`（**不是 nil**），日志带 `skipped/total` |
| P2 `RunOnce` panic | panic 穿透 `Start` goroutine 打挂进程 | `recover` 转 `上游同步轮次 panic: …`（带栈），串行标记照常释放，下一轮可跑 |
| 空白枚举 | `" openai-chat "` 被判非法（只对空值 trim） | trim 后判合法、并按 trim 后的枚举落库（CHECK 只认枚举本身） |

对应新/改用例：`TestAttributionArgs_Normalization`（含空白与非法值归一）、
`TestAttribution_RealSchema_WriteAndReadBack`（非法归因**不阻断写入**、trim 后落库）、
`TestRecord_TxPath_DeterministicInsertError_NoPlainFallback`（资金底线）、
`TestSyncer_StepSkipIsTerminal_NoRetryNoBackoff`、`TestSyncer_PanicInStepIsRecovered`、
`backoff.TestDo_StopReturnsImmediately`、`db.TestMigrationBatch_V2Phase1`。

### 4.4 门禁（未设 DSN 的默认口径）

`gofmt`（自有文件）/ `go vet ./...` / `go test -race`（除 `tests` 包）全绿。

## 5. Step 2：待拍板的三件事（本轮只出方案，不动手）

### 5.1 账单/会话/令牌里「渠道密钥」这一列怎么对外

| 选项 | 做法 | 代价 | 适用判断 |
| --- | --- | --- | --- |
| (a) 下线字段 | 返回结构去掉 `ChannelKeyID`/`KeyName`/`ChannelName` | **改返回结构**；前端要同步 | 渠道体系整体退役、不需要按渠道追溯 |
| (b) 恒返回 null/空 | 保留字段，值恒空（本轮已让账单侧值不丢，副作用最小） | 长期留死字段 | 过渡期，先不惊动前端 |
| (c) 改由 `call_logs` 供数 | 账单维度继续可用（本轮已就位），名字类降级为 id | 名字**永久不可恢复**；成本页按渠道聚合需重建（`channels` 已删） | 还要按渠道做账/排障 |

必须写明的硬事实：`channels` / `channel_keys` 被 DROP 之后，**历史账单的渠道名与密钥名永久不可恢复**；
`call_logs` 只能供「当时落点的 id（以及若当时也落过名字则供名字）」，不能反推账单维度的名字。

### 5.2 控制面两处（拦 0021 上线的真正原因）

- `sessions.channel_key_id`：会话粘性 + 切换判定 + 重启恢复都用它；摘掉 = 路由行为变更。
- `tokens.tag_id`：令牌默认路由的输入（`tagKV(token.TagID)`），且 `TagID` 在令牌 API 里有字段。
- 这两处都与「渠道/标签域下线」同批：要么删掉会话粘性语义（产品确认），要么把状态寄存在保留列
  （如 `session_raw` JSONB）过渡。
- ⇒ **0021 不能单独上线**：现在跑全量迁移，网关的会话写入与令牌读写在 42703 上。

### 5.3 展示面（表已删）

按渠道聚合的统计/成本页（`billing/stats.go` 的按密钥 TOP、`console/cost.go` 的实体解析、
`cost_trace`、名称映射）需要：(a) 随渠道维度下线，(b) 改成「按 `call_logs` 快照聚合、名字留空」，
或 (c) 加一张保留的维度快照表（本期口径不允许新增表）。迁移后实测只有这 5 个用例红。

## 6. 不做的事（本轮画线）

- 不动 `internal/domain/channel/*`、`tag/*`、`probe_logs` 相关代码（随域下线整包处理）。
- 不动 `call_logs` 的列（0021 明确保留，账单维度的唯一留存来源）。
- 不动任何迁移文件、不改 any 对外字段与统计口径、不加新依赖。