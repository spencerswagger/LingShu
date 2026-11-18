# V2 迁移链收敛 · 对象级零丢失核对（实测证据）

日期：2026-11-16　执行：Core　依据：`docs/designs/2026-11-16-v2-migration-converge-mapping.md`（架构 Ark，`origin/lingshu/arch`）

## 0 结论

**零丢失成立。** 旧线（`0019_v2_facade` + `0020_reconcile_blocked_status`）相对唯一链（`0019/0020/0022/0023/0023b/0023c/0024`）多出的 **193 个对象**，逐个落到映射表已裁定的四类里，**没有一个是「该保留却丢了」的**；
唯一链多出的 **109 个对象**全部是本次新增能力（5 张新表 + 归因列 + 十列窗口形状），不含对旧对象的意外改动。

因此：**没有 schema 需要「并入」唯一链**（与映射表 §0 一致），收敛的施工量在「删文件 + 改断言 + 改代码」。

## 1 方法（可复现）

1. 在真实 PG 17 上建两个空库：`cvg_new`（唯一链）、`cvg_old`（旧线）。
2. `cvg_new`：按字典序把 `backend/internal/db/migrations/*.sql` **全部**逐个 `psql -v ON_ERROR_STOP=1 -f`（0001…0024）。
3. `cvg_old`：0001…0018，再叠加 `0019_v2_facade.sql`（取 `origin/feat/lingshu-v2-p1-usage-billing`，sha256 `23ddc9d2…`、4930 B —— 与 PR #81 分支同一 blob）与
   `0020_reconcile_blocked_status.sql`（同一分支）。
4. 两侧各导出五类对象为「种类 + 名字」的行文本：`information_schema.tables` / `information_schema.columns`（含类型与可空性）/ `pg_indexes` / `pg_constraint`（`表名:约束名`）/ `information_schema.sequences`，各自 `sort` 后 `comm` 双向比对。

实测规模：唯一链 **382** 行对象指纹；旧线 **466** 行。

## 2 旧线独有 193 项 —— 逐类归档

| 类别 | 项数 | 明细 | 裁定 | 判定 |
| --- | --- | --- | --- | --- |
| 10 张退役表的全部对象 | **149** | `channels` 19、`channel_models` 23、`channel_keys` 16、`probe_logs` 19、`auto_models` 16、`auto_model_members` 13、`semantic_tags` 13、`channel_model_events` 12、`channel_key_events` 11、`channel_tags` 7（各含表本身、列、索引、约束） | 0024 删除（`db.RetiredTables()` 十张） | ✅ 预期消失 |
| `magpie_keys`（表 + 10 列 + 3 索引 + 5 约束） | **19** | `id/token_id/key_name/key_id/key_cipher/state/last_synced_at/last_error/created_at/updated_at` | 映射表 §2.1：**决定不建**（服务端单把 key 走环境变量） | ✅ 预期不存在 |
| 被裁掉的列 | **7** | `billing_records.magpie_key_name`、`billing_records.estimated`、`billing_records.upstream_usage`、`billing_records.channel_key_id`、`external_models.magpie_model`、`sessions.channel_key_id`、`tokens.tag_id` | 映射表 §2.3–2.6 + 0024 | ✅ 预期消失 |
| 随上述列一起消失的索引/外键 | **7** | `billing_records`：`billing_records_channel_key_id_fkey`、`idx_billing_key_time`、`idx_billing_magpie_key_time`；`external_models`：`uq_external_models_magpie_model`；`sessions`：`sessions_channel_key_id_fkey`、`idx_sessions_key`；`tokens`：`tokens_tag_id_fkey` | 随列删除（部分唯一索引在新口径下本身是错的） | ✅ 预期消失 |
| `reconcile_windows` 旧形状 | **11** | 8 列（`id`、`pulled_at`、`req_count_ls`、`req_count_magpie`、`tokens_ls`、`tokens_magpie`、`dev_pct`、`detail`）+ 列内联 `reconcile_windows_status_check` + 复合唯一 `reconcile_windows_window_start_window_end_key`（约束与其索引各 1 行） | 映射表 §2.2：以 0023 十列定稿形状为准 | ✅ 预期消失 |
| **合计** | **193** | 149 + 19 + 7 + 7 + 11 | — | — |

