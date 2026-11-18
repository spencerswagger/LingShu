# 代理侧账本适配 + 上游错误计费归因（2026-11-17）

## 0. 一句话

两件事都只在**灵枢侧**做，不改代理侧产品（spencer 已定「透传不改它」）：

1. 账本读取改走代理侧**官方 cursor 变更流**，字段/幂等键/时间过滤全部按代理侧真实口径映射；
2. 上游 4xx/5xx 在账单里单独成状态 `upstream_error`，保留上游状态码与错误体摘要；
   只有「上游 2xx 但确实没有 usage」才继续落 `failed/no-usage`。

两项都**不改变**「无 usage 不计费」的资金口径：失败/上游错误一律整笔退预扣、金额 0。

---

## 1. 代理侧账本适配

### 1.1 为什么推翻原来的 offset 实现

原实现按「通用分页接口」假设写的，和代理侧真实口径差了四处（全部由 `magpie_ledger.go` 收敛）：

| 假设 | 代理侧事实 | 后果 |
| --- | --- | --- |
| 行数组叫 `items/data/requests/list` | 叫 **`rows`** | 真实报文解析不出行，静默 0 行 |
| 按 `from`/`to` 时刻过滤 | 只有 `period`（today/week/month/all 或 `from..to` 日期）与 `day`（代理侧时区） | 时间窗口参数无效 |
| 用 `occurred_at ASC` 排序 + `ProbeLedgerOrder` 闸门 | offset 账本**最新在前**；cursor feed 是**按 seq 的变更流**，不承诺业务时间序 | 排序闸门永远 `unverified` → 整窗 blocked |
| `original_ids.request_id` 当幂等键 | 幂等键是 **`id`（u_…）**；`original_ids` 只作来源审计，代理侧从不编造 | 可能把同一行算成多行 / 把不同行合并 |

另外代理侧 `limit` **硬上限 500**（超过 400），原来发 1000。

### 1.2 采用的接口：cursor 变更流

`GET /v1/magpie/usage/requests?cursor=start|now|<next_cursor>&limit=<≤500>`

响应：

```json
{"rows": [...], "deleted": [{"id":"u_old","seq":900}], "next_cursor":"cur_abc", "has_more": true}
```

口径：

- `cursor=start`：从头读；`cursor=now`：只取此刻之后；
- 同一 `id` 以更高 `seq` 再次出现 = 这一行被改过 → **覆盖**（SQL `ON CONFLICT (ledger_id) DO UPDATE`）；
- `deleted` 里的 id **必须从本地删除**（行被合并/清除，不删会重复计数）；
- `410 cursor_expired/cursor_reset`、`400 cursor_invalid/cursor_filter_changed` → 从 `start` **重放一次**，
  语义是**整段替换**而不是「只 upsert」：代理侧 `cursor=start` 明确「不给 deleted 列表」
  （`Reading with cursor=start lists the ledger as it is and no deleted ids`），只 upsert 会让
  代理侧已经删掉/合并掉的旧 id 永久留在本地，与重放出来的新 id 并存 → 同一通调用被计两次
  （实测 `u_session` 与 `u_gateway` 并存）。实现：重放第一页的事务里先 `Truncate` 再 `Upsert`
  （清空与第一页同事务，失败整页回滚、旧账本原样保留）；重放跨轮时只清空一次；
  重放仍失败就报错（不死循环）；
  **已知边界**：重放跨轮期间本地账本是「部分重建」状态（最旧的先回来，最近的最后回来），
  这段窗口里对账可能看到账本侧偏少而告警；`alarm_streak` 要到 3 才升级，重放完成后
  下一轮对账会把该窗重算成 ok。`billing_records.ledger_entry_id` 是文本留痕，
  账本行被代理侧删掉时不会级联删除（也不该级联）。
- `has_more=true` 但没有 `next_cursor` → 显式报错（否则原地死循环）；
- `limit` 夹到 500；`rows` 缺失 → 显式报错（不许静默 0 行，那正是 F9 要防的「空轮假绿」）。

