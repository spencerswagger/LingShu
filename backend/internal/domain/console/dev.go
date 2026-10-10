// dev.go 开发端控制台查询：本人用量聚合、账单列表/详情（三步拆解）。
package console

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/decimalx"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// Dev 承载开发端控制台处理器。userID 从鉴权上下文取（userIDFrom 注入）。
type Dev struct {
	billing    *billing.SqlStore
	userIDFrom func(ctx context.Context) (int64, bool)
	callLogs   callLogReader // 调用日志只读查询（经 SetCallLogStore 注入；nil=未装配）
}

// NewDev 创建开发端控制台处理器。
func NewDev(billingStore *billing.SqlStore, userIDFrom func(ctx context.Context) (int64, bool)) *Dev {
	return &Dev{billing: billingStore, userIDFrom: userIDFrom}
}

// SetCallLogStore 注入调用日志只读查询（nil=未装配：BillingDetail.CallLog 为 nil）。
func (h *Dev) SetCallLogStore(s callLogReader) { h.callLogs = s }

// attachCallLog 查 call_logs 并填充账单详情（未装配/无记录时为 nil）。
func (h *Dev) attachCallLog(ctx context.Context, bid string) *CallLogView {
	if h.callLogs == nil || bid == "" {
		return nil
	}
	cl, err := h.callLogs.GetByBillingID(ctx, bid)
	if err != nil || cl == nil {
		return nil
	}
	return toCallLogView(cl)
}

func (h *Dev) currentUser(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return 0, false
	}
	return id, true
}

// HandleUsage GET /api/v1/dev/usage?days=&model= 按日聚合。
func (h *Dev) HandleUsage(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	days := 30
	if v := q.Get("Days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	list, err := h.billing.DailyUsage(r.Context(), userID, days, q.Get("Model"))
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询用量失败")
		return
	}
	// 补零：按日聚合结果为空日补 0，保证连续天数展示。
	filled := fillZeroDates(days)
	for _, u := range list {
		if f, ok := filled[u.Date]; ok {
			f.Credits = decimalx.Round5(u.Credits)
			f.Calls = u.Calls
			filled[u.Date] = f
		}
	}
	out := make([]map[string]any, 0, len(filled))
	keys := sortedKeys(filled)
	for _, k := range keys {
		v := filled[k]
		out = append(out, map[string]any{"Date": k, "Credits": v.Credits, "Calls": v.Calls})
	}
	resp.OK(w, r, map[string]any{"List": out, "Days": days})
}

// HandleListBillings GET /api/v1/dev/billings?page=&size= 查询本人账单。
func (h *Dev) HandleListBillings(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := billing.RecordFilter{
		UserID: &userID,
		Page:   parsePage(q.Get("Page")),
		Size:   parseSize(q.Get("Size")),
	}
	records, total, err := h.billing.ListRecords(r.Context(), f)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询账单失败")
		return
	}
	list := make([]billingListItem, 0, len(records))
	for i := range records {
		list = append(list, toListItem(&records[i]))
	}
	resp.OK(w, r, map[string]any{"List": list, "Total": total, "Page": f.Page, "Size": f.Size})
}

// HandleGetBilling GET /api/v1/dev/billings/{billing_id} 本人账单详情（不含内部模型/渠道名）。
func (h *Dev) HandleGetBilling(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	bid := r.PathValue("billing_id")
	rec, err := h.billing.GetByBillingID(r.Context(), bid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			resp.Err(w, r, http.StatusNotFound, resp.CodeNotFound, "账单不存在")
			return
		}
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询账单失败")
		return
	}
	// 开发端仅能查看本人账单。
	if rec.UserID != userID {
		resp.Err(w, r, http.StatusNotFound, resp.CodeNotFound, "账单不存在")
		return
	}
	steps, routeDiff := buildBreakdown(rec)
	detail := BillingDetail{
		BillingID:       rec.BillingID,
		CallTime:        rec.CallTime.Format(time.RFC3339),
		Model:           rec.ExternalModel,
		PricingMode:     rec.PricingMode,
		CreditsConsumed: decimalx.Round5(rec.CreditsConsumed),
		Status:          rec.Status,
		Steps:           steps,
		RouteDiff:       routeDiff,
		CallLog:         h.attachCallLog(r.Context(), rec.BillingID),
	}
	resp.OK(w, r, detail)
}

// fillZeroDates 生成最近 days 天（含今天）的（date → 零值）映射。
// 按日切分以北京时间（Asia/Shanghai）为准。
func fillZeroDates(days int) map[string]dailyRow {
	out := make(map[string]dailyRow, days)
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.Local
	}
	now := time.Now().In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	for i := days - 1; i >= 0; i-- {
		d := start.AddDate(0, 0, -i)
		out[d.Format("2006-01-02")] = dailyRow{}
	}
	return out
}

type dailyRow struct {
	Credits float64
	Calls   int64
}

func sortedKeys(m map[string]dailyRow) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 简单插入排序保证按字典序（ISO 日期即按时间序）。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
