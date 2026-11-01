# 0.2.0 数据集 · 兰台字段级对接说明（可直接照拼，不含待拍板项）

> 配套草案：`notes/preset-pack-0.2.0-pricing-draft.md`（Spec 分支 `lingshu/spec`，HEAD `6c3521f`）。**本文所有数字都抄自草案，未新造任何价。**
> 格式正本：`docs/preset-pack-format.md` v0.2（PR #64）；档位/分段/缓存三条结论：Ark 分支 `docs/preset-pack-pricing-boundaries`（`ab228f2`，未建 PR）G1/G2/G3。
> 本文**只覆盖不等拍板就能做的部分**；3 个待定项在 §6 单列，**勿填占位数**。

---

## ⚠ 顶部警示（比价更重要，先读这条）

> **火山方舟 Agent Plan 两档（`volcengine-agentplan-personal` / `-enterprise`）的协议形态与 baseUrl，仅按官方公布的 Anthropic 形态填写。**
> 官方「快速开始」页全文只有一个 URL：`https://ark.cn-beijing.volces.com/api/plan`，且原文强调「为专属 Base URL，**其他 Base URL 无法在 Agent Plan 中使用**」。
> **在拿到「OpenAI 兼容 / Responses 形态」的官方证据之前：不得发布、不得自行补 `openai-chat` / `openai-responses` 协议、不得套用 Coding Plan 的 `/api/coding*`。** 协议或 baseUrl 填错不会报错，只会让该渠道**静默不可用**——这比价格错更隐蔽。

---

## 0. 拼包前先知道的两件事

1. **模型 key 与 `id` 一律不带档位后缀**（G1 硬约定）：`glm-5.3` 就是 `glm-5.3`，不要写 `glm-5.3@team`、`glm-5.3-personal`——带后缀会打不中上游模型。
2. **`externalModelID` 不要填**：它是灵枢内部 ID，兰台不可知；**必须留空字符串或整个键不写，绝不能写数字**（写数字会整包解析失败）。

---

## 1. 8 条 providerId 一览

| # | `id`（providerId） | `name`（中文展示名，全局唯一） | `gateway.protocols` | 档位来源 |
|---|---|---|---|---|
| 1 | `volcengine-agentplan-personal` | 火山方舟 Agent Plan 个人版 | **仅 `anthropic-messages`** | 元/百万 = 系数 × 0.2（1 AFP = ¥0.002） |
| 2 | `volcengine-agentplan-enterprise` | 火山方舟 Agent Plan 企业版 | **仅 `anthropic-messages`** | 元/百万 = 系数 × 0.3（1 AFP = ¥0.003） |
| 3 | `tencent-tokenplan-lite` | 腾讯云 Token Plan Lite | `openai-chat`、`anthropic-messages` | 档位价（20 积分 = 1 元） |
| 4 | `tencent-tokenplan-standard` | 腾讯云 Token Plan Standard | 同上 | 同上 |
| 5 | `tencent-tokenplan-pro` | 腾讯云 Token Plan Pro | 同上 | 同上 |
| 6 | `tencent-tokenplan-max` | 腾讯云 Token Plan Max | 同上 | 同上 |
| 7 | `zhipu-bigmodel` | 智谱 BigModel | `openai-chat`、`anthropic-messages` | 单一价（API 刊例） |
| 8 | `xfyun-astron-tokenplan` | 讯飞星辰 Astron Token Plan | `openai-chat`、`anthropic-messages` | 单一价（1 积分 = ¥0.01） |

**每条 provider 的 `gateway` 里必须有的三项**：`"costCurrency": "CNY"`、`"offers": []`、`"protocols": {…}`。
`protocols` 键名**精确小写**，只允许 `openai-chat` / `openai-responses` / `anthropic-messages`（`openai-compat` 是别名、会被归一，但别主动写；其他拼写会被白名单丢弃）。

### 1.1 `gateway.baseUrl` 取值状态（火山已一手确认；其余三条按状态标，别猜）

