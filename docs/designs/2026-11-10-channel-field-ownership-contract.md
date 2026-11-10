# 渠道 / 渠道协议字段归属契约（唯一真源）

> Status: **RULED-v4**（v3 之后补入 §6.1(b) 软删口径 S1~S3 + 「归档集合 ≡ 清空集合」不变量、§6.2 未归档渠道的还原路径、§6.4 预检 `2b)`/`2c)` 口径；四项裁定不变）
> Date: 2026-11-10
> Author: Ark（架构）
> Scope: 契约与迁移口径。**不含实现代码**；Core / View 按本文落地。
> 判据来源：`docs/preset-pack-format.md`（预设包格式，已冻结）、`docs/designs/2026-10-16-channel-protocol-gateway-fixes-design.md`（D1「引用协议自动物化」）。
> 变更记录：v1 初稿 → v2（四项裁定落文、去掉渠道级基址覆盖与转换下沉）→ v3（协议子集下移 + 上线预检 + >5 万路径缺口）→ v4（软删口径与回滚不变量）。

---

## 0. 结论（先看这三条）

1. **四层职责，每字段只有一个写入口**：`channel_protocols`＝跨渠道复用的**模板**；`channels`＝单渠道**实例**（身份/路由/运行额度）；`channel_models`＝每渠道每模型的**唯一运行时真源**；`channel_keys`＝凭据。
2. **`channel_protocols.models` 降级为「物化种子」，运行时只保留一项例外：跨协议转换（`convertEnabled/ConvertTargets`）仍由协议定义、运行时可读**（裁定 ②：渠道不得各自改转换行为）。其余模型级配置（协议子集 / 对外绑定 / 成本 / 时段 / 分档）运行时只读 `channel_models`。
3. **渠道级基址没有「覆盖」这条路**（裁定 ①④）：内联模式用 `channels.base_url`；引用协议则基址**完全跟随协议**，`channels.base_url` 必须为空。引用协议时再传非空 `BaseURL` → **400 明确报错**（裁定 ③），不静默转覆盖。

相比 v1，**新增列从 4 个降到 1 个**（只剩 `channel_models.edited`），迁移面更小、可回滚更容易。

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

前 4 项读实例、第 5 项读模板 —— 这就是「后端改一处、前端对不上」的根因。裁定 ② 明确**转换保持协议级**，因此本任务不把转换下沉，改为在契约里写死「转换＝协议独占，渠道不可差异化」。

### 1.4 写入口互相踩

- 协议保存/预设同步后 `RematerializeProtocol`（`channel/service.go:417`）按种子**全量覆盖**实例行（`service.go:359-400` 只保留 `state/rate_limit/health_probe/reliability`）。
- 管理员在渠道模型页改的成本（`ChannelModels.vue:129` → `channel/handler.go:260`），下一次协议同步就被抹掉，且**没有任何标记/提示**。

### 1.5 顺带发现的现存缺陷（本任务同批修）

| # | 缺陷 | 证据 | 影响 |
|---|---|---|---|
| B1 | 种子缺 `costRates` 时，物化把 `channel_models.cost_rates` 写成 **JSON null** | `channel/service.go:378` → `model_store.go:367`（`cmJSON`，`:546`） | 下一次计费直接报 `model rates empty`（`handler.go:1932`），该模型不可用 |
| B2 | 引用协议时后端优先 `channels.base_url`，前端却禁用并回传旧值 | `gateway/protocol.go:198` + `ChannelEdit.vue:401` | 改协议 baseUrl 不生效，静默打到旧地址 |
| B3 | `models[m].baseUrl` 可写但无任何读取点 | `channelprotocol/model.go:85` vs `gateway/protocol.go:198` | 配置了不生效，误导排障 |

修法（对应 §9 裁定）：B1 → 种子缺省的键不写实例（§5.4）；B2 → 引用协议时不读 `channels.base_url`，写入非空即 400（§5.1）；B3 → 废弃并在协议包导入/导出时丢弃该键。

---

## 2. 归属原则（新字段照此判定）

| 规则 | 内容 |
|---|---|
| R1 | 跨渠道复用的上游拓扑（基址/协议集/请求头/错误策略/特性/转换策略）→ **协议层** |
| R2 | 单渠道实例的身份与运行额度（名称/标签/优先级/限流/探测/可靠性）→ **渠道层** |
| R3 | 每渠道每模型的价格、绑定、模型级能力 → **`channel_models`**，且是**唯一运行时真源** |
| R4 | 凭据只在 `channel_keys`；其它层只允许持 ID 引用 |
| R5 | 「0/空 = 继承」**只允许**用于运行策略（`rate_limit` / `health_probe` / `reliability`）；商业字段不允许隐形继承 |
| R6 | 模板→实例只允许**单向物化**，且物化**不得覆盖已编辑实例**（`edited` 标记） |
| R7 | 一个字段只有一个**写入口**；只读派生值必须单独命名（如 `BaseURLEffective`），不复用可写字段名 |
| R8 | **不存在「同协议两种行为」**：渠道对协议的任何差异（基址/协议集/转换/请求头/错误策略）一律通过**新建协议**表达，不允许渠道级覆盖（裁定 ①②） |

---

## 3. 字段清单表（30 条）

处理图例：**不变** / **改语义**（收窄为单层）/ **种子**（降为物化种子，运行时不再读）/ **新增** / **废弃**。

### 3.1 上游拓扑

| # | 字段 | 含义 | 归属 | 目标存储 | 运行时读点 | 处理 |
|---|---|---|---|---|---|---|
| 1 | `base_url` | 渠道内联上游基址 | 渠道（仅内联） | `channels.base_url` | 仅 `channel_protocol_id IS NULL` 时读 | 改语义（引用协议时强制 `''`，写入非空 → 400） |
| 2 | `base_url` | 协议默认基址 | 协议 | `channel_protocols.base_url` | 引用协议时基址唯一来源 | 不变 |
| 3 | `protocols[p].baseUrl` | 单协议基址 | 协议 | `channel_protocols.protocols` | 协议级优先 | 不变 |
| 4 | `models[m].baseUrl` | 模型级基址 | — | — | **无** | **废弃**（导入/导出容忍但丢弃；要按协议区分请用 #3） |
| 5 | `path` | 单协议默认路径 | 协议 | `protocols[p].path` | 端点拼接 | 不变 |
| 6 | `default` | 首选协议标记 | 协议 | `protocols[p].default` | 协议选择 | 不变（同协议内至多 1 个，`service.go:152`） |

### 3.2 可达协议集

| # | 字段 | 含义 | 归属 | 目标存储 | 运行时读点 | 处理 |
|---|---|---|---|---|---|---|
| 7 | `protocol` | 内联模式单一基础协议 | 渠道（仅内联） | `channels.protocol` | 仅内联模式；`openai-compat`→`openai-chat` 归一 | 改语义（引用协议时忽略，写入即 400） |
| 8 | `protocols` | 协议能力集 | 协议 | `channel_protocols.protocols` | 模型行无覆盖时的能力集 | 不变 |
| 9 | `models[m].protocols` | 协议内模型级子集 | 协议 | 同上 JSONB | **不再回落读** | 种子 |
| 10 | `protocols` | 模型级可达协议覆盖 | 模型 | `channel_models.protocols` | 非空即最高优先 | 不变（确认为唯一运行覆盖） |

