# 渠道预设包数据集 · 交付说明（v0.2 草案 · 2026-10-28）

> 交付人：Ark（架构）｜派工：hugo（夜班 P0-①，仅文档，不改代码不部署）
> 正本：`docs/preset-pack-format.md`（字段口径）＋ `docs/preset-pack-format.schema.json`（JSON Schema 参考实现）
> 本文档 = 四条结论的落地说明 + 兼容性验证证据 + 拉取验收勾选表。

---

## 1. 四条结论（一句话版）

| # | 交付件 | 结论 |
|---|---|---|
| 1 | 数据集 schema | v0.2 草案定稿，四件套齐：字段表（含**必填性 / 灵枢是否消费**两列）、枚举容忍表、校验规则（必失败 V1–V7 / 静默出问题 S1–S7）、JSON Schema 参考实现。**兰台可直接冻结**。 |
| 2 | 定价口径 | 数据集价格 = **人民币元/百万 token 倍率**；真正计费的载体是 `models.<m>.gateway.costRates`（**不是** v1 的 `cost`）；币种用 `gateway.costCurrency: "CNY"` 标注，**缺省按 USD**（保证无损读 models.dev 原包），数据集层强制 CNY。**不走 `billing.cny_rate`**。 |
| 3 | 新字段兼容性 | **兼容**。灵枢全仓 0 处 `DisallowUnknownFields`，未知字段静默忽略。`offers` 放 `gateway.offers`，**0.1.0 先摆字段位留空 `[]`**，无需任何灵枢改造。 |
| 4 | 验收清单 | 见第 4 节勾选表（12 项，含 2 个负向用例）。今晚只写不执行。 |

---

## 2. 定价口径：币种怎么标才不破坏格式、灵枢读的时候不报错

### 2.1 先纠正一个前提（重要，但不推翻 spencer 的结论）

「数据集的 `cost` 语义 = 人民币倍率」这条**方向对，但 `cost` 不是灵枢的计费入口**：

- v1 的 `models.<m>.cost` 在灵枢里**根本没有对应结构体字段**（`Model` 结构体只有 `ID / Name / Description / Gateway`）。灵枢不解析它、不入库、不影响计费。
- 灵枢真正的成本入口是 `models.<m>.gateway.costRates`（三段 `Input/Output/CacheRead`），落库到 `channel_models.cost_rates`，再由 `billing.ComputeCredits` 直接算积分。

⇒ 所以「人民币落库、不走汇率」这件事，靠 `costRates` 就已经天然成立，**不需要动 `cost` 的形状，也不需要任何代码改造**。

### 2.2 币种标注：唯一推荐方案

```jsonc
"gateway": { "costCurrency": "CNY" }
```

| 方案 | 取舍 | 结论 |
|---|---|---|
| A. 在 `cost` 对象内加 `currency` 子键 | 形状仍在 v1 内，但 `cost` 是 models.dev 的封闭数字块，塞子键语义外挂、未来 v1 加同名字段就撞车 | **不采纳** |
| B. 顶层加 `currency` | 污染 v1 顶层形状，直接违反「扩展只进 gateway」原则 | **不采纳** |
| C. 把 `cost` 改成字符串 `"0.8 CNY"` | 破坏 v1 数字类型，模型消费方必崩 | **不采纳** |
| **D. `gateway.costCurrency`（provider 级，模型级可覆盖）** | v1 字段一个字节不动；扩展在自有命名空间；缺省 USD 保证仍能无损读 `api.json` | **✅ 唯一推荐** |

解析路径为什么不会报错（三重保险）：

1. `costCurrency` 不在 `GatewayBlock` 结构体里 → 裸 `json.Decoder` 静默忽略（无 `DisallowUnknownFields`）；
2. `cost` 的币种变化对灵枢**零影响**，因为它不解析 `cost`；
3. `costRates` 走 `WireRates` 反序列化，只认三段键、未知键保留透传、Pascal/snake 都接受。

### 2.3 倍率 → 积分（兰台可直接抄的算式）

```
积分 = (输入token × Input + 输出token × Output + 缓存读token × CacheRead) × 时段系数 × 分档系数 ÷ R
R = billing.r（默认 10000）

⇒ 每 1 积分 = 0.01 元
⇒ 每百万 token 积分 = 倍率 × 100
```

