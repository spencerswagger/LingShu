---
intent: 削减网关请求热路径上的同步 DB 往返：配置类读走内存缓存（写路径显式失效），非资金写走统一异步持久化层 asyncwrite，资金写收口为两处同步事务；不改变计费口径、路由结果与响应语义。
success_criteria:
  - 可缓存配置类读命中后 0 次 DB（清单见设计文档 success_criteria）。
  - 仅两处同步资金操作（预扣条件写；账单+结算同事务），且仅限成功热路径。
  - 非资金写经 asyncwrite：同 Key 保序、幂等可重放、崩溃丢失窗口 < 50ms、优雅退出 flush。
  - 密钥/模型「状态列」保持同步落库（仅事件异步）。
  - 令牌/用户缓存：撤销/禁用/删除经钩子即时生效；自然过期精确（读时比对 ExpiresAt）。
  - go build / go vet / go test ./... 绿；go test -race ./internal/... 零告警。
risk_level: high
auto_approve: false
branch: trae/agent-52ia7Q
worktree: false
---

## Steps

- [ ] **Step 1: 建立 asyncwrite 包骨架与 API**
action: 新建 `backend/internal/pkg/asyncwrite/`，创建 `asyncwrite.go`（导出 `Submit(job Job)`、`Start(ctx)`、`Shutdown(ctx)`、`Stats() Stats` 与构造函数 `New(cfg Config) *Writer`，先返回 not-implemented 空实现）与 `job.go`（`type Kind int`，常量 `KindSession/KindChannelKeyEvent/KindChannelModelEvent/KindCallLog/KindTokenLastUsed`；`type Job struct { Kind Kind; Key string; IdempotencyKey string; Exec func(ctx context.Context) error }`；`type Stats struct { Depth, Dropped, Retried int; LastFSync time.Time }`；`Config` 含 `WALPath string`、`Shards int`、`FSyncInterval time.Duration`、`MaxBatch int`、`QueueSize int`、`AsyncKinds map[Kind]bool`）。所有字段与分类策略写入中文注释（显式声明默认值：FSyncInterval=10ms、MaxBatch=100、丢失窗口上界 50ms）。不改动任何既有包。
loop: false
verify: cd backend && go build ./... && go vet ./internal/pkg/asyncwrite/

- [ ] **Step 2: 写 asyncwrite 队列/保序/统计的失败测试**
action: 新建 `backend/internal/pkg/asyncwrite/asyncwrite_test.go`，编写表驱动测试：① `Submit` 非阻塞且不丢（提交 N 条后 `Stats().Depth` 反映在途）；② 同 `Key` 的 Exec 调用顺序与提交顺序一致（记录调用序列断言 FIFO）；③ 不同 Key 可并发（断言总执行次数 = 提交次数）；④ `Start` 后所有 job 最终执行完毕。测试需 `Start`/`Shutdown`/`Submit`。此步只写测试。
loop: false
verify: cd backend && go test ./internal/pkg/asyncwrite/ 2>&1 | tail -20

- [ ] **Step 3: 实现有界队列 + 分片 worker 使保序测试通过**
action: 在 `asyncwrite.go`/新增 `worker.go` 实现：有界 channel 队列；`Shards` 个 worker，按 `hash(Key) % Shards` 路由到固定 worker 以保同 Key FIFO；worker 循环从自己分片的队列取 job 调 `Exec`；`Start(ctx)` 启动 worker，`Shutdown(ctx)` 停止接收并 drain。背压默认「短暂阻塞（默认 50ms）后仍满才记 `Dropped` 并打日志」，注释写明「绝不静默丢弃审计/会话」。
loop: until go test ./internal/pkg/asyncwrite/ 全绿
max_iterations: 4
verify: cd backend && go test ./internal/pkg/asyncwrite/

- [ ] **Step 4: 写 WAL spool + 批量 fsync + 启动 replay 的失败测试**
action: 在 `asyncwrite_test.go` 追加测试：① `Submit` 后立即杀掉 Writer（模拟崩溃，不调 Shutdown）再 `New` 同一 `WALPath` 并 `Start`，断言未完成 job 被 replay 执行（不丢）；② 同一 `IdempotencyKey` 重复提交只执行一次（幂等，用计数断言）；③ 批量：提交 500 条后 `Stats().LastFSync` 非零且 fsync 次数远小于 500（通过注入计数 fsync 钩子断言）。此步只写测试。
loop: false
verify: cd backend && go test ./internal/pkg/asyncwrite/ -run 'Replay|Idempotent|BatchFSync' 2>&1 | tail -20

