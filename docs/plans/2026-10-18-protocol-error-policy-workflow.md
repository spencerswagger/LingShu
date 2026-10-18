---
intent: 在渠道协议上引入可配置的「错误策略」与「协议级错误统计」：网关在上游错误处理处按命中协议取策略，决定「是否计入渠道健康错误率 / 是否 failover / 触发动作(none/drain/disable) / 下游状态码」，默认安全态（未配置时行为与改造前一致），消除请求/协议类错误对渠道健康度的误伤。
success_criteria:
  - 可在渠道协议上为每个「错误签名(status + code/type/message)」配置处置，生效范围可选「整渠道」或「指定若干模型」。
  - 协议级错误按「错误类别」计数，可在控制台查看。
  - 配置「某错误不计入健康」后，该错误不再抬升渠道错误率、不触发排空；配置「failover=false」后不再转移。
  - 未配置任何策略时，路由结果、错误码、错误率与改造前一致。
risk_level: high
auto_approve: false
---

## Steps

- [x] **Step 1: 迁移——channel_protocols 增加 error_policy 列**
action: 新建迁移 backend/internal/db/migrations/0014_channel_protocol_error_policy.sql，内容为 `ALTER TABLE channel_protocols ADD COLUMN IF NOT EXISTS error_policy JSONB NOT NULL DEFAULT '[]'::jsonb;`。随后在 backend/internal/domain/channelprotocol/model.go 的 ChannelProtocol 结构体加 `ErrorPolicy []ErrorPolicy json:"errorPolicy"`，并在 backend/internal/domain/channelprotocol/store.go 的列清单/扫描/写入中加上该列（编解码方式完全照抄同文件既有的 `models` JSONB 字段处理）。
loop: false
verify: cd backend && go build ./... && go test ./internal/domain/channelprotocol/...

- [x] **Step 2: 定义错误策略领域类型**
action: 在 backend/internal/domain/channelprotocol/model.go 增加：`ErrorPolicy{ Scope string; ModelIDs []int64; Match ErrorMatch; Disposition ErrorDisposition; Note string }`；`ErrorMatch{ Status int; ErrorType string; ErrorCode string; MessageRegex string }`；`ErrorDisposition{ CountIntoHealth bool; Failover *bool; Action string; DownstreamStatus int }`。Action 取值为 `none|drain|disable`；Failover 用 `*bool` 以区分「未设置=沿用现状」。
loop: false
verify: cd backend && go build ./...

- [x] **Step 3: 实现错误签名匹配 MatchError**
action: 在 backend/internal/domain/channelprotocol/errorpolicy.go 实现 `MatchError(policies []ErrorPolicy, scope string, modelID int64, status int, errType, errCode, msg string) (*ErrorPolicy, bool)`：先按 scope 过滤（`channel` 命中；`models` 仅当 modelID ∈ ModelIDs），再按签名匹配；优先级 status+errCode > status+errType > 仅 status；MessageRegex 已编译缓存，空则跳过。无命中返回 `(nil,false)`。
loop: false
verify: cd backend && go build ./...

- [x] **Step 4: 单测——签名匹配与优先级**
action: 新建 backend/internal/domain/channelprotocol/errorpolicy_test.go，表驱动覆盖：status+code 优先于 status+type 优先于仅 status；scope=models 时非 ModelIDs 模型不命中；MessageRegex 命中/不命中；全空策略列表返回 `(nil,false)`。
loop: false
verify: cd backend && go test ./internal/domain/channelprotocol/ -run MatchError -v

- [x] **Step 5: 协议校验——错误策略合法性**
action: 在 backend/internal/domain/channelprotocol/service.go 的校验/归一函数中加入 error_policy 校验：Scope ∈ {channel, models}；Action ∈ {none,drain,disable}；DownstreamStatus 为 0 或合法 4xx/5xx；MessageRegex 可编译（否则报错）；scope=models 时 ModelIDs 非空。校验失败返回明确错误信息。
loop: false
verify: cd backend && go test ./internal/domain/channelprotocol/ -run Validate -v

- [x] **Step 6: 生效范围解析——协议→(渠道, 模型)**
action: 在 backend/internal/domain/gateway 增加 `policyForProtocol(protocolID int64, channelID int64, internalModelID int64, status int, errType, errCode, msg string) (*channelprotocol.ErrorPolicy, bool)`：经注入的协议查询接口取该协议 error_policy，调 channelprotocol.MatchError。所需协议查询接口通过 gateway 的依赖注入接入（照抄同包既有的协议/头策略查询注入方式）。
loop: false
verify: cd backend && go build ./...

