# 渠道预设包 0.2.0 · 定价草案（不发布，供兰台替换 0.1.0 plan 渠道占位价）

> 状态：**草案，只写文档**。不改代码、不发布、不建 PR。
> 格式正本：`docs/preset-pack-format.md` v0.2（PR #64）。字段名/语义一律照正本，本文**不自创字段**。
> **格式结论以 Ark 的 `docs/preset-pack-pricing-boundaries`（HEAD `ab228f2`，未建 PR）为准**：G1 拆 providerId、G2 分段价取高档、G3 缓存价显式填。本文已按三条展开。
> 数据来源：本仓 `notes/plan-rate-sources-2026-10-28.md` / `-supplement.md`（1–11 节）已定稿材料，本轮未重新抓取。
> as_of：除单独标注外均为 2026-10-30（抓取日），页面更新日随条目注出。
>
> 修订记录：本版（第二版）按 Ark 三条结论重排 providerId、分段价改为取高档、火山逐行补 `CacheRead`；并修正上一版两处理解写反（§8.1 高估/低估、§8.2 `costCurrency` 措辞）。

---

## 0. 三个前提结论（决定这份草案能落多少）

1. **能落 `costRates` 的**：智谱（API 一手价 7 条）、讯飞（1 积分 = ¥0.01，12 条）、腾讯「分 token 类型计价」9 条 + 「按档位统一计价」3 条 × 4 档、火山 Agent Plan 21 行 × 2 档。
2. **进不了 `costRates` 的两条（留空、标不可用）**：
   - **阿里**：官方从不公布「模型 → Credits」系数，只有 qwen3.6-plus 一个算例（`Credits ≈ 刊例价×100` 是我方推算），套餐是 Credits 制 → 只能给套餐均价。
   - **OpenCode**：官方价是**美元**（Console 按量 USD/百万；Go 是月度美元额度）→ 与「数据集只有人民币、不含汇率」口径冲突，**不换算、不进表**。
3. **档位怎么表达**：按 Ark **G1 —— 拆 providerId**（不是方案 B）。详见 §6。

---

## 1. 格式约束（兰台照抄，别扩展）

| 约束 | 依据 |
|---|---|
| 价格只写 `models.<key>.gateway.costRates`，三段 `Input` / `Output` / `CacheRead` | 正本 §9.1 |
| 数值 = **人民币元/百万 token 字面量**（照抄官网人民币价，不乘不除） | 正本 §9.1 |
| `gateway.costCurrency: "CNY"`（provider 级） | 正本 §6.5 |
| 缺任一段 → 不报错但**成本积分静默记 0**（S2） | 正本 §11.2 |
| 模型条目**内联完整对象**；`externalModelID` 必须字符串且兰台**不要填** | 正本 §0.1 F2 / §12.3 |
| `gateway.offers: []` 先留空（折扣清单见 §5） | 正本 §6.6 |
| **只能挂 `preset.pack_url`**，挂 `sync.price_source_url` 会被当美元 × 6.8 | 正本 §9.4 |
| 档位 = 不同 providerId；**模型 key / `id` 不许加档位后缀** | Ark G1 |

---

## 2. providerId 清单（G1 落地的产物，共 8 条）

| providerId | 产品线 / 档位 | 说明 |
|---|---|---|
| `volcengine-agentplan-personal` | 火山 Agent Plan 个人版 | 元/百万 = 系数 × **0.2** |
| `volcengine-agentplan-enterprise` | 火山 Agent Plan 企业版 | 元/百万 = 系数 × **0.3** |
| `tencent-tokenplan-lite` | 腾讯 Token Plan Lite | 同一供应商、不同订阅与凭据 → 不同渠道 |
| `tencent-tokenplan-standard` | 腾讯 Token Plan Standard | |
| `tencent-tokenplan-pro` | 腾讯 Token Plan Pro | |
| `tencent-tokenplan-max` | 腾讯 Token Plan Max | |
| `zhipu-bigmodel` | 智谱 BigModel（API 刊例价，单一价） | 无档位维度 |
| `xfyun-astron-tokenplan` | 讯飞 Astron Token Plan（积分制，1 积分 = ¥0.01 固定） | 积分价不随档位变 → 单条 |

