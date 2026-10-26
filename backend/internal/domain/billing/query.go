package billing

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RecordFilter 账单查询过滤条件。所有字段可选，零值表示不过滤。
type RecordFilter struct {
	UserID       *int64
	TokenID      *int64 // 令牌维度（列头按密钥筛选用）
	ChannelKeyID *int64 // 渠道密钥维度（替换原 channel_id；运行时实体）
	SessionID    string // 会话维度（存 x-session-id）
	Model        string // external_model_name 精确匹配
	PricingMode  string // sale | cost
	Status       string // completed | failed
	From         *time.Time
	To           *time.Time
	// RequestID 按网关请求链路 ID 过滤（billing_records 无该列，经 call_logs.request_id 关联）：
	// 调用方报一个 request_id，就能在账单里直接定位到这一次请求（含其所有候选尝试）。
	RequestID string
	// GroupByRequest 列表按「一次请求一行」归并：一次请求若有多次候选尝试会产生多条账单，
	// 归并后只展示主行（优先成功行，其次最新一次），Attempts 记该请求共产生几条账单。
	// 统计（Summarize）不受影响，仍按行聚合（金额口径不变）。
	GroupByRequest bool
	Page           int
	Size           int
}

// recordWhere 构建账单过滤 WHERE 子句与参数（ListRecords/Summarize 共用，避免两处漂移）。
// Model 过滤同时匹配 external_model_name 与 auto_model_name：Auto 请求的账单
// external_model_name 记命中成员名，用户按 Auto 名也应能搜到（见设计决策 #6）。
func recordWhere(f RecordFilter) (string, []any) { return recordWhereOn("", f) }

// recordWhereOn 与 recordWhere 相同，但给列名加表别名前缀（多表 JOIN 的归并查询必须限定别名，
// 否则 call_logs 与 billing_records 的同名列会歧义）。prefix 形如 "br."，空串表示不限定。
// RequestID 过滤只在非空时追加条件与参数：其余场景生成的 SQL 与参数位与历史完全一致
// （不无谓平移占位符，避免既有 SQL 断言与执行计划漂移）。
func recordWhereOn(prefix string, f RecordFilter) (string, []any) {
	where := fmt.Sprintf(`WHERE ($1::bigint IS NULL OR %[1]suser_id = $1)
	          AND ($2::bigint IS NULL OR %[1]schannel_key_id = $2)
	          AND ($3::bigint IS NULL OR %[1]stoken_id = $3)
	          AND ($4 = '' OR %[1]ssession_id = $4)
	          AND ($5 = '' OR %[1]sexternal_model_name = $5 OR %[1]sauto_model_name = $5)
	          AND ($6 = '' OR %[1]spricing_mode = $6)
	          AND ($7 = '' OR %[1]sstatus = $7)
	          AND ($8::timestamptz IS NULL OR %[1]scall_time >= $8)
	          AND ($9::timestamptz IS NULL OR %[1]scall_time <= $9)`, prefix)
	args := []any{
		nullableInt64(f.UserID), nullableInt64(f.ChannelKeyID), nullableInt64(f.TokenID),
		f.SessionID, f.Model, f.PricingMode, f.Status,
		nullableTime(f.From), nullableTime(f.To),
	}
	if f.RequestID != "" {
		// 一次请求可能产生多条账单（多候选/多次上游失败），按 request_id 命中即整组一起返回。
		//
		// ⚠️ 关联列必须**限定到外层表**：子查询里 call_logs 也有 billing_id，
		// 写成裸 `billing_id` 会解析到子查询自己的列，条件退化成 `clr.billing_id = clr.billing_id`
		// 恒真（PR #63 验收 P1：按 request_id 搜索后展开子记录会返回别人的账单）。
		outerBillingID := "billing_records.billing_id" // 无前缀时外层表名就是 billing_records
		if prefix != "" {
			outerBillingID = prefix + "billing_id"
		}
		args = append(args, f.RequestID)
		where += fmt.Sprintf(` AND EXISTS (SELECT 1 FROM call_logs clr
		            WHERE clr.billing_id = %[1]s AND clr.request_id = $%[2]d)`,
			outerBillingID, len(args))
	}
	return where, args
}

