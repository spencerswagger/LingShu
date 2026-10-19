---
design_type: feature
created_at: 2026-10-19
updated_at: 2026-10-19
---

# 状态页（ServiceStatus）表格与详情弹窗优化 — 设计

> 2026-10-19 修订（code review 后续）：范围扩大为「表格/详情精简 + 后端探测时间回填 + 会话成本事件渠道名 + 跨页面三态按钮组件化 + 工程清理」，`risk_level` 由 `low` 上调为 `medium`。原稿中「纯前端、单文件、维度勾选联动列显隐」的表述已按实际实现更正。

## Intent Contract
```
intent: 精简「状态」页表格与详情弹窗（列宽自适应、去成本列、四组限流合一列、详情改四列字段对比表）；顺带回填渠道/内部模型的最近探测时间、让会话成本事件流展示渠道名、使非有限浮点（未知）经 WAL 往返不漂移，并把「三态操作」按钮抽为共享组件统一各渠道页面。
constraints:
  - 维度固定全量展示（取消勾选）：行轴恒为「对外模型 × 内部模型 × 渠道 × 密钥」叉积；四维度列恒定可见。
  - 序位仍以后端 Candidates.Rank 为准，前端绝不重算。
  - 复用既有组件与配色语义（状态灯 / StatusTag / ErrorBubble）；新增共享组件 StateActions。
  - WAL 线格式对非有限浮点保持无损往返（NaN 不得在重放后退化为 0）。
success_criteria:
  - 表格无「成本」列；四组 RPM/TPM 合并为一列（上行模型级、下行密钥级，各带用量灯）。
  - 列宽随内容自适应（min-width），减少横向滚动。
  - 渠道列不显示派生状态灯；详情「渠道」列无状态行。
  - 详情弹窗：无原始 ID 行、字段名全中文、四列对比表、操作按钮就近归属维度、顶部「详情/事件链」tab、候选显示渠道名/密钥名。
  - 渠道/内部模型的「最近探测」经探测后非空（后端回填）。
  - 会话成本事件流「渠道迁移」展示渠道名（起点显示「起点」，解析不到回退 ID）。
  - 非有限浮点（NaN/±Inf）经 WAL 编码→重放后仍为「未知」，不退化为 0。
  - 前端 vue-tsc 零类型错误；后端 go build / go test 全绿。
risk_level: medium
```

## Verification Contract
```
verify_steps:
  - run backend build: cd backend && go build ./...
  - run backend tests: cd backend && go test ./...
  - run frontend build: cd frontend && npm run build   # vue-tsc --noEmit && vite build
  - check: 手动核对表格列增删/行轴、详情四列对比表与事件链 tab、候选显示名称；探测后渠道/模型「最近探测」非空
  - confirm: 两端构建零错误、后端测试全绿
```

## Governance Contract
```
approval_gates: 设计稿确认（本步）；实现完成后的构建结果确认；后端契约/持久化语义改动需显式确认（2026-10-19：跨页面三态按钮改动已由 owner 授权）
rollback: git 回退本 PR 涉及文件；后端可单独回退 router/cost_event.go、channel/probe.go、console/admin.go
ownership: 前端维护者 + 后端维护者
```

## Scope
| 范围 | 内容 |
| --- | --- |
| In | 表格：维度固定全量、列宽自适应、序位色条（密钥列）、删成本列、合并限流列、删渠道状态灯、合并格组聚合状态灯 |
| In | 详情弹窗：四列对比表、删 ID、译字段名、删渠道状态行、操作就近、顶部 tab、候选显示名称 |
| In | 后端：渠道/内部模型 LastProbe 回填（probe.go）；会话成本事件解析渠道名（console/admin.go）；事件幂等键含 from>to、WAL 非有限浮点无损往返（router/cost_event.go） |
| In | 跨页面：新增共享组件 StateActions，统一 ServiceStatus / ChannelEdit / ChannelList / ChannelModels 的三态按钮（经 owner 授权） |
| In | 工程：移除误提交的 `.devtools/`、`.uploads/` 工作区产物并加入 `.gitignore`；还原 `.trae-html-share-packages/...zip` |
| Out | 其它业务接口契约（仅做向后兼容的字段新增） |
| Out | 事件链的数据与端点（仅移动展示位置） |

## Decisions

**表格**