- [ ] **Step 5: 实现 WAL spool（常开 fd + 批量 fsync + 原子重写）+ replay + 幂等**
action: 新增 `spool.go`：常开 fd 追加写 job（Kind/Key/IdempotencyKey/序列化载荷），后台按 `FSyncInterval` 或累积 `MaxBatch` 条触发 fsync（group commit）；完成后用「写 tmp + os.Rename」原子重写剩余集。`New` 时读取 spool 并 replay；用 `IdempotencyKey` 去重集保证重复提交只执行一次。job 的 Exec 需可重放——为可重放性，序列化只存业务主键，Exec 由调用方在注册期提供（replay 使用同一注册表）。
loop: until go test ./internal/pkg/asyncwrite/ 全绿
max_iterations: 5
verify: cd backend && go test ./internal/pkg/asyncwrite/

- [ ] **Step 6: 实现 Shutdown flush 并补测试**
action: 实现 `Shutdown(ctx)`：先置「停止接收」，再 drain 队列 + 最后一次 fsync + 关闭 fd；`Submit` 在停止后返回错误。追加测试：`Shutdown` 返回时 `Stats().Depth==0` 且 spool 文件为空/不存在。
loop: until go test ./internal/pkg/asyncwrite/ -run ShutdownFlush 全绿
max_iterations: 3
verify: cd backend && go test ./internal/pkg/asyncwrite/ -run ShutdownFlush

- [ ] **Step 7: 实现 per-Kind 异步/同步开关并补测试**
action: 实现 `Config.AsyncKinds`：某 Kind 未启用异步时，`Submit` 退化为同步直接调用 `Exec` 并返回其错误（安全态）。追加测试：Kind 未启用时 `Submit` 同步执行且错误透传；启用时异步执行。
loop: until go test ./internal/pkg/asyncwrite/ 全绿
max_iterations: 3
verify: cd backend && go test ./internal/pkg/asyncwrite/

- [ ] **Step 8: asyncwrite 并发与 race 校验**
action: 在 `asyncwrite_test.go` 增加并发压测（多 goroutine 并发 Submit + 同 Key 交错 + 并发 Stats），确保无 data race。不改业务代码。
loop: until go test -race ./internal/pkg/asyncwrite/ 零告警
max_iterations: 3
verify: cd backend && go test -race -count=1 ./internal/pkg/asyncwrite/

- [ ] **Step 9: 写 router 会话异步持久化适配器的失败测试**
action: 新建 `backend/internal/domain/router/session_async.go`，定义 `AsyncPersister`：实现 `sessionPersister`（`Upsert/Delete/DeleteMany/SetClosed/LoadActive`），内部把 `Upsert/Delete/SetClosed` 提交给 asyncwrite，`Key = session_id`（保证同一会话行 Upsert 与 SetClosed/Delete 同分片、不乱序）；`LoadActive` 保持同步直读（启动恢复）。在 `session_async_test.go` 写测试：Upsert 与 SetClosed 以同一 session_id 提交时，最终 DB 状态为 closed=true（不乱序复活）。
loop: false
verify: cd backend && go test ./internal/domain/router/ -run SessionAsync 2>&1 | tail -20

- [ ] **Step 10: 实现会话异步适配器并接线 main.go**
action: 实现 `AsyncPersister` 方法体（`Upsert`/`Delete`/`SetClosed` 走 asyncwrite，`Key=session_id`；`DeleteMany` 按 session_id 逐条提交以分片保序）。在 `cmd/server/main.go` 构建 asyncwrite 并把 `sess.SetStore` 由 `sessionStore` 改为 `NewAsyncPersister(writer, sessionStore)`；`LoadPersisted` 仍在启动时用同步直读。写入注释显式声明该分类与丢失窗口。
loop: until go test ./internal/domain/router/ 全绿
max_iterations: 4
verify: cd backend && go build ./... && go test ./internal/domain/router/

- [ ] **Step 11: 写调用日志异步适配器的失败测试**
action: 新建 `backend/internal/domain/gateway/calllog_async.go`，定义 `AsyncCallLogStore` 包装 `CallLogStore`：`Insert` 提交 asyncwrite（`Kind=KindCallLog`，`Key=billing_id`，`IdempotencyKey=billing_id`，因表已 `ON CONFLICT (billing_id) DO NOTHING` 幂等）。在测试中注入 fake 底层 store，断言提交后底层收到调用且重复 id 只落一次。
loop: false
verify: cd backend && go test ./internal/domain/gateway/ -run AsyncCallLog 2>&1 | tail -20

