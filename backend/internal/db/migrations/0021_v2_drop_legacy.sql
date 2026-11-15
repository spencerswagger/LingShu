-- 0021_v2_drop_legacy：删退役的渠道/标签/Auto 体系。
--
-- ⚠️ 本文件是 0019–0023 里**唯一含 DROP、唯一不可回滚**的迁移。
--    执行前必须完成「执行前核对 + 三步证据」（下附），否则不许执行。
--
-- ── 数表：实际删 10 张，不是 11 张 ──────────────────────────────────────
--    本文件有 11 条 DROP TABLE，其中 channel_events 在 0009_drop_channel_state.sql
--    （该文件末句：DROP TABLE IF EXISTS channel_events;）就已经删掉了，
--    这里那条**永远是空操作**。保留它只为清单完整 + 收拾「手工恢复过该表」的环境。
--    按 11 张去数会以为漏了一张而去找不存在的表 —— 按 10 张数。
--
-- ── 顺序不可调（两个层次都不可调）────────────────────────────────────────
-- ① 先删列，再删表：billing_records / sessions / tokens 上的外键列必须先摘掉，
--    否则 DROP TABLE 会被外键挡住（sessions.channel_key_id 还是 NOT NULL）。
-- ② 表按「子表 → 父表」删：channel_tags、channel_model_events、channel_key_events
--    引用 channels/channel_keys/semantic_tags；auto_model_members 引用 auto_models；
--    channel_tags 同时引用 channels 与 semantic_tags。顺序颠倒会报
--    "cannot drop table X because other objects depend on it"。
--    改这条顺序的人要先看 pg_depend，不要按字母序重排。
--
-- ── 执行前核对：幂等键是「文件名」，不是编号（必须先做）──────────────────
--    migrate.go 以**文件名**为幂等键：同名不同内容 → 静默跳过（schema 缺列且不报错）；
--    同编号不同名（旧分支的 0019_v2_facade.sql）→ 两条都执行 → DDL 直接撞车。
--    远端仍有 3 个旧 v2 分支带 0019_v2_facade.sql，其中一个还带 0020_reconcile_blocked_status.sql。
--      psql -tAc "SELECT name FROM applied_migrations ORDER BY name DESC LIMIT 1"
--        → 必须 0018_call_logs_req_params.sql
--      psql -tAc "SELECT name FROM applied_migrations WHERE name >= '0019' ORDER BY name"
--        → 必须 0 行；有行 = 该库被旧分支迁移过。库里不存校验和，证明不了「同名即同内容」
--          → 停手逐个人工比对，比对不上不许执行。
--
-- ── 执行前的三步证据（产物贴进 PR 描述）─────────────────────────────────
--   1) 备份：pg_dump -Fc -Z6 -f backup/<db>-<stamp>.dump <db>；退出码必须 0，文件必须 >1KiB。
--   2) 还原：createdb -T template0 <tmp_db> && pg_restore -d <tmp_db> --no-owner --no-privileges -j4 <dump>；
--      再用同一份「全表精确计数」SQL 在源库与临时库各跑一次，diff 必须为空。
--   3) 比对：迁移前存一份「保留表计数」基线，迁移后再跑一次，diff 必须为空；
--      并断言 10 张实际退役表（+ 空操作的 channel_events）to_regclass 全为 NULL。
--    判定点：备份失败/还原失败/计数不一致/退役表仍存在 —— 任一命中即中止（详见定稿文档 §5）。
--    负向对照（已实测）：在本文件末尾注入 SELECT 1/0 → 整份事务回滚、表与列原样。
--    这条是「跑一半也能回滚」的证据，不是推断；正式执行前建议再跑一次这个对照。
--
-- ── 中途失败能不能自动回滚？结论：能，但只在「本文件的事务内」────────────
--    migrate.go（internal/db/migrate.go:16-70）把整个文件放在一个 tx 里执行，PG 的 DDL 是
--    事务性的（DROP TABLE / DROP COLUMN 均可回滚）。任一句报错 → tx.Rollback() → 库回到
--    0018 的状态，且 applied_migrations 不会记 0021，重试是安全的。
--    所以 dump 保护的不是「迁移跑到一半」——那由事务兜住；dump 保护的是「文件已 commit、
--    但事后发现删错了/需要取回历史渠道密钥名」这类**事务之外**的误判。
--    推论：这五个迁移文件里都不得出现 CREATE INDEX CONCURRENTLY / VACUUM / CREATE DATABASE
--    这类不能在事务内执行的语句，否则迁移会直接失败（那才是真回滚不了）。
--
-- ── 已 commit 之后发现删错了：整库还原（已实测）──────────────────────────
--    用步骤 1 的同一份 dump 还原：dropdb --if-exists <db> → createdb -T template0 <db>
--    → pg_restore -d <db> --no-owner --no-privileges -j4 <dump>，行数与 before-all.txt 应 0 差异。
--    窗口期限制：dump 是执行**之前**的快照，还原会丢掉「迁移后 → 回滚前」的增量
--    （新账单/新账本条目/新用户）。超过一个业务周期才回滚，正确做法是从 dump 单独抽被删表的历史
--    数据补回，而不是整库覆盖。
--
-- ── 保留不删（别顺手清）───────────────────────────────────────────────
--    channel_protocols / protocol_error_stats：不在批准清单内，等阶段二后单独拍板。
--    watchlist_items / price_change_alerts：价格监控，未批准下架。
--    call_logs.channel_id / channel_key_id / auto_model_name：无外键、不阻塞，
--    且是历史日志快照——删了就没有第二份证据可查。
--
-- 幂等：全部 DROP ... IF EXISTS，重复执行不报错（但已删的数据取不回）。

-- ① 先删阻塞列
ALTER TABLE billing_records DROP COLUMN IF EXISTS channel_key_id;   -- 连带 idx_billing_key_time
ALTER TABLE sessions        DROP COLUMN IF EXISTS channel_key_id;   -- NOT NULL，必须删；连带 idx_sessions_key
ALTER TABLE tokens          DROP COLUMN IF EXISTS tag_id;           -- 引用 semantic_tags

-- ② 再删表（子表在前，顺序不可调）
DROP TABLE IF EXISTS channel_tags;            -- 引用 channels + semantic_tags
DROP TABLE IF EXISTS channel_model_events;    -- 引用 channels
DROP TABLE IF EXISTS channel_key_events;      -- 引用 channel_keys
DROP TABLE IF EXISTS probe_logs;              -- 引用 channel_keys
DROP TABLE IF EXISTS channel_models;          -- 引用 channels + external_models
DROP TABLE IF EXISTS channel_keys;            -- 引用 channels
DROP TABLE IF EXISTS channel_events;          -- 0009 已删 → 空操作（保留只为清单完整；不做实际删除）
DROP TABLE IF EXISTS channels;                -- 引用 channel_protocols（出向，不挡）
DROP TABLE IF EXISTS auto_model_members;      -- 引用 auto_models + external_models
DROP TABLE IF EXISTS auto_models;             -- 引用 users
DROP TABLE IF EXISTS semantic_tags;           -- 引用 users