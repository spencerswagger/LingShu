// stats.go 统计页（统一仪表盘）聚合接口：admin（全局）与 dev（本人）共用同一聚合器与响应结构。
// 运维块（渠道/密钥/模型计数）仅管理端返回，开发端返回 ops 为空。
package console

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/pkg/decimalx"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// statsKPI 顶部关键指标。
type statsKPI struct {
	TotalTokens     int64   `json:"TotalTokens"`
	TotalCredits    float64 `json:"TotalCredits"`
	TotalRequests   int64   `json:"TotalRequests"`
	FailedRequests  int64   `json:"FailedRequests"`
	SuccessRate     float64 `json:"SuccessRate"` // 0~1
	AvgDurationMS   float64 `json:"AvgDurationMS"`
	AvgFirstTokenMS float64 `json:"AvgFirstTokenMS"`
	RPM             float64 `json:"RPM"` // 忙时口径（见 billing.DashboardStats.BusyRPM），文案仍叫 RPM
	TPM             float64 `json:"TPM"` // 忙时口径（见 billing.DashboardStats.BusyTPM），文案仍叫 TPM
	// BusyRequests 忙时口径的分子（有耗时请求数，与 RPM/TPM 的分母同源）。
	// 前端判断「样本是否偏少」必须用它，不能用 TotalRequests（后者含无耗时的失败记录，不同源会误判）。
	BusyRequests int64 `json:"BusyRequests"`
	// SumDurationMS 忙时分母（成功且带耗时请求的耗时之和，毫秒），供前端做「样本量/时长是否够」的标注。
	SumDurationMS int64   `json:"SumDurationMS"`
	DeltaTokens   float64 `json:"DeltaTokens"` // 环比上期（比例，可为负）
	DeltaCredits  float64 `json:"DeltaCredits"`
	DeltaRequests float64 `json:"DeltaRequests"`
	// 以下三个字段说明「首屏三个大数字实际覆盖的时间窗」：
	// 默认窗口 = 请求原始区间（近 24h 就是滚动 24 小时），与趋势图、模型广场同一窗口；
	// Usage.Daily 等「按自然日」字段仍是日界归一口径，两者刻意不同，别混用（PR #61 Owl P2）。
	WindowFrom string `json:"WindowFrom"` // RFC3339
	WindowTo   string `json:"WindowTo"`   // RFC3339
	WindowMode string `json:"WindowMode"` // exact（默认）| calendar
}

// statsCounts 三态计数（密钥 / 内部模型共用）。
type statsCounts struct {
	Total    int `json:"Total"`
	Normal   int `json:"Normal"`
	Drain    int `json:"Drain"`
	Disabled int `json:"Disabled"`
}

// statsChannelCounts 渠道计数：渠道无独立状态，仅总数（三态见 Keys）。
type statsChannelCounts struct {
	Total int `json:"Total"`
}

// statsModelCounts 模型计数（对外模型 / 渠道内部模型）。
type statsModelCounts struct {
	ExternalTotal   int `json:"ExternalTotal"`
	ExternalEnabled int `json:"ExternalEnabled"`
	InternalTotal   int `json:"InternalTotal"`
	InternalNormal  int `json:"InternalNormal"`
}

// statsOps 运维块。
type statsOps struct {
	Channels statsChannelCounts `json:"Channels"`
	Keys     statsCounts        `json:"Keys"`
	Models   statsModelCounts   `json:"Models"`
}

// statsResp 统计页响应（admin/dev 共用；ops 仅 admin 返回）。
type statsResp struct {
	Scope      string                 `json:"Scope"` // global | self
	From       string                 `json:"From"`
	To         string                 `json:"To"`
	KPI        statsKPI               `json:"KPI"`
	Usage      billing.DashboardStats `json:"Usage"`
	ModelPlaza billing.ModelPlaza     `json:"ModelPlaza"` // 模型广场（新块，老调用方忽略即可）
	Ops        *statsOps              `json:"Ops,omitempty"`
}

// statsRange 统计区间。
//   - From/To：归一到整天边界后的区间（既有口径，供 Daily / 环比 / 首屏数字沿用，语义不变）；
//   - ExactFrom/ExactTo：请求里给的原始区间（不归一），小时粒度按它切桶，
//     否则「近 24h」会被整天归一拉成最长 48 小时、多出半天空桶。
type statsRange struct {
	From, To           time.Time
	ExactFrom, ExactTo time.Time
}

