# 渠道 / 渠道协议字段归属：实现说明（0019 · RULED-v2）

> Status: IMPLEMENTED（代码 + 迁移 + 回填脚本 + 测试已落）
> Date: 2026-11-10
> 上游契约：`docs/designs/2026-11-10-channel-field-ownership-contract.md`（Ark，**RULED-v2**，四项口径已裁定）
> 本文只记「怎么落的、边界在哪、上线怎么走」；设计意图以契约为准。

---

## 1. 落地结果一览

| 维度 | v1（DRAFT）设计 | 实际落地（RULED-v2） |
|---|---|---|
| 新增业务列 | 4（`base_url_override` / `convert_enabled` / `convert_targets` / `edited`） | **1**（`channel_models.edited`） |
| 新增辅助表 | 无 | `channel_base_url_archive`（留档，使清空基址可无损回滚） |
| 渠道级基址覆盖 | 允许（新列） | **不存在**：引用协议时基址完全跟随协议 |
| 转换配置 | 下沉到实例列 | **保持协议独占**（不新增列，运行时仍读协议种子） |
| 回填方式 | 迁移内单语句 | ≤5 万行：迁移内单语句；>5 万行：`scripts/backfill-0019`（分批 + 断点 + 幂等） |

四项裁定的落点：

| 裁定 | 落点 | 断言它的测试 |
|---|---|---|
| ① 引用协议不覆盖基址 | `channel/probe.go:effectiveProtocolBaseURL`、`gateway/protocol.go:effectiveBaseURL`（引用协议时只走协议链）；迁移 (b) 归档并清空 `channels.base_url`/`protocol` | `TestEffectiveProtocolBaseURL_ProtocolReferenceIgnoresInlineLenacy`、`TestEffectiveBaseURL_ProtocolReferenceIgnoresInlineBaseURL`、`TestMigrate_0019_FieldOwnership` |
| ② 转换按协议定义 | `gateway/conversion.go` 仍读 `channel_protocols.models[m]`；`EffectiveModelConvert` 保留；无 convert 列 | `TestResolveUpstreamProtocol`（`per-model disabled` / `override target order`）、`TestChannelModelRequest_DropsChannelLevelConvertFields` |
| ③ 非空 inner BaseURL → 400 | `channel/service.go:validateBase`（文案含协议名 + 「要改请新建协议」） | `TestValidateBase_RejectsInlineBaseURLWhenProtocolReferenced` |
| ④ `edited` 字段级置位 | `channel/model_service.go:commercialFieldsChanged` | `TestCommercialFieldsChanged_FieldLevel`、`TestUpdateChannelModel_EditedFlag` |

`edited` 的唯一出口：`POST /api/v1/admin/channels/{id}/models/{mid}/reset-from-protocol`
（恢复种子值 + 置回 `edited=false`；种子未定义的键保持现值）。

---

## 2. 两个必须知道的实现细节

### 2.1 `Protocol` 不做 400，忽略并落空

老前端恒定回传 `protocol: "openai-compat"`；若和 `BaseURL` 一样 400，会拦住一切正常保存。
该值在引用模式下从不参与解析，因此服务端**忽略并落空**（`inlineProtocol()`），迁移 (b) 也把存量值清空。
只有「手工填了 BaseURL」才会 400。

### 2.2 迁移 (c) 是契约 §6.1 之外补的一步

契约 §5.2 承诺「迁移会把 `models[m].protocols` 复制进 `channel_models.protocols`，因此存量行为不变」，
但 §6.1 的 SQL 里没有这一步。不补的话，凡是「实例 protocols 为空、靠种子回落」的模型行会突然丢掉
模型级协议子集（行为回退）。迁移 0019 用 (c) 段补上，只写「实例为空」的行。

### 2.3 归档范围：只处理「引用了仍在生效的协议」的渠道

迁移 (b) 与回填的归档/清空语句都 `JOIN channel_protocols cp ON cp.id = c.channel_protocol_id
AND cp.deleted_at IS NULL`：协议已被软删的渠道**保持原值不动**——那种状态下运行时解析不到协议基址，
清空只会丢掉唯一可恢复的地址，属于要人工修复的脏数据，不是迁移该悄悄处理的范围。
同理软删渠道不归档、`base_url` 与 `protocol` 都空的渠道不产生留档行。