`billing.cny_rate`（默认 6.8）**不参与**这条链路，它只服务 models.dev 美元参考价 → 售价倍率那条运营辅助路径。

### 2.4 样例：`glm-5.3-flash`（官网人民币价）

来源：智谱 BigModel 开放平台 `https://open.bigmodel.cn/pricing`，as_of **2026-10-28**

| 项 | 官网人民币价（元/百万 token） | `costRates` | 每百万 token 积分 |
|---|---|---|---|
| 输入 | 0.8 | `Input: 0.8` | **80** |
| 输出 | 2.8 | `Output: 2.8` | **280** |
| 缓存命中 | 0.23 | `CacheRead: 0.23` | **23** |

算式与交叉校验：

- 100 万输入 token → `1,000,000 × 0.8 ÷ 10000 = 80` 积分
- 一次调用（10 万输入 + 1 万输出、无缓存）→ `(100000 × 0.8 + 10000 × 2.8) ÷ 10000 = 108000 ÷ 10000 =` **10.8 积分**
- 反算金额：`10.8 × 0.01 = 0.108 元`，按量价 `0.08 + 0.028 = 0.108 元` ✓ 完全一致

参考：该模型限时五折（2026-08-26 起，至 2026-09-09 24:00 UTC+8，**已结束**）= 0.4 / 1.4 / 0.115 → 40 / 140 / 11.5 积分每百万 token。正好是 `offers` 字段的写法示例。

### 2.5 一条硬红线

数据集 URL **只能**挂 `preset.pack_url`，**绝不能**挂 `sync.price_source_url`：

| 配置项 | 价格语义 | 是否折算 |
|---|---|---|
| `preset.pack_url` | 人民币倍率 | 否 |
| `sync.price_source_url` | **美元**/百万 token | 是，× `cny_rate`（6.8）|

挂错 = 人民币数字被当美元、**放大 6.8 倍**，且会经「一键应用参考价」污染对外模型售价。必须写进兰台的发布纪律。

---

## 3. 新字段兼容性验证（`offers`）

### 3.1 结论

**兼容 → `offers` 在 0.1.0 先摆字段位（`"offers": []` 留空），0.2.0 再填内容，灵枢零改造。**

### 3.2 证据（代码位置，均为本次实读）

| # | 事实 | 位置 |
|---|---|---|
| E1 | 拉包解码为裸 `json.NewDecoder(...).Decode(&pack)`，**未开严格模式** | `backend/internal/domain/channelprotocol/source.go` · `fetchPack` |
| E2 | import / sync 的请求体解码同样无严格模式 | `backend/internal/domain/channelprotocol/handler.go` · `decodeBody` |
| E3 | `Model` 结构体只有 4 个字段，连 v1 的 `cost/limit` 都没有 → 「惰性字段」是既有惯例 | `backend/internal/domain/channelprotocol/source.go` · `type Model` |
| E4 | `GatewayBlock` 只有 `baseUrl/protocols/requestHeaders/edited/sourceStatus` → `offers` 进不去 | 同上 · `type GatewayBlock` |
| E5 | 全仓 grep `DisallowUnknownFields` = **0 命中** | `backend/` 全量 |
| E6 | `ValidateRates` 只校验三段，多余键「不参与校验、不影响计费」 | `backend/internal/domain/billing/rates.go` |

### 3.3 为什么「不兼容」不成立

唯一的严格校验是物化**之后**的 `validate()`（拒绝非法 `passthroughMode` / 非法 `sourceStatus` / 多个 `default`）。这些只作用于**已被结构体接住的字段**；`offers` 到不了 `validate()`，那条报错路径不存在。

### 3.4 命名与位置

- **位置**：`gateway.offers`（provider 级 = 通用优惠；模型级 = 模型专属优惠）。
  - 为什么不是顶层 `offers`：虽然一样不会报错（同为未知键），但会与 models.dev 未来字段撞名，违反「扩展只进 gateway」。
  - 为什么不是 `discounts`：还包含免费额度与限量抢购，不都是"打折"。
  - 为什么是数组：首购价 + 限时活动 + 免费额度可**同时存在**，需要并列表达；合并语义定为**拼接**（provider 在前、模型在后），不做覆盖——覆盖会丢事实。
- **必须带 `label` + `source` + `asOf`**：优惠是时效性极强的商业信息，没有来源与采信日就无法审计。这条对齐 spencer「拿不到就留空标待定，不许猜」的要求。

