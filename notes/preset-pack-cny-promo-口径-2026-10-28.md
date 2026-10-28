# 预设包数据集 · 人民币口径 / 优惠字段 / 兼容性（Spec 产品口径，2026-10-28）

> 本文是**产品口径输入**，供 0.1.0 数据集冻结与 0.2.0 排期使用；schema 正本仍以 `docs/preset-pack-format.md` 为准（由架构维护）。
> 结论先行：**能加、不会报错、但 0.1.0 只摆字段位不会被消费**；币种必须显式声明，不能靠默认值猜。

---

## 1. 定价口径（spencer 已定死，这里落成可执行规则）

**规则**：数据集里的 `cost` = **人民币元 / 百万 token 的倍率数值**；灵枢据此直接生成积分，**不乘 `billing.cny_rate`、不做美元/汇率换算**。

### 1.1 币种怎么标注才不破坏 models.dev v1 格式

| 方案 | 做法 | 判断 |
|---|---|---|
| A. 把 `cost` 改成字符串（`"¥8"` 或 `"8 CNY"`） | — | ❌ 破坏 v1 数值形状，无法无损读 `api.json`，strict 校验直接挂 |
| B. 在 provider / model 顶层加 `cost_currency` | 与 v1 字段并列 | ⚠ 能用，但占用了「v1 未来可能新增的键」，违背「扩展隔离」原则，长期有撞键风险 |
| C. **在 `gateway` 命名空间加 `costUnit`**（推荐） | `gateway.costUnit = "CNY_PER_MTOK"`，模型级可覆盖 | ✅ 不碰 v1 形状，未知键被忽略，向后兼容 |

