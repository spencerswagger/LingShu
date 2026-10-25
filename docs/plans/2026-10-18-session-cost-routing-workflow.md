---
intent: 在同一对外模型下，把渠道路由从“会话无脑粘性”升级为“会话亲和 + 逐轮成本决策”——按渠道成本价 cost_rates 的三档价叠加时段倍率与上下文分档倍率，在会话起点做零成本择优、在会话中途以“预计剩余轮数 > 回本轮数”为门控做保守切换，并把切换事件与“假设节省 / 相位”经既有 asyncwrite 记录；不改变对外售价计费口径与响应语义。
success_criteria:
  - 路由成本比较纳入“三档成本价 × 当前时段倍率 × 当前上下文分档倍率”，样值优先取会话本轮真实 token，缺失回退 routing.cost_sample。
  - 会话起点按 argmin(本轮花费 + 重灌代价 ÷ 预计剩余轮数) 选渠道（起点重灌代价=0）。
  - 会话中途仅当 预计剩余轮数 > 回本轮数 才切换；分母 ≤ 0 永不回本不切换。
  - 决策产物（选中渠道 / 本轮花费 / 重灌代价 / 回本轮数 / 决策原因）可回放。
  - 影子模式开关关闭时不切换渠道，仍产出“本可切换”的假设节省记录。
  - 管理界面参数面板可调、临界点随参数实时变化，线上入参从模型成本 / 会话真实 token / 当前时刻与实际分档自动取值。
  - go build / go vet / go test ./... 绿；go test -race ./internal/... 零告警；frontend npm run build 通过。
risk_level: high
auto_approve: false
branch: trae/agent-gRQMdM
worktree: false
---

## Steps

> **修订（2026-10-20，全量 review 后）**：实现已按设计文档「修订记录」修正 F1–F8。要点：
> 剩余轮数取固定值 10（不按 k 预测，k 仅作事件轮次观测）；中途切换改为**路由层只决策、网关确认后迁移**
> （`Engine.ConfirmSessionSwitch`）；新增 `routing.sample_profile` 补齐路由时刻样值三段；影子观测改为独立开关
> `shadow_enabled`（默认 false）；起点择优 priority 恢复硬分层。以下步骤保留原始执行记录，语义以修正后为准。

> **移除说明（2026-10-25）**：会话成本路由的**控制台观测面已移除**——Step 17 的后端面板接口、
> Step 18–20 的前端参数面板 / 临界点图 / 分布 / 事件流（含 `SessionCostRouting.vue` 及其 API 与
> 路由注册）均已删除；后端进程内事件存储 `SessionCostEventStore` 及其 `main.go` 装配、
> `NewAsyncCostEventSink` 的可选 store 变参也一并移除。路由决策、`routing.session_cost` 配置与
> asyncwrite 决策事件留痕保留不变。以下 Step 17–20 仅保留历史执行记录。

- [x] **Step 1: 新增“会话成本路由”配置读取与默认安全态**
action: 新建 `backend/internal/domain/router/cost_config.go`，定义 `type SessionCostConfig struct { Enabled bool; RemainingA float64; RemainingB float64; RemainingMin int; RemainingMax int; MinRemaining int; CooldownSec int }` 与 `func NewSessionCostProvider(src configSource) *SessionCostProvider`，从 `sys_configs` 键 `routing.session_cost` 读取（JSON 对象，缺省 `{Enabled:false, RemainingA:12, RemainingB:1, RemainingMin:1, RemainingMax:20, MinRemaining:1, CooldownSec:0}`），复用 `billing.ConfigProvider` 的缓存与失效代际机制。`Enabled=false` 为默认安全态（等价现状）。中文注释写明安全态语义。新增 `cost_config_test.go` 断言缺省值与解析。不改现有路由行为。
loop: false
verify: cd backend && go build ./... && go test ./internal/domain/router/ -run SessionCostConfig

- [x] **Step 2: 写“成本比较纳入三档成本价 × 时段 × 分档”的失败测试**
action: 在 `backend/internal/domain/router/router_test.go`（或新增 `cost_order_test.go`）写表驱动测试：给定两个候选渠道，构造“最低输入价渠道 ≠ 最低总成本渠道”，分别注入 `TimeCoeffConfig`（时段倍率≠1）与 `ContextTiers`（分档倍率≠1），断言 `orderCandidates` 择到总成本更优者；并断言样值优先取 `RouteSpec` 携带的本轮 token 三段（输入/缓存读/输出）。此步只写测试。
loop: false
verify: cd backend && go test ./internal/domain/router/ -run CostOrder 2>&1 | tail -20