### 1.3 字段映射（灵枢九字段）

| 灵枢 `ledger_entries` | 代理侧 `rows[]` | 说明 |
| --- | --- | --- |
| `ledger_id` | `id` | 幂等键（`u_…`），不是 `original_ids.request_id` |
| `provider` | `provider` | |
| `provider_account` | **`providerAccount`** | camelCase；**不是** `callerKeyName`（那是网关调用方 key） |
| `model` | `model` | 缺失才回落 `served`（上游自报模型） |
| `tokens_in` | **`in` + `cache_read` + `cache_write`** | 提示词侧总量。代理侧 `in` 是「未缓存输入」，缓存读/写是独立字段；灵枢账单侧是 `input（含 cache_write）+ cache_read + output`，只有并进来两侧合计才同口径（见 §1.3.1）。`cache_write_1h` 是 `cache_write` 的 TTL 拆分（子集），不重复加；`reasoning` 已含在 `out` 里 |
| `tokens_out` | **`out`** | 不是 `output_tokens` |
| `occurred_at` | **`t`** | RFC3339（可带纳秒/时区偏移）；缺失或解析失败的行**跳过**（不许 now() 兜底） |
| `cost_credits` | `cost`（USD，仅 `priced=true`） | 走注入的换算函数，见 §1.4 |

跳过规则（都计入 `FeedPage.Skipped` 并打 WARN，不静默）：

- 没有 `id`（无幂等键）；
- 没有/无法解析 `t`；
- `handed_on=true`（代理侧文档明确「既不算请求也不算失败」，写进来会把笔数算重）。

`rejected`、`status=499`（客户端取消）**保留**：它们确实是发生过的请求，token 为 0/实际值，不能被账本吞掉。

**已知边界（handed_on）**：代理侧 `handed_on=true` 是「一次失败后被转交给下一次尝试」的行，它的
token 仍会计入代理侧总计。灵枢账本按「一条请求一行」使用，因此跳过 handed_on 行（避免笔数虚高）；
若代理侧的最终行没有合并这次尝试的 token，账本成本会略低。要精确到尝试级需要给 `ledger_entries`
加 `attempt` 维度，属后续裁定。

### 1.3.1 token 口径（两侧必须同源，2026-11-17 复审修正）

代理侧一行有四个 token 字段：`in`（**未缓存**输入）、`out`、`cache_read`、`cache_write`
（`cache_write_1h` 是 `cache_write` 按 TTL 的拆分，子集）。灵枢账单侧是三段互斥口径：
`input`（含 Anthropic 的 cache creation，即 cache write）+ `cache_read` + `output`，
合计的唯一实现是 `billing.TokenSumExpr`。

所以账本侧的合计必须是 `in + cache_read + cache_write + out`。`ledger_entries` 本期仍是九字段
（不加缓存列），因此在适配层把 `cache_read + cache_write` 并进 `tokens_in`（提示词侧总量），
`tokens_in + tokens_out` 即与 `TokenSumExpr` 同口径 —— 对账（`reconcile`）与账本结算
（`ledgersettle` 的 `TokenDelta`）都读这一处，改一处两边同时对齐。

配套改动：对账 `alarm` 增加**最小 token 门槛** `reconcile.MinAlarmTokens = 1000`
（两侧较大者低于它不告警，detail 记 `below_min_tokens`）。理由：窗口只有几十到几百 token 时，
1% 只有零点几个 token，「无 usage 走估算」与取整噪声必然超阈；门槛只看两侧较大者，
所以「本地有量、账本整段缺失」照样告警。本期清空/上量的边界：V2 还没上线，历史行不存在回填问题；
已写入的账本行会在该 id 下次以更高 `seq` 出现时被覆盖成新口径。

### 1.4 成本单位（必须显式声明）

