// ops.go 是阶段二 A6 的两个运营接口：盈亏（P&L）与对账窗口。
//
// 为什么现在才做：盈亏需要「收入（积分售价）」与「成本（账本成本 + 订阅摊销）」两侧都有真值 ——
// A1/A3/A5 落完这三列（credits_consumed / cost_credits+cost_source）之后，这两个接口才有意义。
//
// 口径（不许在 SQL 里各写一套）：
//   - 收入 = sum(credits_consumed)（用户实际扣的积分，售价口径）；
//   - 成本 = sum(cost_credits)（上游实付 + 订阅摊销，成本口径），按 cost_source 拆成两段；
//   - 毛利 = 收入 − 成本；毛利率 = 毛利 / 收入（收入为 0 时返回 null，不返回 0 假装「不亏」）；
//   - token = billing.TokenSumExpr（三段互斥口径的唯一实现）；
//   - `uncosted` = 成本为 NULL 的请求数。**必须返回**：它说明这批账单还没有成本口径
//     （账本没拉到 / 订阅没配），毛利此刻是「虚高」的，页面要能提示，不许当成真实毛利。
package console

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/decimalx"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// pnlGroup 是盈亏分组维度。
type pnlGroup string

const (
	pnlByProvider pnlGroup = "provider"
	pnlByAccount  pnlGroup = "account"
	pnlByModel    pnlGroup = "model"
)

// pnlGroupExpr 把分组维度映射成 SQL 表达式。
//
// `(unknown)` 是刻意保留的标签：历史账单没有 upstream_* 值，按「未知」单独成组，
// 不许 COALESCE 成空串混进某个真实分组（那会让某个供应商的数字莫名其妙变大）。
func pnlGroupExpr(g pnlGroup) string {
	switch g {
	case pnlByAccount:
		return `COALESCE(upstream_provider,'(unknown)') || '/' || COALESCE(upstream_account,'(unknown)')`
	case pnlByModel:
		return `COALESCE(internal_model_id,'(unknown)')`
	default:
		return `COALESCE(upstream_provider,'(unknown)')`
	}
}

// pnlRow 是一行盈亏。
type pnlRow struct {
	Key       string   `json:"Key"`
	Requests  int64    `json:"Requests"`
	Tokens    int64    `json:"Tokens"`
	Revenue   float64  `json:"Revenue"`
	Cost      float64  `json:"Cost"`
	Margin    float64  `json:"Margin"`
	MarginPct *float64 `json:"MarginPct"`
	// LedgerCost / AmortizedCost 拆开给运营看「变动成本 vs 固定成本」。
	LedgerCost    float64 `json:"LedgerCost"`
	AmortizedCost float64 `json:"AmortizedCost"`
	// Uncosted 是成本还没有口径的请求数（>0 时毛利偏高）。
	Uncosted int64 `json:"Uncosted"`
}

// pnlResponse 是 /admin/ops/pnl 的响应。
type pnlResponse struct {
	From      time.Time `json:"From"`
	To        time.Time `json:"To"`
	GroupBy   string    `json:"GroupBy"`
	Requests  int64     `json:"Requests"`
	Tokens    int64     `json:"Tokens"`
	Revenue   float64   `json:"Revenue"`
	Cost      float64   `json:"Cost"`
	Margin    float64   `json:"Margin"`
	MarginPct *float64  `json:"MarginPct"`
	Uncosted  int64     `json:"Uncosted"`
	Groups    []pnlRow  `json:"Groups"`
	// Truncated 表示分组数超过上限被截断（前端要提示，不许当成全部）。
	Truncated bool `json:"Truncated"`
}

// pnlMaxGroups 是分组上限（防止一个时间窗把整张表拉回来）。
const pnlMaxGroups = 200

