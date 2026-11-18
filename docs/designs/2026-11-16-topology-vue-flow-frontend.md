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

> 2026-11-17 校正：PR #88（后端 T1/T3，`d60d992`）已合入 v2，字段以此为准——本节早前版本是照着表结构
> 自己推的形状（`Providers[].Ref/Protocol`、`Accounts[].Ref/ProviderRef/Kind/Label`、
> `Externals[].Name/PriceIn/PriceOut`），**后端一个都没有**。真报文喂进旧 normalizer 的结果是
> 供应商全空 ref（两张卡同 id）、账号全 `a::`、画布只剩一张空卡。已按 `handler.go` 的 view 改齐。

`GET /api/v1/admin/topology` → `Data`（PascalCase 为主，snake_case 也认）：

```
Items:     [{ Ref, ProviderRef, Kind: group|direct, DisplayName, ContextLen, PosX, PosY, LastSeenAt, Stale, Caps?, ListPrice? }]
Links:     [{ InternalRef, ExternalModelID(string), PosX, PosY }]     // 画布坐标，可空
Providers: [{ Provider, Models, Stale, Accounts }]                    // 没有 ref 字段；Stale 是**条数**
Accounts:  [{ Provider, Account, Subscribed, LastSeenAt }]            // 没有 ref 字段；(Provider, Account) 即唯一键
Externals: [{ ID(string), ExternalName, Enabled, BoundInternalRef, InternalModelHit }]
StaleCount, UnboundExternal, SyncedAt,
Sync:      { LastSuccessAt, LastRunAt, State: ok|failed|never, Message, Created, Stale }
```

节点身份（前端派生，别再发明后端字段）：

| 层 | 节点 id | 依据 |
|---|---|---|
| 供应商 | `p:${Provider}` | Providers[] 无 ref；与 `Items[].ProviderRef` 同一套取值，卡里才能按它挂直通模型行 |
| 账号 | `a:${Provider}:${Account}` | Accounts[] 无 ref；(Provider, Account) 是后端去重键 |
| 路由组 | `g:${Ref}` | Items[] 自带 ref；组是跨供应商聚合层，`ProviderRef` 恒空串 |
| 对外模型 | `x:${ID}` | Externals[] 的雪花 id（字符串下发，超 JS 安全整数） |

- `Providers` 缺失 → 前端按 `Items.ProviderRef` 聚合（`Kind==='group'` 跳过），保证「供应商 → 模型」计数与列表条数一致（验收 1）。
- **`Provider=""` 那一行必须丢掉**：后端 `Providers` 是 `GROUP BY provider_ref`，组（`provider_ref=''`）会占一行，
  照收就会多出一张空的「(未知)」供应商卡。组本身由 Items 里的 group 节点渲染。
- `Accounts` 缺失 → 账号层空着（三处来源现算，灵枢没有镜像表，**不编**）；额度窗口 / 最近调用时间后端不返回，页面不显示。
- `Externals` 的 `BoundInternalRef` 是绑定真源（`external_models.bound_internal_ref`），空串 = 未绑定 = 不售卖；
  `InternalModelHit=false` 且有绑定 = **绑定悬空**（目标已从网关下线/改名）→ 节点打橙标、不画线。
- T1 不给价格档（`PriceIn/PriceOut` 在模型编辑页），画布**不显示价格**，不拿 0 冒充。

## 3 渲染与交互

| 项 | 规则 |
|---|---|
| 四层 | 账号（蓝）→ 供应商大卡（卡内是直通模型行）→ 路由组（紫）/ 对外模型（绿） |
| 连线 | 只有两种：账号→供应商（网关事实）、实际模型/路由组→对外模型（灵枢唯一可写映射）。**不画「模型→组」**——组成员在网关侧，灵枢没有镜像 |
| stale | 节点/行降透明度 + 虚线边框 + 文案「本轮未见」，仍展示、仍可绑定 |
| 布局 | T2（坐标保存）未上线 → 自动布局（列 × 纵向堆叠），刷新重排；页面不暗示「拖完会保存」 |
| 立即同步 | 调 `POST /admin/topology/sync`；**成败看 `State`**（失败也回 200）：`ok` 绿字「同步完成：拉取/新增/失效」、`failed` 红字带原因 + 「旧快照已保留」、`State` 缺失橙字「无法确认成败」；失败同样重取 T1 让状态条翻失败。404/405/501/503（未配网关源）如实说明，不做假成功 |
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

### 5.1 #88 真报文回归（2026-11-17）

脚本 `/data/pv/topology-t1-check.mjs`（一次性，不入仓库）把 #88 的真报文回桩（含 `Provider=""` 那一行、
含绑定到组的对外模型、含 `InternalModelHit=false` 的悬空绑定），断言：

- 供应商卡 3 / 账号 3 / 路由组 1 / 对外模型 3，连线 6（账号→供应商 3 + 内部模型/组→对外模型 3，两类都齐）；
- 节点 id 无重复、无空身份（旧实现在这里是 `p:` / `a::`）；
- 页面不出现「(未知)」卡；账号页签走真字段（供应商 / 账号 / 订阅 / 最后出现）；
- T3 回 200 + `State=failed` → 红字「同步失败 + 原因」，不是绿字「同步完成」；
- 390px 横向溢出 0。