// stats 请求的参数默认值与上限（越界一律收敛，非数字才报 400）。
const (
	statsGranularityAuto = "auto"
	statsGranularityHour = "hour"
	statsGranularityDay  = "day"
	// statsHourMaxSpan auto 模式下按小时出桶的最大跨度：超过就按天，避免小时桶无限增长。
	statsHourMaxSpan = 26 * time.Hour
	// statsHourHardSpan 显式 Granularity=hour 时允许的最大跨度（2 天）。
	statsHourHardSpan = 48 * time.Hour

	statsDefaultModelLimit = 50

	// KPI 窗口模式：exact = 严格按请求区间（默认，与前端标签一致）；calendar = 日界归一（旧行为）。
	statsWindowExact    = "exact"
	statsWindowCalendar = "calendar"
)

// parseRFC3339Lenient 解析 RFC3339 时间，并兼容查询串中未编码的 '+'：'2026-10-15T23:59:59+08:00'
// 若被 URL 解码为空格（' 08:00'）会解析失败，这里把空格还原为 '+' 后重试。
func parseRFC3339Lenient(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, strings.ReplaceAll(v, " ", "+"))
}

// parseStatsRange 解析 from/to（RFC3339，缺省近 30 天），限制跨度 ≤366 天。
// 返回两套区间：整天归一的 From/To（既有口径，语义不变）与请求原始值的 ExactFrom/ExactTo。
// 日界统一按 Asia/Shanghai 归一（与全站统计口径一致），不随请求自带时区偏移截断。
func parseStatsRange(q url.Values, now time.Time) (statsRange, error) {
	to := now
	if v := q.Get("To"); v != "" {
		t, err := parseRFC3339Lenient(v)
		if err != nil {
			return statsRange{}, errors.New("to 时间格式应为 RFC3339")
		}
		to = t
	}
	from := to.AddDate(0, 0, -29) // 近 30 天（含今天）
	if v := q.Get("From"); v != "" {
		t, err := parseRFC3339Lenient(v)
		if err != nil {
			return statsRange{}, errors.New("from 时间格式应为 RFC3339")
		}
		from = t
	}
	if from.After(to) {
		return statsRange{}, errors.New("开始时间不能晚于结束时间")
	}
	if to.Sub(from) > 366*24*time.Hour {
		return statsRange{}, errors.New("时间跨度不能超过 366 天")
	}
	loc := cnLocation()
	fromCN := from.In(loc)
	toCN := to.In(loc)
	return statsRange{
		From:      time.Date(fromCN.Year(), fromCN.Month(), fromCN.Day(), 0, 0, 0, 0, loc),
		To:        time.Date(toCN.Year(), toCN.Month(), toCN.Day(), 23, 59, 59, 0, loc),
		ExactFrom: from,
		ExactTo:   to,
	}, nil
}

// statsRangeFromQuery 保留既有签名（只关心整天归一的区间）：
// 老调用方与既有测试无需感知 ExactFrom/ExactTo。
func statsRangeFromQuery(q url.Values, now time.Time) (time.Time, time.Time, error) {
	r, err := parseStatsRange(q, now)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return r.From, r.To, nil
}

// resolveGranularity 决定实际出桶粒度。
//   - 未传 / auto：跨度 ≤ 26h 按小时，否则按天（跨大范围自动降级为天）；
//   - hour：显式请求，但跨度 > 2 天直接报 400（避免一次拉出上千个小时桶）；
//   - day：一律按天（老调用方口径）。
func resolveGranularity(q url.Values, span time.Duration) (string, error) {
	switch g := q.Get("Granularity"); g {
	case "":
		return autoGranularity(span), nil
	case statsGranularityAuto:
		return autoGranularity(span), nil
	case statsGranularityHour:
		// 跨度过大时**自动降级为天**（不报错）：显式请求 hour 但没带 From/To（默认近 30 天）也照样可用，
		// 避免前端因为少了两个参数就拿到 400。
		if span > statsHourHardSpan {
			return statsGranularityDay, nil
		}
		return statsGranularityHour, nil
	case statsGranularityDay:
		return statsGranularityDay, nil
	default:
		return "", errors.New("Granularity 取值应为 auto / hour / day")
	}
}

func autoGranularity(span time.Duration) string {
	if span <= statsHourMaxSpan {
		return statsGranularityHour
	}
	return statsGranularityDay
}

// statsQueryOptions 统计接口的增量可选参数（缺省值保持既有行为）。
type statsQueryOptions struct {
	Granularity string // hour | day
	CompareDays int    // 0=不返回同期对比
	ModelSort   string // tokens | tpm | failed | latency
	ModelLimit  int
	Window      string // exact（默认）| calendar
}

