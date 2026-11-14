# 旁路 usage 计费闭环（V2 第一期 · 第③件）

## 1. 范围

本件只打通「钱」的闭环：预扣 → 转发 → 按 usage 结算 → 退补 → 对账。
不改转发行为、不改前端、不改价目表定价逻辑、不改代理侧仓库、不写请求日志（`call_logs` 保持停写）。

## 2. 链路

```
client → 灵枢 facade（准入）
        ├─ 改写 model、换 callerKey（第②件）
        ├─ Reserve：按请求体估算输入 token → EstimateCredits → credit.Consume（冻结）
        │           余额不足/估算失败 → 402 拒绝，不发上游
        ├─ 转发（响应字节原样回流，usage 只旁路读）
        └─ Settle：读 Result（usage/HasUsage/NoContent/Canceled/Status/RequestBody/ObservedTextRunes）
                   → 账单落库（billing_records）
```

- 预扣只按**输入**估算（与 V1 `estimatePreConsumeCost` 同口径），输出留待结算多退少补。
- 结算沿用 `RecordReq.PreConsumed`：`>0` 时只结算差额，`=0` 时全额扣。
- 不引入预扣状态机（preauth/settled/expired），账单只多一个 `estimated` 标记。

## 3. 口径表

| 情形 | 钱 | 账单 |
| --- | --- | --- |
| 2xx + 有 usage | 按 `usage × 售价 × 系数 ÷ R`，与预扣差额多退少补 | `completed` / `estimated=false` / `upstream_usage` 原文 |
| 2xx + 无 usage | 按请求体估算（输入）+ 已见生成内容估算（输出） | `completed` / `estimated=true` / `upstream_usage=NULL` |
| 上游 4xx/5xx / 连接失败 / 首包超时 | **退预扣、不收钱** | `failed` / `credits=0` |
| 客户端取消 + 有 usage | 按 usage 计费 | `completed` |
| 客户端取消 + 无 usage | **退预扣、不估算**（行为反转：旧口径是估算计费） | `failed` / `error_message=client_cancelled` |
| 204 / 304 | 退预扣 | `failed`（无内容可计费） |
| count_tokens 类（`Billable=false`） | 不预扣不结算 | 无账单 |

「有 usage」的判定沿用第②件旁路采集：四协议按契约字段路径归一成三段（`input/cache_read/output`），
三段全零视为未取到。

## 4. 与对账的关系

`billing_records.magpie_key_name` 是对账分组维度（与 `magpie_keys.key_name` 同名）。
`estimated=true` 的账单单独成组、**不计入** token 主口径：它是估算值，拿来比必然误报。

## 5. 对账

- 调度：60 秒一窗，`pg_try_advisory_lock` 保证多副本只跑一个实例。
- 输入 A：`billing_records`（`magpie_key_name IS NOT NULL` + `status='completed'`）按 `(key, 内部模型)` 聚三段 token 与笔数。
- 输入 B：`GET /v1/magpie/usage/requests?from&to&callerKey`（每次 ≤50 key 分批）。
- 只比 token 三段与笔数，不比金额（代理侧美元牌价 vs 灵枢人民币倍率，比钱必误报）。
- 阈值：分组笔数 ≥5 且任一段相对偏差 >5% → `alarm`；窗口整体加权偏差 >2% 且总量 ≥10000 → `alarm`。
  相对偏差分母以**代理侧**为基准；`magpie=0 且 ls>0` 该段直接命中；两边同为 0 跳过。
  笔数不一致只进 `detail`（仅作样本门槛）。
- 单侧缺账（含「灵枢只有估算单」）不做偏差判定，只记进 `absent_in_ls/absent_in_magpie`。
- 连续 3 窗 `alarm` → 升级：写 `audit_logs`（`action=reconcile_alarm_escalated`）+ ERROR 日志。
- **永不回写账单**：本件唯一写路径是 `reconcile_windows`（`UNIQUE(window_start,window_end)` + `ON CONFLICT DO UPDATE`）。
- 拉取失败 → 窗口记 `error`（下一窗自然重试）；未配 `magpie.admin_key` → 每窗记 `skipped`，
  让运维看得见「没在对账」而不是无记录。

### 5.1 最小字段集与降级表（契约 §8.2）