不出 provider：**阿里**（不可用）、**OpenCode**（美元口径）。命名均满足正本 `^[a-z0-9][a-z0-9._-]{0,63}$`。
备查：若将来要走「单包内多档价」，需新增 `tierRates` 一类字段——**已登记为待确认代码遗留，0.2.0 不做**（Ark 结论）。

---

## 3. 火山方舟 Agent Plan（两条 provider，各 21 行；`CacheRead` 逐行显式填 = Input）

**锚点（三处互证）**：个人版四档 40/20,000 = 200/100,000 = 500/250,000 = 1,000/500,000 = **¥0.002/AFP**；企业版四档 120/40,000 = … = **¥0.003/AFP**（价格 3 倍、额度 2 倍 ⇒ 单价 1.5 倍，已核非笔误）。
**协议与 baseUrl（2026-10-30 更新，官方一手）**：官方「快速开始」页全文只有一个 URL `https://ark.cn-beijing.volces.com/api/plan`（原文：专属 Base URL，其他 baseUrl 不可用），**只公布 Anthropic 形态** ⇒ 两条 provider 与全部模型条目的 `protocols` **只写 `anthropic-messages`**，`openai-chat`/`openai-responses` 在拿到官方证据前**不得补**。详见 `notes/preset-pack-0.2.0-lanta-handoff.md` 顶部警示。
**换算**：`AFP/百万 = 100 × 系数`；个人 `元/百万 = 系数 × 0.2`；企业 `= 系数 × 0.3`。
**G3**：官方只公布输入=输出系数，**未公布缓存系数** → `CacheRead` 一律**显式填 `Input` 同价**，标注「官方未公布缓存系数，按输入价保守计入」。理由见 §8.1（这是**高估**、即保守方向；不填会静默记 0）。

| # | 模型（上游 ID） | 系数 | AFP/百万 | 个人 Input | 个人 Output | 个人 CacheRead | 企业 Input | 企业 Output | 企业 CacheRead |
|---|---|---|---|---|---|---|---|---|---|
| 1 | `doubao-seed-2.0-mini` | 0.25 | 25 | 0.05 | 0.05 | 0.05※ | 0.075 | 0.075 | 0.075※ |
| 2 | `doubao-seed-2.0-mini`（含音频） | 2.5 | 250 | 0.50 | 0.50 | 0.50※ | 0.75 | 0.75 | 0.75※ |
| 3 | `doubao-seed-2.0-lite` ⚠ | 0.5 | 50 | 0.10 | 0.10 | 0.10※ | 0.15 | 0.15 | 0.15※ |
| 4 | `doubao-seed-2.1-lite` | 0.5 | 50 | 0.10 | 0.10 | 0.10※ | 0.15 | 0.15 | 0.15※ |
| 5 | `doubao-seed-2.1-lite`（含音频） | 4.5 | 450 | 0.90 | 0.90 | 0.90※ | 1.35 | 1.35 | 1.35※ |
| 6 | `doubao-embedding-vision` | 0.5 | 50 | 0.10 | 0.10 | 0.10※ | 0.15 | 0.15 | 0.15※ |
| 7 | `deepseek-v4-flash` | 0.5 | 50 | 0.10 | 0.10 | 0.10※ | 0.15 | 0.15 | 0.15※ |
| 8 | `glm-5.3-flash` | 0.5 | 50 | 0.10 | 0.10 | 0.10※ | 0.15 | 0.15 | 0.15※ |
| 9 | `deepseek-v4.1-flash-economy` | 2 | 200 | 0.40 | 0.40 | 0.40※ | 0.60 | 0.60 | 0.60※ |
| 10 | `deepseek-v4.1-flash` | 2.5 | 250 | 0.50 | 0.50 | 0.50※ | 0.75 | 0.75 | 0.75※ |
| 11 | `doubao-seed-2.1-turbo` | 2.5 | 250 | 0.50 | 0.50 | 0.50※ | 0.75 | 0.75 | 0.75※ |
| 12 | `doubao-seed-2.1-pro` | 2.5 | 250 | 0.50 | 0.50 | 0.50※ | 0.75 | 0.75 | 0.75※ |
| 13 | `doubao-seed-evolving` | 2.5 | 250 | 0.50 | 0.50 | 0.50※ | 0.75 | 0.75 | 0.75※ |
| 14 | `minimax-m3` | 2.5 | 250 | 0.50 | 0.50 | 0.50※ | 0.75 | 0.75 | 0.75※ |
| 15 | `glm-5.2`（将下线） | 4.5 | 450 | 0.90 | 0.90 | 0.90※ | 1.35 | 1.35 | 1.35※ |
| 16 | `glm-5.3` | 4.5 | 450 | 0.90 | 0.90 | 0.90※ | 1.35 | 1.35 | 1.35※ |
| 17 | `kimi-k2.7-code`（即将下线） | 4.5 | 450 | 0.90 | 0.90 | 0.90※ | 1.35 | 1.35 | 1.35※ |
| 18 | `deepseek-v4-pro` | 5.5 | 550 | 1.10 | 1.10 | 1.10※ | 1.65 | 1.65 | 1.65※ |
| 19 | `kimi-k3.1-pro` | 7 | 700 | 1.40 | 1.40 | 1.40※ | 2.10 | 2.10 | 2.10※ |
| 20 | `kimi-k2.8-preview` | 8 | 800 | 1.60 | 1.60 | 1.60※ | 2.40 | 2.40 | 2.40※ |
| 21 | `kimi-k3` | 10 | 1,000 | 2.00 | 2.00 | 2.00※ | 3.00 | 3.00 | 3.00※ |

