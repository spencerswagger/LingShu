---
design_type: feature
created_at: 2026-10-19
---

# 渠道配置默认值对齐（健康探测/限流）

> Status: IMPLEMENTED — 代码与单测已落地，待人工 UI 核验（workflow Step 7 人工闸门）
> Branch: (待定)
> Author: agent
> Last updated: 2026-10-19

## Intent Contract

```
intent: 把「新建渠道」的内置默认值统一校正为：健康探测每天一次(0 0 0 * * *)、RPM 120、TPM 1000 万、并发会话数 100、超限策略拒绝(REJECT)；并消除“改了默认值却不生效”的多副本漂移现象。
constraints:
  - 仅影响新建渠道的默认值；不得改动任何存量渠道已存库中的 rate_limit / health_probe 配置。
  - 不改动限流算法、探测调度、状态机与计费口径。
  - 不引入新依赖、不新增默认值接口；保持现有「默认值在多处副本中并维护」的结构（经用户确认）。
  - 不改动 DB schema 与列默认值（rate_limit / health_probe 仍为 JSONB '{}'，空值由 normalize 兜底）。
  - backend go test ./... 必须保持全绿。
success_criteria:
  - 后端 DefaultRateLimit 返回 RPM=120、TPM=10000000、MaxConcurrent=100、OnExceed=REJECT。
  - 后端 DefaultHealthProbe 返回 Interval="0 0 0 * * *"（每天 00:00 一次）。
  - 前端「新建渠道」页初始表单与提交兜底值同上述口径一致（RPM 120 / TPM 10 百万 / 并发 100 / 拒绝 / 每天一次）。
  - 前端「编辑渠道」页同名字段兜底值一致（仅影响库中为空值时的回显，不迁移存量数据）。
  - 单测 TestDefaultConfigs 更新为断言新值并通过。
risk_level: low   # 仅默认值常量与前端表单初始值，无安全/计费/数据迁移影响
```

## Verification Contract

```
verify_steps:
  - run tests: cd backend && go test ./...
  - build: cd backend && go build ./... ; cd frontend && npm run build
  - check: 断言 DefaultRateLimit / DefaultHealthProbe 四项取值；核对前端两表单初始值
  - confirm:
      1) go test 中 TestDefaultConfigs 通过且断言 Interval="0 0 0 * * *"、RPM=120、MaxConcurrent=100、OnExceed=REJECT；
      2) 新建渠道页初始显示：探测频率 0 0 0 * * *、RPM 120、TPM 10(百万)、并发会话数 100、超限策略「拒绝」；
      3) 新建并保存后，编辑页回显与上述一致；
      4) 存量渠道（旧默认 1000/16/QUEUE/每小时）打开编辑页仍显示其库中原值，未被改写。
```

## Governance Contract

```
approval_gates:
  - 本设计文档需用户确认后方可进入实施（当前处于 HARD GATE 前）。
rollback: 代码回滚到改动前提交即可；无数据迁移、无 schema 变更，回滚无需数据修复。
ownership: agent 实现，用户验收。
```

## Scope

| In scope | Out of scope |
|---|---|
| 后端 DefaultRateLimit 四项默认值校正 | 存量渠道数据迁移/刷新（用户已明确不改） |
| 后端 DefaultHealthProbe.Interval 由「每小时」修正为「每天一次」 | 限流算法 / 探测调度 / 状态机逻辑变更 |
| 前端新建/编辑表单同口径默认与兜底值 | 新增「默认值」接口或抽取单一来源（用户选择保持多副本） |
| TestDefaultConfigs 断言更新 | DB schema / 列默认值变更 |

## Decisions

| # | decision | choice | rejected alternatives |
|---|---|---|---|
| D1 | 生效范围 | 仅新建渠道默认值；存量渠道不动 | 迁移同旧默认值的存量渠道 / 强制覆盖全部存量渠道（风险高，用户否决） |
| D2 | 默认值组织方式 | 保持多副本逐一同步（后端 2 个 Default + 前端 2 个表单 + 单测） | 后端单一来源接口 + 前端拉取；仅抽前端共享常量 |
| D3 | 健康探测频率表达式 | `0 0 0 * * *`（6 段：秒 分 时 日 月 周，每天 00:00:00） | 保留 `0 0 * * * *`（实为每小时一次，与注释/预期不符，为原缺陷） |
| D4 | 超限策略默认 | `REJECT` | 保留 `QUEUE` |

## Surface

后端默认值常量：`channel.DefaultRateLimit()` 与 `channel.DefaultHealthProbe()`（manager.go）。二者经 fillDefaults / normalize 在创建渠道时兜底零值，并作为缺失渠道的回退值（如 KeyMaxSessions）。TPM 后端已是 10,000,000，无需改动。

后端单测：`manager_test.go` 中 `TestDefaultConfigs` 现断言 `Interval == "0 0 * * * *"`，需改为 `0 0 0 * * *`；建议补断言 RPM/MaxConcurrent/OnExceed 以防再次漂移。

前端表单：`ChannelCreate.vue` 与 `ChannelEdit.vue` 的 reactive 表单初始值、TPM 录入初值（1 百万 → 10 百万）、以及提交与回显时的字符串/数值兜底值（`|| 'QUEUE'`、`|| 16`、`|| '0 0 * * * *'`）。前端无独立默认值模块，二者各自内联维护。

数据契约：默认值最终以 rate_limit / health_probe JSONB 存入 channels 表，结构不变；本设计不触碰表结构与列默认值。

## Risks & Open Questions

- 副本漂移（已知、接受）：默认值分散于后端两处 + 前端两处 + 单测，本次仅做值对齐；将来再改仍需手工同步全部副本，这是「改了不生效」的根因，D2 选择暂不根治。
- 存量渠道观感：修 Default 后，已存在渠道（旧默认值）在列表/编辑页仍显示旧值（如 RPM 1000、QUEUE）。这是「仅新建默认」的预期行为，若用户后续希望一并刷新，需另立迁移任务。
- `0 = 不限` 语义冲突（既有缺陷，本次不处理）：限流 UI 提示 RPM/TPM/并发「0 不限」，但 normalizeRateLimit 会把 0 视为零值并替换为默认值，故 0 实际不可表达「不限」。本次仅记录，不改变 normalize 行为，避免扩大范围。
- 编辑页默认值作用有限：ChannelEdit 的表单初始值会被 load() 用行数据覆盖，故其改动主要影响库中字段为空值时的回显一致性，不改变存量渠道。
- lint 工具缺失：hotl 插件的 document-lint.sh / hotl-config-resolve.sh 未随本次安装提供，设计文档采用人工等价自检（七段结构 + 契约一致性 + 无实现泄漏）。