// parseStatsOptions 解析增量可选参数。缺省：粒度按区间自动判定，不做同期对比，
// 模型广场按 Token 倒序取前 50；越界值收敛到上限，只有明显的类型错误才报 400。
func parseStatsOptions(q url.Values, span time.Duration) (statsQueryOptions, error) {
	granularity, err := resolveGranularity(q, span)
	if err != nil {
		return statsQueryOptions{}, err
	}
	opts := statsQueryOptions{
		Granularity: granularity,
		ModelSort:   billing.NormalizeModelSort(q.Get("ModelSort")),
		ModelLimit:  statsDefaultModelLimit,
		Window:      statsWindowExact,
	}
	switch w := q.Get("Window"); w {
	case "":
		// 默认严格按请求区间：标签写「近 24h」就必须是滚动 24 小时。
	case statsWindowExact:
		opts.Window = statsWindowExact
	case statsWindowCalendar:
		opts.Window = statsWindowCalendar
	default:
		return statsQueryOptions{}, errors.New("Window 取值应为 exact / calendar")
	}
	if v := q.Get("CompareDays"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return statsQueryOptions{}, errors.New("CompareDays 必须为数字")
		}
		if n < 0 {
			n = 0
		}
		if n > billing.MaxCompareDays {
			n = billing.MaxCompareDays
		}
		opts.CompareDays = n
	}
	if v := q.Get("ModelLimit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return statsQueryOptions{}, errors.New("ModelLimit 必须为数字")
		}
		if n <= 0 {
			n = statsDefaultModelLimit
		}
		if n > billing.MaxModelLimit {
			n = billing.MaxModelLimit
		}
		opts.ModelLimit = n
	}
	return opts, nil
}

// cnLocation 返回北京时间时区；加载失败回退进程本地时区（进程时区已在启动时统一为 Asia/Shanghai）。
func cnLocation() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.Local
	}
	return loc
}

// deltaRatio 环比比例：(cur-prev)/prev；prev<=0 时返回 0。
func deltaRatio(cur, prev float64) float64 {
	if prev <= 0 {
		return 0
	}
	return (cur - prev) / prev
}

// buildKPI 由**KPI 窗口**的总量（billing.WindowTotals）+ 上期总量构造 KPI 块。
// 注意：入参是 WindowTotals 而不是 DashboardStats —— 首屏数字必须与趋势图/模型广场同窗口，
// 不能再用日界归一的聚合结果（PR #61 Owl P2）。
func buildKPI(wt billing.WindowTotals, prevCalls int64, prevCredits, prevTokens float64) statsKPI {
	return statsKPI{
		TotalTokens:     wt.Total.Total(),
		TotalCredits:    decimalx.Round5(wt.Credits),
		TotalRequests:   wt.Calls,
		FailedRequests:  wt.Failed,
		SuccessRate:     wt.Success,
		AvgDurationMS:   decimalx.Round2(wt.AvgMS),
		AvgFirstTokenMS: decimalx.Round2(wt.AvgFirst),
		// 长区间不用墙钟平均，改用忙时处理速率：请求数/Token ÷ 耗时之和折算的分钟数。
		RPM:           decimalx.Round2(wt.BusyRPM),
		TPM:           decimalx.Round2(wt.BusyTPM),
		BusyRequests:  wt.BusyRequests,
		SumDurationMS: wt.SumDurationMS,
		DeltaTokens:   decimalx.Round5(deltaRatio(float64(wt.Total.Total()), prevTokens)),
		DeltaCredits:  decimalx.Round5(deltaRatio(wt.Credits, prevCredits)),
		DeltaRequests: decimalx.Round5(deltaRatio(float64(wt.Calls), float64(prevCalls))),
	}
}

