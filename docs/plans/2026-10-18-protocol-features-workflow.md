---
intent: 在渠道协议上引入可挂载的「协议特性(features)」机制（代码实现、按 key + 参数挂载，可多项启停），并落地两个内置特性：thinking-roundtrip（请求侧按上游约定回传 reasoning_content，修复 S6）与 tool-schema-normalize（对齐工具 input_schema，修复 S4）。
success_criteria:
  - 渠道协议可挂载/卸载特性并携带参数，未知特性 key 被校验拒绝。
  - 开启 thinking-roundtrip 后，DeepSeek thinking 模式多轮请求不再因未回传 reasoning_content 而 400。
  - 开启 tool-schema-normalize 后，web_search 类工具请求不再因 input_schema 缺失而 422。
  - 未挂载任何特性时，上游请求体与改造前逐字节一致。
risk_level: high
auto_approve: false
---

## Steps

- [ ] **Step 1: 迁移——channel_protocols 增加 features 列**
action: 新建迁移 backend/internal/db/migrations/0015_channel_protocol_features.sql（若 P1 已占用 0015，则用当前最大编号+1），内容为 `ALTER TABLE channel_protocols ADD COLUMN IF NOT EXISTS features JSONB NOT NULL DEFAULT '[]'::jsonb;`。在 backend/internal/domain/channelprotocol/model.go 的 ChannelProtocol 上加 `Features []Feature`，并在 store.go 的列清单/扫描/写入加该列（照抄既有 `models` JSONB 处理）。
loop: false
verify: cd backend && go build ./... && go test ./internal/domain/channelprotocol/...

- [ ] **Step 2: 定义特性类型**
action: 在 backend/internal/domain/channelprotocol/model.go 增加 `Feature{ Key string; Params map[string]any }` 与合法 key 集合常量（`thinking-roundtrip`、`tool-schema-normalize`）；在 service.go 校验中：Key 必须 ∈ 合法集合，Params 允许为空 map。
loop: false
verify: cd backend && go build ./...

- [ ] **Step 3: 网关特性注册表与扩展点接口**
action: 新建 backend/internal/domain/gateway/features.go，定义 `RequestFeature interface { Key() string; RewriteRequest(req *UpstreamRequest) error }` 与 `ResponseFeature interface { Key() string; NormalizeResponse(body []byte) ([]byte, error) }`，以及 `featureRegistry map[string]...` 与 `registerFeatures()` 注册初始两个特性（Step 6/7 实现）。提供 `requestFeaturesFor(protocolFeatures []channelprotocol.Feature) []RequestFeature` 与响应侧同构函数，按协议 features 顺序解析。
loop: false
verify: cd backend && go build ./...

- [ ] **Step 4: 请求侧特性接入 buildUpstreamRequest**
action: 修改 backend/internal/domain/gateway/headers.go 的 `buildUpstreamRequest`：在透传→鉴权注入→set 覆盖之后，按 Step 3 的 `requestFeaturesFor` 顺序调用各 `RewriteRequest`。未挂载特性时保持原样（零差异）。
loop: false
verify: cd backend && go test ./internal/domain/gateway/ -run UpstreamRequest -v

- [ ] **Step 5: 响应侧特性接入**
action: 在 backend/internal/domain/gateway 的上游响应解析入口（非流式响应体读取处、流式 usage 拼装后）按 `responseFeaturesFor` 顺序调用 `NormalizeResponse`；接入点选在 usage 归一之前、计费之前，保证计费读取归一后的内容。未挂载特性时零差异。
loop: false
verify: cd backend && go test ./internal/domain/gateway/ -run NormalizeResponse -v

- [ ] **Step 6: 内置特性 thinking-roundtrip**
action: 新建 backend/internal/domain/gateway/feature_thinking.go 实现 RequestFeature：当参数 `enabled=true` 时，把消息历史中的 `reasoning_content` 按上游约定回传（在 assistant 消息中带上该字段）；由 Params 控制是否仅在 thinking 模式生效。注册到 registry。
loop: until 单测通过
max_iterations: 3
verify: cd backend && go test ./internal/domain/gateway/ -run ThinkingRoundtrip -v

- [ ] **Step 7: 内置特性 tool-schema-normalize**
action: 新建 backend/internal/domain/gateway/feature_toolschema.go 实现 RequestFeature：扫描请求 `tools`，对缺失/非法 `input_schema` 的工具（如 `web_search_20250305`）补齐合法 schema（内置一个最小 `web_search` schema，可被 Params.schema 覆盖）。注册到 registry。
loop: until 单测通过
max_iterations: 3
verify: cd backend && go test ./internal/domain/gateway/ -run ToolSchemaNormalize -v

- [ ] **Step 8: 单测——注册表、校验、未挂载零差异**
action: 新增 backend/internal/domain/gateway/features_test.go：未知特性 key 被 channelprotocol 校验拒绝；协议挂载 `[]` 时 `buildUpstreamRequest` 输出与不挂载逐字节一致；灰阶：仅挂载一个特性时另一个不生效。
loop: false
verify: cd backend && go test ./internal/domain/gateway/ -run Features -v

- [ ] **Step 9: 前端——协议编辑页特性配置区**
action: frontend/src/views/admin/ChannelProtocolEdit.vue 增加「特性」区：可勾选/卸载 `thinking-roundtrip`、`tool-schema-normalize`，并编辑其参数（key-value）；frontend/src/api/admin.ts 扩展 `ProtocolFeature` 类型。
loop: false
verify: cd frontend && npm run build

- [ ] **Step 10: 人工评审——请求/响应改写正确性**
action: 汇总两个内置特性的改写规则（涉及哪些字段、thinking 多轮行为、计费口径不受影响），提交人工评审（risk_level high，禁止自动通过）。
gate: human

- [ ] **Step 11: 全量回归 + 竞态**
action: 运行全量后端测试与竞态检测。
verify:
  - type: shell
    command: cd backend && go test ./... && go build ./... && go vet ./...
  - type: shell
    command: cd backend && go test -race ./internal/domain/gateway/... ./internal/domain/channelprotocol/...