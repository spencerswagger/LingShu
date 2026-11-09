# 渠道 / 渠道协议字段归属：实现说明与拍板落地（0019）

> Status: IMPLEMENTED（代码 + 迁移 + 测试已落）
> Date: 2026-11-10
> 上游契约：`docs/designs/2026-11-10-channel-field-ownership-contract.md`（Ark，DRAFT-v1）
> 本文只记「怎么落的、和 DRAFT-v1 差在哪、为什么差」——设计意图以契约为准，冲突处以拍板为准。

---

## 1. 拍板四条如何落地

| # | 拍板结论 | 落地位置 | 行为断言（测试） |
|---|---|---|---|
| ① | 引用协议**不覆盖基址**，要改就显式新建协议 | `channel/probe.go:effectiveProtocolBaseURL`、`gateway/protocol.go:effectiveBaseURL`（引用协议时只走协议链）；`channel/service.go:validateBase`（引用协议时提交非空 `BaseURL` → 400） | `TestEffectiveProtocolBaseURL_ProtocolReferenceIgnoresInlineLenacy`、`TestEffectiveBaseURL_ProtocolReferenceIgnoresInlineBaseURL`、`TestValidateBase_RejectsInlineBaseURLWhenProtocolReferenced` |
| ② | 转换**按协议定义、不按渠道差异化** | 新增列 `channel_models.convert_enabled/convert_targets` 作为协议的**物化副本**；运行时读实例列（`gateway/conversion.go`），渠道层**没有**写入路径 | `TestService_Materialize_EditedRowSkipsCommercialButSyncsConvert`、`TestResolveUpstreamProtocol`（`model row disabled` / `model row target order`） |
| ③ | 写入带非空 inner BaseURL → **400** | `channel/service.go:validateBase` | 同上 validateBase 用例 |
| ④ | `edited` **字段级置位**：只有字段值真变才置位 | `channel/model_service.go:commercialFieldsChanged`（成本按三段数值比、协议名 canonical、时段/分档按结构比） | `TestCommercialFieldsChanged_FieldLevel`、`TestUpdateChannelModel_EditedFlag` |

`edited` 的唯一出口：`POST /api/v1/admin/channels/{id}/models/{mid}/reset-from-protocol`
（恢复种子值 + 置回 `edited=false`；种子未定义的键保持现值）。

---

## 2. 与 DRAFT-v1 的差异（必须知道的三条）

### 2.1 `base_url_override` 列**没有**加（拍板①）

DRAFT-v1 设计的是「新增覆盖列、留空=继承」，理由是「完全禁止覆盖会改掉存量渠道实际地址」。
拍板选了后者（备选方案），因此：

- 不加 `base_url_override`；新增列由 4 个变 **3 个**（`convert_enabled` / `convert_targets` / `edited`）。
- **存量行为会变**：引用协议且 `channels.base_url` 非空的渠道，上线后走**协议地址**（不再是那列历史值）。
- 迁移**不改写** `channels.base_url`：运行时忽略它，保留原值使「回滚老二进制即恢复原地址」成立（零数据破坏）。
- 影响面必须在上线前盘点：`backend/scripts/0019-preflight-audit.sql`（第 2 段列出会变地址的渠道，
  第 2b 段列出「协议也没配基址」的高危渠道——这些必须先建协议再上线）。

### 2.2 迁移补了 DRAFT-v1 §6.1 漏掉的一步（契约缺口）

§5.2 承诺「迁移会把 `models[m].protocols` 复制进 `channel_models.protocols`，存量行为不变」，
但 §6.1 的 SQL 里**没有**这一步。若不补，降级为种子后凡是「实例 protocols 为空、靠种子回落」的
模型行会突然丢掉模型级协议子集（行为回退）。迁移 0019 用 (b) 段补上，只写「实例为空」的行，不覆盖本地值。

### 2.3 `models[m].baseUrl` 废弃的落点（B3）

包格式仍容忍该键（全仓无 `DisallowUnknownFields`），但 `channelprotocol.normalizeIn` 在落库前清空它，
不再回显。原因：它从来没有运行时读点，存下来只会让管理员以为配了生效。

---

## 3. 迁移 0019 的四段与回滚

| 段 | 内容 | 幂等性 |
|---|---|---|
| (a) | 加 3 列（`IF NOT EXISTS`）；`convert_targets NOT NULL DEFAULT '[]'`、`edited NOT NULL DEFAULT false` | 是 |
| (b) | 种子的模型级 `protocols` 下沉到实例（仅实例为空时） | 是 |
| (c) | 种子的 `convertEnabled/convertTargets` 下沉（仅种子定义了的键） | 是 |
| (d) | 漂移标记 `edited=true`（种子定义了 + 双方都有值 + 值不等；成本按 `Input/Output/CacheRead` → snake 逐键 numeric 比较） | 是 |

回滚：`backend/scripts/rollback-0019.sql`（纯 SQL，可 psql 直接跑；**不在** `migrations/` 下）。
回滚损失面：仅 `edited` 标记与两列副本（种子仍在协议里，可重建），`channels.base_url` 全程未动。

**已验证**：`TestMigrate_0019_FieldOwnership` 在本地 PG17 临时库真跑——搬迁、转换下沉、漂移标记、
B1 不写 null、幂等二次执行、回滚脚本（删列 + 数据保真）逐条断言；`TestMigrate_FullChain` 保证全新库
按序应用全链不冲突。

---

## 4. 存量计数（测试库快照，非生产）

按拍板：「没有生产读权限，用 `lingshu_test` 计数并标清来源」。

| 表 | 行数 | 来源 |
|---|---|---|
| `channels` | 0 | `lingshu_test` 快照（2026-11-10） |
| `channel_protocols` | 0 | 同上 |
| `channel_models` | 0 | 同上 |
| `channel_keys` | 0 | 同上 |

结论：本地三库皆空，回填与漂移标记只能在造数库上验证（已做）。**生产影响面待 spencer 提供**：
把 `scripts/0019-preflight-audit.sql` 在只读账号上跑一次即可拿到「三表计数 + 会变地址的渠道清单 +
预估被标 edited 的行数」。回填实现按「分批 + 断点 + 幂等」设计，不依赖生产量级：
迁移是单事务的全表 UPDATE（PG 对已存在行加行锁），量级大时可先按 `channels.id % N` 分段执行 (b)(c)(d) 三段。

---

## 5. 门禁

- `gofmt`：本次改动文件干净（仓库既有 15 个历史不合规文件未动）。
- `go vet ./...`：通过。
- `go test -race`（除 `tests/` e2e）：29 个包全绿；`internal/db` 的 3 个迁移集成测试在 PG17 上真跑通过。
- `tests/` e2e 的 `TestE2E_GatewayBillingLoop` 失败**属既有问题**（CI 里该套件本就是
  `continue-on-error` 的信息性步骤：脚手架未装配渠道密钥服务），与本次改动的包无交集。

---

## 6. 给下游的两条

- **View**：`ChannelView` 新增只读派生 `BaseURLEffective`（最终生效地址）；`BaseURL` 在引用协议时是
  历史遗留值（运行时忽略），**不要**再回传给后端（会 400）。模型行新增 `ConvertEnabled`（三态：null=继承/
  true/false）、`ConvertTargets`、`Edited`（徽标 + 「重置为协议值」按钮调 `reset-from-protocol`）。
- **Gate**：契约 §7.3 的用例 A「override=Y 打 Y」在拍板①下**不再成立**（改为「引用协议渠道的
  `base_url` 被忽略，打协议地址」）；其余用例不变。