- [ ] **Step 12: 实现调用日志异步适配器并接线**
action: 实现 `AsyncCallLogStore.Insert`（走 asyncwrite），在 `cmd/server/main.go` 用 `gw.SetCallLogs(gateway.NewAsyncCallLogStore(writer, callLogStore))` 替换直接注入。注释声明分类、幂等键与丢失窗口。
loop: until go test ./internal/domain/gateway/ 全绿
max_iterations: 3
verify: cd backend && go build ./... && go test ./internal/domain/gateway/

- [ ] **Step 13: 令牌 LastUsed 走 MemoryOnly 异步**
action: 在 `cmd/server/main.go` 把 `TouchLastUsed: tokenSvc.TouchLastUsed` 改为提交 asyncwrite 的薄包装（`Kind=KindTokenLastUsed`，`Key=tokenID`，该 Kind 配置为 MemoryOnly：不落 spool）。注释显式声明「可丢、仅统计用途」。补一个轻量测试断言该 Kind 不写 spool 文件。
loop: false
verify: cd backend && go build ./... && go test ./internal/pkg/asyncwrite/ -run MemoryOnly

- [ ] **Step 14: 写 Manager 事件异步（状态列保持同步）的失败测试**
action: 在 `backend/internal/domain/channel/` 新建 `event_async_test.go`：给 `Manager` 注入一个 fake async submitter（接口 `type EventSink interface { SubmitEvent(ctx context.Context, kind string, id int64, from, to State, reason string) error }`），断言：发生状态流转时事件经 sink 提交，而 `channel_keys.state` / `channel_models.state` 的更新仍同步发生在 `persistKeyTransition`/`persistModelTransition` 内（用 sqlmock 断言 SetState/UpdateModelState 立即执行）。
loop: false
verify: cd backend && go test ./internal/domain/channel/ -run EventAsync 2>&1 | tail -20

- [ ] **Step 15: 实现事件异步 + 状态列同步并接线**
action: 在 `channel/manager.go` 增加可选 `SetEventSink(EventSink)`；改 `persistKeyTransition`/`persistModelTransition`：`SetState`/`UpdateModelState` 保持同步，`InsertEvent` 改经 sink 提交（sink 未注入时退回同步 InsertEvent，保持既有测试兼容）。事件 `IdempotencyKey` 用预生成事件 ID，底层 `InsertEvent` 改 `ON CONFLICT(id) DO NOTHING`（如无 PK 冲突语义则新增去重约束迁移）。在 `main.go` 注入 sink。
loop: until go test ./internal/domain/channel/ 全绿
max_iterations: 4
verify: cd backend && go build ./... && go test ./internal/domain/channel/

- [ ] **Step 16: 非资金异步整体 race 校验**
action: 不改代码，跑并发与 race 校验覆盖 router/channel/gateway/asyncwrite。
loop: until go test -race ./internal/... 零告警
max_iterations: 3
verify: cd backend && go test -race -count=1 ./internal/...

- [ ] **Step 17: 写资金语义三类用例的失败测试（gate）**
action: 在 `backend/internal/domain/gateway/handler_test.go`（或新文件 `fund_test.go`）写表驱动测试覆盖：① 钱包缺失 → 40401 语义；② 余额不足 → 40201 语义；③ 余额为 0 且预扣金额为 0（费率缺失/估算为 0）→ 仍被拦截（不得放行）。用 fake CreditOps 断言错误码与拦截行为。
loop: false
verify: cd backend && go test ./internal/domain/gateway/ -run FundSemantics 2>&1 | tail -20

- [ ] **Step 18: 实现预扣原子条件写（含 balance>0）使测试通过（gate）**
action: 在 `backend/internal/domain/identity/credit.go` 的 `consumeOn` 条件由 `balance >= $1` 改为 `balance >= $1 AND balance > 0`（amount=0 也下发），失败时按既有逻辑区分「钱包缺失 40401」与「余额不足 40201」。同步更新 `credit_test.go` 覆盖 amount=0 且零余额被拒。
loop: until go test ./internal/domain/identity/ 全绿
max_iterations: 4
gate: human
verify: cd backend && go test ./internal/domain/identity/ -run Credit

