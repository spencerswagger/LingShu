---
intent: 补全请求失败的决策轨迹与流式失败诊断：路由层排除（尤其排空 DRAIN）产出候选轨迹项，并让排除原因显式映射到运维动作；流式上游失败区分「已送达/未送达」并补充响应阶段诊断，使 S1/S2/S7 可定位。
success_criteria:
  - 「仅因排空而无候选」的场景，决策轨迹中出现排空项（而非静默无记录）。
  - 每个候选的排除原因可映射到运维动作（状态/探测、会话、限流、协议、凭据、排空）。
  - 流式上游失败日志明确区分「已送达/未送达」，并含响应阶段诊断（上游状态码、是否收到首字节、阶段耗时）。
  - 轨迹结构为向后兼容扩展，既有前端/聚合消费方不受影响。
risk_level: medium
auto_approve: false
---

## Steps

- [x] **Step 1: 路由层——产出被排除候选**
action: 在 backend/internal/domain/router/engine.go 的 `collectCandidates` 各排除点（对外模型未绑定任何渠道内部模型、标签过滤后为空、排空被剔出新会话候选、`len(cands)==0`）收集一条 `ExcludedCandidate{ ChannelID, ChannelName, ChannelKeyID, InternalModelID, Reason }`，并让它随 `RouteAll` 的结果一并返回（扩展结果结构，新增可空字段，保持既有字段不变）。Reason 使用稳定枚举字符串：`no_binding` / `tag_no_route` / `drained` / `no_candidate`。
loop: false
verify: cd backend && go test ./internal/domain/router/ -run Excluded -v

- [x] **Step 2: 网关——把被排除候选并入决策轨迹**
action: 在 backend/internal/domain/gateway/handler.go 组装 `attempts` 时，把 Step 1 的 `Excluded` 转为轨迹项（带 `skipped=true` 与映射后的 reason），置于已尝试候选之前；无 Excluded 时行为不变。
loop: false
verify: cd backend && go test ./internal/domain/gateway/ -run Decision -v

- [x] **Step 3: 排除原因 → 运维动作映射**
action: 在 backend/internal/domain/gateway/errlog.go 增加纯函数 `exclusionHint(reason string) string`，把稳定 reason 映射为运维动作提示（`drained`→「查渠道/密钥状态或手动探测」；`session_full`→「查会话并发/存活」；`rate_limited`→「查模型/渠道/密钥 RPM·TPM」；`unsupported_protocol`→「查协议可达性」；`credential_unavailable`→「查密钥凭据」；`disabled`→「查禁用状态」）。该提示进入 `summarizeAttempts` 的轨迹摘要与调用日志 decision。
loop: false
verify: cd backend && go test ./internal/domain/gateway/ -run ExclusionHint -v

- [x] **Step 4: 单测——排空场景轨迹完整**
action: 新增/扩展 backend/internal/domain/gateway/handler_test.go：构造「所有候选因排空被排除」请求，断言返回 503 且 `call_logs.decision.attempts` 含 `reason=drained` 的 `skipped` 项；断言 `attempt_trail` 中包含排空步骤。
loop: false
verify: cd backend && go test ./internal/domain/gateway/ -run DrainTrail -v

- [x] **Step 5: 流式失败诊断字段**
action: 在 backend/internal/domain/gateway/handler.go 的 `serveStream` 失败分支，向结构化日志与调用日志补充：`delivered`(bool，取 `wrote.Load()`)、`stage`、`first_byte_received`(bool)、`elapsed_ms`、`upstream_status`(若可得)。保持两条中文文案不变（`上游流式连接失败` / `上游流式请求失败（已送达，不转移）`），仅补字段。
loop: false
verify: cd backend && go test ./internal/domain/gateway/ -run StreamDiag -v

- [x] **Step 6: errInfo 结构扩展**
action: 在 backend/internal/domain/gateway/errlog.go 的 `errInfo` 增加 `Delivered *bool`、`FirstByteReceived *bool`、`ElapsedMS *int64` 字段，并在 `logUpstreamErr`/`logErr` 中输出；`sanitizeErrSummary` 逻辑不变。`kind` 保持 `connect_failed` 但新增 `delivered` 字段区分两种情况。
loop: false
verify: cd backend && go test ./internal/domain/gateway/ -run ErrLog -v

- [x] **Step 7: 调用日志 decision 兼容扩展**
action: 在 backend/internal/domain/gateway/calllog.go 的 decision 组装处纳入新轨迹项与新字段（仅新增，不改既有键名）。确认插入失败容错不变。
loop: false
verify: cd backend && go test ./internal/domain/gateway/ -run CallLog -v

- [x] **Step 8: 前端展示**
action: frontend/src/components/CallLogPanel.vue 与 frontend/src/views/admin/SessionDetail.vue：轨迹渲染支持新原因（排空等）与 `skipped` 样式，展示 `exclusionHint` 与流式 `delivered/first_byte_received/elapsed_ms`；对未知字段容错。
loop: false
verify: cd frontend && npm run build

- [x] **Step 9: 单测——流式已送达/未送达区分**
action: 新增 backend/internal/domain/gateway 测试：假上游在「连接阶段失败」与「已送达后失败」两种情形各一例，断言日志/调用日志分别出现 `delivered=false` 与 `delivered=true`，且前者可 failover、后者回 502 不转移。
loop: false
verify: cd backend && go test ./internal/domain/gateway/ -run StreamDelivered -v

- [x] **Step 10: 全量回归 + 竞态**
action: 运行全量后端测试与竞态检测，确认既有轨迹/账单语义不变。
verify:
  - type: shell
    command: cd backend && go test ./... && go build ./... && go vet ./...
  - type: shell
    command: cd backend && go test -race ./internal/domain/gateway/... ./internal/domain/router/...