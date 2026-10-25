// live.go 统计页「实时区」：请求中会话（我的会话）与在线用户。
//
// 安全边界（**字段层面**收口，不靠前端自觉）：
//   - 本文件只输出「运行态计数与聚合值」：会话 ID/名称、模型名、上下文长度、本轮 Token、
//     吞吐、积分、采样点。DTO 里没有任何承载会话内容的字段（消息、提示词、模型输入输出、
//     原始 x-session-id 一律不出现），因此前端即使原样渲染也无从泄露内容。
//   - 在线用户只给本人/该用户的汇总吞吐与消耗，不含任何会话级字段。
//
// 数据来源：
//   - 「请求中」以 router.SessionRegistry 内存计数为准（Inflight > 0，由网关在包住上游转发时
//     +1/-1），DB 的 sessions 表是投影、不含该运行时字段，不能用来判「正在请求」。
//   - Token / 积分 / 上下文来自 billing_records（一次请求完成才落一行），
//     所以「本轮 Token」= 最近一次**已完成**请求的量，进行中的这一轮要等它落账才计入。
//
// 三个吞吐口径不混用（与模型广场、首屏 KPI 各自独立，详见接口文档）：
// 本文件的 TPM = 近 Window 分钟（默认 60）窗口内的 Token ÷ 窗口墙钟分钟数。
package console

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/decimalx"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// 实时区参数默认值与上限。
const (
	liveDefaultWindowMinutes = 60   // TPM / 消耗的统计窗口（分钟），对齐「近 60 分钟」口径
	liveMaxWindowMinutes     = 1440 // 最长 24 小时
	liveDefaultSparkPoints   = 12   // 曲线采样点数
	liveMaxSparkPoints       = 60
	liveDefaultLimit         = 20 // 单次返回的会话行数
	liveMaxLimit             = 100
	liveMaxSessions          = 500 // 单次参与聚合的「请求中会话」上限（防极端并发拖垮查询）
	liveMaxUsers             = 100 // 单次返回的在线用户上限
	// liveRecentLookback 会话「最近若干次请求」的回溯上限：用于上下文长度 / 本轮 Token / 曲线。
	// 请求中的会话上一轮必然很近，取 7 天是为了兜住长时间空闲后又发起请求的会话，
	// 同时避免按 session_id 扫出整段历史（重会话可达上万行）。
	liveRecentLookback = 7 * 24 * time.Hour
)

// liveParams 实时区查询参数（已收敛到合法范围）。
type liveParams struct {
	WindowMinutes int
	Window        time.Duration
	SparkPoints   int
	Limit         int
}

// parseLiveParams 解析 Window / SparkPoints / Limit。
// 越界值一律收敛到 [1, 上限]（不报错，前端少一次失败）；非数字才报 400。
func parseLiveParams(q url.Values) (liveParams, error) {
	p := liveParams{
		WindowMinutes: liveDefaultWindowMinutes,
		SparkPoints:   liveDefaultSparkPoints,
		Limit:         liveDefaultLimit,
	}
	if v := q.Get("Window"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return p, errors.New("Window 必须为数字（分钟）")
		}
		if n < 1 {
			n = 1
		}
		if n > liveMaxWindowMinutes {
			n = liveMaxWindowMinutes
		}
		p.WindowMinutes = n
	}
	if v := q.Get("SparkPoints"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return p, errors.New("SparkPoints 必须为数字")
		}
		if n < 1 {
			n = 1
		}
		if n > liveMaxSparkPoints {
			n = liveMaxSparkPoints
		}
		p.SparkPoints = n
	}
	if v := q.Get("Limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return p, errors.New("Limit 必须为数字")
		}
		if n <= 0 {
			n = liveDefaultLimit
		}
		if n > liveMaxLimit {
			n = liveMaxLimit
		}
		p.Limit = n
	}
	p.Window = time.Duration(p.WindowMinutes) * time.Minute
	return p, nil
}

