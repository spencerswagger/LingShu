---
design_type: feature
created_at: 2026-10-16
---

# Auto 模型（对外模型有序别名组）

> Status: ALIGNED
> Author: agent
> Last updated: 2026-10-16

## Intent Contract

```
intent: 在对外模型之上引入「Auto 模型」别名层——Auto 是一个有序的对外模型成员列表，请求 Auto 名时按序选择第一个可用的成员对外模型，解析完成后完全沿用现有路由/亲和/计费/failover 链路，使用户无需随上游模型演进更换请求名。

constraints:
  - 不改动计费五段公式、usage 归一化、cost 模式（PricingModeCost）与准入校验逻辑。
  - 不改动既有路由选择算法（priority/成本/存活会话数排序）、会话亲和粒度与 failover 语义。
  - 不放开 channel_models 唯一约束 uq_channel_models_active(channel_id, internal_model_id)；Auto 不直接绑定渠道。
  - billing_records.external_model_name 语义保持为「实际命中的对外模型名」，现有统计/查询/前端交互不变。
  - Auto 名与 external_models.external_name 不重名；Auto 不可嵌套 Auto。
  - 全局 Auto 与私有 Auto 不允许重名（私有不能覆盖全局）。
  - 迁移单向新增，不改既有列语义。

success_criteria:
  - 管理员可创建/编辑/删除多个全局 Auto，每个仅维护一个有序的对外模型成员列表（可部分引入，不要求全量）。
  - 开发者可创建仅对自己生效的私有 Auto。
  - 请求 auto 名：按成员 position 升序逐个试路由，第一个有可用候选的成员被选中；选择后请求链路与直接请求该成员完全一致（含会话粘性、渠道 failover）。
  - 计费按实际命中成员的售价（cost 模式按实际命中渠道成本），总额与直接请求该成员一致。
  - 账单记录：external_model_name 记命中成员对外模型名；新增列记录请求的 Auto 名（非 Auto 请求为 NULL）。
  - 全部成员均无可用候选时返回「无可用渠道」，拒绝账单的 external_model_name 为空、auto 名列保留请求的 Auto 名。
  - GET /v1/models 列出全局 Auto + 该令牌用户的私有 Auto + 启用对外模型。
```

## Verification Contract

```
verify_steps:
  - run tests: cd backend && go test ./...
  - build: cd backend && go build ./... ; cd frontend && npm run build ; npx vue-tsc --noEmit
  - check: 起后端（迁移自动执行）+ 假上游；构造 global auto（成员 deepseek-flash > glm-flash）复现：
      1) 请求 auto 名 → 上游收到的是 deepseek-flash 对应的内部模型；
      2) 停用 deepseek-flash 的全部渠道 → 请求 auto 名 → 命中 glm-flash；
      3) 全部成员不可用 → 503 无可用渠道，账单 external_model_name 为空、auto 列保留 auto 名；
      4) 同一 session 连续请求 auto，成员排序变更后仍粘在首轮命中成员；
      5) 对比直接请求 deepseek-flash 与请求 auto（命中 deepseek-flash）的账单金额一致。
  - confirm:
      - billing_records.external_model_name = 命中成员名，auto_model_name = auto 名。
      - GET /v1/models 含 auto 名（管理员建全局、开发者看自己私有）。
```

## Governance Contract

```
approval_gates:
  - 数据模型变更（新增 auto_models / auto_model_members 表、billing_records 新增列）需人工评审。
  - 网关 handler 解析层前置（影响所有 /v1 请求入口）属高风险路径，需人工评审。
  - 私有 Auto 首次引入「用户级私有模型实体」概念，命名/可见性规则需人工确认。
rollback: 代码回滚到 main；迁移为纯新增表与新增可空列，回滚无数据修复需求（保留列不影响旧代码）。
ownership: agent 实现，用户验收。
```

## Scope

