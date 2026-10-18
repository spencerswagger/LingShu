---
design_type: feature
created_at: 2026-10-19
---

# 状态页（ServiceStatus）表格与详情弹窗优化 — 设计

## Intent Contract
```
intent: 让「状态」页表格按勾选维度裁剪列、列宽随内容收敛、去掉无用列、把四组限流合并为一列；详情弹窗改为四列字段对比表并大幅精简，使主体一屏可读。
constraints:
  - 纯前端改动，不改后端契约与任何接口。
  - 维度勾选仍为「行轴 = 已勾选维度叉积」语义；至少保留一个维度（min=1 不变）。
  - 序位仍以后端 Candidates.Rank 为准，前端绝不重算。
  - 复用既有组件与配色语义（状态灯 / StatusTag / ErrorBubble）。
success_criteria:
  - 未勾选维度对应的列消失；默认勾选「对外模型 + 渠道」。
  - 表格不再出现「成本」列；四个 RPM/TPM 合并为一列。
  - 列宽随内容自适应，减少固定过宽列与横向滚动。
  - 渠道列不再显示派生状态灯；详情「渠道」区不再显示状态行。
  - 详情弹窗：无原始 ID 行、字段名全部中文、四列对比表、操作按钮就近归属维度、顶部「详情/事件链」tab、候选显示名称而非 ID。
  - vue-tsc 构建零类型错误。
risk_level: low
```

## Verification Contract
```
verify_steps:
  - run build: cd frontend && npm run build   # vue-tsc --noEmit && vite build
  - check: 手动核对勾选/取消各维度时列的增删与行轴变化；详情弹窗各区块内容与一屏可见性
  - confirm: 构建零类型错误；页面无控制台报错；详情弹窗主体一屏内可见
```

## Governance Contract
```
approval_gates: 设计稿确认（本步）；实现完成后的构建结果确认
rollback: git 回退 frontend/src/views/admin/ServiceStatus.vue 单文件改动
ownership: 前端维护者
```

## Scope
| 范围 | 内容 |
| --- | --- |
| In | 表格：维度↔列联动、默认维度、列宽自适应、序位图例、删成本列、合并限流列、删渠道状态灯 |
| In | 详情弹窗：四列对比表、删 ID、译字段名、删渠道状态行、操作就近、顶部 tab、候选显示名称 |
| Out | 后端接口 / 数据结构改动 |
| Out | 事件链的数据与端点（仅移动展示位置） |
| Out | 其它页面的任何改动 |

## Decisions
**表格**

| # | 决策点 | 选择 | 放弃的备选 |
| --- | --- | --- | --- |
| 1 | 维度与列 | 未勾选维度其列消失；行轴 = 勾选维度叉积；默认 对外模型+渠道 | 折叠为摘要列（现状） |
| 2 | 列宽 | 维度列/指标列改用内容自适应 min-width，去掉固定宽 | 固定像素宽 |
| 3 | 序位图标 | 保留 ★/☆/·N，加表头图例与图标悬浮说明 | 删除 / 改纯文字 |
| 4 | 成本列 | 删除（含相关计算与表头说明） | 保留 |
| 5 | RPM/TPM 列 | 四列合一列，单元格内两行（模型/密钥），各显示 值/上限 + 用量灯 | 保留四列 |
| 6 | 指标列可见性 | 优先级/合并限流列/会话数始终显示（按当前行轴聚合） | 随维度联动显隐 |

**详情弹窗**

| # | 决策点 | 选择 | 放弃的备选 |
| --- | --- | --- | --- |
| 7 | 整体布局 | 四列字段对比表（列=对外模型｜内部模型｜渠道｜密钥，左→右即链路顺序） | 2x2 分区 / 纵向流式 |
| 8 | 原始 ID | 删除所有 ID 行（对外模型 ID、ModelRowID、渠道 ID、密钥 KeyID） | 保留 |
| 9 | 字段名 | 全部中文（状态/最近探测/最近错误/凭据尾号/可靠性阈值/限流配置…） | 英文原始名 |
| 10 | 渠道状态 | 删除（渠道无独立状态机，派生值会误导为「禁用」） | 保留派生状态 |
| 11 | 快速恢复 | 拆散：重新探测/强制状态按钮并入对应「内部模型」「密钥」列 | 独立区块 |
| 12 | 事件链 | 顶部 tab「详情 / 事件链」，事件链内保留 模型事件/探测日志/密钥事件 子 tab | 底部堆叠 / 删除 |
| 13 | 候选序位 | 显示渠道名 + 密钥名（回退显示 ID） | 显示 ID |

## Surface
- 改动文件：仅 `frontend/src/views/admin/ServiceStatus.vue`。
- 复用（不改）：`StatusTag.vue`、`ErrorBubble.vue`，以及 `@/api/admin` 既有类型与端点 `getServiceStatus` / `probeChannelKey` / `probeChannelModel` / `channelModelState` / `channelKeyState` / `listModelEvents` / `listProbeLogs` / `listChannelKeyEvents`。
- 无新增接口、无存储结构改动。

## Risks & Open Questions
- 合并限流列在模型轴与密钥轴同时勾选时无纵向合并，单元格较高；符合「挤挤放」预期。
- Element Plus 表格列宽自适感受布局限制，极窄视口下仍可能横向滚动；通过减少列数缓解。
- 候选名称查找依赖当前视图返回的渠道/密钥清单；若候选项引用了不在清单中的对象，回退显示原 ID。
