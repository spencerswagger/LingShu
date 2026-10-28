# 预设包数据集 · 人民币口径 / 优惠字段（Spec 产品口径，2026-10-28）

> ⚠ **唯一正本 = `docs/preset-pack-format.md` v0.2（PR #64，branch `docs/preset-pack-dataset-v0.2`）。**
> 本页只是产品视角的补充说明（为什么这么定、兰台冻结时注意什么），**字段名一律以正本为准**。
> 本页 2026-10-28 首版曾自造 `gateway.costUnit` / `gateway.promotions` 两个名字，已全部作废，见文末对照表。

---

## 1. 定价口径（spencer 已定死，这里落成可执行规则）

**规则**：数据集里的价格 = **人民币元 / 百万 token 的倍率**；灵枢据此直接算积分，**不乘 `billing.cny_rate`、不涉及美元与汇率**。

### 1.1 字段落位（以正本 9.1 / 9.2 / 6.5 为准）

| 角色 | 字段 | 说明 |
|---|---|---|
| **唯一计费载体** | `models.<m>.gateway.costRates` | 三段 `Input` / `Output` / `CacheRead`，**PascalCase**，数值 = 官网人民币价，无单位纯数字 |
| 币种声明 | `gateway.costCurrency: "CNY"` | provider 级为默认，模型级可覆盖；**缺省按 `USD`**（保住「直接读 models.dev api.json 不会错」） |
| v1 兼容展示位 | `models.<m>.cost` | 保留 v1 形状，灵枢**完全不解析**；数值必须与 `costRates` 一致（建议由 `costRates` 派生） |
| 禁用地 | `sync.price_source_url` | 那条路把 `cost` 当美元 × `billing.cny_rate`(6.8) → 人民币会被放大 6.8 倍并污染售价。**数据集只能挂 `preset.pack_url`** |

```jsonc
{
  "id": "zhipu",
  "name": "智谱 BigModel",
  "models": {
    "glm-5.3-flash": {
      "id": "glm-5.3-flash",
      "name": "GLM-5.3-Flash",
      "description": "…",
      "cost": { "input": 0.8, "output": 2.8, "cache_read": 0.23 },
      "gateway": { "costRates": { "Input": 0.8, "Output": 2.8, "CacheRead": 0.23 } }
    }
  },
  "gateway": {
    "baseUrl": "https://open.bigmodel.cn/api/paas/v4",
    "protocols": { "openai-chat": { "path": "/chat/completions" } },
    "costCurrency": "CNY",
    "offers": []
  }
}
```

### 1.2 样例：glm-5.3-flash（官网人民币价 → 倍率 → 积分）

| 项 | 官网人民币价（元/百万 token） | `costRates` | 每百万 token 积分（×100） |
|---|---|---|---|
| 输入 | 0.8 | `Input: 0.8` | **80** |
| 输出 | 2.8 | `Output: 2.8` | **280** |
| 缓存命中 | 0.23 | `CacheRead: 0.23` | **23** |

一次调用（10 万输入 + 1 万输出）= `(100000×0.8 + 10000×2.8) ÷ 10000 = 10.8` 积分 = `10.8 × 0.01 = 0.108 元`，与按量价 0.08+0.028 对得上 ✓

> **关键换算锚（正本 9.3 已定）**：`R = 10000` → **1 灵枢积分 = ¥0.01**，**每百万 token 积分 = 人民币倍率 × 100**。这条是后续所有跨渠道折算的唯一尺子。

### 1.3 换算桥接：4 家有官方锚点的渠道 → 灵枢积分

> 用途：把 P0-② 收到的「套餐积分 / AFP / Credits」折成灵枢口径，供 P1 成本页对比。**不是 `costRates` 的内容**——`costRates` 只填 API 刊例价（元/百万 token），套餐内价属另一张表。

通用公式：`灵枢积分/百万token = 人民币元/百万token × 100`；换单位时先求「1 单位 = 多少人民币」，再 `× 100`。

| 渠道 | 官方锚点 | 桥接公式 | 实例 |
|---|---|---|---|
| 腾讯云 Token Plan | 1 腾讯积分 = ¥0.05 | `灵枢积分 = 腾讯积分 × 5` | 刊例 160/560/40（=¥8/28/2）→ **800 / 2800 / 200** 灵枢积分 |
| 讯飞 Astron | 1 讯飞积分 = ¥0.01 | **1:1，无需换算** | GLM-5.2 输出 2800 → **2800** 灵枢积分（=¥28，与 API 刊例一致 ✓） |
| 火山方舟 Agent Plan | 1 AFP = ¥0.002 | `灵枢积分 = AFP × 0.2` | glm-5.3 系数 4.5 → 450 AFP/百万 → **90** 灵枢积分（=¥0.9） |
| 阿里云百炼 Credits | 1 Credit ≈ ¥0.0028 ~ ¥0.0034（按档） | `灵枢积分 ≈ Credit × 0.28 ~ 0.34`（**按档取值，不可统一**） | Lite 39/11,500 → 0.00339；Pro 499/180,000 → 0.00277 |