// liveSessionItem 「我的会话」一行：请求中的会话及其运行态。
// 刻意不含：消息、提示词、模型输入输出、原始 x-session-id、渠道密钥（前端不需要，也就不给）。
type liveSessionItem struct {
	ID           string  `json:"ID"`           // 会话指纹（不是客户端原始 x-session-id）
	Name         string  `json:"Name"`         // 会话可读名（可能为空，前端用 ID 前缀兜底）
	Model        string  `json:"Model"`        // 对外模型名
	Inflight     int     `json:"Inflight"`     // 进行中请求数（>0 才会出现在列表里）
	ContextUsed  int64   `json:"ContextUsed"`  // 上下文长度：最近一次请求的输入 Token（input+cache_read）
	ContextLimit int64   `json:"ContextLimit"` // 上下文上限；平台未配置模型上限时恒为 0（= 未知）
	RoundTokens  int64   `json:"RoundTokens"`  // 本轮 Token：最近一次已完成请求的总 Token（三段求和）
	Calls        int64   `json:"Calls"`        // 会话累计请求数
	WindowTokens int64   `json:"WindowTokens"` // 近 Window 分钟 Token（TPM 的分子）
	TPM          float64 `json:"TPM"`          // WindowTokens ÷ Window 分钟（墙钟窗口平均）
	SpentCredits float64 `json:"SpentCredits"` // 会话累计消耗积分
	LastActive   string  `json:"LastActive"`   // 最近活跃时间（RFC3339）
	Spark        []int64 `json:"Spark"`        // 最近 SparkPoints 次请求的 Token（时间正序，可能不足）
}

// liveUserItem 「在线用户」一行：只有汇总吞吐与消耗。
type liveUserItem struct {
	ID           string  `json:"ID"`           // 用户 ID（字符串，避免 JS 大整数精度丢失）
	Name         string  `json:"Name"`         // 昵称优先，空则登录名
	Sessions     int     `json:"Sessions"`     // 该用户当前请求中的会话数
	Tokens       int64   `json:"Tokens"`       // 近 Window 分钟 Token（仅统计请求中会话）
	TPM          float64 `json:"TPM"`          // Tokens ÷ Window 分钟（墙钟窗口平均）
	SpentCredits float64 `json:"SpentCredits"` // 近 Window 分钟消耗积分
	Spark        []int64 `json:"Spark"`        // 近 SparkPoints 分钟逐分钟 Token（时间正序，缺分钟补 0）
}

// liveResp 实时区响应（admin/dev 同构，Scope 区分）。
type liveResp struct {
	Scope          string            `json:"Scope"`          // global | self
	SampledAt      string            `json:"SampledAt"`      // 采样时刻 RFC3339
	WindowMinutes  int               `json:"WindowMinutes"`  // TPM / 消耗的统计窗口
	SparkPoints    int               `json:"SparkPoints"`    // 曲线采样点数
	SessionsTotal  int               `json:"SessionsTotal"`  // 请求中会话总数（未截断）
	UsersTotal     int               `json:"UsersTotal"`     // 在线用户总数
	Truncated      bool              `json:"Truncated"`      // 会话列表是否被 Limit 截断
	UsersTruncated bool              `json:"UsersTruncated"` // 用户列表是否被上限截断
	Sessions       []liveSessionItem `json:"Sessions"`
	Users          []liveUserItem    `json:"Users"`
}

// liveSessionAgg 单个会话的聚合结果。
type liveSessionAgg struct {
	Calls        int64
	Credits      float64
	WindowTokens int64
	ContextUsed  int64
	RoundTokens  int64
	Spark        []int64
}

// liveSessionAggregates 按会话聚合：累计请求数 / 累计积分 / 窗口内 Token。
// 一次 SQL 聚合一批会话（避免逐会话查询）。
func liveSessionAggregates(ctx context.Context, db *sql.DB, ids []string, windowStart time.Time) map[string]*liveSessionAgg {
	out := make(map[string]*liveSessionAgg, len(ids))
	if db == nil || len(ids) == 0 {
		return out
	}
	rows, err := db.QueryContext(ctx, `
SELECT session_id,
       count(*),
       COALESCE(sum(credits_consumed),0),
       COALESCE(sum(`+billing.TokenSumExpr("")+`) FILTER (WHERE call_time >= $2),0)
FROM billing_records
WHERE session_id = ANY(string_to_array($1, ','))
GROUP BY session_id`, joinIDs(ids), windowStart)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		var a liveSessionAgg
		if err := rows.Scan(&sid, &a.Calls, &a.Credits, &a.WindowTokens); err != nil {
			continue
		}
		out[sid] = &a
	}
	return out
}

