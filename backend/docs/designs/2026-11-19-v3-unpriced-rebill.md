# unpriced 账单补收（B 方案·手动触发）设计

日期：2026-11-19　分支：`fix/v3-settle-gap`（追加 commit）　作者：Core

## 1. 背景与目标

V3 旁路计费（`poller.go`）对解析不到定价口径的账本行落 unpriced 账单（营收 0、不扣钱）。
运营在拓扑页给组定价后，这批历史账单需要按当前售价补收。spencer 拍板 B 方案：手动按钮触发，
**补开新账单、不改旧行**（原行只允许追加标记）。

## 2. 接口

```
POST /api/v1/admin/billings/rebill-unpriced
Body: {"Ref": "group/auto-flash"}        // 支持：组 ref / 组 id / auto/<model>
Resp: {"GroupRef","Scanned","Rebilled","TotalCredits","NewBillingIDs","AlreadyMarked"}
```

错误映射：组未定价 / 行级解析失败 → 400 + Reason（`rebill_group_unpriced` / `rebill_row_unresolvable`），
前端据此提示「先绑定售价」。只挂 `/api/v1/admin`（成本侧数据不给开发者端，与阶段三运营端点同纪律）。

## 3. 规则落点（与拍板口径逐条对应）

| 口径 | 实现 |
|---|---|
| 补开新账单、不改旧行 | 新账单 status='completed' 走 `bill.Record` 正常扣款；原行只有 `error_message` 尾部追加『；已补收:<新账单id>』（金额/状态/原因原文不动） |
| 新账单 Remark | `billing_records` **无 remark 列** → 落 `error_message = '补收 unpriced <原账单id>'`（与主链 unpriced 原因同一通道，详情页原文可见） |
| 售价 = 补收时刻当前售价 | 复用主链 `resolvePrice` 候选链（req/served/组快照/route_id 顺序逐字一致），不复制第二套定价逻辑 |
| 用量 = 原行 tokens | 读 `billing_records.tokens`（JSONB）原值 |
| 成本沿用原行 | 直写 `cost_credits = 原行值, cost_source='ledger'`（不带 CostRates，不重算） |
| 扫描匹配范围 | 组 ref / 去前缀组 id / auto\<model\> slug 归一（`groupRefForms`）/ 组快照 display_name（`groupMatches`）；route_id 映射按主链口径在售价解析侧生效（权威映射在网关内存、镜像不到） |
| 幂等 | 见 §4 |

## 4. 幂等三层（同一原行只能补收一次）

1. **billing_id 固定派生**：新账单号 = `rebill-<原billing_id>-v1`，撞 `billing_records.billing_id`
   唯一索引 = 已补收过。Record 的事务路径撞库时扣款随事务回滚（不重复扣）。
2. **行内新鲜判定**（一次往返）：原行标记已带 / 新账单已存在 → 跳过。承接「新账单已落、
   标记事务丢失」的中断态：重放只补成本与状态核对，不再扣款。
3. **标记守卫 UPDATE**：`WHERE status='unpriced' AND error_message NOT LIKE '%已补收:%'`，
   RowsAffected==0 → 报错拒绝。防「标记在而账单不在」被二次扣款。

**原子性取舍**：刻意不开外层大事务。补收逐行自愈（任何时点中断都可安全重放）、
行与行互不牵连；外层事务会把长时间行锁摊到整批扫描行上，与 60s 结算轮争锁。
真库并发双击用例（`TestRebill_PG_ConcurrentDoubleClick`）验证：两路并发只出一张新账单、只扣一次钱。

## 5. 测试资产

| 文件 | 覆盖 |
|---|---|
| `rebill_test.go` | sqlmock 行为链：正常补收（金额=ComputeCredits 独立复算）/ 三种重复形态 / 组未定价整体拒 / 守卫归零报错 / 23505 库层回声 / 余额不足透传 / 匹配范围表驱动 / 标记 SQL 源码级守卫 / **随机 20 轮**（随机 token×随机价格，金额逐轮自校验 + 重复补收全拒） |
| `rebill_pg_test.go` | 真库（DSN 门控）：端到端（余额逐笔吻合 + 原行标记正确 + 无关组不碰 + 成本沿用）/ 中断重放不重复扣款 / 并发双击单张单扣 |
| `routes_rebill_test.go` | 路由只挂 admin + 无 dev 镜像（源码级）+ 线上注册核对（无凭据 401 而非 404） |

变异验证（三组，均已验证会红、还原即绿）：删标记守卫谓词 → 守卫归零用例红；
billing_id 改随机派生 → 固定派生断言红；匹配去形态归一 → Matching 红。

## 6. 已知边界

- 扫描是全表 `status='unpriced'` 行 + Go 侧匹配（unpriced 行量有界：只在解析失败时产生）。
  量级失控时再收紧 SQL 侧过滤，但**必须**保持与 `groupRefForms` 同源（不许 SQL 里另写一套 LIKE）。
