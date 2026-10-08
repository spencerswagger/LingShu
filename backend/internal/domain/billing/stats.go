package billing

// stats.go 统计页（统一仪表盘）聚合查询：按天趋势、多维度 Top、成功率/延迟/吞吐量、Token 构成。
// 所有方法接受可选 userID（nil = 全局，非 nil = 仅该用户），供 admin/dev 共用同一套聚合。
//
// SQL 占位约定：$1 = user_id（可空）、$2 = from、$3 = to；额外参数（LIMIT）从 $4 开始。

import (
	"context"
	"sort"
	"time"
)

// UsageSplit 五段 Token 用量。
type UsageSplit struct {
	Input      int64 `json:"Input"`
	Output     int64 `json:"Output"`
	CacheRead  int64 `json:"CacheRead"`
	CacheWrite int64 `json:"CacheWrite"`
	Reasoning  int64 `json:"Reasoning"`
}

// Total 五段求和。
func (u UsageSplit) Total() int64 {
	return u.Input + u.Output + u.CacheRead + u.CacheWrite + u.Reasoning
}

// DashDay 按天聚合行（趋势 / 成功率 / 吞吐量）。
type DashDay struct {
	Date        string  `json:"Date"` // 2006-01-02
	Calls       int64   `json:"Calls"`
	Failed      int64   `json:"Failed"`
	Credits     float64 `json:"Credits"`
	Tokens      int64   `json:"Tokens"`
	DurationMS  int64   `json:"DurationMS"`
	SuccessRate float64 `json:"SuccessRate"` // 0~1
}

// DashTopItem 多维度 Top 项（模型 / 渠道密钥 / 用户 / 计费模式 / 错误共用）。
type DashTopItem struct {
	Key      string  `json:"Key"`      // 维度键（模型名 / 密钥名 / 用户名 / 模式 / 错误信息）
	Label    string  `json:"Label"`    // 展示名
	SubLabel string  `json:"SubLabel"` // 次级展示（渠道名 / 昵称）
	Calls    int64   `json:"Calls"`
	Failed   int64   `json:"Failed"`
	Credits  float64 `json:"Credits"`
	Tokens   int64   `json:"Tokens"`
}

// DashLatencyBucket 耗时分桶（毫秒）。
type DashLatencyBucket struct {
	Bucket string `json:"Bucket"`
	Count  int64  `json:"Count"`
}

// DashboardStats 统计页单次聚合返回结构。
type DashboardStats struct {
	From      string              `json:"From"` // RFC3339
	To        string              `json:"To"`
	Days      int64               `json:"Days"`
	Total     UsageSplit          `json:"Total"`
	TotalCred float64             `json:"TotalCred"`
	TotalCall int64               `json:"TotalCall"`
	Failed    int64               `json:"Failed"`
	Success   float64             `json:"Success"` // 0~1
	AvgMS     float64             `json:"AvgMS"`
	P50MS     float64             `json:"P50MS"`
	P90MS     float64             `json:"P90MS"`
	P95MS     float64             `json:"P95MS"`
	AvgFirst  float64             `json:"AvgFirst"`
	AvgRPM    float64             `json:"AvgRPM"`
	AvgTPM    float64             `json:"AvgTPM"`
	Daily     []DashDay           `json:"Daily"`
	ByModel   []DashTopItem       `json:"ByModel"`
	ByKey     []DashTopItem       `json:"ByKey"`
	ByUser    []DashTopItem       `json:"ByUser"`
	ByMode    []DashTopItem       `json:"ByMode"`
	Errors    []DashTopItem       `json:"Errors"`
	Latency   []DashLatencyBucket `json:"Latency"`
}

// dashWhere 生成时间范围 + 可选用户过滤 WHERE 子句；prefix 为表别名前缀（如 "br."）。
// 占位符固定 $1(user_id) / $2(from) / $3(to)。
func dashWhere(prefix string) string {
	return `WHERE (` + prefix + `call_time >= $2 AND ` + prefix + `call_time <= $3)
		AND ($1::bigint IS NULL OR ` + prefix + `user_id = $1)`
}

