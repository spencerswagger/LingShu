---
design_type: initiative
created_at: 2026-10-15
---

# 渠道细分选择与成本感知路由

> 本文件为 initiative 级设计（跨多阶段）。Phase 1 / Phase 2 各自进入独立的 brainstorming → writing-plans 循环并产出 feature 级设计。

## 1. 问题（Problem）

现状有三条硬约束，合起来造成了一个能力缺口：**同一逻辑模型无法按"场景维度"被路由到不同的渠道子集**。

1. **细分只能挂在 key 上。** `tokens.tag_id` 是单标签外键，一个 key 只有一套策略；令牌标签与渠道标签的匹配（`tag.StrictContains`）是当前唯一的细分手段。
2. **请求级的选择信号被工具限制。** 多数 AI 工具无法自定义请求头，切换 key 也很繁琐。因此唯一"好用且请求级"的信号是**模型名**——工具切换模型是天然且低成本的。
3. **成本优化是静态的。** 候选比较用的是全局固定样值 `routing.cost_sample`；会话命中时 `router.collectCandidates` 无条件把已绑定的 `(key, model)` 顶到首位，会话一旦建立便**死粘首渠道，永不迁移**。

由此缺口：

- 无法让"同一把 key、跨项目/跨场景"使用不同的渠道子集（例如项目 A 能走国外渠道、项目 B 不能）。
- 无法让平台利用"某些渠道短会话便宜、某些渠道长会话便宜"的差异自动择优。

## 2. 目标（Vision）

建立**统一的"渠道细分选择"抽象**，让"选哪些渠道"这件事可以在两个层面被表达，并复用现有标签机制，不引入并行体系：

- **用户/管理员选（静态）**：用**模型变体**承载渠道细分。同一逻辑模型对外暴露多个变体（如 `A-cn` / `A-us` / `A-cheap`），用户切换模型即切换渠道子集。region 只是其中一个例子，任意维度组合（region / vendor / tier / 成本形状…）都走同一机制。
- **平台自动选（动态）**：在会话内按**成本**（并感知会话形状）自动在**已允许的**渠道子集里择优，并允许**受控迁移**（短会话 → 长会话渠道）。

一句话分工：**用户用模型名定义"允许用哪些渠道"；平台在允许集内做"用哪个最优"**。

## 3. 非目标（Non-goals）

- 不做请求头 / 自定义元数据方案（工具不支持，工程上不成立）。
- 不强制"按项目拆 key"（运维成本高，与"同 key 跨场景"诉求相悖）。
- 不把"平台内部路由属性"（如"长/短会话渠道"）变成用户必须声明的标签——这类属性应由成本模型自己算出。
- 本期不做"预测请求的 region / tier / vendor"（承认不现实，只做成本维度）。
- 不承诺跨渠道迁移时上游 prompt cache 的完全无损（只做代价估算与迁移抑制）。

## 4. 干系人（Stakeholders）

| 角色 | 关心什么 | 本设计如何影响 |
|---|---|---|
| 开发者（用 key + 工具） | 选择渠道的便利性 | 切模型名即切渠道；无需改头/换 key |
| 平台管理员 | 渠道画像、变体、成本策略的可维护性 | 新增"变体定义"与"画像约定"，避免组合爆炸 |
| 计费 / 财务 | 分账与定价 | 变体是独立对外模型，天然分账、可分别定价 |
| 平台自身 | 采购成本与毛利 | 动态迁移降低渠道采购成本 |

## 5. 核心抽象与架构（Architecture）

### 5.1 渠道画像（Channel Profile）

渠道用**语义标签**声明自身属性：现有 `semantic_tags.kv_pairs` + `channel_tags`（渠道多标签、多维、可软删）。约定画像维度使用小写枚举值，如 `{region:cn, vendor:openai, tier:gold, cost_shape:cache_heavy}`。

**原则**：画像描述"渠道真实是什么"，不描述"用户想要什么"。

### 5.2 选择器（Selector）

两类选择器，共用同一套匹配语义：

- **变体选择器（用户选）**：一个对外模型别名 + 一组"要求的画像 KV"。建议在 `external_models` 新增字段 `route_tags`。
- **成本选择器（平台选）**：运行时成本策略，在会话内决定是否迁移渠道。