### 3.3 模型商业字段

| # | 字段 | 含义 | 归属 | 目标存储 | 运行时读点 | 处理 |
|---|---|---|---|---|---|---|
| 11 | `models[m].externalModelID` | 绑定的对外模型 | 协议（种子） | JSONB | 仅物化 | 种子（口径写死 + 加测试防回退） |
| 12 | `external_model_id` | 同上 | 模型 | `channel_models.external_model_id` | 路由/计费 | 不变（**唯一真源**） |
| 13 | `models[m].costRates` | 成本三段 | 协议（种子） | JSONB | 仅物化 | 种子 |
| 14 | `cost_rates` | 成本三段 | 模型 | `channel_models.cost_rates` | `handler.go:1928` | 不变（**唯一真源**） |
| 15 | `models[m].timeConfig` | 时段系数 | 协议（种子） | JSONB | 仅物化 | 种子 |
| 16 | `time_config` | 时段系数 | 模型 | `channel_models.time_config` | `handler.go:1929` | 不变（**唯一真源**） |
| 17 | `models[m].contextTiers` | 上下文分档 | 协议（种子） | JSONB | 仅物化 | 种子 |
| 18 | `context_tiers` | 上下文分档 | 模型 | `channel_models.context_tiers` | `handler.go:1930` | 不变（**唯一真源**） |
| 19 | `models[m].convertEnabled` | 是否允许跨协议转换 | **协议（运行时真源）** | JSONB | `gateway/conversion.go:55` 不变 | 不变（**协议独占**，裁定 ②） |
| 20 | `models[m].convertTargets` | 转换目标协议偏好 | **协议（运行时真源）** | JSONB | 同上 | 不变（协议独占） |
| 30 | `edited` | 实例行商业字段是否被本地编辑 | 模型 | `channel_models.edited`（**新列**） | 物化守卫 | 新增 |

### 3.4 请求头 / 错误策略 / 特性（协议独占，无渠道级）

| # | 字段 | 含义 | 归属 | 目标存储 | 处理 |
|---|---|---|---|---|---|
| 21 | `request_headers.*` | 透传模式 + 名单 + 覆盖 | 协议 | `channel_protocols.request_headers` | 不变（同协议多渠道共享，要差异化就建独立协议） |
| 22 | `error_policy[]` | 协议级错误处置 | 协议 | `channel_protocols.error_policy` | 不变（同上） |
| 23 | `features[]` | 协议特性挂载 | 协议 | `channel_protocols.features` | 不变 |

### 3.5 运行策略（分层继承，不是双写）

| # | 字段 | 含义 | 归属 | 目标存储 | 处理 |
|---|---|---|---|---|---|
| 24 | `rate_limit` | 限流窗口 | 渠道级 + 模型级 | `channels.rate_limit` / `channel_models.rate_limit` | 不变（**逐字段** 0 值→渠道级→默认，`manager.go:402`） |
| 25 | `health_probe` | 探测节奏/阈值 | 渠道级 + 模型级 | 同上 | 不变（`manager.go:385`） |
| 26 | `reliability` | 可靠性滑动窗口 | 渠道级 + 模型级 | 同上 | 不变（`manager.go:384`） |
| 27 | `state` | 运行状态 | 密钥级 + 模型级 | `channel_keys.state` / `channel_models.state` | 不变（渠道无独立状态，0009 已删） |
| 28 | `priority` / `tags` / `name` | 路由与身份 | 渠道 | `channels.*` | 不变（协议 `name` 是模板名，不同实体） |
| 29 | `credential_enc` | 凭据密文 | 密钥 | `channel_keys.credential_enc` | 不变（协议侧 `auth` 列 0010 已删） |

**处理统计（共 30 行）**：

| 处理 | 条数 | 明细（表内序号） |
|---|---|---|
| 不变 | 21 | 2、3、5、6、8、10、12、14、16、18、19、20、21、22、23、24、25、26、27、28、29 |
| 种子 | 5 | 9、11、13、15、17 |
| 新增 | 1 | 30（`edited`） |
| 改语义 | 2 | 1、7 |
| 废弃 | 1 | 4 |

> 表内序号到 30 为止（#30 为新增列，排在 3.3 末尾便于与实例字段同表阅读）。

---

## 4. 目标数据模型

### 4.1 DDL（迁移 `0019_channel_field_ownership.sql`）

```sql
-- 唯一新增业务列：实例侧商业字段编辑标记（物化守卫，R6）
ALTER TABLE channel_models ADD COLUMN IF NOT EXISTS edited BOOLEAN NOT NULL DEFAULT false;

-- 仅用于「基址随协议」迁移的可回滚留档（不进任何业务读路径）
CREATE TABLE IF NOT EXISTS channel_base_url_archive (
    channel_id  BIGINT PRIMARY KEY,
    base_url    TEXT NOT NULL DEFAULT '',
    protocol    TEXT NOT NULL DEFAULT '',
    archived_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**不改的既有约束**（Core 不要动）：
- `channel_protocols`：`uq_channel_protocols_active(name) WHERE deleted_at IS NULL`、`uq_channel_protocols_preset(preset_source_id)`、`source_status CHECK`。
- `channels`：`uq_channels_active(name)`、`channels.channel_protocol_id REFERENCES channel_protocols(id)`（**不带 ON DELETE**，删除协议靠「仍被引用则拒绝」，`channelprotocol/service.go:325`）。
- `channel_models`：`uq_channel_models_active(channel_id, internal_model_id)`、`external_model_id NOT NULL`、`state CHECK`。

### 4.2 Go 结构体变更

| 结构体 | 文件:行 | 变更 |
|---|---|---|
| `Channel` | `channel/store.go:89` | 无新字段（基址语义收窄靠服务层校验，不加列） |
| `ChannelInput` | `channel/service.go:47` | 无新字段；`BaseURL`/`Protocol` 语义收窄为内联专用 |
| `ChannelView` | `channel/manager.go:190` | +`BaseURLEffective`（只读派生）；两处构造点 `manager.go:995`、`detail.go:96` 同时补（`detail.go` 目前连 `ChannelProtocolID` 都没传，顺手补） |
| `ChannelModel` | `channel/model_store.go:22` | +`Edited bool` |
| `ChannelModelInput` | `channel/model_service.go:15` | +`Edited` 不入参（服务端计算），仅 `RateLimit/HealthProbe/Reliability` 等既有字段 |
| `viewChannelModel` | `channel/handler.go:220` | +`Edited bool` |
| `channelprotocol.ChannelProtocol` | `channelprotocol/model.go:117` | 不变（`Models` 仍是种子载体；`convert*` 仍为运行时真源） |
| `channelprotocol.EffectiveModelProtocols` | `model.go:223` | **删除**（强制编译报错定位调用点） |
| `channelprotocol.EffectiveModelConvert` | `model.go:238` | **保留不改**（裁定 ②） |

⚠ Core 注意：`channel_models` 有**三处列清单**必须同步追加 `edited`，漏一处会静默丢字段：
`cmCols`（`model_store.go:43`）、`cmColsJoinExt`（`:110`）、`cmColsJoinExtChannel`（`:166`）。

### 4.3 对外 DTO（前端契约）

| 接口 | 字段 | 变化 |
|---|---|---|
| `GET /admin/channels`、`GET /admin/channels/{id}` | `BaseURLEffective` | 新增；**只读派生**，最终生效地址（走 §5.1 解析链） |
| 同上 | `BaseURL` | 语义收窄为「内联值」；引用协议时恒为 `''` |
| `POST/PUT /admin/channels` | `BaseURL` | 引用协议时**非空即 400**（§9 裁定 ③，错误文案见 §5.1） |
| 同上 | `Protocol` | 引用协议时**忽略并落空**（不 400）：老前端总会回传默认值 `openai-compat`，400 会拦住一切正常保存；该值在引用模式下从不参与解析，忽略不改变行为 |
| `GET/POST/PUT /admin/channels/{id}/models` | `Edited` | 新增（只读回显；置位由服务端判定，见 §9 裁定 ④） |
| `GET/POST/PUT /admin/channel-protocols` | `Models[m].baseUrl` | **废弃**：仍接受并忽略（全局未知字段容忍约定），不再回传 |

不改的契约：`ModelPricing` 依赖的 `CostRates` 键仍是 **PascalCase**（`billing.WireRates`，`billing/rates.go:77`），存储列仍是 **snake**（`billing.Rates`）。**这条不变**，迁移 SQL 比较时必须显式映射（见 §6.1）。

---

## 5. 取值口径（每字段唯一解析链）

### 5.1 上游基址与协议

```
if channel_protocol_id IS NULL:            # 内联模式
    base     = channels.base_url           # 必填，校验 http(s)://
    protocol = canonical(channels.protocol) # 必填，非法值回落 openai-chat