// dashArgs 构造基础参数 [userID(可空), from, to]；extra 追加在 $4 之后。
func dashArgs(userID *int64, from, to time.Time, extra ...any) []any {
	var u any
	if userID != nil {
		u = *userID
	}
	args := []any{u, from, to}
	return append(args, extra...)
}

// tokenSum 五段 token 求和 SQL 表达式（prefix 为表别名前缀）。
func tokenSum(prefix string) string {
	p := prefix + "tokens"
	return `(COALESCE(NULLIF(` + p + `->>'input','')::bigint,0)
		+ COALESCE(NULLIF(` + p + `->>'output','')::bigint,0)
		+ COALESCE(NULLIF(` + p + `->>'cache_read','')::bigint,0)
		+ COALESCE(NULLIF(` + p + `->>'cache_write','')::bigint,0)
		+ COALESCE(NULLIF(` + p + `->>'reasoning','')::bigint,0))`
}

// Dashboard 聚合统计页所需全部数据（一次调用）。
func (s *SqlStore) Dashboard(ctx context.Context, from, to time.Time, userID *int64, limit int) (DashboardStats, error) {
	var out DashboardStats
	out.From = from.Format(time.RFC3339)
	out.To = to.Format(time.RFC3339)
	out.Days = daysBetween(from, to)
	if limit <= 0 {
		limit = 10
	}

	// 1) 总计 / 成功率 / 平均延迟 / 吞吐量 / Token 构成
	var total UsageSplit
	var totalCred float64
	var totalCall, failed int64
	var sumDur, sumFirst, okDur, okFirst int64
	err := s.db.QueryRowContext(ctx,
		`SELECT
		    COALESCE(sum(COALESCE(NULLIF(tokens->>'input','')::bigint,0)),0),
		    COALESCE(sum(COALESCE(NULLIF(tokens->>'output','')::bigint,0)),0),
		    COALESCE(sum(COALESCE(NULLIF(tokens->>'cache_read','')::bigint,0)),0),
		    COALESCE(sum(COALESCE(NULLIF(tokens->>'cache_write','')::bigint,0)),0),
		    COALESCE(sum(COALESCE(NULLIF(tokens->>'reasoning','')::bigint,0)),0),
		    COALESCE(sum(credits_consumed),0),
		    count(*),
		    COALESCE(sum((status='failed')::int),0),
		    COALESCE(sum(duration_ms),0),
		    COALESCE(sum(first_token_ms),0),
		    COALESCE(sum((duration_ms IS NOT NULL)::int),0),
		    COALESCE(sum((first_token_ms IS NOT NULL)::int),0)
		 FROM billing_records `+dashWhere(""),
		dashArgs(userID, from, to)...).
		Scan(&total.Input, &total.Output, &total.CacheRead, &total.CacheWrite, &total.Reasoning,
			&totalCred, &totalCall, &failed, &sumDur, &sumFirst, &okDur, &okFirst)
	if err != nil {
		return out, err
	}
	out.Total = total
	out.TotalCred = totalCred
	out.TotalCall = totalCall
	out.Failed = failed
	if totalCall > 0 {
		out.Success = float64(totalCall-failed) / float64(totalCall)
	}
	if okDur > 0 {
		out.AvgMS = float64(sumDur) / float64(okDur)
	}
	if okFirst > 0 {
		out.AvgFirst = float64(sumFirst) / float64(okFirst)
	}
	if minutes := to.Sub(from).Minutes(); minutes > 0 {
		out.AvgRPM = float64(totalCall) / minutes
		out.AvgTPM = float64(total.Total()) / minutes
	}

	if err := s.dashboardDaily(ctx, from, to, userID, &out); err != nil {
		return out, err
	}
	if err := s.dashboardTop(ctx, from, to, userID, limit, &out); err != nil {
		return out, err
	}
	if err := s.dashboardLatency(ctx, from, to, userID, &out); err != nil {
		return out, err
	}
	if err := s.dashboardPercentiles(ctx, from, to, userID, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (s *SqlStore) dashboardDaily(ctx context.Context, from, to time.Time, userID *int64, out *DashboardStats) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT date_trunc('day', call_time AT TIME ZONE 'Asia/Shanghai')::date,
		        count(*),
		        COALESCE(sum((status='failed')::int),0),
		        COALESCE(sum(credits_consumed),0),
		        COALESCE(sum(`+tokenSum("")+`),0),
		        COALESCE(sum(duration_ms),0)
		 FROM billing_records `+dashWhere("")+`
		 GROUP BY 1 ORDER BY 1`, dashArgs(userID, from, to)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byDate := map[string]DashDay{}
	for rows.Next() {
		var d time.Time
		var dd DashDay
		if err := rows.Scan(&d, &dd.Calls, &dd.Failed, &dd.Credits, &dd.Tokens, &dd.DurationMS); err != nil {
			return err
		}
		dd.Date = d.Format("2006-01-02")
		if dd.Calls > 0 {
			dd.SuccessRate = float64(dd.Calls-dd.Failed) / float64(dd.Calls)
		}
		byDate[dd.Date] = dd
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// 补零：保证区间内日期连续（与 SQL 的 Asia/Shanghai 日界对齐）。
	loc := cnLocation()
	for d := from.In(loc); !d.After(to.In(loc)); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		if _, ok := byDate[key]; !ok {
			byDate[key] = DashDay{Date: key, SuccessRate: 1}
		}
	}
	keys := make([]string, 0, len(byDate))
	for k := range byDate {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out.Daily = make([]DashDay, 0, len(keys))
	for _, k := range keys {
		out.Daily = append(out.Daily, byDate[k])
	}
	return nil
}

func (s *SqlStore) dashboardTop(ctx context.Context, from, to time.Time, userID *int64, limit int, out *DashboardStats) error {
	// 按模型（ORDER BY tokens）
	rows, err := s.db.QueryContext(ctx,
		`SELECT external_model_name,
		        count(*), COALESCE(sum((status='failed')::int),0),
		        COALESCE(sum(credits_consumed),0),
		        COALESCE(sum(`+tokenSum("")+`),0) AS tokens
		 FROM billing_records `+dashWhere("")+`
		 GROUP BY 1 ORDER BY tokens DESC LIMIT $4`, dashArgs(userID, from, to, limit)...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var it DashTopItem
		if err := rows.Scan(&it.Key, &it.Calls, &it.Failed, &it.Credits, &it.Tokens); err != nil {
			rows.Close()
			return err
		}
		it.Label = it.Key
		out.ByModel = append(out.ByModel, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	// 按渠道密钥（密钥名 + 渠道名）
	rows, err = s.db.QueryContext(ctx,
		`SELECT ck.name, ch.name,
		        count(*), COALESCE(sum((br.status='failed')::int),0),
		        COALESCE(sum(br.credits_consumed),0),
		        COALESCE(sum(`+tokenSum("br.")+`),0) AS tokens
		 FROM billing_records br
		 JOIN channel_keys ck ON ck.id = br.channel_key_id
		 JOIN channels ch ON ch.id = ck.channel_id
		 `+dashWhere("br.")+`
		 GROUP BY 1,2 ORDER BY tokens DESC LIMIT $4`, dashArgs(userID, from, to, limit)...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var it DashTopItem
		if err := rows.Scan(&it.Key, &it.SubLabel, &it.Calls, &it.Failed, &it.Credits, &it.Tokens); err != nil {
			rows.Close()
			return err
		}
		it.Label = it.Key
		out.ByKey = append(out.ByKey, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	// 按用户（仅全局视角）
	if userID == nil {
		rows, err = s.db.QueryContext(ctx,
			`SELECT u.username, u.nickname,
			        count(*), COALESCE(sum((br.status='failed')::int),0),
			        COALESCE(sum(br.credits_consumed),0),
			        COALESCE(sum(`+tokenSum("br.")+`),0) AS tokens
			 FROM billing_records br
			 JOIN users u ON u.id = br.user_id
			 `+dashWhere("br.")+`
			 GROUP BY 1,2 ORDER BY tokens DESC LIMIT $4`, dashArgs(userID, from, to, limit)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var it DashTopItem
			if err := rows.Scan(&it.Key, &it.SubLabel, &it.Calls, &it.Failed, &it.Credits, &it.Tokens); err != nil {
				rows.Close()
				return err
			}
			it.Label = it.Key
			out.ByUser = append(out.ByUser, it)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}

	// 计费模式
	rows, err = s.db.QueryContext(ctx,
		`SELECT pricing_mode,
		        count(*), COALESCE(sum((status='failed')::int),0),
		        COALESCE(sum(credits_consumed),0),
		        COALESCE(sum(`+tokenSum("")+`),0) AS tokens
		 FROM billing_records `+dashWhere("")+`
		 GROUP BY 1 ORDER BY tokens DESC`, dashArgs(userID, from, to)...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var it DashTopItem
		if err := rows.Scan(&it.Key, &it.Calls, &it.Failed, &it.Credits, &it.Tokens); err != nil {
			rows.Close()
			return err
		}
		it.Label = it.Key
		out.ByMode = append(out.ByMode, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	// 错误信息聚合（仅失败记录）
	rows, err = s.db.QueryContext(ctx,
		`SELECT COALESCE(NULLIF(error_message,''),'(未记录)') AS msg,
		        count(*) AS c
		 FROM billing_records
		 WHERE status='failed' AND call_time >= $2 AND call_time <= $3
		   AND ($1::bigint IS NULL OR user_id = $1)
		 GROUP BY 1 ORDER BY c DESC LIMIT $4`, dashArgs(userID, from, to, limit)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var it DashTopItem
		if err := rows.Scan(&it.Key, &it.Calls); err != nil {
			return err
		}
		it.Label = it.Key
		it.Failed = it.Calls
		out.Errors = append(out.Errors, it)
	}
	return rows.Err()
}

func (s *SqlStore) dashboardLatency(ctx context.Context, from, to time.Time, userID *int64, out *DashboardStats) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT CASE
		          WHEN duration_ms < 100 THEN '<100ms'
		          WHEN duration_ms < 500 THEN '100-500ms'
		          WHEN duration_ms < 1000 THEN '500ms-1s'
		          WHEN duration_ms < 3000 THEN '1-3s'
		          WHEN duration_ms < 10000 THEN '3-10s'
		          ELSE '>10s' END AS bucket,
		        count(*)
		 FROM billing_records
		 WHERE duration_ms IS NOT NULL AND call_time >= $2 AND call_time <= $3
		   AND ($1::bigint IS NULL OR user_id = $1)
		 GROUP BY 1`, dashArgs(userID, from, to)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var b string
		var n int64
		if err := rows.Scan(&b, &n); err != nil {
			return err
		}
		counts[b] = n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range []string{"<100ms", "100-500ms", "500ms-1s", "1-3s", "3-10s", ">10s"} {
		out.Latency = append(out.Latency, DashLatencyBucket{Bucket: b, Count: counts[b]})
	}
	return nil
}

// dashboardPercentiles 用 SQL 计算 duration_ms 分位数（P50/P90/P95），避免全量取数。
func (s *SqlStore) dashboardPercentiles(ctx context.Context, from, to time.Time, userID *int64, out *DashboardStats) error {
	return s.db.QueryRowContext(ctx,
		`SELECT
		    COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY duration_ms),0),
		    COALESCE(percentile_disc(0.9) WITHIN GROUP (ORDER BY duration_ms),0),
		    COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY duration_ms),0)
		 FROM billing_records
		 WHERE duration_ms IS NOT NULL AND call_time >= $2 AND call_time <= $3
		   AND ($1::bigint IS NULL OR user_id = $1)`,
		dashArgs(userID, from, to)...).Scan(&out.P50MS, &out.P90MS, &out.P95MS)
}

// RangeTotals 仅返回区间的请求数 / 积分 / token 总量（用于环比上期，避免重复全量聚合）。
func (s *SqlStore) RangeTotals(ctx context.Context, from, to time.Time, userID *int64) (calls int64, credits float64, tokens int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT count(*),
		        COALESCE(sum(credits_consumed),0),
		        COALESCE(sum(`+tokenSum("")+`),0)
		 FROM billing_records `+dashWhere(""),
		dashArgs(userID, from, to)...).Scan(&calls, &credits, &tokens)
	return calls, credits, tokens, err
}

func daysBetween(from, to time.Time) int64 {
	days := int64(to.Sub(from).Hours()/24) + 1
	if days < 1 {
		days = 1
	}
	return days
}