// liveRecentCalls 取每个会话最近 sparkPoints 次请求的 Token，并回填上下文长度 / 本轮 Token。
// 返回的 Spark 按时间正序；会话没有历史请求时不进 map。
func liveRecentCalls(ctx context.Context, db *sql.DB, ids []string, now time.Time, sparkPoints int) map[string]*liveSessionAgg {
	out := make(map[string]*liveSessionAgg, len(ids))
	if db == nil || len(ids) == 0 {
		return out
	}
	rows, err := db.QueryContext(ctx, `
SELECT session_id, rn, tokens_total, ctx_tokens FROM (
    SELECT session_id,
           `+billing.TokenSumExpr("")+` AS tokens_total,
           `+billing.InputTokensExpr("")+` AS ctx_tokens,
           row_number() OVER (PARTITION BY session_id ORDER BY call_time DESC, id DESC) AS rn
    FROM billing_records
    WHERE session_id = ANY(string_to_array($1, ',')) AND call_time >= $3
) t
WHERE rn <= $2
ORDER BY session_id, rn DESC`, joinIDs(ids), sparkPoints, now.Add(-liveRecentLookback))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		var rn int
		var tokens, ctx int64
		if err := rows.Scan(&sid, &rn, &tokens, &ctx); err != nil {
			continue
		}
		a := out[sid]
		if a == nil {
			a = &liveSessionAgg{Spark: make([]int64, 0, sparkPoints)}
			out[sid] = a
		}
		a.Spark = append(a.Spark, tokens)
		if rn == 1 {
			a.ContextUsed = ctx
			a.RoundTokens = tokens
		}
	}
	return out
}

// liveUserAggregates 按用户聚合窗口内 Token / 积分，范围限定在给定会话集合内
// （只算「请求中」的那部分会话，用户其它闲置会话不计入在线吞吐）。
func liveUserAggregates(ctx context.Context, db *sql.DB, userIDs []int64, sessionIDs []string, windowStart time.Time) map[int64]*liveUserItem {
	out := make(map[int64]*liveUserItem, len(userIDs))
	if db == nil || len(userIDs) == 0 || len(sessionIDs) == 0 {
		return out
	}
	rows, err := db.QueryContext(ctx, `
SELECT user_id,
       COALESCE(sum(`+billing.TokenSumExpr("")+`) FILTER (WHERE call_time >= $3),0),
       COALESCE(sum(credits_consumed) FILTER (WHERE call_time >= $3),0)
FROM billing_records
WHERE user_id = ANY(string_to_array($1, ',')::bigint[])
  AND session_id = ANY(string_to_array($2, ','))
GROUP BY user_id`, joinInt64IDs(userIDs), joinIDs(sessionIDs), windowStart)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var uid int64
		var item liveUserItem
		if err := rows.Scan(&uid, &item.Tokens, &item.SpentCredits); err != nil {
			continue
		}
		item.ID = strconv.FormatInt(uid, 10)
		row := item
		out[uid] = &row
	}
	return out
}

// liveUserSpark 逐分钟 Token（近 sparkPoints 分钟，按北京时间分钟界），返回 sutffed 到固定长度的序列。
// 无请求的分钟补 0：这是采样曲线，缺分钟即「该分钟没用量」，与趋势图「缺数据断线」语义不同。
func liveUserSpark(ctx context.Context, db *sql.DB, userIDs []int64, sessionIDs []string, now time.Time, sparkPoints int) map[int64][]int64 {
	out := make(map[int64][]int64, len(userIDs))
	if db == nil || len(userIDs) == 0 || len(sessionIDs) == 0 {
		return out
	}
	loc := cnLocation()
	end := now.In(loc).Truncate(time.Minute)
	start := end.Add(time.Duration(-(sparkPoints - 1)) * time.Minute)
	keys := make([]string, 0, sparkPoints)
	for i := 0; i < sparkPoints; i++ {
		keys = append(keys, start.Add(time.Duration(i)*time.Minute).Format("2006-01-02T15:04"))
	}
	rows, err := db.QueryContext(ctx, `
SELECT user_id,
       date_trunc('minute', call_time AT TIME ZONE 'Asia/Shanghai'),
       COALESCE(sum(`+billing.TokenSumExpr("")+`),0)
FROM billing_records
WHERE user_id = ANY(string_to_array($1, ',')::bigint[])
  AND session_id = ANY(string_to_array($2, ','))
  AND call_time >= $3
GROUP BY 1,2 ORDER BY 1,2`, joinInt64IDs(userIDs), joinIDs(sessionIDs), start)
	if err != nil {
		return out
	}
	defer rows.Close()
	buckets := make(map[int64]map[string]int64, len(userIDs))
	for rows.Next() {
		var uid int64
		var minute time.Time
		var tokens int64
		if err := rows.Scan(&uid, &minute, &tokens); err != nil {
			continue
		}
		m := buckets[uid]
		if m == nil {
			m = make(map[string]int64, sparkPoints)
			buckets[uid] = m
		}
		m[minute.Format("2006-01-02T15:04")] = tokens
	}
	for _, uid := range userIDs {
		series := make([]int64, sparkPoints)
		if m := buckets[uid]; m != nil {
			for i, k := range keys {
				series[i] = m[k]
			}
		}
		out[uid] = series
	}
	return out
}