else:                                       # 引用协议
    assert channels.base_url == ''          # 写入非空 → 400
    channels.protocol = ''                  # 写入值忽略并落空（不 400，见 §4.3）
    base = first_non_empty(
        channel_protocols.base_url,                       # 协议默认基址
        channel_protocols.protocols[协议].baseUrl)        # 单协议基址
```

**400 错误文案（Core 照抄，必须能指导操作）**：
> `引用了渠道协议时不允许设置内联 BaseURL：基址跟随被引用协议（<协议名>）；如需独立基址，请新建一个渠道协议并在渠道中引用它。`

`Protocol` 不做 400：见 §4.3 的理由（老前端恒定回传默认值，400 会误伤正常保存）。

实现落点：`gateway/protocol.go:198 effectiveBaseURL`、`channel/probe.go:27`（探测与真实转发必须共用同一函数，现状已共用，不要分叉）。

### 5.2 可达协议集

```
model_row.protocols 非空        → 用它（canonical 归一）
else 引用协议                    → channel_protocols.protocols
else 内联                        → { canonical(channels.protocol) }，非法值回落 openai-chat
```

`channel_protocols.models[m].protocols` **不出现在运行时链上**。迁移会把已有值复制进 `channel_models.protocols`，因此存量行为不变。

### 5.3 模型商业字段与转换

| 字段 | 运行时唯一读点 |
|---|---|
| 成本 / 时段 / 分档 | `channel_models`（`gateway/handler.go:1928-1930`）——已如此，不改 |
| 对外模型绑定 | `channel_models.external_model_id`（NOT NULL）——已如此，不改 |
| 跨协议转换 | **`channel_protocols.models[m].convertEnabled/convertTargets`**（`gateway/conversion.go:55`）——协议独占，不改；渠道无法覆盖 |

### 5.4 物化规则（Core 必须逐条实现）

对某协议 P 的每个 `Models[m]`（`m` = 内部模型 ID，`btrim` 后比较，**大小写敏感**）：

1. `externalModelID <= 0` → **跳过**（`external_model_id NOT NULL`，沿用 D5）。
2. 实例行不存在 → 插入，`edited=false`，`state=NORMAL`。
3. 实例行存在且 `edited=true` → **跳过该行所有商业字段**（绑定/成本/时段/分档/协议子集）。
4. 实例行存在且 `edited=false` → 逐字段写，**但种子缺省的键不写**（保留现值）：
   - `? externalModelID` 才写 `external_model_id`；
   - `? costRates` 才写 `cost_rates`（**这就是 B1 的修法**：不再把 nil 写成 JSON null）；
   - `? timeConfig` / `? contextTiers` / `? protocols` 同理。
5. 永远不覆盖 `state` / `rate_limit` / `health_probe` / `reliability`（现状已如此，`service.go:386-389`）。
6. 永远不碰 `convert_*`（协议独占，实例无此列）。
7. 永远不删除协议未列出的实例行（现状已如此）。

### 5.5 运行策略继承（不是双写）

`rate_limit` / `health_probe` / `reliability` 三层：**模型级 0 值 → 渠道级 0 值 → 默认**，逐字段独立（`manager.go:402 mergeRateLimit`、`:384 mergeReliability`、`:385 mergeHealthProbe`）。
口径明确：**0 表示「继承」，不表示「不限」**，本任务不改；若要「显式不限」需另立字段（不在本次范围）。

---

## 6. 迁移方案

### 6.1 顺序（Migration 是「每文件一个事务」，`db/migrate.go:69`）

`0019_channel_field_ownership.sql` 单个文件内按序：

```sql
-- (a) 加列 + 留档表（见 §4.1，幂等 IF NOT EXISTS）

-- (b) 存量：引用**生效中**协议的渠道，原内联基址/协议类型先归档再清空（基址随协议，裁定 ①）
--     归档与清空必须用**完全相同的谓词**（见下方「集合相等」不变量）
INSERT INTO channel_base_url_archive(channel_id, base_url, protocol)
SELECT c.id, btrim(c.base_url), btrim(c.protocol)
FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE c.deleted_at IS NULL
  AND (btrim(c.base_url) <> '' OR btrim(c.protocol) <> '')
ON CONFLICT (channel_id) DO NOTHING;

UPDATE channels c SET base_url = '', protocol = '', updated_at = now()
FROM channel_protocols cp
WHERE cp.id = c.channel_protocol_id
  AND cp.deleted_at IS NULL
  AND c.deleted_at IS NULL
  AND (btrim(c.base_url) <> '' OR btrim(c.protocol) <> '');

-- (c) 存量：种子里的「模型级协议子集」下移到实例（唯一运行时真源成立的前提）
UPDATE channel_models cm
SET protocols = cp.models -> cm.internal_model_id -> 'protocols'
FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE cm.channel_id = c.id
  AND cm.deleted_at IS NULL
  AND cp.models ? cm.internal_model_id
  AND cp.models -> cm.internal_model_id ? 'protocols'
  AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'protocols') = 'object'
  AND (cp.models -> cm.internal_model_id -> 'protocols') <> '{}'::jsonb
  AND (cm.protocols IS NULL OR cm.protocols = '{}'::jsonb OR jsonb_typeof(cm.protocols) <> 'object');

