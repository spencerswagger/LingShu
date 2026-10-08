// stats.go 统计页（统一仪表盘）聚合接口：admin（全局）与 dev（本人）共用同一聚合器与响应结构。
// 运维块（渠道/密钥/模型计数）仅管理端返回，开发端返回 ops 为空。
package console

import (
	"context"
	"errors"
	"net/http"
	"net/url"
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
	RPM             float64 `json:"RPM"`
	TPM             float64 `json:"TPM"`
	DeltaTokens     float64 `json:"DeltaTokens"` // 环比上期（比例，可为负）
	DeltaCredits    float64 `json:"DeltaCredits"`
	DeltaRequests   float64 `json:"DeltaRequests"`
}

// statsCounts 三态计数（渠道 / 密钥共用）。
type statsCounts struct {
	Total    int `json:"Total"`
	Normal   int `json:"Normal"`
	Drain    int `json:"Drain"`
	Disabled int `json:"Disabled"`
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
	Channels statsCounts      `json:"Channels"`
	Keys     statsCounts      `json:"Keys"`
	Models   statsModelCounts `json:"Models"`
}

// statsResp 统计页响应（admin/dev 共用；ops 仅 admin 返回）。
type statsResp struct {
	Scope string                 `json:"Scope"` // global | self
	From  string                 `json:"From"`
	To    string                 `json:"To"`
	KPI   statsKPI               `json:"KPI"`
	Usage billing.DashboardStats `json:"Usage"`
	Ops   *statsOps              `json:"Ops,omitempty"`
}

// statsRangeFromQuery 解析 from/to（RFC3339，缺省近 30 天），归一到整天边界，限制跨度 ≤366 天。
// 日界统一按 Asia/Shanghai 归一（与全站统计口径一致），不随请求自带时区偏移截断。
func statsRangeFromQuery(q url.Values, now time.Time) (time.Time, time.Time, error) {
	to := now
	if v := q.Get("To"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("to 时间格式应为 RFC3339")
		}
		to = t
	}
	from := to.AddDate(0, 0, -29) // 近 30 天（含今天）
	if v := q.Get("From"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("from 时间格式应为 RFC3339")
		}
		from = t
	}
	if from.After(to) {
		return time.Time{}, time.Time{}, errors.New("开始时间不能晚于结束时间")
	}
	if to.Sub(from) > 366*24*time.Hour {
		return time.Time{}, time.Time{}, errors.New("时间跨度不能超过 366 天")
	}
	loc := cnLocation()
	fromCN := from.In(loc)
	toCN := to.In(loc)
	from = time.Date(fromCN.Year(), fromCN.Month(), fromCN.Day(), 0, 0, 0, 0, loc)
	to = time.Date(toCN.Year(), toCN.Month(), toCN.Day(), 23, 59, 59, 0, loc)
	return from, to, nil
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

// buildKPI 由聚合结果 + 上期总量构造 KPI 块。
func buildKPI(ds billing.DashboardStats, prevCalls int64, prevCredits, prevTokens float64) statsKPI {
	return statsKPI{
		TotalTokens:     ds.Total.Total(),
		TotalCredits:    decimalx.Round5(ds.TotalCred),
		TotalRequests:   ds.TotalCall,
		FailedRequests:  ds.Failed,
		SuccessRate:     ds.Success,
		AvgDurationMS:   decimalx.Round2(ds.AvgMS),
		AvgFirstTokenMS: decimalx.Round2(ds.AvgFirst),
		RPM:             decimalx.Round2(ds.AvgRPM),
		TPM:             decimalx.Round2(ds.AvgTPM),
		DeltaTokens:     decimalx.Round5(deltaRatio(float64(ds.Total.Total()), prevTokens)),
		DeltaCredits:    decimalx.Round5(deltaRatio(ds.TotalCred, prevCredits)),
		DeltaRequests:   decimalx.Round5(deltaRatio(float64(ds.TotalCall), float64(prevCalls))),
	}
}

// buildStatsUsage 拉取区间聚合 + 上期总量并组装响应（scope 由 userID 决定）。
func buildStatsUsage(ctx context.Context, store *billing.SqlStore, from, to time.Time, userID *int64, scope string) (statsResp, error) {
	ds, err := store.Dashboard(ctx, from, to, userID, 10)
	if err != nil {
		return statsResp{}, err
	}
	// 上期：等长时间窗（紧邻当前区间之前）。
	span := to.Sub(from)
	prevFrom := from.Add(-span - time.Second)
	prevTo := from.Add(-time.Second)
	prevCalls, prevCredits, prevTokens, err := store.RangeTotals(ctx, prevFrom, prevTo, userID)
	if err != nil {
		return statsResp{}, err
	}
	return statsResp{
		Scope: scope,
		From:  from.Format(time.RFC3339),
		To:    to.Format(time.RFC3339),
		KPI:   buildKPI(ds, prevCalls, prevCredits, float64(prevTokens)),
		Usage: ds,
	}, nil
}

// HandleStatsDashboard GET /api/v1/admin/stats/dashboard?from=&to= 管理端全局统计。
func (h *Admin) HandleStatsDashboard(w http.ResponseWriter, r *http.Request) {
	from, to, err := statsRangeFromQuery(r.URL.Query(), h.now())
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	out, err := buildStatsUsage(r.Context(), h.billing, from, to, nil, "global")
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

// buildOps 汇总运维块：渠道/密钥运行时三态 + 对外模型、渠道内部模型计数。
func (h *Admin) buildOps(ctx context.Context) (*statsOps, error) {
	ops := &statsOps{}
	// 渠道运行时三态
	for _, v := range h.channelMgr.Snapshot() {
		ops.Channels.Total++
		switch v.State {
		case channel.StateNormal:
			ops.Channels.Normal++
		case channel.StateDrain:
			ops.Channels.Drain++
		case channel.StateDisabled:
			ops.Channels.Disabled++
		}
	}
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

// HandleStatsDashboard GET /api/v1/dev/stats/dashboard?from=&to= 开发端本人统计（无运维块）。
func (h *Dev) HandleStatsDashboard(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	from, to, err := statsRangeFromQuery(r.URL.Query(), time.Now())
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	out, err := buildStatsUsage(r.Context(), h.billing, from, to, &userID, "self")
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询统计数据失败")
		return
	}
	resp.OK(w, r, out)
}