- [ ] **Step 19: 去掉 handler 独立余额读取，改由预扣条件写统一拦截（gate）**
action: 在 `backend/internal/domain/gateway/handler.go` 移除进入候选前独立的 `g.credit.Balance` 零余额前置读取，改为仅在预扣阶段由条件写拦截；保留 `g.balance`（无 Credit 装配时的兼容路径）行为不变。中文注释显式写明「零余额与费率缺失均由预扣条件写拦截，错误码语义不变」。
loop: until go test ./internal/domain/gateway/ -run FundSemantics 全绿
max_iterations: 4
gate: human
verify: cd backend && go test ./internal/domain/gateway/ -run FundSemantics

- [ ] **Step 20: 资金收口人工评审（gate）**
action: 人工评审：确认仅两处同步资金操作、40401/40201 与零余额语义不变、对账口径不变、失败路径（退款/拒绝落账）不受影响。产出评审结论（PASS/REVISE）。
loop: false
gate: human
verify:
  type: human-review
  check: 资金写收口后计费错误码与对账口径与改造前一致，未引入 TOCTOU 或赊账窗口

- [ ] **Step 21: 对外模型名读缓存 + 失效接线**
action: 在 `backend/internal/domain/model/` 为 `GetByExternalName` 增加内存缓存（键 = external_name，值 = {id, enabled, hit}），在 `service.go` 的 `CreateModel`/`UpdateModel`/`BatchDeleteModels`/`ApplyPrice`（仅影响的键）与 `store` 写入后失效对应键。新增测试：命中后零 DB（底层 store 置 nil 不 panic）、写路径后立即失效。
loop: until go test ./internal/domain/model/ 全绿
max_iterations: 4
verify: cd backend && go test ./internal/domain/model/ -run Cache

- [ ] **Step 22: 令牌标签读缓存 + 失效接线**
action: 在 `backend/internal/domain/tag/` 为 `GetByID` 增加缓存（键 = tagID），在 tag CRUD（Create/Update/Delete）后失效。新增测试覆盖命中零 DB 与写后立即失效。
loop: until go test ./internal/domain/tag/ 全绿
max_iterations: 4
verify: cd backend && go test ./internal/domain/tag/ -run Cache

- [ ] **Step 23: 定价模式读缓存 + 失效接线**
action: 在 `cmd/server/main.go` 的 `pricingModeResolver`（identity.Store.GetByID）外层加缓存（键 = userID），在 identity 用户写路径（`Service.UpdateUser`/`BatchDeleteUsers`/`AdminCreateUser`）失效。新增测试断言命中零 DB 与写后失效。
loop: until go test ./internal/domain/identity/ 全绿
max_iterations: 4
verify: cd backend && go test ./internal/domain/identity/ -run PricingCache

- [ ] **Step 24: Auto 成员解析读缓存 + 失效接线**
action: 在 `backend/internal/domain/automodel/` 为 `ResolveMembers` 增加缓存（键 = name + ownerUserID），在 Auto 模型 CRUD 后失效。新增测试覆盖命中零 DB 与写后立即失效。
loop: until go test ./internal/domain/automodel/ 全绿
max_iterations: 4
verify: cd backend && go test ./internal/domain/automodel/ -run Cache

- [ ] **Step 25: billing.r / context_tiers 读缓存 + 失效接线**
action: 在 `backend/internal/domain/billing/service.go` 的 `defaultConfigProvider` 增加 `R()` 与 `ContextTiers()` 缓存（与既有 `TimeConfig`/`SampleUsage` 同模式），在 `InvalidateCostConfig`（或新增 `InvalidateAll`）中一并清空，由既有 `SetConfigWriteHook` 触发。新增测试：命中零 DB、`UpsertConfig` 后立即失效。
loop: until go test ./internal/domain/billing/ 全绿
max_iterations: 4
verify: cd backend && go test ./internal/domain/billing/ -run ConfigCache

- [ ] **Step 26: 渠道内部模型查询读缓存（纳入内存快照）**
action: 在 `backend/internal/domain/channel/manager.go` 增加 `GetByChannelAndInternal(ctx, channelID, internalModelID)`（读内存 `mrt`，语义与 `channelModelStore.GetByChannelAndInternal` 等价，返回快照值拷贝），并在 `main.go` 以 `chMgr` 适配 gateway 的 `ChannelModelResolver`（`recordBilling` 用）。新增测试：命中零 DB、`UpsertModel`/`DropModel` 后立即反映。
loop: until go test ./internal/domain/channel/ 全绿
max_iterations: 4
verify: cd backend && go test ./internal/domain/channel/ -run GetByChannelAndInternal