-- (d) 存量：实例行与种子「双方都有值且不一致」时标记 edited=true（物化守卫）
UPDATE channel_models cm
SET edited = true
FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NULL
WHERE cm.channel_id = c.id AND cm.deleted_at IS NULL
  AND cp.models ? cm.internal_model_id
  AND cp.models -> cm.internal_model_id ?| ARRAY['externalModelID','protocols','costRates','timeConfig','contextTiers']
  AND (
       (cp.models -> cm.internal_model_id ? 'externalModelID'
        AND NULLIF(btrim(cp.models -> cm.internal_model_id ->> 'externalModelID'),'') IS NOT NULL
        AND cm.external_model_id IS DISTINCT FROM NULLIF(btrim(cp.models -> cm.internal_model_id ->> 'externalModelID'),'')::bigint)
    OR (cp.models -> cm.internal_model_id ? 'protocols'
        AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'protocols') = 'object'
        AND (cp.models -> cm.internal_model_id -> 'protocols') <> '{}'::jsonb
        AND cm.protocols IS NOT NULL AND jsonb_typeof(cm.protocols) = 'object'
        AND cm.protocols <> '{}'::jsonb
        AND cm.protocols IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'protocols'))
    OR (cp.models -> cm.internal_model_id ? 'costRates'
        AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'costRates') = 'object'
        AND cm.cost_rates IS NOT NULL AND jsonb_typeof(cm.cost_rates) = 'object'
        AND cm.cost_rates <> '{}'::jsonb
        AND (
             (cp.models -> cm.internal_model_id -> 'costRates' ? 'Input'
              AND (cp.models -> cm.internal_model_id -> 'costRates' ->> 'Input') ~ '^-?[0-9]+(\.[0-9]+)?$'
              AND COALESCE((cm.cost_rates ->> 'input') ~ '^-?[0-9]+(\.[0-9]+)?$', false)
              AND (cm.cost_rates ->> 'input')::numeric
                  IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'costRates' ->> 'Input')::numeric)
          OR (cp.models -> cm.internal_model_id -> 'costRates' ? 'Output'
              AND (cp.models -> cm.internal_model_id -> 'costRates' ->> 'Output') ~ '^-?[0-9]+(\.[0-9]+)?$'
              AND COALESCE((cm.cost_rates ->> 'output') ~ '^-?[0-9]+(\.[0-9]+)?$', false)
              AND (cm.cost_rates ->> 'output')::numeric
                  IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'costRates' ->> 'Output')::numeric)
          OR (cp.models -> cm.internal_model_id -> 'costRates' ? 'CacheRead'
              AND (cp.models -> cm.internal_model_id -> 'costRates' ->> 'CacheRead') ~ '^-?[0-9]+(\.[0-9]+)?$'
              AND COALESCE((cm.cost_rates ->> 'cache_read') ~ '^-?[0-9]+(\.[0-9]+)?$', false)
              AND (cm.cost_rates ->> 'cache_read')::numeric
                  IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'costRates' ->> 'CacheRead')::numeric)
        ))
    OR (cp.models -> cm.internal_model_id ? 'timeConfig'
        AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'timeConfig') = 'object'
        AND cm.time_config IS NOT NULL AND jsonb_typeof(cm.time_config) = 'object'
        AND cm.time_config IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'timeConfig'))
    OR (cp.models -> cm.internal_model_id ? 'contextTiers'
        AND jsonb_typeof(cp.models -> cm.internal_model_id -> 'contextTiers') = 'array'
        AND jsonb_array_length(cp.models -> cm.internal_model_id -> 'contextTiers') > 0
        AND cm.context_tiers IS NOT NULL AND jsonb_typeof(cm.context_tiers) = 'array'
        AND jsonb_array_length(cm.context_tiers) > 0
        AND cm.context_tiers IS DISTINCT FROM (cp.models -> cm.internal_model_id -> 'contextTiers'))
  );
```

#### (b) 的软删口径（三条，属契约）

| # | 规则 | 理由 | 与裁定的关系 |
|---|---|---|---|
| S1 | 渠道引用的是**已软删协议**（`channel_protocols.deleted_at IS NOT NULL`）→ **保持原值，不清空、不归档** | 这不是"可恢复性"问题，是**正确性**问题：运行时 `lookupChannelProtocol`（`channel/manager.go:313-322`）用 `GetByID`（过滤 `deleted_at IS NULL`）解析协议，协议已软删 → `rt.Protocol == nil` → 网关回落读 `channels.base_url` / `channels.protocol`（`gateway/protocol.go:184/198`）。**这两个内联值此刻就是该渠道的生效配置**，清空等于把渠道打向空地址。 | 不与裁定 ① 冲突：裁定说的是「**引用协议**时基址随协议」；协议已不存在时渠道事实上处于内联态，适用内联规则。 |
| S2 | **已软删渠道**（`channels.deleted_at IS NOT NULL`）→ 不产生留档、不处理 | 软删渠道不接流量，清空无收益；且它日后被恢复时还会带着原值。 | 与 R2（实例层）一致，无冲突。 |
| S3 | 渠道**基址与协议均空** → 不产生留档 | 没有可归档的东西；写空行只会污染回滚集合。 | 无冲突。 |

**不变量（回滚正确性的唯一依赖）**：`(b)` 的 `INSERT`（归档）与 `UPDATE`（清空）必须使用**逐字相同的谓词与连接**。由此得到 `归档集合 ≡ 清空集合`，于是「**未归档 ⇒ 未被改动**」，回滚才可能无损。这条要在测试里断言（见 §7.3）。

> 更正记录：v3 草稿的 (b) 只写了 `channel_protocol_id IS NOT NULL`，没有 join 协议的 `deleted_at`，会把「引用已软删协议」的渠道一并清空——**按 S1 作废**，以 Core 实现（`b87e63d`）与本段为准。

**(c) 是契约条款，不是实现方自行发明**，条款如下（Core 的 0019 与之一致）：

| 项 | 口径 |
|---|---|
| 下移对象 | `channel_protocols.models[m].protocols`（模型级协议子集）→ `channel_models.protocols` |
| 为什么必须 | 本任务删除了运行时对种子的回落读（`channelprotocol/model.go:223 EffectiveModelProtocols`）。若不落库，存量里「只配在协议模型级」的子集会在切换后消失，模型可用协议集合变回协议级全集 → **静默放大可达协议**（可能把请求发到上游并不支持的协议）。 |
| 下移条件 | 种子该键存在且为**非空 object**；实例侧 `protocols` 为空（`NULL` / `{}` / 非 object）。逐行判断，不做跨行推断。 |
| 冲突（实例已有值且不同） | **不下移、不覆盖**；(d) 会把它标 `edited=true`，此后协议同步不再碰该行。理由是实例值=本地显式配置，优先级高于模板。 |
| 顺序 | (c) **必须在 (d) 之前**。否则「实例为空」的行会先在 (d) 里被判成漂移并标 `edited`，(c) 再跳过 → 子集丢失且永久冻结。 |
| 可回滚 | **无需回滚**：下移写入的值 = 旧解析链回落得到的值，回滚到旧二进制后解析结果逐字节等价（旧：实例空→种子→协议级；新：实例=种子值）。因此 §6.2 回滚脚本不含本步；`protocols` 列留在原值是**无害**的。 |

**(d) 的三条判定口径**（裁定 ④ 在 SQL 侧的对齐，与实现一致）：

1. **只有种子定义了的键参与判定**。种子没定义该键 → 该行这项配置与种子无关，不得标 `edited`（否则会永久冻结协议同步）。
2. **只有「双方都有值且不等」才算漂移**。实例侧为 `NULL` / `{}` / 空数组 = **未设置**（不是"本地改成了空"），不算漂移，留给 (c) 或后续物化填值。这条是 (c) 能生效的前提。
3. **成本必须逐键按 numeric 比较**：种子 `costRates` 是 Pascal 键（`billing.WireRates`），实例 `cost_rates` 是 snake 键（`billing.Rates`），整体 `IS DISTINCT FROM` 恒为真会误标全表；`1` 与 `1.0` 不算差异；非数字脏值跳过（迁移不得中断）。

> 更正记录：v2 草稿里 (c) 写的是整体 `jsonb_strip_nulls(...)` 比较且没有「实例非空」前置条件，会把「实例 `protocols` 为 NULL」的行误标 `edited`，从而让下移失效。**以本节为准，草稿作废**；Core 的实现是对的。

### 6.1.1 已验证

**Ark 本地（2026-11-10，PG17 dev 库，`BEGIN … ROLLBACK`，不落库）**：(a)(b)(d) 与 §6.2 回滚按断言跑通——引用协议 + 非空 `base_url` 归档并清空；仅 `protocol` 非空（默认值 `openai-compat`）同样归档并清空；内联渠道不动；与种子一致不标 `edited`、成本偏离的标、种子无该键的不标；回滚能从归档表还原 `base_url` 与 `protocol`。

**v3 补正后复验（同一环境）**：§6.1(c)(d) 五条断言全过——实例 `protocols` 为 NULL → 下移写入种子值且不标 `edited`；实例有值且不同 → 不下移、标 `edited`；成本 `1` vs `1.0` → 不标；种子无 `costRates` → 不标；种子有 `timeConfig` 而实例 NULL → 不标。

**v4 软删口径复验（同一环境，造 C1~C5 五类渠道）**：引用生效协议 + 非空值 → 归档并清空；引用**已软删协议** → 逐字不动且不进归档表；**已软删渠道** → 不归档、不清空；基址与协议均空 → 不归档；内联渠道 → 不动；**「被清空但未归档」的渠道数为 0**（集合相等）；回滚只还原归档过的渠道，S1 渠道不受影响。

**Core 实现侧（PR #75 自测）**：迁移正反向、回填幂等（跑两遍逐行一致）、断点续跑（限批模拟中断后继续）均通过；四个裁定点各有用例。

⚠ 由此确认一个实现细节：`channels.protocol` 有默认值 `'openai-compat'`，所以**归档表的行数 ≈ 引用「生效中」协议的渠道数**（不止「基址非空」的那些；引用已软删协议的渠道按 S1 不归档）。这张表只做留档，体量等同渠道数，可忽略；`0020` 里凭 spencer 确认后清理即可。

Core 落地时把这段搬进 `db/migrate_test.go`（已有迁移测试骨架）。

### 6.2 可回滚路径

代码回滚 + 手工执行（**不要放进 `migrations/`，会被自动应用**）：`backend/scripts/rollback-0019.sql`

```sql
-- 1) 还原被清空的内联基址与协议类型
UPDATE channels c SET base_url = a.base_url, protocol = a.protocol
FROM channel_base_url_archive a
WHERE c.id = a.channel_id
  AND c.deleted_at IS NULL
  AND c.channel_protocol_id IS NOT NULL
  AND btrim(c.base_url) = '';

