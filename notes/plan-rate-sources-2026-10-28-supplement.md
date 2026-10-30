# 0.2.0 费率材料 · 补缺篇（四项，2026-10-28 夜 · 只查资料）

> 承接 `notes/plan-rate-sources-2026-10-28.md`。本篇只补 spencer 点名的四项：Kimi 额度、阿里 Credits 系数、腾讯并发、OpenCode 档位口径。
> 规矩：官方不公布就找第三方实测；**推算必须带算式和假设，不许冒充实测**；多口径冲突就并列，不取平均。
> 未调用灵枢库/网关，未改代码。

---

## 1) Kimi 额度（官方不公布 → 三条第三方口径并列）

**结论**：官方至今不公布 Kimi Code 的 token 额度（帮助中心只写「共享额度池 + 滚动 5 小时限额，按实际 token 扣」）。第三方有 3 条互相矛盾的推算，**采信度都低**，不能算作「已确认额度」；可用于「量级参考」，不能进 0.2.0 换算表。

| # | 口径 | 数字 | 来源 / as_of | 假设与可信度 |
|---|---|---|---|---|
| K-1 | 旧档位（Andante 49 元，K2.5 时代） | 5h 约 300–1,200 次调用；**周限 1M token、月限 4M token**；Moderato ×4、Allegretto ×20、Allegro ×60 | X 帖（kk，2026 上半年）https://x.com/zhul78/status/2031338838920933554 ；as_of 2026-10-28 复读 | 档位名已作废（现为 Go/Plus/Pro/Max），模型也已换 K3；**仅存档**，不可用 |
| K-2 | 199 元档（旧 Allegretto，K3） | 2300 万 token 占 14% → 周约 1.6 亿、**月约 6.5 亿**；另一网友 2800 万占 15% → 周约 2 亿、**月约 8 亿** | 知乎「雨飞」2026-07-18 实测反推 https://zhuanlan.zhihu.com/p/2061965148114400603 ；as_of 2026-10-28 复读 | 假设：用量页百分比线性可推、月额度≈4×周额度；**非官方、单用户**，可信度低 |
| K-3 | 99 元档（Plus，K3-256k 最高思考档） | **月限约 9,355 万 token（0.94 亿）** | 什么值得买 2026 https://post.smzdm.com/p/anvxd892 ；as_of 2026-10-28 复读 | 假设：同上百分比反推；与 K-2 差 7–8 倍（价格只差 2 倍），**两条互斥**，不能同时采信 |

**冲突说明（重要）**：K-2 与 K-3 数学上不自洽——199 元档推算 6.5–8 亿/月，99 元档推算 0.94 亿/月，2 倍价差给出 7–8 倍额度。说明「按用量页百分比反推」这个方法的误差被放大，**任何单条都不该当事实用**。

**我自己做的一条上界核算（明确是推算，不是实测）**：用 K3 API 刊例（缓存命中 ¥2/百万、未命中 ¥20/百万、输出 ¥100/百万，来源：知乎评测 2026-08-25 https://zhuanlan.zhihu.com/p/2075655974036291623），按编程场景 95:5 输入输出比、90% 缓存命中估算，**每百万 token 约合 ¥8.6**；99 元档若真有 0.94 亿 token，相当于 ¥800+ 的 API 价值（约 8 倍折扣）。→ 结论：**0.94 亿/月量级可信，6.5–8 亿/月明显偏离**（约 28–35 倍折扣，不符合任何一家套餐的折扣深度）。

**给 0.2.0 的处理**：Kimi 仍标「待定」，但允许在对比页显示「月额度约 1 亿 token 级（第三方反推，仅供参考）」并注明来源日期；**不要**写 6.5 亿。

---

## 2) 阿里 Credits 的模型系数（可反推，且被我三点交叉验证）

**结论**：官方不给表，但**存在一条稳定换算律：`Credits/百万 token ≈ 按量刊例人民币价 × 100`**。用官方唯一示例（qwen3.6-plus）抽一个模型验证，三个数字全部吻合。**属推算，非官方系数表；只作材料，不进 `gateway.costRates`。**

### 2.1 官方示例（原文数字，两个版本并存）