- 最小 9 项：逐笔键、带时区时间戳、caller key、model、in/out tokens、failed、retried（必需），
  cache_read（可降级）。每次响应产出 `LedgerReport`（可见性 + 分页形态 + failed/retried 笔数），
  随窗口落进 `detail.ledger_report`。
- 缺任一必需项 / 时间戳不带时区 / 分页只有 offset 形式（或看不出来）/ 一条记录都没有（`no_samples`）
  → 该窗口**不可对账**：`status='blocked'`、`detail.blocked='blocked: pending POC'`、
  `detail.blocked_reasons=[...]`。既不算通过也不算不通过——未做 POC 就不许写通过。
  本地比对仍照算，但只作参考放 `detail.local_comparison`；blocked 窗不累计 `alarm_streak`、不升级审计。
- 分页不臆造 cursor：只有接口自己回了 `next_cursor` 之类的字段才认 cursor，否则按 offset 判不可对账。
- `blocked` 与 `skipped` 的区别：`skipped` = 我方没接上账本；`blocked` = 接上了但字段不全，
  两者的排查动作不同，故单列状态（迁移 `0020_reconcile_blocked_status.sql`）。
- 报警逻辑不变：只比 token 三段 + 笔数，不比金额；超阈值只告警、连续 3 窗升级、**永不回写账单**。

### 5.2 资金流水备注常量（契约 §8.3）

- 本期不补 `billing_id`（二期随状态机做）。守恒断言按「`user_id` × 60s 窗口 × 备注类别」三条：
  钱包守恒 / 账单对齐 / 单笔上限。
- 备注文案即分组键，只能有一处来源：`billing.FlowRemarkReserve`（预扣）/ `FlowRemarkDelta`（差额）/
  `FlowRemarkRefund`（退回）。有源码级用例扫全仓，禁止在调用点另写字面量。

## 6. 已知风险

1. 不注入 `include_usage`（第②件刻意不做）→ OpenAI 系流式 usage 可能整段缺失，`estimated=true` 占比升高；
   超过 20% 应向代理侧提「注入开关」需求。
2. `usage/requests` 的字段名尚未联调确认：adapter 用候选键容错，字段改名会降级成 0（窗口偏差会立刻暴露），
   HTTP 层失败才报 `error`。
3. cost 模式用户本期没有成本价源（`channel_models.cost_rates` 已随渠道下线），仍按售价计费，`cost_credits` 为 0。
4. 预扣是「按估算扣」，估算偏高会让用户短时被多冻（结算即多退）；偏低则结算补扣，余额不足时报错。

## 7. 对账/结算的四处硬缺陷（#82 复审修复）

1. **分组维度**：`billing_records` 同 (key, model) 会因 `estimated` 返回两行。先合并再按标记切分会
   **整组**被剔出主口径（估算单顶掉真实 token），必须先按维度过滤再合并。`estimated` 单只做计数
   （`estimated_groups` / `estimated_tokens`），只有估算单的组记 `absent_in_ls` 供人工定位。
2. **结算失败必须退预扣**：预扣在转发前就真扣了；结算失败（差额扣不动/账单没落）若不退回，
   就是「用户被扣钱、账单显示 0、对账只统计 completed 看不到」。顺序固定为
   「退预扣（带重试）→ 落 failed 账单（0 积分 + 自解释原因）→ 资金异常审计」。
   退款最终失败写 `audit_logs`（`action=billing_anomaly`），账单同时标注需人工核对——
   对账看不到这笔钱，这是唯一的可检索痕迹。
3. **流水号**：`bill-YYYYMMDD-<12 hex>`（48 bit）。旧 6 位十进制单日 1 万单期望约 50 次碰撞，
   而它同时是钱包流水与账单的幂等键。撞号（23505）**不许**重查返回已存在账单（那是别人的单），
   一律换新号重试；幂等快速路径加归属校验（user/token/model）。
4. **advisory lock 必须在同一连接**：会话级锁在连接池上取/解，解锁会落到别的连接 →
   锁泄漏、后续窗口恒拿不到 → 对账静默停摆。用 `db.Conn` 固定连接，解锁返回值显式检查；
   拿不到锁累计 `LockSkips` 并打 WARN（宁可噪声，也不能静默）。