| In scope | Out of scope |
|---|---|
| 新增 auto_models / auto_model_members 表与迁移 | 按负载/成本动态调整成员顺序（本期明确不做） |
| Auto 解析层（请求名 → 成员按序试路由） | Auto 嵌套 Auto（禁止） |
| 计费按命中成员售价（sale）/ 成本（cost） | 修改计费公式、usage 归一化口径 |
| 账单新增 auto 名记录列 | 修改现有统计聚合口径与查询接口签名 |
| 管理端全局 Auto CRUD（含成员排序） | 多渠道多实例分布式配置刷新（单实例内存模型，沿用现状） |
| 开发者端私有 Auto CRUD | 私有 Auto 的配额/审批流程 |
| GET /v1/models 列出全局 + 私有 Auto | Auto 级别的限流/标签绑定 |
| 前端：管理端与开发者端 Auto 管理页面 | Auto 级别的独立售价卡（按成员计价，不设 Auto 售价） |

## Decisions

| # | decision | choice | rejected alternatives |
|---|---|---|---|
| 1 | Auto 语义 | 业务侧模型演进别名：按序选第一个可用成员，与成本无关 | A 纯故障转移链 / B 每次择优 / C 能力路由（用户明确均非本意） |
| 2 | 会话内行为 | 会话内粘性不变：仅首次/切换时按序选，选定后沿用现有亲和 | 每轮重排（对话中途换模型，用户可感知） |
| 3 | 是否改动 channel_models 唯一约束 | 不改；Auto 不直接绑渠道，成员才是绑定实体 | 放开 (channel_id, internal_model_id) 为含 external_model_id（改动大、风险高） |
| 4 | Auto 建模方式 | 方案 A：独立实体 auto_models + auto_model_members | B 复用 external_models（需拆全局唯一索引加 kind/owner，风险高）/ C sys_configs JSON 别名（隐式逻辑） |
| 5 | 计价方式 | 按实际命中成员的售价（cost 模式按命中渠道成本），公式零改动 | Auto 自带统一售价卡（平台承担成员成本差） |
| 6 | 账单模型名归属 | external_model_name 记命中成员对外模型名；新增列记 Auto 名 | 混合记入 external_model_name（污染统计）/ 只记成员（用户按 auto 搜不到账单） |
| 7 | 命名与可见性 | 全局与私有不允许重名（私有不能覆盖全局），Auto 名不得与对外模型名重名 | 私有优先覆盖全局（隐性偏离平台推荐）/ 强制前缀（请求啰嗦） |
| 8 | 嵌套 | 禁止 Auto 作为成员 | — |
| 9 | 成员顺序语义 | position 升序，逐个「试路由」，第一个有可用候选者被选中 | 合并全部成员候选后统一排序（会打乱用户设定的成员优先级） |
| 10 | 成员不可用 | 成员停用/删除/无候选时跳过该成员，不做级联删除 | 级联清理 Auto 成员 |
| 11 | 全部成员不可用 | 返回无可用渠道；拒绝账单 external_model_name 为空、auto 列保留请求的 Auto 名（便于按 Auto 排查） | 两列均记空 |
| 12 | /v1/models | 列出全局 Auto + 用户私有 Auto + 启用对外模型 | 不列 Auto（客户端下拉看不到，体验断裂） |

## Surface

**存储（新增迁移，单向）**

新增 `auto_models`：`id` / `name` / `scope`（global | private）/ `owner_user_id`（可空，global 为空）/ `enabled` / `description` / `deleted_at` / 时间戳。

- 唯一性：单一扁平命名空间——`(name)`（`deleted_at IS NULL` 部分唯一），即任意两个 Auto（无论 scope 与 owner）不得重名。
- 跨表校验：应用层保证 `auto_models.name` 不与任何 `external_models.external_name` 重名。

新增 `auto_model_members`：`id` / `auto_model_id`（外键）/ `external_model_id`（外键）/ `position`（升序优先级）/ `deleted_at` / 时间戳；按 `(auto_model_id, position)` 组织。