// ListRecords 分页查询账单记录，返回列表与总条数（Record 结构，字段已解析）。
// f.GroupByRequest=true 时按「一次请求一行」归并（见 listRecordsGrouped）。
func (s *SqlStore) ListRecords(ctx context.Context, f RecordFilter) ([]Record, int64, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.Size < 1 {
		f.Size = 20
	}
	if f.GroupByRequest {
		return s.listRecordsGrouped(ctx, f)
	}
	offset := int64((f.Page - 1) * f.Size)
	where, args := recordWhere(f)

	var total int64
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM billing_records `+where, args...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count billing records: %w", err)
	}

	limitArg, offsetArg := len(args)+1, len(args)+2
	args = append(args, f.Size, offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+recordCols+` FROM billing_records `+where+
			fmt.Sprintf(` ORDER BY id DESC LIMIT $%d OFFSET $%d`, limitArg, offsetArg), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list billing records: %w", err)
	}
	defer rows.Close()

	list := make([]Record, 0, f.Size)
	for rows.Next() {
		rec, err := sqlScan(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan billing record: %w", err)
		}
		list = append(list, *rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// groupedExtraCols 把「归并附加列」（request_id、该请求的账单条数）接到 sqlScan 的扫描目标后面，
// 从而复用同一个扫描实现（不复制 25 个列的扫描代码，避免两处漂移）。
type groupedExtraCols struct {
	row interface{ Scan(...any) error }
	rid *sql.NullString
	cnt *int
}

// Scan 实现扫描接口：先按 recordCols 顺序填 dest，再补两个附加列。
func (g groupedExtraCols) Scan(dest ...any) error {
	return g.row.Scan(append(dest, g.rid, g.cnt)...)
}

// listRecordsGrouped 按「一次请求一行」归并分页：
// 分组键 = call_logs.request_id（无调用日志时退回 billing_id，保证每次请求独立成行）。
// 主行选取：优先 status='completed' 的行，其次 id 最大（最新一次尝试）——即「成功的那次优先，
// 全失败时展示最后一次失败的现场」。Attempts 为该请求产生的账单条数（多次候选/多次上游失败）。
// 注意：金额类聚合仍走 Summarize（按行），归并只影响列表展示，不改变计费口径。
func (s *SqlStore) listRecordsGrouped(ctx context.Context, f RecordFilter) ([]Record, int64, error) {
	offset := int64((f.Page - 1) * f.Size)
	where, args := recordWhereOn("br.", f)
	base := ` FROM billing_records br LEFT JOIN call_logs cl ON cl.billing_id = br.billing_id ` + where

	var total int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM (
		    SELECT 1`+base+`
		    GROUP BY COALESCE(NULLIF(cl.request_id, ''), br.billing_id)) grp`, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count grouped billing records: %w", err)
	}

	limitArg, offsetArg := len(args)+1, len(args)+2
	args = append(args, f.Size, offset)
	rows, err := s.db.QueryContext(ctx,
		`WITH grp AS (
		    SELECT COALESCE(NULLIF(cl.request_id, ''), br.billing_id) AS g,
		           COALESCE(cl.request_id, '') AS rid,
		           (array_agg(br.billing_id ORDER BY (br.status = 'completed') DESC, br.id DESC))[1] AS main_bid,
		           count(*)::int AS cnt`+base+`
		    GROUP BY 1, 2)
		 SELECT `+recordCols+`, grp.rid, grp.cnt
		 FROM grp JOIN billing_records ON billing_records.billing_id = grp.main_bid
		 ORDER BY billing_records.id DESC `+fmt.Sprintf(`LIMIT $%d OFFSET $%d`, limitArg, offsetArg), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list grouped billing records: %w", err)
	}
	defer rows.Close()

	list := make([]Record, 0, f.Size)
	for rows.Next() {
		var rid sql.NullString
		var cnt int
		rec, err := sqlScan(groupedExtraCols{row: rows, rid: &rid, cnt: &cnt})
		if err != nil {
			return nil, 0, fmt.Errorf("scan grouped billing record: %w", err)
		}
		rec.RequestID = rid.String
		rec.Attempts = cnt
		list = append(list, *rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// busyFilter 忙时口径的行过滤条件：只统计「成功（completed）且带耗时」的请求。
// 分子（请求数 / token）与分母（耗时之和）必须共用同一条件，
// 否则会重新引入「分子含失败、分母不含失败」的口径错位（失败请求哪怕写了耗时也两侧同时剔除）。
// 该常量是 RecordSummary 与 DashboardStats 两处忙时口径的唯一来源，避免两处漂移。
const busyFilter = `status = 'completed' AND duration_ms IS NOT NULL`

// busyFilterOn 返回带表别名的忙时过滤条件（busyFilter 的前缀版本，口径完全一致）。
// 多表 JOIN 的查询（模型广场按实体聚合）必须限定别名，否则列名有歧义。
func busyFilterOn(prefix string) string {
	if prefix == "" {
		return busyFilter
	}
	return prefix + `status = 'completed' AND ` + prefix + `duration_ms IS NOT NULL`
}

// RecordSummary 账单筛选聚合结果（统计栏）：请求数、总积分、总 token（三段求和）、总耗时、
// 以及「忙时处理速率」口径的 RPM/TPM（见 BusyRPM/BusyTPM）。
// 注：三段互斥（输入为纯输入、不含缓存读），总量统计直接求和 input/cache_read/output
// （与 stats.go UsageSplit.Total() 口径一致）；缓存命中率分母 = InputTokens + CacheReadTokens。
type RecordSummary struct {
	Requests        int64   `json:"Requests"`
	CreditsTotal    float64 `json:"CreditsTotal"`
	TokensTotal     int64   `json:"TokensTotal"`
	DurationTotalMS int64   `json:"DurationTotalMS"`
	InputTokens     int64   `json:"InputTokens"`
	CacheReadTokens int64   `json:"CacheReadTokens"`
	// 忙时口径（长区间不用墙钟平均，用「忙时处理速率」= 处理数 ÷ 这批请求的耗时之和 ≈ 1 ÷ 平均耗时）：
	// 只统计成功且带耗时的行（busyFilter），BusyRequests/BusyTokens/BusyDurationMS 三者严格同源；
	// BusyRPM/BusyTPM = BusyRequests/BusyTokens ÷ (BusyDurationMS/60000)，分母 ≤0 时为 0。
	BusyRequests   int64   `json:"BusyRequests"`
	BusyTokens     int64   `json:"BusyTokens"`
	BusyDurationMS int64   `json:"BusyDurationMS"` // 忙时分母（成功且带耗时请求的 duration_ms 之和）
	BusyRPM        float64 `json:"BusyRPM"`
	BusyTPM        float64 `json:"BusyTPM"`
}

// BusyRate 由「计数 ÷ 忙时分母」得出忙时处理速率（每分钟）。
// 忙时分母 = 这批成功请求的耗时之和折算成分钟（≈1 ÷ 平均耗时）；分母非正时返回 0。
// 该函数是 RecordSummary 与 DashboardStats 两处忙时口径的唯一实现，避免两处漂移。
func BusyRate(count float64, sumDurationMS int64) float64 {
	if sumDurationMS <= 0 {
		return 0
	}
	return count / (float64(sumDurationMS) / 60000.0)
}

// Summarize 按与列表完全相同的过滤条件聚合统计（不分页），供账单页统计栏展示。
// 忙时口径：请求数/token/耗时三者都用 FILTER (WHERE status='completed' AND duration_ms IS NOT NULL) 统计，
// 分母为 0 时 BusyRPM/BusyTPM 返回 0（其余统计口径不受 status 影响）。
func (s *SqlStore) Summarize(ctx context.Context, f RecordFilter) (RecordSummary, error) {
	where, args := recordWhere(f)
	var out RecordSummary
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*),
		        COALESCE(sum(credits_consumed), 0),
		        COALESCE(sum(
		            COALESCE(NULLIF(tokens->>'input','')::bigint, 0) +
		            COALESCE(NULLIF(tokens->>'cache_read','')::bigint, 0) +
		            COALESCE(NULLIF(tokens->>'output','')::bigint, 0)), 0),
		        COALESCE(sum(duration_ms), 0),
		        COALESCE(sum(COALESCE(NULLIF(tokens->>'input','')::bigint, 0)), 0),
		        COALESCE(sum(COALESCE(NULLIF(tokens->>'cache_read','')::bigint, 0)), 0),
		        COALESCE(count(*) FILTER (WHERE `+busyFilter+`), 0),
		        COALESCE(sum(
		            COALESCE(NULLIF(tokens->>'input','')::bigint, 0) +
		            COALESCE(NULLIF(tokens->>'cache_read','')::bigint, 0) +
		            COALESCE(NULLIF(tokens->>'output','')::bigint, 0)
		        ) FILTER (WHERE `+busyFilter+`), 0),
		        COALESCE(sum(duration_ms) FILTER (WHERE `+busyFilter+`), 0)
		 FROM billing_records `+where, args...).
		Scan(&out.Requests, &out.CreditsTotal, &out.TokensTotal, &out.DurationTotalMS,
			&out.InputTokens, &out.CacheReadTokens,
			&out.BusyRequests, &out.BusyTokens, &out.BusyDurationMS)
	if err != nil {
		return out, fmt.Errorf("summarize billing records: %w", err)
	}
	out.BusyRPM = BusyRate(float64(out.BusyRequests), out.BusyDurationMS)
	out.BusyTPM = BusyRate(float64(out.BusyTokens), out.BusyDurationMS)
	return out, nil
}

// cnLocation 返回北京时间时区；加载失败回退进程本地时区（进程时区已在启动时统一为 Asia/Shanghai）。
func cnLocation() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.Local
	}
	return loc
}

// DailyUsage 按日聚合某用户在最近 days 天内的调用量与积分消耗。
// model 非空时按 external_model_name 过滤。按日切分以北京时间（Asia/Shanghai）为准。
type DailyUsage struct {
	Date    string  `json:"Date"` // 2006-01-02
	Credits float64 `json:"Credits"`
	Calls   int64   `json:"Calls"`
}

// DailyUsage 查询按日聚合结果（含零量日期占位可忽略）。
func (s *SqlStore) DailyUsage(ctx context.Context, userID int64, days int, model string) ([]DailyUsage, error) {
	if days <= 0 {
		days = 30
	}
	now := time.Now().In(cnLocation())
	cutoff := now.AddDate(0, 0, -(days - 1))
	cutoff = time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, cutoff.Location())

	rows, err := s.db.QueryContext(ctx,
		`SELECT date_trunc('day', call_time AT TIME ZONE 'Asia/Shanghai')::date AS d,
		        count(*) AS calls,
		        COALESCE(sum(credits_consumed), 0) AS credits
		 FROM billing_records
		 WHERE user_id = $1 AND status = 'completed' AND call_time >= $2
		   AND ($3 = '' OR external_model_name = $3)
		 GROUP BY d ORDER BY d ASC`,
		userID, cutoff, model)
	if err != nil {
		return nil, fmt.Errorf("daily usage: %w", err)
	}
	defer rows.Close()

	out := make([]DailyUsage, 0, days)
	for rows.Next() {
		var d time.Time
		var u DailyUsage
		if err := rows.Scan(&d, &u.Calls, &u.Credits); err != nil {
			return nil, fmt.Errorf("scan daily usage: %w", err)
		}
		u.Date = d.Format("2006-01-02")
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// UpsertConfig 写入/更新一条 sys_configs 配置，写入成功后触发写钩子（失效配置缓存），立即生效。
func (s *SqlStore) UpsertConfig(ctx context.Context, key string, value []byte, updatedBy int64) error {
	var updater any
	if updatedBy != 0 {
		updater = updatedBy
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sys_configs(key, value, updated_by)
		 VALUES($1, $2::jsonb, $3)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now(), updated_by = EXCLUDED.updated_by`,
		key, value, updater)
	if err != nil {
		return fmt.Errorf("upsert config %s: %w", key, err)
	}
	if s.onConfigWrite != nil {
		s.onConfigWrite()
	}
	return nil
}

func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableTime(v *time.Time) any {
	if v == nil {
		return nil
	}
	return *v
}
