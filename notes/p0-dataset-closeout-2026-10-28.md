# 渠道预设包数据集 · P0 收口（2026-10-28）

**结论**：步骤 3 拉取验收通过（含 `latest.json` 走 302 的方案 A）——数据集 → `preset.pack_url` → sync 链路可用。

- 同步：`POST /api/v1/admin/channel-protocols/sync {"confirm":true}` → `HTTP 200 / Code=0 / Applied=40 / Pending=[] / Stale=0`。
- 四个数：`channel_protocols` **40 行**、`STALE=0`（ACTIVE=40）；`preset_source_id` 与 `name` 重复**各 0**、40 个协议 ID 与上一轮逐一相同；`claude-sonnet-5.5` = **13.4 / 67 / 0.67**、32 条 `costRates` 与包内差值 **0**；包哈希 `c8a31383…` 未变。
- 302 链路：`latest.json` → 302（相对 Location `/channel-preset-pack/versions/1.json`，首跳 `cache-control: max-age=60, must-revalidate`）→ **只跳 1 跳** → 200 / `application/json` / **52,163 B** / 末跳 `immutable`；body 以 `{"tencent-coding-plan":…` 开头，**sha256 与包本体逐字节一致**。
- 边界①：不跟随 302 会报「预设包返回非 200 状态码: 302」（本次未出现）。
- 边界②：`http.DefaultClient` 不做响应缓存，`max-age=60` 无影响。
- 边界③：`manifest.json` 这类返回 200 的清单 JSON **不可填进 `pack_url`**（走 F3 第 ④ 形态，裸 500）。
- 复跑方式：`POST /api/v1/admin/channel-protocols/sync {"confirm":true}`；首启同步有 AC-12 已同步标记、会被跳过，**必须手工 sync**。

口径正本：`docs/preset-pack-format.md` §0.1 F3 / §11.1 V9 —— PR #64 @ `0b2e1f4`（OPEN，待 Owl 评审与合入）。