### 3.5 需要提前说清的语义

`offers` 目前**没有消费方**（灵枢只当噪声丢掉）。它的价值是「先把契约冻结、兰台先攒数据」。若将来要让优惠价真正生效，正确做法是**消费方在拉取后按 `window` + `effect.priceOverride` 覆盖 `costRates`**，而不是让数据集对同一模型出现两个价。这属于 0.2.0 之后的独立设计，今晚不做。

### 3.6 Schema 自检结果（已实跑，不是纸面结论）

用 `ajv 8`（draft 2020-12）编译 `docs/preset-pack-format.schema.json` 并跑 7 个用例，全部符合预期：

| 用例 | 期望 | 实跑 |
|---|---|---|
| 合格数据集（含 `offers` 两条 + PascalCase `costRates`） | 通过 | ✅ 通过 |
| 合格数据集（`costRates` 用 snake 键） | 通过 | ✅ 通过 |
| 加未知字段 `gateway["x-probe"]` + 模型级未知字段 | 通过（容忍） | ✅ 通过 |
| `costCurrency: "JPY"` | 拒绝 | ✅ 拒绝 |
| `costRates` 只有 `Input`（缺两段） | 拒绝 | ✅ 拒绝 |
| `passthroughMode: "bogus"` | 拒绝 | ✅ 拒绝 |
| 协议键拼错成 `openai_compat` | 拒绝 | ✅ 拒绝 |

> 注意：第 3 条验证的是**兰台侧 schema 的容忍度**（未知字段不拦），与第 3.1 节的灵枢侧结论（未知字段不报错）互为佐证，但两者是不同层面的验证。


---

## 4. 拉取验收清单（勾选表 · 今晚只写不执行）

**执行前置**：等兰台发布数据集 → hugo 通知后，在**测试环境**执行。
**纪律**：全程只走管理端 HTTP 接口，**不直连数据库、不调灵枢库/网关数据**（spencer 明确「不准」）；遇 429/503/超时按 5s→20s→60s 指数退避，最多 3 次。

### 4.1 准备

| ☐ | 检查项 | 预期值 | 失败判定 |
|---|---|---|---|
| ☐ | `preset.pack_url` 已指向数据集 URL，且**不等于** `sync.price_source_url` | 两个 URL 不同 | 相同 = 红线违规，停止验收 |
| ☐ | 数据集中已写入 `gateway.costCurrency: "CNY"` | 每个 provider 都有 | 缺失 = 币种歧义，退回兰台 |
| ☐ | 数据集能被 `GET` 到且返回 200 + `application/json` | 200 | 非 200 / HTML 错误页 = 阻断 |
| ☐ | 用 `docs/preset-pack-format.schema.json` 校验数据集 | 0 error | 有 error = 退回兰台修 |

### 4.2 同步调用

| ☐ | 检查项 | 预期值 | 失败判定 |
|---|---|---|---|
| ☐ | `POST /api/v1/admin/channel-protocols/sync`，body **必须显式** `{"confirm":true}` | `Code=0` | 非 0 → 记录 `Message`，对照文档第 11 节 V1–V7 |
| ☐ | 响应 `Data.Applied` | = 数据集 provider 数（首次） | 少于预期 = 有 provider 被静默丢弃 |
| ☐ | 响应 `Data.Pending` | 首次应为空数组 | 非空 = 有单据被判定"已编辑"，需人工确认 |

### 4.3 逐字段核对（`GET /api/v1/admin/channel-protocols` + `GET /{id}`）

| ☐ | 检查项 | 预期值 | 失败判定 |
|---|---|---|---|
| ☐ | 协议条数 | = 数据集 provider 数 | 不等 = 见 V3/V6 |
| ☐ | `PresetSourceID` | = 数据集 provider `id` | 空 = 物化键错 |
| ☐ | `Name` | = 数据集 `name`（缺省时 = `id`） | 不一致 = 字段映射错 |
| ☐ | `BaseURL` | = `gateway.baseUrl` | 不一致 = 字段名拼错（大小写） |
| ☐ | `Protocols` 键集合 | ⊆ {openai-chat, openai-responses, anthropic-messages} 且与数据集交集一致 | 多出键 = 白名单过滤异常；缺失 = 键名拼错 |
| ☐ | `Models` 键集合 | = 数据集中**带 `protocols`** 的模型（无协议的会被丢弃，属预期） | 有价格模型未出现 = 触发 S1 |
| ☐ | 每模型 `costRates` 三段 | 与数据集**数值逐位一致**，且三段齐全 | 缺键 = 触发 S2（计费静默失效） |
| ☐ | `costRates` 数值 > 0（除非官网确实免费） | 与官网人民币价同值 | 出现 6.8 的倍数关系 = 挂错 URL 或用了美元价 |
| ☐ | 模型 `protocols` | = 数据集模型级 `protocols` | 不一致 = 继承逻辑异常 |