**归属核对是怎么做的**：索引/约束名不带表名前缀（`channels_pkey`、`uq_channels_active`），所以逐条反查 `pg_indexes.tablename` 与 `pg_constraint.conrelid`，
再按所属表归档 —— 除上表四类（退役表 / magpie_keys / 被裁的列 / 旧窗口形状）外**剩余 0 行**，即没有任何一项既不属于「已裁定丢弃」又不在唯一链里。

## 3 唯一链独有 109 项 —— 逐类归档

| 类别 | 项数 | 内容 | 判定 |
| --- | --- | --- | --- |
| 新表 | 5 | `internal_models`、`model_links`、`ledger_entries`、`credit_holds`、`subscriptions` | ✅ 阶段一/二新增 |
| 新列（8 张表） | 64 | `billing_records` 七列归因（`request_id/upstream_provider/upstream_account/upstream_model/upstream_protocol/ledger_entry_id/cost_source`）、`external_models.bound_internal_ref`、`reconcile_windows` 十列形状、以及 5 张新表的全部列 | ✅ 预期 |
| 新索引/约束 | 40 | `idx_billing_upstream_time`、`idx_billing_request`、`idx_reconcile_blocked`、`ck_reconcile_status`（五值）、`ck_reconcile_window_len`（60s）等 | ✅ 预期 |

注：`billing_records.channel_key_id` 在旧线是「列 + 外键」，唯一链里被 0024 删除；但 **`call_logs.channel_key_id` 两侧都在**（保留列，历史快照），收敛时读路径要切到它，不是全删（映射表 §7 的陷阱条目）。

## 4 不在本次核对范围（已在别处登记）

1. **行数**：本核对只比对象。保留 15 表的行数前后一致由 `TestGate_F7_RetainedRowCounts` 与 0024 三步证据（`docs/plans/2026-11-15-v2-0024-preflight-evidence.md`，1,625,951 行级）覆盖。
2. **旧文件删除 —— 阻塞：等 #81 处置拍板。** 按映射表 §6/§8-3，`git rm 0019_v2_facade.sql 0020_reconcile_blocked_status.sql` 必须在**各自分支内部**完成；
   `#81` 的处置未拍板前，**不删任何迁移文件、不动 #81 相关分支及其读写代码**（`magpie/credentials.go`、`keyadmin.go`、`handler.go`）。
   待删清单（拍板后执行，不在本分支）：slim-migrate 与 usage-billing 的 `0019_v2_facade` + `0020_reconcile_blocked_status`，
   以及 `docs/lingshu-v2-p1-data-contract` 那份**第四副本**（含与 `0024` 逐字重复的 `0021_v2_drop_legacy`，编号 0021 作废不复用）。
   **`0023b` / `0023c` 一律不许删** —— 它们是「同名文件已按旧内容记账」的老库唯一的修形路径。
3. **`0024` 文件头探测①排除清单漏 `0023b`/`0023c`**：仍待 spencer 点头后再改（改注释会变文件哈希，需重跑三步证据）。
4. **幂等键机制（口径更正）**：迁移器的幂等键是 `applied_migrations.name` 即**完整文件名**，不是版本号。因此
   ① 两个**不同文件名**的 `0019`（`0019_v2_facade` 与 `0019_v2_topology`）**都会执行**，`facade` 字典序在前，先建出错形的窗口表；
   ② 真正被静默跳过的是**同名文件不同内容**（后合的那份零报错跳过）。判断老库要不要重建，按这两条分别看，别按「同号只会跑一次」处理。
5. **契约文档旧编号**：`docs/designs/2026-11-15-v2-p1-data-contract.md` §5.1 里仍写 `0021_v2_drop_legacy.sql`，与目录（`0024_*`）漂移；文档属架构侧，未擅自改。
6. **代码摘除（0024 的 P0 前置闸门）**：未做，见升级评估 §4 与映射表 §7 的逐项落点。

## 5 复跑命令（下次直接抄）

```sh
# 1) 建两个空库（需要 CREATEDB 权限）
psql "$TEST_DATABASE_URL" -c 'CREATE DATABASE cvg_new' -c 'CREATE DATABASE cvg_old'
# 2) 唯一链
for f in $(ls backend/internal/db/migrations/*.sql | sort); do psql "${DSN/cvg_new}" -q -v ON_ERROR_STOP=1 -f "$f"; done
# 3) 旧线：0001–0018 + facade 0019 + 0020_reconcile_blocked_status（从 usage-billing 分支取）
# 4) 五类对象各导一份、sort + comm 双向比对
```