| # | 决策点 | 选择 | 放弃的备选 |
| --- | --- | --- | --- |
| 1 | 维度与列 | 维度固定全量展示（取消勾选）：行轴 = 四维度叉积，四维度列恒定可见 | 维度↔列联动显隐（原稿）/ 折叠为摘要列（更早现状） |
| 2 | 列宽 | 维度列/指标列改用内容自适应 min-width，去掉固定宽 | 固定像素宽 |
| 3 | 序位 | 密钥列左缘色条（金=首选 / 浅灰=次选 / 深灰=不参与；跨对外模型不一致时中性灰），悬停见释义 | 渠道列 ★/☆/·N 文字图标 |
| 4 | 成本列 | 删除（含相关计算与表头说明） | 保留 |
| 5 | RPM/TPM 列 | 四列合一列，单元格内两行（模型/密钥），各显示 值/上限 + 用量灯 | 保留四列 |
| 6 | 指标列可见性 | 优先级/合并限流列/会话数始终显示 | 随维度联动显隐 |
| 7 | 合并格状态灯 | 按合并组的聚合状态着色（全正常=绿 / 部分可用=橙 / 全异常=红），异常明细入悬停 tooltip | 取最差值直接压成红色 |

**详情弹窗**

| # | 决策点 | 选择 | 放弃的备选 |
| --- | --- | --- | --- |
| 8 | 整体布局 | 四列字段对比表（列=对外模型｜内部模型｜渠道｜密钥，左→右即链路顺序） | 2x2 分区 / 纵向流式 |
| 9 | 原始 ID | 删除所有 ID 行（对外模型 ID、ModelRowID、渠道 ID、密钥 KeyID） | 保留 |
| 10 | 字段名 | 全部中文（状态/最近探测/最近错误/凭据尾号/可靠性阈值/限流配置…） | 英文原始名 |
| 11 | 渠道状态 | 删除（渠道无独立状态机，派生值会误导为「禁用」） | 保留派生状态 |
| 12 | 快速恢复 | 拆散：重新探测/强制状态图标按钮并入对应「内部模型」「密钥」列 | 独立区块 |
| 13 | 事件链 | 顶部 tab「详情 / 事件链」，事件链内保留 模型事件/探测日志/密钥事件 子 tab | 底部堆叠 / 删除 |
| 14 | 候选序位 | 显示渠道名 + 密钥名（尾号），回退显示 ID | 显示 ID |

**后端 / 跨页面**

| # | 决策点 | 选择 | 放弃的备选 |
| --- | --- | --- | --- |
| 15 | 模型/渠道 LastProbe | 探测后回填 lastProbe（不改 LastErr，避免引入瞬时 429 噪音） | 保持恒空 |
| 16 | 事件流渠道迁移 | 后端用 GetKeyRuntime 把密钥 ID 解析为所属渠道名，前端展示名称 | 前端透出裸 ID |
| 17 | 事件幂等键 | `session:Seq`（`Seq` 由 sink 在提交时分配、每次发射单调唯一；网关 `SessionTurn` 恒 0 无法区分轮次） | `session:turn` / `session:turn:from>to`（同轮次同转移会被误去重） |
| 18 | WAL 非有限浮点 | 编码时归零并在 `__non_finite` 记录字段名，重放时还原为 NaN（无损往返） | 直接归零（重放后退化为 0） |
| 19 | 三态操作按钮 | 抽为共享组件 `StateActions`（图标化、当前状态置灰），各渠道页复用 | 各页面各自内联文字按钮 |

## Surface
- 前端：`ServiceStatus.vue`、`components/StateActions.vue`（新增）、`components/ChannelModels.vue`、`views/admin/ChannelEdit.vue`、`views/admin/ChannelList.vue`、`views/admin/SessionCostRouting.vue`、`api/admin.ts`。
- 后端：`backend/internal/domain/channel/probe.go`、`backend/internal/domain/console/admin.go`、`backend/internal/domain/router/cost_event.go`。
- 工程：`.gitignore`；移除 `.devtools/`、`.uploads/`；还原 `.trae-html-share-packages/session-cost-routing/session-cost-routing.html.zip`。
- 复用（不改）：`StatusTag.vue`、`ErrorBubble.vue`，以及 `@/api/admin` 既有端点。

## Risks & Open Questions
- 合并限流列在模型与密钥同时存在时无纵向合并，单元格较高；符合「挤挤放」预期。
- Element Plus 表格列宽自适感受布局限制，极窄视口下仍可能横向滚动；通过减少列数缓解。
- 候选名称查找依赖当前视图返回的渠道/密钥清单；引用不在清单中的对象时回退显示原 ID。
- 事件幂等键为 `session:Seq`：每次发射唯一，不再误去重同一轮次的多次独立决策；幂等语义由「按轮次」变为「按发射」（同一条记录重放仍幂等，`Seq` 随 Payload 落 WAL）。待网关 `SessionTurn` 接线后，如需按轮次去重可再评估。
- WAL 线格式新增 `__non_finite` 键：向下兼容（旧 payload 无该键时非有限值退化为 0，与历史行为一致）。