**定稿建议（C）**：

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
      "limit": { "context": 1000000, "output": 128000 },
      "gateway": { "costUnit": "CNY_PER_MTOK" }
    }
  },
  "gateway": {
    "baseUrl": "https://open.bigmodel.cn/api/paas/v4",
    "protocols": { "openai-chat": { "path": "/chat/completions" } },
    "costUnit": "CNY_PER_MTOK"        // provider 级默认，模型级可覆盖
  }
}
```

**读取规则（灵枢侧，0.2.0 实现）**：

1. `costUnit` 缺省 → 按 models.dev 原义解释为 `USD_PER_MTOK`（**这样直接吃 `api.json` 不会错**）；
2. `costUnit = "CNY_PER_MTOK"` → `cost.*` 即人民币元/百万 token，**跳过 `billing.cny_rate`**，直接 `积分 = cost / 积分单价`；
3. 未知 `costUnit` 值 → 不猜、不换算，标记该模型价格为「不可用」，前端不展示（宁可缺，不可错）。

> 为什么不用「默认人民币」：灵枢要能直接吃 models.dev 原始数据（那是 USD），默认值一改就会把 USD 当 CNY，价格虚高 7 倍。**默认必须保持 v1 语义，只有我们自己产出的数据集才显式写 CNY。**

### 1.2 样例：glm-5.3-flash（官网人民币价 → 倍率 → 积分）

| 项 | 数值 | 来源 |
|---|---|---|
| 官网刊例（人民币/百万 token） | 输入 ¥0.8、输出 ¥2.8、缓存命中 ¥0.23 | 智谱 API 定价（2026-08 起，限时 5 折前的刊例） |
| 数据集写法 | `cost: {input: 0.8, output: 2.8, cache_read: 0.23}` + `gateway.costUnit: "CNY_PER_MTOK"` | — |
| 灵枢积分（示例：1 积分 = ¥0.01） | 输入 **80 积分**、输出 **280 积分**、缓存命中 **23 积分**（每百万 token） | 积分 = 人民币 ÷ 积分单价 |
| 灵枢积分（示例：1 积分 = ¥0.05，同腾讯锚点） | 输入 16、输出 56、缓存命中 4.6 | 同上 |

算式一行：`积分/百万token = 人民币价 × (1 / 积分单价)`，不出现美元、不出现汇率。

> 注意：这个 `cost` 只描述**API 刊例价**；套餐内价（腾讯 GLM-5.3 套内 8/28 元 vs 智谱套内约 3.3/11.6 元）是另一张表，来自 `notes/plan-rate-sources-2026-10-28.md`。两者**不要混写进同一个 cost**。

---

## 2. 新字段：「优惠方式」结构化 —— 会不会报错、放哪、叫什么

### 2.1 结论（先给答案）

- **不会报错**。灵枢拉取预设包用的是 Go `encoding/json` 的非严格解码（`internal/domain/channelprotocol/source.go: fetchPack`），未知键直接忽略；全仓库没有 `DisallowUnknownFields`。
- **放 `gateway` 命名空间**，名字用 `promotions`（数组）：provider 级放整包促销，模型级放单模型促销。
- **0.1.0 可以先摆字段位（留空数组或不写）**，不影响同步；但**0.1.0 读不到内容**（结构体没有对应字段，也不会落库）。
- ⚠ 一个真实坑：模型级 `gateway` 若只有 `promotions` 而**没有** `protocols` / `baseUrl` / `externalModelId`，`toChannelProtocol` 会**整条跳过该模型**（不报错，但模型清单里就没了）。所以「只挂优惠」的模型条目，在 0.1.0 里等于不存在。

### 2.2 字段定义

```jsonc
"gateway": {
  "promotions": [
    { "type": "first_purchase", "price": 39, "originalPrice": 60,
      "appliesTo": "plan", "note": "新客首月", "startsAt": "2026-07-20",
      "endsAt": "2026-12-31", "sourceUrl": "https://…", "asOf": "2026-10-28" }
  ]
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `type` | string | 是 | `first_purchase` 首购价 / `limited_time` 限时活动 / `renewal` 续费价 / `invite` 邀请折扣 / `free_quota` 免费额度 / `flash_sale` 限量抢购 / `night_discount` 错峰折扣（新增，各家都有） |
| `price` / `originalPrice` | number | 否 | 人民币元；`originalPrice` 填划线价，**不要填折后价** |
| `discountPercent` | number | 否 | 无明确价格只有折扣时用（如「5.5 折」→ 55） |
| `appliesTo` | string | 否 | `plan`（套餐价）/ `model`（模型倍率）/ `credit`（额度） |
| `startsAt` / `endsAt` | string(date) | 否 | 限时活动必须有 `endsAt`，否则无法判断过期 |
| `note` | string | 否 | 一句话说明，给前端展示 |
| `sourceUrl` / `asOf` | string | 否 | 素材可追溯（P0-② 的下游产物直接填这里） |

- `type` 按**开放枚举**处理：未知值透传 / 忽略，不报错（与格式第 2 节原则一致）。
- 过期判定：消费方以 `asOf` 为准，`endsAt < asOf` 的条目不展示、不计价。

### 2.3 兼容性验证结论表（基于代码，不是猜测）

| 验证点 | 结果 | 证据 |
|---|---|---|
| 预设包解析是否严格 | 否，未知键忽略 | `source.go: fetchPack` → `json.NewDecoder(resp.Body).Decode(&pack)` |
| 是否存在 `DisallowUnknownFields` | 无 | 全 backend `internal/` 搜索 0 命中 |
| `Provider` 结构解析哪些键 | `id/name/env/npm/api/doc/models/gateway` | `source.go: Provider` |
| `Model` 结构解析哪些键 | 只有 `id/name/description/gateway` | `source.go: Model`（**`cost`/`limit` 当前不解析**） |
| 模型级 gateway 全零的后果 | 该模型被跳过，不入库 | `source.go: toChannelProtocol` 的 `continue` 条件 |
| 新增 `gateway.costUnit` / `gateway.promotions` 是否报错 | 不报错，且 0.1.0 不生效 | 结构体无该字段 → 被忽略 |
| 空数组 `promotions: []` 是否安全 | 安全 | 同上 |

**结论：0.1.0 可以冻结当前 schema 并在文档里写进这两个字段位（留空），0.2.0 再补消费逻辑（价格展示 + 积分换算 + 优惠展示）。不阻塞今天的数据集发布。**

> 给兰台的一条硬要求：数据集里**只要出现 `cost`，就必须同时出现 `gateway.costUnit`**（provider 级或模型级），否则消费方会按 USD 解释。这条要进校验规则。

---

## 3. 校验规则（给兰台冻结 JSON Schema 用）

必填 / 条件必填：

1. 顶层 `{ "<providerId>": Provider }`；`Provider.id` 缺省时用 map 键兜底（代码已有兜底，但建议强制写）。
2. `Provider`：`id`、`name`、`models`、`gateway.baseUrl`、`gateway.protocols` 必填；`protocols` 键必须是基础协议名（`openai-chat` / `openai-responses` / `anthropic-messages`）。
3. `Model`：`id`、`name`、`description` 必填（对齐 v1）；`cost` 若出现 → `gateway.costUnit` 必填（provider 或模型级任一）。
4. 已有 `cost` 的模型：数值 ≥ 0，单位百万 token；`tiers` 出现时忽略 `context_over_200k`（legacy）。
5. 模型级 `gateway` 若只写 `promotions` 不写 `protocols`/`baseUrl` → **允许但不生效**，Schema 层给 warning 而非 error（否则数据集没法先摆位）。
6. `promotions[].type` 开放枚举；`endsAt` 格式 `YYYY-MM-DD`；`price` 若填必须 ≥ 0 且 ≤ `originalPrice`。
7. 日期允许 `YYYY-MM` 或 `YYYY-MM-DD`（v1 两种都产）。
8. 未知键一律允许（不设 strict），但 CI 里用一份「已知键清单」出 diff 报告，防止拼写错误（如 `costUnit` 写成 `cost_unit`）。

---

## 4. 拉取验收清单（勾选表，给测试/兰台用）

前置：`preset.pack_url` 指向数据集 URL（HTTPS、200、`application/json`）。

| # | 检查项 | 期望 | 通过 |
|---|---|---|---|
| 1 | `pack_url` 未配置 | sync 返回空结果、不报错、不写库 | ☐ |
| 2 | 首次 `POST /api/v1/admin/channel-protocols/sync {"confirm":false}` | 全部 `Applied`，`Pending` 为空 | ☐ |
| 3 | `{"confirm":true}` 再跑 | `Applied` = provider 条目数，库内新增同数量 | ☐ |
| 4 | 回读 `GET /api/v1/admin/channel-protocols` | `baseUrl` / `protocols` / `requestHeaders` 与数据集逐字段一致；协议键已归一（`openai-compat` → `openai-chat`，非基础协议被丢弃） | ☐ |
| 5 | 模型级 gateway（带 protocols 的） | 出现在协议的 `models` 覆盖里 | ☐ |
| 6 | 模型级 gateway 只写 `promotions` 的模型 | **不出现**（预期行为，非 bug） | ☐ |
| 7 | 幂等：再跑一次 sync（数据未变） | `Applied` = 0 或仅刷新、`Pending` 空 | ☐ |
| 8 | 数据集删除一个 provider 后 sync | 该协议变 `STALE`，**不删除** | ☐ |
| 9 | 人工编辑过的协议（`edited=true`） | `confirm:false` 时只进 `Pending` 并带 diff；`confirm:true` 才覆盖 | ☐ |
| 10 | 数据集带 `costUnit` / `promotions` 新字段 | sync 成功，0.1.0 不落这两个字段（预期） | ☐ |
| 11 | 非法输入：非 200 / 非法 JSON / 缺 `baseUrl` | 报错且**不改库**（事务回滚） | ☐ |
| 12 | 每条 provider 的 `cost` 都有 `costUnit` | 校验通过（第 3 节规则 3） | ☐ |

---

## 5. 待确认（需要 spencer / 架构拍板）

1. **1 积分 = 多少钱**？上面换算用的是示例值（¥0.01 / ¥0.05）。这个参数是数据集口径的锚，必须固定下来写进 `docs/preset-pack-format.md`。
2. `costUnit` 枚举是否就叫 `CNY_PER_MTOK` / `USD_PER_MTOK`？（名字定了就别改，兰台照着冻结）
3. `promotions` 是否 0.2.0 就上展示，还是先只进库不露？若上展示，前端要明确「以官方为准 + as_of 时间」，避免用户拿过期活动价投诉。
