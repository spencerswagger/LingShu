package billing

import (
	"context"
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
	Page         int
	Size         int
}

// recordWhere 构建账单过滤 WHERE 子句与参数（ListRecords/Summarize 共用，避免两处漂移）。
func recordWhere(f RecordFilter) (string, []any) {
	where := `WHERE ($1::bigint IS NULL OR user_id = $1)
	          AND ($2::bigint IS NULL OR channel_key_id = $2)
	          AND ($3::bigint IS NULL OR token_id = $3)
	          AND ($4 = '' OR session_id = $4)
	          AND ($5 = '' OR external_model_name = $5)
	          AND ($6 = '' OR pricing_mode = $6)
	          AND ($7 = '' OR status = $7)
	          AND ($8::timestamptz IS NULL OR call_time >= $8)
	          AND ($9::timestamptz IS NULL OR call_time <= $9)`
	args := []any{
		nullableInt64(f.UserID), nullableInt64(f.ChannelKeyID), nullableInt64(f.TokenID),
		f.SessionID, f.Model, f.PricingMode, f.Status,
		nullableTime(f.From), nullableTime(f.To),
	}
	return where, args
}

// ListRecords 分页查询账单记录，返回列表与总条数（Record 结构，字段已解析）。
func (s *SqlStore) ListRecords(ctx context.Context, f RecordFilter) ([]Record, int64, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.Size < 1 {
		f.Size = 20
	}
	offset := int64((f.Page - 1) * f.Size)
	where, args := recordWhere(f)

	var total int64
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM billing_records `+where, args...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count billing records: %w", err)
	}

	args = append(args, f.Size, offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+recordCols+` FROM billing_records `+where+
			` ORDER BY id DESC LIMIT $10 OFFSET $11`, args...)
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

// RecordSummary 账单筛选聚合结果（统计栏）：请求数、总积分、总 token、总耗时。
// 总 token 口径与 UsageSplit.Total 一致：缓存读是输入的子集（不重复计入），
// 缓存写为独立段（计入），推理已从输出剥离（单独加 reasoning）。
type RecordSummary struct {
	Requests        int64   `json:"Requests"`
	CreditsTotal    float64 `json:"CreditsTotal"`
	TokensTotal     int64   `json:"TokensTotal"`
	DurationTotalMS int64   `json:"DurationTotalMS"`
}

// Summarize 按与列表完全相同的过滤条件聚合统计（不分页），供账单页统计栏展示。
func (s *SqlStore) Summarize(ctx context.Context, f RecordFilter) (RecordSummary, error) {
	where, args := recordWhere(f)
	var out RecordSummary
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*),
		        COALESCE(sum(credits_consumed), 0),
		        COALESCE(sum(
		            COALESCE(NULLIF(tokens->>'input','')::bigint, 0) +
		            COALESCE(NULLIF(tokens->>'cache_write','')::bigint, 0) +
		            COALESCE(NULLIF(tokens->>'output','')::bigint, 0) +
		            COALESCE(NULLIF(tokens->>'reasoning','')::bigint, 0)), 0),
		        COALESCE(sum(duration_ms), 0)
		 FROM billing_records `+where, args...).
		Scan(&out.Requests, &out.CreditsTotal, &out.TokensTotal, &out.DurationTotalMS)
	if err != nil {
		return out, fmt.Errorf("summarize billing records: %w", err)
	}
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

// UpsertConfig 写入/更新一条 sys_configs 配置，立即生效（无缓存）。
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