| Token 类型 | 数量 | 消耗 Credits（版本 A / 版本 B） |
|---|---|---|
| 输入 | 8,349 | 1.67 / 1.67 |
| 缓存 | 40,794 | **1.63 / 0.82** |
| 输出 | 573 | 0.69 / 0.69 |
| 合计 | — | 约 4 / 约 3.18 |

来源：阿里云百炼 Token Plan 文档示例（经 TokCost https://tokcost.com/tokenplan/aliyuntokenplan 、阿里云百科 https://www.aliyunbaike.com/bailian/10012 转述）+ 阿里云开发者社区问答 https://developer.aliyun.com/ask/704714 ；as_of 2026-10-28。**缓存项两版本差 2 倍（1.63 vs 0.82），可能对应个人版/团队版不同口径，未证实。**

### 2.2 反推算式（逐项，含假设）

```
输入：1.67 ÷ 8,349 × 1,000,000 ≈ 200 Credits/百万
输出：0.69 ÷ 573   × 1,000,000 ≈ 1,204 Credits/百万（≈1,200）
缓存：1.63 ÷ 40,794 × 1,000,000 ≈ 40 Credits/百万（版本 B：≈20）
```
假设：① Credits 与 token 数线性；② 该示例不含"思考模式"加价（若含，输入系数应更低）；③ 无时段折扣叠加（夜间四折/五折未计入）。

### 2.3 三点交叉验证（这是可信度的来源）

| 验证点 | 数字 | 结果 |
|---|---|---|
| ① 官方示例反推 | 输入 200 Credits/百万 | 基准 |
| ② 团队版 Credits 单价 × 200 应等于知乎实测的「Token Plan 标准版 1.58 元/百万」 | 198 元 ÷ 25,000 Credits = ¥0.00792/Credit；×200 = **¥1.584** | ✅ 与 1.58 元吻合（高级版 698/100,000×200=1.396≈1.40；尊享 1,398/250,000×200=1.118≈1.12，三档全中）来源：https://zhuanlan.zhihu.com/p/2076678646404849711 （2026-08-29） |
| ③ 「Credits ≈ 刊例价 × 100」是否成立 | qwen3.6-plus 按量 ¥2 输入 / ¥12 输出 / 缓存 ¥0.4（百炼定价页 https://help.aliyun.com/zh/model-studio/model-pricing ） | ✅ 2×100=200、12×100=1,200、0.4×100=40，三项全中 |

**推论（可直接给 0.2.0 用）**：阿里 Token Plan 的折扣深度 = `套餐 Credits 单价 ÷ ¥0.01` —— 个人版 ¥0.00277~0.00339/Credit ≈ **2.8~3.4 折**；团队版 ¥0.00559~0.00792 ≈ **5.6~7.9 折**（团队版贵在席位与管理）。

**⚠ 易混淆陷阱（务必写进实现文档）**：阿里云开发者社区同一问答里出现「Credits 单价 0.05 元/Credit」，那是**无影 AgentBay** 的 Credits（Browser Use 16.8/134.4、Mobile Use 11.2/89.6 Credits/百万），**不是百炼 Token Plan**。两者差 15 倍，混用会得出荒谬单价。

**边界重申**：这条只用于「材料/对比图」，数据集价格字段仍只填 API 刊例人民币价；阿里没有官方系数表，此推算若上展示必须标「推算」。

---

## 3) 腾讯 Token Plan 各档并发/额度（额度已补齐官方原文；并发只有定性排序）

**结论**：**额度与重置口径已拿官方原文**（此前只有二手）；**并发/速率官方无任何数值**，仅给「Max > Pro > Standard > Lite、平台动态调整」的定性排序。