- 代理侧 `cost` 是 **USD**；灵枢 `ledger_entries.cost_credits` 是**积分**。
- 换算与订阅录入同源：**积分 = 元 × 1e6 ÷ R**，USD 先 × `billing.cny_rate`：
  `credits = cost_usd × cny_rate × 1_000_000 / R`
- 换算函数由 `cmd/server` 注入（读 `billing.ConfigProvider.CnyRate()/R()`）；上游包不直读配置。
- 配置读不到/非法 → 该页报错（窗口 blocked），**不静默写 0 成本**。
- `priced=false` 的行成本记 0，且不调用换算（代理侧没算价时 `cost` 无意义）。
- `cost` 是**读取时刻**的价格；代理侧换价不产生新行，所以灵枢账本成本是「读取时点」口径。

> 已知口径冲突（登记，不在本批改）：`0023_v2_reconcile.sql` 文件头写「对账不比金额」，而
> `ledgersettle` 会把 `ledger_entries.cost_credits` 直接写进 `billing_records.cost_credits`。
> 本文件把账本成本明确为「代理侧 USD 按当前汇率/R 折算的积分」，为后续按账本结算成本提供唯一口径；
> 对账是否比金额需要另立裁定。

### 1.5 续读游标的持久化

- cursor 是**不透明字符串**，塞不进 `reconcile_windows.scroll_offset`（BIGINT）；
- 存在 `reconcile_windows.detail->>'ledger_cursor'`（detail 本来就是排障/非判定数据，不新增列）；
- 断点优先级：最旧 blocked 行的 cursor → 最近一次成功轮的 cursor → `start`；
- 与账本页**同事务**写入（沿用契约 §6.2 的「一页一事务 + 窗口行同事务」）；
- 重启/下一轮从持久化游标继续，不重放整本账。

### 1.6 对 Rounder 的影响

- `RounderOptions.Feed` 非 nil 时走 `runFeedOnce`，`Source`（offset）保留给旧调用方/排障；
- cursor 模式下窗口水位线仍写，但语义退化为「这一轮什么时候跑过、拉了多少」；
  真正的进度是 cursor。成功轮的窗口取**当前**那一格，保证 `LastSuccessEnd` 会推进；
- 单轮上限（20 页 / 4000 条）照旧：超限时已提交的页保留、窗口置 `blocked` 并持久化 cursor，
  下一轮从该 cursor 继续；
- 空轮（`Fetched==0`）照旧 WARN。

---

## 2. 上游错误计费归因

### 2.1 状态机

| 场景 | 账单状态 | 金额 | 上游状态码/摘要 |
| --- | --- | --- | --- |
| 上游 2xx 且有 usage | `completed` | 按 usage 结算 | NULL |
| 上游 2xx 但无 usage | `failed` + `no-usage` | 0（退预扣） | NULL |
| 上游 4xx（非 499） | **`upstream_error`** | 0（退预扣） | 有 |
| 上游 5xx | **`upstream_error`** | 0（退预扣） | 有 |
| 连接失败/超时（Status=0） | **`upstream_error`** | 0（退预扣） | 状态码 NULL |
| 客户端取消（499） | `failed` + `client-canceled` | 0（退预扣） | NULL |
| 结算失败 | `failed`（settle-failed） | 0（退预扣） | 视 res 而定 |

> 旧实现把 4xx 落进 `default → no-usage`，运营看到「上游没给用量」，实际是上游拒了。
> 这是本次修的核心缺陷。

### 2.2 落库字段

迁移 `0025_v2_billing_upstream_error.sql`：

- `status` 值域放开 `upstream_error`（`DROP CONSTRAINT IF EXISTS billing_records_status_check` +
  `ADD CONSTRAINT ck_billing_status CHECK (status IN ('completed','failed','upstream_error'))`）；
- `upstream_status SMALLINT`（NULL/0 = 没收到响应）；
- `upstream_error_summary TEXT`（已遮蔽本次凭据、折叠空白、UTF-8 安全截断 512B）。