### 2.4 没有「渠道侧旧转换配置归并到协议」这一步，因为不存在渠道侧存储

0019 之前，跨协议转换只存在于 `channel_protocols.models[m].convertEnabled/convertTargets`，
`channel_models` 从来没有这两列（契约 §1.3 的盘点即如此）。因此裁定②落地时不需要
「把渠道侧旧值归并回协议」——没有来源可归并，迁移也就不含该步骤；协议里的既有值原样保留、
继续作运行时真源。将来若有人误加渠道侧转换列/字段，`TestChannelModelRequest_DropsChannelLevelConvertFields`
会先失败。

---

## 3. 迁移 0019 与回滚

| 段 | 内容 | 幂等 |
|---|---|---|
| (a) | 加 `edited` 列；建 `channel_base_url_archive` | 是（`IF NOT EXISTS`） |
| (b) | 引用协议渠道：`base_url`/`protocol` 归档后清空 | 是（`ON CONFLICT DO NOTHING` + 仅命中非空） |
| (c) | 种子的模型级 `protocols` 下移到实例（仅实例为空时；下移必须在 (d) 之前） | 是 |
| (d) | 漂移标记 `edited=true`（种子定义了 + 双方都有值且**都非空** + 值不等；成本按 `Input/Output/CacheRead` → snake 逐键 numeric 比较） | 是 |

> 「非空」守卫对四个字段一视同仁：`cost_rates='{}'`、`protocols='{}'`、`context_tiers='[]'`、
> `time_config='{}'` 都视为**未设置**、不标 `edited`。漏掉 `time_config` 这一条会把该行永久冻结在
> 协议同步之外（Gate P1-① 的回归点）。

(b)(c)(d) 三段整体包在 `DO $$ … END $$` 阈值守卫里（方案 A，见 §4）：若 `channel_models` 未删除行数超过
`db.Backfill0019InlineThreshold`（默认 5 万），只执行 (a) 并 `RAISE WARNING`，数据段留给独立脚本。

回滚：`backend/scripts/rollback-0019.sql`（纯 SQL）。
从留档表还原 `base_url`/`protocol` → 删 `edited` 列 → 删留档表 → 清理 `applied_migrations` 记录 → 自检。
**有损点为零**：老代码用显式列清单查询，不读留档表。

已验证（PG17 真跑）：`TestMigrate_0019_FieldOwnership` 覆盖搬迁/漂移/幂等/回滚/B1；`TestMigrate_FullChain` 保证全新库全链无冲突。

---

## 4. 回填：两条路径 + 阈值守卫（方案 A，已落地）

| 生产 `channel_models` 行数 | 路径 | 特点 |
|---|---|---|
| ≤ 5 万 | 迁移 0019 内联（单事务） | 单语句幂等，JSONB 逐行比较通常 < 2s |
| > 5 万 | `go run ./scripts/backfill-0019 -dsn "$DSN"`（或 `db.Backfill0019`） | 主键游标、**单批 5000 行**、每批独立事务、进度写 `backfill_checkpoints(name,last_id)` |

阈值守卫（**方案 A**）的落点：

- **单一开关**：`db.Backfill0019InlineThreshold`（Go 常量）。迁移 SQL 里写的是占位符
  `{{backfill_threshold}}`，由 `db.renderMigration()` 在执行前替换——同一个数字不会同时硬编码在
  `.sql` 与 Go 两处。要退到方案 B（承认单事务全量回填），把这个常量调到极大（如 `math.MaxInt64`）
  或删掉迁移里的 `DO $$ … END $$` 守卫即可，脚本仍保留为补救/演练工具。
- **超阈值行为**：迁移只做 DDL（`edited` 列 + 留档表），数据段（b)(c)(d) 整段跳过并 `RAISE WARNING`；
  `channel_models` 的 `edited` 列此时已存在，所以独立脚本才有执行窗口（这是原先的死角）。
- **启动告警**：`main.go` 迁移后调 `db.PendingLargeBackfills`，未完成时打 **ERROR** 级日志，
  文案包含「还差哪个阶段 + 具体命令」；**不阻止启动**（回填前网关仍可服务，实例行缺协议子集会
  退化成协议级全集，属行为放大、不是崩溃）。