两条必须守住的边界：

1. **阿里只能算套餐均价**：官方不公布「模型 → Credits 系数」，所以它**进不了 `costRates`**（数据集里只能填阿里 API 刊例价），套餐对比图上只能标「估算」。
2. **智谱套餐内价 ≠ 智谱 API 价**：`costRates` 填 API 刊例（0.8/2.8/0.23），套餐内是积分系数（6.9/1.7/24），两者不能混写；套餐侧另表（`notes/plan-rate-sources-2026-10-28.md`）。

---

## 2. 优惠字段 `gateway.offers`（正本 6.6，兼容性已实测）

**结论**：兼容。灵枢读到任何未知字段都不报错（全仓 `DisallowUnknownFields` 0 处），`offers` 进不了 `GatewayBlock` 结构体，也就到不了 `validate()`。
**→ 0.1.0 先摆空数组 `"offers": []`，0.2.0 再填六类优惠，无需灵枢改造。**

```jsonc
"offers": [
  { "kind": "time_limited", "label": "首发五折", "eligible": "new_user",
    "window": { "start": "2026-08-26", "end": "2026-09-09T24:00+08:00" },
    "effect": { "priceOverride": { "input": 0.4, "output": 1.4, "cache_read": 0.115 } },
    "note": "折扣截止 2026-09-09 24:00（UTC+8）",
    "source": "https://open.bigmodel.cn/pricing", "asOf": "2026-10-28" }
]
```

- 位置：`gateway.offers`（provider 级 = 通用优惠，模型级 = 模型专属）；**合并语义是数组拼接，不覆盖**。
- `kind` 六类：`first_purchase` / `time_limited` / `renewal` / `invite_discount` / `free_quota` / `flash_sale`（开放枚举，未知值整条忽略）。
- `effect` 三选一互斥：`priceOverride`（元/百万 token，可直接顶替 `costRates`）/ `discount`（percent|amount）/ `quota`（unit: token|credit|afp|call）。
- 时间只是「有效区间」标注，**不做自动生效/失效调度**——优惠切换靠重新发布数据集。

### 2.1 那条坑（正本 S1，验收必查）

模型级 `gateway` 若**只有** `offers`、没有 `protocols` / `baseUrl` / `externalModelID`，该模型**整条被丢弃**（含价格）：不报错、不告警。⇒ 带价格的模型**至少声明一个 `protocols` 键**；优惠要么挂 provider 级，要么挂在已可达的模型上。

---

## 3. 校验规则（兰台冻结 Schema 时照抄）

1. 顶层 `{ "<providerId>": Provider }`；`Provider.id` / `name` / `models` / `gateway.baseUrl` / `gateway.protocols` 必填，`id` 与 `name` 不得同时为空（否则 400）。
2. `gateway.baseUrl` 必须以 `http://` 或 `https://` 开头（否则 400）。
3. `gateway.costCurrency` **required + `const: "CNY"`**（正本 6.5 的收紧要求）。
4. 出现 `gateway.costRates` 时，`Input` / `Output` / `CacheRead` **三段齐全且 ≥ 0**（缺一段或为负 → 不报错，但成本积分**静默不记**）。
5. 同一模型内 `cost.*` 与 `gateway.costRates.*` 数值一致（不一致不报错，但两处读数打架）。
6. 模型级 `gateway` 只写 `offers` 不写 `protocols` → Schema 给 **warning**（允许先摆位，但要提示该模型会消失）。
7. `protocols` 键只能是基础协议名（`openai-chat` / `openai-responses` / `anthropic-messages`）；每作用域 `default: true` 至多一个（否则 400）。
8. 日期允许 `YYYY-MM` / `YYYY-MM-DD`；`offers[].window.end` 带时区。
9. 未知键一律允许（不做 strict），CI 出一份「已知键清单 diff」防拼写错（`costRates` 写成 `cost_rates`、`costCurrency` 写成 `cost_unit` 之类）。

---

## 4. 拉取验收清单（12 项勾选表，今晚只写不跑）

前置：数据集只能挂 `preset.pack_url`；**绝不挂 `sync.price_source_url`**。