| provider | baseUrl | 状态 |
|---|---|---|
| 火山 Agent Plan（个人/企业，两条同值） | `https://ark.cn-beijing.volces.com/api/plan` | ✅ **官方一手**（「快速开始」页唯一 URL；原文：专属 Base URL，其他 baseUrl 不可用）。**只支持 Anthropic 形态**；OpenAI 兼容 / Responses 形态官方未公布 |
| 火山 Coding Plan（非本草案，仅供参考） | `https://ark.cn-beijing.volces.com/api/coding/v3`（OpenAI 兼容）、`https://ark.cn-beijing.volces.com/api/coding`（Anthropic 兼容） | ✅ 官方原文，但**这是 Coding Plan 的，不可当 Agent Plan 用** |
| 腾讯 Token Plan（四条同） | `https://tokenhub.tencentmaas.com/plan/v3`（OpenAI 兼容）、`https://tokenhub.tencentmaas.com/plan/anthropic`（Anthropic） | ⚠ **第三方转述**（腾讯云社区文章），兰台请用官方「Token Plan 快速入门」页核一遍 |
| 智谱 BigModel | `https://open.bigmodel.cn/api/paas/v4`（OpenAI 兼容） | ⚠ 需按官方接入页核对一次 |
| 讯飞星辰 Astron | **未取到官方原文** | ⚠ **待确认**：官方只给了「兼容 OpenAI/Anthropic 协议接入地址」描述，未见具体域名 → 需抓订阅页/接入文档 |

> 说明：`baseUrl` 缺失/写错不会报错，只会让请求 404——这是**验收时必须实测一次 `/models` 或一次补全请求**的原因。

---

## 2. 各 provider 的 `models` 清单（key = `id`，均不带档位后缀）

> 所有 `costRates` 数值 = **人民币元/百万 token 字面量**；火山两档的 `CacheRead` 一律 = `Input`（官方未公布缓存系数，按输入价保守计入，属**高估**、方向保守）。
> `protocols` 每条模型都要有（带价模型若一个协议都不声明，整条含价格会被静默丢弃）。
> **火山两档的模型条目 `protocols` 只写 `{"anthropic-messages": {}}`**——与 provider 级协议一致；不要写 `openai-chat`（官方未公布 OpenAI 兼容形态，见顶部警示）。

### 2.1 `volcengine-agentplan-personal` / `-enterprise`（各 21 条；模型 `protocols` 均只写 `anthropic-messages`）

| 模型 key / `id` | personal Input / Output / CacheRead | enterprise Input / Output / CacheRead |
|---|---|---|
| `doubao-seed-2.0-mini` | 0.05 / 0.05 / 0.05 | 0.075 / 0.075 / 0.075 |
| `doubao-seed-2.0-mini`（含音频，key 用 `doubao-seed-2.0-mini-audio` 或文档约定后缀，**不得含档位**） | 0.50 / 0.50 / 0.50 | 0.75 / 0.75 / 0.75 |
| `doubao-seed-2.0-lite` ⚠待定 | 0.10 / 0.10 / 0.10 | 0.15 / 0.15 / 0.15 |
| `doubao-seed-2.1-lite` | 0.10 / 0.10 / 0.10 | 0.15 / 0.15 / 0.15 |
| `doubao-seed-2.1-lite`（含音频） | 0.90 / 0.90 / 0.90 | 1.35 / 1.35 / 1.35 |
| `doubao-embedding-vision` | 0.10 / 0.10 / 0.10 | 0.15 / 0.15 / 0.15 |
| `deepseek-v4-flash` | 0.10 / 0.10 / 0.10 | 0.15 / 0.15 / 0.15 |
| `glm-5.3-flash` | 0.10 / 0.10 / 0.10 | 0.15 / 0.15 / 0.15 |
| `deepseek-v4.1-flash-economy` | 0.40 / 0.40 / 0.40 | 0.60 / 0.60 / 0.60 |
| `deepseek-v4.1-flash` | 0.50 / 0.50 / 0.50 | 0.75 / 0.75 / 0.75 |
| `doubao-seed-2.1-turbo` | 0.50 / 0.50 / 0.50 | 0.75 / 0.75 / 0.75 |
| `doubao-seed-2.1-pro` | 0.50 / 0.50 / 0.50 | 0.75 / 0.75 / 0.75 |
| `doubao-seed-evolving` | 0.50 / 0.50 / 0.50 | 0.75 / 0.75 / 0.75 |
| `minimax-m3` | 0.50 / 0.50 / 0.50 | 0.75 / 0.75 / 0.75 |
| `glm-5.2` | 0.90 / 0.90 / 0.90 | 1.35 / 1.35 / 1.35 |
| `glm-5.3` | 0.90 / 0.90 / 0.90 | 1.35 / 1.35 / 1.35 |
| `kimi-k2.7-code` | 0.90 / 0.90 / 0.90 | 1.35 / 1.35 / 1.35 |
| `deepseek-v4-pro` | 1.10 / 1.10 / 1.10 | 1.65 / 1.65 / 1.65 |
| `kimi-k3.1-pro` | 1.40 / 1.40 / 1.40 | 2.10 / 2.10 / 2.10 |
| `kimi-k2.8-preview` | 1.60 / 1.60 / 1.60 | 2.40 / 2.40 / 2.40 |
| `kimi-k3` | 2.00 / 2.00 / 2.00 | 3.00 / 3.00 / 3.00 |