-- 2) 撤销列与留档表（确认无需再回溯后执行）
ALTER TABLE channel_models DROP COLUMN IF EXISTS edited;
DROP TABLE IF EXISTS channel_base_url_archive;
```

**回滚后老代码仍可运行**：老二进制读 `channels` / `channel_models` 用的是显式列清单（`channelCols` `store.go:140`、`cmCols` `model_store.go:43`），多/少一个 `edited` 列不影响查询；基址与协议类型已按归档还原，行为等价于迁移前。**迁移有损点为零**（归档覆盖 `base_url` 与 `protocol` 两列）。
**唯一"不回滚"的一项**：§6.1(c) 写进 `channel_models.protocols` 的值**不回滚**——它与旧解析链回落得到的值相同，回滚后行为逐项等价，留着无害（见 §6.1 下移条款「可回滚」行）。

**未归档的渠道靠什么保证可还原**（按 §6.1 S1~S3 分三类，全部是「没被动过」，不需要还原）：

| 类别 | 迁移期间发生了什么 | down 时的还原路径 | 风险 |
|---|---|---|---|
| 引用**已软删协议**的渠道（S1） | 什么都不做，`base_url`/`protocol` 原样保留 | **无需动作**（值从未被改）。回滚脚本是 `UPDATE channels … FROM channel_base_url_archive`，它不在归档表里 → 天然涉及不到 | 无 |
| 已软删渠道（S2） | 不归档、不清空 | 无需动作 | 无 |
| 基址与协议均空的渠道（S3） | 不归档（本来就没值） | 无需动作 | 无 |
| 引用**生效中**协议、且内联值非空 | 归档 + 清空 | 归档表还原（脚本第 1 步） | 无（前提是 §6.1「集合相等」不变量成立） |

> 由此，回滚的可还原性**不依赖任何外部备份**，只依赖一条不变量：**归档集合 ≡ 清空集合**。若将来有人只改其中一条语句的谓词（例如给 `UPDATE` 加了条件但忘了给 `INSERT` 加），回滚就会静默丢地址——所以 §7.3 把「两集合相等」列为 Gate 必测项。

### 6.3 老数据兼容与读写窗口

| 场景 | 处理 |
|---|---|
| 引用生效协议 + `base_url` 非空（B2 存量） | 归档后清空；**生效基址改为协议基址**（裁定 ① 的直接后果，行为变更需在 PR 里列出受影响渠道清单） |
| 引用生效协议 + `protocol` 非空（脏数据） | 清空（该值本来就不被读） |
| 引用**已软删协议** + 内联值非空 | **完全不动**（S1）：此时内联值就是生效配置，清了渠道就打空地址 |
| 已软删渠道 / 基址协议全空 | 不归档、不处理（S2/S3） |
| 内联渠道（未引用协议） | 完全不动 |
| 实例行与种子不一致 | `edited=true`，**不再被同步覆盖**；需要恢复由新接口重置 |
| 种子有模型级 `protocols`、实例为空 | 由 §6.1(c) 下移写入实例（值 = 旧解析链回落结果，回滚无需撤销） |
| 实例行与种子一致 | `edited=false`，继续跟随协议更新 |
| 种子缺某商业键 | 物化不写该列（B1 修复），现值保留 |
| 迁移期间旧后端仍在跑 | 旧后端仍读 `channels.base_url`（已清空）→ 引用协议的渠道回落到协议地址，与裁定一致。**旧前端保存渠道会把空值原样回传，不会触发 400**；只有「手工填了基址」才会 400。因此后端与迁移可同批发布。 |
| 空库 | 全部语句幂等，无数据操作，可重复执行 |

**新增接口**（Core 实现）：`POST /admin/channels/{id}/models/{mid}/reset-from-protocol` → `edited=false` + 重新物化该行（返回新的实例行）。这是 `edited` 的唯一出口，避免「标记后再也同步不了」。

### 6.4 上线前置预检（可照做，不是"要注意"）

**谁在什么时候跑**：由 **spencer 或其指定的生产运维** 执行；时点 = **0019 在生产库执行之前**（迁移随应用启动自动执行，所以要在此之前，或首次启动前先手工 `psql` 跑）。架构/Core **没有生产读权限，不代跑、不代读**。

**第 1 步：执行（只读，脚本全是 SELECT）**

```bash
# 1) 建议先开只读事务，防止手滑
PGOPTIONS='-c default_transaction_read_only=on' \
psql "$PROD_DSN" -v ON_ERROR_STOP=1 \
     -f backend/scripts/0019-preflight-audit.sql \
     > 0019-preflight-$(date +%F-%H%M).log 2>&1
