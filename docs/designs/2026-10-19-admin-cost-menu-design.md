---
design_type: feature
created_at: 2026-10-19
updated_at: 2026-10-19
---

# 「成本」菜单：渠道×内部模型成本统计 + 定价曲线 + 会话成本曲线 — 设计

## Intent Contract
```
intent: 在管理端新增「成本」菜单，提供三块能力——(1) 按「渠道 × 内部模型」聚合成本、售价、缓存命中率、输入输出比与总计；(2) 选择若干内部模型绘制「每轮成本/售价随轮次变化」的定价曲线并标出交点，参数（每轮新增输入 token / 缓存命中率 / 输入输出比）可手填、默认取全平台 token 统计；(3) 选择一场会话，绘制该会话实际每轮的 token 计算出的成本/售价曲线。
constraints:
  - 纯只读：不改路由、不改计费公式、不改对外售价与响应语义；不新增同步写路径。
  - 数据源沿用既有 billing_records（三段 token、credits_consumed、cost_credits、session_id、channel_key_id、internal_model_id、call_time）与 channel_models / external_models / channel_keys。
  - 时间范围默认「全部历史」；全平台默认值只用 token 统计（命中率、输入输出比、每轮输入），不使用金额，避免被历史旧价干扰。
  - 金额单位一律「积分」，不做 R 值/汇率折算。
  - 仅管理端（/admin），开发者端不加。
  - 复用既有前端范式（VChart.vue 的 ECharts 封装、api/admin.ts、Element Plus 表格/选择器/输入框）与后端 console 只读接口风格。
success_criteria:
  - 侧边栏出现「成本」菜单，路由 /admin/cost 可达，面包屑标题为「成本」。
  - 统计表按「渠道 × 内部模型」逐行聚合：成本(积分)、售价(积分)、缓存命中率、输入输出比、请求数、三段 token（输入/缓存读/输出）；底部有总计行；可按列排序。
  - 模型曲线图：可多选内部模型；横轴 = 轮次；纵轴 = 每轮积分；成本曲线与售价曲线可切换/并存；相邻曲线交点被标出（轮次 + 积分）。
  - 三个参数输入框可改并实时重绘：每轮新增输入 token（默认 10000）、缓存命中率（默认全平台统计）、输入输出比（默认全平台统计）。
  - 会话曲线：可选择会话，绘制该会话每一轮的 成本 与 售价（积分），横轴 = 轮次。
  - 前端 vue-tsc 零类型错误、npm run build 通过；后端 go build / go vet / go test ./... 全绿。
risk_level: medium
```

## Verification Contract
```
verify_steps:
  - run backend build/tests: cd backend && go build ./... && go vet ./... && go test ./...
  - run frontend build: cd frontend && npm run build   # vue-tsc --noEmit && vite build
  - check:
      1) 统计表：造若干条 billing_records（不同渠道/内部模型/会话/时间），核对每行 成本=Σcost_credits、售价=Σcredits_consumed、命中率=cache_read/(input+cache_read)、输入输出比=input/output、以及总计行数值与手算一致。
      2) 默认值：/cost/defaults 返回的每轮输入 / 命中率 / 输入输出比，仅由 token 求和得出，与金额无关（改动金额字段不改变这三个默认值）。
      3) 曲线：选 ≥2 个内部模型，填同一组参数，核对某轮 y 值 = 该模型该轮成本（按设计口径手算一致）；两模型交点轮次与手算一致。
      4) 会话曲线：取一场多轮会话，各轮 y 值等于该轮 billing_records 的 cost_credits / credits_consumed。
      5) 只读：所有接口均为 GET；调用后 billing_records / channel_models 等无写入变化。
  - confirm:
      1) 两端构建零错误、后端测试全绿。
      2) 「成本」菜单可进入，三块可视化与手算一致；参数改动实时重绘。
```

## Governance Contract
```
approval_gates: 设计稿确认（本步）；实现完成后构建结果确认；若需新增表/索引或改动既有接口契约，需显式确认。
rollback: 前端回退 Cost.vue + router/index.ts + AdminLayout.vue + api/admin.ts；后端回退 console 新增的 cost 接口与 server.go 路由注册；不涉及数据迁移，无副作用残留。
ownership: 前端维护者 + 后端维护者
```

## Scope
| 范围 | 内容 |
| --- | --- |
| In | 后端只读接口：成本统计（渠道×内部模型 + 总计）、全平台 token 默认值、可选内部模型清单及费率、单会话逐轮成本/售价 |
| In | 前端：新增「成本」菜单与 /admin/cost 页面；统计表；模型定价曲线（多选 + 参数输入 + 交点）；会话成本曲线（会话选择） |
| In | 复用：VChart.vue（ECharts）、api/admin.ts、Element Plus 组件、既有会话列表数据 |
| Out | 路由/计费/售价/响应语义任何变更 |
| Out | 开发者端（/dev）页面 |
| Out | 新增数据库表或迁移（优先纯查询；若查询性能不足再评估索引，届时单独确认） |
| Out | 金额的 R 值/汇率折算（仅积分） |

