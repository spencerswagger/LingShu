// summary.go 开发端本人摘要：令牌数 / 在线会话数 / 当日·昨日·本月积分消耗 / 近 1 分钟 RPM·TPM。
package console

import (
	"context"
	"net/http"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/resp"
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
func (h *Dev) HandleMeSummary(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	now := h.now()
	today, y, m, win := summaryRanges(now, cnLocation())

	// 查询顺序严格为 today、yesterday、month、window。
	// 三档消耗按 status='completed' 统计，与既有「累计消费」（LoadLedger）口径一致；
	// 吞吐窗口（RPM/TPM）不过滤状态，以反映真实请求速率。
	todaySum, err := h.billing.Summarize(r.Context(), billing.RecordFilter{UserID: &userID, Status: "completed", From: &today.From, To: &today.To})
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询摘要失败")
		return
	}
	ySum, err := h.billing.Summarize(r.Context(), billing.RecordFilter{UserID: &userID, Status: "completed", From: &y.From, To: &y.To})
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询摘要失败")
		return
	}
	mSum, err := h.billing.Summarize(r.Context(), billing.RecordFilter{UserID: &userID, Status: "completed", From: &m.From, To: &m.To})
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询摘要失败")
		return
	}
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
		TodayCredits:     todaySum.CreditsTotal,
		YesterdayCredits: ySum.CreditsTotal,
		MonthCredits:     mSum.CreditsTotal,
		// 短窗口口径：窗口内计数 ÷ 窗口分钟数（近 1 分钟即 ÷1.0），不用忙时口径。
		RPM:             float64(winSum.Requests) / summaryWindow.Minutes(),
		TPM:             float64(winSum.TokensTotal) / summaryWindow.Minutes(),
		InputTokens:     mSum.InputTokens,
		CacheReadTokens: mSum.CacheReadTokens,
	})
}
