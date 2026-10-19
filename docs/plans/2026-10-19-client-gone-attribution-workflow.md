---
intent: 让「下游客户端/前置代理提前断开导致的请求取消」被正确归因——观测不再误记为上游失败、健康不再污染渠道熔断、计费对齐 new-api rc.38（按估算用量结算而非整单退款）。
success_criteria: 建连阶段 client-gone 记 kind=client_gone/Info、不回喂渠道健康、按估算输入用量结算（非 0、不整单退）；请求级结论文案区分且不计 failed_candidates；流式+非流式一致；go test ./... 与 -race 全绿。
risk_level: high
auto_approve: true
branch: hotl/client-gone-attribution
worktree: false
# 说明：本改动的计费/健康语义已由设计 2026-10-19-client-gone-attribution-design.md 经用户批准（治理门已满足）；
# 仍保留 PR 提交与评审步骤的 gate: human 作为落地前的人审门。
---

## Steps

- [ ] **Step 1: 新增失败回归测试（流式建连 client-gone）**
action: 在 backend/internal/domain/gateway/handler_test.go 新增测试 TestClientGone_StreamConnectStage_NoUpstreamFailure。先参照同文件已有 noWriteTraceTransport / wroteThenFailTransport 的结构，新增 `canceledTransport struct{ calls int32 }`，其 RoundTrip 返回 `(nil, context.Canceled)`。测试体：`h := build(t, live.URL)`（live 为任意 httptest.Server，实际不会被命中）；`h.gw.streamClient = &http.Client{Transport: &canceledTransport{}}`；`dump := withLogCapture(h)`；`rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", "{\"model\":\"gpt-4\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}")`。断言：dump() 含 "kind=client_gone"；dump() 不含 "上游流式调用失败"；dump() 不含 "status=502"；`len(h.chans.feeds)==0`。注释说明：本测试在改造前应失败（RED）。
verify: cd backend && go test ./internal/domain/gateway/ -run TestClientGone_StreamConnectStage_NoUpstreamFailure
loop: false
max_iterations: 3

- [ ] **Step 2: 新增失败回归测试（非流式建连 client-gone）**
action: 在同文件新增测试 TestClientGone_NonStreamConnectStage。测试体：`h := build(t, live.URL)`；`h.gw.client = &http.Client{Transport: &canceledTransport{}}`（非流式走 gw.client）；`dump := withLogCapture(h)`；`rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", "{\"model\":\"gpt-4\",\"stream\":false,\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}")`。断言同 Step 1（kind=client_gone、不含 status=502、`len(h.chans.feeds)==0`）。
verify: cd backend && go test ./internal/domain/gateway/ -run TestClientGone_NonStreamConnectStage
loop: false
max_iterations: 3

- [ ] **Step 3: 新增失败回归测试（计费按估算结算、不整单退）**
action: 在同文件新增测试 TestClientGone_SettlesByEstimateNotFullRefund。以 build 构造的 harness 为准（其 fakeCredit 初始余额>0）；将 streamClient 设为 canceledTransport；发起一条流式 client-gone 请求。断言：`h.billing` 记录到一次 Record 且其 usage 的 Input>0、Output==0（按估算输入结算）；并且 `h.credit.refunded` 中不存在等于该请求整笔预扣金额的退还（即未整单退）。断言风格参照同文件 TestServePreConsume_RefundsOnUpstreamFailure 与 TestServeBillingSurvivesCanceledRequestCtx；如 harness 字段名与预期不符，读取这两个既有用例确认 fakeCredit/fakeBilling 的实际字段后再写断言。
verify: cd backend && go test ./internal/domain/gateway/ -run TestClientGone_SettlesByEstimateNotFullRefund
loop: false
max_iterations: 3

- [ ] **Step 4: 新增失败回归测试（请求级结论区分且不计失败候选）**
action: 在同文件新增测试 TestClientGone_RequestOutcomeDistinct。发起流式 client-gone 请求并捕获日志。断言：请求级日志行（含 "gateway request outcome"）的 reason 含 "客户端已断开"；该行含 "failed_candidates=0"；dump() 不含 "候选失败"。注释说明改造前应失败（RED）。
verify: cd backend && go test ./internal/domain/gateway/ -run TestClientGone_RequestOutcomeDistinct
loop: false
max_iterations: 3