## Decisions
| # | 决策点 | 选择 | 放弃的备选 |
| --- | --- | --- | --- |
| 1 | 数据源 | billing_records 聚合（含三段 token、cost_credits、credits_consumed） | 从 call_logs 反算（token 不在 call_logs）/ 实时重算定价 |
| 2 | 统计时间范围 | 全部历史（默认） | 近 7/30 天 |
| 3 | 默认值口径 | 只用 token 求和（命中率、输入输出比、每轮输入），与金额解耦 | 用金额反推（会被旧价干扰） |
| 4 | 金额单位 | 仅积分 | 积分 + 折算元 |
| 5 | 菜单范围 | 仅管理端 | 两端 |
| 6 | 曲线横轴 | 轮次（k=1..K） | 输入 token / 缓存命中率 |
| 7 | 曲线参数 | 三个参数在「图外」输入：每轮新增输入(默认 10000) / 缓存命中率 / 输入输出比（后两者默认为平台统计） | 把某个参数当横轴 |
| 8 | 每轮成本口径 | 缓存读随轮次增长（= 命中率 × 累计历史新增输入），每轮新增输入与输出恒定 | 全部随上下文等比放大（会退化为过原点的直线、无交点） |
| 9 | 缓存命中率定义 | cache_read ÷ (input + cache_read) | cache_read ÷ input |
| 10 | 输入输出比定义 | 输入 ÷ 输出（即每 1 单位输出对应多少输入）；输出 = 每轮新增输入 ÷ 输入输出比 | 输出 ÷ 输入 |
| 11 | 交点标注 | 相邻两条曲线在线性区间内解交点，标注「轮次 + 积分」 | 不标 / 只标最优点 |
| 12 | 会话曲线内容 | 同时画 成本 与 售价 两条（积分） | 只画其一 |

## Surface
**后端（只读接口，挂管理端）**
- 成本统计：按「渠道 × 内部模型 × 对外模型」聚合成本、售价、请求数、三段 token，并给出行级命中率/输入输出比与全局总计。数据来自 billing_records 关联 channel_keys/channels 取名。
- 全平台默认值：由历史全量 token 求和得出「每轮输入 token 均值、缓存命中率、输入输出比」，供前端作为输入框默认值。
- 可选内部模型清单：返回每个「渠道 × 内部模型」的成本费率与其对外模型售价费率（含时段/分档配置的只读值），供曲线计算。
- 会话逐轮成本：按 session_id 取该会话每一轮的轮次序号、三段 token、成本、售价、渠道/内部模型、时间。

**前端**
- 路由新增 /admin/cost（仅 ADMIN）；AdminLayout 菜单新增「成本」（图标沿用 Element Plus 既有图标集）。
- 新页面 Cost.vue：统计表（渠道×内部模型 + 总计）、模型定价曲线（多选内部模型 + 三个参数输入 + 交点标注）、会话成本曲线（会话选择 + 逐轮曲线）。
- 复用 VChart.vue 承载 ECharts；复用 api/admin.ts 的请求封装与既有会话列表接口取会话候选。

**数据/存储**
- 不改表结构。统计与逐轮查询均走 billing_records 既有索引（call_time、channel_key_id、session_id）。若「全部历史 × 全量聚合」在数据量下的性能不足，再单独评估物化/索引方案（本轮不做）。

## Risks & Open Questions
- 数据量：全部历史的聚合查询在流水很大时可能偏慢；本轮先用分页/聚合下推控制，必要时再评估索引或物化视图（需单独确认）。
- 口径可见性：每轮成本是「按当前费率对历史 token 重算」还是「历史实际 cost_credits」（含当时时段/分档系数）可能不同；本设计**逐轮会话曲线用历史实际 cost_credits/credits_consumed**，**模型曲线用当前费率推算**——两者含义不同，需在 UI 上明确标注，避免误读。
- 「输入输出比 = 输入÷输出」的方向为默认约定；若与业务习惯相反，改一个除法方向即可，需确认。
- 模型曲线默认只含线性三档（分档/时段系数取当前瞬时值或恒 1）；若所选模型的对外模型配置了 context_tiers/time_config，曲线是否叠加其跳变属于增强项，本轮先按当前瞬时系数处理。
- Open question：模型曲线默认轮次上限 K（如 30）与是否允许用户改 K。
- Open question：统计表是否需要「导出 CSV」；本轮先不做。