| 项 | 官方原文 | 来源 / as_of |
|---|---|---|
| 通用 Token Plan 四档额度 | Lite 780 / Standard 1,980 / Pro 5,980 / Max 11,980 积分/订阅月；39 / 99 / 299 / 599 元 | https://cloud.tencent.com/document/product/1823/130060 （个人版套餐页），as_of 2026-10-28 |
| Hy Token Plan 四档 | 560 / 1,560 / 4,760 / 9,360 积分；28 / 78 / 238 / 468 元 | 同上 |
| 官方参考用量 | Lite「约 70 轮问答」、Standard「约 200 轮」、Pro「额度提升至基础版 3 倍」、Max「更多额度」 | 同上（官方明确「实际轮次受单轮长度/复杂度影响」） |
| 重置 | 自然月，自购买当日起算（例：01.04 10:00 购 → 02.04 09:59 到期）；**不结转** | 同上 |
| 变配 | 支持升配（补差价，剩余积分=新档总额−已用，API Key 与到期时间不变）；**不支持降配、不退订** | 同上 |
| 配额 | 每个主账号最多同时持有 2 个（1 通用 + 1 Hy）；个人版仅 1 个 API Key | 同上 |
| **并发/速率** | 官方无数字；社区文章（腾讯云开发者社区 2026-10-23）：「并发速率按 Max > Pro > Standard > Lite 排序，平台会动态调整」；额度耗尽报错码 **20097** | https://cloud.tencent.com/developer/article/2767797 ，as_of 2026-10-28 |
| 已查过但无数值的来源 | 官方积分抵扣规则页（1823/133811）、企业版专业套餐页（1823/130659）、活动页（act/pro/tokenplan）、产品 PDF（1823_130657）、技术百科 Token Plan 词条 | — |

**处理**：腾讯并发在 0.2.0 里写「未公布（官方定性：Max>Pro>Standard>Lite，动态调整）」，不要编数字。额度项从「二手」升级为「已确认」。

---

## 4) OpenCode 档位口径（结论 + 仓库证据）

**结论**：4 条渠道 = **2 档 × 2 种入站协议**，**同档共用一套价，不当 4 个不同套餐**。但必须补一句：**同一档内两个协议暴露的模型集合不重叠**（按上游原生协议切分），所以「同档」≠「同模型集」。

**证据（仓库文件 `inputs/octop-provider-template.json`，40 个 provider 中 4 条 opencode）**

| 渠道 id | base_url | protocol | variant | 模型数 | api_key_env |
|---|---|---|---|---|---|
| `opencode-zen-openai` | `https://opencode.ai/zen/v1` | openai | `zen_compatible` | 12 | `OPENCODE_API_KEY` |
| `opencode-zen-anthropic` | `https://opencode.ai/zen` | anthropic | `zen_anthropic` | 6 | `OPENCODE_API_KEY` |
| `opencode-go-openai` | `https://opencode.ai/zen/go/v1` | openai | `go_compatible` | 11 | `OPENCODE_API_KEY` |
| `opencode-go-anthropic` | `https://opencode.ai/zen/go` | anthropic | `go_anthropic` | 6 | `OPENCODE_API_KEY` |

- 4 条字段结构完全相同（仅 `base_url` / `protocol` / `variant` / 模型清单不同），同一 `vendor: opencode`、同一 `api_key_env`、同一 logo → 是「一个产品、两种协议」的写法，不是 4 个套餐。
- **模型清单不重叠**（实测取交集为空）：zen-openai = grok-4.5 / deepseek-v4-pro|flash / kimi-k3 / k2.7-code / k2.6 / glm-5.2 / 5.1 / minimax-m3 / m2.7 / big-pickle / deepseek-v4-flash-free；zen-anthropic = claude-opus-4-6 / sonnet-4-6 / haiku-4-5 / qwen3.7-max / qwen3.7-plus / qwen3.6-plus；go 侧同理（openai 11 条含 mimo-v2.5/pro、hy3；anthropic 6 条含 minimax-m3/m2.7/m2.5、qwen3.7 系）。→ 分区依据是上游原生协议，与官方 `/inference/anthropic/…` vs `/inference/openai/…` 的分法一致。
- **价格**：该配置文件**没有任何价格字段**（4 条的键只有 `id/name/base_url/protocol/api_key_env/api_key_prefix/logo/models/vendor/vendor_name/variant`），所以「同档共用一套价」不是从配置读出来的，而是来自官方定价结构：Zen 是一张按量单价表（`/v2/docs/console/models`），Go/Go Plus 是订阅（$10/$40，按模型月度美元额度，`/v2/docs/console/go`），**两档内部都不按协议分价**。

