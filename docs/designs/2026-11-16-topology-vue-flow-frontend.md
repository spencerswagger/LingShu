# 拓扑页（Vue Flow）与 13 页骨架 · 前端实现说明

> 2026-11-16 · View（前端）· 分支 `feat/v2-console-ops-pages`
> 依据：`2026-11-16-stage3-frontend-13-pages.md`（P02 验收口径）、`2026-11-15-v2-p1-data-contract.md` §2.1–§2.3（表与语义）、
> 预览站 `previews/admin/topology.html` + `previews/dag-demo/`（已确认原型）

## 1 骨架：菜单按已确认原型收口

- 管理端 8 项：统计 · 拓扑 · 用户 · 令牌 · 账单 · 盈亏与对账 · 公告 · 系统；
  另挂三项供给面（模型 / 订阅 / 账本）与「对账」子页——它们是 Spec 13 页里的 P03/P04/P05/P06，不属于原型 8 页但业务上必须在。
- 开发者端 4 项：统计 · 令牌 · 模型（只读）· 账单。
- **下线**：会话（管理端/开发端均已摘菜单，路由 redirect 到统计）、V1 成本页（redirect 到盈亏）。
  两者都写在 Spec §1.2 的下线清单里；页面文件保留在本轮之外单独清理，避免和本棒的画布改动混在一个 diff。

## 2 拓扑页数据契约（前端消费的 T1 形状）

`GET /api/v1/admin/topology` → `Data`（PascalCase 为主，snake_case 也认）：

```
Items:     [{ Ref, ProviderRef, Kind: group|direct, DisplayName, ContextLen, PosX, PosY, LastSeenAt, Stale }]
Links:     [{ InternalRef, ExternalModelID, PosX, PosY }]            // 画布坐标，可空
Providers: [{ Ref, Protocol, Accounts, Models, Stale }]              // 缺省时前端按 Items 现算
Accounts:  [{ Ref, ProviderRef, Kind: plan|payg|'', Label, WindowResetAt, LastCallAt, Stale }]
Externals: [{ ID, Name, BoundInternalRef, Enabled, PriceIn, PriceOut, PriceCache, Stale }]
StaleCount, UnboundExternal, SyncedAt,
Sync:      { LastSuccessAt, LastRunAt, State: ok|failed|never, Message, Created, Stale }
```

- `Providers` 缺失 → 前端按 `Items.ProviderRef` 聚合，保证「供应商 → 模型」计数与列表条数一致（验收 1）。
- `Accounts` 缺失 → 账号层空着（网关只读，灵枢没有镜像表，**不编**）。
- `Externals` 的 `BoundInternalRef` 是绑定真源（`external_models.bound_internal_ref`），空串 = 未绑定 = 不售卖。

## 3 渲染与交互

| 项 | 规则 |
|---|---|
| 四层 | 账号（蓝）→ 供应商大卡（卡内是直通模型行）→ 路由组（紫）/ 对外模型（绿） |
| 连线 | 只有两种：账号→供应商（网关事实）、实际模型/路由组→对外模型（灵枢唯一可写映射）。**不画「模型→组」**——组成员在网关侧，灵枢没有镜像 |
| stale | 节点/行降透明度 + 虚线边框 + 文案「本轮未见」，仍展示、仍可绑定 |
| 布局 | T2（坐标保存）未上线 → 自动布局（列 × 纵向堆叠），刷新重排；页面不暗示「拖完会保存」 |
| 立即同步 | 调 `POST /admin/topology/sync`；200 显示「拉取/新增/失效」并重取列表；404/405/501 如实说明「接口未上线」，不做假成功 |
| 点对外模型 | 跳 `/admin/models/{id}/edit`（绑定/售价在那里改） |
| 不展示 | 任何凭据、密钥、上游 URL（Spec P02 验收 5） |

## 4 未就绪与空态

- 接口 404/405/501/403 → `pending`：灰底说明依赖哪几个端点 + 「预览演示数据」按钮；**默认不显示任何数字**。
- `live` 但 `Items` 为空 → 空态「还没有内部模型：同步器未跑过，或网关未返回任何模型」。
- 错误 → `ErrorBubble`（带 requestId）。

## 5 本轮自测（DOM 断言，非截图）

`vite dev` + puppeteer 拦 `/api/v1/admin/**` 回桩数据，量真实视图：

- 1440px：`.vue-flow__node` = 12、`.vue-flow__edge` = 7（3 账号 + 4 已绑定对外模型）、统计卡 5 张、
  页面横向溢出 0；点「立即同步」（桩 404）后出现「接口未上线」说明而不是静默。
- 390px：横向溢出 0，画布高度 420px。
- 账单页（计费）：3 行渲染、无「会话」列、有「归因 / 成本来源」列、390px 溢出 0。
- 盈亏页：汇总卡 8 项 + `Uncosted` 黄条 + 分组表，收入−成本=毛利自检通过。
