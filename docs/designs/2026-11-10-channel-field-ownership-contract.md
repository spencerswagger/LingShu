# 渠道 / 渠道协议字段归属契约（唯一真源）

> Status: DRAFT-v1（4 项待拍板，见 §9）
> Date: 2026-11-10
> Author: Ark（架构）
> Scope: 契约与迁移口径。**不含实现代码**；Core / View 按本文落地。
> 判据来源：`docs/preset-pack-format.md`（预设包格式，已冻结）、`docs/designs/2026-10-16-channel-protocol-gateway-fixes-design.md`（D1「引用协议自动物化」）。

---

## 0. 结论（先看这三条）

1. **四层职责，每字段只有一个写入口**：`channel_protocols`＝跨渠道复用的**模板**；`channels`＝单渠道**实例**（身份/路由/运行额度）+ 唯一的**上游覆盖**；`channel_models`＝每渠道每模型的**唯一运行时真源**；`channel_keys`＝凭据。
2. **`channel_protocols.models` 降级为「物化种子」**，网关运行时**零读取**。模型级配置（协议子集 / 对外绑定 / 成本 / 时段 / 分档 / 跨协议转换）运行时只读 `channel_models`。这一条修掉今天「同一个成本，一半读实例、一半读模板」的漂移。
3. **渠道级上游基址改为单链**：引用协议时 `channels.base_url_override`（新列）→ `channel_protocols.base_url` → `protocols[p].baseUrl`；`channels.base_url` 只在**内联模式**（未引用协议）下有效，引用协议时服务端强制为空。

现状盘点里另外发现 3 个现存缺陷（§1.5），建议与本任务同批修，判据已写进 §8。

---

## 1. 现状：同一语义有 4 个「家」

### 1.1 上游基址（4 处）

| 位置 | 谁写 | 运行时读 |
|---|---|---|
| `channels.base_url` | 渠道编辑页 | `gateway/protocol.go:198` **优先**读它 |
| `channel_protocols.base_url` | 协议编辑页 | `gateway/protocol.go:203` 兜底 |
| `channel_protocols.protocols[p].baseUrl` | 协议编辑页 | `channelprotocol/model.go:202` 协议级优先 |
| `channel_protocols.models[m].baseUrl` | 协议编辑页 | **无人读**（`protocol.go:198` 不查模型级） |

前端已经把「引用协议时内联字段禁用」（`ChannelCreate.vue:81`、`ChannelEdit.vue` 同构），但**提交时仍把旧 `BaseURL` 原样回传**（`ChannelEdit.vue:401`），而后端在引用协议时**仍然优先取 `channels.base_url`** → 协议里改 URL 不生效。这是本任务最典型的「改一处、对不上」。

### 1.2 可达上游协议（4 处）

| 位置 | 形态 | 运行时读 |
|---|---|---|
| `channels.protocol` | 单值字符串（历史 `openai-compat`） | 仅内联模式（`gateway/protocol.go:184`） |
| `channel_protocols.protocols` | JSONB `{协议名: {baseUrl,path,default}}` | 协议级能力集 |
| `channel_protocols.models[m].protocols` | 同形状，模型级子集 | **运行时回落读**（`model.go:223`） |
| `channel_models.protocols` | 同形状，模型级覆盖 | 最高优先（`model.go:225`） |

三级回落（实例覆盖 → 模板模型级 → 模板协议级）全部有效，**同一个「某模型支持哪些协议」有三个可写点**，且 UI 三处都能改。

### 1.3 模型商业字段（2 处，且读写不一致）

| 字段 | 协议种子 | 实例列 | 运行时实际读 |
|---|---|---|---|
| 对外模型绑定 | `models[m].externalModelID`（`model.go:80`） | `channel_models.external_model_id`（NOT NULL） | **实例** |
| 成本三段 | `models[m].costRates` | `channel_models.cost_rates` | **实例**（`gateway/handler.go:1928`） |
| 时段系数 | `models[m].timeConfig` | `channel_models.time_config` | **实例**（`handler.go:1929`） |
| 上下文分档 | `models[m].contextTiers` | `channel_models.context_tiers` | **实例**（`handler.go:1930`） |
| 跨协议转换 | `models[m].convertEnabled/convertTargets` | **无列** | **协议**（`gateway/conversion.go:55`） |

同一份模型配置，前 4 项读实例、第 5 项读模板 —— 这就是「后端改一处、前端对不上」的根因。

### 1.4 写入口互相踩

- 协议保存/预设同步后 `RematerializeProtocol`（`channel/service.go:417`）按种子**全量覆盖**实例行（`service.go:359-400` 只保留 `state/rate_limit/health_probe/reliability`）。
- 管理员在渠道模型页改的成本（`ChannelModels.vue:129` → `channel/handler.go:260`），下一次协议同步就被抹掉，且**没有任何标记/提示**。

