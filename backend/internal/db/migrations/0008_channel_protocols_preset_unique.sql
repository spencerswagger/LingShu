-- 预设来源唯一约束：Sync 以 preset_source_id 为幂等键，需保证「一来源一条」。
-- 先兜底去重（历史 bug 可能产生同 preset_source_id 的重复物化行：仅保留最小 id，其余软删除），
-- 再建部分唯一索引；避免存量重复数据导致建索引失败。内联协议 preset_source_id='' 不受约束。
UPDATE channel_protocols cp SET deleted_at = now(), updated_at = now()
WHERE cp.deleted_at IS NULL
  AND cp.preset_source_id <> ''
  AND cp.id > (
      SELECT MIN(dup.id) FROM channel_protocols dup
      WHERE dup.preset_source_id = cp.preset_source_id AND dup.deleted_at IS NULL
  );

DROP INDEX IF EXISTS idx_channel_protocols_preset;
CREATE UNIQUE INDEX IF NOT EXISTS uq_channel_protocols_preset
    ON channel_protocols(preset_source_id)
    WHERE deleted_at IS NULL AND preset_source_id <> '';