※ = **G3 占位**：官方未公布缓存系数，按输入价保守计入（高估成本）。溯源 S1 / S1'。
算式示例：`glm-5.3` 系数 4.5 → AFP `4.5×100 = 450` → 个人 `4.5×0.2 = ¥0.90` → 企业 `4.5×0.3 = ¥1.35`。
不进 `costRates` 的非文本口径：视频 `token/10,000×系数`（仅 Large/Max）；图片 `张数×系数`；TTS `字符/10,000×系数`；ASR `小时×系数`；**Agent 进化 250 AFP/百万 = 个人 ¥0.50**（等价系数 2.5）；豆包搜索 5 AFP/次、专业数据集 12–240 AFP/次、Agent 记忆 25 AFP/小时。

---

## 4. 腾讯云 Token Plan（20 积分 = 1 元 → 元/百万 = 积分 ÷ 20；四条 provider：lite / standard / pro / max）

### 4.1 分 token 类型计价（9 条，**四档同价 → 四条 provider 内容完全相同**）

| 模型 | Input | Output | CacheRead | 溯源 |
|---|---|---|---|---|
| `deepseek/deepseek-flash`（V4.1-Flash-1017 原厂直供） | 2 | 8 | 0.04 | S2 |
| `hy4-preview` | 6 | 18 | 0.30 | S2 |
| `glm-5.3-flash` | 0.8 | 2.8 | 0.23 | S2 |
| `glm-5.3` / `glm-5-3` | 8 | 28 | 2 | S2 |
| `kimi-k3` | 20 | 100 | 2 | S2 |
| `kimi-k2.7-code` | 6.5 | 27 | 1.3 | S2 |
| `minimax-m3`（**G2：分段价取高档 >512k**） | 4.2 | 16.8 | 0.84 | S2（官方分段，取高档） |
| `mimo-v2.6-flash` | 1 | 2 | 0.02 | S2 |
| Hy：`hy3-202608` | 1 | 4 | 0.25 | S2 |

> G2 说明：官方 `minimax-m3` 分 ≤512k / >512k 两档（2.1/8.4/0.42 与 4.2/16.8/0.84），`costRates` 无分段维度 → **只填高档**，溯源列已注明。不填会记 0，比高估更糟。

### 4.2 按档位统一计价（3 条，**四档价不同 → 四张 provider 各一套值**）

输入 = 输出 = 缓存命中（统一计价）。

| 模型 | `-lite` | `-standard` | `-pro` | `-max` | 溯源 |
|---|---|---|---|---|---|
| `tc-code-latest`（Auto） | 1.1143（22.285 积分） | 0.99（19.8） | 0.9344（18.687） | 0.9215（18.43） | S2 |
| `deepseek-v4-flash-202605` 等（V4-Flash 原厂直供） | 1.1143 | 0.99 | 0.9344 | 0.9215 | S2 |
| Hy：`hy3` / `hy3-preview` | 0.80（16） | 0.78（15.6） | 0.7438（14.875） | 0.72（14.4） | S2 |

