// live.go 统计页「实时区」：我的会话（**仅请求中**）与在线用户（所选范围内有请求）。
//
// 会话准入（spencer 拍板）：列表准入恒为 `Inflight > 0`。请求刚结束的会话只进
// `RecentlyExited` 供前端做淡出过渡，**不进主列表、不进任何计数与聚合**。
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
//   - 「刚结束请求」同样以注册表为准（LastActive，网关每次请求续期）：空闲会话仍留在注册表里
//     直到 TTL 过期，这里用它填 `RecentlyExited`（淡出窗口 liveFadeWindow）——**仅展示语义**，
//     不改变准入，也不参与任何计数。
//   - 「在线用户」改为查 DB（billing_records）：所选范围（页面右上角 24h/7天/30天/90天/自定义）
//     内有过请求的用户全集，`UsersTotal` 与 `Users` 列表**同一查询同源**（总数=未截断行数）。
//     只看注册表会漏掉「范围内活跃但此刻空闲」的用户（v4 口径修正）。
//   - Token / 积分 / 上下文来自 billing_records（一次请求完成才落一行），
//     所以「本轮 Token」= 最近一次**已完成**请求的量，进行中的这一轮要等它落账才计入；
//     同理「在线用户」里也暂不包含「首次请求仍在进行中、尚未落账」的用户。
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

	// liveFadeWindow 淡出窗口：请求结束后会话仍在注册表里保留一会儿，前端可据此做「淡出再消失」的
	// 视觉过渡。**它只用于 `RecentlyExited` 这一展示数组，绝不是列表准入条件**——
	// 「我的会话」准入恒为 `Inflight > 0`（spencer 拍板；口径与 main 一致）。
	liveFadeWindow = 5 * time.Minute
	// liveMaxRecentlyExited 淡出数组的行数上限：纯展示用途，不参与任何计数与聚合。
	liveMaxRecentlyExited = 20
	// liveDefaultRange 在线用户默认统计范围：对齐页面右上角默认预设「近 24h」。
	liveDefaultRange = 24 * time.Hour
	// liveMaxRangeSpan 在线用户统计范围上限（与 dashboard 一致，366 天）。
	liveMaxRangeSpan = 366 * 24 * time.Hour
	// liveMaxDailyPoints 每用户「日活跃序列」的最大点数（对齐 90 天预设 + 余量）。
	// 自定义超长区间只回最近这么多天（`DailyFrom` 会如实回报序列起点），避免 30s 刷新扛大包。
	liveMaxDailyPoints = 92
)

// liveParams 实时区查询参数（已收敛到合法范围）。
type liveParams struct {
	WindowMinutes int
	Window        time.Duration
	SparkPoints   int
	Limit         int
	// From/To 在线用户统计范围（页面右上角时间范围），默认近 24h。
	// 与 Window 刻意分开：Window 是 TPM/消耗的滑动窗口，From/To 是「哪些用户算在线」的筛选范围。
	From time.Time
	To   time.Time
}

