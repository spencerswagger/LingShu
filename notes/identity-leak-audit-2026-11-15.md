# 阶段一 · 客户端可见面身份泄漏排查（前端界面层）

排查对象：`frontend/src`（82 个源文件）+ `frontend/index.html` + `frontend/nginx.conf`
基线：`origin/main`（`3720ed7`）
关键词：`magpie`、上游 provider 名（openai/anthropic/deepseek/… 共 22 个品牌词）、渠道名、渠道密钥名、上游实际模型名、`sk-` 长串密钥。

## 0. 判定分层（先说清楚什么算「客户端可见面」）

| 层 | 范围 | 判定 |
|---|---|---|
| L1 客户端可见 | `/dev/*`（DEVELOPER 角色）+ `/login`、`/register` 公开页 | **硬要求**：不得出现上游 provider / 渠道名 / 密钥名 / 上游实际模型名 |
| L2 管理端内部面 | `/admin/*`（ADMIN 角色） | 运营自己就是渠道与凭据的管理者，渠道名/密钥尾号/上游地址属正常职能信息；**只登记，不按泄漏处理** |
| L3 中性词 | 「上游」「渠道」「供应商」等泛词 | 不是具体身份，不计泄漏 |

> 说明：`/dev` 复用了 5 个管理端页面（账单/会话/模型/Auto），判定按 `mode='self'` 分支走，不看目录名。

## 1. 清单表

### 1.1 L1 客户端可见面

| # | 位置（文件:行） | 当前文案 / 字段 | 类别 | 风险 | 处置 |
|---|---|---|---|---|---|
| 1 | `views/StatsDashboard.vue:1300-1310`（admin 与 dev 共用） | 「Top 渠道密钥」卡片 + 折叠标题里的该词 | 页面渲染 | P0 | **已改**：该卡片仅管理端渲染，折叠标题按角色裁剪（dev 端不再出现该区块） |
| 2 | 同上（数据侧） | `/dev/stats/dashboard` → `Usage.ByKey[].Label/SubLabel` = **渠道密钥名 + 渠道名** | 接口返回 | P0 | **需后端**：dev 作用域把 `ByKey` 置空（见 §2-D） |
| 3 | `views/admin/SessionDetail.vue:154` | `渠道 {{ callChannel(call).channel }}`（`isSelf` 分支，逐次调用显示渠道名） | 页面渲染 | P0 | **待裁定**（2026-10-26 已定「开发者端保留 channel_name 显示名」，与本次硬要求冲突） |
| 4 | `components/CallLogPanel.vue:398 / 449 / 495` | 「最终选择 · 渠道名」、决策轨迹「渠道」列、窄屏卡片「渠道 xxx」——`showInternal=false`（dev）时仍渲染 | 页面渲染 | P0 | **待裁定**（同一份 `channel_name` 白名单） |
| 5 | `views/admin/BillingQuery.vue:845 / 503` | self 模式列「渠道」；值 = 渠道名，缺失时显示「不展示」 | 页面渲染 | P1 | **待裁定**（后端 `toDevBillingListItem` 目前不填 `ChannelName`，实际恒为「不展示」；该文案本身像缺陷） |
| 6 | `api/http.ts:51-71` + `components/ErrorBubble.vue:24` | 后端统一壳 `Message` 原样进全局 toast / 错误块 | 错误回显 | P0 | **需后端**（消息里可能带上游原文，见 #7） |
| 7 | `components/CallLogPanel.vue:295` | 错误信息块 `log.ErrorMessage` 原样 = 「上游返回 429（触发限流）: {上游响应体原文}」 | 错误回显 | P0 | **需后端**（见 §2-B） |
| 8 | `views/admin/BillingQuery.vue:710 / 968` | 账单展开子行 / 卡片失败原因列 = `ErrorMessage`（同上） | 错误回显 | P0 | **需后端**（同一字段） |
| 9 | `components/CallResponseView.vue:38-47` | 「原始响应」折叠 = 上游响应体原样 JSON（含 `model`= 上游实际模型名、上游响应 id、供应商指纹字段） | 页面渲染 | P0 | **需后端 + 待裁定**（见 §2-C、§3-2） |
| 10 | `components/ReqParamsView.vue:184-190` | 「上游协议 openai-chat / anthropic-messages」+「发生过跨协议转换」标记 | 页面渲染 | P1 | **待裁定**（协议名不是 provider 名，但转换标记可反推上游协议族） |
| 11 | `components/CallLogPanel.vue:362-364` | 用量折叠标题里的 `upstreamUsage.Protocol` 标签 | 页面渲染 | P1 | **待裁定**（同上） |
| 12 | `views/Login.vue:125`、`views/Register.vue:128` | 公开页卖点「OpenAI 兼容统一网关入口」 | 纯文案 | P2 | **待裁定**（对外协议兼容声明，不是「我们上游是谁」） |
| 13 | `views/dev/TokenList.vue:179`（`views/admin/AdminTokenList.vue:186` 同句） | tooltip「复制本网关 OpenAI 兼容 API 地址（/v1）…」 | 纯文案 | P2 | **待裁定**（同上，指自家入口协议） |