```

**第 2 步：读输出并处置**

| 输出段 | 含义 | 判读与处置 |
|---|---|---|
| `1) 存量计数` | 四张表的 `rows_all` / `rows_live` | 记下来，作为 §10 的分母；**若 `channel_models.rows_live > 50000` → 走 §10.2 的独立回填路径**，不要直接让迁移跑 |
| `1b) 引用协议的渠道分布` | 每个协议被多少渠道引用 | 判断影响面集中度；单协议引用很多说明协议拆分成本高 |
| `2) 裁定①影响面` | 引用**生效中**协议**且**内联 `base_url`/`protocol` 非空的渠道清单（含 `inline_base_url_to_be_archived`、协议侧 `protocol_base_url`） | **非空就要逐条决策**（见下）。口径已按 §6.1 S1 过滤 `cp.deleted_at IS NULL`：引用**已软删协议**的渠道**不会**出现在此段（它们保持原值，不需要处置） |
| `2b) 高危：协议自身无基址` | 清空后**没有可用地址**的渠道 | **阻断项**：必须先给协议补基址（或把渠道改回内联）。有任意一行都不允许跑迁移。口径同样只看生效协议（`cp.deleted_at IS NULL`），软删协议渠道不计入——它们的内联值仍在生效，不受迁移影响 |
| `2c) 引用已软删协议`（**待补**） | 引用了已删除协议、因此处于「内联兜底」状态的渠道 | **信息项，不阻断**：迁移不动它们。但这类渠道的协议已不存在，属于既有故障，需人工修复引用（重新激活/换协议，或确认改回内联） |
| `3) 预估 edited 行数` | **候选行数上界**（种子定义了任一商业字段的行），不是「将标 `edited` 的确切行数」 | 只用来估首次同步覆盖面。要精确值就按 §6.1(d) 的 `WHERE` 条件跑 `select count(*)` |
| `3b) 协议配了转换的模型数` | 裁定 ② 的现状核对 | 只做记录，不触发动作 |
| `4) 空成本行` | 种子缺 `costRates` 且实例成本为空的行（B1 遗留） | 迁移后由协议调价自动补上；若 > 0，说明历史上这些模型**当前计费会报错**，需在上线后确认已恢复 |
| `5) 量级判断` | 脚本最后打印的建议路径 | 与 §10.2 一致 |

**第 3 步：`2)` 非空时逐条三选一**（每条渠道只选一种，结论写进上线记录）

| 选项 | 适用 | 操作 |
|---|---|---|
| A. 改协议 | 这些渠道本来就该用同一个地址（旧内联值是历史残留） | 确认协议 `base_url` 正确 → 直接跑迁移（内联值被归档） |
| B. 新建协议 | 渠道确实需要独立地址 | **迁移前**复制该协议为新协议（命名 `<协议名>-<渠道名>`）→ 改该渠道引用新协议 → 跑预检确认 `2)` 不再列出该渠道 → 跑迁移 |
| C. 确认可切换 | 内联值已失效/等价于协议地址 | 无需操作，归档后自动指向协议地址；在执行记录里写「已确认」 |

**第 4 步：放行条件**（四条同时满足才允许跑迁移）

1. `2b)` 结果为 **0 行**；
2. `2)` 的每一行都有 A/B/C 的结论且已执行；
3. `channel_models.rows_live ≤ 50000`，或已改走 §10.2 的独立回填路径；
4. 预检日志已留档（附在 PR 评论或运维记录里，**不含 DSN/口令**）。

**预检脚本待改两处**（与 §6.1 S1 对齐；Core 改完后 `2b)` 的"必须 0 行"前提**不变**，只是集合更准）

```sql
-- 2c) 新增：引用「已软删协议」的渠道（信息项，不阻断；迁移不动它们，需人工修复引用）
\echo '=== 2c) 引用已软删协议的渠道（保持内联兜底，迁移不动）==='
SELECT c.id, c.name, c.channel_protocol_id,
       btrim(c.base_url) AS inline_base_url, btrim(c.protocol) AS inline_protocol
FROM channels c
JOIN channel_protocols cp ON cp.id = c.channel_protocol_id AND cp.deleted_at IS NOT NULL
WHERE c.deleted_at IS NULL
ORDER BY c.id;

-- 2b) 修正：判定「协议没有任何可用基址」要看全部协议键，不能只看 openai-chat
--     （现写法会把「只给 anthropic-messages 配了 baseUrl」的协议误判为高危 → 假阳性阻断上线）
WHERE c.deleted_at IS NULL
  AND btrim(cp.base_url) = ''
  AND NOT EXISTS (
      SELECT 1 FROM jsonb_each(cp.protocols) e
      WHERE btrim(COALESCE(e.value ->> 'baseUrl', '')) <> ''
  )
