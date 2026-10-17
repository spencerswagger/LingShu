---
design_type: initiative
created_at: 2026-10-16
---

# 会话亲和与成本控制（相位感知 + 迁移感知的成本路由）

> Status: DRAFT
> Branch: (未建)
> Author: agent
> Last updated: 2026-10-16

## Intent Contract

```
intent: 把路由从「静态样本最便宜 + 老会话无脑粘性」升级为「相位感知 + 迁移感知」的成本路由，
        使同一对外模型下的请求在渠道售价结构（输入/缓存读/输出）与各渠道闲忙时差异下，
        逼近「每个请求都落在当时最优渠道」的成本下界，同时不破坏会话连续性与并发约束。
constraints:
  - 不改五段计费口径与 usage 归一化（只改路由决策输入，不动 billing 公式）。
  - 同一请求内所有候选必须使用同一成本基准（否则候选成本不可横向比较）。
  - 不引入跨厂商质量/降智判断；质量类差异显式排除。
  - 会话连续性与并发上限不得被迁移决策破坏（迁移失败必须可回滚）。
  - v1 不做跨协议转换、不做全局 bin-packing、不做延迟不敏感流量排队调度。
success_criteria:
  - 路由决策可获取会话相位（轮数 / 上下文 token / 已缓存前缀估算），并真实参与决策。
  - 同一对外模型下，短会话与长会话能按售价结构选到不同渠道（可被单测与决策轨迹证伪）。
  - 闲忙时切换时，按 migration_penalty 公式决定是否迁移，且决策轨迹可回放（含成本/时段系数/迁移代价/是否迁移/原因）。
  - 影子对照实验（不改线上，仅并行计算）在样本流量上显示加权单请求成本下降，达到运营在 P1 设定的目标阈值。
risk_level: high   # 触及路由决策、会话迁移、成本等核心路径
```

## Verification Contract

```
verify_steps:
  - run tests: cd backend && go test ./...
  - build: cd backend && go build ./... ; cd frontend && npm run build
  - check: 单测覆盖「样本选择」「迁移代价公式边界（缓存命中/失效、跨时区时段、0 代价）」、
           「滞回防抖」「决策轨迹字段」
  - confirm:
      1) 路由可拿到并记录相位；决策轨迹含 phase 与 migration penalty；
      2) 构造两渠道售价结构相反的场景，短会话与长会话分别命中不同渠道；
      3) 构造闲忙时切换场景，验证「切换」与「不切换」两侧盈亏平衡边界正确；
      4) 影子对照：模型集合加权单请求成本下降达阈值，且无会话连续性/并发回归。
```

## Governance Contract

```
approval_gates:
  - 配置 schema 变更（routing.cost_sample 由单值扩展为多档）需评审。
  - 迁移决策进入网关热路径需评审。
  - 渠道 cache 能力 / 缓存 TTL 元数据的字段与语义需评审。
  - 影子对照实验结论与是否开启线上迁移需人工确认。
rollback:
  - 决策函数以开关降级：关闭后回落「静态单值样本 + 现有粘性」行为（等价现状）。
  - 配置向后兼容：单值 cost_sample 仍可解析（视作 default 档）。
  - cache/相位相关为新增列/新增配置，回滚无需数据修复。
ownership: agent 实现，用户验收。
```

## Problem