### 5.3 匹配语义

- **有效要求 = 令牌标签 ∪ 变体 `route_tags`**（合取：两者的每个 KV 都必须满足）。
- **渠道命中 ⇔ 存在该渠道的某个绑定标签 ⊇ 有效要求**（复用 `tag.StrictContains` 的"令牌 ⊆ 渠道"方向）。
- **有效要求为空 → 走默认路由**（现状：不过滤）。
- 由此，令牌标签与变体筛选是**叠加收窄**关系，不是替代关系。

### 5.4 计费与分账

变体是独立的 `external_model_name`（`billing_records.external_model_name` 已是计费维度），因此 region / 变体天然分账，sale 模式可对同模型的国内/国外分别定价。

### 5.5 数据模型改动（概要）

- `external_models` 新增：
  - `route_tags JSONB`（该变体要求的渠道画像 KV）。
  - 可选 `group_key`（控制台把变体归为"一个逻辑模型家族"）。
  - 可选 `variant_of`（指向基础模型，用于**继承 channel_models 绑定**，避免为每个变体重复维护绑定行）。
- 渠道侧：沿用 `semantic_tags` / `channel_tags`，不改。
- 备选（见第 7 节决策 D1）：渠道结构化属性列。

### 5.6 关键流程（概要）

```
请求(model=变体名)
  → 解析 external_model，取 route_tags
  → 有效要求 = token_tag_KV ∪ route_tags
  → 候选 = channel_models(绑定/继承) ∩ 渠道画像筛选(StrictContains)
  → 会话亲和 / 迁移策略(Phase 2)
  → 成本排序(Phase 2 会话形状感知)
  → 计费按变体名分账
```

## 6. 阶段拆解（Phase breakdown）

### Phase 0 — 底座：画像与选择器抽象（无行为变化）

- 明确画像维度命名规范；验证"变体 `route_tags` 合并进路由筛选"不破坏现有行为（现有无标签令牌 → 有效要求仍为空 → 默认路由）。
- **出口**：仅新增可选字段，默认值等价于现状；补兼容性回归测试。

### Phase 1 — 能力 A：模型变体（静态细分，用户选）

- `external_models` 增 `route_tags`；控制台支持"一个逻辑模型 → 多个变体"；路由合并筛选。
- 覆盖场景：region 拆分、vendor 拆分、tier 拆分、以及任意维度组合。
- **出口**：用户切模型即切渠道；现有无标签令牌行为完全不变；变体可分别定价。

### Phase 2 — 能力 B：成本感知的会话内迁移（动态细分，平台选）

- 会话形状感知的成本样值（基于 `countUserTurns` 与 prompt token 估算）。
- 路由迁移决策：会话命中时判断"续留 vs 迁移"，不再无条件死粘。
- 迁移惩罚估算（缓存重算代价）+ 滞后（切换后粘定，抑制 flap）。
- **出口**：`cost` 模式用户账单下降；`sale` 模式平台毛利提升；无抖动、无缓存反复失效。

## 7. 决策（Decisions）

| # | 决策点 | 选择 | 备选 / 否决理由 |
|---|---|---|---|
| D1 | 渠道画像承载方式 | 复用语义标签 KV（`semantic_tags` + `channel_tags`） | 结构化属性列：类型安全但改 schema 且与标签机制并行；否决 |
| D2 | 变体的渠道绑定 | `variant_of` 继承基础模型的 `channel_models` 绑定 + `route_tags` 收窄 | 每变体独立绑定：组合爆炸；纯命名别名：无法按画像自动收窄 |
| D3 | 选择信号载体 | 模型名（`external_model_name`） | 请求头/自定义元数据、切 key：均被工程现实否决 |
| D4 | 动态迁移的优化目标 | 统一最小化渠道 `cost_rates` 采购成本 | 按计价模式分别预算：计算其实一致（见 D5） |
| D5 | 计价模式差异 | 两者都用 `cost_rates` 决策，差异仅在**对外叙事**（sale=平台毛利、cost=用户账单） | 用 `sale_rates` 决策：会让 sale 模式误判采购成本 |
| D6 | 迁移触发度量 | 会话形状（轮次 + 上下文规模） | 仅按轮次：粗但简单；纯解析曲线：更准但工程量大。建议融合 |
| D7 | 迁移抑制 | 一次性迁移 + 滞后锁定 | 无抑制：交叉点附近 flap，每次 flap 付缓存重算代价 |