- [x] **Step 3: 实现成本比较（替换“分档系数恒 1 + cost_sample 样值”口径）**
action: 在 `backend/internal/domain/router/engine.go` 改造成本计算：用 `channel_models.cost_rates` 三档 × `billing.TimeCoeffConfig.CoeffFor(e.now())` × `billing.ContextTierCoeff(input+cacheRead, tiers)`；样值优先取 `RouteSpec` 新增字段（`SampleInput/SampleCacheRead/SampleOutput int64`），缺失时回退 `costSample`。`RouteSpec` 新增字段不改变旧调用行为（零值=回退）。扩展 `Engine` 以注入 `SessionCostProvider` 与 `ContextTiers()/TimeConfig()` 只读访问（复用 `billing.ConfigProvider`）。中文注释声明口径与回退规则。
loop: until go test ./internal/domain/router/ 全绿
max_iterations: 4
gate: human
verify: cd backend && go build ./... && go test ./internal/domain/router/ -run CostOrder

- [x] **Step 4: 写“会话成本决策层”的失败测试（起点 / 中途 / 亲和保真）**
action: 新增 `backend/internal/domain/router/session_cost_test.go`，覆盖：① 起点（无旧渠道）重灌代价=0，按摊还成本最小选渠道；② 中途 `预计剩余轮数 ≤ 回本轮数` 时不切换；③ 中途 `预计剩余轮数 > 回本轮数` 时切换；④ 分母 ≤ 0（新渠道每轮不更便宜）永不回本不切换；⑤ 回本轮数用 Venice→DeepSeek 示例断言（缓存读 2 万→18 轮、下限 ≈ 5.9）。此步只写测试。
loop: false
verify: cd backend && go test ./internal/domain/router/ -run SessionCost 2>&1 | tail -20

- [x] **Step 5: 实现会话成本决策层（回本轮数 / 重灌代价 / k 经验式剩余轮数）**
action: 在 `engine.go` 新增决策函数：`remainingTurns(k) = clamp(RemainingA - RemainingB*k, RemainingMin, RemainingMax)`；`rechargeCost(新渠道, 缓存读) = 缓存读 × (新渠道输入价 − 新渠道缓存读价) × 新渠道时段倍率 × 分档倍率 ÷ 1e6`；`paybackTurns = 分子 ÷ 分母`（分子=新输入价−新缓存读价)×缓存读；分母=(旧缓存读−新缓存读)×缓存读 + (旧输入−新输入)×新增输入 + (旧输出−新输出)×输出；分母≤0 → 永不回本）。产出 `SessionCostDecision{Switch bool; FromChannelKeyID, ToChannelKeyID int64; TurnCostOld, TurnCostNew, RechargeCost, PaybackTurns float64; RemainingTurns int; Reason string}`。起点判定：`SessionRegistry.Lookup` 未命中即起点。
loop: until go test ./internal/domain/router/ 全绿
max_iterations: 4
verify: cd backend && go build ./... && go test ./internal/domain/router/ -run SessionCost

- [x] **Step 6: 影子模式（只读假设节省）失败测试 + 实现**
action: 在 `session_cost_test.go` 增加用例：`SessionCostConfig.Enabled=false` 时 `SessionCostDecision.Switch` 恒为 false（渠道不变），但仍返回完整的“假设”字段（TurnCostOld/New/RechargeCost/PaybackTurns/Reason）供记账；随后在 `engine.go` 实现该分支（不改变选中渠道，仅计算并返回假设值）。
loop: until go test ./internal/domain/router/ 全绿
max_iterations: 3
verify: cd backend && go test ./internal/domain/router/ -run Shadow

- [x] **Step 7: 决策产物经 asyncwrite 落库（失败测试 + 实现 + 装配）**
action: 在 `backend/internal/pkg/asyncwrite/job.go` 的 `Kind` 新增 `KindSessionCostEvent`（默认 `AsyncKinds` 不含它 → 同步执行的安全态）；新增 `backend/internal/domain/router/cost_event.go` 定义 `CostEventSink` 接口与 `asyncCostEventSink` 包装（`Key=session_id` 保序，Payload=决策产物 JSON，`IdempotencyKey=session_id+":"+轮次`）。在 `engine.go` 决策后经注入的 sink 提交（sink 未注入则 no-op）。在 `main.go` `asyncwrite.New(...)` 的 `AsyncKinds` 增加 `KindSessionCostEvent` 并注入 sink，WAL 复用 `data/asyncwrite.wal`。测试：启用异步时提交成功且重复 `IdempotencyKey` 只落一次。
loop: until go test ./internal/domain/router/ ./internal/pkg/asyncwrite/ 全绿
max_iterations: 4
verify: cd backend && go build ./... && go test ./internal/domain/router/ -run CostEvent && go test ./internal/pkg/asyncwrite/

