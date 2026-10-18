---
design_type: feature
created_at: 2026-10-18
---

# 会话成本路由：会话亲和 + 逐轮成本决策

> Status: DRAFT (待评审)
> Branch: trae/agent-gRQMdM（实现可另开分支）
> Author: agent
> Last updated: 2026-10-18

## Intent Contract

```
intent: 在同一对外模型下，把渠道路由从“会话无脑粘性”升级为“会话亲和 + 逐轮成本决策”——按渠道成本价的三档价（输入 / 缓存读 / 输出）叠加时段倍率与上下文分档倍率，在会话起点做零成本择优、在会话中途以“预计剩余轮数 > 回本轮数”为门控做保守切换，并把切换事件与“假设节省 / 相位”经异步落库记录；不改变对外售价计费口径与响应语义。
constraints:
  - 成本决策口径 = 渠道成本价 channel_models.cost_rates（三档）；对外售价 external_models.sale_rates 仅用于对用户计费，不参与路由。
  - 计费公式、系数、对外售价、响应语义不变；三段 usage 口径不变（input=纯输入，cache_read=缓存读，output=含推理）。
  - 倍率来源：时段倍率取 billing.time_config（含模型级 time_config 覆盖），按当前时刻；上下文分档取 billing.context_tiers（含模型级覆盖），按“本轮 输入 + 缓存读 = 实际上下文规模”落档。
  - 不依赖检测 provider 侧缓存失效（上下文压缩 / TTL 空闲过期对网关不可感知）；会话中途切换一律按最坏情况保守计入一次性重灌代价。
  - 会话亲和默认保持（SessionKey 命中优先不失效）；仅当决策门控通过才切换。
  - “预计剩余轮数”用会话位置 k 的可配置经验式；观测写入沿用既有 asyncwrite，不新增同步 DB 往返。
  - 单实例假设，不引入 Redis 广播。
  - 成本决策与切换能力默认处于“安全态”（等价现状的纯会话粘性），逐阶段开关可独立回退。
  - go build / go vet / go test ./... 必须绿；并发路径 go test -race 零告警。
success_criteria:
  - 路由成本比较纳入“三档成本价 × 当前时段倍率 × 当前上下文分档倍率”，替换现有“分档系数恒 1、仅用 routing.cost_sample 样值”的比较口径；样值可由会话本轮真实 token 明细驱动，缺省回退 cost_sample。
  - 会话起点：按 argmin( 本轮花费 + 一次性重灌代价 ÷ 预计剩余轮数 ) 选渠道；起点无旧缓存，重灌代价为 0。
  - 会话中途：仅当 预计剩余轮数 > 回本轮数 才切换，否则保持亲和。
  - 回本轮数 = 分子 ÷ 分母（分母 ≤ 0 即永不回本，不切换）；一次性重灌代价 = 缓存读 token × (新渠道输入价 − 新渠道缓存读价) × 新渠道时段倍率 × 分档倍率 ÷ 1e6。
  - 决策产物（选中渠道、本轮花费、重灌代价、回本轮数、决策原因）可在控制台按会话回放。
  - 影子模式：开关关闭时渠道不变，但仍产出“本可切换”的假设节省记录，用于账实校准。
  - 管理界面：可调参数面板 + 临界点图（多渠道曲线汇聚）+ 临界点分布 + 事件流 / 影子对照；线上入参默认从模型成本、会话真实 token、当前时刻与实际分档自动取值。
risk_level: high   # 触及路由成本口径与计费口径的消费（资金相关）
```

## Verification Contract

```
verify_steps:
  - run tests: cd backend && go test ./...
  - race: cd backend && go test -race ./internal/...   # 覆盖 router / gateway / billing / asyncwrite
  - build: cd backend && go build ./... && go vet ./...
  - frontend: cd frontend && npm ci && npm run build
  - check:
      1) 成本口径：构造“最低输入价渠道 ≠ 最优总成本渠道”用例，断言路由择到“三档成本价 × 时段倍率 × 分档倍率”总成本更优者。
      2) 分档落档：给定 输入+缓存读 token，命中配置档位时取对应分档倍率；未命中档位报错（沿用既有 ContextTierCoeff 语义）。
      3) 起点择优：起点重灌代价为 0，按摊还成本最小选渠道；断言与手算一致。
      4) 中途门控：剩余轮数 > 回本轮数 才切；分母 ≤ 0 永不回本不切；断言回本轮数计算与示例一致（Venice→DeepSeek：缓存读 2 万→18 轮、下限 ≈ 5.9、临界 ≈ 1.35 万）。
      5) 亲和保真：SessionKey 命中且门控不通过时渠道不变。
      6) 影子模式：开关关闭时行为与现状一致（渠道不变），仍产出假设节省记录。
      7) 异步落库：切换事件 / 假设节省经 asyncwrite 提交，同 Key 保序、幂等可重放。
  - confirm:
      1) 现有回归全绿；对外售价计费与响应体不变。
      2) 管理界面参数可调、临界点随参数实时变化；线上取值来自模型成本并带来源标签。
      3) 影子模式运行期“假设节省 vs 实际结算”偏差在设定阈值内。
      4) 全量 -race 零告警；P0→P3 各阶段开关可独立回退到纯会话粘性。
```

