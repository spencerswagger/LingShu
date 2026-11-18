# 渠道域读取代码摘除（与迁移 0024 同批）

日期：2026-11-16
分支：已并入 `feat/lingshu-v2-converge`（原 `feat/lingshu-v2-drop-channel-reads`），随 PR #84 与 0024 同批交付
状态：实现完成，**必须与 `0024_v2_drop_legacy.sql` 同一发布批次上线**

## 1. 为什么必须同批

`0024_v2_drop_legacy.sql` 删掉了以下对象：

- 列：`billing_records.channel_key_id`、`sessions.channel_key_id`、`tokens.tag_id`
- 表：`channel_tags`、`channel_model_events`、`channel_key_events`、`probe_logs`、`channel_models`、
  `channel_keys`、`channel_events`、`channels`、`auto_model_members`、`auto_models`、`semantic_tags`

迁移先上、代码后上，中间任何一次请求都会撞 42P01 / 42703；反过来代码先上、迁移后上，
代码就查不到它以为还在的列。因此**只有同批**这一种上线方式：

- 同一发布批次（同一次部署窗口），或
- 同一个 PR 集合按「迁移 + 代码」一次性合入。

## 2. 摘除点清单

| # | 位置 | 摘了什么 | 下线后的对外行为 |
|---|------|---------|-----------------|
| 1 | `internal/domain/billing/service.go` | 账单 INSERT 去掉 `channel_key_id`（32 → 31 参数）；`fillChannelKeys` 只读 `call_logs.channel_key_id`（不再 COALESCE 回落本表） | 账单元数据不再写渠道密钥 |
| 2 | `internal/domain/billing/query.go` | 渠道过滤改为对 `call_logs` 快照做 `EXISTS` 子查询 | 过滤参数仍接受，命中的是历史快照 |
| 3 | `internal/domain/billing/stats.go` | ① `dashboardTop` 的「按密钥 TOP」恒 nil（不再 JOIN `channel_keys`/`channels`）② `modelEntityResolveBody` 改为按 `external_models.external_name` 匹配 | TOP 榜不含渠道维度；模型广场归属只看模型名 |
| 4 | `internal/domain/console/cost.go` | 删实体解析函数；实体键 = `name:<对外模型>#<内部模型>`，`channel_id/channel_model_id` 恒 0、`channel_name` 恒空 | 成本统计仍按模型出数，金额不变 |
| 5 | `internal/domain/console/cost_trace.go` | 删打平点预测、跨渠道模型加载；轨迹点的渠道字段字面量化 | 轨迹点无渠道维度；预测打平点恒「不可用 + 原因」 |
| 6 | `internal/domain/console/cost_pricing.go` | 成本线无配置源 → `Available=false` + 原因；多线（`ChannelModelIDs`）忽略；售价线只看显式模型 | 定价曲线只有售价线 |
| 7 | `internal/domain/console/admin.go` | 密钥名/渠道名反查恒空（`keyInfos`/`channelNames` 返回 nil）；会话列表去掉 `channel_key_id` 列；会话渠道过滤参数禁用 | 名称字段空串，不再用 `#<id>` 兜底 |
| 8 | `internal/domain/console/channel_names.go`、`cmd/server/main.go` | `channelNamesByKeyIDs` / `keyNameResolver` 恒 nil | 同上 |
| 9 | `internal/domain/identity/token.go` | `tokenCols` / `tokenInfoCols` / `scanToken` / `scanTokenInfo` / `CreateToken` 去 `tag_id`（结构体字段保留恒 nil） | 令牌不再带标签 |
| 10 | `internal/domain/router/session_store.go` | `sessionCols` / `sessionUpsertSQL` / `Upsert` / `LoadActive` 去 `channel_key_id`（占位符重编号 $1..$10） | 会话不再持久化渠道粘性维度 |
| 11 | `internal/domain/tag/`（整目录）+ `internal/server/server.go` + `cmd/server/main.go` | 标签域处理器与装配整体删除，路由改 410 | `/tags` 返回 410 停用 |

## 3. 口径：API 结构保留、渠道维度恒空

所有响应**字段结构一个都没删**（前端不需要特判），只是值恒为 `0` / `""` / `nil`：

- 账单/会话列表的 `ChannelKeyID`、`KeyName`、`ChannelName`、`ChannelName`（dev 视图）
- 成本统计行的 `ChannelID` / `ChannelModelID` / `ChannelName`
- 定价曲线的 `Cost.ChannelModelID` 等

**历史渠道名永久取不回**：改名/改名前的历史归属没有快照，`0024` 之后无从反查。
这是已知且不可逆的信息损失，登记在案，不再尝试补偿。

## 4. 回滚方式

**结论先说：没有「先回代码」这条路。** 代码与迁移必须同批前进；要退只有整库还原。

只有两种可执行操作：

1. **同批前进**（唯一的正常路径）：代码 + 0024 一起上线，一起回退到同一个提交/镜像。
   原因：0024 删掉的 `channels` / `channel_keys` / `channel_key_id` 等对象是**旧代码的运行时依赖**，
   代码一旦回退、迁移没退，进程启动阶段就会崩（`console` 装配查 `channels` → 42P01，
   `sessions` / `billing_records` 写 `channel_key_id` → 42703），不是「部分接口报错」而是起不来。
2. **整库还原到 0024 之前的快照**：停服 → `pg_restore` 迁移前备份（含被删的 10 张表与 3 个列）
   → 同时**回退代码到同一时间点**（两边必须是同一个版本，否则同样 42P01/42703）。
   数据恢复的完整度取决于备份时点，0024 之后写入的账单/会话不在其中。

明确**不可执行**的做法（原稿写错过，这里留痕避免再犯）：

- ❌「先回代码、再回迁移」：回代码那一步就会 42P01/42703，服务起不来，中间态没有可用版本。
- ❌ 只跑 `0024_v2_drop_legacy.sql` 的 DOWN 段：它只能重建**空表**（`CREATE TABLE` + 空列），
  被删的渠道配置/渠道密钥/历史渠道名不会回来；重建后还要手工录入配置，
  而历史账单上的渠道归属（无快照）永远取不回。

因此**生产上不假设可回滚**：上线前必须做 0024 的三步证据（`pg_dump → 临时库还原 → 行数比对`），
把「不可回滚」当成既定事实来评估。

## 5. 本切片带来的功能缺口（已登记，不在本切片修）

- 模型广场不再能把「改名前的历史账单」归到新名下 → 落到「未匹配」行（守恒不变）。
- 成本页 / 定价曲线的成本线不可用，成本以账本（`ledger_entries` + `cost_credits`）为准。
- 会话粘性不再持久化渠道维度；跨重启的会话恢复不含渠道信息。
- 令牌默认路由的标签维度消失（`tokens.tag_id` 不再落库）。
