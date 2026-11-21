-- 0030_magpie_key_rename.sql：callerKey 改名支持（legacy_key_name 兜底列）。
--
-- 背景（V3 旁路计费）：账本行按 caller_key_name 反查令牌结算。callerKey 名字从
-- ls-token-<id> 改成 ls-u<uid>-t<tid>-<slug> 后，改名窗口之前落库的账本行仍是旧名快照；
-- 没有旧名兜底，那些行按名反查失败、永远不会计费。
--
-- 本迁移只加一列：legacy_key_name 保存「改名前的旧名」。
--  1. 新令牌首建：legacy_key_name = ''（无旧名）；
--  2. 存量改名回填：改名成功时 legacy_key_name = 旧 key_name（供结算反查兜底）；
--  3. 管理端手工绑定/再次改名：每次改名把当前 key_name 移入 legacy_key_name。
-- 语义：key_name = 代理侧当前生效名；legacy_key_name = 该令牌曾在代理侧用过的名字。
--
-- 幂等：ADD COLUMN IF NOT EXISTS + 部分索引 IF NOT EXISTS；不改任何既有数据。
-- 可回滚：DROP INDEX + DROP COLUMN（历史账单行不受影响，只是旧名兜底查询消失）。

ALTER TABLE magpie_keys ADD COLUMN IF NOT EXISTS legacy_key_name TEXT NOT NULL DEFAULT '';

-- 结算反查兜底走这一列：窗口内旧名命中。
CREATE INDEX IF NOT EXISTS idx_magpie_keys_legacy_name
    ON magpie_keys (legacy_key_name) WHERE legacy_key_name <> '';

COMMENT ON COLUMN magpie_keys.legacy_key_name IS
  '改名前的旧 callerKey 名（结算按名反查的兜底键；未改过名为空串）';
