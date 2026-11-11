# 上游原始错误「运营侧可见」：三个落点 + 探活吞错修复（2026-11-11）

> 口径（用户纠正版，覆盖前两版验收）：判断标准只有一条 —— **运营在后台看得到上游返回的原始错误体原文**。
> 下游客户端能不能读到不是本任务目标（对外契约保持 PR #77 v2 的样子，不为本任务再改）。

## 1. 三个落点

| 落点 | 入口 | 落地 |
| --- | --- | --- |
| 调用日志 / 账单 | `gateway/handler.go` 六个上游失败分支（429 / 401-403 / 其它，非流式 + 流式） | `call_logs.resp_body` = 上游原文逐字节（`upstreamErrBody.operatorView(ureq.Cred)`）；`error_message` 同一原文（摘要 4KB 上限）；`upstream_status` + `request_id` + 渠道/密钥/模型/时间/耗时不变 |
| 探活单次结果 | `channel/probe.go` `doProbeAuth` → `probeOnce` / `Manager.ProbeKeyNow` / `ProbeModelNow` → `Handler.HandleProbeKey` / `HandleProbeModel` | `ProbeOutcome` 新增 `UpstreamStatus` + `ErrorBody`（原文，仅遮蔽本次凭据；32KB 截断标注） |
| 探活历史 | `channel/model_store.go` `InsertProbeLog` / `ListProbeLogs(ByKeys)` → `Handler.HandleListProbeLogs` | 迁移 `0020_probe_logs_error_body.sql`：`error_body TEXT` + `upstream_status SMALLINT`；`ProbeLog.ErrorBody/UpstreamStatus` 原样返回 |

## 2. 探活路径：确认吞错，已修

`doProbeAuth` 非 2xx 三个分支（429 / 401-403 / 其它）此前**从不读 `resp.Body`**，只
`fmt.Sprintf("probe http status: %d", code)`；Key/Model 两条手动探测共用该函数，所以一起吞。
`probe_logs` 也没有可存原文的列。

修法：三个分支统一 `readProbeErrBody(resp, cred)`（读上限 64KB → 保存上限 32KB + UTF-8 边界截断 +
同字段标注），原文进 `ProbeOutcome.ErrorBody` 与 `probe_logs.error_body`；
`Error` 仍是短文案（状态机 `LastErr` 也用它，不能塞 32KB）。

## 3. 「原文被吃掉」的两个真实原因（都不是探活）

1. **按样式清洗**：`sanitizeErrSummary` 把 `sk-*`/Bearer/KV/**高熵长串（≥20 位字母数字混排）**
   一律换成 `***`。`resp_body` 历史上取自它 → 上游报文里的 request-id、随机串、模型名被整段抹掉
   （实测「第 45 字节后整段消失」）。
2. **落库上限静默**：无标注可循（历史无 32KB 概念）。

修法：新增第三视图 `operatorView`（`gateway/upstream_error.go`）——原文逐字节，**只**遮蔽
本次实际使用的凭据原值（`internal/pkg/errbody.MaskSecrets`，精确子串、长度 ≥8 才参与）；
超 32KB 同字段标注，读满 64KB 时如实写「超过读取上限」。`Clean` 保留给日志短字段与错误分类。

## 4. 测试（改现有，不重做）

`internal/domain/gateway/upstream_error_passthrough_test.go`：
- 删掉固定样例（`openCodeGoRegionErrBody` 那类「某厂商某段文案」）作为断言依据；
- `TestServeUpstreamError_ShapeTable` → `TestServeUpstreamError_RandomShapes`：**随机驱动**
  （seed 打印在日志里），每轮内容不同；
- 断言只有一条：随机内容**逐字节**出现在 `call_logs.resp_body`（超长则前缀逐字节 + 同字段标注）；
- 跑旧实现必红（已验证：把 `operatorView` 换回 `Clean`、把 `readProbeErrBody` 改成不读体 → 5 个用例 FAIL）。

`internal/domain/channel/probe_operator_visible_test.go`（新增）：
- 同一套随机用例（≥10 组随机值 + 8 类形状）× 模型探测 / 密钥探测：`ProbeOutcome.ErrorBody` 逐字节 + 与
  `probe_logs.error_body` 一致；
- HTTP 级：`HandleProbeModel` / `HandleProbeKey` 响应 `Data.ErrorBody`、`HandleListProbeLogs` 历史原文；
- `TestProbeErrorBody_MasksOnlyExactCredential`：只遮蔽凭据，request-id / 模型名 / URL query 保留。

`internal/pkg/errbody`：精确遮蔽与截断的单测。
真库：`TestProbeLog_PgRoundTrip`（新增，无 DSN 跳过）、`TestCallLog_PgRoundTrip`（改随机报文）、
`internal/db/migrate_test.go` 增 `probe_logs` 列断言（迁移链 21 个文件全绿）。

## 5. 边界

- 保存上限 32KB（`callLogErrBodyMaxBytes` / `probeErrBodyMaxBytes`）；读取上限 64KB。
  超限同字段标注，截断按 UTF-8 边界（不会切出半个汉字）。
- `probe-logs` 列表默认最多 100 条 × 单条 32KB → 最坏约 3.2MB 响应体（原文优先，未再截短）。
- 只遮蔽「本次实际使用的凭据」：若上游回显的是**别的**凭据（例如上游自己的内部 token），会原样落库。
  这是刻意取舍——按形状猜会误伤原文，而误伤原文正是本任务要解决的问题。
- 前端探活页/账单详情要展示 `ErrorBody`（`UpstreamStatus` 同理），属 View 侧改动。
- 401 之后密钥/模型转禁用，紧接着再点探活可能「无可用模型/密钥」而不打上游（既有状态机语义，未改）。

## 6. 分支

`fix/upstream-error-passthrough`（PR #77，同一特性分支上追加本组提交；未自行合入、未推 main）。