## 8. 风险与未决问题（Risks & Open Questions）

| # | 风险 / 问题 | 影响 | 缓解方向 |
|---|---|---|---|
| R1 | **迁移丢上游 prompt cache**：长会话缓存占比高，迁移后 `cache_read` 变 `cache_write`，一次性损失可能吃掉节省 | 高（可致净亏） | 迁移惩罚计入成本曲线；仅当净收益超阈值才迁；抑制迁移频率 |
| R2 | 变体 alias 膨胀（N 维度 × M 模型） | 中 | `group_key` 分组治理 + 命名规范；只对"真有差异"的维度建变体 |
| R3 | 会话指纹含模型名（`SessionID(user,token,model,raw)`），不同变体 = 不同会话 | 中 | 属预期行为，但需确认"用户中途换变体"的预期（新会话 vs 续会话） |
| R4 | 成本曲线的分档阶跃（`context_tiers`）与逐渠道时段系数（模型级 `time_config`）使交点非单值 | 中 | 曲线按分档分段求解；时段系数纳入求值 |
| R5 | 动态迁移的"谁受益"对外口径 | 中 | 平台 vs 用户披露分开；cost 模式可宣传账单下降，sale 模式不宣传 |
| OQ1 | Phase 2 的触发度量最终取"轮次桶"还是"解析曲线"？ | — | 留待 Phase 2 设计细化 |
| OQ2 | 变体的 `route_tags` 是否允许随时间/时段变化？ | — | 暂否，保持静态可预测 |

## 9. HOTL 契约（概要）

> 以下为 initiative 级概要；Phase 1 / 2 各自产出完整契约。

### Intent Contract

```
intent: 用模型变体承载请求级的渠道细分选择，并使平台能在会话内按成本自动迁移渠道。
constraints:
  - 不破坏现有无标签令牌的默认路由行为
  - 不改动请求头/客户端协议
  - 复用现有语义标签机器，不引入并行细分体系
success_criteria:
  - 用户切换模型名即可把请求定向到指定渠道子集
  - cost 模式用户账单可见下降；sale 模式平台采购成本下降
  - 不引入会话抖动与缓存反复失效
risk_level: high   # 触及计费口径（迁移改变 cost 模式用户实付渠道 → 影响账单），按 HOTL 规则计费类始终人工把关
```

### Verification Contract

```
verify_steps:
  - run tests: go test ./... （含 tag/router/channel/model/billing 各域）
  - check: 现有路由/计费回归用例全绿；新增变体筛选与迁移决策用例
  - confirm: 无标签令牌行为不变；变体筛选按预期命中；迁移按阈值触发且滞后生效
```

### Governance Contract

```
approval_gates:
  - Phase 0 抽象定稿后（架构评审）
  - Phase 1 数据模型变更后（schema 评审）
  - Phase 2 迁移策略定稿后（涉及计费口径，需人工确认）
rollback:
  - Phase 1：route_tags 默认为空 = 现状，可回退
  - Phase 2：迁移策略开关可关闭，回退为死粘首渠道
ownership: 待指定（平台 / 网关负责人）
```

## 10. 参考（现状锚点）

- 标签匹配：`backend/internal/domain/tag/match.go`（`StrictContains`）
- 令牌标签解析：`backend/cmd/server/main.go`（`tagKVResolver`）
- 路由候选与会话亲和：`backend/internal/domain/router/engine.go`（`collectCandidates`、`orderCandidates`）
- 会话迁移能力：`backend/internal/domain/router/sessions.go`（`Attach` 支持密钥迁移）
- 轮次/上下文可算：`backend/internal/domain/gateway/tokens.go`（`countUserTurns`）、`backend/internal/domain/gateway/handler.go`（session TTL 计算）
- 计费口径：`backend/internal/domain/billing/engine.go`（`ComputeCredits` / `RawTotal`）、`backend/internal/domain/gateway/handler.go`（sale vs cost 取价）
- 数据模型：`backend/internal/db/migrations/0001_init.sql`（`semantic_tags` / `channel_tags` / `tokens` / `external_models` / `channel_models`）