- [ ] **Step 27: 上游响应头白名单读缓存 + 失效接线**
action: 在 `cmd/server/main.go` 的 `UpstreamResponseHeaderWhitelist` 读取处（`billStore.GetConfig`）加缓存（键 = 该 config key），在 `console.Admin` 写该配置（`UpsertConfig`）后失效。新增测试：命中零 DB、写后立即失效。
loop: until go test ./internal/domain/console/ 全绿
max_iterations: 4
verify: cd backend && go test ./internal/domain/console/ -run WhitelistCache

- [ ] **Step 28: 配置缓存启用时序与零 DB 断言**
action: 确认所有缓存失效钩子已先于缓存启用接线（复核 main.go 装配顺序并加注释）；新增一个集成式测试断言「未变更的连续 N 次配置读取产生 0 次 DB 查询」（注入计数 fake store）。提供直查 fallback 开关（环境变量/Config 字段），默认开启缓存但可一键退回直查。
loop: until go test ./internal/domain/... 全绿
max_iterations: 3
verify: cd backend && go test -count=1 ./internal/domain/...

- [ ] **Step 29: 写令牌/用户两级缓存结构的失败测试**
action: 在 `backend/internal/domain/identity/` 新建 `token_cache_test.go`：断言令牌缓存键结构为「plain→entry」且维护「userID→[plain]」二级索引；令牌项内嵌 `ExpiresAt`。此步只写测试。
loop: false
verify: cd backend && go test ./internal/domain/identity/ -run TokenCache 2>&1 | tail -20

- [ ] **Step 30: 实现令牌/用户缓存与读时过期校验**
action: 在 `TokenService.LookupByPlain` 前端加缓存：命中后比对 `ExpiresAt`（到点即视为未命中并触发一次直查确认）；缓存项含用户状态快照。实现两级索引与 `InvalidateByPlain`/`InvalidateByUserID` 方法。中文注释显式声明「失效点覆盖全部写路径；安全 TTL 仅最后防线，不计入即时性」。
loop: until go test ./internal/domain/identity/ -run TokenCache 全绿
max_iterations: 4
verify: cd backend && go test ./internal/domain/identity/ -run TokenCache

- [ ] **Step 31: 令牌缓存失效点全接线（gate）**
action: 在 `TokenService.Create`/`BatchDelete`/`Rotate`/`Toggle` 及存储层 `UpdateStatus`/`Delete` 调用点全部挂 `InvalidateByPlain`（能反查 hash 者）与必要的按 ID 失效。新增表驱动测试：对每条写路径各一例，断言缓存即刻失效。
loop: until go test ./internal/domain/identity/ -run TokenInvalidate 全绿
max_iterations: 4
gate: human
verify: cd backend && go test ./internal/domain/identity/ -run TokenInvalidate

- [ ] **Step 32: 用户状态缓存失效点全接线（含与 pkg/session 同写点）（gate）**
action: 在 `Service.UpdateUser`/`BatchDeleteUsers`/`AdminCreateUser`/`AdminResetPassword` 处调用 `InvalidateByUserID`，并与既有 `pkg/session` 用户状态缓存的写入点合并为同一处（保证双缓存同时失效）。新增表驱动测试：每条用户写路径各一例，断言令牌/用户缓存与 pkg/session 缓存均即刻失效。
loop: until go test ./internal/domain/identity/ ./internal/pkg/session/ 全绿
max_iterations: 4
gate: human
verify: cd backend && go test ./internal/domain/identity/ -run UserInvalidate && go test ./internal/pkg/session/

- [ ] **Step 33: 令牌/用户缓存人工评审 + 全量 race 收口（gate）**
action: 人工评审「失效点清单完整性」与「直查 fallback 安全态」；随后跑全量 race 与构建。产出最终验收结论。
loop: false
gate: human
verify:
  - type: shell
    command: cd backend && go build ./... && go vet ./...
  - type: shell
    command: cd backend && go test -race -count=1 ./internal/...
  - type: human-review
    check: 令牌/用户缓存失效点覆盖全部写路径；撤销/禁用/删除即时生效；自然过期精确；fallback 默认安全态