`billing_records` 新增可空列（记请求的 Auto 名，非 Auto 为 NULL）；`call_logs` 同步新增可空列以保持与账单 1:1 的决策轨迹可读。

**解析层（网关入口前置）**

在 handler 解析请求体得到请求模型名后、进入现有路由之前，插入 Auto 解析：

1. 依名查 Auto（单条查询内完成可见性过滤：global 对所有用户可见，private 仅 owner 可见；因扁平命名空间，二者至多命中其一）。
2. 命中 Auto → 老会话粘性优先：若该会话已绑定的成员对外模型名（`Session.Model`）仍可路由（该对外模型仍存在且有候选），则**直接沿用该成员**，完全不参考当前成员列表（成员重排/增删都不改变老会话落点）。
3. 新会话，或老会话原成员已不可路由 → 取成员按 `position` 升序；对每个成员，以「成员对外模型名」调用现有路由（`RouteAll` / 候选收集）；第一个返回非空候选的成员即被选中，此后 `model` 替换为该成员名。
4. 未命中 Auto → 保持现有逻辑（`GetByExternalName`）。
5. 全部成员均无候选 → 按「无可用渠道」返回，并落拒绝账单（external_model_name 为空、auto 列保留请求的 Auto 名）。

Auto 解析必须早于预扣估算：预扣金额需以命中成员的售价计算（否则 Auto 名查不到售价会静默跳过预扣，使余额前置校验失效）。

**计费**

售价/时段/分档取值由「按请求名取对外模型」改为「按命中成员取对外模型」——`recordBilling` 与 `estimatePreConsumeCost` 内取模型的入参统一为命中成员名；五段公式、cost 模式、usage 归一化不变。账单 `external_model_name` 保持为命中成员名，新增列写入请求的 Auto 名。

**接口**

- 管理端：`/api/v1/admin/auto-models` 全套 CRUD（含成员列表读写与排序）。
- 开发者端：`/api/v1/dev/auto-models` CRUD，仅作用于本人（`owner_user_id` 强制为当前用户）。
- `GET /v1/models`：返回值并入全局 Auto 与该令牌用户的私有 Auto；需在鉴权后保留 token（当前实现校验后即丢弃，需改为持有 userID）。

**前端**

- 管理端：新增 Auto 模型列表/编辑页（成员为可排序的对外模型选择列表），导航「模型」分组下入口，沿用现有列表页范式（合并页头、图标操作列、行点击详情）。
- 开发者端：新增私有 Auto 管理入口（`/dev/auto-models`），复用同一成员编辑组件。
- 账单/会话详情：展示 Auto 名（若非空），保留命中对外模型展示。

## Risks & Open Questions

- **入口前置的影响面**：Auto 解析插在 `/v1` 请求入口，任何解析异常都会影响全部请求；需保证未命中 Auto 时零行为差异（性能与语义），并覆盖单测。
- **跨表重名校验的并发窗口**：Auto 与对外模型分属两表，应用层校验存在并发创建同名窗口；需在创建/改名时双重检查，必要时以数据库层约束兜底。
- **预扣口径一致性**：预扣以命中成员售价估算，结算以实际命中成员售价结算——两者须取同一成员，避免 Auto 场景下「预扣用 A 成员、结算用 B 成员」导致的退补跳变。
- **私有 Auto 可见性边界**：私有 Auto 仅对 owner 生效；需确认同名私有与全局的关系在 `/v1/models`、账单、会话列表各处表现一致（本期按「不允许重名」简化）。
- **成员排序调整的生效范围**：因会话粘性不变，调整成员顺序只对新会话与新切换生效；老会话保持原成员直至其不可用——需确认运营可接受此时间差（用户已确认接受）。
- **文档 lint**：本安装未提供 `document-lint.sh`，本次跳过 lint 自检。