// parseLiveParams 解析 Window / SparkPoints / Limit / From / To。
// 越界值一律收敛到 [1, 上限]（不报错，前端少一次失败）；非数字/非法时间才报 400。
// From/To 缺省 = 近 24h（对齐页面默认预设），跨度上限 366 天（与 dashboard 同口径）。
func parseLiveParams(q url.Values, now time.Time) (liveParams, error) {
	p := liveParams{
		WindowMinutes: liveDefaultWindowMinutes,
		SparkPoints:   liveDefaultSparkPoints,
		Limit:         liveDefaultLimit,
		To:            now,
		From:          now.Add(-liveDefaultRange),
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
	if v := q.Get("To"); v != "" {
		t, err := parseRFC3339Lenient(v)
		if err != nil {
			return p, errors.New("To 时间格式应为 RFC3339")
		}
		p.To = t
	}
	if v := q.Get("From"); v != "" {
		t, err := parseRFC3339Lenient(v)
		if err != nil {
			return p, errors.New("From 时间格式应为 RFC3339")
		}
		p.From = t
	}
	if p.From.After(p.To) {
		return p, errors.New("开始时间不能晚于结束时间")
	}
	if p.To.Sub(p.From) > liveMaxRangeSpan {
		return p, errors.New("时间跨度不能超过 366 天")
	}
	p.Window = time.Duration(p.WindowMinutes) * time.Minute
	return p, nil
}

// liveSessionItem 「我的会话」一行：**仅请求中**（`Inflight > 0`）的会话及其运行态。
// 刻意不含：消息、提示词、模型输入输出、原始 x-session-id、渠道密钥（前端不需要，也就不给）。
type liveSessionItem struct {
	ID           string  `json:"ID"`           // 会话指纹（不是客户端原始 x-session-id）
	Name         string  `json:"Name"`         // 会话可读名（可能为空，前端用 ID 前缀兜底）
	Model        string  `json:"Model"`        // 对外模型名
	Inflight     int     `json:"Inflight"`     // 进行中请求数（进列表即 > 0）
	ContextUsed  int64   `json:"ContextUsed"`  // 上下文长度：最近一次请求的输入 Token（input+cache_read）
	ContextLimit int64   `json:"ContextLimit"` // 上下文上限；平台未配置模型上限时恒为 0（= 未知）
	RoundTokens  int64   `json:"RoundTokens"`  // 本轮 Token：最近一次已完成请求的总 Token（三段求和）
	Calls        int64   `json:"Calls"`        // 会话累计请求数
	TotalTokens  int64   `json:"TotalTokens"`  // 会话累计 Token（三段求和，用于卡片「累计」）
	WindowTokens int64   `json:"WindowTokens"` // 近 Window 分钟 Token（TPM 的分子）
	TPM          float64 `json:"TPM"`          // WindowTokens ÷ Window 分钟（墙钟窗口平均）
	SpentCredits float64 `json:"SpentCredits"` // 会话累计消耗积分
	CreatedAt    string  `json:"CreatedAt"`    // 会话创建时间（RFC3339；注册表未记录时为空串）
	LastActive   string  `json:"LastActive"`   // 最近活跃时间（RFC3339）
	Spark        []int64 `json:"Spark"`        // 最近 SparkPoints 次请求的 Token（时间正序，可能不足）
}

// liveUserItem 「在线用户」一行：只有汇总吞吐与消耗。
//
// **旧字段语义与 origin/main 完全一致**（spencer 拍板保持向后兼容）：
//   - 行集 = 仅由「请求中会话」涉及的用户（没有请求中会话的用户不出现在这里）；
//   - Tokens / SpentCredits / Spark 只按**请求中会话**聚合，不含该用户其它空闲会话。
//
// 范围内活跃用户（另一套口径）放在新字段 `RangeUsersTotal` / `RangeUsers[]`，两者不要混用。
type liveUserItem struct {
	ID           string  `json:"ID"`           // 用户 ID（字符串，避免 JS 大整数精度丢失）
	Name         string  `json:"Name"`         // 昵称优先，空则登录名
	Sessions     int     `json:"Sessions"`     // 该用户当前请求中的会话数
	Tokens       int64   `json:"Tokens"`       // 近 Window 分钟 Token（仅统计请求中会话）
	TPM          float64 `json:"TPM"`          // Tokens ÷ Window 分钟（墙钟窗口平均）
	SpentCredits float64 `json:"SpentCredits"` // 近 Window 分钟消耗积分（仅请求中会话）
	Spark        []int64 `json:"Spark"`        // 近 SparkPoints 分钟逐分钟 Token（时间正序，缺分钟补 0）
}

// liveRangeUserItem 「范围内活跃用户」一行（新口径，单独字段，**不参与**旧字段语义）。
//
// 行集 = 所选范围 [From, To] 内有过请求的用户全集（DB 为准），包含「此刻没有请求中会话」的用户。
// 字段刻意加 Range 前缀，与 `Users[]` 的窗口口径区分开，避免前端把两套数混着用。
type liveRangeUserItem struct {
	ID   string `json:"ID"`   // 用户 ID（字符串，避免 JS 大整数精度丢失）
	Name string `json:"Name"` // 昵称优先，空则登录名
	// RangeSessions / RangeCalls / RangeTokens / RangeCredits 都是**所选范围内**的合计。
	RangeSessions int     `json:"RangeSessions"` // 范围内有过请求的会话数（去重）
	RangeCalls    int64   `json:"RangeCalls"`    // 范围内请求数
	RangeTokens   int64   `json:"RangeTokens"`   // 范围内 Token 合计（三段求和）
	RangeCredits  float64 `json:"RangeCredits"`  // 范围内消耗积分合计
	// WindowTokens / SpentCredits 是该用户**近 Window 分钟**的用量（覆盖其全部会话，与旧字段的
	// 「仅请求中会话」不同源，因此不放进 Users[]）。
	WindowTokens int64   `json:"WindowTokens"`
	SpentCredits float64 `json:"SpentCredits"`
	TPM          float64 `json:"TPM"`   // WindowTokens ÷ Window 分钟（墙钟窗口平均）
	Spark        []int64 `json:"Spark"` // 近 SparkPoints 分钟逐分钟 Token（时间正序，缺分钟补 0）
	Daily        []int64 `json:"Daily"` // 日活跃序列：DailyFrom~DailyTo 逐日 Token（无请求日补 0）
}

// liveExitedItem 刚刚结束请求、仍处于淡出窗口（liveFadeWindow）的会话。
//
// **纯展示字段，不是准入条件**：它只供前端把已经消失的卡片做「淡出再移除」的过渡。
// 这一数组不参与 SessionsTotal / UsersTotal / Truncated / 用户聚合 / 任何金额与 Token 求和，
// 也就是说「实时区有多少会话/多少用户」永远不会被淡出项抬高。
type liveExitedItem struct {
	ID       string `json:"ID"`       // 会话指纹
	Name     string `json:"Name"`     // 会话可读名（可能为空）
	Model    string `json:"Model"`    // 对外模型名
	ExitedAt string `json:"ExitedAt"` // 淡出起点（= 该会话最近一次活跃/请求结束时刻，RFC3339）
}

// liveResp 实时区响应（admin/dev 同构，Scope 区分）。
type liveResp struct {
	Scope         string `json:"Scope"`         // global | self
	SampledAt     string `json:"SampledAt"`     // 采样时刻 RFC3339
	WindowMinutes int    `json:"WindowMinutes"` // TPM / 消耗的统计窗口
	SparkPoints   int    `json:"SparkPoints"`   // 曲线采样点数
	// RangeFrom/RangeTo 在线用户的筛选范围（= 页面右上角时间范围），回显实际生效值。
	RangeFrom string `json:"RangeFrom"`
	RangeTo   string `json:"RangeTo"`
	// DailyFrom/DailyTo/DailyPoints 每用户日活跃序列的实际覆盖（超长区间只回最近 DailyPoints 天）。
	DailyFrom   string `json:"DailyFrom"`
	DailyTo     string `json:"DailyTo"`
	DailyPoints int    `json:"DailyPoints"`
	// SessionsTotal 我的会话总数（未截断）= **仅请求中**（Inflight > 0）。
	SessionsTotal int `json:"SessionsTotal"`
	// UsersTotal 在线用户总数 = **有请求中会话的用户数**（与 Users 行集同源；旧字段，语义不变）。
	UsersTotal int `json:"UsersTotal"`
	// RangeUsersTotal 所选范围内有过请求的用户数（新口径，与 RangeUsers 同源、未截断）。
	RangeUsersTotal int               `json:"RangeUsersTotal"`
	Truncated       bool              `json:"Truncated"`      // 会话列表是否被 Limit 截断
	UsersTruncated  bool              `json:"UsersTruncated"` // 用户列表是否被上限截断
	Sessions        []liveSessionItem `json:"Sessions"`
	Users           []liveUserItem    `json:"Users"`
	// RangeUsers 范围内活跃用户（新口径）：包含此刻没有请求中会话的用户。
	RangeUsers []liveRangeUserItem `json:"RangeUsers"`
	// RecentlyExited 淡出窗口内已结束请求的会话（**纯展示，不参与任何计数/聚合**）。
	// 前端用法：收到数组后把对应卡片标记为「淡出」，`ExitedAt + FadeWindowSeconds` 之后移除节点；
	// 它们从来不在 Sessions 里，所以不要用它去算会话数或用户数。
	RecentlyExited    []liveExitedItem `json:"RecentlyExited"`
	FadeWindowSeconds int              `json:"FadeWindowSeconds"`
}

// liveSessionAgg 单个会话的聚合结果。
type liveSessionAgg struct {
	Calls        int64
	Credits      float64
	TotalTokens  int64
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
       COALESCE(sum(`+billing.TokenSumExpr("")+`),0),
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
		if err := rows.Scan(&sid, &a.Calls, &a.Credits, &a.TotalTokens, &a.WindowTokens); err != nil {
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

// liveUserAggregates 按用户聚合窗口内 Token / 积分，范围限定在给定**请求中会话**集合内
// （旧字段口径：只算「请求中」的那部分会话，用户其它闲置会话不计入在线吞吐）。
//
// 返回值与 origin/main 逐字段一致（spencer 拍板保持向后兼容）；范围内活跃用户另走
// liveRangeUserAggregates。
func liveUserAggregates(ctx context.Context, db *sql.DB, userIDs []int64, sessionIDs []string,
	windowStart time.Time) map[int64]*liveUserItem {
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

// liveUserSpark 逐分钟 Token（近 sparkPoints 分钟，按北京时间分钟界），范围限定在**请求中会话**集合内。
// 无请求的分钟补 0：这是采样曲线，缺分钟即「该分钟没用量」，与趋势图「缺数据断线」语义不同。
func liveUserSpark(ctx context.Context, db *sql.DB, userIDs []int64, sessionIDs []string,
	now time.Time, sparkPoints int) map[int64][]int64 {
	out := make(map[int64][]int64, len(userIDs))
	if db == nil || len(userIDs) == 0 || len(sessionIDs) == 0 {
		return out
	}
	loc := cnLocation()
	end := now.In(loc).Truncate(time.Minute)
	start := end.Add(time.Duration(-(sparkPoints - 1)) * time.Minute)
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
		if buckets[uid] == nil {
			buckets[uid] = make(map[string]int64, sparkPoints)
		}
		buckets[uid][minute.In(loc).Format("2006-01-02T15:04")] = tokens
	}
	if err := rows.Err(); err != nil {
		return out
	}
	// 与 origin/main 一致：**每个**用户都回一个补零到 sparkPoints 的定长序列（没有数据的用户是全 0）。
	for _, uid := range userIDs {
		series := make([]int64, sparkPoints)
		if b := buckets[uid]; b != nil {
			for i := 0; i < sparkPoints; i++ {
				series[i] = b[start.Add(time.Duration(i)*time.Minute).Format("2006-01-02T15:04")]
			}
		}
		out[uid] = series
	}
	return out
}

// liveRangeUserAggregates 按用户聚合所选范围（p.From~p.To）内的用量，并顺带给出窗口用量。
// **返回的行就是「范围内活跃用户」全集**（`RangeUsersTotal` = 未截断行数，列表取前 liveMaxUsers 行）。
//   - onlyUser 非 nil（开发端）→ 只统计本人；
//   - 范围外无请求的用户不出行；
//   - 窗口量（WindowTokens / SpentCredits）覆盖该用户**全部会话**，与旧字段的「仅请求中会话」不同源。
func liveRangeUserAggregates(ctx context.Context, db *sql.DB, from, to, windowStart time.Time,
	onlyUser *int64) ([]int64, map[int64]*liveRangeUserItem) {
	out := make(map[int64]*liveRangeUserItem, 16)
	if db == nil {
		return nil, out
	}
	var u any
	if onlyUser != nil {
		u = *onlyUser
	}
	rows, err := db.QueryContext(ctx, `
SELECT user_id,
       count(*),
       count(DISTINCT session_id) FILTER (WHERE session_id IS NOT NULL AND session_id <> ''),
       COALESCE(sum(`+billing.TokenSumExpr("")+`) FILTER (WHERE call_time >= $3),0),
       COALESCE(sum(credits_consumed) FILTER (WHERE call_time >= $3),0),
       COALESCE(sum(`+billing.TokenSumExpr("")+`),0),
       COALESCE(sum(credits_consumed),0)
FROM billing_records
WHERE call_time >= $2 AND call_time <= $4
  AND ($1::bigint IS NULL OR user_id = $1)
GROUP BY user_id
ORDER BY 6 DESC, 1`, u, from, windowStart, to)
	if err != nil {
		return nil, out
	}
	defer rows.Close()
	ids := make([]int64, 0, 16)
	for rows.Next() {
		var uid int64
		var item liveRangeUserItem
		if err := rows.Scan(&uid, &item.RangeCalls, &item.RangeSessions,
			&item.WindowTokens, &item.SpentCredits, &item.RangeTokens, &item.RangeCredits); err != nil {
			continue
		}
		item.ID = strconv.FormatInt(uid, 10)
		item.SpentCredits = decimalx.Round5(item.SpentCredits)
		item.RangeCredits = decimalx.Round5(item.RangeCredits)
		row := item
		out[uid] = &row
		ids = append(ids, uid)
	}
	return ids, out
}

// liveRangeUserSpark 逐分钟 Token（近 sparkPoints 分钟），范围是该用户**全部会话**（范围内口径）。
func liveRangeUserSpark(ctx context.Context, db *sql.DB, userIDs []int64, now time.Time,
	sparkPoints int) map[int64][]int64 {
	out := make(map[int64][]int64, len(userIDs))
	if db == nil || len(userIDs) == 0 {
		return out
	}
	loc := cnLocation()
	end := now.In(loc).Truncate(time.Minute)
	start := end.Add(time.Duration(-(sparkPoints - 1)) * time.Minute)
	rows, err := db.QueryContext(ctx, `
SELECT user_id,
       date_trunc('minute', call_time AT TIME ZONE 'Asia/Shanghai'),
       COALESCE(sum(`+billing.TokenSumExpr("")+`),0)
FROM billing_records
WHERE user_id = ANY(string_to_array($1, ',')::bigint[])
  AND call_time >= $2
GROUP BY 1,2 ORDER BY 1,2`, joinInt64IDs(userIDs), start)
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
		if buckets[uid] == nil {
			buckets[uid] = make(map[string]int64, sparkPoints)
		}
		buckets[uid][minute.In(loc).Format("2006-01-02T15:04")] = tokens
	}
	for uid, b := range buckets {
		series := make([]int64, 0, sparkPoints)
		for i := 0; i < sparkPoints; i++ {
			series = append(series, b[start.Add(time.Duration(i)*time.Minute).Format("2006-01-02T15:04")])
		}
		out[uid] = series
	}
	return out
}

// liveDailyKeys 生成日活跃序列的日期键（北京时间日界，正序）；超长区间只保留最近 liveMaxDailyPoints 天。
func liveDailyKeys(from, to time.Time) []string {
	loc := cnLocation()
	fromCN, toCN := from.In(loc), to.In(loc)
	start := time.Date(fromCN.Year(), fromCN.Month(), fromCN.Day(), 0, 0, 0, 0, loc)
	end := time.Date(toCN.Year(), toCN.Month(), toCN.Day(), 0, 0, 0, 0, loc)
	if end.Before(start) {
		return []string{}
	}
	if days := int(end.Sub(start).Hours()/24) + 1; days > liveMaxDailyPoints {
		start = end.AddDate(0, 0, -(liveMaxDailyPoints - 1))
	}
	keys := make([]string, 0, liveMaxDailyPoints)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		keys = append(keys, d.Format("2006-01-02"))
	}
	return keys
}

func liveUserDaily(ctx context.Context, db *sql.DB, userIDs []int64, dayKeys []string, from, to time.Time) map[int64][]int64 {
	out := make(map[int64][]int64, len(userIDs))
	if db == nil || len(userIDs) == 0 || len(dayKeys) == 0 {
		return out
	}
	loc := cnLocation()
	start := mustParseDay(dayKeys[0]).In(loc)
	if from.After(start) {
		start = from
	}
	rows, err := db.QueryContext(ctx, `
SELECT user_id,
       date_trunc('day', call_time AT TIME ZONE 'Asia/Shanghai')::date,
       COALESCE(sum(`+billing.TokenSumExpr("")+`),0)
FROM billing_records
WHERE user_id = ANY(string_to_array($1, ',')::bigint[])
  AND call_time >= $2 AND call_time <= $3
GROUP BY 1,2 ORDER BY 1,2`, joinInt64IDs(userIDs), start, to)
	if err != nil {
		return out
	}
	defer rows.Close()
	buckets := make(map[int64]map[string]int64, len(userIDs))
	for rows.Next() {
		var uid int64
		var day time.Time
		var tokens int64
		if err := rows.Scan(&uid, &day, &tokens); err != nil {
			continue
		}
		m := buckets[uid]
		if m == nil {
			m = make(map[string]int64, len(dayKeys))
			buckets[uid] = m
		}
		m[day.In(loc).Format("2006-01-02")] = tokens
	}
	for _, uid := range userIDs {
		series := make([]int64, len(dayKeys))
		if m := buckets[uid]; m != nil {
			for i, k := range dayKeys {
				series[i] = m[k]
			}
		}
		out[uid] = series
	}
	return out
}

// mustParseDay 解析 YYYY-MM-DD（liveDailyKeys 的产物，格式自洽，不会失败）。
func mustParseDay(day string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", day, cnLocation())
	if err != nil {
		return time.Time{}
	}
	return t
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
//   - reg 为 nil（未装配会话注册表）→ **只有「我的会话」为空**，在线用户仍按 DB 口径照常返回
//     （两套口径各自独立取数，不互相降级）；
//   - db 为 nil（未装配 DB）→ 会话列表仍返回（含 Inflight），数值列为 0；在线用户为空；
//   - userID 非 nil 时只统计该用户（开发端「我的会话」/「我」）。
//
// 「我的会话」与「在线用户」是**两套口径**，各自独立取数：
//   - 我的会话：注册表里 未关闭 **且 Inflight > 0**（准入只看请求中，spencer 拍板）；
//     刚结束请求的会话只进 RecentlyExited 做淡出展示，**不进列表、不进任何计数**；
//   - 在线用户：billing_records 里所选范围（p.From~p.To）内有请求的用户全集（DB 为准）。
//     两者不再互相推导——用户没有请求中的会话，只要范围内有请求，也仍然算在线。
func buildLiveStats(ctx context.Context, db *sql.DB, reg *router.SessionRegistry, now time.Time,
	userID *int64, scope string, p liveParams) (liveResp, error) {
	if p.Window <= 0 {
		p.Window = time.Duration(p.WindowMinutes) * time.Minute
	}
	if p.To.IsZero() {
		p.To = now
	}
	if p.From.IsZero() {
		p.From = p.To.Add(-liveDefaultRange)
	}
	dayKeys := liveDailyKeys(p.From, p.To)
	out := liveResp{
		Scope:             scope,
		SampledAt:         now.Format(time.RFC3339),
		WindowMinutes:     p.WindowMinutes,
		SparkPoints:       p.SparkPoints,
		RangeFrom:         p.From.Format(time.RFC3339),
		RangeTo:           p.To.Format(time.RFC3339),
		DailyPoints:       len(dayKeys),
		Sessions:          []liveSessionItem{},
		Users:             []liveUserItem{},
		RecentlyExited:    []liveExitedItem{},
		FadeWindowSeconds: int(liveFadeWindow / time.Second),
	}
	if len(dayKeys) > 0 {
		out.DailyFrom = dayKeys[0]
		out.DailyTo = dayKeys[len(dayKeys)-1]
	}

	// 1) 我的会话：**仅请求中**（Inflight > 0，注册表内存为准），最近活跃优先。
	// 未装配注册表时只跳过这一段，**不**影响下一步的在线用户（DB 口径不需要注册表）。
	var sessions []*router.Session
	if reg != nil {
		sessions = make([]*router.Session, 0, 16)
		fadeSince := now.Add(-liveFadeWindow)
		exited := make([]*router.Session, 0, 8)
		for _, s := range reg.ListAll() {
			if s == nil || s.Closed {
				continue
			}
			if userID != nil && s.UserID != *userID {
				continue
			}
			if s.Inflight > 0 {
				sessions = append(sessions, s)
				continue
			}
			// 已结束请求：只收进淡出数组（且必须落在淡出窗口内），绝不进 sessions。
			if !s.LastActive.Before(fadeSince) {
				exited = append(exited, s)
			}
		}
		sort.Slice(sessions, func(i, j int) bool {
			if !sessions[i].LastActive.Equal(sessions[j].LastActive) {
				return sessions[i].LastActive.After(sessions[j].LastActive)
			}
			return sessions[i].SessionID < sessions[j].SessionID
		})
		sort.Slice(exited, func(i, j int) bool {
			if !exited[i].LastActive.Equal(exited[j].LastActive) {
				return exited[i].LastActive.After(exited[j].LastActive)
			}
			return exited[i].SessionID < exited[j].SessionID
		})
		if len(exited) > liveMaxRecentlyExited {
			exited = exited[:liveMaxRecentlyExited]
		}
		out.RecentlyExited = make([]liveExitedItem, 0, len(exited))
		for _, s := range exited {
			out.RecentlyExited = append(out.RecentlyExited, liveExitedItem{
				ID:       s.SessionID,
				Name:     s.Name,
				Model:    s.Model,
				ExitedAt: s.LastActive.Format(time.RFC3339),
			})
		}
	}
	out.SessionsTotal = len(sessions)
	if len(sessions) > liveMaxSessions {
		sessions = sessions[:liveMaxSessions]
	}
	page := sessions
	if len(page) > p.Limit {
		page = page[:p.Limit]
	}
	out.Truncated = out.SessionsTotal > len(page)

	// 2) 会话聚合（全量入选会话，避免展示上限把统计算偏）。
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		if s.SessionID != "" {
			ids = append(ids, s.SessionID)
		}
	}
	windowStart := now.Add(-p.Window)
	aggs := liveSessionAggregates(ctx, db, ids, windowStart)
	recent := liveRecentCalls(ctx, db, ids, now, p.SparkPoints)

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
		if !s.CreatedAt.IsZero() {
			item.CreatedAt = s.CreatedAt.Format(time.RFC3339)
		}
		if a := aggs[s.SessionID]; a != nil {
			item.Calls = a.Calls
			item.TotalTokens = a.TotalTokens
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

	// 3) 在线用户（**旧字段，语义与 origin/main 一致**）：行集只由「请求中会话」反推，
	//    窗口用量也只汇总这些会话。没有请求中会话时 Users 为空、UsersTotal=0
	//    （逐字节与 main 一致，见 testdata/live_compat_*.golden.json）。
	if len(sessions) > 0 {
		seenUser := make(map[int64]int, 8)
		userIDs := make([]int64, 0, 8)
		for _, s := range sessions {
			if _, ok := seenUser[s.UserID]; !ok {
				seenUser[s.UserID] = 0
				userIDs = append(userIDs, s.UserID)
			}
			seenUser[s.UserID]++
		}
		out.UsersTotal = len(userIDs)
		out.UsersTruncated = len(userIDs) > liveMaxUsers
		userAggs := liveUserAggregates(ctx, db, userIDs, ids, windowStart)
		userSparks := liveUserSpark(ctx, db, userIDs, ids, now, p.SparkPoints)
		names := liveUserNames(ctx, db, userIDs)
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
	}

	// 4) 范围内活跃用户（**新口径，单独字段**）：所选范围内有过请求的用户全集（DB 为准），
	//    包含「此刻没有请求中会话」的用户。与上一段的旧字段互不影响，前端按需取用。
	rangeIDs, rangeAggs := liveRangeUserAggregates(ctx, db, p.From, p.To, windowStart, userID)
	out.RangeUsersTotal = len(rangeIDs)
	if len(rangeIDs) == 0 {
		return out, nil
	}
	sort.Slice(rangeIDs, func(i, j int) bool {
		a, b := rangeAggs[rangeIDs[i]], rangeAggs[rangeIDs[j]]
		if a == nil || b == nil {
			return rangeIDs[i] < rangeIDs[j]
		}
		if a.RangeTokens != b.RangeTokens {
			return a.RangeTokens > b.RangeTokens
		}
		if a.WindowTokens != b.WindowTokens {
			return a.WindowTokens > b.WindowTokens
		}
		return rangeIDs[i] < rangeIDs[j]
	})
	rangeUsers := make([]liveRangeUserItem, 0, len(rangeIDs))
	rangeShown := make([]int64, 0, len(rangeIDs))
	for _, uid := range rangeIDs {
		a := rangeAggs[uid]
		if a == nil {
			continue
		}
		item := *a
		item.TPM = decimalx.Round2(ratePerMinute(float64(item.WindowTokens), windowMinutes))
		item.Spark = []int64{}
		item.Daily = []int64{}
		rangeUsers = append(rangeUsers, item)
		rangeShown = append(rangeShown, uid)
		if len(rangeUsers) >= liveMaxUsers {
			break
		}
	}
	// 曲线/序列只给要返回的行（前 liveMaxUsers 个），别为截断掉的行白算。
	rangeNames := liveUserNames(ctx, db, rangeShown)
	rangeSparks := liveRangeUserSpark(ctx, db, rangeShown, now, p.SparkPoints)
	rangeDailies := liveUserDaily(ctx, db, rangeShown, dayKeys, p.From, p.To)
	for i := range rangeUsers {
		uid := rangeShown[i]
		rangeUsers[i].Name = rangeNames[uid]
		if sp := rangeSparks[uid]; len(sp) > 0 {
			rangeUsers[i].Spark = sp
		}
		if dl := rangeDailies[uid]; len(dl) > 0 {
			rangeUsers[i].Daily = dl
		}
	}
	out.RangeUsers = rangeUsers
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

// HandleStatsLive GET /api/v1/admin/stats/live?Window=&SparkPoints=&Limit=&From=&To= 管理端实时区（全局）。
// From/To = 在线用户的筛选范围（页面右上角时间范围），缺省近 24h。
func (h *Admin) HandleStatsLive(w http.ResponseWriter, r *http.Request) {
	now := h.now()
	p, err := parseLiveParams(r.URL.Query(), now)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	out, err := buildLiveStats(r.Context(), h.db, h.sessions, now, nil, "global", p)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询实时会话失败")
		return
	}
	resp.OK(w, r, out)
}

// HandleStatsLive GET /api/v1/dev/stats/live?Window=&SparkPoints=&Limit=&From=&To= 开发端实时区（仅本人）。
func (h *Dev) HandleStatsLive(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	now := h.now()
	p, err := parseLiveParams(r.URL.Query(), now)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	out, err := buildLiveStats(r.Context(), h.db, h.sessions, now, &userID, "self", p)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询实时会话失败")
		return
	}
	resp.OK(w, r, out)
}