// HandlePnL 返回区间盈亏（默认最近 24 小时，最多 366 天）。
func (h *Admin) HandlePnL(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := h.now()
	to := now
	from := now.Add(-24 * time.Hour)
	if v := q.Get("From"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "From 需为 RFC3339 时间")
			return
		}
		from = t
	}
	if v := q.Get("To"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "To 需为 RFC3339 时间")
			return
		}
		to = t
	}
	if !to.After(from) {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "To 必须晚于 From")
		return
	}
	if to.Sub(from) > 366*24*time.Hour {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "区间不得超过 366 天")
		return
	}
	group := pnlGroup(strings.TrimSpace(q.Get("GroupBy")))
	switch group {
	case pnlByProvider, pnlByAccount, pnlByModel:
	case "":
		group = pnlByProvider
	default:
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "GroupBy 只支持 provider/account/model")
		return
	}
	if h.db == nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "盈亏查询未装配数据库")
		return
	}

	query := `SELECT ` + pnlGroupExpr(group) + ` AS k,
	                 count(*),
	                 COALESCE(sum(` + billing.TokenSumExpr("") + `),0),
	                 COALESCE(sum(COALESCE(credits_consumed,0)),0),
	                 COALESCE(sum(COALESCE(cost_credits,0)),0),
	                 COALESCE(sum(COALESCE(cost_credits,0)) FILTER (WHERE cost_source = 'ledger'),0),
	                 COALESCE(sum(COALESCE(cost_credits,0)) FILTER (WHERE cost_source = 'amortized'),0),
	                 count(*) FILTER (WHERE cost_credits IS NULL)
	            FROM billing_records
	           WHERE call_time >= $1 AND call_time < $2
	           GROUP BY 1
	           ORDER BY 4 DESC, 1
	           LIMIT ` + strconv.Itoa(pnlMaxGroups+1)

	rows, err := h.db.QueryContext(r.Context(), query, from, to)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询盈亏失败")
		return
	}
	defer rows.Close()

	out := pnlResponse{From: from, To: to, GroupBy: string(group), Groups: []pnlRow{}}
	for rows.Next() {
		var row pnlRow
		if err := rows.Scan(&row.Key, &row.Requests, &row.Tokens, &row.Revenue, &row.Cost,
			&row.LedgerCost, &row.AmortizedCost, &row.Uncosted); err != nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "读取盈亏失败")
			return
		}
		row.Revenue = decimalx.Round5(row.Revenue)
		row.Cost = decimalx.Round5(row.Cost)
		row.Margin = decimalx.Round5(row.Revenue - row.Cost)
		row.MarginPct = marginPct(row.Revenue, row.Margin)
		out.Requests += row.Requests
		out.Tokens += row.Tokens
		out.Revenue = decimalx.Round5(out.Revenue + row.Revenue)
		out.Cost = decimalx.Round5(out.Cost + row.Cost)
		out.Uncosted += row.Uncosted
		out.Groups = append(out.Groups, row)
	}
	if err := rows.Err(); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "遍历盈亏失败")
		return
	}
	if len(out.Groups) > pnlMaxGroups {
		out.Groups = out.Groups[:pnlMaxGroups]
		out.Truncated = true
	}
	out.Margin = decimalx.Round5(out.Revenue - out.Cost)
	out.MarginPct = marginPct(out.Revenue, out.Margin)
	resp.OK(w, r, out)
}

// marginPct 返回毛利率；收入 <= 0 时返回 nil（无收入谈不上毛利率，返回 0 会被读成「不赚钱但也不亏」）。
func marginPct(revenue, margin float64) *float64 {
	if revenue <= 0 {
		return nil
	}
	v := decimalx.Round5(margin / revenue)
	return &v
}

// reconcileWindow 是 /admin/ops/reconcile-windows 的一行。
type reconcileWindow struct {
	WindowStart time.Time `json:"WindowStart"`
	WindowEnd   time.Time `json:"WindowEnd"`
	Status      string    `json:"Status"`
	AlarmStreak int16     `json:"AlarmStreak"`
	// 计数列可空：null = 还没算过（不是「0 笔」）。
	LocalCount     *int64    `json:"LocalCount"`
	UpstreamCount  *int64    `json:"UpstreamCount"`
	LocalTokens    *int64    `json:"LocalTokens"`
	UpstreamTokens *int64    `json:"UpstreamTokens"`
	DeviationRate  *float64  `json:"DeviationRate"`
	ScrollOffset   int64     `json:"ScrollOffset"`
	CheckedAt      time.Time `json:"CheckedAt"`
}

// HandleReconcileWindows 返回对账窗口（默认最近 60 分钟）。
//
// 这个接口存在的理由：漏账必须「看得见」。前端要能回答三个问题——
// 哪些窗口没算过（计数 null）、哪些被 blocked（同步没拉完）、偏差率多少。
func (h *Admin) HandleReconcileWindows(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := h.now()
	to := now
	from := now.Add(-time.Hour)
	if v := q.Get("From"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "From 需为 RFC3339 时间")
			return
		}
		from = t
	}
	if v := q.Get("To"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "To 需为 RFC3339 时间")
			return
		}
		to = t
	}
	if !to.After(from) {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "To 必须晚于 From")
		return
	}
	limit := 500
	if v := q.Get("Limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "Limit 需为正整数")
			return
		}
		if n > 2000 {
			n = 2000
		}
		limit = n
	}
	if h.db == nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "对账查询未装配数据库")
		return
	}

	rows, err := h.db.QueryContext(r.Context(),
		`SELECT window_start, window_end, status, alarm_streak,
		        local_count, upstream_count, local_tokens, upstream_tokens,
		        (detail->>'deviation_rate')::double precision,
		        scroll_offset, checked_at
		   FROM reconcile_windows
		  WHERE window_start >= $1 AND window_start < $2
		  ORDER BY window_start DESC
		  LIMIT $3`, from, to, limit)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询对账窗口失败")
		return
	}
	defer rows.Close()

	items := make([]reconcileWindow, 0, limit)
	for rows.Next() {
		var (
			item   reconcileWindow
			lc, uc *int64
			lt, ut *int64
			dev    *float64
		)
		if err := rows.Scan(&item.WindowStart, &item.WindowEnd, &item.Status, &item.AlarmStreak,
			&lc, &uc, &lt, &ut, &dev, &item.ScrollOffset, &item.CheckedAt); err != nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "读取对账窗口失败")
			return
		}
		item.LocalCount, item.UpstreamCount, item.LocalTokens, item.UpstreamTokens = lc, uc, lt, ut
		item.DeviationRate = dev
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "遍历对账窗口失败")
		return
	}
	resp.OK(w, r, map[string]any{"From": from, "To": to, "Items": items})
}