| # | 检查项 | 期望 | 通过 |
|---|---|---|---|
| 1 | `preset.pack_url` 未配置 | sync 返回空结果、不报错、不写库 | ☐ |
| 2 | `{"confirm":false}` 首次 | 全部 `Applied`，`Pending` 为空 | ☐ |
| 3 | `{"confirm":true}` 再跑 | `Applied` = provider 条目数，库内新增同数量 | ☐ |
| 4 | 回读 `GET /admin/channel-protocols` | `baseUrl` / `protocols` / `requestHeaders` 逐字段一致；`openai-compat` 归一为 `openai-chat`，非基础协议被丢弃 | ☐ |
| 5 | 模型级 `costRates` 落库 | `channel_models.cost_rates` 三段齐全（=¥/百万 token 原值，未乘 6.8） | ☐ |
| 6 | 模型级只写 `offers` 的条目 | **不出现**在协议 models 里（预期行为=S1，非 bug） | ☐ |
| 7 | 幂等：数据未变再 sync | `Applied` = 0 或仅刷新，`Pending` 空 | ☐ |
| 8 | 数据集删掉一个 provider 后 sync | 该协议变 `STALE`，**不删除** | ☐ |
| 9 | 人工编辑过的协议（`edited=true`） | `confirm:false` 只进 `Pending` 带 diff；`confirm:true` 才覆盖 | ☐ |
| 10 | **反向验证①**：往 provider/model 里塞未知字段（`offers`/`costCurrency`/乱写键） | sync 照样成功，未知字段不落库 | ☐ |
| 11 | **反向验证②**：故意改坏一个字段（如 `baseUrl: "ftp://…"` 或 `default:true` 两个） | 报 400 且**整次回滚**，库内无半成品 | ☐ |
| 12 | `costRates` 为负 / 缺一段 | 落库后成本积分不记（S2/S3），且手工保存该行报 400「cost_rates 非法」 | ☐ |

---

## 5. 对照表：我的原写法 → 正本采用（收敛记录）

| 项 | 我 2026-10-28 首版写法 | 正本（PR #64）采用 | 是否影响兰台冻结 | 影响面 |
|---|---|---|---|---|
| 价格字段名 | `gateway.costUnit`（配 `cost` 数值） | **`models.<m>.gateway.costRates`**，三段 `Input/Output/CacheRead` | **是（致命）** | 按旧名冻结 → 灵枢读不到价，成本积分静默为 0 |
| 币种标注 | `costUnit: "CNY_PER_MTOK"` | **`gateway.costCurrency: "CNY"`**（缺省 USD） | **是（致命）** | 按旧名冻结 → 币种声明丢失，S5：被按 USD 理解 |
| 优惠字段名 | `gateway.promotions[]`，`type` 枚举 | **`gateway.offers[]`**，`kind` 枚举 + `effect` 三选一 + `window` | **是（高）** | 按旧名冻结 → 字段位摆错，0.2.0 填充时全量返工 |
| 优惠合并语义 | 未定义 | **数组拼接**（provider 在前、模型在后），不覆盖不去重 | 是（中） | 兰台若按「覆盖」冻结，模型专属优惠会吃掉通用优惠 |
| 「只挂优惠的模型会被跳过」 | 已识别（`promotions` 场景） | 正本归为 **S1**（无 `protocols`/`baseUrl`/`externalModelID` → 整条丢弃） | 是（中） | 验收第 6 项要按「预期丢弃」而非「bug」判 |
| 积分单价 | 未定（示例 ¥0.01 / ¥0.05 并存） | **R=10000 → 1 积分 = ¥0.01；每百万 token 积分 = 倍率 × 100** | **是（致命）** | 单价不统一，换算表会整体差 5 倍 |
| 禁止路径 | 只提「不走 `billing.cny_rate`」 | **`sync.price_source_url` 是硬红线**（×6.8 污染售价） | 是（高） | 挂错 URL 会让全量人民币价虚高 6.8 倍并写入售价 |

> **作废名（勿用于冻结，保留仅为追溯）**：`gateway.costUnit`、`costUnit: "CNY_PER_MTOK"`、`gateway.promotions`、`promotions[].type`。

---

## 6. 我对正本的两点疑问（请转 Ark / 后端判断，不是反对）

1. **`costRates` 的 PascalCase 与 `ValidateRates` 的 snake_case 之间，转换点没写明。**
   正本 9.1 要求兰台输出 `Input/Output/CacheRead`，而 13.2 E6 说 `ValidateRates` 只校验 `input/output/cache_read`。若物化时没有一次显式映射，S2（成本积分静默不记）就会**常态发生**而不是例外。建议在正本里点明「物化时 PascalCase → snake_case 的转换位置」，验收第 5 项也顺手核对大小写是否真落进 `cost_rates`。

2. **`costCurrency` 缺省 USD 与 `costRates` 恒为人民币，二者语义打架，缺口是「静默错价」而非「仅影响人读」。**
   S5 说忘记写 `costCurrency` 「仅影响人读/未来消费方」。但如果一份包**写了 `costRates` 却没写 `costCurrency`**（例如有人把 models.dev 的美元 `cost` 抄进 `costRates`），灵枢会把这些美元数字当人民币倍率落库 → 价格**虚低 6.8 倍**，且不报错。这与 9.4 的「虚高 6.8 倍」是同一枚硬币的另一面。
   建议：Schema 层把「`costRates` 出现 ⇒ `costCurrency` 必须显式出现」设为硬约束（而不是只在数据集层用 `const: "CNY"` 兜），并在 S5 里补一句「有 `costRates` 时缺 `costCurrency` = 错价，不是无害」。
