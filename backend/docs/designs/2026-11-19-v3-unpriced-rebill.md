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