- **三个阶段**（顺序即契约 §6.1 顺序，脚本与迁移同一套 SQL 常量）：
  `0019_channels`（归档清空）→ `0019_protocols_downshift`（协议子集下移）→ `0019_channel_models`（漂移标记）。
  下移必须在漂移之前，否则「实例 protocols 为空」的行会先被标 `edited`、下移再跳过，子集永久丢失。

为什么分两条：`internal/db/migrate.go` 把**每个迁移文件包在一个事务里**，分批与断点在单事务内无法成立。

幂等与续跑的实现口径：
- 归档 `ON CONFLICT (channel_id) DO NOTHING`（同一渠道不会写第二份留档）；
- 清空语句带非空条件（已清空的行不再命中）；
- 下移只在「实例为空」时写（已下移的行不再命中）；
- 漂移标记只把 `edited` 写成 true；
- 游标只前进；崩在事务中则整批回滚，重启从 `last_id` 之后重做。
- 想要从头重跑（演练）：`-reset`。

已验证（PG17 真跑）：
`TestMigrate_0019_ArchiveScope`（软删渠道/已空渠道/引用软删协议的渠道都不归档、原值不被清）、
`TestBackfill0019_BeforeMigrationFailsReadably`（迁移前跑回填给出「请先执行迁移 0019」的可读错误）、
`TestMigrate_0019_AfterBackfillIsIdempotent`（>5 万行：迁移→回填→再跑迁移，结果逐行不变且告警消失）、
`TestBackfill0019_Idempotent`（跑两遍状态逐行一致、第二遍零写入）、
`TestBackfill0019_ResumeFromCheckpoint`（限批模拟中断 → 断点前进 → 续跑后与一次跑完逐行一致）、
`TestBackfill0019_AfterMigrationIsNoop`、`TestBackfill0019_MatchesMigrationResult`（脚本三段结果与迁移逐行一致）、
`TestMigrate_0019_ThresholdGuard`（超阈值只做 DDL + 告警 + 跑完脚本后告警消失）、
`TestMigrate_0019_DriftBoundaries`（契约 §6.1(c)(d) 边界逐条断言，含 `time_config='{}'` 不误标）。
计数口径：三段都取语句的 `RowsAffected`（本次真正写入数），`-reset` 重跑计数归零。

---

## 5. 存量计数（**测试库快照，非生产**）

| 表 | 总行数 | 未删除 | 来源 |
|---|---|---|---|
| `channels` | 0 | 0 | `lingshu_test` 快照（2026-11-10） |
| `channel_protocols` | 0 | 0 | 同上 |
| `channel_models` | 0 | 0 | 同上 |
| `channel_keys` | 0 | 0 | 同上 |

`lingshu_dev` 同为 0；`lingshu_staging` 无业务表。**生产数待 spencer 提供后修订本节**。
采集入口：`psql "$DSN" -f backend/scripts/0019-preflight-audit.sql`（只读，一次给出：三表计数 +
裁定①影响面清单 + 高危（协议无基址）+ 预估 `edited` 行数 + 回填路径建议）。

---

## 6. 门禁

- `go vet ./...`：通过。
- `go test -race`（除 `tests/` e2e）：全绿；`internal/db` 的迁移与回填集成测试在 PG17 真跑通过。
- `gofmt`：本次改动文件干净（仓库既有 15 个历史不合规文件未动）。
- `tests/` e2e 的 `TestE2E_GatewayBillingLoop` 失败**属既有问题**（CI 中该套件本就是
  `continue-on-error` 的信息性步骤：脚手架未装配渠道密钥服务），与本次改动无交集。

---

## 7. 给下游

- **View**：`ChannelView` **与 `GET /api/v1/admin/channels` 的 `Channel`** 都带只读派生 `BaseURLEffective`
  （引用协议→协议基址；内联→自身 `base_url`；渠道列表页不再空白）；引用协议时 `BaseURL`/`Protocol` 恒为 `''`，
  前端应隐藏这两个输入并展示「基址跟随协议：<协议名>（<BaseURLEffective>）」，提交时传 `''`。
  模型行新增只读 `Edited`（徽标 + 「重置为协议值」调 `reset-from-protocol`）。转换配置仍在协议编辑页（标注协议级共享）。
- **Gate**：契约 §7.3 的用例表可直接抄；注意「PUT `Protocol="openai-chat"`」的期望是**不报错且落库为 `''`**，
  只有 `BaseURL` 非空才 400。回填相关另加两条：幂等（跑两遍一致）与断点续跑（限批中断后续做）。
