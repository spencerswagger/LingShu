package db

// V2 迁移链的「保留表 / 退役表」清单（唯一来源）。
//
// 为什么放在代码里而不是只写进迁移文件注释：运维脚本、迁移用例、PR 证据三条都要用同一份清单，
// 三处各抄一遍必然漂移（抄一次少一张表，谁也不会发现）。这里导出常量，用例断言数量与成员。
//
// 口径（F2 裁定）：保留表 **15 张**。前 13 张是业务在用的表（账单/钱包/流水/会话/日志/配置/
// 公告/审计），后 2 张 watchlist_items / price_change_alerts 是价格监控表——未批准下架，
// 一刀切「渠道域全删」会把它们顺手带走，所以显式列进来。
//
// 判据（F7 裁定）：迁移前后**行数不得变少**（before <= after 逐表成立）。不要求逐字相同：
// 迁移期间有正常新写入（新账单/新流水/新会话），为「逐字相同」去停机不划算。

// RetainedTables 返回 V2 迁移链执行前后都必须存在的表（15 张）。
func RetainedTables() []string {
	return []string{
		"users",
		"tokens",
		"billing_records",
		"credit_wallets",
		"credit_flows",
		"sessions",
		"call_logs",
		"external_models",
		"sys_configs",
		"announcements",
		"audit_logs",
		"channel_protocols",
		"protocol_error_stats",
		"watchlist_items",
		"price_change_alerts",
	}
}

// RetiredTables 返回 0024 实际删除的表（10 张；channel_events 在 0009 已删，不计入）。
func RetiredTables() []string {
	return []string{
		"channels",
		"channel_keys",
		"channel_models",
		"channel_key_events",
		"channel_model_events",
		"channel_tags",
		"semantic_tags",
		"probe_logs",
		"auto_models",
		"auto_model_members",
	}
}

// V2MigrationFiles 返回 V2 这批迁移的文件名（按执行顺序 = 字典序）。
//
// 0021_v2_drop_legacy 已按裁定更名为 0024 并排到最后：字典序 = 执行序，
// 一次启动跑完整链，不做「手工分两批」。
func V2MigrationFiles() []string {
	return []string{
		"0019_v2_topology.sql",
		"0020_v2_billing_attribution.sql",
		"0022_v2_ledger.sql",
		"0023_v2_reconcile.sql",
		"0024_v2_drop_legacy.sql",
	}
}