// buildStatsUsage 拉取区间聚合 + KPI 窗口总量并组装响应（scope 由 userID 决定）。
//
// 两套窗口刻意分开，别混用：
//   - **KPI 窗口**（默认 = 请求原始区间）：首屏三个大数字、小时趋势、模型广场共用，
//     所以「近 24h」就是滚动 24 小时，标签与数字一致（PR #61 Owl P2）；
//   - **日界窗口**（rg.From/rg.To，按 Asia/Shanghai 归一整天）：仅供 `Usage.Daily` 与既有的
//     按天/维度聚合字段使用，口径与改造前完全一致（前端另有用途）。
//
// Window=calendar 时 KPI 窗口退回日界窗口（等价改造前行为），供需要旧数字的调用方使用。
func buildStatsUsage(ctx context.Context, store *billing.SqlStore, rg statsRange, userID *int64, scope string, opts statsQueryOptions) (statsResp, error) {
	// 日界口径聚合（老字段：Usage.Daily / 各 Top / 延迟分位 等，语义不变）。
	from, to := rg.From, rg.To
	ds, err := store.Dashboard(ctx, from, to, userID, 10)
	if err != nil {
		return statsResp{}, err
	}

	// KPI 窗口：默认严格用请求原始区间；calendar 模式才退回日界区间。
	winFrom, winTo := rg.ExactFrom, rg.ExactTo
	if opts.Window == statsWindowCalendar {
		winFrom, winTo = from, to
	}
	wt, err := store.WindowTotals(ctx, winFrom, winTo, userID)
	if err != nil {
		return statsResp{}, err
	}
	// 环比上期：与 KPI 窗口等长、紧邻其前。
	span := winTo.Sub(winFrom)
	prevFrom := winFrom.Add(-span - time.Second)
	prevTo := winFrom.Add(-time.Second)
	prevCalls, prevCredits, prevTokens, err := store.RangeTotals(ctx, prevFrom, prevTo, userID)
	if err != nil {
		return statsResp{}, err
	}

	// 小时桶 / 同期对比 / 模型广场：全部落在同一个 KPI 窗口上。
	ds.Granularity = opts.Granularity
	if opts.Granularity == statsGranularityHour {
		hourly, err := store.DashboardHourly(ctx, winFrom, winTo, userID)
		if err != nil {
			return statsResp{}, err
		}
		if hourly != nil {
			ds.Hourly = hourly
		}
	}
	if opts.CompareDays > 0 {
		compare, err := store.DashboardCompare(ctx, winTo, userID, opts.CompareDays)
		if err != nil {
			return statsResp{}, err
		}
		if compare != nil {
			ds.Compare = compare
		}
	}
	plaza, err := store.DashboardModels(ctx, winFrom, winTo, userID, opts.ModelSort, opts.ModelLimit)
	if err != nil {
		return statsResp{}, err
	}
	kpi := buildKPI(wt, prevCalls, prevCredits, float64(prevTokens))
	kpi.WindowFrom = winFrom.Format(time.RFC3339)
	kpi.WindowTo = winTo.Format(time.RFC3339)
	kpi.WindowMode = opts.Window
	return statsResp{
		Scope:      scope,
		From:       from.Format(time.RFC3339),
		To:         to.Format(time.RFC3339),
		KPI:        kpi,
		Usage:      ds,
		ModelPlaza: plaza,
	}, nil
}

// HandleStatsDashboard GET /api/v1/admin/stats/dashboard?from=&to=&Granularity=&CompareDays=&ModelSort=&ModelLimit=
// 管理端全局统计。
func (h *Admin) HandleStatsDashboard(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rg, err := parseStatsRange(q, h.now())
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	opts, err := parseStatsOptions(q, rg.ExactTo.Sub(rg.ExactFrom))
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	out, err := buildStatsUsage(r.Context(), h.billing, rg, nil, "global", opts)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询统计数据失败")
		return
	}
	ops, err := h.buildOps(r.Context())
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询运维统计失败")
		return
	}
	out.Ops = ops
	resp.OK(w, r, out)
}

// buildOps 汇总运维块：渠道总数 + 密钥运行时三态 + 对外模型、渠道内部模型计数。
func (h *Admin) buildOps(ctx context.Context) (*statsOps, error) {
	ops := &statsOps{}
	// 渠道总数（渠道无独立状态，可用性由密钥/模型三态体现）
	ops.Channels.Total = len(h.channelMgr.Snapshot())
	// 密钥运行时三态
	for st, n := range h.channelMgr.KeyStateCounts() {
		ops.Keys.Total += n
		switch st {
		case channel.StateNormal:
			ops.Keys.Normal += n
		case channel.StateDrain:
			ops.Keys.Drain += n
		case channel.StateDisabled:
			ops.Keys.Disabled += n
		}
	}
	// 渠道内部模型运行时三态
	for st, n := range h.channelMgr.ModelStateCounts() {
		ops.Models.InternalTotal += n
		if st == channel.StateNormal {
			ops.Models.InternalNormal += n
		}
	}
	// 对外模型计数（DB）
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*), COALESCE(sum(enabled::int),0)
		 FROM external_models WHERE deleted_at IS NULL`).
		Scan(&ops.Models.ExternalTotal, &ops.Models.ExternalEnabled); err != nil {
		return nil, err
	}
	return ops, nil
}

// HandleStatsDashboard GET /api/v1/dev/stats/dashboard?from=&to=&Granularity=&CompareDays=&ModelSort=&ModelLimit=
// 开发端本人统计（无运维块）。
func (h *Dev) HandleStatsDashboard(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	rg, err := parseStatsRange(q, h.now())
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	opts, err := parseStatsOptions(q, rg.ExactTo.Sub(rg.ExactFrom))
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	out, err := buildStatsUsage(r.Context(), h.billing, rg, &userID, "self", opts)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询统计数据失败")
		return
	}
	resp.OK(w, r, out)
}