### 1.2 L2 管理端内部面（登记，不按泄漏处理）

| # | 位置（文件:行） | 当前文案 / 字段 | 类别 | 风险 | 处置 |
|---|---|---|---|---|---|
| 14 | `views/admin/ChannelCreate.vue:162` | placeholder「如 主渠道-DeepSeek」 | 纯文案 | P3 | **待裁定**（管理端示例文案点名上游品牌；中性化会降低填写指引） |
| 15 | `components/ChannelModels.vue:542 / 691-692` | 「上游模型名（请求体 model 字段）」，placeholder「如 deepseek-chat」 | 纯文案 | P3 | **待裁定**（同上） |
| 16 | `components/ModelPriceCatalog.vue:147 / 155 / 168` | placeholder「如 deepseek / gpt-4o」、「同一模型在不同供应商下…」、表格列 `Provider` | 纯文案/表格 | P3 | 保留（models.dev 公共参考价，管理端定价取数用） |
| 17 | `views/admin/ChannelProtocolEdit.vue:174 / 460-462` | 「openai-compat（OpenAI 兼容）」等协议选项；特性说明提到 DeepSeek thinking | 配置项 | P3 | 保留（协议名是配置必需项，改名会打断既有配置语义） |
| 18 | `views/admin/ChannelList.vue`、`ChannelEdit.vue`、`ChannelProtocolList.vue:135` | 渠道名、密钥名、`CredentialTail` 尾号、协议 `BaseURL`、`PresetSourceID` | 配置项/表格 | P3 | 保留（管理端职能；凭据只出尾号，不出明文） |
| 19 | `views/admin/Cost.vue:1068` 等 | 「渠道」列 / `ChannelName` / 多渠道对比 / 渠道轨迹 | 表格/图表 | P3 | 保留（成本页仅管理端路由） |
| 20 | `views/admin/ServiceStatus.vue:1097` | 探测日志 `e.Error` 原样（可能含上游地址、上游报文） | 日志面板 | P3 | 保留（管理端状态页；如需脱敏应在 `probe_logs.error` 写入侧做） |
| 21 | `views/admin/BillingConfig.vue:345` | 导出文件名 `channel-protocols-<ts>.json`，内容含各协议 `BaseURL`（上游地址） | 导出 | P3 | 保留（文件名是通用词不是具体渠道/供应商名；导出物是管理员自己的配置备份） |
| 22 | `views/admin/Cost.vue:1039` | 「当前后端版本仍带只读门控」 | 纯文案 | P3 | 保留（内部实现提示，不属身份泄漏） |

### 1.3 L3 中性 / 已确认无问题

| # | 位置 | 项 | 结论 |
|---|---|---|---|
| 23 | 全仓（frontend/backend/docs） | `magpie` 字样 | **0 命中** |
| 24 | `frontend/src` | `sk-` 长串密钥 | **0 命中**；凭据明文只在 `AdminTokenList`「查看密钥」弹窗按需展示（管理端），列表恒为掩码 `TokenDisplay` |
| 25 | `frontend/src` | 上游主机名硬编码（`api.openai.com` 之类） | **0 命中** |
| 26 | `index.html` 标题、两套布局、登录/注册品牌区 | 「AI 网关控制台」「AI网关 / AI Gateway」「© 2026 AI Gateway · Internal Console」 | 中性，无需改 |
| 27 | 多处 | 泛词「上游」「渠道」「供应商」 | 中性词（非具体身份），保留 |