- [x] **Step 8: P0 影子观测人工评审门（成本口径不变 + 账实校准口径）**  <!-- gate: human — 用户预授权放行（“一直执行完”）-->
action: 人工评审：① 路由成本口径改为“三档成本价 × 时段 × 分档”，确认不改变对外售价计费与响应语义；② 影子记录字段足以做“假设节省 vs 实际结算”校准；③ `Enabled=false` 默认安全态、开关可一键回退。产出 PASS/REVISE 结论。（并在本门显式确认：成本比较口径变更=新基线，模型级 context_tiers 会影响存量排序；对外售价计费与响应语义不变。）
loop: false
gate: human
verify:
  type: human-review
  check: 成本口径变更未影响对外售价计费与响应语义；影子记录可支撑账实校准；默认安全态与回退路径成立

- [x] **Step 9: 起点择优端到端测试**
action: 在 `session_cost_test.go` 增加：同一对外模型多候选、新会话（起点）；断言 `Enabled=true` 时按“本轮花费（重灌代价=0）最小”选渠道，并与手算一致；断言 `Enabled=true` 但起点候选含会话亲和命中时仍按成本择优（起点无旧缓存）。
loop: until go test ./internal/domain/router/ -run StartPick 全绿
max_iterations: 3
verify: cd backend && go test ./internal/domain/router/ -run StartPick

- [x] **Step 10: 实现起点择优并接线网关**
action: 在 `engine.go` 的 `Route/RouteAll` 中，当 `SessionCostConfig.Enabled=true` 且为起点时，把会话成本决策结果用于候选排序/选择（`RouteSpec` 需携带本轮 token 三段与 `now`）；在 `backend/internal/domain/gateway/handler.go` 的 `RouteAll` 调用点组装 `RouteSpec`（sessionID、本轮 输入/缓存读/输出、会话位置 k）。注释声明起点语义。
loop: until go test ./internal/domain/router/ ./internal/domain/gateway/ 全绿
max_iterations: 4
verify: cd backend && go build ./... && go test ./internal/domain/router/ -run StartPick && go test ./internal/domain/gateway/

- [x] **Step 11: P1 起点择优人工放行门**  <!-- gate: human — 用户预授权放行 -->
action: 依据 P0 影子数据评估起点择优收益与偏差，人工放行 P1（把 `routing.session_cost.enabled` 在生产置 true 前需评审）。产出 PASS/REVISE 结论。
loop: false
gate: human
verify:
  type: human-review
  check: 影子数据表明起点择优净收益为正且无“对外售价/响应语义”变化；放行 P1

- [x] **Step 12: 中途切换门控测试**
action: 在 `session_cost_test.go` 增加：① `预计剩余轮数 > 回本轮数` → 切换；② `≤` → 不切换（保持亲和）；③ 分母 ≤ 0 → 永不回本不切换；④ 缓存读低于临界（示例 ≈ 1.35 万）时起点/中途均不切换。
loop: until go test ./internal/domain/router/ -run MidSwitch 全绿
max_iterations: 3
verify: cd backend && go test ./internal/domain/router/ -run MidSwitch

- [x] **Step 13: 实现中途切换（保守计价）+ 抖动抑制**  <!-- gate: human — 用户预授权放行 -->
action: 在 `engine.go` 实现中途决策：按 D6 保守计入一次性重灌代价（不检测 provider 缓存失效）；加入抖动抑制——`MinRemaining` 最小剩余轮数门槛、`CooldownSec` 冷却（同会话切换最短间隔，基于 `Session.LastActive`/新字段）、滞回（仅当新渠道总成本优于当前渠道超过阈值才切）。注释声明保守计价与抑制参数来源。
loop: until go test ./internal/domain/router/ 全绿
max_iterations: 4
gate: human
verify: cd backend && go build ./... && go test ./internal/domain/router/ -run MidSwitch

- [x] **Step 14: 切换时的会话迁移 + 并发上限校验**
action: 在 `backend/internal/domain/router/sessions.go` 支持切换：更新 `Session.ChannelKeyID/InternalModelID` 并经 `sessionPersister.Upsert`（走既有 asyncwrite `AsyncPersister`，`Key=session_id` 保序）；沿用 `Attach`/`Renew` 的并发上限与滑动 TTL 语义。新增测试：切换后 `Lookup` 返回新渠道；同会话 Upsert 与 SetClosed 不乱序；并发上限超限时回退保持原渠道。
loop: until go test ./internal/domain/router/ 全绿
max_iterations: 4
verify: cd backend && go test ./internal/domain/router/ -run SwitchMigrate

- [x] **Step 15: P2 中途切换人工放行门**  <!-- gate: human — 用户预授权放行 -->
action: 评估 P2 实际收益、抖动率与账实偏差，人工放行中途切换（生产开启）。产出 PASS/REVISE 结论。
loop: false
gate: human
verify:
  type: human-review
  check: 中途切换净收益为正、抖动率与账实偏差在阈值内；放行 P2