### 1.5 顺带发现的现存缺陷（建议同批修）

| # | 缺陷 | 证据 | 影响 |
|---|---|---|---|
| B1 | 种子缺 `costRates` 时，物化把 `channel_models.cost_rates` 写成 **JSON null** | `channel/service.go:378` → `model_store.go:367`（`cmJSON`，`:546`） | 下一次计费直接报 `model rates empty`（`handler.go:1932`），该模型不可用 |
| B2 | 引用协议时后端优先 `channels.base_url`，前端却禁用并回传旧值 | `gateway/protocol.go:198` + `ChannelEdit.vue:401` | 改协议 baseUrl 不生效，静默打到旧地址 |
| B3 | `models[m].baseUrl` 可写但无任何读取点 | `channelprotocol/model.go:85` vs `gateway/protocol.go:198` | 配置了不生效，误导排障 |

---

## 2. 归属原则（新字段照此判定）

| 规则 | 内容 |
|---|---|
| R1 | 跨渠道复用的上游拓扑（基址/协议集/请求头/错误策略/特性）→ **协议层** |
| R2 | 单渠道实例的身份与运行额度（名称/标签/优先级/限流/探测/可靠性）→ **渠道层** |
| R3 | 每渠道每模型的价格、绑定、模型级能力 → **`channel_models`**，且是**唯一运行时真源** |
| R4 | 凭据只在 `channel_keys`；其它层只允许持 ID 引用 |
| R5 | 「0/空 = 继承」**只允许**用于运行策略（`rate_limit` / `health_probe` / `reliability`）；商业字段不允许隐形继承，必须是显式值或 NULL 显式表达继承 |
| R6 | 模板→实例只允许**单向物化**，且物化**不得覆盖已编辑实例**（需要 `edited` 标记） |
| R7 | 一个字段只有一个**写入口**；只读派生值必须单独命名（如 `BaseURLEffective`），不复用可写字段名 |

---

## 3. 字段清单表（33 条）

处理图例：**不变** / **改语义**（收窄为单层）/ **种子**（降为物化种子，运行时不再读）/ **新增** / **废弃**。

### 3.1 上游拓扑

| # | 字段 | 含义 | 归属 | 目标存储 | 运行时读点 | 处理 |
|---|---|---|---|---|---|---|
| 1 | `base_url` | 渠道内联上游基址 | 渠道 | `channels.base_url` | 仅 `channel_protocol_id IS NULL` 时读 | 改语义（引用协议时强制 `''`） |
| 2 | `base_url_override` | 渠道对协议基址的显式覆盖 | 渠道 | `channels.base_url_override`（**新列**） | 引用协议时最高优先 | 新增 |
| 3 | `base_url` | 协议默认基址 | 协议 | `channel_protocols.base_url` | 协议级兜底 | 不变 |
| 4 | `protocols[p].baseUrl` | 单协议基址 | 协议 | `channel_protocols.protocols` | 协议级优先 | 不变 |
| 5 | `models[m].baseUrl` | 模型级基址 | — | — | **无** | **废弃**（包格式容忍，落库丢弃；需要按协议区分请用 #4） |
| 6 | `path` | 单协议默认路径 | 协议 | `protocols[p].path` | 端点拼接 | 不变 |
| 7 | `default` | 首选协议标记 | 协议 | `protocols[p].default` | 协议选择 | 不变（同协议内至多 1 个，`service.go:152`） |

### 3.2 可达协议集

| # | 字段 | 含义 | 归属 | 目标存储 | 运行时读点 | 处理 |
|---|---|---|---|---|---|---|
| 8 | `protocol` | 内联模式单一基础协议 | 渠道 | `channels.protocol` | 仅内联模式；`openai-compat`→`openai-chat` 归一 | 改语义（引用协议时忽略且强制 `''`） |
| 9 | `protocols` | 协议能力集 | 协议 | `channel_protocols.protocols` | 模型行无覆盖时的能力集 | 不变 |
| 10 | `models[m].protocols` | 协议内模型级子集 | 协议 | 同上 JSONB | **不再回落读** | 种子 |
| 11 | `protocols` | 模型级可达协议覆盖 | 模型 | `channel_models.protocols` | 非空即最高优先 | 不变（确认为唯一运行覆盖） |

### 3.3 模型商业字段