**要求落地（建议给 Octop 配置的改法，本次不动文件）**：4 条的 `description` 统一写「同 Go/Zen 档，仅入站协议不同；模型按上游原生协议分组，两协议模型不重叠」，并在展示层把 4 条折叠成 2 张卡（Zen / Go），协议作为卡内 tab。

**⚠ 一条要核的差异（不是我判断错，是配置与官方文档不一致）**：官方文档给的上游端点是 `https://opencode.ai/inference/openai/v1/...` 与 `https://opencode.ai/inference/anthropic/v1/messages`，而 Octop 模板用的是 `https://opencode.ai/zen/v1` 与 `https://opencode.ai/zen`（Go 侧 `/zen/go/v1`、`/zen/go`）。建议让 Ark 核一次是否旧版短域名，别上线才发现 404。

---

## 5) 待定清单（更新版）

| # | 项 | 状态 | 说明 |
|---|---|---|---|
| 1 | 腾讯各档并发数值 | **仍待定（已定性）** | 官方无数字，只有 Max>Pro>Standard>Lite + 动态调整；已查 6 处来源 |
| 2 | 腾讯通用套餐其余模型积分价 | 仍待定 | 官方 PDF 目录页截断；本次未补 |
| 3 | 智谱国内官方订阅页价格快照 | 仍待定 | 需浏览器抓订阅页（hugo 可顺手做） |
| 4 | 智谱国际版价格两源冲突 | 仍待定 | 同上 |
| 5 | 阿里全模型 Credits 系数 | **已补（推算，3 点验证）** | 律：`Credits≈刊例价×100`；官方未确认，标推算 |
| 6 | Kimi 额度 token 数 | **已补（第三方，低可信）** | 3 条口径并列；建议按「约 1 亿/月量级」参考 |
| 7 | 火山 Coding Plan 官方倍率 | 待 hugo 代抓 | 本次不碰火山 |
| 8 | 火山 Agent Plan 各档 5h/周/日额度 | 待 hugo 代抓 | 同上 |
| 9 | 讯飞速通版 5h/周额度 | 仍待定 | 官方只给月 30,000 次 |
| 10 | OpenCode 结转/并发/「4 档」对应 | **已补（4 条=2 档×2 协议）** | 结转仍官方未写；另有 base_url 与官方文档不一致待 Ark 核 |
| 11 | 真实用量依据 | 仍禁止 | 按 spencer 红线，不调灵枢库/网关 |

---

## 6) 新增来源（本篇）

| 项 | 来源 | 类型 / as_of |
|---|---|---|
| Kimi 额度 | https://zhuanlan.zhihu.com/p/2061965148114400603 （199 档实测反推） | 第三方实测 2026-07-18 |
| Kimi 额度 | https://post.smzdm.com/p/anvxd892 （99 档反推 9,355 万） | 第三方 2026 |
| Kimi 额度 | https://x.com/zhul78/status/2031338838920933554 （旧档位口径） | 第三方，已过时 |
| Kimi API 价（上界核算用） | https://zhuanlan.zhihu.com/p/2075655974036291623 （K3 缓存 2 / 未命中 20 / 输出 100 元） | 第三方 2026-08-25 |
| 阿里 Credits | https://developer.aliyun.com/ask/704714 （官方示例 + AgentBay 陷阱） | 官方问答 2026-06-14 |
| 阿里 Credits | https://zhuanlan.zhihu.com/p/2076678646404849711 （三档 1.58/1.40/1.12 元交叉验证） | 第三方 2026-08-29 |
| 阿里 Credits | https://help.aliyun.com/zh/model-studio/model-pricing （按量刊例） | 官方 |
| 腾讯额度 | https://cloud.tencent.com/document/product/1823/130060 （个人版套餐页） | 官方 |
| 腾讯并发 | https://cloud.tencent.com/developer/article/2767797 （定性排序 + 错误码 20097） | 第三方（腾讯云社区）2026-10-23 |
| OpenCode | `inputs/octop-provider-template.json`（4 条渠道定义） | 仓库配置 |
| OpenCode | https://opencode.ai/v2/docs/console/models ；https://opencode.ai/v2/docs/console/go | 官方 |
