---
intent: 对外模型支持多个别名，使工具（如 claude code）内置的默认模型名可直接命中已配置的对外模型；别名仅用于模型名解析，不出现在 /v1/models。
success_criteria:
  - 对外模型可配置多个别名，请求别名可解析到该模型并沿用既有路由/计费链路。
  - 别名改名/删除/模型启停后，解析即时生效（无缓存残留）。
  - 别名不出现在 GET /v1/models。
  - 别名与任何 external_name、任何 Auto 名不重名。
risk_level: medium
auto_approve: false
---

## Steps

- [x] **Step 1: 迁移——external_models 增加 aliases 列**
action: 新建迁移 backend/internal/db/migrations/0016_external_model_aliases.sql（编号取当前最大+1），内容为 `ALTER TABLE external_models ADD COLUMN IF NOT EXISTS aliases JSONB NOT NULL DEFAULT '[]'::jsonb;`。在 backend/internal/domain/model/store.go 的 `ExternalModel` 结构体与 `cols` 常量中加入 `aliases`（编解码照抄同表既有 `time_config` JSONB 处理）。
loop: false
verify: cd backend && go build ./... && go test ./internal/domain/model/...

- [x] **Step 2: 单测——迁移幂等与默认空**
action: 在 backend/internal/domain/model 增加用例：新建模型未指定别名时读出为空切片；重复迁移不报错。
loop: false
verify: cd backend && go test ./internal/domain/model/ -run Alias -v

- [x] **Step 3: 解析扩展——GetByExternalName 支持别名**
action: 修改 backend/internal/domain/model/store.go 的 `GetByExternalName`：查询条件从 `external_name = $1` 扩展为 `external_name = $1 OR aliases @> to_jsonb(ARRAY[$1])`（仍限定 `enabled = true AND deleted_at IS NULL`）。缓存键沿用入参名字（别名与其规范名为**不同键**、指向同一模型条目），负缓存语义不变。
loop: false
verify: cd backend && go test ./internal/domain/model/ -run GetByExternalName -v

- [x] **Step 4: 校验——别名唯一性与命名空间**
action: 在 backend/internal/domain/model/service.go 的创建/更新校验中加入：别名列表去重、非空校验、不得与本模型 `external_name` 重复；别名不得与任何**其他** `external_name` 重复；别名不得与任何 Auto 名重复（调用既有 automodel 重名校验，双向：Auto 创建时也校验不撞别名）。冲突返回明确错误。
loop: false
verify: cd backend && go test ./internal/domain/model/ -run AliasValidate -v

- [x] **Step 5: 缓存失效覆盖别名键**
action: 确认/补齐 backend/internal/domain/model/store.go 的失效钩子：`Insert`（按名+别名逐个失效）、`Update`（改名/改别名→整体或按旧/新键失效）、`BatchDelete`（整体清空）均已覆盖别名键；并核对 service 层包装失效点同步。
loop: false
verify: cd backend && go test ./internal/domain/model/ -run AliasInvalidate -v

- [x] **Step 6: 单测——别名命中与即时失效**
action: 表驱动用例：按别名命中正确模型；改名/删除别名后下一次按旧别名不再命中（且无残留正/负缓存）；对未变更请求断言零 DB（注入 fake store 计数）。
loop: false
verify: cd backend && go test ./internal/domain/model/ -run AliasResolve -v

- [x] **Step 7: 前端——模型编辑页别名输入**
action: frontend/src/views/admin/ModelEdit.vue（或 ModelCreate.vue）增加「别名」多值输入（复用既有 StringListEditor 组件）；frontend/src/api/admin.ts 的对外模型类型加 `aliases`。保存/回显与既有字段一致。
loop: false
verify: cd frontend && npm run build

- [x] **Step 8: 单测——别名不进 /v1/models**
action: 在 /v1/models 列表构造路径加/复用用例：断言输出仅含启用对外模型名与 Auto 名，绝不包含别名。
loop: false
verify: cd backend && go test ./internal/domain/gateway/ -run Models -v

- [x] **Step 9: 全量回归 + 竞态**
action: 运行全量后端测试与竞态检测，确认模型解析与缓存行为无回归。
verify:
  - type: shell
    command: cd backend && go test ./... && go build ./... && go vet ./...
  - type: shell
    command: cd backend && go test -race ./internal/domain/model/... ./internal/domain/gateway/...