| # | 字段 | 含义 | 归属 | 目标存储 | 运行时读点 | 处理 |
|---|---|---|---|---|---|---|
| 12 | `models[m].externalModelID` | 绑定的对外模型 | 协议（种子） | JSONB | 仅物化 | 种子（口径写死 + 加测试） |
| 13 | `external_model_id` | 同上 | 模型 | `channel_models.external_model_id` | 路由/计费 | 不变（**唯一真源**） |
| 14 | `models[m].costRates` | 成本三段 | 协议（种子） | JSONB | 仅物化 | 种子 |
| 15 | `cost_rates` | 成本三段 | 模型 | `channel_models.cost_rates` | `handler.go:1928` | 不变（**唯一真源**） |
| 16 | `models[m].timeConfig` | 时段系数 | 协议（种子） | JSONB | 仅物化 | 种子 |
| 17 | `time_config` | 时段系数 | 模型 | `channel_models.time_config` | `handler.go:1929` | 不变（**唯一真源**） |
| 18 | `models[m].contextTiers` | 上下文分档 | 协议（种子） | JSONB | 仅物化 | 种子 |
| 19 | `context_tiers` | 上下文分档 | 模型 | `channel_models.context_tiers` | `handler.go:1930` | 不变（**唯一真源**） |
| 20 | `models[m].convertEnabled` | 是否允许跨协议转换 | 协议（种子） | JSONB | **改为不读** | 种子 |
| 21 | `models[m].convertTargets` | 转换目标协议偏好 | 协议（种子） | JSONB | **改为不读** | 种子 |
| 22 | `convert_enabled` | 同上 | 模型 | `channel_models.convert_enabled`（**新列**，NULL=继承全局） | `gateway/conversion.go:55` 改读模型行 | 新增 |
| 23 | `convert_targets` | 同上 | 模型 | `channel_models.convert_targets`（**新列**，默认 `[]`） | 同上 | 新增 |
| 24 | `edited` | 实例行是否被本地编辑过 | 模型 | `channel_models.edited`（**新列**） | 物化守卫 | 新增 |

### 3.4 请求头 / 错误策略 / 特性（协议独占，无渠道级）

| # | 字段 | 含义 | 归属 | 目标存储 | 处理 |
|---|---|---|---|---|---|
| 25 | `request_headers.*` | 透传模式 + 名单 + 覆盖 | 协议 | `channel_protocols.request_headers` | 不变（**同协议多渠道共享**，要差异化就建独立协议） |
| 26 | `error_policy[]` | 协议级错误处置 | 协议 | `channel_protocols.error_policy` | 不变（同上，共享口径） |
| 27 | `features[]` | 协议特性挂载 | 协议 | `channel_protocols.features` | 不变 |

### 3.5 运行策略（分层继承，不是双写）

| # | 字段 | 含义 | 归属 | 目标存储 | 处理 |
|---|---|---|---|---|---|
| 28 | `rate_limit` | 限流窗口 | 渠道级 + 模型级 | `channels.rate_limit` / `channel_models.rate_limit` | 不变（**逐字段** 0 值→渠道级→默认，`manager.go:402`） |
| 29 | `health_probe` | 探测节奏/阈值 | 渠道级 + 模型级 | 同上 | 不变（`manager.go:385`） |
| 30 | `reliability` | 可靠性滑动窗口 | 渠道级 + 模型级 | 同上 | 不变（`manager.go:384`） |
| 31 | `state` | 运行状态 | 密钥级 + 模型级 | `channel_keys.state` / `channel_models.state` | 不变（渠道无独立状态，0009 已删） |
| 32 | `priority` / `tags` / `name` | 路由与身份 | 渠道 | `channels.*` | 不变（协议 `name` 是模板名，不同实体，不算重复） |
| 33 | `credential_enc` | 凭据密文 | 密钥 | `channel_keys.credential_enc` | 不变（协议侧 `auth` 列 0010 已删，鉴权由基础协议隐含） |

**处理统计（共 33 行）**：

| 处理 | 条数 | 明细 |
|---|---|---|
| 不变 | 19 | 3、4、6、7、9、11、13、15、17、19、25、26、27、28、29、30、31、32、33 |
| 种子 | 7 | 10、12、14、16、18、20、21（其中 12/14/16/18 现状已只被物化消费，本次把口径写死并加测试防回退） |
| 新增 | 4 | 2、22、23、24 |
| 改语义 | 2 | 1、8 |
| 废弃 | 1 | 5 |

---

## 4. 目标数据模型

### 4.1 DDL（迁移 `0019_channel_field_ownership.sql`）

