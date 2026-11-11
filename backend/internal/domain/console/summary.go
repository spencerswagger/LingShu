// summary.go 开发端本人摘要：令牌数 / 在线会话数 / 当日·昨日·本月积分消耗 / 近 1 分钟 RPM·TPM。
package console

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/resp"
	"github.com/team/llmgateway/internal/pkg/statscache"
)

// TokenCounter 令牌计数抽象（identity.TokenStore 满足；nil=未装配时取 0）。
type TokenCounter interface {
	CountByUser(ctx context.Context, userID int64) (int64, error)
}

// SetSessions 注入会话注册表（nil=未装配时在线会话数取 0）。
func (h *Dev) SetSessions(reg *router.SessionRegistry) { h.sessions = reg }

// SetTokenCounter 注入令牌计数器（nil=未装配时令牌数取 0）。
func (h *Dev) SetTokenCounter(c TokenCounter) { h.tokenCounter = c }

// timeRange 闭区间时间范围。
type timeRange struct {
	From time.Time
	To   time.Time
}

// summaryWindow 本人摘要中 RPM/TPM 的统计窗口：近 1 分钟。
// 短窗口（≤1 分钟）用墙钟窗口速率，不用忙时口径。
const summaryWindow = time.Minute

// summaryRanges 按北京时间口径计算今日 / 昨日 / 本月 / 近 1 分钟四个统计区间。
//   - today     = [当日 00:00:00, 当日 23:59:59]
//   - yesterday = [前一自然日 00:00:00, 前一自然日 23:59:59]
//   - month     = [当月 1 日 00:00:00, 当日 23:59:59]
//   - window    = [now-1m, now]
func summaryRanges(now time.Time, loc *time.Location) (today, yesterday, month, window timeRange) {
	n := now.In(loc)
	dayStart := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	dayEnd := time.Date(n.Year(), n.Month(), n.Day(), 23, 59, 59, 0, loc)

	prev := dayStart.AddDate(0, 0, -1)

	today = timeRange{From: dayStart, To: dayEnd}
	yesterday = timeRange{
		From: time.Date(prev.Year(), prev.Month(), prev.Day(), 0, 0, 0, 0, loc),
		To:   time.Date(prev.Year(), prev.Month(), prev.Day(), 23, 59, 59, 0, loc),
	}
	month = timeRange{
		From: time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc),
		To:   dayEnd,
	}
	window = timeRange{From: now.Add(-summaryWindow), To: now}
	return
}

// meSummaryResp 本人摘要响应（字段 PascalCase）。
type meSummaryResp struct {
	TokenCount       int64   `json:"TokenCount"`
	SessionCount     int64   `json:"SessionCount"`
	TodayCredits     float64 `json:"TodayCredits"`
	YesterdayCredits float64 `json:"YesterdayCredits"`
	MonthCredits     float64 `json:"MonthCredits"`
	RPM              float64 `json:"RPM"`
	TPM              float64 `json:"TPM"`
	// 本月缓存口径：InputTokens=纯输入（不含缓存读）总量，CacheReadTokens=缓存读命中量；
	// 前端据此算缓存命中率（分母 = 输入 + 缓存读）。
	InputTokens     int64 `json:"InputTokens"`
	CacheReadTokens int64 `json:"CacheReadTokens"`
}

// HandleMeSummary GET /api/v1/auth/me/summary 本人摘要。
//
// 数据来源（2026-11-10 改造）：
//   - 今日 / 昨日 / 本月积分与本月三段 token 走日级预聚合 `user_daily_stats`（行数=天数，与明细行数无关），
//     并套一层进程内 5 分钟 TTL 缓存（按用户 key，并发合并回源）；
//   - 近 1 分钟 RPM/TPM 属于「秒级口径」，不做缓存、仍查明细（走 (user_id, call_time) 索引，只命中分钟级行数）；
//   - 令牌数、在线会话数与上述无关，保持原实现。
func (h *Dev) HandleMeSummary(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	now := h.now()
	today, y, m, win := summaryRanges(now, cnLocation())

	// 三档消耗走聚合表 + 缓存：同一用户 5 分钟内只回源一次，并发请求合并成一次 DB 查询。
	totals, fromCache, err := h.summaryCache().Get(r.Context(), meSummaryCacheKey(userID),
		func(ctx context.Context) (billing.UserDayTotals, error) {
			return h.billing.UserDayTotals(ctx, userID, today.From, y.From, m.From)
		})
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询摘要失败")
		return
	}
	if fromCache {
		w.Header().Set("x-stats-cache", "hit")
	} else {
		w.Header().Set("x-stats-cache", "miss")
	}

	// 吞吐窗口（RPM/TPM）：近 1 分钟，不过滤状态，以反映真实请求速率。
	winSum, err := h.billing.Summarize(r.Context(), billing.RecordFilter{UserID: &userID, From: &win.From, To: &win.To})
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询摘要失败")
		return
	}

	var tokenCount int64
	if h.tokenCounter != nil {
		tokenCount, err = h.tokenCounter.CountByUser(r.Context(), userID)
		if err != nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询摘要失败")
			return
		}
	}

	var sessionCount int64
	if h.sessions != nil {
		for _, s := range h.sessions.ListAll() {
			if s.UserID == userID && !s.Closed && s.ExpireAt.After(now) {
				sessionCount++
			}
		}
	}

	resp.OK(w, r, meSummaryResp{
		TokenCount:       tokenCount,
		SessionCount:     sessionCount,
		TodayCredits:     totals.TodayCredits,
		YesterdayCredits: totals.YesterdayCredits,
		MonthCredits:     totals.MonthCredits,
		// 短窗口口径：窗口内计数 ÷ 窗口分钟数（近 1 分钟即 ÷1.0），不用忙时口径。
		RPM:             float64(winSum.Requests) / summaryWindow.Minutes(),
		TPM:             float64(winSum.TokensTotal) / summaryWindow.Minutes(),
		InputTokens:     totals.MonthInput,
		CacheReadTokens: totals.MonthCacheRead,
	})
}

// meSummaryCacheKey 本人摘要缓存键：必须带用户 ID——
// 键里少一个隔离维度就会把 A 的积分读给 B（单测 TestDevSummary_CacheIsolatesUsers 守着）。
func meSummaryCacheKey(userID int64) string {
	return "me-summary:" + strconv.FormatInt(userID, 10)
}

// summaryCache 懒初始化本人摘要缓存（5 分钟 TTL，进程内 map + singleflight）。
func (h *Dev) summaryCache() *statscache.Cache[billing.UserDayTotals] {
	h.cacheOnce.Do(func() {
		if h.meSummaryCache == nil {
			h.meSummaryCache = statscache.New[billing.UserDayTotals](statscache.DefaultTTL)
		}
	})
	return h.meSummaryCache
}

// InvalidateMeSummary 主动失效某用户的摘要缓存（记账写入后由装配层挂到 billing 回调上）。
func (h *Dev) InvalidateMeSummary(userID int64) {
	if h.meSummaryCache == nil {
		return
	}
	h.meSummaryCache.Invalidate(meSummaryCacheKey(userID))
}
