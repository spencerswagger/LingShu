---
intent: 把「新建渠道」的内置默认值统一校正为：健康探测每天一次(0 0 0 * * *)、RPM 120、TPM 1000 万、并发会话数 100、超限策略拒绝(REJECT)，并消除多副本漂移导致的“改了默认值却不生效”。
success_criteria: 后端 DefaultRateLimit 返回 RPM=120/TPM=10000000/MaxConcurrent=100/OnExceed=REJECT；DefaultHealthProbe 返回 Interval="0 0 0 * * *"；前端新建/编辑表单默认与兜底值同口径一致；go test ./... 全绿；前端 vue-tsc 构建通过；存量渠道库中配置不被改写。
risk_level: low
auto_approve: true
---

## Steps

- [x] **Step 1: 校正后端默认值常量（manager.go）**
action: 编辑 backend/internal/domain/channel/manager.go。在 DefaultRateLimit() 中把 RPM 由 1000 改为 120、MaxConcurrent 由 16 改为 100、OnExceed 由 OnExceedQueue 改为 OnExceedReject（TPM 保持 10000000，BurstMultiplier/QueueSize/QueueTimeoutMS 不动）。在 DefaultHealthProbe() 中把 Interval 由 "0 0 * * * *" 改为 "0 0 0 * * *"，并修正该行注释为「默认每天 00:00 一次」。
loop: false
max_iterations: 1
verify: cd backend && go build ./...
gate: auto

- [x] **Step 2: 同步单测断言（manager_test.go）**
action: 编辑 backend/internal/domain/channel/manager_test.go 的 TestDefaultConfigs：把 Interval 断言由 "0 0 * * * *" 改为 "0 0 0 * * *"（并修正失败信息文案）；新增断言 DefaultRateLimit().RPM==120、DefaultRateLimit().MaxConcurrent==100、DefaultRateLimit().OnExceed=="REJECT"。保留既有 TPM==10000000 与 P99 断言不动。
loop: false
max_iterations: 1
verify: cd backend && go test ./internal/domain/channel/ -run TestDefaultConfigs -v
gate: auto

- [x] **Step 3: 同步前端新建表单默认值（ChannelCreate.vue）**
action: 编辑 frontend/src/views/admin/ChannelCreate.vue。表单初始值：rpm 1000→120、on_exceed 'QUEUE'→'REJECT'、max_concurrent 16→100、interval '0 0 * * * *'→'0 0 0 * * *'。TPM 录入初值 tpmInput 由 1 改为 10（单位仍为百万，对应 10000000），并同步该行注释。提交兜底 Interval: form.interval || '0 0 * * * *' 改为 || '0 0 0 * * *'。不得改动限流/探测其余字段与 UI 结构。
loop: false
max_iterations: 1
verify: grep -q "rpm: 120" frontend/src/views/admin/ChannelCreate.vue && grep -q "on_exceed: 'REJECT'" frontend/src/views/admin/ChannelCreate.vue && grep -q "max_concurrent: 100" frontend/src/views/admin/ChannelCreate.vue && grep -q "interval: '0 0 0 \* \* \*'" frontend/src/views/admin/ChannelCreate.vue && grep -q "tpmInput = ref<number>(10)" frontend/src/views/admin/ChannelCreate.vue && grep -q "|| '0 0 0 \* \* \*'" frontend/src/views/admin/ChannelCreate.vue && ! grep -q "rpm: 1000" frontend/src/views/admin/ChannelCreate.vue
gate: auto

- [x] **Step 4: 同步前端编辑表单默认与兜底值（ChannelEdit.vue）**
action: 编辑 frontend/src/views/admin/ChannelEdit.vue。表单初始默认：on_exceed 'QUEUE'→'REJECT'、max_concurrent 16→100、interval '0 0 * * * *'→'0 0 0 * * *'。load() 回显兜底：on_exceed || 'QUEUE'→|| 'REJECT'、max_concurrent || 16→|| 100、interval || '0 0 * * * *'→|| '0 0 0 * * *'。提交兜底 Interval: form.interval || '0 0 * * * *' 改为 || '0 0 0 * * *'。保持 load() 用行数据覆盖初始值的机制不变（存量渠道仍回显其库中原值）。
loop: false
max_iterations: 1
verify: grep -q "on_exceed: 'REJECT'" frontend/src/views/admin/ChannelEdit.vue && grep -q "max_concurrent: 100" frontend/src/views/admin/ChannelEdit.vue && grep -q "interval: '0 0 0 \* \* \*'" frontend/src/views/admin/ChannelEdit.vue && grep -q "|| 'REJECT'" frontend/src/views/admin/ChannelEdit.vue && grep -q "|| 100" frontend/src/views/admin/ChannelEdit.vue && grep -q "|| '0 0 0 \* \* \*'" frontend/src/views/admin/ChannelEdit.vue && ! grep -q "0 0 \* \* \* \*" frontend/src/views/admin/ChannelEdit.vue
gate: auto

- [x] **Step 5: 后端全量测试**
action: 运行后端完整测试套件，确认默认值改动未破坏任何既有用例（尤其 channel 域、gateway 域）。
loop: true
max_iterations: 3
verify: cd backend && go test ./...
gate: auto

- [x] **Step 6: 前端类型检查与构建**
action: 运行前端构建（含 vue-tsc --noEmit 类型检查），确认为改动无误。
loop: true
max_iterations: 3
verify: cd frontend && npm run build
gate: auto

- [ ] **Step 7: 人工核对 UI 默认值与存量不受影响**
action: 启动前端（cd frontend && npm run dev）。打开「新建渠道」页，确认：健康探测频率初始为 0 0 0 * * *、RPM 120、TPM 输入框显示 10（百万单位）、并发会话数 100、超限策略选中「拒绝」；切换 TPM 单位为「无」应显示 10000000。再打开一个改动前已存在的渠道编辑页，确认其 RPM/并发/超限策略仍显示库中原值（未被改写）。
loop: false
max_iterations: 1
verify:
  type: browser
  url: http://localhost:5173/admin/channels/create
  check: 新建表单初始值 RPM=120、TPM 输入=10(百万)、并发会话数=100、超限策略=拒绝、探测频率=0 0 0 * * *；已有渠道编辑页仍显示其库中原值
gate: human