```sql
-- 1) 渠道级：唯一的上游覆盖列（引用协议时生效；内联模式不使用）
ALTER TABLE channels ADD COLUMN IF NOT EXISTS base_url_override TEXT NOT NULL DEFAULT '';

-- 2) 模型级：跨协议转换从协议种子下沉为实例真源
ALTER TABLE channel_models ADD COLUMN IF NOT EXISTS convert_enabled BOOLEAN;              -- NULL = 继承全局开关
ALTER TABLE channel_models ADD COLUMN IF NOT EXISTS convert_targets JSONB NOT NULL DEFAULT '[]'::jsonb;

-- 3) 模型级：本地编辑标记，物化守卫（R6）
ALTER TABLE channel_models ADD COLUMN IF NOT EXISTS edited BOOLEAN NOT NULL DEFAULT false;
```

**不改的既有约束**（Core 不要动）：
- `channel_protocols`：`uq_channel_protocols_active(name) WHERE deleted_at IS NULL`、`uq_channel_protocols_preset(preset_source_id)`、`source_status CHECK`。
- `channels`：`uq_channels_active(name)`、`channels.channel_protocol_id REFERENCES channel_protocols(id)`（**不带 ON DELETE**，删除协议靠「仍被引用则拒绝」，`channelprotocol/service.go:325`）。
- `channel_models`：`uq_channel_models_active(channel_id, internal_model_id)`、`external_model_id NOT NULL`、`state CHECK`。

### 4.2 Go 结构体变更

| 结构体 | 文件:行 | 变更 |
|---|---|---|
| `Channel` | `channel/store.go:89` | +`BaseURLOverride string`；`channelCols`（`:140`）追加 `base_url_override` |
| `ChannelInput` | `channel/service.go:47` | +`BaseURLOverride string` |
| `ChannelView` | `channel/manager.go:190` | +`BaseURLOverride`、+`BaseURLEffective`（只读派生）；两处构造点 `manager.go:995`、`detail.go:96` 同时补 |
| `ChannelModel` | `channel/model_store.go:22` | +`ConvertEnabled *bool`、+`ConvertTargets []string`、+`Edited bool` |
| `ChannelModelInput` | `channel/model_service.go:15` | 同上三字段 |
| `viewChannelModel` | `channel/handler.go:220` | 同上三字段（`ConvertEnabled` 用 `*bool` 保留 nil 语义） |
| `channelModelRequest` | `channel/handler.go:260` | 同上三字段 |
| `channelprotocol.ChannelProtocol` | `channelprotocol/model.go:117` | 不变（`Models` 保留为种子载体） |
| `channelprotocol.EffectiveModelProtocols` | `model.go:223` | **删除**（强制编译报错定位调用点） |
| `channelprotocol.EffectiveModelConvert` | `model.go:238` | **删除** |

⚠ Core 注意：`channel_models` 有**三处列清单**必须同步追加，漏一处会静默丢字段：
`cmCols`（`model_store.go:43`）、`cmColsJoinExt`（`:110`）、`cmColsJoinExtChannel`（`:166`）。

### 4.3 对外 DTO（前端契约）

| 接口 | 字段 | 变化 |
|---|---|---|
| `GET /admin/channels`、`GET /admin/channels/{id}` | `BaseURLOverride` | 新增；引用协议时为覆盖值，`''`=继承 |
| 同上 | `BaseURLEffective` | 新增；**只读派生**，最终生效地址（走 §5 解析链） |
| 同上 | `BaseURL` | 语义收窄为「内联值」；引用协议时恒为 `''` |
| `POST/PUT /admin/channels` | `BaseURL` | 引用协议时**非空即 400**（见 §5.1 校验） |
| 同上 | `BaseURLOverride` | 新增；引用协议时写覆盖列，内联时忽略 |
| `GET/POST/PUT /admin/channels/{id}/models` | `ConvertEnabled` / `ConvertTargets` / `Edited` | 新增 |
| `GET/POST/PUT /admin/channel-protocols` | `Models[m].baseUrl` | **废弃**：仍接受并忽略（全局未知字段容忍约定），不再回传 |
| 同上 | 其余 `Models[m]` 键 | 语义改为「模板默认值 + 物化种子」 |

不改的契约：`ModelPricing` 组件依赖的 `CostRates` 键仍是 **PascalCase**（`billing.WireRates`，`billing/rates.go:77`），存储列仍是 **snake**（`billing.Rates`）。**这条不变**，迁移 SQL 比较时必须显式映射（见 §6.2）。

---

## 5. 取值口径（每字段唯一解析链）

### 5.1 上游基址

```
if channel_protocol_id IS NULL:            # 内联模式
    base = channels.base_url               # 必填，校验 http(s)://
else:                                       # 引用协议
    assert channels.base_url == ''          # 服务端强制：PUT 时非空 → 400「引用渠道协议时不允许设置内联 BaseURL」
    base = first_non_empty(
        channels.base_url_override,
        channel_protocols.base_url,
        channel_protocols.protocols[协议].baseUrl)
```