## Governance Contract

```
approval_gates:
  - 路由成本口径变更（纳入时段 / 分档倍率；样值由 cost_sample → 会话真实 token 明细）落地前，人工评审“不改变对外售价计费与响应语义”。
  - 真实切换上线前逐阶段放行：P1 起点择优、P2 中途切换、P3 闲忙时联动，各阶段开关默认关闭，影子数据达阈值后人工放行（billing / routing，risk_level high）。
  - 管理界面“参数面板 / 临界点图”确认线上默认只读来源，What-if 覆盖不误写真值。
rollback: 每阶段独立开关（影子只读 → 起点择优 → 中途切换 → 闲忙时联动），逐级回退到“纯会话粘性”；路由保留“禁用成本决策=现状”的安全态；观测写入沿用既有异步落库，不新增同步往返。
ownership: agent 实现，用户验收。
```

## Scope

| In scope | Out of scope |
|---|---|
| 路由成本比较纳入“三档成本价 × 时段倍率 × 分档倍率”，替换现有“分档系数恒 1 + 仅 cost_sample 样值”口径 | provider 侧缓存失效信号检测（压缩 / TTL 不可感知；列为未来增强） |
| 会话成本决策：起点择优（重灌代价=0）+ 中途门控（剩余轮数 > 回本轮数） | 多实例一致性 / Redis 失效广播（单实例假设） |
| 预计剩余轮数：会话位置 k 的可配置经验式 | 计费公式、对外售价、响应语义变更 |
| 影子模式（只读假设节省）+ P0–P3 逐阶段开关与回退 | Auto 模型的“按成本重排”（Auto 明确不做，本方案在成本维度决策） |
| 决策产物（选中渠道 / 本轮花费 / 重灌代价 / 回本轮数 / 决策原因）可回放 | 三段 usage 口径与 billing_records 结构变更 |
| 观测：切换事件 / 相位 / 假设节省 走既有 asyncwrite | 令牌 LastUsed 之外新增同步 DB 往返 |
| 管理界面：参数面板（线上取模型成本 / 真实 token / 当前时刻与实际分档）+ 临界点图（多渠道曲线汇聚）+ 临界点分布 + 事件流 / 影子对照 | 渠道协议 / 上游协议逻辑变更 |

## Decisions

| # | decision | choice | rejected alternatives |
|---|---|---|---|
| D1 | 成本决策口径 | 用渠道成本价 `channel_models.cost_rates` 三档；售价 `sale_rates` 仅计费 | 用对外售价择优（偏离“省成本”目标）；两套并行（复杂度高） |
| D2 | 倍率纳入 | 时段倍率（按当前时刻 `CoeffFor(now)`）+ 上下文分档倍率（按本轮 输入+缓存读）都纳入路由成本比较 | 维持现状“分档系数恒 1”（无法体现闲忙时 / 长上下文差异） |
| D3 | 成本样值来源 | 优先用会话本轮真实 token 明细；缺失回退 `routing.cost_sample` | 仅用 cost_sample（无法反映会话实际规模） |
| D4 | 决策点 | 起点择优（重灌代价=0）+ 中途门控（剩余轮数 > 回本轮数） | 仅起点择优（错过中途收益）；仅中途（起点未优化） |
| D5 | 预计剩余轮数 | 会话位置 k 的可配置经验式（形如 clamp(a − b·k, min, max)） | 复用 session.expiration turn 档位（语义是 TTL 非剩余轮数）；固定常数（粗糙） |
| D6 | 中途切换计价 | 不检测 provider 缓存失效，按最坏情况保守计入一次性重灌代价 | 依赖“检测到重置点再切”（网关不可感知，不可执行） |
| D7 | 亲和保真 | 门控不通过时保持当前渠道（不切换） | 每轮无条件重新择优（抖动、丢缓存） |
| D8 | 观测落库 | 切换事件 / 相位 / 假设节省走 asyncwrite（复用现有链路） | 新增同步 DB 往返（拖慢热路径） |
| D9 | 管理界面取值 | 线上入参默认只读自动来源（模型成本 / 会话真实 token / 当前时刻与实际分档），What-if 才可覆盖 | 手填全部入参（易与真实口径脱节） |
| D10 | 交付顺序 | P0 影子只读 → P1 起点择优 → P2 中途切换 + 抖动抑制 → P3 闲忙时联动与可视化，逐级开关 | 一次性全量上线（风险不可控） |
| D11 | 与 Auto 关系 | Auto 不做成本重排，成本维度由本方案决策，Auto 让位 | Auto 内也做成本重排（双重择优、职责重叠） |