## 2. 需后端配合（前端已确认只是渲染，不应在前端做字符串黑名单）

| 编号 | 证据 | 问题 | 建议 |
|---|---|---|---|
| A | `backend/internal/domain/gateway/handler.go:1303`（`normalizeUpstreamBody` 只归一协议特性）+ 成功路径把上游体/转换结果原样下发 | **网关对下游响应体不重写 `model`**：客户端（含 `/v1` API 调用方）看到的是上游实际模型名 | 下发前把 `model` 改写为请求的对外模型名；`id`/指纹类字段按需归一。阶段一四协议透传会放大这条 |
| B | `handler.go:1527 upstreamErrDetail()` → `call_logs/billing_records.error_message` | 错误文案 = 「上游返回 N: {上游响应体原文}」，dev DTO 原样下发，前端 toast/错误块/列表都直接显示 | 落库侧改为「分类摘要 + request_id」，原文只留管理端字段（如单独 `upstream_raw`，dev 不下发） |
| C | `backend/internal/domain/console/dev_view.go`（`devAttemptKeys`/`DevCallLogView`） | dev DTO 仍下发：`RespBody`（上游原文）、`UpstreamUsage`（协议+原始键值）、`Decision.attempts[].channel_id/channel_name`、`ReqParams.upstream_protocol/converted` | 按阶段一口径收紧白名单（`RespBody` 只留入站协议形状的归一内容；`channel_*` 移出 dev 白名单） |
| D | `backend/internal/domain/billing/stats.go:334-355`（`dashboardTop` 的 ByKey）+ `console/stats.go:437` dev handler 直接复用 | `/dev/stats/dashboard` 返回 `Usage.ByKey`（渠道密钥名 + 渠道名），违反 `docs/pending-tasks.md` 第 6 条「开发者端 DTO 不返回渠道密钥 id/名」 | dev 作用域把 `ByKey` 置空（前端已不渲染，但数据仍到达浏览器，属实质泄漏） |

## 3. 待裁定（与既有已定口径冲突，需负责人拍板）

1. **开发者端渠道显示名**：2026-10-26 已定「开发者端保留 `channel_name` 显示名，但按白名单返回」（`docs/pending-tasks.md` 交付记录），与本次「客户端可见面不得出现渠道名」正面冲突。涉及 3 处渲染 + 1 处 DTO 白名单，改与不改都要一句话口径。
2. **开发者端「原始响应 / 上游返回」**：同一条任务书第 7 条明确要求详情页展示「上游返回」，而展示原文必然带上游实际模型名/上游报文。是「只展示入站协议归一后的内容」，还是「原文仅管理端可见」？
3. **上游协议名与「跨协议转换」标记**是否算泄漏（协议名可反推上游协议族；纯透传后该标记会天然消失）。
4. **「OpenAI 兼容」这类协议兼容文案**（公开页卖点、令牌页 tooltip）是否保留——它声明的是我们对外提供的协议，不是上游身份。
5. **管理端示例文案**里的上游品牌（「主渠道-DeepSeek」「如 deepseek-chat」「如 deepseek / gpt-4o」）是否中性化——管理端内部面，中性化会损失填写指引。

## 4. 已改动

| 文件 | 改动 | 影响面 |
|---|---|---|
| `frontend/src/views/StatsDashboard.vue` | 新增 `moreTitle`（按角色裁剪折叠标题）；「Top 渠道密钥」卡片加 `v-if="isAdmin"` | 仅开发者端统计页少一个只读图表区块；**不改任何接口字段名、返回结构与统计口径**，管理端页面与数据一字未动 |

## 5. 口径与约束自查

- 未引入任何字符串黑名单/全局遮蔽；「不渲染」只用于内部运维维度的区块，数据侧问题按 §2 回报。
- 未改接口契约（字段名、结构、请求参数）、未改统计含义、未动网关代码。
- 门禁：`npx vue-tsc --noEmit` 通过；`npm run build` 通过。