实现落点：`gateway/protocol.go:198 effectiveBaseURL`、`channel/probe.go:27`（探测与真实转发必须共用同一函数，现状已共用，不要分叉）。

### 5.2 可达协议集

```
model_row.protocols 非空        → 用它（canonical 归一）
else 引用协议                    → channel_protocols.protocols
else 内联                        → { canonical(channels.protocol) }，非法值回落 openai-chat
```

`channel_protocols.models[m].protocols` **不出现在运行时链上**。迁移会把已有值复制进 `channel_models.protocols`，因此存量行为不变。

### 5.3 模型商业字段

| 字段 | 运行时唯一读点 |
|---|---|
| 成本 / 时段 / 分档 | `channel_models`（`gateway/handler.go:1928-1930`）——已如此，不改 |
| 对外模型绑定 | `channel_models.external_model_id`（NOT NULL）——已如此，不改 |
| 跨协议转换 | **改为** `channel_models.convert_enabled`（NULL=继承全局 `conversionEnabled()`）、`convert_targets`（空=按默认偏好） |

### 5.4 物化规则（Core 必须逐条实现）

对某协议 P 的每个 `Models[m]`（`m` = 内部模型 ID，`btrim` 后比较，**大小写敏感**）：

1. `externalModelID <= 0` → **跳过**（`external_model_id NOT NULL`，沿用 D5）。
2. 实例行不存在 → 插入，`edited=false`，`state=NORMAL`。
3. 实例行存在且 `edited=true` → **跳过该行所有商业字段**（成本/时段/分档/绑定/协议子集/转换）。
4. 实例行存在且 `edited=false` → 逐字段写，**但种子缺省的键不写**（保留现值）：
   - `? externalModelID` 才写 `external_model_id`；
   - `? costRates` 才写 `cost_rates`（**这就是 B1 的修法**：不再把 nil 写成 JSON null）；
   - `? timeConfig` / `? contextTiers` / `? protocols` 同理；
   - `? convertEnabled` 才写 `convert_enabled`（否则保持原值，不置 NULL）；
   - `? convertTargets` 才写 `convert_targets`。
5. 永远不覆盖 `state` / `rate_limit` / `health_probe` / `reliability`（现状已如此，`service.go:386-389`）。
6. 永远不删除协议未列出的实例行（现状已如此）。

`channel_models.updated_at`：仅在实际写入字段时刷新，跳过不改。

### 5.5 运行策略继承（不是双写）

`rate_limit` / `health_probe` / `reliability` 三层：**模型级 0 值 → 渠道级 0 值 → 默认**，逐字段独立（`manager.go:402 mergeRateLimit`、`:384 mergeReliability`、`:385 mergeHealthProbe`）。
口径明确：**0 表示「继承」，不表示「不限」**。这是既有约定，本次不改；若要「显式不限」需另立字段（不在本任务范围）。

---

## 6. 迁移方案

### 6.1 顺序（Migration 是「每文件一个事务」，`db/migrate.go:69`）

`0019_channel_field_ownership.sql` 单个文件内按序：

```sql
-- (a) 加列（见 §4.1，幂等 IF NOT EXISTS）

-- (b) 存量：引用协议的渠道，把今天「隐式生效」的 base_url 搬进显式覆盖列
UPDATE channels
SET base_url_override = btrim(base_url), base_url = ''
WHERE deleted_at IS NULL
  AND channel_protocol_id IS NOT NULL
  AND btrim(base_url) <> '';

-- (c) 存量：协议种子的转换配置下沉到实例
-- 只写种子存在的键；缺省键保留实例现值（convert_targets NOT NULL，必须 ELSE 保留，不能落 NULL）
UPDATE channel_models cm
SET convert_enabled = CASE WHEN cp.models -> cm.internal_model_id ? 'convertEnabled'
                           THEN (cp.models -> cm.internal_model_id ->> 'convertEnabled')::boolean
                           ELSE cm.convert_enabled END,
    convert_targets = CASE WHEN cp.models -> cm.internal_model_id ? 'convertTargets'
                           THEN (cp.models -> cm.internal_model_id -> 'convertTargets')
                           ELSE cm.convert_targets END
FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE cm.channel_id = c.id AND cm.deleted_at IS NULL
  AND cp.models ? cm.internal_model_id
  AND cp.models -> cm.internal_model_id ?| ARRAY['convertEnabled','convertTargets'];

-- (d) 存量：与种子不一致的实例行标记 edited=true（防止下一次同步静默覆盖既有配置）
UPDATE channel_models cm
SET edited = true
FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE cm.channel_id = c.id AND cm.deleted_at IS NULL
  AND cp.models ? cm.internal_model_id
  AND (
       (cp.models -> cm.internal_model_id ? 'externalModelID'
        AND cm.external_model_id IS DISTINCT FROM NULLIF(cp.models -> cm.internal_model_id ->> 'externalModelID','')::bigint)
    OR (cp.models -> cm.internal_model_id ? 'protocols'
        AND cm.protocols IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'protocols'))
    OR (cp.models -> cm.internal_model_id ? 'costRates'
        AND cm.cost_rates IS DISTINCT FROM jsonb_strip_nulls(jsonb_build_object(
              'input',      (cp.models -> cm.internal_model_id -> 'costRates' ->> 'Input')::numeric,
              'output',     (cp.models -> cm.internal_model_id -> 'costRates' ->> 'Output')::numeric,
              'cache_read', (cp.models -> cm.internal_model_id -> 'costRates' ->> 'CacheRead')::numeric)))
    OR (cp.models -> cm.internal_model_id ? 'timeConfig'
        AND cm.time_config IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'timeConfig'))
    OR (cp.models -> cm.internal_model_id ? 'contextTiers'
        AND cm.context_tiers IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'contextTiers'))
  );
```

