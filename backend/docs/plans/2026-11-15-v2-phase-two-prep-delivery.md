# V2 阶段二准备件（迁移链 + 转发主链 + 对账计数）交付说明

日期：2026-11-15　分支：`feat/lingshu-v2-sync-config`（本地特性分支，未开 PR、未提工单）

> 结论先说：**四条硬裁定（D1/D4/F1/F4）都已落到代码里，迁移链与转发主链一次启动即可跑通；
> 但「0024 删渠道维度」与现有账单/统计代码冲突这件事没有解决**——9 个真库用例会红（见 §3）。
> 本件登记为未完成项，不假装已解决。

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

- `gofmt`：改动文件全干净（`gofmt -l` 空）。
- `go vet ./...`：干净。
- `go test ./...`：35 个包全绿（无 DSN 口径；DB 门控用例自动 skip）。
- `go test -race`（改动涉及包 `magpie` / `gateway` / `upstream` / `reconcile` / `db` / `seeding` / `model`）：全绿。
- **真库门禁**（自建 PG17，`LLMGATEWAY_TEST_PG_DSN` 指向空库）：
  - `TestGate_F4_OneStartupFullChain` PASS：一次 `Migrate` 应用全部迁移；`0020` 早于 `0024`；`0024` 排最后；目录里无 `0021`；15 张保留表 `to_regclass` 非空；10 张退役表不存在；`billing_records.channel_key_id` 已删除。
  - `TestGate_F10_ExistingNullRowsNormalizedAndChecked` PASS：存量 NULL 成本行被归一成 `'none'`，跨列 CHECK 拦住「0 成本 + ledger」。
  - `TestGate_F7_RetainedRowCountsDoNotDecrease` PASS：0024 前后 15 张保留表行数一条不减，账单/账本/对外模型各 1 行仍在。
  - `TestReconciler_RealSQL_WritesCounts`、`TestSQLWindowStore_BlockedSurvivesRestart` 在真库上 PASS（此前无 DSN 时被 skip，属"假绿"口径，已实跑）。

## 3. 未完成项与已知缺口（登记，不承诺已解决）

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

## 4. 同一窗口重复写 `blocked` 的幂等性

一句话：**offset 只会前进、账本按 `ledger_id` 去重、计数是绝对重算**，所以重复写同一个窗口不会重复入账、也不会丢进度。

1. 窗口行以 `window_start` 为主键 upsert（`ON CONFLICT (window_start) DO UPDATE`），`scroll_offset` 用 EXCLUDED 覆盖：同一窗口反复写 blocked 只会把断点更新到更靠后的位置，不会产生第二行、也不会退回已提交位置。
2. 断点与页数据**同事务提交**：`Upsert(页) + SaveTx(带下一 offset 的窗口行)` 一起 commit。要么这一页和它的断点都在，要么都不在——不会出现「offset 前进了但数据没落」的静默跳过。
3. 账本写入是 `INSERT ... ON CONFLICT (ledger_id) DO NOTHING`，重拉同一页的重复条目被吃掉，返回的插入条数如实反映实际新增。
4. 计数列由对账服务**整窗重算后覆盖**（`SaveCounts` 写绝对值，不做 `+=`），所以多跑几轮结果恒定；`TestReconciler_RealSQL_WritesCounts` 里第二轮断言数值不变。
5. 对账服务拒绝覆盖 `blocked`/`error` 行（返回 `ErrWindowNotReconcilable`）：补拉尚未完成时，计数方不许把窗口标成 ok，避免「漏账被自己的对账抹平」。
6. blocked 只在两种情况下被清：补拉到 `!has_more` 后写 OK 行，或 `ClearFrom` 覆盖到的区间；两种情况都在事务里完成，重启后行为一致（`TestSQLWindowStore_BlockedSurvivesRestart`）。