- route_id → 组 ref 的权威映射只在网关内存里；扫描侧不引入额外名字（与主链口径一致）。
- 前端（View）按任务书加「检测到 N 条未定价账单」按钮与确认弹窗，后端只提供本接口。

## 7. 结算链路 kill-switch（2026-11-20 拍板落地）

**口径**：新结算链路开关按 env 闸门控制、**默认关**——未设置 `V3_SETTLE_ENABLED` 时走
旧链路（= `main` v0.15.0-rc4 的结算语义，逐条复刻在 `settleOneLegacy`：req/served 名字直查、
查不到留行下轮重试、无 unpriced 兜底、无补收），显式置 `1/true/on/yes` 才启用新链路
（组 ref 多形态候选链、route_id 绑定优先、unpriced 兜底、补收入口）。

- **判定单源** `v3billing.SettleEnabledFromEnv()`（装配与测试共用）；**fail-closed**：
  只有显式真值才开，打错字（如 `ture`）落回旧链路——新链路写账单动钱包，不许静默启用
  （刻意区别于 CALLERKEY_BACKFILL_ON_BOOT 的「非空即开」）。
- **两个结算触发点都吃同一个开关**（开关长在 Poller 上）：60s ticker 与账本同步轮内的
  `settleNow`（拉完即结）；装配层 `cmd/server` 启动即按 env 设置并打点两条 INFO 日志
  （「新链路已启用 / 未启用」），不留「部署完为什么没出账单」的猜。
- **补收入口**：关闭时 `Rebill` 返回 `ErrRebillDisabled` → HTTP `400 + Reason=rebill_disabled`。
  补收定价逻辑 = 新链路 `resolvePrice`；开关关闭通常意味着对新链路定价存疑，放行等于把
  同一套存疑逻辑重放进账本，故一并拒绝（不是 500——这是部署形态问题，不是服务故障）。
- **部署面**：`docker-compose.yml` backend.environment 透传 `V3_SETTLE_ENABLED`（默认空），
  `.env.example` 有注释条目。dry-run 分桶结果确认后再开；出账异常应急回退 = 清空该变量重启。
- **旧链路纪律**：`settleOneLegacy` 刻意不复用新链路的 resolvePrice/候选链——kill-switch 的
  价值就在两条路径互不借用，防行为漂移。新链路的解析增强一律不得泄漏进这条路径。

测试（`gate_test.go` + `wiring_test.go`）：env 判定表（含打错字）/ 包内默认新链路 /
**同一行数据的开/关 A/B**（关 = 名字直查不到绑定 → 留行重试零账单；开 = 绑定命中正常结算）/
关时无 unpriced 兜底 / 关时补收拒绝且零 DB 访问 + HTTP 400 reason / 装配层源码级守卫
（判定必须走单源、两条分支都要显式 SetNewSettleEnabled、两条启动日志都要在）。
变异验证：删 Once 分流 / 删 Rebill 闸门 / 删裸名反推分支 → 各自用例红，还原全绿。

## 8. 补收入口多形态归一（评审 P3，2026-11-20）

**问题**：入口 `Rebill()` 预检把目标组折叠成单一 ref 形态（`groupRefForm`），无组快照桥时
`Ref=auto/<m>` 与 `Ref=<裸名>` 都会被误判成「该组当前没有售价口径」直接 400——而同一组
的绑定/镜像形态其实有价。Gate 真机实测：网关 auto 组账本行 `req_model` 是**裸组名**，
裸名归一只出 `group/<名>`，与抽屉绑定形态之间全靠组快照桥接，桥缺失即断链。

**修法（与行匹配同源）**：
1. `groupRefForms` 补**第三向**：裸名也反推抽屉绑定形态（`auto/<裸名>`）与镜像 slug 档
   （`group/auto-<slug>`）——主链结算与补收共用同一归一，裸名行在没有桥时也能按绑定/镜像
   形态命中售价。`group/<名>`（非 auto 组 ref）刻意**不**反推 auto/：「组名叫 X」与
   「auto 组名叫 X」是两个不同对象。
2. 入口目标从单 ref 改为 **refs 集合**：预检逐形态尝试（任一命中售价即通过），行匹配
   （`rebillMatchesRow`）按集合比对；响应 `GroupRef` 取首位（= 旧版 `groupRefForm` 归一结果，
   wire 兼容）。

**已知局限保持**：slug 有损（`Flash x` → `flash-x`），从 slug 串还原不出非 slug 绑定——
该方向仍走组快照桥（display_name），与既有用例的「只断言无损方向」口径一致
（`TestRebillEntrance_MultiFormNoSnapshot` 第三子用例钉住这一边界，防未来误当 bug 修）。