(c)(d) 的键大小写是**必须**的：种子 `costRates` 是 `billing.WireRates`（Pascal 键），实例 `cost_rates` 是 `billing.Rates`（snake 键），直接 `IS DISTINCT FROM` 恒为真 → 会把全部行误标为 edited。

(d) 另一条口径：**种子没定义的键不参与漂移判定**。例如种子里某模型只有 `externalModelID`、没有 `costRates`，则该行成本与种子无关，不得标 `edited`（否则会永久冻结协议同步）。

### 6.1.1 已验证（2026-11-10，Ark 本地）

上述 (a)~(d) 与回滚 SQL 已在 PostgreSQL 17（dev 库，`BEGIN … ROLLBACK`，不落库）按 4 个断言场景实跑通过：
- 引用协议 + 非空 `base_url` → 搬迁到覆盖列、`base_url` 清空；
- 引用协议 + 空 `base_url` → 不动；
- 内联渠道 → 不动；
- 与种子一致的行不标 `edited`、成本偏离种子的行标 `edited`、种子无该键的行不标；
- 回滚 SQL 能把 `base_url` 还原。
Core 落地时把这段直接搬进迁移测试即可（`db/migrate_test.go` 已有迁移测试骨架）。

### 6.2 可回滚路径

代码回滚 + 手工执行（**不要放进 `migrations/`，会被自动应用**）：`backend/scripts/rollback-0019.sql`

```sql
UPDATE channels
SET base_url = base_url_override
WHERE deleted_at IS NULL AND channel_protocol_id IS NOT NULL
  AND btrim(base_url) = '' AND btrim(base_url_override) <> '';

ALTER TABLE channels DROP COLUMN IF EXISTS base_url_override;
ALTER TABLE channel_models
  DROP COLUMN IF EXISTS convert_enabled,
  DROP COLUMN IF EXISTS convert_targets,
  DROP COLUMN IF EXISTS edited;
```

**回滚后老代码仍可运行**：老二进制读 `channels` / `channel_models` 用的是显式列清单（`channelCols` `store.go:140`、`cmCols` `model_store.go:43`），多出来的列不影响查询；被删的三列数据可由协议种子重建（转换配置本来就在 `channel_protocols.models`，本次只复制不删除）。
唯一不可逆点：`base_url_override` 的值在 DROP 前已回写进 `base_url`，行为等价。

### 6.3 老数据兼容与读写窗口

| 场景 | 处理 |
|---|---|
| 引用协议 + `base_url` 非空（B2 存量） | 迁到 `base_url_override`，**生效地址不变**（before/after 必须逐渠道比对，Gate 用它当判据） |
| 引用协议 + `base_url` 为空 | 无需处理，解析链落到协议 |
| 内联渠道（未引用协议） | 完全不动 |
| 实例行与种子不一致 | `edited=true`，**不再被同步覆盖**；需要恢复由新接口重置 |
| 实例行与种子一致 | `edited=false`，继续跟随协议更新 |
| 种子缺某商业键 | 物化不写该列（B1 修复），现值保留 |
| 迁移期间旧后端仍在跑 | 旧后端读 `channels.base_url`（已清空）→ 引用协议的渠道会回落到协议地址。**要求迁移与新后端同版本发布**（单实例部署已满足）；若必须灰度，先上后端兼容代码（读 `base_url_override` 优先），再跑迁移。 |
| 空库 | 全部语句幂等，无数据操作，可重复执行 |