## Surface

**路由引擎（router/engine.go）**
- 成本比较重写：候选成本由“三档成本价 × 当前时段倍率 × 分档倍率”计算（替换现有“分档系数恒 1、仅 cost_sample 样值”的 `costScalar` / `fillCost` 口径）；样值优先取本轮真实 token 明细。
- 新增“会话成本决策”层：起点择优 + 中途门控；产出决策产物（选中候选、本轮花费、一次性重灌代价、回本轮数、决策原因、是否切换）。
- `RouteSpec` 扩展以携带：会话本轮 token 三段、会话位置 k、当前时刻（默认注入 now）。旧调用（不带这些字段）退化为现状行为。

**会话注册表（router/sessions.go / session_store.go）**
- 暴露会话位置 k（累计轮数）、当前上下文规模（可在网关侧由本轮 输入+缓存读得到）、以及切换时迁移会话的 `ChannelKeyID` / `InternalModelID`（沿用 Attach / Renew 语义与并发上限校验）。
- 会话 TTL（session.expiration）与亲和判定（Hit）语义不变。

**计费（billing）**
- 复用 `ConfigProvider`（R / CnyRate / ContextTiers / TimeConfig / SampleUsage）与 `TimeCoeffConfig.CoeffFor`、`ContextTierCoeff`；不新增公式、不改 Record 语义。
- 新增只读配置：会话成本路由开关、k 经验式系数、门控阈值（走既有 sys_configs 读写与缓存失效机制，默认安全态）。

**网关（gateway/handler.go）**
- 组装 `RouteSpec`（sessionID + 本轮 token 三段 + 会话位置 k）；调用点仍为 `RouteAll`。
- 切换事件 / 相位 / 假设节省写入沿用既有 asyncwrite（如需新事件类型则新增 Kind，默认同步安全态）。

**异步落库（pkg/asyncwrite）**
- 复用现有 `Writer`（KindCallLog / KindSession 已接入）；不新增同步往返。

**控制台与前端（console / frontend）**
- 后端新增“会话成本路由”面板接口：参数（各候选渠道三档成本价 / 时段与分档倍率 / 本轮 token / 剩余轮数）、临界点数据、事件流、临界点分布、影子对照。
- 会话列表 / 详情展示决策原因（选中渠道、本轮花费、重灌代价、回本轮数）。
- 计费配置页复用现有 `billing.r` / `billing.time_config` / `billing.context_tiers` 读写；参数面板复用模型成本来源。

## Risks & Open Questions

| Risk | Mitigation |
|---|---|
| 成本口径变更影响既有路由行为 | 默认安全态=现状；影子模式先观测；开关可一键回退 |
| 时段 / 分档取值与计费不一致 | 复用同一 `TimeCoeffConfig` / `ContextTierCoeff` 与同一配置源；单测断言路由与计费取同值 |
| 中途切换丢缓存导致多付 | D6 保守计价（按最坏情况）；P1 先只做起点择优 |
| 切换抖动 | 最小剩余轮数门槛、冷却、滞回（P2 引入） |
| 剩余轮数估计偏差 | k 经验式系数可配置；影子模式校准；偏差超阈值告警 |
| 真实 token 明细缺失导致样值回退 | 回退 `cost_sample` 并在决策产物标注“样值来源” |
| 与 Auto 机制重叠 | D11：Auto 不做成本重排，成本维度由本方案决策 |
| 新增配置项被误改 | 面板默认只读来源 + 变更留痕（异步落库） |
| Open question：k 经验式的默认系数与 clamp 上下界 | 以影子数据拟合后回填，默认给保守值 |
| Open question：是否引入 provider 缓存失效信号 | 作为未来增强（前置依赖 provider 支持），当前不依赖 |