// liveUserNames 批量取用户展示名（昵称优先，空则登录名）；失败返回空 map（不阻断实时区）。
func liveUserNames(ctx context.Context, db *sql.DB, ids []int64) map[int64]string {
	out := make(map[int64]string, len(ids))
	if db == nil || len(ids) == 0 {
		return out
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, username, nickname FROM users WHERE id = ANY(string_to_array($1, ',')::bigint[])`, joinInt64IDs(ids))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var username, nickname string
		if err := rows.Scan(&id, &username, &nickname); err != nil {
			continue
		}
		if nickname != "" {
			out[id] = nickname
		} else {
			out[id] = username
		}
	}
	return out
}

// buildLiveStats 组装实时区数据。
//   - reg 为 nil（未装配会话注册表）→ 返回空集合，不报错；
//   - db 为 nil（未装配 DB）→ 会话列表仍返回（含 Inflight），数值列为 0；
//   - userID 非 nil 时只统计该用户（开发端「我的会话」/「我」）。
func buildLiveStats(ctx context.Context, db *sql.DB, reg *router.SessionRegistry, now time.Time,
	userID *int64, scope string, p liveParams) (liveResp, error) {
	out := liveResp{
		Scope:         scope,
		SampledAt:     now.Format(time.RFC3339),
		WindowMinutes: p.WindowMinutes,
		SparkPoints:   p.SparkPoints,
		Sessions:      []liveSessionItem{},
		Users:         []liveUserItem{},
	}
	if reg == nil {
		return out, nil
	}

	// 1) 请求中的会话（内存计数为准），最近活跃优先。
	inflight := make([]*router.Session, 0, 16)
	for _, s := range reg.ListAll() {
		if s == nil || s.Closed || s.Inflight <= 0 {
			continue
		}
		if userID != nil && s.UserID != *userID {
			continue
		}
		inflight = append(inflight, s)
	}
	sort.Slice(inflight, func(i, j int) bool {
		if !inflight[i].LastActive.Equal(inflight[j].LastActive) {
			return inflight[i].LastActive.After(inflight[j].LastActive)
		}
		return inflight[i].SessionID < inflight[j].SessionID
	})
	out.SessionsTotal = len(inflight)
	if len(inflight) > liveMaxSessions {
		inflight = inflight[:liveMaxSessions]
	}
	if len(inflight) == 0 {
		return out, nil
	}

	page := inflight
	if len(page) > p.Limit {
		page = page[:p.Limit]
	}
	out.Truncated = out.SessionsTotal > len(page)

	// 2) 聚合（全量会话，避免展示上限把「在线用户」算偏）。
	ids := make([]string, 0, len(inflight))
	userIDs := make([]int64, 0, 8)
	seenUser := make(map[int64]int, 8)
	for _, s := range inflight {
		if s.SessionID != "" {
			ids = append(ids, s.SessionID)
		}
		if _, ok := seenUser[s.UserID]; !ok {
			seenUser[s.UserID] = 0
			userIDs = append(userIDs, s.UserID)
		}
		seenUser[s.UserID]++
	}
	windowStart := now.Add(-p.Window)
	aggs := liveSessionAggregates(ctx, db, ids, windowStart)
	recent := liveRecentCalls(ctx, db, ids, now, p.SparkPoints)
	userAggs := liveUserAggregates(ctx, db, userIDs, ids, windowStart)
	userSparks := liveUserSpark(ctx, db, userIDs, ids, now, p.SparkPoints)
	names := liveUserNames(ctx, db, userIDs)

	windowMinutes := p.WindowMinutes
	for _, s := range page {
		item := liveSessionItem{
			ID:         s.SessionID,
			Name:       s.Name,
			Model:      s.Model,
			Inflight:   s.Inflight,
			LastActive: s.LastActive.Format(time.RFC3339),
			Spark:      []int64{},
		}
		if a := aggs[s.SessionID]; a != nil {
			item.Calls = a.Calls
			item.WindowTokens = a.WindowTokens
			item.SpentCredits = decimalx.Round5(a.Credits)
		}
		if r := recent[s.SessionID]; r != nil {
			item.ContextUsed = r.ContextUsed
			item.RoundTokens = r.RoundTokens
			if len(r.Spark) > 0 {
				item.Spark = r.Spark
			}
		}
		item.TPM = decimalx.Round2(ratePerMinute(float64(item.WindowTokens), windowMinutes))
		out.Sessions = append(out.Sessions, item)
	}

	// 3) 在线用户汇总（只给吞吐与消耗）。
	out.UsersTotal = len(userIDs)
	out.UsersTruncated = len(userIDs) > liveMaxUsers
	users := make([]liveUserItem, 0, len(userIDs))
	for _, uid := range userIDs {
		item := liveUserItem{
			ID:       strconv.FormatInt(uid, 10),
			Name:     names[uid],
			Sessions: seenUser[uid],
			Spark:    []int64{},
		}
		if a := userAggs[uid]; a != nil {
			item.Tokens = a.Tokens
			item.SpentCredits = decimalx.Round5(a.SpentCredits)
		}
		item.TPM = decimalx.Round2(ratePerMinute(float64(item.Tokens), windowMinutes))
		if sp := userSparks[uid]; len(sp) > 0 {
			item.Spark = sp
		}
		users = append(users, item)
	}
	// 吞吐高的排前面，同值按 ID 升序（稳定，避免刷新时行序抖动）。
	sort.Slice(users, func(i, j int) bool {
		if users[i].TPM != users[j].TPM {
			return users[i].TPM > users[j].TPM
		}
		return users[i].ID < users[j].ID
	})
	if len(users) > liveMaxUsers {
		users = users[:liveMaxUsers]
	}
	out.Users = users
	return out, nil
}

// ratePerMinute 窗口速率：总量 ÷ 窗口分钟数（墙钟平均）。窗口 ≤0 时返回 0。
func ratePerMinute(total float64, minutes int) float64 {
	if minutes <= 0 {
		return 0
	}
	return total / float64(minutes)
}

// joinIDs / joinInt64IDs 把 ID 列表拼成逗号串，配合 SQL 里的 string_to_array($n, ',') 使用。
// 这样是**单参数绑定**（不拼 SQL，无注入面），同时避免把 []string 直接当驱动参数
// （部分驱动/sqlmock 不支持切片参数）。会话 ID 是十六进制指纹，不含逗号。
func joinIDs(ids []string) string { return strings.Join(ids, ",") }

func joinInt64IDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

// HandleStatsLive GET /api/v1/admin/stats/live?Window=&SparkPoints=&Limit= 管理端实时区（全局）。
func (h *Admin) HandleStatsLive(w http.ResponseWriter, r *http.Request) {
	p, err := parseLiveParams(r.URL.Query())
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	out, err := buildLiveStats(r.Context(), h.db, h.sessions, h.now(), nil, "global", p)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询实时会话失败")
		return
	}
	resp.OK(w, r, out)
}

// HandleStatsLive GET /api/v1/dev/stats/live?Window=&SparkPoints=&Limit= 开发端实时区（仅本人）。
func (h *Dev) HandleStatsLive(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	p, err := parseLiveParams(r.URL.Query())
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	out, err := buildLiveStats(r.Context(), h.db, h.sessions, h.now(), &userID, "self", p)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询实时会话失败")
		return
	}
	resp.OK(w, r, out)
}