### 2.2 `tencent-tokenplan-lite|standard|pro|max`（每条 12 个模型）

**A. 分 token 类型计价（9 条，四条 provider 完全相同）**

| 模型 key / `id` | Input | Output | CacheRead |
|---|---|---|---|
| `deepseek-v4.1-flash-1017`（原厂直供，官方 Model ID 为 `deepseek/deepseek-flash`，key 用上游 `id`） | 2 | 8 | 0.04 |
| `hy4-preview` | 6 | 18 | 0.30 |
| `glm-5.3-flash` | 0.8 | 2.8 | 0.23 |
| `glm-5.3` | 8 | 28 | 2 |
| `kimi-k3` | 20 | 100 | 2 |
| `kimi-k2.7-code` | 6.5 | 27 | 1.3 |
| `minimax-m3`（官方分段价，取高档 >512k） | 4.2 | 16.8 | 0.84 |
| `mimo-v2.6-flash` | 1 | 2 | 0.02 |
| `hy3-202608` | 1 | 4 | 0.25 |

**B. 按档位统一计价（3 条，输入=输出=缓存同价，四档各不同）**

| 模型 | lite | standard | pro | max |
|---|---|---|---|---|
| `tc-code-latest` | 1.1143 | 0.99 | 0.9344 | 0.9215 |
| `deepseek-v4-flash-202605`（原厂直供） | 1.1143 | 0.99 | 0.9344 | 0.9215 |
| `hy3`（`hy3-preview` 同价） | 0.80 | 0.78 | 0.7438 | 0.72 |

**C. 不填（官方表格为空、无在售价，见 §6）**：`deepseek-v4-pro-202606`、`minimax-m2.7`、`glm-5.2`。

### 2.3 `zhipu-bigmodel`（7 条，单一价）

| 模型 key / `id` | Input | Output | CacheRead |
|---|---|---|---|
| `glm-5.3` | 8 | 28 | 2 |
| `glm-5.3-flash` | 0.8 | 2.8 | 0.23 |
| `glm-5.3-flashx` | 2 | 7 | 0.57 |
| `glm-5.2` | 8 | 28 | 2 |
| `glm-5`（官方分段，取高档 ≥32K） | 6 | 22 | 1.5 |
| `glm-5-turbo`（取高档 ≥32K） | 7 | 26 | 1.8 |
| `glm-5.1`（取高档 ≥32K） | 8 | 28 | 2 |

### 2.4 `xfyun-astron-tokenplan`（12 条，1 积分 = ¥0.01）