```

`2b)` 的已知局限（可接受）：它是**粗筛**，只看协议是否一个基址都没有，不按该渠道实际可达协议逐项判定；更细的判定交给上线后的探测/联调，不放进上线阻断条件。

**回滚触发**：只要发现「有渠道的请求打到了非预期地址」，按 §6.2 跑回滚脚本，归档表能把内联基址与协议类型原样还原。

---

## 7. 交接清单

### 7.1 给 Core（实现顺序）

1. **迁移** `backend/internal/db/migrations/0019_channel_field_ownership.sql`（§6.1 逐条；回填规模策略见 §10）。
2. **存储层**：`channel/model_store.go`（ChannelModel + **三处列清单** `cmCols:43` / `cmColsJoinExt:110` / `cmColsJoinExtChannel:166` + Insert/Update）。
3. **服务层**：`channel/service.go` —— `validateBase`（:99）：引用协议时 `BaseURL` 非空 → 400（§5.1 文案）、`Protocol` 一律落空不报错；内联时两者必填。`materializeFromProtocol`（:359）按 §5.4 重写（含 B1 修复）。新增 `ResetModelFromProtocol`。
4. **模型服务**：`channel/model_service.go` —— `UpdateChannelModel` 按 §9 裁定 ④ 做**字段级**置位：先读现状，逐字段比较**归一化后的值**（金额/系数按数值、`protocols` 按 canonical 协议名 + trim、`time_config`/`context_tiers` 按解析后的结构、`external_model_id` 按数值），只有真变才置 `edited=true`；同值重写、键序/格式差异、`0` 与 `0.0` 之类序列化差异**不置位**。`CreateChannelModel` 一律 `edited=true`。
5. **删除模板回落**：`channelprotocol/model.go` 删 `EffectiveModelProtocols`(:223)，改 `gateway/protocol.go:172 effectiveProtocols`；**`EffectiveModelConvert` 不动**。
6. **出参**：`manager.go:190 ChannelView` + 两处构造（`manager.go:995`、`detail.go:96`）。
7. **协议包**：`channelprotocol/source.go` 导入时丢弃 `models[m].baseUrl`（仍在 JSON 里容忍，不报错）。
8. **回填路径（§10.2 缺口）**：按方案 A 给 0019 加阈值守卫；`scripts/backfill-0019` 补齐「协议子集下移」阶段，使三阶段（channels / protocols 下移 / drift 标记）与迁移内 DML 等价；启动时对未完成的 `backfill_checkpoints` 告警。
9. **上线前置**：交付 `backend/scripts/0019-preflight-audit.sql`（已在 PR #75），口径见 §6.4；两处待改——① 补 `2c)`「引用已软删协议」清单（信息项）；② `2b)` 的基址判定改为 `NOT EXISTS (jsonb_each -> 'baseUrl' 非空)`，不再只看 `openai-chat`。`3)` 的标题建议改成「候选行数（上界）」以免被读成精确值。

**必须保持的对外行为**（改坏了 Gate 会挂）：
- `openai-compat` → `openai-chat` 归一（`channelprotocol/model.go:29`）继续生效。
- 内联渠道缺 `base_url`/`protocol` → 400，且非法协议名回落 `openai-chat`（`gateway/protocol.go:184`）。
- 协议被渠道引用时删除协议 → 409「仍被渠道引用」（`channelprotocol/service.go:325`）。
- 预设同步仍以 `preset_source_id` 幂等，`edited=true` 的协议仍需二次确认（`service.go:425`）。
- 请求头策略变更后立即生效、无需重启（refresh 钩子，`channelprotocol/service.go:74`）。
- 转换行为完全不变（仍读协议，`gateway/conversion.go:55`）。
- 所有 JSON 解码**不要**加 `DisallowUnknownFields`（全仓 0 处，未知字段容忍是全局约定）——`models[m].baseUrl` 的「废弃但仍容忍」靠这条。

### 7.2 给 View（前端契约变化）

| 文件 | 改动 |
|---|---|
| `src/api/admin.ts` | `AdminChannel` +`BaseURLEffective`；`ChannelModel` +`Edited`；`ChannelProtocolModelOverride` 去掉 `baseUrl` |
| `views/admin/ChannelCreate.vue` | 引用协议时**隐藏/禁用** Base URL 与协议类型输入，并展示「基址跟随协议：<协议名>（<BaseURLEffective>）」；提交时这两个字段传 `''`。内联模式仍必填 |
| `views/admin/ChannelEdit.vue` | 同 Create；`:401` 的 `BaseURL: form.base_url` 必须在引用协议时传 `''`（否则被 400 拦住）。建议把 400 的 message 直接 `ElMessage.error` 展示 |
| `views/admin/ChannelProtocolEdit.vue` | 删除模型级 `baseUrl` 编辑（`:396-400`、`:452`、`:480`、`:499`）；成本/时段/分档/协议子集标注为「模板默认值，仅对未编辑过的渠道模型生效」；转换配置保留并标注「协议级，所有引用渠道共享」 |
| `components/ChannelModels.vue` | 增加 `Edited` 徽标 + 「重置为协议值」按钮（调 reset 接口）；成本等字段旁提示「保存后该模型不再随协议同步」 |
| `components/ModelPricing.vue` | 不变（`CostRates` Pascal 键口径不动） |

前端铁律：引用协议时**不要让用户以为能改基址**——输入框直接隐藏，而不是禁用后仍回传旧值。

### 7.3 给 Gate（可写成测试的判据）

1. **唯一真源**：`grep -rn "EffectiveModelProtocols" backend/` → 0 命中；`EffectiveModelConvert` 仍在且 `conversion.go` 调用点不变。
2. **迁移可回滚**：空库 + 造数库各跑一次 `0019` → `rollback-0019` → 数据与迁移前一致（`channels.base_url` 逐行等值、`channel_models` 行数不变）。
3. **老数据兼容**：造「引用协议 + 非空 base_url」渠道，迁移后其**有效基址 = 协议基址**（行为变更需在测试里显式断言，不能靠人看）。
4. **行为用例**（表驱动，Gate 直接抄）：
   | 用例 | 期望 |
   |---|---|
   | A 引用 P（P.base_url=X） | 请求打 X |
   | 引用协议时 PUT `BaseURL="https://evil"` | **400**，message 含「基址跟随被引用协议」 |
   | 引用协议时 PUT `Protocol="openai-chat"` | 不报错，落库为 `''`（不参与解析） |
   | A 切回内联（protocol_id=NULL） | `base_url`/`protocol` 必填，缺 → 400 |
   | 改 P.costRates（种子有） | A 未编辑模型行跟着变；`edited=true` 行不变 |
   | 改 P 但种子缺 costRates | 既有 `cost_rates` **不得**被写成 null（B1 回归用例） |
   | 人工把 A 某模型成本改成相同值（同值重写） | `edited` **仍为 false**（裁定 ④） |
   | 人工把某模型成本改成不同值 | `edited=true`，后续同步不覆盖 |
   | 只改模型限流、不动商业字段 | `edited` **仍为 false**（裁定 ④ 的边界） |
   | 改 P 的 convertEnabled=false | 转换停用（仍读协议，`conversion.go`） |
   | `reset-from-protocol` | 该行恢复为种子值且 `edited=false` |
   | 种子有模型级 `protocols`、实例为空 | 迁移后实例值 = 种子值；**运行时可达协议集合与迁移前逐项相同**（下移生效） |
   | 引用**已软删协议** + 内联值非空 | 迁移后 `base_url`/`protocol` **逐字不变**，且**不在** `channel_base_url_archive`（S1） |
   | 已软删渠道 | 不产生留档行（S2） |
   | 基址与协议均空的渠道 | 不产生留档行（S3） |
   | 归档集合 ≡ 清空集合 | `select count(*) from channel_base_url_archive` 等于「本就该清空的渠道数」，且**不存在**「被清空但没归档」的渠道（回滚无损的唯一依赖） |
   | 种子有 `protocols`、实例有且不同 | 不下移、实例值不变、`edited=true`（冲突不覆盖） |
   | 种子有 `protocols`、实例为空但被先标 `edited` | **不允许发生**：断言 (c) 在 (d) 之前，且 (d) 的 `protocols` 分支要求实例非空 |
   | 成本 `1` vs 种子 `1.0` | 不标 `edited`（numeric 比较，裁定 ④） |
5. **三口径全绿**：`go vet ./...`、`go test -race ./...`、`npx vue-tsc --noEmit`。
6. **不做的事**（防误伤）：不加 `DisallowUnknownFields`；不删 `channel_protocols.models` 列；不给渠道加基址覆盖列；不改计费公式与三段 usage 口径。

---

## 8. 风险与取舍

| 风险 | 取舍与理由 |
|---|---|
| 裁定 ① 让「引用协议 + 自定义基址」的存量渠道**改了实际上游地址** | 这是裁定明确接受的方向（宁可逼出显式协议，也不要「同协议两种基址」）。缓解：迁移归档原值 + PR 里列出受影响渠道清单 + 回滚脚本可一键还原。 |
| 物化不再覆盖已编辑行 → 协议调价对不同渠道生效时点不一致 | 有意：静默覆盖用户配置危害更大。补偿：`edited` 徽标 + 一键重置。 |
| 裁定 ③ 硬 400 会让老前端保存失败 | 只有「引用了协议还填了基址」才会 400；老前端从 GET 拿到的是归档前的值吗？——迁移已清空，GET 返回 `''`，老前端原样回传 `''` → 不会 400。真正的风险是**用户在升级前手工填了基址**：会被拦住，这正是裁定要的效果。 |
| 废弃 `models[m].baseUrl` | 它今天**没有任何读取点**（B3），保留只会继续误导。包格式仍容忍该键，不破坏预设包兼容。 |
| 转换仍协议独占 | 裁定 ② 明确：渠道只能选协议，转换行为随协议。想差异化就新开协议——代价是协议数量增加，但没有「同协议不同行为」。 |
| 请求头/错误策略仍协议独占 | 影响**安全**（鉴权头注入、熔断），跨渠道共享是有意的约束；要差异化就建独立协议。 |

---

## 9. 已裁定（spencer 2026-11-10；原「待拍板清单」按此关闭）

| # | 裁定 | 实现约束（必须满足） |
|---|---|---|
| ① | **引用协议不覆盖基址**：基址跟随被引用协议；要改就显式新建协议。 | 不引入任何渠道级基址覆盖列；`channels.base_url` 在 `channel_protocol_id IS NOT NULL` 时一律视为 `''`（读路径不读、写路径 400、迁移归档后清空）。不产生「同协议两种基址」。 |
| ② | **转换按协议定义，不按渠道差异化**：渠道只能选协议；确有差异就新开协议。 | 不新增 `channel_models.convert_*` 列；`EffectiveModelConvert` 与 `gateway/conversion.go:55` 保持读协议；物化不碰转换配置。 |
| ③ | **非空 inner BaseURL 返回 400 明确报错**，不静默转覆盖。 | 400 message 必须可指导操作（§5.1 文案：说明「基址跟随被引用协议」+「如需独立基址请新建协议」）；禁止任何「静默落库成覆盖/静默忽略后照常 200」的兼容分支。**例外（不在裁定范围，按最小伤害处理）**：`Protocol` 字段老前端恒定回传默认值 `openai-compat`，一律忽略并落空，不做 400。 |
| ④ | **`edited` 字段级置位**：字段值真变才置位；格式化/序列化差异、同值重写都不置位。 | 置位判定必须基于**归一化后的语义值**逐字段比较（数值按数值、协议名 canonical + trim、JSON 按解析结构），不得基于原始字节/`reflect.DeepEqual` 比较请求体；只改 `rate_limit`/`health_probe`/`reliability` 不置位。判定逻辑要能单测（同值重写 → false）。 |

---

## 10. 回填规模与来源

### 10.1 计数（来源：**本地 `lingshu_test` 测试库快照，非生产**）

| 表 | 总行数（含软删除） | 其中未删除 |
|---|---|---|
| `channels`（渠道） | **0** | 0 |
| `channel_protocols`（协议） | **0** | 0 |
| `channel_models`（模型） | **0** | 0 |

- 采集命令（只读）：`select count(*) from <table>;`，另按 `channel_protocol_id` 分组统计，结果为空集（无渠道）。
- 同机 `lingshu_dev` 同为 0 行；`lingshu_staging` 无业务表（只有连接可用）。
- **本机没有任何存量数据可用于规模估计**；生产量级待 spencer 提供后修订本节，并同步修订 §6.2 回滚影响面。

### 10.2 量级判断与分批策略

按当前（0 行）量级，回填用「分批 + 断点 + 幂等」当然够——但真正要解决的是**生产未知量级**，所以口径按上限设计：

| 生产 `channel_models` 行数 | 做法 |
|---|---|
| ≤ 5 万 | 直接放 `0019` 迁移里（DDL + 单语句幂等 `UPDATE`），一次事务内完成；JSONB 逐行比较 5 万行在 PG17 上通常 < 2s，锁窗口可接受 |
| > 5 万 | **迁移只做 DDL**；回填改为独立脚本 `backend/scripts/backfill-0019`（Go，按 `id` 升序游标分批，**单批 5000 行**，每批独立事务 + 把已处理 `max(id)` 写入 `backfill_checkpoints(name, last_id)`，失败可断点续跑；语句天然幂等，重复执行无副作用） |

**为什么是 5000/批**：`channel_models` 单行含 5 个 JSONB 列，批太大（如 5 万）会让单事务 WAL 与锁持有时间明显变长，且失败重跑成本高；批太小（如 200）在 10 万行时会产生 500 次往返，收益不抵开销。5000 是「单批 < 1s + 总往返可控」的折中。

⚠ **必须知道的一个限制**：`db/migrate.go:69` 把**每个迁移文件包在一个事务里**，所以「分批 + 断点」**无法写在 0019 的 .sql 里**（只能是一个大事务）。这是上面「≤5 万放迁移、>5 万走独立脚本」这条分界的原因，不是风格偏好。

#### ⚠ 实现缺口（Core 需改，否则 >5 万路径是空话）

**现状问题**：`0019_channel_field_ownership.sql` 里的 (b)(c)(d) 三个 `UPDATE` 是**无条件执行**的。也就是说无论行数多少，迁移都会在启动时用一个事务把全表跑完；`scripts/backfill-0019` 既不能在迁移前跑（那时 `edited` 列还不存在，`markDriftSQL` 直接报错），在迁移后跑也没有任何事可做。**「>5 万走脚本」目前没有行使窗口。**

**契约要求（二选一，Core 定，写进 PR 说明）**：

- **方案 A（推荐，改动小）**：`0019` 内用阈值守卫包住回填，超阈值时**跳过 DML 并显式告警**：

```sql
DO $$
DECLARE n bigint;
BEGIN
    SELECT count(*) INTO n FROM channel_models WHERE deleted_at IS NULL;
    IF n > 50000 THEN
        RAISE WARNING '0019: channel_models 有 % 行（>50000），迁移内回填已跳过；'
                      '请立刻执行 backend/scripts/backfill-0019 并在完成前不要接流量', n;
    ELSE
        -- 此处执行 (b)(c)(d) 三条 UPDATE（原语句不动）
        NULL;
    END IF;