**新增接口**（Core 实现）：`POST /admin/channels/{id}/models/{mid}/reset-from-protocol` → `edited=false` + 重新物化该行（返回新的实例行）。这是 `edited` 的唯一出口，避免「标记后再也同步不了」。

---

## 7. 交接清单

### 7.1 给 Core（实现顺序）

1. **迁移** `backend/internal/db/migrations/0019_channel_field_ownership.sql`（§6.1 内容逐条）。
2. **存储层**：`channel/store.go`（Channel + channelCols + Insert/Update 的列与参数）、`channel/model_store.go`（ChannelModel + **三处列清单** `cmCols:43` / `cmColsJoinExt:110` / `cmColsJoinExtChannel:166` + Insert/Update）。
3. **服务层**：`channel/service.go` —— `ChannelInput`、`validateBase`（:99，引用协议时 `BaseURL` 非空 → 400 且忽略 `BaseURLOverride`；内联时 `BaseURLOverride` 必须为空）、`materializeFromProtocol`（:359，按 §5.4 重写）、新增 `ResetModelFromProtocol`。
4. **模型服务**：`channel/model_service.go` —— Input 三字段；`CreateChannelModel` 写 `edited=true`（人工创建即视为本地编辑）；`UpdateChannelModel` 写 `edited=true`；**仅在商业字段变化时**才置 `edited`（只改 `rate_limit` 等运行策略不应冻结协议同步）。
5. **删除模板回落**：`channelprotocol/model.go` 删 `EffectiveModelProtocols`(:223)、`EffectiveModelConvert`(:238)；改 `gateway/protocol.go:172 effectiveProtocols`、`gateway/conversion.go:55`。
6. **出参**：`manager.go:190 ChannelView` + 两处构造（`manager.go:995`、`detail.go:96`）；`detail.go` 目前连 `ChannelProtocolID` 都没传，顺手补上。
7. **物化守卫**：`channelprotocol/service.go:375 Sync` → `notifyRefresh` → `channel/service.go:417 RematerializeProtocol` 链路不用改，规则改在 `materializeFromProtocol` 内部。

**必须保持的对外行为**（改坏了 Gate 会挂）：
- `openai-compat` → `openai-chat` 归一（`channelprotocol/model.go:29`）继续生效。
- 内联渠道缺 `base_url`/`protocol` → 400，且非法协议名回落 `openai-chat`（`gateway/protocol.go:184`）。
- 协议被渠道引用时删除协议 → 409「仍被渠道引用」（`channelprotocol/service.go:325`）。
- 预设同步仍以 `preset_source_id` 幂等，`edited=true` 的协议仍需二次确认（`service.go:425`）。
- 请求头策略变更后立即生效、无需重启（既有 refresh 钩子，`channelprotocol/service.go:74`）。
- 所有 JSON 解码**不要**加 `DisallowUnknownFields`（全仓 0 处，未知字段容忍是全局约定）——`models[m].baseUrl` 的「废弃但仍容忍」靠这条。

### 7.2 给 View（前端契约变化）

| 文件 | 改动 |
|---|---|
| `src/api/admin.ts` | `AdminChannel` +`BaseURLOverride`/`BaseURLEffective`；`ChannelInput` +`BaseURLOverride`；`ChannelModel`/`ChannelModelInput` +`ConvertEnabled`/`ConvertTargets`/`Edited`；`ChannelProtocolModelOverride` 去掉 `baseUrl` |
| `views/admin/ChannelCreate.vue` | 引用协议时把「Base URL」输入框换成「Base URL 覆盖（留空=继承协议）」，提交 `BaseURLOverride`，**不再回传协议值**；内联模式仍提交 `BaseURL` 且必填 |
| `views/admin/ChannelEdit.vue` | 同 Create；`:401` 的 `BaseURL: form.base_url` 必须按模式分流 |
| `views/admin/ChannelProtocolEdit.vue` | 删除模型级 `baseUrl` 编辑（`:396-400`、`:452`、`:480`、`:499`）；成本/时段/分档/协议子集/转换标注为「模板默认值，仅对未编辑过的渠道模型生效」 |
| `components/ChannelModels.vue` | 增加 `Edited` 徽标 + 「重置为协议值」按钮（调 reset 接口）；`ConvertEnabled` 三态（继承/开/关） |
| `components/ModelPricing.vue` | 不变（`CostRates` Pascal 键口径不动） |

前端铁律：`BaseURL` 与 `BaseURLOverride` 不要同时可编辑；引用协议时只显示后者。

### 7.3 给 Gate（可写成测试的判据）