| 模型 key / `id` | Input | Output | CacheRead |
|---|---|---|---|
| `Spark-X2.5` | 3.20 | 12.00 | 0.48 |
| `Spark-X2-Flash` | 1.00 | 2.00 | 0.20 |
| `GLM-5.2` | 8.00 | 28.00 | 2.00 |
| `GLM-5.1` | 8.00 | 28.00 | 1.60 |
| `GLM-5` | 6.00 | 22.00 | 1.20 |
| `DeepSeek-V4-Pro` | 12.00 | 24.00 | 2.40 |
| `DeepSeek-V4-Flash` | 1.00 | 2.00 | 0.20 |
| `DeepSeek-V3.2` | 2.00 | 3.00 | 0.40 |
| `Kimi-K2.6` | 6.50 | 27.00 | 1.30 |
| `Kimi-K2.5` | 4.00 | 21.00 | 0.80 |
| `MiniMax-M2.5` | 2.10 | 8.40 | 0.42 |
| `Qwen3.5-397B-A17B` | 1.20 | 7.20 | 0.24 |

**合计：8 条 provider / 109 个模型条目**（火山 21×2 + 腾讯 12×4 + 智谱 7 + 讯飞 12）。

---

## 3. 可直接粘贴的 JSON 骨架（火山 personal + `glm-5.3` + `deepseek-v4-flash`）

```jsonc
{
  "volcengine-agentplan-personal": {
    "id": "volcengine-agentplan-personal",
    "name": "火山方舟 Agent Plan 个人版",
    "env": [],
    "doc": "https://www.volcengine.com/docs/ark/agent-plan-personal-plan-overview",
    "models": {
      "glm-5.3": {
        "id": "glm-5.3",
        "name": "GLM-5.3",
        "description": "火山方舟 Agent Plan 个人版，1 AFP = ¥0.002，系数 4.5 → 0.90 元/百万 token；官方仅公布 Anthropic 形态专属 Base URL",
        "cost": { "input": 0.9, "output": 0.9, "cache_read": 0.9 },
        "gateway": {
          "protocols": { "anthropic-messages": {} },
          "costRates": { "Input": 0.9, "Output": 0.9, "CacheRead": 0.9 }
        }
      },
      "deepseek-v4-flash": {
        "id": "deepseek-v4-flash",
        "name": "DeepSeek-V4-Flash",
        "description": "火山方舟 Agent Plan 个人版，系数 0.5 → 0.10 元/百万 token；官方仅公布 Anthropic 形态专属 Base URL",
        "cost": { "input": 0.1, "output": 0.1, "cache_read": 0.1 },
        "gateway": {
          "protocols": { "anthropic-messages": {} },
          "costRates": { "Input": 0.1, "Output": 0.1, "CacheRead": 0.1 }
        }
      }
    },
    "gateway": {
      "baseUrl": "https://ark.cn-beijing.volces.com/api/plan",
      "protocols": {
        "anthropic-messages": { "path": "/v1/messages" }
      },
      "requestHeaders": { "passthroughMode": "none" },
      "costCurrency": "CNY",
      "offers": []
    }
  }
}
```

要点：
- `cost` 是 v1 展示位（灵枢不解析），但**必须与 `costRates` 同值**，否则两处读数打架。
- `costRates` 三段**一个都不能少**；火山三段的 `CacheRead` 故意等于 `Input`（见 §2 抬头说明）。
- `models` 的 key 与 `id` 一致、不带档位后缀；**不写 `externalModelID`**。
- **`baseUrl` 是官方原值 `…/api/plan`，协议只有 `anthropic-messages`**；路径走 Anthropic 默认 `/v1/messages`（官方 base 到 `/api/plan` 为止，SDK 自行拼 `/v1/messages`）。
- **鉴权不用配**：`anthropic-messages` 隐含注入 `x-api-key`，正好对应官方的 `ANTHROPIC_AUTH_TOKEN`（Agent Plan 专属 Key；方舟普通 Key、Coding Plan Key 都不能用）→ `requestHeaders.set` 不要写鉴权头。
- `offers: []` 先留空（折扣见 §6）。

---