- [x] **Step 16: 闲忙时 / 分档联动收口（P3）**  <!-- gate: human — 用户预授权放行 -->
action: 在 `engine.go` 确认跨时段（`CoeffFor(now)`）与跨分档（`ContextTierCoeff`）在决策与成本比较中一致生效；新增端到端测试：同一会话在闲时/忙时（不同 `now`）与不同上下文规模下择渠道结果符合预期；断言路由与 `billing.ComputeCredits` 对同一输入取到相同的 `timeCoeff`/`ctxCoeff`（同源）。
loop: until go test ./internal/domain/router/ ./internal/domain/billing/ 全绿
max_iterations: 4
gate: human
verify: cd backend && go build ./... && go test ./internal/domain/router/ -run TimeTier && go test ./internal/domain/billing/

- [x] **Step 17: 后端“会话成本路由”面板接口**
action: 在 `backend/internal/domain/console/admin.go` 新增接口（挂到 `server.go` 路由）：`GET /api/v1/admin/session-cost/config`（返回各候选渠道三档成本价、时段/分档倍率、当前时刻倍率、`routing.cost_sample` 默认样值、k 系数与门控阈值）、`GET /api/v1/admin/session-cost/events`（读 `KindSessionCostEvent` 落地的事件流）、`GET /api/v1/admin/session-cost/distribution`（按会话聚合临界缓存读的分位/直方）。所有值标明来源（模型成本 / 真实 token / 当前时刻 / 实际分档）。新增测试断言字段与来源标签存在。
loop: until go test ./internal/domain/console/ 全绿
max_iterations: 4
verify: cd backend && go build ./... && go test ./internal/domain/console/ -run SessionCost

- [x] **Step 18: 前端参数面板 + 临界点图 + 分布 + 事件流**
action: 在新页面（如 `frontend/src/views/admin/SessionCostRouting.vue`）实现：参数面板（渠道三档成本价 / 时段倍率 / 分档倍率 / 本轮 token / 剩余轮数）；临界点图（ECharts，多渠道路由成本曲线汇聚、标出交叉临界点）；临界点分布（直方 + P10/中位/P90）；切换事件流表（沿用 `components/CallLogPanel.vue` 风格）。在 `frontend/src/api/admin.ts` 增加对应调用。路由注册到 `frontend/src/router/index.ts`（admin 区）。
loop: until cd frontend && npm run build 通过
max_iterations: 4
verify: cd frontend && npm run build

- [x] **Step 19: 前端线上取值来源 + 只读 / What-if（gate）**  <!-- gate: human — 用户预授权放行 -->
action: 面板入参默认**只读**并展示来源标签（“来自模型成本”“来自本轮 usage”“按当前时刻”“按实际分档”）；提供 What-if 开关，仅覆盖模式允许手改且图上高亮“假设值 vs 真实值”。确认覆盖态不写回真实配置（仅本地预览）。新增/扩展 `admin/BillingConfig.vue` 与 `ModelPricing.vue` 的复用（时段/分档/r 的现有读写）。
loop: until cd frontend && npm run build 通过
max_iterations: 3
gate: human
verify:
  - type: shell
    command: cd frontend && npm run build
  - type: human-review
    check: 线上入参默认只读且标注来源；What-if 覆盖仅本地预览、不写回真实配置

- [ ] **Step 20: 前端浏览器验证（临界点随参数变化）**  <!-- BLOCKED(env): 本机无 Chrome 且以 root 运行，浏览器工具不可用；按 HOTL 降级为人工复审，未自动验证 -->
action: 启动 dev server，打开 `SessionCostRouting.vue`；拨动参数，确认临界点图/分布随参数实时更新、无 console 报错、只读/覆盖态显示正确。
loop: false
verify:
  type: browser
  url: http://localhost:5173/admin/session-cost-routing
  check: 临界点图随参数实时更新且无 console 报错；只读来源标签与 What-if 覆盖态显示正确

- [x] **Step 21: P3 放行门 + 全量收口**  <!-- gate: human — 用户预授权放行；shell 全量 verify 已通过 -->
action: 人工放行 P3（闲忙时联动与可视化）；随后跑全量构建与 race 收口，确认各阶段开关可独立回退到纯会话粘性。产出最终验收结论。
loop: false
gate: human
verify:
  - type: shell
    command: cd backend && go build ./... && go vet ./...
  - type: shell
    command: cd backend && go test -race -count=1 ./internal/...
  - type: shell
    command: cd frontend && npm run build
  - type: human-review
    check: 各阶段开关可独立回退到纯会话粘性；成本口径不变；账实偏差在阈值内