- [x] **Step 7: 渠道健康回喂门控**
action: 在 backend/internal/domain/channel/manager.go 增加 `FeedResultGated(keyID, modelRowID int64, fb Feedback, countIntoHealth bool)`（或在既有 `FeedResult` 上加布尔参数）：`countIntoHealth=false` 时既不写入窗口样本、也不计 429/auth 计数（即完全不影响熔断）。既有调用点保持默认 true。
loop: false
verify: cd backend && go test ./internal/domain/channel/ -run Feed -v

- [x] **Step 8: 网关接入策略（非流式）**
action: 修改 backend/internal/domain/gateway/handler.go 的 `serveNonStream` 上游错误默认分支：读上游错误体并解析出 (status, errType, errCode, msg) 后，调 Step 6 的 `policyForProtocol`；按 Disposition 执行——`CountIntoHealth=false` 调 `FeedResultGated(..., false)`；`Failover` 显式为 false 或未设置时保持现状（未设置沿用 `retryableUpstreamStatus`）；`Action=drain|disable` 调 channel 管理器 `ManualSetKeyState`/`ManualSetModelState`；`DownstreamStatus≠0` 用它替代默认 502。
loop: until 单测通过
max_iterations: 3
verify: cd backend && go test ./internal/domain/gateway/ -run Policy -v

- [x] **Step 9: 网关接入策略（流式，与非流式同构）**
action: 修改 backend/internal/domain/gateway/handler.go 的 `serveStream` 对应错误分支，复用 Step 8 的同一策略决策函数（抽取为包内共享 `applyErrorPolicy(...)`，非流式与流式都调用），保证两处语义一致。
loop: until 单测通过
max_iterations: 3
verify: cd backend && go test ./internal/domain/gateway/ -run Policy -v

- [x] **Step 10: 协议级错误统计表 + 写入**
action: 新建迁移 backend/internal/db/migrations/0015_protocol_error_stats.sql 建表 `protocol_error_stats(protocol_id BIGINT NOT NULL, category TEXT NOT NULL, day DATE NOT NULL, count BIGINT NOT NULL DEFAULT 0, PRIMARY KEY(protocol_id, category, day))`。在 Step 8 的策略决策处，命中错误类别时对该表 upsert `count = count + 1`（量小，直接同步 upsert；category 取匹配到的错误类别标识，如 `status:422` 或 `code:<errCode>`）。
loop: false
verify: cd backend && go test ./internal/domain/gateway/ -run Stats -v

- [x] **Step 11: 控制台端点——协议级错误统计查询**
action: 在 backend/internal/domain/channelprotocol/handler.go 增加 `GET /admin/channel-protocols/{id}/error-stats?from=&to=`，返回按 category 聚合的计数（从 protocol_error_stats 汇总）。路由注册照抄同文件既有 admin 路由的注册方式。
loop: false
verify: cd backend && go test ./internal/domain/channelprotocol/ -run ErrorStats -v

- [x] **Step 12: 前端——协议编辑页错误策略配置 + 统计展示**
action: frontend/src/views/admin/ChannelProtocolEdit.vue 增加「错误策略」配置区（生效范围单选 整渠道/指定模型 + 模型多选；错误签名 status/type/code/message 输入；处置开关 计入健康/failover/动作/下游状态码）；frontend/src/api/admin.ts 扩展 `ErrorPolicy` 类型与 `getChannelProtocolErrorStats(id)`。协议详情或服务状态页增加按类别计数展示。
loop: false
verify: cd frontend && npm run build

- [x] **Step 13: 人工评审——默认策略下的健康熔断语义**（人工签核：默认=全部计入+能转移就转移）
action: 汇总「未配置策略时」各错误类别（422/404/400/5xx/429/401）的 CountIntoHealth 取值与 failover 取值，形成一页对照表，提交人工确认（risk_level high，禁止自动通过）。
gate: human

- [x] **Step 14: 全量回归 + 竞态**
action: 运行全量后端测试与竞态检测，确认默认安全态下行为与改造前一致。
verify:
  - type: shell
    command: cd backend && go test ./... && go build ./... && go vet ./...
  - type: shell
    command: cd backend && go test -race ./internal/domain/channel/... ./internal/domain/gateway/... ./internal/domain/channelprotocol/...