END $$;
```

  配套要求：
  1. `backfill-0019` 必须补齐 (c) 的「协议子集下移」阶段（现在只有 channels + drift 两段，缺这段会让模型级协议子集丢失）；
  2. 应用启动时读 `backfill_checkpoints`：`0019_channel_models` 未完成 → 启动日志 **ERROR** 级告警（是否拒绝启动由 spencer 定，见下"待确认"）；
  3. 回填完成前，网关仍可服务（实例行缺 `protocols` 会退化成协议级全集，属行为放大，不是崩溃）。

- **方案 B（更简单）**：删掉「>5 万走脚本」的说法，承认 0019 是**单事务全量回填**，>5 万时改为**停机维护窗口**执行；`backfill-0019` 仅保留为「迁移被手工跳过后的补救/演练」工具。文档与 README 同步改口径。

**我的建议**：先按 A 落地（守卫 + 补齐下移阶段 + 启动告警），因为它同时修掉「脚本无窗口」和「下移遗漏」两个真问题；若 spencer 不想加启动告警，再退到 B。

### 10.3 上线前要拿的数（由 §6.4 预检脚本一次产出）

1. `channels` 总数，以及「引用协议 且 `base_url`/`protocol` 非空」的行数（裁定 ① 的影响面）→ 预检 `1)` / `2)`；
2. `channel_protocols` 总数，以及 `models` 里含 `convertEnabled/convertTargets` 的协议数（裁定 ② 现状核对）→ 预检 `1)` / `3b)`；
3. `channel_models` 总数，以及与种子不一致的行数（`edited` 初始比例）→ 预检 `1)` / `3)`；
4. 协议自身无基址的高危渠道数 → 预检 `2b)`，**必须为 0** 才放行。