| 不可用（官方表格该行为空，无在售价） | 处置 |
|---|---|
| `deepseek-v4-pro-202606`、`minimax-m2.7`、`glm-5.2` | 四条 provider 里都**不填这三个模型**，标「官方无在售价」 |

校验锚：官方称 Lite ¥39 单跑 `glm-5.3` ≈ 1,427 万 token ⇒ 混合 ≈ ¥2.73/百万（刊例 8/28，差额来自缓存命中）。

---

## 5. 其余渠道

### 5.1 智谱 BigModel（`zhipu-bigmodel`，7 条，API 一手价）

| 模型 | Input | Output | CacheRead | 溯源 |
|---|---|---|---|---|
| `glm-5.3` | 8 | 28 | 2 | S3 |
| `glm-5.3-flash` | 0.8 | 2.8 | 0.23 | S3 |
| `glm-5.3-flashx` | 2 | 7 | 0.57 | S3 |
| `glm-5.2` | 8 | 28 | 2 | S3 |
| `glm-5`（**G2 取高档 ≥32K**） | 6 | 22 | 1.5 | S3（官方分段，取高档） |
| `glm-5-turbo`（**G2 取高档 ≥32K**） | 7 | 26 | 1.8 | S3（官方分段，取高档） |
| `glm-5.1`（**G2 取高档 ≥32K**） | 8 | 28 | 2 | S3（官方分段，取高档） |

> G2 依据：`contextTiers` 的 `TierRule.Coeff` 是**单一系数**且乘在 `Input+Output+CacheRead` 求和之后（`billing/engine.go:15-27`）。`glm-5.1` 输入比 1.333、输出比 1.167 **比例不一致**，用单系数必错一侧 → 一律取高档单值。**只有三段比例完全一致时**才考虑 `contextTiers`；精确保留分段矩阵已登记遗留。

### 5.2 讯飞星辰 Astron Token Plan（`xfyun-astron-tokenplan`，12 条；元/百万 = 积分 ÷ 100）

| 模型 | Input | Output | CacheRead | 溯源 |
|---|---|---|---|---|
| `Spark-X2.5` | 3.20 | 12.00 | 0.48 | S4 |
| `Spark-X2-Flash` | 1.00 | 2.00 | 0.20 | S4 |
| `GLM-5.2` | 8.00 | 28.00 | 2.00 | S4 |
| `GLM-5.1` | 8.00 | 28.00 | 1.60 | S4 |
| `GLM-5` | 6.00 | 22.00 | 1.20 | S4 |
| `DeepSeek-V4-Pro` | 12.00 | 24.00 | 2.40 | S4 |
| `DeepSeek-V4-Flash` | 1.00 | 2.00 | 0.20 | S4 |
| `DeepSeek-V3.2` | 2.00 | 3.00 | 0.40 | S4 |
| `Kimi-K2.6` | 6.50 | 27.00 | 1.30 | S4 |
| `Kimi-K2.5` | 4.00 | 21.00 | 0.80 | S4 |
| `MiniMax-M2.5` | 2.10 | 8.40 | 0.42 | S4 |
| `Qwen3.5-397B-A17B` | 1.20 | 7.20 | 0.24 | S4 |

算式：`GLM-5.2 输出 2,800 积分 ÷ 100 = ¥28.00`（与智谱 API 输出价 ¥28 吻合，佐证 1 积分 = ¥0.01）。
官方「思考」积分与 Output 同价 → `costRates` 三段已足够，无需额外字段。

### 5.3 阿里云百炼：**不进数据集**

| 项 | 结论 |
|---|---|
| Credits 系数 | 官方不公布；仅 qwen3.6-plus 一例，我方反推 `输入 200 / 缓存 40 / 输出 ~1,204 Credits/百万` 与 `Credits ≈ 刊例 × 100`（三点交叉） |
| 为何不能进 | `costRates` 要「人民币元/百万」确定值；阿里只有套餐均价（1 Credit ≈ ¥0.00277–0.00339，随档位变）且系数是推算 |
| 处置 | 数据集不出 provider；外部文档标「Credits 制，系数未公布，不可用于成本计算」 |
| 可留作对比页 | 个人版 2.8–3.4 折、团队版 5.6–7.9 折（`Credits 单价 ÷ ¥0.01`） |

### 5.4 OpenCode：**不进数据集（币种冲突）**