写入侧：

- `billing.RecordReq`/`Record` 增 `Status`、`UpstreamStatus`、`UpstreamErrorSummary`；
- `insertOn` 末尾追加两列（33 参数），`recordCols`/`sqlScan` 同步两列；
- 状态非空且非法 → 直接报错（不静默归 completed）。

读取/统计侧：

- `recordCols` 26 列，所有 `Record` 查询/归并/详情自动带上；
- `billing/stats.go` 的失败口径统一改成 `status IN ('failed','upstream_error')`
  （失败率、错误 Top N、按状态聚合都不再漏掉上游错误）；
- `console` 管理端/开发者端列表透出 `UpstreamStatus`/`UpstreamErrorSummary`（`omitempty`）。

### 2.3 错误体摘要的捕获（`internal/domain/magpie`）

- 只在上游 `status >= 400` 时旁路 tee 捕获（8KB），不改写转发字节；
- 能拿到明文才捕：解压成功 / 头与体不自洽（实际是明文）/ 无编码；
  压缩体（br、没配凭据时的 gzip）标注「未解析」，不把压缩字节当摘要；
- 摘要优先取 `error.message`（+ `error.code`/`error.type` 前缀），否则折叠原文；
- 遮蔽本次 callerKey（含 `Bearer <key>` 形态），非法 UTF-8 替换，截断到 512B；
- **坑**：不能用 `defer` 在 `return res` 后写摘要 —— 返回值在 defer 之前已复制，
  调用方拿到空串。必须在每个返回点前收尾（本次实测踩过）。

---

## 3. 验证

- 无 DSN：`go test ./...` 全绿（真库用例按惯例 skip，skip ≠ 通过）。
- 复审两条修正的锚点用例（都不需要 DSN）：
  - `TestParseMagpieFeed_PromptSideTokensIncludeCache`（tokens_in = in + cache_read + cache_write，
    `cache_write_1h` 不重复加、`reasoning` 不另加）；
  - `TestDecide_MinTokenThreshold`（门槛以下不告警且 detail 标注；门槛之上照旧按 1% 判定）；
  - `TestRounder_CursorFeed_CursorResetReplacesLocalLedger`（410 后旧 id 不残留、`Replaced` 可观测）、
    `TestRounder_CursorFeed_ResetTruncatesEvenOnEmptyFirstPage`（空首屏也要清空）。
- 带 DSN（自建 PG17 + 真实迁移链到 0025）：
  - `internal/db`（含 `TestMigrate_0025_BillingUpstreamError`：值域/两列/幂等/非法状态被拒）；
  - `internal/domain/upstream`（含 `TestSQLWindowStore_LedgerCursorRoundTrip`：游标跨重启、清 blocked）；
  - `internal/domain/billing` / `console`（26 列扫描、失败口径、归并/详情）；
  - `internal/domain/facadebill`（4xx→upstream_error、2xx 无 usage→failed、传输失败→NULL 状态码）。
- 前端：`vue-tsc --noEmit`（本次只改注释/枚举文案）。

## 4. 未做/边界

- 代理侧产品一行未改；cursor 模式之外的 offset 接口仍保留（`HTTPLedgerSource`）供旧调用方/排障。
- `rounder` 的排序闸门（`ProbeLedgerOrder`）在 cursor 模式下不再使用；offset 模式原样保留。
- 对账「不比金额」与账本成本直写 `billing_records.cost_credits` 的口径冲突见 §1.4，需另立裁定。
- `upstream_error_summary` 只截 512B；完整错误体仍在网关日志 / 调用日志里（不在账单表）。
- 迁移号 `0025` 属于 V2 批次右界（`V2MigrationRightBound="0025"`，右开）之后的第一个文件；
  本批的 `[0019,0025)` 门禁/探测不覆盖它。下一批（原 token_scope）请顺延 0026，避免撞号。