### 4.4 幂等与边界（必做，两项负向用例）

| ☐ | 检查项 | 预期值 | 失败判定 |
|---|---|---|---|
| ☐ | 不改数据集，再 `sync {"confirm":true}` 一次 | `Applied` = 协议数、`Pending` 空、**协议 ID 不变** | 出现新增 = 幂等键（`preset_source_id`）失效 |
| ☐ | **正向兼容用例**：数据集某 provider 的 `gateway` 里加一个未知字段 `"x-probe": 1`，再 sync | **依然 `Code=0`**，其余结果不变 | 报错 = 兼容性结论（第 3 节）被推翻，**立即上报** |
| ☐ | **负向用例**：把某 provider 的 `passthroughMode` 改成 `"bogus"`，再 sync | 400 + 整次回滚（**其他协议的 `UpdatedAt` 不变**） | 报了 200 = 校验缺失；部分协议被改 = 事务原子性破损 |
| ☐ | 从数据集删掉一个 provider，再 sync | 该协议 `SourceStatus=STALE` 且**未被删除** | 协议消失 = 违反「不删除，只标记失效」 |
| ☐ | 恢复数据集原状（去掉 probe、改回 passthroughMode、补回 provider），再 sync | 全部回到基准值 | 残留脏数据 = 回滚不彻底 |

### 4.5 价格真正生效的链路（注意：需人工一步）

数据集的 `costRates` 只到「渠道协议」这一层。要落到 `channel_models.cost_rates`：

| ☐ | 步骤 | 预期值 | 失败判定 |
|---|---|---|---|
| ☐ | 在某渠道协议的模型上绑定对外模型（`externalModelID`）并保存 | 保存成功 | 400「cost_rates 非法」= 触发 S2 |
| ☐ | 该渠道的模型列表里成本倍率可读 | 与协议里 `costRates` 一致 | 不一致 = 重物化未触发 |

> 原因：`externalModelID` 是灵枢内部雪花 ID，**兰台不可能知道**，所以数据集无法自动完成「价格落库」的最后一步，必须由管理员在界面上绑定对外模型。这是当前架构的既定限制，不是数据集缺陷。

---

## 5. 给兰台的落地清单（照做即可）

1. 按 `docs/preset-pack-format.md` 第 4/5/6 节建数据集；每 provider 必带 `gateway{baseUrl, protocols, costCurrency:"CNY", offers:[]}`。
2. 价格填`schema` 里的 `gateway.costRates`（PascalCase，三段齐全，人民币元/百万 token）；`cost` 同值填一份用于展示。
3. **每个带价格的模型必须至少声明一个 `protocols` 键**，否则整条会被灵枢丢弃（数据白给）。
4. `offers` 本轮一律 `[]`；0.2.0 再按第 6.6 节填（首购价 / 限时活动 / 续费价 / 邀请折扣 / 免费额度 / 限量抢购）。
5. 用 `docs/preset-pack-format.schema.json` 自检通过后再发布；发布 URL 只给 `preset.pack_url`。
6. 每条优惠 / 每个价格带 `source` + `asOf`；拿不到的留空，**不许猜**。

---

## 6. 未做 / 待办

| 项 | 说明 |
|---|---|
| 步骤 3 真实拉取验收 | 等兰台发布数据集 + hugo 通知后开，见第 4 节 |
| `offers` 消费实现 | 0.2.0 及以后，需单开设计（谁在什么时机用优惠价覆盖 costRates） |
| P0-② 0.2.0 材料收集（15 plan 渠道 + OpenCode 4） | 不在本次派工范围（本次派工明确「仅 P0-①」）。若需要，我按「官方公式优先 / 第三方实测次之 / 拿不到标待定」的纪律接 |
| P1 成本页多渠道对比 | 排队中，今晚未开 |