| 项 | 结论 |
|---|---|
| 档位 | 2 档 × 2 协议，同档共用一套价，不当 4 个套餐 |
| 价格形态 | Console 按量 = USD/百万；Go/Go Plus = $10/$40 + 按模型月度美元额度（GLM-5.3 $15、Kimi K3 $15、GLM-5.3-Flash $60） |
| 处置 | 数据集只收人民币 → 留空；如需人民币价必须乘汇率，违反口径 |
| baseUrl（正本修正后） | Zen：`https://opencode.ai/inference`（协议级 `…/inference/openai/v1`、`…/inference/anthropic`）；Go：`…/inference/go`（协议级 `…/inference/go/openai/v1`、`…/inference/go/anthropic`）。旧 `/zen*` 四处处作废 |

---

## 6. 档位表达：按 Ark G1 拆 providerId（本版定稿）

- **做法**：产品线 × 档位 = 一条 providerId（见 §2），因为档位对应不同订阅与凭据，在灵枢里 providerId → 渠道协议 → 渠道本就是不同渠道。
- **硬约定**：模型 key / `id` **不许加档位后缀**（如 `glm-5.3@team`），否则打不中上游。
- **被否的方案**：只放 API 刊例价（方案 B）——与「plan 渠道实际价进数据集」目标冲突。
- **未做**：单包内多档价（需新增 `tierRates` 类字段）→ 登记为待确认代码遗留，0.2.0 不做。

---

## 7. 限时折扣清单（`offers` 一律 `[]`，先只记录）

| 渠道 | 折扣 | 期间 | 状态（as_of 2026-10-30） |
|---|---|---|---|
| 火山 Agent Plan | Auto 0.5× | 2026-06-10 ~ **11-08** | 进行中 |
| 火山 Agent Plan | `deepseek-v4.1-flash` 5 折 | 09-15 ~ 12-30 | 进行中 |
| 火山 Agent Plan | `deepseek-v4.1-flash-economy` 5 折 | 10-20 ~ 12-30 | 进行中 |
| 火山 Agent Plan | `kimi-k2.8-preview` 6 折 | 09-17 ~ 12-14 | 进行中 |
| 火山 Agent Plan | `kimi-k3` 5 折 | 10-12 ~ **10-25** | **已结束** |
| 火山 Agent Plan | `kimi-k3.1-pro` 5 折 | 10-24 ~ **11-09** | **进行中（剩 10 天）** |
| 腾讯 Token Plan | Auto 6 折 / K2.7-Code 8 折 / K3 7.5 折 / GLM-5.3 5.5 折 / Flash 7 折 | 2026-10-01 ~ **10-30 12:00** | **今天到期** |
| 阿里 Token Plan | 限时 39/79/139/499（原价 60/120/180/600） | 官方未标截止 | 进行中 |

处理：数据集先 `offers: []`；要进 UI 须 spencer 拍板（可用 `kind=time_limited` + `priceOverride` + `window.end`），且**必须显示截止日**。

---

## 8. 两处上一版写反/措辞不准（已改）

### 8.1 「按 `Input` 填缓存价」是**高估**成本，不是低估

上一版写成「会低估缓存成本（其他家缓存价通常是输入价 1/4~1/10）」——方向写反了：**真实缓存价更低，按输入价计就是把成本算高了，属保守方向**（高估 > 漏记）。
之所以仍要这么填：不填 `CacheRead` 会让成本**静默记 0**（正本 S2、`billing/service.go:300-302` 吞错），**记 0 比高估更糟**——高估只是偏保守，记 0 会让账单凭空变便宜、对账时无法解释。**结论不变：显式填 `Input` 同价并标注。**

### 8.2 `costCurrency` 缺省的后果：只影响可读性与校验，不影响计费

上一版写「缺了会被按 USD 理解（S5）」，容易被读成计费受影响。准确说法：**灵枢代码不读该字段（惰性），缺省只影响人读与 Schema 校验；不影响计费与路由**。写它的目的是消歧 + 让兰台的 Schema 能 `const: "CNY"` 卡住数据集口径。

---

## 9. 兰台落地注意事项（照做，无需再问）