- [ ] **Step 5: 实现 serveStream 的 client-gone 归因分支（含计费结算）**
action: 修改 backend/internal/domain/gateway/handler.go：在 fwdOutcome 结构新增字段 `clientGone bool`（注释：下游客户端/前置代理提前断开导致的请求取消，与上游失败区分）。在 serveStream 的 `up, err := g.streamClient.Do(req)` 失败分支内，于 delivered/firstByte/elapsed 计算之后、`g.channels.FeedResult(...)` 之前插入：`if streamClientGone(ctx, err) { ... }`。分支体：(1) 前置构造 streamDiag（delivered/first_byte_received/elapsed_ms），供日志与调用日志复用；(2) 记 Info 日志：`g.logUpstreamErr(ctx, slog.LevelInfo, "stream client gone", endpoint, ureq.Protocol, model, token, routeRes, diagFromRequest(req, ureq.Protocol, ureq.Cred), errInfo{kind: "client_gone", stage: stage, attempt: attempt, retry: false, reason: "下游客户端已断开，请求取消", sessionID: sessionID, durationMs: *elapsed, delivered: &delivered, firstByteReceived: &firstByte, elapsedMS: elapsed})`；(3) 不回喂渠道健康（不调用 FeedResult/FeedResultGated）；(4) 按估算输入用量结算、不整单退：以 `billing.Usage{Input: int64(pc.InputTokens)}` 调 `g.recordBilling(ctx, routeRes, model, token, usage, pricingMode, sessionID, sessionName, pc.Credits, elapsed, nil)`，取返回的 bid，并调 `g.writeCallLog(...)` 落一条 decision（desc 用 "客户端断开"、pc、routeRes，附带 streamDiag）；(5) 不向 w 写任何状态/错误体；(6) `return &fwdOutcome{responded: true, clientGone: true, streamDiag: streamDiag}`。注意 pc 为 serveStream 入参、已在作用域内。既有 delivered/未送达/已送达 等分支逻辑保持不变。
verify: cd backend && go test ./internal/domain/gateway/ -run TestClientGone_StreamConnectStage_NoUpstreamFailure
loop: until cd backend && go test ./internal/domain/gateway/ -run TestClientGone_StreamConnectStage_NoUpstreamFailure >/dev/null 2>&1
max_iterations: 4
gate: human

- [ ] **Step 6: 实现 serveNonStream 的 client-gone 归因分支**
action: 在 backend/internal/domain/gateway/handler.go 的 serveNonStream 中，`up, err := g.client.Do(req)` 失败分支内，于 delivered 判定之前插入与 Step 5 等价的 `if streamClientGone(ctx, err) { ... }`：(1) 记 Info 日志，msg="client gone"、kind="client_gone"、retry=false、reason="下游客户端已断开，请求取消"（此分支无首字节/流式诊断字段，故不带 delivered/firstByteReceived/elapsedMS）；(2) 不回喂渠道健康；(3) 以 `billing.Usage{Input: int64(pc.InputTokens)}` 调 recordBilling 并 writeCallLog（desc "客户端断开"）；(4) 不写任何下游响应；(5) `return &fwdOutcome{responded: true, clientGone: true}`。既有分支保持不变。
verify: cd backend && go test ./internal/domain/gateway/ -run TestClientGone_NonStreamConnectStage
loop: until cd backend && go test ./internal/domain/gateway/ -run TestClientGone_NonStreamConnectStage >/dev/null 2>&1
max_iterations: 4

- [ ] **Step 7: 实现请求级结论与失败计数排除 client-gone**
action: (1) 在 backend/internal/domain/gateway/handler.go 主循环 out.responded 分支的 switch 中新增 `case out.clientGone:` → `attempts = append(attempts, attempt("客户端断开"))`（放在 billFailed/refundPre/interrupted/success 之前或其后按互斥即可）。(2) 在同分支计算 status/trail/failed 之后、既有 `if failed > 0` 之前，新增：`if out.clientGone { g.logOutcome(ctx, slog.LevelInfo, endpoint, model, "客户端已断开，按估算结算", status, false, token, sessionID, attempts, trail, failed, msSince(start)); return }`（status 为 out.downstreamStatus；为 0 时沿用现有兜底为 200，非 4xx/5xx，不受客户端错误日志门控影响）。(3) 在 backend/internal/domain/gateway/errlog.go 的 summarizeAttempts 中，把 reason 等于 "客户端断开" 的轨迹项按与 "success" 相同的规则排除出 failed 计数（不计为失败候选）。不要改动 outcomeIsFailure（clientGone 不含 billFailed/refundPre/interrupted，天然不算失败）。
verify: cd backend && go test ./internal/domain/gateway/ -run TestClientGone_RequestOutcomeDistinct
loop: until cd backend && go test ./internal/domain/gateway/ -run TestClientGone_RequestOutcomeDistinct >/dev/null 2>&1
max_iterations: 4