## 4. 易错对照表（这几条每条都踩过或验过）

| 写法 | 实际结果 | 正确做法 |
|---|---|---|
| 包外面套一层 `{"Pack": {…}}` | **200 + `Created=0` + `Warnings`**（不是 400，静默不落地） | 数据集本体就是裸 `{ "<providerId>": {…} }`；只有 `POST /import` 才需要 `{"Pack": …}` |
| `externalModelID` 写成数字 | **整包解析失败、整次回滚** | 不写，或写字符串；兰台不可知该 ID，**永远别填** |
| `costRates` 缺 `CacheRead` | 同步成功，但**成本积分静默记 0** | 三段必填；火山填 `Input` 同价 |
| 模型写成字符串（`"glm-5.3": "…"`）或写 `$ref` | 字符串 → **整包解析失败**；`$ref` → **被静默丢弃** | 模型必须**内联完整对象** |
| `pack_url` 指向清单页 `manifest.json` | **裸 500** | 指向包本体（或 302 到包本体的 `latest.json`） |
| 发布空包 `{}` | 现有全部 ACTIVE 协议**翻 STALE** | 空包绝不发布；先干跑确认条数 |
| 带价模型一个 `protocols` 都不声明 | 整条模型（含价格）**被静默丢弃** | 至少声明一个协议键 |
| `costCurrency` 忘写 | 不报错、**不影响计费**，只影响可读性与 Schema 校验 | provider 级写 `"CNY"` |
| 把数据集 URL 挂到 `sync.price_source_url` | 人民币被当美元 ×6.8，还会污染售价 | 只挂 `preset.pack_url` |

---

## 5. 干跑与验收步骤

1. **`preset.pack_url` 指向包本体**（HTTPS、200、`application/json`；若用 `latest.json` 302 跳转也可）。
2. **干跑**：`POST /api/v1/admin/channel-protocols/sync` body `{"confirm": false}`
   - 首次（全部新建）→ 全部 `Applied`、`Pending` 为空；
   - 若出现 `Pending`，说明是「已人工编辑过的协议」，需确认后再 `confirm: true`。
3. **正式写**：`{"confirm": true}` → `Applied` = provider 条目数（本次应 **8**，含 STALE 判断见下）。
4. **回读核对** `GET /api/v1/admin/channel-protocols`：
   - 数量 = 8（加上 0.1.0 已有的其它协议则按总数核）；
   - **`STALE` = 0**（若 0.1.0 里有被合并掉的旧 providerId，会翻 STALE，属预期，需登记）；
   - 每条协议 `costRates` 三段齐全，且**数值等于包内值**（不是 0、不是被 ×6.8 放大）。
5. **实测一次调用**：拿一条带价模型发一次最小请求（或先 `POST …/{id}/models/pull` 验 `baseUrl` 通不通）——`baseUrl` 猜错不会报错，只会 404。
6. **回归**：确认没把数据集 URL 配到 `sync.price_source_url`（这是唯一会「静默改价」的配置项）。

---

## 6. 待定项（**勿填占位数**，等拍板）

| # | 项 | 影响面 | 现在怎么处理 |
|---|---|---|---|
| 1 | 火山 `doubao-seed-2.0-lite` 系数（0.5 vs 2.5，5 倍差） | **2 个条目**（personal + enterprise 各 1） | 统一 0.5 暂填并标 TODO；若最终 2.5，两档价分别改 0.50/0.75 |
| 2 | 限时折扣是否进 `gateway.offers` | 影响全部 8 条 provider 的 `offers` | 一律 `[]`；折扣清单见草案 §7（含 6 条火山、5 条腾讯、1 条阿里，含已结束的 `kimi-k3` 05 折） |
| 3 | 腾讯 3 个模型无在售价（`deepseek-v4-pro-202606`、`minimax-m2.7`、`glm-5.2`） | 4 条腾讯 provider × 3 = **12 个条目不写** | 直接不写这些模型，**不要填 0 或占位价**（填 0 会让成本算成免费） |
| 4 | 火山 Agent Plan `baseUrl`、讯飞 `baseUrl` 官方原文未取到 | 影响 3 条 provider（火山 2 + 讯飞 1） | 见 §1.1，**拿到再发布**；试拼可临时用火山 Coding Plan 值验证链路，但不得上线 |

