---
intent: 精简「状态」页表格与详情弹窗（列宽自适应、去成本列、四组限流合一列、详情改四列字段对比表）；顺带回填渠道/内部模型最近探测时间、让会话成本事件流展示渠道名、使非有限浮点经 WAL 往返不漂移，并把三态操作按钮抽为共享组件统一各渠道页面。
success_criteria: 表格无成本列、四组 RPM/TPM 合并为一列、列宽自适应、渠道列无派生状态灯；详情弹窗无 ID 行、字段全中文、四列对比表、操作按钮就近、顶部「详情/事件链」tab、候选显示名称；渠道/内部模型「最近探测」经探测后非空；事件流渠道迁移展示渠道名；非有限浮点经 WAL 编码→重放后不退化；前端 vue-tsc 零类型错误、后端 go build/go test 全绿。
risk_level: medium
auto_approve: false
---

> 2026-10-19 修订（code review 后续）：维度改「固定全量展示（取消勾选）」；新增后端与跨页面步骤（Step 11–13）与工程清理（Step 14）；`risk_level` 由 low 上调为 medium，`auto_approve` 由 true 改为 false（涉及后端契约/持久化语义与跨页面改动）。

## Steps

- [x] **Step 1: 表格维度固定全量展示与合并模型**
action: 在 frontend/src/views/admin/ServiceStatus.vue 中，将 `dims` 固定为 `['external','internal','channel','key']` 并移除工具条维度勾选组；`COLS` 固定为 `['external','internal','channel','key','priority','limits','sessions']` 作为 `spanMethod` 与 `spans` 的唯一索引真源；维度单元格改为按「合并组」预计算（`runsOf` / `runData` / `cellByRow`），合并键只含名称/ID（不含状态）以保证合并块稳定，状态以组聚合灯表达。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [x] **Step 2: 删除成本列**
action: 移除「成本」列（el-table-column）及其 `mergeKeyOf` 的 cost 分支、`costGroup`/`costCell` 函数，并从 COLS 移除 cost；保留 `sampleUsageText`（候选区仍用）。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [x] **Step 3: 合并四组 RPM/TPM 为一列**
action: 以单个「RPM / TPM」列替换原四列；`limitsByRow` 预计算模型级/密钥级各自的 值-上限；单元格内两行（模型/密钥），各前置 usageLamp 用量灯；`mergeKeyOf('limits')` 返回 (渠道|模型|密钥) 组合键。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [x] **Step 4: 列宽随内容自适应**
action: 去掉维度列与指标列上的固定 `width`，统一改用内容自适应 `min-width`；保留 `hcell-name` 的 ellipsis 截断，使表格整体宽度收敛、减少横向滚动。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [x] **Step 5: 序位改密钥列色条 + 渠道列去状态灯**
action: 移除渠道列中的派生状态灯 `lamp` 与其 tooltip（渠道无独立状态机）；序位由渠道列文字图标改为密钥列左缘色条 `rank-bar`（金=首选 / 浅灰=次选 / 深灰=不参与；跨对外模型不一致时 `rank-mixed` 中性灰），并加悬停释义；页脚说明同步更新。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [x] **Step 6: 详情弹窗顶部 tab**
action: 引入顶层 tab（`activeTopTab`，取值 detail/event），「详情」承载详情内容、「事件链」承载原有三个子 tab；打开弹窗默认停在「详情」，切到事件链或切换子 tab 时按 `ensureTab` 去重加载。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [x] **Step 7: 详情改为四列字段对比表并精简**
action: 将原 2x2 分区改为四列对比表（列=对外模型 | 内部模型 | 渠道 | 密钥）；删除全部原始 ID 行；字段名全部中文化（接口地址/路由优先级/RPM/TPM/并发会话/错误率/429 占比/P99 延迟/最近探测/最近错误）；空值统一「—」（新增 `dt()`）。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [x] **Step 8: 快速恢复拆分进对应列**
action: 删除独立「快速恢复」区块；重新探测/强制状态按钮以 `StateActions` 并入对比表「内部模型」「密钥」列；保留 opErr 的 ErrorBubble 展示。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [x] **Step 9: 候选序位显示名称**
action: 新增渠道名/密钥名查表（由 view.Channels 构建），候选行文本改为「渠道名 · 密钥名（尾号）」，查不到回退原 ID；候选区标题改用对外模型名称。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [x] **Step 10: 清理与整包构建校验**
action: 删除不再引用的变量与函数（成本相关、原四列限流模板、折叠摘要相关等）；执行完整前端构建。
loop: false
verify: (cd frontend && npm run build)

- [x] **Step 11: 后端回填渠道/内部模型最近探测**
action: 在 backend/internal/domain/channel/probe.go 新增 `markChannelProbeResult` / `markModelProbeResult`（锁内写 `lastProbe`，不改 `LastErr`），并在 `probeOnce` 中于 `markKeyProbeResult` 后调用。
loop: false
verify: (cd backend && go build ./...)

- [x] **Step 12: 会话成本事件解析渠道名**
action: 在 backend/internal/domain/console/admin.go 中将 `toSessionCostEventView` 改为方法，新增 `channelKeyName`（`GetKeyRuntime` 解析密钥所属渠道名），响应新增 `FromChannelName`/`ToChannelName`；前端 `api/admin.ts` 与 `SessionCostRouting.vue` 展示渠道名（起点显示「起点」，解析不到回退 ID）。
loop: false
verify: (cd backend && go build ./... && go test ./internal/domain/console/...)

- [x] **Step 13: 事件幂等键与 WAL 非有限浮点无损往返**
action: 在 backend/internal/domain/router/cost_event.go 中，幂等键改为 `session:turn:from>to`（避免起点与切换撞键）；`sanitizeEventForJSON` 重构为 `marshalEventForJSON`/`unmarshalEventFromJSON`，非有限浮点在 `__non_finite` 记录字段名，重放时还原为 NaN，保证 WAL 往返不把「未知」退化为 0。
loop: false
verify: (cd backend && go test ./internal/domain/router/...)

- [x] **Step 14: 跨页面三态按钮组件化 + 工作区产物清理**
action: 新增共享组件 frontend/src/components/StateActions.vue（图标化三态 + 可选重新探测，当前状态置灰），并替换 ServiceStatus / ChannelEdit / ChannelList / ChannelModels 中的内联文字按钮（经 owner 授权）；移除误提交的 `.devtools/`、`.uploads/`，还原 `.trae-html-share-packages/...zip`，并把 `.devtools/`、`.uploads/` 加入 `.gitignore`。
loop: false
verify: (cd frontend && npm run build)