现有路由（见 [engine.go](file:///workspace/backend/internal/domain/router/engine.go) 包注释与排序逻辑）以会话亲和保证长会话复用上游缓存，但存在三个成本错配：

1. **售价结构差异被抹平**：`routing.cost_sample` 是**全局单一扁平五段**（[seed.go:41](file:///workspace/backend/internal/seeding/seed.go#L41)），路由对**所有会话、所有候选**用同一个样值。而各渠道售价结构差异巨大——例如 DeepSeek 官方 `输入:缓存读:输出 ≈ 50:2:200`（缓存近乎白送、输出贵），火山引擎 `≈ 1:1:1`（缓存无折扣）。同一个样值无法区分「更适合长会话」与「更适合短会话」的渠道。

2. **闲忙时重建权衡缺失**：`channel_models.time_config`（模型级时段系数）已在路由中生效（[resolveTimeCoeff](file:///workspace/backend/internal/domain/router/engine.go#L508-L522)），但会话一旦亲和组织**无脑粘性**（[collectCandidates](file:///workspace/backend/internal/domain/router/engine.go#L322-L348) 命中即置首）。当会话跨越闲/忙时切换、或其他渠道变便宜时，系统不会重新权衡——即便重灌上下文（丢缓存）可能是划算的。

3. **相位不可得；且「会话未来长度」本质不可预判**：轮数与上下文 token 已被计算（[handler.go:343-351](file:///workspace/backend/internal/domain/gateway/handler.go#L343-L351)），但只用于会话 TTL，且在 `RouteSpec`（[handler.go:239-245](file:///workspace/backend/internal/domain/gateway/handler.go#L239-L245)）构造**之后**才算出；`RouteSpec`（[engine.go:47-L52](file:///workspace/backend/internal/domain/router/engine.go#L47-L52)）无相位字段，`Session`（[sessions.go:16-L35](file:///workspace/backend/internal/domain/router/sessions.go#L16-L35)）也不记录相位。**必须澄清**：这些量描述的是**已经发生的会话长度**（过去/现在），**无法预判会话未来是否还会继续发新请求**。因此策略不能建立在「预判长短」上，只能建立在**逐请求在线决策 + 允许中途迁移**上。

## Vision

在保证会话连续性（缓存可复用、并发不超限）的前提下，让路由成为**每请求决策函数**：

> 输入 = 会话当前相位（轮数 / ctx token / 已缓存前缀 / 空闲时长 / 当前绑定渠道）与候选集合；
> 输出 = 本轮最优候选，并给出可回放的决策轨迹。

由此，短会话走「输入/输出」便宜的渠道，长会话走「缓存读」便宜的渠道；闲忙时切换时按**迁移代价公式**权衡是否中途换渠道，把总成本压向「每请求当时最优」的下界。

## Non-goals

| 明确不做 | 原因 |
|---|---|
| 跨协议转换 | 与成本无关，另属渠道协议议题（见 [channel-protocol 设计](file:///workspace/docs/designs/2026-10-16-channel-protocol-gateway-fixes-design.md)） |
| 跨厂商质量 / 降智判断与对齐 | 用户明确排除；决策只看成本 |
| 全局会话-渠道 bin-packing 指派 | v1 逐会话贪心足够；全局最优复杂度高、收益不确定 |
| 延迟不敏感流量的闲时排队调度 | 本轮不纳入（可作为后续独立方向） |
| 修改五段计费口径 / usage 归一化 | 属 billing 契约，保持稳定 |
| 自动质量回退 | 与「不考虑质量差异」一致 |

## Stakeholders

| 角色 | 关切 |
|---|---|
| 平台成本 / 运营 | 总成本下降、闲忙时套利、成本可观测 |
| 终端用户 | 延迟与结果质量不退化（连续性不被迁移破坏） |
| 上游渠道 | 并发/限流约束被遵守（迁移目标须有余量） |

## Architecture

核心是一个**每请求决策函数**，由 6 个协作部件构成。方向 1（分层样本）与方向 2（迁移代价）是同一决策函数的两个输入维度。

```
请求 ──▶ 相位采集 ──▶ 分层样本选择(A) ──▶ 候选成本标量 ──▶ 迁移代价(B) ──▶ argmin + 护栏 ──▶ 选路
              │                                              │
        Session/SessionStore 记录相位                  cache 能力/TTL 元数据
              │                                              │
              └──────────── 决策轨迹（cost/时段/迁移代价/原因）◀┘
```

1. **相位采集（Session Phase）** — 复用已存在的 `countPromptTokens` / `countUserTurns`（[tokens.go](file:///workspace/backend/internal/domain/gateway/tokens.go)），把计算**提前至路由之前**或把结果经 `RouteSpec` 传入；并**估算已缓存前缀 `C_t`**（可用上轮 ctx token 或最近命中的 cache_read 观测）。`Session` 增相位快照字段、`sessions` 表增列（当前无相位列，[session_store.go:10](file:///workspace/backend/internal/domain/router/session_store.go#L10)）。

2. **分层成本样本（Archetype Samples，A 层）** — 把 `routing.cost_sample` 由单值扩展为**按档位的映射**（如 `default / cold / single-turn(sidecar) / short / long`）。配置 schema、`validCostSample`（[admin.go:779](file:///workspace/backend/internal/domain/console/admin.go#L779)）与前端 [BillingConfig.vue](file:///workspace/frontend/src/views/admin/BillingConfig.vue) 同步扩展，且**单值向后兼容**（视作 default 档）。选择策略：按相位选一个样本；**同一请求内只用同一个样本**，保证候选可比。

3. **候选成本标量** — 复用现有 `costScalar`（[engine.go:527-L536](file:///workspace/backend/internal/domain/router/engine.go#L527-L536)）：`sample × cost_rates × 时段系数`。不改公式。

4. **迁移代价（Migration Cost，B 层）** — 对已绑定渠道与其他候选计算：

   ```
   migration_penalty(t) = C_t · (p_in^new − p_cr^new) × time_coeff^new(t)
   ```

   语义：本该以缓存价读取的 `C_t`，迁移后需在新渠道按全额输入价重灌一次。**代价只付一次、收益每轮复利**。是否迁移由盈亏平衡决定：`R · 每轮节省 > migration_penalty`，其中 `R` 为**期望剩余轮数**（取自 `session.expiration.turn_tiers` 或几何风险模型，见 Open Questions）。
   **免代价条件**：若会话空闲时长 > 目标渠道**缓存 TTL**，缓存本就失效，`migration_penalty = 0`（白赚）。→ 依赖部件 6。

5. **决策与护栏** — 取 `argmin(本轮成本 + 摊销的迁移代价)`；护栏 = 成本阈值（节省需超过阈值）+ 每会话迁移次数上限 + 目标渠道并发余量校验 + **滞回/最小驻留时间防抖**。输出决策轨迹（cost / timeCoeff / migration penalty / 是否迁移 / 原因）。

6. **cache 能力与缓存 TTL 元数据** — 当前**全仓缺失**（无列、无配置、无透传）。需在渠道/模型层新增「是否真缓存 / 缓存 TTL / 缓存写价」元数据，用于：让 `cache_read` 名义价「名副其实」，以及支撑部件 4 的免代价判定。时段系数复用已有 `time_config`（[coefficients.go](file:///workspace/backend/internal/domain/billing/coefficients.go)），无需新增。

7. **并发与一致性修复（P0 必做）** — 迁移路径存在真实缺陷：迁移时先 `removeLocked` 旧会话、再校验新键上限，**失败不回滚**（[sessions.go:199-L205](file:///workspace/backend/internal/domain/router/sessions.go#L199-L205)）→ 目标键满时旧会话从注册表消失。迁移决策依赖迁移可靠性，此缺陷必须并入 P0 修复。

## Phase breakdown

| 阶段 | 目标 | 关键交付 | 验收信号 |
|---|---|---|---|
| **P0 使能** | 让「相位」与「缓存元数据」可用 | 相位提前/经 `RouteSpec` 传入；`Session`+`sessions` 增相位列；渠道 cache 能力/TTL 元数据；修迁移不回滚缺陷 | 路由能读到 turn/ctx/缓存 TTL；迁移失败可回滚（回归测试） |
| **P1 分层样本路由（方向 1）** | 短/长会话按售价结构分流 | `cost_sample` 多档化（配置+admin+前端，向后兼容）；按相位选样本 | 单对外模型下短会话与长会话命中不同渠道；影子对照成本下降 |
| **P2 迁移决策（方向 2）** | 闲忙时切换按公式权衡 | migration_penalty + R 估计 + 盈亏平衡 + 护栏 + 决策轨迹 | 「切/不切」边界正确；无抖动；轨迹可回放 |
| **P3 实测回馈（可选，C 层）** | 用真实用量校准样本与 R | 基于 `billing_records` 实测单轮成本/命中率校准 | 校准后影子成本进一步下降 |

每个阶段各自走「brainstorming → writing-plans」循环，本文件作为战略母文档。

## Evidence（研究结论，2026-10-17 更新）

三波 subagent 研究（代码调研 + 真实公开 trace 实证 + models.dev 真实价格建模）收敛出三条会**改变设计**的结论。以下均为一手证据，非假设。

### E1 · 剩余轮数 R 的主信号是「会话位置 k」，不是「当前轮消息角色」

对 **24 万条**真实消息（WildChat / τ-bench / ToolACE / SWE-agent / OpenHands 等 6 个数据集）做留出整段会话的预测检验（HistGradientBoosting 测试集 R²）：

| 场景 | 角色单用 | k 单用 | 二者合用 | 更强 |
|---|---|---|---|---|
| τ-bench 真实 Agent | 0.003 | **0.309** | 0.320 | **k 压倒性** |
| SWE-agent 编码 Agent | −0.019 | 0.052 | 0.064 | k |
| OpenHands | 0.052 | 0.091 | 0.142 | k（接近） |
| 通用聊天（WildChat） | −0.004 | 0.001 | 0.001 | **都无效** |
| ToolACE（半合成） | 0.208 | 0.192 | 0.464 | 角色略强（数据构造假象） |

- 真实 Agent 循环里角色**周期性重复**，`assistant_final` 并不落在末尾 → 角色几乎零位置信息。
- ToolACE 角色胜出是因为该数据把 `assistant_final` 与结束绑定（77.6% 在末尾），**不可迁移**。
- 开放聊天中两者都无预测力（会话可在任意角色结束）。
- **结论**：`k`（会话位置/剩余长度）为主信号，角色仅为弱补充项。这与本设计最初「以会话长度生存分析估计 R」一致；**「角色为主信号」应明确降级为待验证的弱特征，不作为主线**。

### E2 · 切换边界是「缓存重置点」，不是「用户消息边界」；会话中途切换仅在长 horizon 下划算

缓存主导时可推导出与上下文长度**无关**的盈亏平衡（`ρ`＝新/旧价、缓存写读价比 `β/α`）：

> **回本所需剩余请求数 `n* = (β/α)·ρ/(1−ρ)`**（Claude 系锚点 `β/α≈12.5` 时 `n*≈12.5·ρ/(1−ρ)`）

数值结论：
- 短单任务 Agent 会话（N≈10）+ 折扣 < ~40% → **中途切换必然亏损**（要开始赚钱需目标渠道降价 ≈50%）。
- 长会话（N=100）+ 折扣 ≥10% → 第 45 发前切都赚。
- **用户消息边界不清空前缀缓存**：在保留上下文的情况下于用户轮切换，仍付全额定金（实测 +19%~+40%，反例）。真正的「安全/免费」切换点是**缓存重置事件**（新会话 / 上下文压缩 / clear / TTL 过期），此时 penalty≈0，节省＝价差 `(1−ρ)`。
- 因此：**切换应门控在缓存重置点**；会话中途切换只在「剩余 horizon > n*」时允许。

### E3 · 目标函数必须同时纳入输入/缓存读/输出三档；纯「最低输入价」会选错

models.dev 真实价格（232 供应商 / 2305 个归并模型 / 855 个多渠道）：
- 同模型渠道价差 `ρ` 中位 ≈1.3，**长尾极重**（cache_read P90=15.8×，最大 550×）。
- **约 20%–25%** 的候选对存在 ≥40% 折扣。
- **关键反例**：即便在零惩罚的重置点，**单按「最低输入价」路由仅 71%–77% 真省钱**；「最低输入价渠道」与「最低缓存读价渠道」在 **23.2%** 的模型上不同源（输入价贵一点点但有真缓存定价的渠道，端到端可省数倍）。

### E4 · 运行时可达性（可行，改动小）

决策点（[handler.go L229-L245](file:///workspace/backend/internal/domain/gateway/handler.go#L229-L245)）当前三项输入全缺：本请求 token 未算、`C_t` 全仓无、上轮 usage 未落会话。最小闭环：
- 读侧：`RouteSpec` 增相位字段（[engine.go L47-L52](file:///workspace/backend/internal/domain/router/engine.go#L47-L52)）；把 `countPromptTokens` 提前至 L239 前并复用。
- 写侧：`Session` 增用量字段 + `SessionRegistry` 加持锁写方法，在 [recordBilling L1036](file:///workspace/backend/internal/domain/gateway/handler.go#L1036) 处回写上一轮 usage（含 `cache_read`）。
- 「router 不读库」用「RouteSpec 纯数据传递 + gateway 写 router 自有内存 registry」两条既有范式满足（[engine.go L138](file:///workspace/backend/internal/domain/router/engine.go#L138)）。

### E5 · 缓存重置检测与离线建表：真值已采、事件化未做

- 平台**不能**直接检测上游缓存重置；最强真值是**逐轮 `CacheRead`**（重置时跌向 0），但当前仅做聚合、未逐轮序列化。Attach 的 `created`/迁移信号在热路径被丢弃（[handler.go L459](file:///workspace/backend/internal/domain/gateway/handler.go#L459)）。
- 离线可复用 `sessionRuntimes` 的 jsonb 角色计数 SQL 骨架（[admin.go L1307-L1315](file:///workspace/backend/internal/domain/console/admin.go#L1307-L1315)），**能建近似表**，但空增量行/解析失败行不可恢复、无自然结束事件、`Kill` 硬删、四类右删失缺口 → **需新增列才可信**。

## Decisions

| # | decision | choice | rejected alternatives |
|---|---|---|---|
| D1 | 决策主线 | B（逐轮前瞻公式）为主，A（规则分层）提供样本与 R，C（实测回馈）后置为 P3 | 纯 A（无法处理临界点/闲忙时）；纯 C（冷启动、可解释性差） |
| D2 | 迁移护栏 | 成本阈值 + 迁移次数上限 + 并发余量 + 滞回防抖 | 质量/版本护栏（用户排除）；纯观测不自动切（弱化目标） |
| D3 | 成本样本形态 | `cost_sample` 单值扩展为多档映射，单值向后兼容为 default | 新增独立配置键（割裂）；硬编码多档（不可运营） |
| D4 | 相位传递方式 | 提前计算 / 经 `RouteSpec` 传入，并落 `Session`/`sessions` | 路由层自己重算（重复 IO、时机仍晚） |
| D5 | 缓存时效来源 | 新增渠道/模型级 cache 能力与 TTL 元数据 | 假设上游缓存永久有效（高估长会话红利） |
| D6 | 迁移代价模型 | `C_t·(p_in−p_cr)×时段系数`，空闲超 TTL 则记 0 | 固定代价常量（无法适配渠道差异） |
| D7 | R 的估计器（依 E1） | **会话剩余长度的生存分析（k 为主信号）**；角色仅作弱特征、默认不启用 | 角色为主信号（24 万真实消息证伪）；单一常数 |
| D8 | 切换触发边界（依 E2） | **门控在缓存重置点**（新会话 / 上下文压缩 / clear / TTL 空闲过期）自动切换；会话中途仅当 `剩余 horizon > n*=(β/α)·ρ/(1−ρ)` 才允许 | 用户消息边界即安全切换点（错，不清前缀缓存） |
| D9 | 路由目标函数（依 E3） | **必须同时纳入 输入/缓存读/输出 三档** | 仅按最低输入价选（23% 模型会选错） |
| D10 | 缓存重置检测（依 E5） | **逐轮序列化 `CacheRead` 作真值** + 事件化上报 Attach `created`/迁移 | 依赖上游缓存 TTL 元数据（全仓不存在） |

## Surface

**Backend**
- `router/engine.go`：`RouteSpec` 增相位字段；决策函数引入样本选择与迁移代价项；`ChannelRoute`/`RouteResult` 增迁移代价与是否迁移字段。
- `router/sessions.go` + `session_store.go`：`Session` 增相位快照；表增列；修复迁移不回滚。
- `billing`：`SampleUsage` 由单值扩展为「按档位取样本」；`cost_sample` 解析与校验扩展。
- `channel`：`channel_models` / `channels` 增 cache 能力与 TTL 元数据（列或 JSONB 字段）。
- `console/admin.go`：`validCostSample` 与写入校验扩展。

**Frontend**
- `views/admin/BillingConfig.vue`：成本样本由单值改为多档编辑。
- 渠道/模型编辑页：暴露 cache 能力与 TTL 配置。

## Risks & Open Questions

| # | 风险 / 问题 | 说明与缓解 |
|---|---|---|
| R1 | 候选可比性被破坏 | **设计约束**：同一请求内必须只用同一档样本；相位在一请求内固定 |
| R2 | `cache_read` 名义价偏离真实命中率 | 用 cache 能力/TTL 元数据 +（P3）实测命中率校准；否则长会话红利被高估 |
| R3 | 分词器差异使 `C_t` 跨渠道不等价 | 迁移代价公式存在**成本精度**偏差；记录为已知误差，必要时按渠道 tokenizer 归一（非质量问题） |
| R4 | 期望剩余轮数 R 不可知 | **已收敛（见 E1/D7）**：以会话剩余长度生存分析为主（k 为主信号，τ-bench R²=0.309）；角色仅弱特征。仍需 P3 用 `billing_records` 实测校准 |
| R5 | 迁移抖动（闲忙时反复横跳） | 滞回 + 最小驻留时间 + 每会话迁移次数上限 + **仅缓存重置点/长 horizon 才切（D8）** |
| R6 | 会话 TTL > 上游缓存 TTL | 粘性实际无红利；应由 cache TTL 元数据驱动会话亲和「可释放」判定 |
| R7 | 迁移失败会话丢失（现存缺陷） | P0 必修（[sessions.go:199-L205](file:///workspace/backend/internal/domain/router/sessions.go#L199-L205)） |
| R8 | 会话中途切换多数场景亏损（见 E2） | **设计约束**：切换门控在缓存重置点；中途切换仅当剩余 horizon > n*；否则不切 |
| R9 | 纯最低输入价选错渠道（见 E3） | **设计约束**：目标函数同时含输入/缓存读/输出三档（D9） |
| Open Q1 | R 的估计器选型 | **已决策（D7）**：会话剩余长度生存分析（k 为主）为主；角色弱特征默认关闭；`turn_tiers` 仅作冷启动先验 |
| Open Q2 | cache 元数据落 `channels` 还是 `channel_models` | 上游缓存多按模型/端点，倾向模型级，待 P0 决策 |
| Open Q3 | 多密钥/多模型同一渠道下的迁移粒度 | 现为 (密钥,内部模型) 粒度，是否需渠道级滞回，待 P2 决策 |
| Open Q4 | 缓存重置事件如何落地检测（见 E5） | 首选**逐轮序列化 `CacheRead`**（跌向 0 即重置）+ 事件化上报 Attach `created`/迁移；无需上游 TTL 元数据。待 P0 决策 |
| Open Q5 | 场景分层的粒度 | 编码 Agent（缓存重、长会话）与聊天（无强信号）收益差异大，是否按应用/模型分层待 P3 决策 |
