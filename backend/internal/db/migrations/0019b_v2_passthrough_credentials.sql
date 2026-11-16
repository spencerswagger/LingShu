-- 0019b_v2_passthrough_credentials：转发链仍在使用的最小对象集（纯 ADD、幂等）。
--
-- ── 为什么有这个文件（#81 拆包，走方案 A）────────────────────────────────
--   第一阶段（v2，PR #81）的 `0019_v2_facade.sql` 是一份「加壳」迁移，它把四类对象打在一个文件里：
--     ① magpie_keys                       —— 灵枢令牌 ↔ 代理侧 callerKey 映射（**仍在使用**）
--     ② external_models.magpie_model      —— 对外名 → 代理模型名（**仍在使用**）
--     ③ reconcile_windows                 —— 60 秒对账窗口（**已由 0023 接管，形状不同，不能留两份**）
--     ④ billing_records 三列              —— magpie_key_name / estimated / upstream_usage（**本期不用**）
--
--   收敛目标只有一条链、一个形状，所以 0019_v2_facade.sql 被删除，按对象归属拆开：
--     · 仍在使用的 ①② → 本文件（编号 0019b，字典序在 0019_topology 之后、0020 之前）；
--     · ③ 归 0023_v2_reconcile.sql（PK = window_start、四计数列可空、scroll_offset 持久化）；
--     · ④ 本期**不处理**：无 usage 一律不计费（不做估算），账单侧那三列没有写入方，
--          留待阶段二新分支按新形一并处理；本文件不许偷偷把它们加回来。
--
-- ── 剔除依据（逐对象）─────────────────────────────────────────────────────
--   reconcile_windows：与 0023 同名不同形（旧形 12 列、PK=id、计数列叫 req_count_ls/…、无 scroll_offset）。
--     同一库上两份 CREATE TABLE IF NOT EXISTS 只有第一份生效，第二份直接跳过且不报错 —— 这正是
--     「记录说跑过、对象其实建错」的那类事故，所以同名对象只允许有一个文件持有。持有者是 0023。
--   billing_records.magpie_key_name / estimated / upstream_usage：本期转发链不写账单
--     （第③件计费的口径已改：无 usage 不计费、不做估算），加列只会让「没人写的列」看起来像
--     「功能已在」，对账按 magpie_key_name 分组的查询也会落空。故本文件不建，也不建它的部分索引。
--
-- ── 保留对象与依赖方（删任何一个都会 42703 / 42P01）──────────────────────
--   magpie_keys：internal/domain/magpie/key_store.go（读写）、keyadmin.go（管理端绑定端点）。
--   external_models.magpie_model：internal/domain/model/store.go（cols / INSERT / UPDATE），
--     magpie 准入层用它判定「这个对外名能不能转发」。
--   uq_external_models_magpie_model：一个代理模型名只能被一个对外名占用；冲突映射成 409
--     （model.ErrMagpieModelExists），不靠应用层约定。
--
-- ── 幂等 / 重复执行 ───────────────────────────────────────────────────────
--   CREATE TABLE IF NOT EXISTS + ADD COLUMN IF NOT EXISTS + CREATE INDEX IF NOT EXISTS。
--   已经跑过 `0019_v2_facade.sql` 的环境：本文件全部命中「已存在」→ 零改动（记录表里那条
--   旧文件名留着即可，迁移器只按目录里的文件名比对，历史记录行不影响后续文件执行）。
--   ⚠️ 这类环境的 reconcile_windows 是**旧形状**，由 0023 顶部的自愈 DO 块处理（DROP 重建，
--      纯派生数据无损失），不靠本文件。
--
-- 可回滚：DROP TABLE magpie_keys; ALTER TABLE external_models DROP COLUMN magpie_model;
--   （回滚会同时丢掉令牌↔callerKey 映射，重新录入即可；不影响任何历史账单。）

-- ① 灵枢令牌 ↔ 代理侧 gateway key：一令牌一把。
--    token_id 不加 ON DELETE CASCADE：令牌走 deleted_at 软删，物理删除不在本期口径内。
--    key_name 唯一 = 代理侧 callerKey 全局唯一；UNIQUE (token_id) 把「一令牌一把」落成约束。
CREATE TABLE IF NOT EXISTS magpie_keys (
    id             BIGINT PRIMARY KEY,                     -- 雪花 id，应用侧 idgen 生成，无 DEFAULT
    token_id       BIGINT NOT NULL REFERENCES tokens(id),
    key_name       TEXT   NOT NULL UNIQUE,                 -- 代理侧 callerKey（绝不落明文以外的派生值）
    key_id         TEXT,                                   -- 代理侧内部 key id，可空
    key_cipher     TEXT   NOT NULL,                        -- 密文，口径同 tokens.secret_cipher
    state          TEXT   NOT NULL DEFAULT 'PENDING'
                   CHECK (state IN ('ACTIVE','PENDING','DISABLED')),
    last_synced_at TIMESTAMPTZ,
    last_error     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (token_id)                                      -- 「一令牌一把」落约束，不靠约定
);

-- ② 对外模型 → 代理模型名。NULL = 未映射 = 不转发（准入层直接拒绝，不是「走默认」）。
ALTER TABLE external_models ADD COLUMN IF NOT EXISTS magpie_model TEXT;

-- ③ 唯一约束：两个对外名不得指向同一代理模型（否则对账按模型聚合会合并 → 误报）。
--    部分索引（只覆盖非 NULL 行）：历史上未映射的行不进索引。
CREATE UNIQUE INDEX IF NOT EXISTS uq_external_models_magpie_model
    ON external_models (magpie_model) WHERE magpie_model IS NOT NULL;
