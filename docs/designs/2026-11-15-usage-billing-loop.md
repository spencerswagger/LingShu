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

## 6. 已知风险

1. 不注入 `include_usage`（第②件刻意不做）→ OpenAI 系流式 usage 可能整段缺失，`estimated=true` 占比升高；
   超过 20% 应向代理侧提「注入开关」需求。
2. `usage/requests` 的字段名尚未联调确认：adapter 用候选键容错，字段改名会降级成 0（窗口偏差会立刻暴露），
   HTTP 层失败才报 `error`。
3. cost 模式用户本期没有成本价源（`channel_models.cost_rates` 已随渠道下线），仍按售价计费，`cost_credits` 为 0。
4. 预扣是「按估算扣」，估算偏高会让用户短时被多冻（结算即多退）；偏低则结算补扣，余额不足时报错。