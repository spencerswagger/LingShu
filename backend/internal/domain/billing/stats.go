package billing

// stats.go 统计页（统一仪表盘）聚合查询：按天趋势、多维度 Top、成功率/延迟/吞吐量、Token 构成。
// 所有方法接受可选 userID（nil = 全局，非 nil = 仅该用户），供 admin/dev 共用同一套聚合。
//
// SQL 占位约定：$1 = user_id（可空）、$2 = from、$3 = to；额外参数（LIMIT）从 $4 开始。

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// UsageSplit 三段 Token 用量。
type UsageSplit struct {
	Input     int64 `json:"Input"`
	Output    int64 `json:"Output"`
	CacheRead int64 `json:"CacheRead"`
}

// Total 求总量：三段互斥（Input 为纯输入、不含缓存读），直接求和。
func (u UsageSplit) Total() int64 {
	return u.Input + u.CacheRead + u.Output
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
	From      string     `json:"From"` // RFC3339
	To        string     `json:"To"`
	Days      int64      `json:"Days"`
	Total     UsageSplit `json:"Total"`
	TotalCred float64    `json:"TotalCred"`
	TotalCall int64      `json:"TotalCall"`
	Failed    int64      `json:"Failed"`
	Success   float64    `json:"Success"` // 0~1
	AvgMS     float64    `json:"AvgMS"`
	P50MS     float64    `json:"P50MS"`
	P90MS     float64    `json:"P90MS"`
	P95MS     float64    `json:"P95MS"`
	AvgFirst  float64    `json:"AvgFirst"`
	AvgRPM    float64    `json:"AvgRPM"`
	AvgTPM    float64    `json:"AvgTPM"`
	// 忙时口径（长区间用它代替墙钟平均）：BusyRPM/BusyTPM = 忙时处理数 ÷ 忙时分母，
	// 忙时分母 = 这批「成功且带耗时」请求的耗时之和折算成分钟（≈1 ÷ 平均耗时）；分母为 0 时两者为 0。
	// 分子分母同源（busyFilter：status='completed' AND duration_ms IS NOT NULL），
	// 失败请求哪怕带耗时也两侧同时剔除。BusyRequests 是忙时口径的分子，
	// 前端判断样本量必须用它而不是 TotalCall。SumDurationMS 同为忙时口径（= BusyDurationMS）。
	BusyRequests   int64     `json:"BusyRequests"`
	BusyDurationMS int64     `json:"BusyDurationMS"`
	BusyRPM        float64   `json:"BusyRPM"`
	BusyTPM        float64   `json:"BusyTPM"`
	SumDurationMS  int64     `json:"SumDurationMS"`
	Daily          []DashDay `json:"Daily"`
	// Granularity 实际出桶粒度：hour|day（由 console 层按窗口跨度决定后回填）；
	// Hourly 仅在 hour 粒度下有内容，day 粒度下为未删的空数组（老调用方读 Daily 不受影响）。
	Granularity string              `json:"Granularity"`
	Hourly      []DashHour          `json:"Hourly"`
	Compare     []DashSeries        `json:"Compare"`
	ByModel     []DashTopItem       `json:"ByModel"`
	ByKey       []DashTopItem       `json:"ByKey"`
	ByUser      []DashTopItem       `json:"ByUser"`
	ByMode      []DashTopItem       `json:"ByMode"`
	Errors      []DashTopItem       `json:"Errors"`
	Latency     []DashLatencyBucket `json:"Latency"`
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

// tokenSum token 求和 SQL 表达式（prefix 为表别名前缀）。
// 口径与 UsageSplit.Total() 一致：三段互斥（输入为纯输入、不含缓存读），求和 input/cache_read/output。
func tokenSum(prefix string) string {
	p := prefix + "tokens"
	return `(COALESCE(NULLIF(` + p + `->>'input','')::bigint,0)
		+ COALESCE(NULLIF(` + p + `->>'cache_read','')::bigint,0)
		+ COALESCE(NULLIF(` + p + `->>'output','')::bigint,0))`
}

// TokenSumExpr 导出三段 Token 求和表达式（同一口径的唯一实现），
// 供 console 层（实时会话 / 在线用户聚合）复用，避免各处再抄一份 SQL 造成口径漂移。
func TokenSumExpr(prefix string) string { return tokenSum(prefix) }

// InputTokensExpr 输入 Token 表达式（input + cache_read，三段互斥口径），
// 即「上下文长度」：一次请求送进模型的历史 + 本轮输入。
func InputTokensExpr(prefix string) string {
	p := prefix + "tokens"
	return `(COALESCE(NULLIF(` + p + `->>'input','')::bigint,0)
		+ COALESCE(NULLIF(` + p + `->>'cache_read','')::bigint,0))`
}

// Dashboard 聚合统计页所需全部数据（一次调用）。
func (s *SqlStore) Dashboard(ctx context.Context, from, to time.Time, userID *int64, limit int) (DashboardStats, error) {
	var out DashboardStats
	out.From = from.Format(time.RFC3339)
	out.To = to.Format(time.RFC3339)
	out.Days = daysBetween(from, to)
	// 新增字段默认给空数组（不是 null），老调用方客户端不会因为 null 崩；粒度默认 day。
	out.Granularity = "day"
	out.Hourly = []DashHour{}
	out.Compare = []DashSeries{}
	if limit <= 0 {
		limit = 10
	}

	// 1) 总计 / 成功率 / 平均延迟 / 吞吐量 / Token 构成
	var total UsageSplit
	var totalCred float64
	var totalCall, failed int64
	var sumDur, sumFirst, okDur, okFirst int64
	// 忙时口径专用的三个量（只统计成功且带耗时的行），与 sumDur/okDur（全部行，供 AvgMS 用）互不影响。
	var busyRequests, busyTokens, busyDur int64
	err := s.db.QueryRowContext(ctx,
		`SELECT
		    COALESCE(sum(COALESCE(NULLIF(tokens->>'input','')::bigint,0)),0),
		    COALESCE(sum(COALESCE(NULLIF(tokens->>'output','')::bigint,0)),0),
		    COALESCE(sum(COALESCE(NULLIF(tokens->>'cache_read','')::bigint,0)),0),
		    COALESCE(sum(credits_consumed),0),
		    count(*),
		    COALESCE(sum((status='failed')::int),0),
		    COALESCE(sum(duration_ms),0),
		    COALESCE(sum(first_token_ms),0),
		    COALESCE(sum((duration_ms IS NOT NULL)::int),0),
		    COALESCE(sum((first_token_ms IS NOT NULL)::int),0),
		    COALESCE(count(*) FILTER (WHERE `+busyFilter+`),0),
		    COALESCE(sum(`+tokenSum("")+`) FILTER (WHERE `+busyFilter+`),0),
		    COALESCE(sum(duration_ms) FILTER (WHERE `+busyFilter+`),0)
		 FROM billing_records `+dashWhere(""),
		dashArgs(userID, from, to)...).
		Scan(&total.Input, &total.Output, &total.CacheRead,
			&totalCred, &totalCall, &failed, &sumDur, &sumFirst, &okDur, &okFirst,
			&busyRequests, &busyTokens, &busyDur)
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
	// 忙时口径：分子分母都只取「成功且带耗时」的行（busyFilter），失败请求两侧同时剔除。
	out.BusyRequests = busyRequests
	out.BusyDurationMS = busyDur
	out.SumDurationMS = busyDur // 前端样本标注用同一口径（忙时分母）
	out.BusyRPM = BusyRate(float64(busyRequests), busyDur)
	out.BusyTPM = BusyRate(float64(busyTokens), busyDur)

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

// ===== 小时粒度趋势 / 多日同期对比 / 模型广场 =====
//
// 时区与边界口径（全节统一）：
//   - 「日」「小时」桶一律按 Asia/Shanghai 切分，与 dashboardDaily 的日界同源：
//     一条记录归属哪个桶，看它的 call_time 换算到北京时间后落在哪个自然日 / 自然小时。
//   - 小时桶起点 = 该自然小时的整点（如 2026-10-25T14:00，北京时间墙钟）。
//     区间首尾不在整点时，首尾桶是「不完整桶」（只含区间内的那部分记录），
//     刻意不向外扩展成整点，避免把区间外的数据算进来。
//   - 缺数据的小时桶**不出行**（不补零）：趋势图据此断线，空值与 0 的语义必须可区分。

// MaxCompareDays 多日同期对比允许的最大天数（今天 / 昨天 / 前天 / 大前天 = 4）。
const MaxCompareDays = 7

// MaxModelLimit 模型广场单次返回的最大行数（超出按排序截断，用 Total/Truncated 告知）。
const MaxModelLimit = 200

// 模型广场排序键（其它值按 tokens 处理）。
const (
	ModelSortTokens  = "tokens"  // 窗口内 Token 用量（默认）
	ModelSortTPM     = "tpm"     // 忙时口径吞吐（Token/分钟）
	ModelSortFailed  = "failed"  // 失败数
	ModelSortLatency = "latency" // 全程平均耗时
)

// compareLabels 同期对比的前四个标签（相对「今天」回溯）；更早的日期用 MM-DD。
var compareLabels = []string{"今天", "昨天", "前天", "大前天"}

// DashHour 按小时聚合行（趋势图的数据点）。
type DashHour struct {
	Hour        string  `json:"Hour"`      // 桶起点，2006-01-02T15:04（北京时间墙钟）
	Day         string  `json:"Day"`       // 所属自然日 2006-01-02（北京时间）
	HourOfDay   int     `json:"HourOfDay"` // 0~23，跨日对齐用
	Calls       int64   `json:"Calls"`
	Failed      int64   `json:"Failed"`
	Credits     float64 `json:"Credits"`
	Tokens      int64   `json:"Tokens"`
	DurationMS  int64   `json:"DurationMS"`
	SuccessRate float64 `json:"SuccessRate"` // 0~1（Calls=0 时为 0）
}

// DashSeries 多日同期对比里的一条按小时序列。
type DashSeries struct {
	Day    string     `json:"Day"`    // 该序列所属自然日 2006-01-02（北京时间）
	Label  string     `json:"Label"`  // 今天 / 昨天 / 前天 / 大前天；更早为 MM-DD
	Offset int        `json:"Offset"` // 相对「今天」的回溯天数：0=今天，1=昨天 …
	Points []DashHour `json:"Points"` // 只含有数据的小时桶（缺桶不出行，前端据此断线）
}

// DashModelStat 模型广场行：窗口内按对外模型聚合的用量与质量指标。
// 零用量模型同样出现（Calls=0、Active=false），供前端折叠。
type DashModelStat struct {
	Model      string  `json:"Model"`      // 对外模型名
	Label      string  `json:"Label"`      // 展示名（= Model）
	Enabled    bool    `json:"Enabled"`    // 对外模型是否启用
	Calls      int64   `json:"Calls"`      // 窗口内请求数
	Failed     int64   `json:"Failed"`     // 窗口内失败数
	FailRate   float64 `json:"FailRate"`   // Failed/Calls，0~1（Calls=0 为 0）
	Tokens     int64   `json:"Tokens"`     // 窗口内 Token（三段求和口径）
	Credits    float64 `json:"Credits"`    // 窗口内积分
	TPM        float64 `json:"TPM"`        // 忙时口径吞吐：成功且带耗时请求的 Token ÷ 同批耗时折算分钟
	BusyCalls  int64   `json:"BusyCalls"`  // 忙时口径分子（成功且带耗时请求数）
	BusyMS     int64   `json:"BusyMS"`     // 忙时口径分母（同批请求耗时之和，毫秒）
	AvgMS      float64 `json:"AvgMS"`      // 全程平均耗时（有耗时行）
	AvgFirstMS float64 `json:"AvgFirstMS"` // 首字平均耗时（有首字时间的行）
	Active     bool    `json:"Active"`     // 窗口内是否有请求（Calls>0）
}

// ModelPlaza 模型广场聚合结果（List 已按 Sort 排序并截断到 Limit）。
type ModelPlaza struct {
	Sort      string          `json:"Sort"`      // 实际生效的排序键
	Total     int             `json:"Total"`     // 对外模型目录总数（未截断）
	Active    int             `json:"Active"`    // 窗口内有请求的模型数
	Limit     int             `json:"Limit"`     // 本次返回上限
	Truncated bool            `json:"Truncated"` // Total > len(List)：还有模型未返回
	List      []DashModelStat `json:"List"`      // 零用量模型排在后面
}

// startOfCNDay 返回 t（按北京时间解读）所在自然日的 00:00:00。
func startOfCNDay(t time.Time) time.Time {
	loc := cnLocation()
	c := t.In(loc)
	return time.Date(c.Year(), c.Month(), c.Day(), 0, 0, 0, 0, loc)
}

// compareLabel 同期对比的展示标签：前 4 天用中文相对词，更早用 MM-DD。
func compareLabel(offset int, day time.Time) string {
	if offset >= 0 && offset < len(compareLabels) {
		return compareLabels[offset]
	}
	return day.Format("01-02")
}

// DashboardHourly 按小时聚合区间用量（北京时间小时界）。
// 只返回有数据的小时桶，不补零（缺数据 = 前端断线，而不是 0 值）。
func (s *SqlStore) DashboardHourly(ctx context.Context, from, to time.Time, userID *int64) ([]DashHour, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT date_trunc('hour', call_time AT TIME ZONE 'Asia/Shanghai'),
		        count(*),
		        COALESCE(sum((status='failed')::int),0),
		        COALESCE(sum(credits_consumed),0),
		        COALESCE(sum(`+tokenSum("")+`),0),
		        COALESCE(sum(duration_ms),0)
		 FROM billing_records `+dashWhere("")+`
		 GROUP BY 1 ORDER BY 1`, dashArgs(userID, from, to)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DashHour, 0, 24)
	for rows.Next() {
		var bucket time.Time
		var dh DashHour
		if err := rows.Scan(&bucket, &dh.Calls, &dh.Failed, &dh.Credits, &dh.Tokens, &dh.DurationMS); err != nil {
			return nil, err
		}
		dh.Hour = bucket.Format("2006-01-02T15:04")
		dh.Day = bucket.Format("2006-01-02")
		dh.HourOfDay = bucket.Hour()
		if dh.Calls > 0 {
			dh.SuccessRate = float64(dh.Calls-dh.Failed) / float64(dh.Calls)
		}
		out = append(out, dh)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DashboardCompare 返回「今天 / 昨天 / 前天 / 大前天」等 N 条按小时序列（同日同时刻对齐）。
// 参数 to 决定「今天」是哪一天（按北京时间取自然日）；查询区间为
// [今天-(days-1) 的 00:00, to]，因此今天只会有到当前时刻为止的桶。
// 序列内的 Points 按 HourOfDay 升序，缺失小时不出现在 Points 里。
func (s *SqlStore) DashboardCompare(ctx context.Context, to time.Time, userID *int64, days int) ([]DashSeries, error) {
	if days <= 0 {
		return []DashSeries{}, nil
	}
	if days > MaxCompareDays {
		days = MaxCompareDays
	}
	today := startOfCNDay(to)
	from := today.AddDate(0, 0, -(days - 1))

	rows, err := s.db.QueryContext(ctx,
		`SELECT date_trunc('day', call_time AT TIME ZONE 'Asia/Shanghai')::date,
		        EXTRACT(hour FROM call_time AT TIME ZONE 'Asia/Shanghai')::int,
		        count(*),
		        COALESCE(sum((status='failed')::int),0),
		        COALESCE(sum(credits_consumed),0),
		        COALESCE(sum(`+tokenSum("")+`),0),
		        COALESCE(sum(duration_ms),0)
		 FROM billing_records `+dashWhere("")+`
		 GROUP BY 1,2 ORDER BY 1,2`, dashArgs(userID, from, to)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byDay := make(map[string]map[int]DashHour, days)
	for rows.Next() {
		var day time.Time
		var hourOfDay int
		var dh DashHour
		if err := rows.Scan(&day, &hourOfDay, &dh.Calls, &dh.Failed, &dh.Credits, &dh.Tokens, &dh.DurationMS); err != nil {
			return nil, err
		}
		dh.Day = day.Format("2006-01-02")
		dh.HourOfDay = hourOfDay
		dh.Hour = fmt.Sprintf("%sT%02d:00", dh.Day, hourOfDay)
		if dh.Calls > 0 {
			dh.SuccessRate = float64(dh.Calls-dh.Failed) / float64(dh.Calls)
		}
		if byDay[dh.Day] == nil {
			byDay[dh.Day] = make(map[int]DashHour, 24)
		}
		byDay[dh.Day][hourOfDay] = dh
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]DashSeries, 0, days)
	for offset := 0; offset < days; offset++ {
		day := today.AddDate(0, 0, -offset)
		key := day.Format("2006-01-02")
		series := DashSeries{
			Day:    key,
			Label:  compareLabel(offset, day),
			Offset: offset,
			Points: make([]DashHour, 0, 24),
		}
		for h := 0; h < 24; h++ {
			if p, ok := byDay[key][h]; ok {
				series.Points = append(series.Points, p)
			}
		}
		out = append(out, series)
	}
	return out, nil
}

// modelSortExpr 模型广场排序表达式（白名单，避免把用户输入拼进 SQL）。
func modelSortExpr(sortKey string) string {
	switch sortKey {
	case ModelSortTPM:
		return `CASE WHEN COALESCE(a.busy_ms,0) > 0
		             THEN COALESCE(a.busy_tokens,0)::float8 * 60000.0 / a.busy_ms
		             ELSE 0 END`
	case ModelSortFailed:
		return `COALESCE(a.failed,0)`
	case ModelSortLatency:
		return `COALESCE(a.avg_ms,0)`
	default:
		return `COALESCE(a.tokens,0)`
	}
}

// NormalizeModelSort 归一排序键：非法值一律回落到 tokens（不报错，保证老调用方不受影响）。
func NormalizeModelSort(sortKey string) string {
	switch sortKey {
	case ModelSortTPM, ModelSortFailed, ModelSortLatency:
		return sortKey
	default:
		return ModelSortTokens
	}
}

// DashboardModels 模型广场：以「对外模型目录」为骨架 LEFT JOIN 窗口聚合，
// 因此窗口内零用量的模型也会出现（Calls=0、Active=false），供前端折叠。
//
// 三个吞吐口径不要混用（详见 docs/designs/2026-10-25-stats-dashboard-backend.md）：
//   - 首屏 KPI.TPM = 全窗口忙时口径（BusyTPM）；
//   - 本函数 TPM = **按模型**的忙时口径（同模型成功且带耗时请求的 Token ÷ 同批耗时折算分钟）；
//   - 实时会话 / 在线用户的 TPM = 近 Window 分钟墙钟平均（见 console.live）。
//
// 已从目录删除的历史模型不出现（其历史用量仍在 Dashboard.ByModel 里）。
func (s *SqlStore) DashboardModels(ctx context.Context, from, to time.Time, userID *int64, sortKey string, limit int) (ModelPlaza, error) {
	sortKey = NormalizeModelSort(sortKey)
	if limit <= 0 {
		limit = 50
	}
	if limit > MaxModelLimit {
		limit = MaxModelLimit
	}
	out := ModelPlaza{Sort: sortKey, Limit: limit, List: make([]DashModelStat, 0, limit)}

	rows, err := s.db.QueryContext(ctx,
		`WITH agg AS (
		    SELECT external_model_name AS model,
		           count(*) AS calls,
		           COALESCE(sum((status='failed')::int),0) AS failed,
		           COALESCE(sum(credits_consumed),0) AS credits,
		           COALESCE(sum(`+tokenSum("")+`),0) AS tokens,
		           COALESCE(count(*) FILTER (WHERE `+busyFilter+`),0) AS busy_calls,
		           COALESCE(sum(`+tokenSum("")+`) FILTER (WHERE `+busyFilter+`),0) AS busy_tokens,
		           COALESCE(sum(duration_ms) FILTER (WHERE `+busyFilter+`),0) AS busy_ms,
		           COALESCE(avg(duration_ms) FILTER (WHERE duration_ms IS NOT NULL),0) AS avg_ms,
		           COALESCE(avg(first_token_ms) FILTER (WHERE first_token_ms IS NOT NULL),0) AS avg_first
		      FROM billing_records `+dashWhere("")+`
		     GROUP BY 1
		 )
		 SELECT em.external_name, em.enabled,
		        COALESCE(a.calls,0), COALESCE(a.failed,0), COALESCE(a.credits,0), COALESCE(a.tokens,0),
		        COALESCE(a.busy_calls,0), COALESCE(a.busy_tokens,0), COALESCE(a.busy_ms,0),
		        COALESCE(a.avg_ms,0), COALESCE(a.avg_first,0)
		   FROM external_models em
		   LEFT JOIN agg a ON a.model = em.external_name
		  WHERE em.deleted_at IS NULL
		  ORDER BY (COALESCE(a.calls,0) > 0) DESC, `+modelSortExpr(sortKey)+` DESC, em.external_name ASC
		  LIMIT $4`, dashArgs(userID, from, to, limit)...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var it DashModelStat
		var busyTokens int64
		if err := rows.Scan(&it.Model, &it.Enabled, &it.Calls, &it.Failed, &it.Credits, &it.Tokens,
			&it.BusyCalls, &busyTokens, &it.BusyMS, &it.AvgMS, &it.AvgFirstMS); err != nil {
			return out, err
		}
		it.Label = it.Model
		it.Active = it.Calls > 0
		if it.Calls > 0 {
			it.FailRate = float64(it.Failed) / float64(it.Calls)
		}
		it.TPM = BusyRate(float64(busyTokens), it.BusyMS)
		out.List = append(out.List, it)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	// 目录总数 / 窗口内活跃模型数：与 List 分离统计，避免为拿计数而全量取行。
	if err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM external_models WHERE deleted_at IS NULL),
		        (SELECT count(DISTINCT external_model_name) FROM billing_records `+dashWhere("")+`)`,
		dashArgs(userID, from, to)...).Scan(&out.Total, &out.Active); err != nil {
		return out, err
	}
	out.Truncated = out.Total > len(out.List)
	return out, nil
}