1. **数值必须是人民币字面量**：`0.9` = ¥0.90/百万 token；别写 `9`、别写 `"0.9元"`、别再乘 100。
2. **三段齐全**：`Input`/`Output`/`CacheRead` 缺一段 → 不报错但成本积分静默 0。宁可先不填模型，也别填两段。
3. **火山 21 行 × 2 档共 42 个条目，`CacheRead` 全部显式填 = `Input`**，并带「官方未公布缓存系数，按输入价保守计入」备注。
4. **`costCurrency: "CNY"` 必写**（provider 级）：不影响计费，但影响可读性与 Schema 校验（§8.2）。
5. **别把数据集 URL 挂到 `sync.price_source_url`**（会被当美元 ×6.8 并污染售价），只挂 `preset.pack_url`。
6. **模型必须内联完整对象**；`externalModelID` 不要填且必须字符串。
7. **带价格的模型必须至少声明一个 `protocols` 键**，否则整条（含价格）被静默丢弃（S1）。
8. **先 `POST /api/v1/admin/channel-protocols/sync {"confirm":false}` 干跑**，`Pending` 为空再 `confirm:true`。
9. **四条腾讯 provider 的「分类型计价」9 条模型完全重复**：这是 G1 拆档位的必然结果，兰台按模板生成即可，别手工复制漏行。

---

## 10. 待定清单（照写，不许猜）

| # | 项 | 状态 | 已试过的来源 |
|---|---|---|---|
| 1 | 腾讯「其余模型」积分价 | ✅ 已关闭（§4 已用官方全表落表） | 官方积分抵扣规则页（2026-10-28 更新） |
| 2 | 火山 Coding Plan 逐模型倍率 | ❌ **官方未发布**（结案） | 活动页、文档目录、`coding-plan-personal-plan-overview`、`-model-discount`、`-model-release`、站内搜、36kr/知乎/掘金/博客园 |
| 3 | 火山档位 AFP 额度 | ✅ 已关闭（个人/企业双官方页 + 抵扣规则页三点互证） | `agent-plan-personal-plan-overview`、`agent-plan-enterprise-plan-overview`、`…-afp-credits-billing-rules` |
| 4 | **火山 `doubao-seed-2.0-lite` 系数** | ⚠ 唯一遗留：0.5（标准组）vs 2.5（进阶组），5 倍差 | 公告页分组原文；hugo 两次转述与原文冲突 → 需原始表格行 |
| 5 | 腾讯 `deepseek-v4-pro-202606` / `minimax-m2.7` / `glm-5.2` 积分价 | ⚠ 官方表格为空（无在售价） | 同上，该行空白 |
| 6 | 阿里 Credits 模型系数 | ❌ 官方不公布（推算不进数据集） | 官方个人/团队版文档、FAQ、开发者社区、TokCost、阿里云百科 |
| 7 | OpenCode 人民币价 | ❌ 官方只有美元（口径冲突） | 官方 Console / Go 文档 |

---

## 11. 溯源索引

| 编号 | 来源 | 类型 | as_of |
|---|---|---|---|
| S1 | `https://ark.volcengine.com/region:cn-beijing/docs/agent-plan-personal-afp-credit-deduction`（系数，页更新 2026-10-24）+ `…/agent-plan-personal-plan-overview`（档位/额度，页更新 2026-10-28） | **官方一手**（浏览器抓取正文） | 2026-10-30 |
| S1' | `…/agent-plan-enterprise-plan-overview`（企业版对照，¥0.003/AFP） | 官方一手 | 2026-10-30 |
| S2 | `https://cloud.tencent.com/document/product/1823/133811`（页更新 2026-10-28 15:22） | **官方一手** | 2026-10-30 |
| S3 | `https://docs.bigmodel.cn/cn/guide/start/pricing` | 官方一手 | 2026-10-30 |
| S4 | `https://www.xfyun.cn/doc/spark/TokenPlan.html` | 官方一手 | 2026-10-30 |
| S5 | 阿里：`https://help.aliyun.com/zh/model-studio/token-plan-personal-overview`（额度/价格一手）+ 系数为**我方推算**（`Credits≈刊例×100`，假设见 supplement §2） | 官方 + 推算 | 2026-10-30 |
| S6 | 格式结论：`docs/preset-pack-pricing-boundaries`（Ark，HEAD `ab228f2`，未建 PR）——G1/G2/G3 | 内部定稿 | 2026-10-30 |

> 所有「我方推算」项都写了算式与假设；未标注均为官方页数字直抄。