- [ ] **Step 8: 全量测试 / race / vet / build**
action: 运行完整校验；修复所有失败。禁止通过删改既有断言来"通过"（既有测试是不可回归契约）。
verify:
  - type: shell
    command: cd backend && go test ./...
  - type: shell
    command: cd backend && go test -race ./internal/domain/gateway/... ./internal/domain/channel/...
  - type: shell
    command: cd backend && go build ./... && go vet ./...
loop: until cd backend && go test ./... >/dev/null 2>&1 && go vet ./... >/dev/null 2>&1
max_iterations: 5

- [ ] **Step 9: 默认安全态回归确认**
action: 确认未命中 client-gone 的路径行为不变：运行并确保既有相关用例全绿（TestStreamClientGone、TestStreamDelivered_ConnectFailureFailsOver、TestStreamDelivered_AfterDeliveredNoFailover、TestServePreConsume_RefundsOnUpstreamFailure、TestServeFailover_*、TestUpstreamAuthRejectedLogsFullScene、TestMaskedUpstreamFailureIsLogged）。确认未修改任何既有测试文件中的断言。
verify: cd backend && go test ./internal/domain/gateway/ ./internal/domain/channel/ -count=1
loop: false
max_iterations: 3

- [ ] **Step 10: 提交、推送、创建 PR**
action: 在分支 hotl/client-gone-attribution 上提交全部改动（含 docs/designs/2026-10-19-client-gone-attribution-design.md、本工作流文件、handler.go、errlog.go、handler_test.go）。提交信息：`fix(gateway): attribute client-gone disconnects separately from upstream failures`。推送 origin。随后创建面向默认分支的 PR，正文引用设计文档路径与 Step 8 的测试证据摘要（go test 与 -race 结果）。
gate: human
verify:
  - type: shell
    command: git -C /workspace status --short && git -C /workspace log -1 --oneline
  - type: shell
    command: git -C /workspace rev-parse --abbrev-ref HEAD

- [ ] **Step 11: PR 评审 → 修复 → 复审循环**
action: 对 PR 做多维度代码评审（对照本设计 Intent/Verification 契约、默认安全态、计费与健康语义），产出问题清单；逐条修复并在同一分支追加提交；复审直至无 P0/P1 问题。评审须覆盖：client-gone 判定是否误伤真实上游失败；健康回喂是否确为中性；计费是否按估算结算且未整单退；请求级结论与 failed_candidates 是否正确；流式与非流式是否一致。
loop: until 复审结论无 P0/P1 未决问题
max_iterations: 3
gate: human
verify:
  type: human-review
  check: PR 评审结论无遗留 P0/P1 问题，且 go test ./... 与 go test -race 仍全绿

- [ ] **Step 12: 追加测试（client-gone 归为 499）**
action: 在 backend/internal/domain/gateway/handler_test.go 更新既有 4 个 TestClientGone_* 用例：断言 `rec.Code == 499`，请求级日志含 `status=499`，且不再写 502。既有断言中凡是检查 status=502 的改为 499；保留 kind=client_gone、无 feeds 等断言。
verify: cd backend && go test -count=1 ./internal/domain/gateway/ -run 'TestClientGone'
loop: false
max_iterations: 3

