package console

import (
	"context"
	"database/sql"
)

// channelNamesByKeyIDs 批量把 channel_key_id 映射为「渠道显示名」（channel_keys JOIN channels）。
//
// 单独抽出来的原因：管理端列表（decorateList）与开发者端列表都要展示渠道显示名，
// 两处若各写一份 SQL，口径与脱敏边界会漂移（Owl PR #63 建议 3：dev 列表缺 ChannelName 列）。
// 注意：这里只取**渠道显示名**——渠道密钥名（key name）仍属开发者端不可见字段。
func channelNamesByKeyIDs(ctx context.Context, db *sql.DB, keyIDs []int64) map[int64]string {
	if db == nil || len(keyIDs) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT k.id, c.name FROM channel_keys k JOIN channels c ON c.id = k.channel_id
		 WHERE k.id = ANY('{`+int64List(keyIDs)+`}'::bigint[])`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[int64]string, len(keyIDs))
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			continue
		}
		out[id] = name
	}
	return out
}
