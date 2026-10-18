---
intent: 让「状态」页表格按勾选维度裁剪列、列宽随内容收敛、去掉无用列、把四组限流合并为一列；详情弹窗改为四列字段对比表并大幅精简，使主体一屏可读。
success_criteria: 未勾选维度对应列消失且默认「对外模型+渠道」；表格无成本列、四组 RPM/TPM 合并为一列；列宽自适应；渠道列无派生状态灯；详情弹窗无 ID 行、字段全中文、四列对比表、操作按钮就近、顶部「详情/事件链」tab、候选显示名称；vue-tsc 构建零类型错误。
risk_level: low
auto_approve: true
---

## Steps

- [ ] **Step 1: 表格维度↔列联动与默认维度**
action: 在 frontend/src/views/admin/ServiceStatus.vue 中，将默认 `dims` 改为 `['external', 'channel']`；把常量 `COLS` 改为按 `dims` 计算的 computed（可见维度列按 external,internal,channel,key 固定顺序过滤，末尾固定追加 priority,limits,sessions），并让该 computed 成为 `spanMethod` 与 `spans` 的唯一索引真源；为四个维度列的表头/单元格加 `v-if="dims.includes('...')"`，使未勾选维度其列消失。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [ ] **Step 2: 删除成本列**
action: 在同一文件中移除「成本」列（el-table-column）及其 `mergeKeyOf` 的 cost 分支、`costGroup`/`costCell` 函数，并从 COLS 移除 cost；保留 `sampleUsageText`（候选区仍用）。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [ ] **Step 3: 合并四组 RPM/TPM 为一列**
action: 以单个「RPM / TPM」列替换原「模型 RPM / 模型 TPM / 密钥 RPM / 密钥 TPM」四列；新增一个聚合函数返回模型级与密钥级各自的 RPM 值/上限与 TPM 值/上限（复用现有 modelRpm*/modelTpm*/keyRpm*/keyTpm* 系列逻辑）；单元格内两行，各形如 `模型 0/1200 · 0/1.20M`、`密钥 0/1200 · 0/12.00M`，每行前置一个 usageLamp 用量灯；`mergeKeyOf('limits')` 返回按 (渠道|模型|密钥) 组合键（模型轴与密钥轴同时生效时退化为逐行不合并，仅模型轴按 C|M 合并，仅密钥轴按 C|K 合并，都无则按 C 合并）。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [ ] **Step 4: 列宽随内容自适应**
action: 去掉维度列与指标列上的固定 `width`，统一改用按内容估算的 `min-width`（维度列略小、合并限流列容纳两行文本）；保留 `hcell-name` 的 ellipsis 截断，使表格整体宽度收敛、减少横向滚动。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [ ] **Step 5: 序位图例与渠道列去状态灯**
action: 在「渠道」列表头加入序位图例说明（★首选 / ☆次选 / ·N第N候选 / —不参与），并给渠道列内的 rank-sym 图标加 el-tooltip 悬浮解释（文案与排名映射：rank===1→★首选，rank===2→☆次选，rank>=3→第 N 候选，'none'→不参与新会话）；移除渠道列中的派生状态灯 `lamp` 与其 tooltip（渠道无独立状态机）。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [ ] **Step 6: 详情弹窗顶部 tab**
action: 在详情弹窗内引入顶层 tab（`activeTopTab`，取值 detail/event），「详情」承载原详情内容、「事件链」承载原有三个子 tab（模型事件/探测日志/密钥事件，`activeTab` 语义不变）；打开弹窗时默认停在「详情」，事件链仍在选中行变化时按 `ensureTab` 去重加载。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [ ] **Step 7: 详情改为四列字段对比表并精简**
action: 在「详情」tab 内，将原 2x2 分区改为一个四列对比表（列=对外模型 | 内部模型 | 渠道 | 密钥，行=字段）；删除全部原始 ID 行（对外模型 ID、ModelRowID、渠道 ID、密钥 KeyID），对外模型列保留「名称/启用态」，渠道列去掉「ID / Name」中的 ID 并删除「状态」行；字段名全部中文化：InternalModelID→内部模型标识、State→状态、LastProbe→最近探测、LastErr→最近错误、Protocol→协议、BaseURL→接口地址、Priority→优先级、CredentialTail→凭据尾号、Reliability→可靠性阈值、RateLimit→限流配置、DrainReasons→排空原因；行建议为 名称 / 状态 / 协议·地址 / 优先级 / 限流(RPM/TPM) / 会话数 / 可靠性阈值 / 窗口指标 / 最近探测 / 最近错误，不适用处填「—」。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [ ] **Step 8: 快速恢复拆分进对应列**
action: 删除独立的「快速恢复」区块；将「重新探测该模型」与「强制状态·内部模型（正常/排空/禁用）」按钮移入对比表「内部模型」列下方的操作行（复用 doProbeModel / doState('model', ...)），将「重新探测该渠道密钥」与「强制状态·密钥」按钮移入「密钥」列下方操作行（复用 doProbeKey / doState('key', ...)）；保留 opErr 的 ErrorBubble 展示；对外模型与渠道列无操作按钮。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [ ] **Step 9: 候选序位显示名称**
action: 为「候选序位」区新增渠道名与密钥名查表（由 view.Channels 构建 channelID→Name、以及 (channelID,keyID)→密钥名/凭据尾号 的映射）；候选行文本由「渠道 <ID> · 密钥 <ID>」改为「渠道名 · 密钥名（尾号）」，查不到时回退显示原 ID；候选区标题中的对外模型 ID 改为对外模型名称。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [ ] **Step 10: 清理与整包构建校验**
action: 删除因上述改动而不再被引用的变量与函数（例如不再使用的成本相关、原四列限流各自的模板等），确保无未使用告警；执行一次完整前端构建。
loop: until build passes
max_iterations: 3
verify: (cd frontend && npm run build)