- [ ] **Step 13: 实现 499 语义**
action: (1) handler.go 新增 `const statusClientClosedRequest = 499`（置于 statusOriginTimeout 附近）。(2) serveStream 与 serveNonStream 的 client-gone 分支：在返回前调用 `w.WriteHeader(statusClientClosedRequest)`（不写 body），并把返回改为 `&fwdOutcome{responded: true, clientGone: true, downstreamStatus: statusClientClosedRequest}`（流式分支保留 streamDiag）。(3) backend/internal/domain/gateway/errlog.go 的 logOutcome 客户端错误门控：把 499 与 429 一样豁免（在 `status != http.StatusTooManyRequests` 之外增加 `&& status != statusClientClosedRequest`）。
verify: cd backend && go test -count=1 ./internal/domain/gateway/ -run 'TestClientGone'
loop: until cd backend && go test -count=1 ./internal/domain/gateway/ -run 'TestClientGone' >/dev/null 2>&1
max_iterations: 4

- [ ] **Step 14: 追加测试（已送达超时按估算结算）**
action: 在 handler_test.go 新增 TestDeliveredTimeout_StreamSettlesByEstimate（复用既有 wroteThenFailTransport）与 TestDeliveredTimeout_NonStreamSettlesByEstimate（新增等价「已送达后失败」transport 供 gw.client 使用）。断言：HTTP 502；按估算输入结算（h.billing 收到一次 Record 且 usage.Input>0、Output==0）；h.credit.refunded 不含等于整笔预扣的退款。并更新既有 TestStreamDelivered_AfterDeliveredNoFailover 以匹配新行为（不再整单退；调用日志/账单为估算结算记录）。
verify: cd backend && go test -count=1 ./internal/domain/gateway/ -run 'TestDeliveredTimeout|TestStreamDelivered_AfterDeliveredNoFailover'
loop: false
max_iterations: 3

- [ ] **Step 15: 实现「已送达超时」按估算结算**
action: (1) handler.go 的 fwdOutcome 新增字段 `upstreamFailed bool`（注释：已送达后上游失败/响应超时，已按估算结算、不再整单退预扣）。(2) serveNonStream 与 serveStream 的 delivered=true（Do 报错）分支：把 `g.recordFailure(...)` 替换为 `g.recordBilling(ctx, routeRes, model, token, billing.Usage{Input: int64(pc.InputTokens)}, pricingMode, sessionID, sessionName, pc.Credits, <dur>, nil)`（dur：非流式 msSince(start)、流式为 elapsed），并捕获 berr 记 `g.logger.Error("delivered-timeout billing failed", ...)`；保留 writeCallLog 与 `writeOpenAIError(w, http.StatusBadGateway, msgUpstreamDown)`；返回由 `refundPre: true` 改为 `upstreamFailed: true, downstreamStatus: http.StatusBadGateway`（流式保留 streamDiag）。(3) handler 主循环 out.responded 的 switch 增加 `case out.upstreamFailed:` → `attempts = append(attempts, attempt("上游失败"))`（置于 billFailed 之后、refundPre 之前）。(4) errlog.go 的 outcomeIsFailure 增加 `|| out.upstreamFailed`。上游明确返回错误状态（5xx/504/401/403/429）的分支保持 `refundPre: true` 不变。
verify: cd backend && go test -count=1 ./internal/domain/gateway/
loop: until cd backend && go test -count=1 ./internal/domain/gateway/ >/dev/null 2>&1
max_iterations: 4

- [ ] **Step 16: 全量校验、提交、推送（更新 PR #51）**
action: 运行 `go vet ./...`、`go test -count=1 ./...`、`go test -race -count=1 ./internal/domain/gateway/... ./internal/domain/channel/...` 全绿；提交（信息 `feat(gateway): 499 for client-gone and estimate-settlement for delivered timeouts`）；push 到 hotl/client-gone-attribution（更新 PR #51）。
gate: human
verify:
  - type: shell
    command: git -C /workspace log -1 --oneline
  - type: shell
    command: cd backend && go test -count=1 ./...

- [ ] **Step 17: 复审循环（rev2）**
action: 对 rev2 增量做多维度代码评审，重点：499 是否正确写回且访问日志可见、logOutcome 门控是否豁免 499、已送达超时是否按估算结算且未整单退、上游明确报错是否仍退款、流式与非流式是否一致、默认安全态是否保持。逐条修复并追加提交，复审至无 P0/P1。
loop: until 复审结论无 P0/P1 未决问题
max_iterations: 3
gate: human
verify:
  type: human-review
  check: rev2 评审无遗留 P0/P1，且 go test ./... 与 -race 仍全绿