1. **唯一真源**：`grep -rn "EffectiveModelProtocols\|EffectiveModelConvert" backend/` → 0 命中（函数已删、调用点为 0）。
2. **迁移可回滚**：空库 + 造数库各跑一次 `0019` → `rollback-0019` → 数据与迁移前一致（`channels.base_url` 逐行等值、`channel_models` 行数不变）。
3. **老数据兼容**：造 1 个「引用协议 + 非空 base_url」渠道，迁移前后**有效上游基址**必须相同（可用 `go test` 直接调解析函数）。
4. **行为用例**（表驱动，Gate 直接抄）：
   | 用例 | 期望 |
   |---|---|
   | A 引用 P（P.base_url=X），A.override 空 | 请求打 X |
   | A.override=Y | 请求打 Y |
   | A 切回内联（protocol_id=NULL） | `base_url` 必填，缺 → 400 |
   | 引用协议时 PUT `BaseURL` 非空 | 400（或静默丢弃 + 落 `base_url_override`，二选一**由 Core 定并写测试**，见 §9-3） |
   | 改 P.costRates（种子有） | A 未编辑模型行跟着变；`edited=true` 行不变 |
   | 改 P 但种子缺 costRates | 既有 `cost_rates` **不得**被写成 null（B1 回归用例） |
   | 改 P 的 convertEnabled=false | 转换停用（读实例列，`conversion.go` 生效） |
   | 人工改 A 某模型成本 → 再同步协议 | 该行成本不被覆盖 |
   | `reset-from-protocol` | 该行恢复为种子值且 `edited=false` |
5. **三口径全绿**：`go vet ./...`、`go test -race ./...`、`npx vue-tsc --noEmit`。
6. **不做的事**（防误伤）：不加 `DisallowUnknownFields`；不删 `channel_protocols.models` 列；不改计费公式与三段 usage 口径。

---

## 8. 风险与取舍

| 风险 | 取舍与理由 |
|---|---|
| 物化不再覆盖已编辑行 → 协议调价对不同渠道生效时点不一致 | 这是**有意的**：静默覆盖用户配置比「同步延迟」危害大得多。补偿手段是 `edited` 徽标 + 一键重置。 |
| 新增 `base_url_override` 而不是「渠道不再允许覆盖基址」 | 后者会破坏存量（引用协议却指向独立代理地址的渠道会被改到错误上游）。**行为兼容优先**。 |
| 废弃 `models[m].baseUrl` | 它今天**没有任何读取点**（B3），保留只会继续误导。包格式仍容忍该键（未知字段容忍 + 物化忽略），不破坏预设包兼容。 |
| 转换配置下沉到实例 | 唯一会让「同协议多渠道共享转换策略」变成「各自可配」。默认仍继承全局，行为不变；只是把覆盖权交回实例层，和成本/时段一致。 |
| 请求头/错误策略仍协议独占 | 它们影响**安全**（鉴权头注入、熔断），跨渠道共享是有意的约束；要差异化就建独立协议（协议可复制）。**不在本次放开**。 |

---

## 9. 待拍板清单（需要 spencer 决定）

1. **引用协议时渠道还允许覆盖基址吗？**
   我的建议：允许，但只走 `base_url_override`（显式、可清空、留空=继承）。备选：完全禁止覆盖（强制用独立协议）——更纯粹，但会改掉存量渠道的实际地址，需人工逐个确认。
2. **跨协议转换是否允许按渠道差异化？**
   我的建议：允许（下沉到 `channel_models`），默认继承全局，行为不变。备选：维持协议级共享（不加列），但那样 `channel_protocols.models` 就得继续当运行时读源，与「唯一真源」冲突。
3. **引用协议时 PUT 带非空 `BaseURL`：硬 400 还是静默转成覆盖？**
   我的建议：**硬 400**（配置错误应当报错）。代价：老前端（未升级）保存渠道会失败——需前后端同批发布。备选：静默写入 `base_url_override`，兼容旧前端但会掩盖错误。
4. **`channel_models.edited` 的置位粒度**
   我的建议：只有「商业字段」（绑定/成本/时段/分档/协议子集/转换）变化才置 `edited`；改运行策略（限流/探测/可靠性）不置位。备选：任何保存都置位（简单，但会让改个限流就冻结协议同步）。

**另外待确认一条事实**：本地 dev/test/staging 三库当前均为空表（`channels` / `channel_protocols` / `channel_models` 各 0 行），迁移只能对「造数」验证；spencer 生产库的存量分布（引用协议且 `base_url` 非空的渠道数、与种子不一致的模型行数）我无法读取，**上线前需要他给一次计数**，用于评估 §6.1 (b)(d) 的影响面。
