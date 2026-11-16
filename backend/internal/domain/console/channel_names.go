package console

import (
	"context"
	"database/sql"
)

// channelNamesByKeyIDs 批量把 channel_key_id 映射为「渠道显示名」。
//
// ⚠️ **渠道体系已下线**（迁移 0024_v2_drop_legacy 删掉了 channels / channel_keys /
// billing_records.channel_key_id），已经没有任何地方能提供「渠道显示名」：
// 这个函数现在恒返回 nil，调用方（管理端账单列表 / 开发者端列表 / 会话列表）拿到的是空串，
// 前端按空串渲染即可（响应结构不变，不用特判字段是否存在）。
//
// 历史账单的渠道名**永久取不回**：当时没有给账单留渠道名快照，表一删就没有第二份来源。
// 这是下线渠道体系的既定代价，不是本函数的选择。
//
// 保留函数签名只为不打断调用点；后续若确认没人再传 keyIDs，可连同调用点一起删除。
func channelNamesByKeyIDs(ctx context.Context, db *sql.DB, keyIDs []int64) map[int64]string {
	_ = ctx
	_ = db
	_ = keyIDs
	return nil
}