> 前三项属「等 spencer 拍板」，第四项属「等 hugo 抓一页」——两类都**不阻塞兰台今天照骨架搭包**，只阻塞「正式发布」。

---

## 7. 给 hugo 的「待抓页面清单」（按优先级，抓不到就保持待定）

> 只要 **URL + 要提取的字段 + 页面内定位关键词**；每条都写了「抓不到的预期结果」，避免白抓。
> 判定口径：**只有官方页正文才算一手**；第三方转述只能维持「待核」状态。

### ① 火山方舟 Agent Plan 接入页（最高优先：确认「是否真的只有 Anthropic 形态」）

- URL：`https://docs.volcengine.com/docs/ark/agent-plan-personal-get-started`（或 `https://ark.volcengine.com/region:cn-beijing/docs/agent-plan-personal-get-started`）
- 要提取：**是否存在「OpenAI 兼容 / Responses / 接入 AI 工具」章节**；若有，抓其中的 `base_url` 与支持协议；若没有，记录「全页 URL 只有 `/api/plan`」。
- 定位关键词：`ANTHROPIC_BASE_URL`、`api/plan`、`OpenAI`、`Responses`、`arkcli auth login`。
- **预期结果**：若确认只有 Anthropic 形态 → 本文件「火山只写 `anthropic-messages`」的结论**定稿**，兰台可发布；若发现 OpenAI 兼容形态 → 我再补 `openai-chat`（含 path）并更新 §1/§2。
- 备注：hugo 已抓过一版，全篇只有 `/api/plan`；这一条只为**确认没有遗漏章节**，扫一遍「接入 AI 工具」目录即可，不必逐字读。

### ② 讯飞星辰 Astron 接入页（当前完全空白）

- URL：`https://www.xfyun.cn/doc/spark/TokenPlan.html`（同一文档内的「接入方式 / 调用示例」章节）
- 要提取：**Token Plan 的 base URL（OpenAI 兼容与 Anthropic 兼容各是什么）+ 鉴权头形态**。
- 定位关键词：`base_url`、`https://`、`OpenAI 兼容`、`Anthropic`、`apiKey`、`Authorization`。
- **预期结果**：抓不到 → 该 provider 保持 `baseUrl` 留空、**不发布**；我不会用第三方转述填。

### ③ 腾讯云 Token Plan 快速入门（把「第三方转述」升级成一手）

- URL：`https://cloud.tencent.com/document/product/1823/130059`（Token Plan 个人版）→「快速开始 / API 使用说明」子页
- 要提取：`Base URL`（OpenAI 兼容 `/plan/v3`、Anthropic `/plan/anthropic` 是否属实）+ 是否支持 Responses。
- 定位关键词：`Base URL`、`/plan/v3`、`/plan/anthropic`、`Token Plan 专属`。
- **预期结果**：若与转述一致 → 四条腾讯 provider 的 baseUrl 定稿；若不一致 → 以官方为准，我改 §1.1。

### ④ 智谱 BigModel 接入页（核对 `api/paas/v4` 与 Anthropic 兼容入口）

- URL：`https://docs.bigmodel.cn/cn/guide/start/quick-start`（或「接入指南 / API 参考」首节）
- 要提取：OpenAI 兼容 `base_url`、Anthropic 兼容 `base_url`（若有）、是否支持 Responses。
- 定位关键词：`base_url`、`open.bigmodel.cn`、`/api/paas/v4`、`/api/anthropic`。
- **预期结果**：抓不到 → 保持「待核」，仍可按现值试拼（智谱不属 plan 渠道阻塞项）。

> 一句话：**① 和 ② 是硬阻塞**（火山不确认不能发布、讯飞没 baseUrl 不能发布）；③④ 是「把待核变一手」，不阻塞搭包。
