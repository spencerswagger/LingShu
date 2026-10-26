---
intent: 账单详情页支持「请求定位」——把一次请求当初发了什么参数、走了哪条路由、上游回了什么、用量多少、为什么失败全部摊开；会话详情复用同一详情视图；列表侧支持按 request_id 定位并展示「一次请求一行」；列表 Total 文案跟随后端语义改为「请求数」；详情与列表在手机宽度下可读可用。
success_criteria: 详情页展示完整增量消息（含 system 与历史上下文，长消息折叠但不截断）、req_params 三组全字段（omitted:NB / dropped / over_budget 有明确标记）、决策轨迹（候选/跳过/重试/最终选择）、上游返回、用量四段（缺失显示「未解析」而非 0）、错误信息含 request_id 且可一键复制；会话详情改用同一组件（不再复制模板）；列表可按 request_id 搜索、归并主行显示「N 次尝试」且可展开子记录、分页文案为「共 N 个请求」；≤768px 无横向溢出、表格转卡片、正文字号 ≥14px；vue-tsc 零错、vite build 通过。
risk_level: medium
auto_approve: false
---

## 背景

后端（PR #63）已交付：`call_logs.req_params` 入站参数快照（≤8KB、三级裁剪）、`req_messages` 完整化、网关错误体顶层 `request_id`、账单列表按 `request_id` 搜索并按请求归并（`Attempts` 标记、`Group=0` 展开）、开发者端 DTO 层脱敏。前端需要把这些能力做成「可定位一次请求」的界面，且不假设开发者端能拿到敏感字段。

## Steps

- [x] **Step 1: req_params 渲染层（新增）**
action: 新增 `src/utils/reqParams.ts` 把后端快照规整为视图（兼容 snake/camel），`omitted:NB` 识别为「值已省略」、`dropped` 补成占位行、`over_budget` 单独警示；新增 `src/components/ReqParamsView.vue` 按「请求 / 上游 / 鉴权」三组渲染，三态各有视觉标记，长值折行不撑破。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [x] **Step 2: 用量四段解析（新增）**
action: 新增 `src/utils/usage.ts`：按「键名别名 + 任意层级」从 `upstream_usage` 取 input/output/cache_read/cache_creation（覆盖 OpenAI Chat/Responses 与 Anthropic 三种 naming），取不到留 `undefined`，展示为「未解析」——**不补 0**。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [x] **Step 3: 调用详情唯一视图**
action: `CallLogPanel.vue` 升级为唯一详情视图：头部常显请求 ID（一键复制 + 「在账单中定位」），区块为 错误信息 / 请求参数快照 / 请求增量（带 role 计数）/ 响应·上游返回 / 用量 / 决策轨迹（候选数、路由排除数、进入上游次数、重试次数、上游报错数、最终选择）；`variant='collapse' | 'timeline'` 只切换容器形态。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [x] **Step 4: 会话详情复用同一视图**
action: `SessionDetail.vue` 删除自己那套消息/决策轨迹模板，改为逐次调用渲染 `<CallLogPanel variant="timeline">`；渠道显示名两端可见，密钥/内部模型仅管理端。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [x] **Step 5: 列表侧 request_id 搜索 + 归并展开 + Total 文案**
action: `BillingQuery.vue` 加 request_id 搜索框（回车/「定位」生效，支持 `?request_id=` 深链）、主行显示「N 次尝试」、`type="expand"` 展开时按 `RequestID + Group=0` 懒加载子记录；分页 `total` 布局改为自定义「共 N 个请求」。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [x] **Step 6: 窄屏适配**
action: 新增 `composables/useIsNarrow.ts`（matchMedia）；`BillingQuery` 表格↔卡片、`BillingDetail`/`SessionDetail` descriptions 双列↔单列、决策轨迹表格↔卡片、req_params 双列↔单列；≤768px 正文 ≥14px，长 URL/JSON `break-all`，页面无横向滚动。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

- [x] **Step 7: 开发者端口径防守**
action: `showInternal=false` 时不渲染上游 URL（即使后端漏脱敏）、不渲染密钥/内部模型/成本列；列表 self 模式隐藏「用户/密钥」列与密钥维度筛选；`http.ts` 兼容网关错误体的小写 `request_id`。
loop: false
verify: (cd frontend && npx vue-tsc --noEmit)

## 手工验证（构造各状态）

1. 正常快照：任一次带采样参数的请求 → 详情页「请求参数快照」应显示端点数、模型、流式、消息条数、tools 摘要（仅名称）与各参数原值。
2. 单字段省略：请求体塞超大 `response_format`（>2KB）→ 该行显示「已省略 NNNNB」并提示「键本身保留」。
3. 字段被裁：请求体塞多个大字段顶到 8KB → 被丢字段出现在「已裁剪」清单，采样参数里保留同名占位行。
4. over_budget：极端体积 → 顶部出现「参数快照超出体积预算」警示。
5. 旧数据：迁移前的老账单 → 「请求参数快照（未记录）」「未记录上游 usage」，不崩不显示 0。
6. 用量未解析：上游只回两段（如 Responses 只剩 input/output）→ 缓存写显示「未解析」。
7. 归并展开：构造一次请求多候选失败（多条账单同一 request_id）→ 列表该行「N 次尝试」→ 展开出子记录。
8. request_id 定位：把网关错误体顶层 `request_id` 贴进列表搜索框 → 只剩该请求；或从详情/会话点「在账单中定位」。
9. 窄屏：DevTools 390px 宽 → 列表变卡片、无横向滚动、正文 ≥14px、长 JSON 折行。
